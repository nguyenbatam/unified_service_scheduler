package integration_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

// insertConfigEvent writes one technician-config outbox event in its own TX, marked with label in
// its payload (see eventLabel).
func insertConfigEvent(t *testing.T, st *store.Store, aggregate uuid.UUID, label string) {
	t.Helper()
	tx, err := st.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := tx.InsertOutbox(t.Context(), domain.AggregateTechnician, aggregate, domain.EventConfigUpdated, map[string]any{"version": 1, "label": label}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// created_at is the TX start: a TX that began first but committed its event second used to be
// published first. seq follows insert (= commit, under the aggregate lock) order.
func TestOutboxOrderFollowsInsertNotTxStart(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()

	early, err := st.Begin(ctx) // now() is fixed here
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = early.Rollback(ctx) }()
	// Begin waited for the server's BEGIN, which fixed early's now(): the next transaction starts
	// after that round trip (the precondition below checks the timestamps really are in that order).
	insertConfigEvent(t, st, agg, "FIRST") // begins later, commits first
	if err := early.InsertOutbox(ctx, domain.AggregateTechnician, agg, domain.EventConfigUpdated, map[string]any{"version": 2, "label": "SECOND"}); err != nil {
		t.Fatal(err)
	}
	if err := early.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !scalar[bool](t, pool, `
		SELECT (SELECT created_at FROM outbox_events WHERE payload->>'label' = 'SECOND')
		     < (SELECT created_at FROM outbox_events WHERE payload->>'label' = 'FIRST')`) {
		t.Fatal("setup: SECOND must carry the earlier created_at")
	}

	bus := newTestBus(busRules{})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig())
	for range 3 {
		if _, err := ob.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := bus.types(); len(got) != 2 || got[0] != "FIRST" || got[1] != "SECOND" {
		t.Fatalf("published %v, want [FIRST SECOND]", got)
	}
}

// A rejected event backs off; newer events of the same aggregate wait behind it (head-of-line)
// while other aggregates keep flowing.
func TestOutboxHeadOfLinePerAggregate(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	blocked, other := uuid.New(), uuid.New()
	insertConfigEvent(t, st, blocked, "A1")
	insertConfigEvent(t, st, blocked, "A2")
	insertConfigEvent(t, st, other, "B1")

	bus := newTestBus(busRules{byType: map[string]error{"A1": context.DeadlineExceeded}})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig())
	n, err := ob.Flush(ctx)
	if err != nil || n != 2 {
		t.Fatalf("first flush claimed %d (err %v), want 2: one per aggregate", n, err)
	}
	for range 2 {
		if n, err := ob.Flush(ctx); err != nil || n != 0 {
			t.Fatalf("A2 must wait behind A1's backoff: claimed %d err %v", n, err)
		}
	}
	if got := bus.types(); len(got) != 1 || got[0] != "B1" {
		t.Fatalf("published %v, want [B1]", got)
	}

	bus.update(func(r *busRules) { r.byType = nil })
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now() WHERE payload->>'label' = 'A1'`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := ob.Flush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := bus.types(); len(got) != 3 || got[1] != "A1" || got[2] != "A2" {
		t.Fatalf("published %v, want [B1 A1 A2]", got)
	}

	// A poisoned row no longer blocks its aggregate.
	insertConfigEvent(t, st, blocked, "A3")
	insertConfigEvent(t, st, blocked, "A4")
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxFailed)+` WHERE payload->>'label' = 'A3'`); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := bus.types(); len(got) != 4 || got[3] != "A4" {
		t.Fatalf("published %v, want A4 after the poisoned A3", got)
	}
}

// Several workers draining several aggregates concurrently, released together from a barrier,
// still publish each aggregate strictly in order (the aggregates' events interleave in the table,
// so a claim that ignored the per-aggregate head rule would let a worker overtake another).
func TestOutboxConcurrentWorkersKeepAggregateOrder(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	const aggregates, perAggregate, workers = 4, 12, 6
	aggs := make([]uuid.UUID, aggregates)
	for a := range aggs {
		aggs[a] = uuid.New()
	}
	want := map[string][]string{} // aggregate prefix → its events in insert order
	for i := range perAggregate {
		for a := range aggs {
			prefix := fmt.Sprintf("G%d", a)
			ev := fmt.Sprintf("%s-E%02d", prefix, i)
			want[prefix] = append(want[prefix], ev)
			insertConfigEvent(t, st, aggs[a], ev)
		}
	}
	total := aggregates * perAggregate
	bus := newTestBus(busRules{})
	var ready, wg sync.WaitGroup
	start := make(chan struct{})
	deadline := time.Now().Add(20 * time.Second)
	for i := range workers {
		ob := worker.NewOutbox(st, bus, slog.Default(), fmt.Sprintf("w%d", i), worker.DefaultOutboxConfig())
		ready.Add(1)
		wg.Go(func() {
			ready.Done()
			<-start // every worker claims its first batch at the same moment
			for len(bus.types()) < total && time.Now().Before(deadline) {
				n, err := ob.Flush(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					// Another worker holds the heads: wait a moment instead of spinning claims
					// against the database.
					time.Sleep(2 * time.Millisecond)
				}
			}
		})
	}
	ready.Wait()
	close(start)
	wg.Wait()
	got := bus.types()
	if len(got) != total {
		t.Fatalf("published %d of %d: %v", len(got), total, got)
	}
	per := map[string][]string{}
	for _, ev := range got {
		prefix, _, _ := strings.Cut(ev, "-")
		per[prefix] = append(per[prefix], ev)
	}
	for prefix, w := range want {
		if !slices.Equal(per[prefix], w) {
			t.Fatalf("aggregate %s published %v, want %v (all: %v)", prefix, per[prefix], w, got)
		}
	}
}

// power(2, attempts) overflowed float8 at attempts >= 1024 before LEAST applied: the retry failed
// and the row stayed leased until the lease expired, forever.
func TestRetryBackoffSurvivesHugeAttempts(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	insertConfigEvent(t, st, uuid.New(), "X")
	claimed, err := st.ClaimOutbox(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim %d %v", len(claimed), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET attempts = 2000 WHERE id = $1`, claimed[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkOutboxRetryAfter(ctx, []uuid.UUID{claimed[0].ID}, claimed[0].ClaimedAt, "broker down", 0, 0); err != nil {
		t.Fatalf("outbox retry at attempts=2000: %v", err)
	}
	if !scalar[bool](t, pool, `
		SELECT status = `+code(domain.OutboxPending)+` AND next_attempt_at BETWEEN now() + interval '59 seconds' AND now() + interval '61 seconds'
		FROM outbox_events WHERE id = $1`, claimed[0].ID) {
		t.Fatal("outbox row must be pending with the 60 s max backoff")
	}

	// Same for notifications.
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnqueueNotifications(ctx, []store.NewNotification{{
		ID: uuid.New(), AppointmentID: item.ID, Kind: domain.KindConfirmation, Channel: domain.ChannelEmail, Recipient: "a@example.com",
		Subject: "s", Body: "b", DedupeKey: "overflow|email", StartAt: item.StartAt, AppointmentVersion: item.Version,
	}}); err != nil {
		t.Fatal(err)
	}
	batch, err := st.ClaimNotifications(ctx, 10)
	if err != nil || len(batch) != 1 {
		t.Fatalf("claim %d %v", len(batch), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET attempts = 2000 WHERE id = $1`, batch[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkNotificationsRetry(ctx, []store.SendFailure{{ID: batch[0].ID, Reason: "down"}}, batch[0].ClaimedAt, 5000); err != nil {
		t.Fatalf("notification retry at attempts=2000: %v", err)
	}
	if !scalar[bool](t, pool, `
		SELECT status = `+code(domain.OutboxPending)+` AND next_attempt_at BETWEEN now() + interval '599 seconds' AND now() + interval '601 seconds'
		FROM notifications WHERE id = $1`, batch[0].ID) {
		t.Fatal("notification must be pending with the 10 min max backoff")
	}
}

// errBrokerDown is flakyBus's whole-batch failure: transient, and the outcome is known (nothing
// was sent), so the worker retries after its plain backoff.
var errBrokerDown = errors.New("broker unavailable")

// flakyBus fails every third PublishBatch call as a whole (a transient broker error: the worker
// backs the batch off, nothing is sent) and forwards the rest to its testBus.
type flakyBus struct {
	*testBus
	mu    sync.Mutex
	calls int
}

func (b *flakyBus) PublishBatch(ctx context.Context, batch []events.Envelope) ([]error, error) {
	b.mu.Lock()
	b.calls++
	fail := b.calls%3 == 0
	b.mu.Unlock()
	if fail {
		return nil, errBrokerDown
	}
	return b.testBus.PublishBatch(ctx, batch)
}

// Several workers draining several interleaved aggregates while every third publish fails: each
// aggregate's events still reach the bus in seq order, every event gets there, and a failed
// batch never lets a newer event of its aggregate overtake it. The backoff of a failed batch is
// cut short (next_attempt_at is only a cost hint for order, invariant 7), so retries interleave
// with fresh claims instead of the test waiting out seconds of backoff.
func TestOutboxConcurrentWorkersKeepOrderUnderFailures(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	const aggregates, perAggregate = 5, 10
	aggs := make([]uuid.UUID, aggregates)
	for i := range aggs {
		aggs[i] = uuid.New()
	}
	for e := range perAggregate { // interleaved: A0E0, A1E0, …, A0E1, …
		for _, agg := range aggs {
			insertConfigEvent(t, st, agg, fmt.Sprintf("E%02d", e))
		}
	}
	bus := &flakyBus{testBus: newTestBus(busRules{})}
	published := func() map[string][]string {
		out := map[string][]string{}
		for _, env := range bus.envelopes() {
			out[env.Key] = append(out[env.Key], eventLabel(env.Event))
		}
		return out
	}
	done := func() bool {
		got := published()
		for _, agg := range aggs {
			if len(got[agg.String()]) < perAggregate {
				return false
			}
		}
		return true
	}
	var wg sync.WaitGroup
	deadline := time.Now().Add(30 * time.Second)
	for i := range 4 {
		ob := worker.NewOutbox(st, bus, slog.Default(), fmt.Sprintf("w%d", i), worker.DefaultOutboxConfig())
		wg.Go(func() {
			for !done() && time.Now().Before(deadline) {
				n, err := ob.Flush(ctx)
				if err != nil && !errors.Is(err, errBrokerDown) {
					t.Error(err)
					return
				}
				if n == 0 {
					// SKIP LOCKED: never wait on (or deadlock with) a worker's mark of the same rows.
					if _, err := pool.Exec(ctx, `
						UPDATE outbox_events SET next_attempt_at = now()
						WHERE id IN (SELECT id FROM outbox_events WHERE status = `+code(domain.OutboxPending)+` AND next_attempt_at > now()
						             FOR UPDATE SKIP LOCKED)`); err != nil {
						t.Error(err)
						return
					}
					time.Sleep(2 * time.Millisecond)
				}
			}
		})
	}
	wg.Wait()
	bus.mu.Lock()
	calls := bus.calls
	bus.mu.Unlock()
	if calls < 3 {
		t.Fatalf("only %d publish calls: no batch failed, the test proved nothing", calls)
	}
	got := published()
	for _, agg := range aggs {
		seq := got[agg.String()]
		if len(seq) != perAggregate {
			t.Fatalf("aggregate %s: published %v, want each of %d events once", agg, seq, perAggregate)
		}
		for i, typ := range seq {
			if want := fmt.Sprintf("E%02d", i); typ != want {
				t.Fatalf("aggregate %s: publish order %v, want E00…E%02d in order", agg, seq, perAggregate-1)
			}
		}
	}
	if left := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE status <> `+code(domain.OutboxPublished)); left != 0 {
		t.Fatalf("%d outbox rows not published", left)
	}
}
