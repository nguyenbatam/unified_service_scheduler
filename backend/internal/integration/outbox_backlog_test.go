package integration_test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
	"github.com/segmentio/kafka-go"
)

var transient = &events.RetriableError{Err: kafka.NotEnoughReplicas}

// outboxRow is one row's scheduling state.
type outboxRow struct {
	status   string
	attempts int
	due      bool          // next_attempt_at <= now()
	wait     time.Duration // next_attempt_at - now() (negative when due)
}

func outboxState(t *testing.T, pool *pgxpool.Pool, label string) outboxRow {
	t.Helper()
	var r outboxRow
	var secs float64
	if err := pool.QueryRow(t.Context(), `
		SELECT `+outboxStatusText("status")+`, attempts, next_attempt_at <= now(), extract(epoch FROM next_attempt_at - now())::float8
		FROM outbox_events WHERE payload->>'label' = $1`, label).Scan(&r.status, &r.attempts, &r.due, &secs); err != nil {
		t.Fatalf("outbox row %s: %v", label, err)
	}
	r.wait = time.Duration(secs * float64(time.Second))
	return r
}

func flushN(t *testing.T, ob *worker.Outbox, n int) {
	t.Helper()
	for range n {
		if _, err := ob.Flush(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}

// A head that backs off pushes the rows queued behind it to its own retry time, so the claim does
// not read them while it waits; they are still published strictly in order once the head goes
// out, each one releasing the next, and other aggregates are never held up.
func TestOutboxBackoffPushesQueueAndReleasesItInOrder(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()
	for i := 1; i <= 5; i++ {
		insertConfigEvent(t, st, agg, fmt.Sprintf("A%d", i))
	}
	insertConfigEvent(t, st, uuid.New(), "B1")
	bus := newTestBus(busRules{byType: map[string]error{"A1": transient}})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig())
	flushN(t, ob, 2)

	head := outboxState(t, pool, "A1")
	if head.status != "pending" || head.due {
		t.Fatalf("A1 after a transient failure = %+v, want pending and backing off", head)
	}
	// The claim held the queue back while A1 was in flight; the retry keeps it at least until A1
	// is due again.
	if !scalar[bool](t, pool, `
		SELECT bool_and(l.next_attempt_at >= h.next_attempt_at)
		FROM outbox_events l, outbox_events h
		WHERE h.payload->>'label' = 'A1' AND l.aggregate_id = h.aggregate_id AND l.seq > h.seq`) {
		t.Fatal("rows queued behind a backed-off head must wait at least until its retry time")
	}
	// A backoff longer than the claim's hold pushes the queue to the head's retry time.
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now(), attempts = 6 WHERE payload->>'label' = 'A1'`); err != nil {
		t.Fatal(err)
	}
	flushN(t, ob, 1)
	if !scalar[bool](t, pool, `
		SELECT bool_and(l.next_attempt_at = h.next_attempt_at) AND min(h.next_attempt_at) > now() + interval '50 seconds'
		FROM outbox_events l, outbox_events h
		WHERE h.payload->>'label' = 'A1' AND l.aggregate_id = h.aggregate_id AND l.seq > h.seq`) {
		t.Fatal("a 60 s backoff must push the queue behind the head to the head's retry time")
	}
	if got := bus.types(); !slices.Equal(got, []string{"B1"}) {
		t.Fatalf("published %v, want only B1", got)
	}

	// Another aggregate's head is still claimed at once, past the pushed queue.
	insertConfigEvent(t, st, uuid.New(), "C1")
	flushN(t, ob, 1)
	if got := bus.types(); !slices.Equal(got, []string{"B1", "C1"}) {
		t.Fatalf("published %v, want C1 right after B1", got)
	}

	// The head goes out early (an operator nudged it); the rows behind it still carry the old
	// retry time and must not wait for it: each publish releases the next.
	bus.update(func(r *busRules) { r.byType = nil })
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now() WHERE payload->>'label' = 'A1'`); err != nil {
		t.Fatal(err)
	}
	flushN(t, ob, 6)
	if got := bus.types(); !slices.Equal(got, []string{"B1", "C1", "A1", "A2", "A3", "A4", "A5"}) {
		t.Fatalf("published %v, want the A queue in order right away", got)
	}
}

// A head that fails for good (or is poisoned) no longer blocks its aggregate: the row behind it,
// held back while the head was retrying, is released in the same statement.
func TestOutboxFailedHeadReleasesNextRow(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()
	insertConfigEvent(t, st, agg, "H1")
	insertConfigEvent(t, st, agg, "H2")
	cfg := worker.DefaultOutboxConfig()
	cfg.MaxRejections = 1
	bus := newTestBus(busRules{byType: map[string]error{"H1": transient}})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", cfg)
	flushN(t, ob, 1)
	if next := outboxState(t, pool, "H2"); next.due {
		t.Fatalf("H2 = %+v, want it held back behind H1's backoff", next)
	}

	bus.update(func(r *busRules) { r.byType = map[string]error{"H1": kafka.MessageSizeTooLarge} }) // a content rejection: fails H1
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET next_attempt_at = now() WHERE payload->>'label' = 'H1'`); err != nil {
		t.Fatal(err)
	}
	flushN(t, ob, 1)
	if got := outboxState(t, pool, "H1"); got.status != "failed" {
		t.Fatalf("H1 = %+v, want failed", got)
	}
	if next := outboxState(t, pool, "H2"); !next.due {
		t.Fatalf("H2 = %+v, want it released when H1 failed", next)
	}
	flushN(t, ob, 1)
	if got := bus.types(); !slices.Equal(got, []string{"H2"}) {
		t.Fatalf("published %v, want H2 after the failed H1", got)
	}
}

// The release never shortens a row's own backoff: an event whose publish outcome is unknown must
// wait out the writer lifetime even when an older, requeued event of its aggregate goes out first.
func TestOutboxReleaseKeepsUnknownOutcomeWait(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()
	insertConfigEvent(t, st, agg, "OLD")
	insertConfigEvent(t, st, agg, "NEW")
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxFailed)+` WHERE payload->>'label' = 'OLD'`); err != nil {
		t.Fatal(err)
	}
	unknown := &events.RetriableError{Err: fmt.Errorf("%w: %w", events.ErrOutcomeUnknown, context.DeadlineExceeded)}
	bus := newTestBus(busRules{byType: map[string]error{"NEW": unknown}})
	ob := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig())
	flushN(t, ob, 1)
	floor := events.MaxWriteLifetime - 2*time.Second
	if got := outboxState(t, pool, "NEW"); got.status != "pending" || got.wait < floor {
		t.Fatalf("NEW after an unknown outcome = %+v, want pending for about %s", got, events.MaxWriteLifetime)
	}

	oldID := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE payload->>'label' = 'OLD'`)
	select { // drop a wake-up left by the setup's commits
	case <-st.OutboxWakeups():
	default:
	}
	if n, skipped, err := st.RequeueFailedOutboxIdle(ctx, []uuid.UUID{oldID}); err != nil || n != 1 || len(skipped) != 0 {
		t.Fatalf("requeue = %d %v %v", n, skipped, err)
	}
	select { // the requeued event is due now: the worker is woken, not left to its next poll
	case <-st.OutboxWakeups():
	default:
		t.Fatal("requeue did not wake the outbox worker")
	}
	bus.update(func(r *busRules) { r.byType = nil })
	flushN(t, ob, 1)
	if got := bus.types(); !slices.Equal(got, []string{"OLD"}) {
		t.Fatalf("published %v, want the requeued OLD", got)
	}
	if got := outboxState(t, pool, "NEW"); got.status != "pending" || got.wait < floor-time.Second {
		t.Fatalf("NEW after OLD went out = %+v: its own unknown-outcome wait must stand", got)
	}
}

// Poisoned rows (no event can be built from them) are failed in one fenced statement: a worker
// whose lease expired cannot fail a row another worker has claimed since.
func TestOutboxPoisonIsBatchedAndFenced(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	insert := func(n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status)
			SELECT gen_random_uuid(), `+code(domain.AggregateAppointment)+`, gen_random_uuid(), `+code(domain.EventAppointmentCreated)+`,
			       '"unreadable"', `+code(domain.OutboxPending)+` FROM generate_series(1, $1)`, n); err != nil {
			t.Fatal(err)
		}
	}
	insert(3)
	insertConfigEvent(t, st, uuid.New(), "GOOD")
	bus := newTestBus(busRules{})
	if _, err := worker.NewOutbox(st, bus, slog.Default(), "test", worker.DefaultOutboxConfig()).Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE payload = '"unreadable"' AND status = `+code(domain.OutboxFailed)+` AND claimed_at IS NULL AND last_error LIKE '%cannot unmarshal%'`); got != 3 {
		t.Fatalf("%d of 3 poisoned rows failed", got)
	}
	if got := bus.types(); !slices.Equal(got, []string{"GOOD"}) {
		t.Fatalf("published %v, want the good row next to the poisoned ones", got)
	}

	// Fencing: the lease expires, another worker claims the row again, the first one reports late.
	clearOutbox(t, pool)
	insert(1)
	first, err := st.ClaimOutbox(ctx, 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("claim %d %v", len(first), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET claimed_at = now() - interval '1 minute' WHERE id = $1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReclaimStaleOutbox(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	second, err := st.ClaimOutbox(ctx, 10)
	if err != nil || len(second) != 1 {
		t.Fatalf("re-claim %d %v", len(second), err)
	}
	if err := st.MarkOutboxPoison(ctx, []store.OutboxPoison{{ID: first[0].ID, Reason: "late"}}, first[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE id = $1`, first[0].ID); got != "processing" {
		t.Fatalf("a stale poison mark changed the row to %s", got)
	}
	if err := st.MarkOutboxPoison(ctx, []store.OutboxPoison{{ID: second[0].ID, Reason: "bad"}}, second[0].ClaimedAt); err != nil {
		t.Fatal(err)
	}
	if got := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` || ':' || last_error FROM outbox_events WHERE id = $1`, second[0].ID); got != "failed:bad" {
		t.Fatalf("current lease holder's poison mark = %s", got)
	}
}

// The operator requeue only reads processing rows, never locks them: a mark holding one (here a TX
// standing in for it) must not make the requeue wait, and the event is reported as skipped.
func TestRequeueDoesNotLockProcessingRows(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	agg := uuid.New()
	insertConfigEvent(t, st, agg, "OLD")
	insertConfigEvent(t, st, agg, "NEW")
	insertConfigEvent(t, st, agg, "NEWER") // pending behind NEW: the requeue locks it
	oldID := scalar[uuid.UUID](t, pool, `SELECT id FROM outbox_events WHERE payload->>'label' = 'OLD'`)
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxFailed)+` WHERE payload->>'label' = 'OLD'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxProcessing)+`, claimed_at = now() WHERE payload->>'label' = 'NEW'`); err != nil {
		t.Fatal(err)
	}
	mark, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mark.Rollback(ctx) }()
	if _, err := mark.Exec(ctx, `SELECT 1 FROM outbox_events WHERE payload->>'label' = 'NEW' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	began := time.Now()
	n, skipped, err := st.RequeueFailedOutboxIdle(waitCtx, []uuid.UUID{oldID})
	if err != nil {
		t.Fatalf("requeue next to a locked processing row: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("requeue waited %s on the processing row", took)
	}
	if n != 0 || len(skipped) != 1 || skipped[0] != oldID {
		t.Fatalf("requeued %d, skipped %v; want OLD skipped while NEW is in flight", n, skipped)
	}
}

// A large backlog queued behind backed-off heads does not slow the claim down or hide other
// aggregates' events: the queue is pushed out of the claim's range, and every other head is
// claimed in one round.
func TestOutboxClaimSkipsBackedOffBacklog(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	// 20 aggregates, each a head backing off with 100 rows behind it; a backed-off head pushes
	// its queue (as MarkOutboxRetryAfter does) — here written directly for speed.
	if _, err := pool.Exec(ctx, `
		WITH aggs AS (SELECT gen_random_uuid() AS a, g FROM generate_series(1, 20) g)
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status, attempts, next_attempt_at)
		SELECT gen_random_uuid(), `+code(domain.AggregateTechnician)+`, a, `+code(domain.EventConfigUpdated)+`, '{"version":1,"label":"QUEUED"}', `+code(domain.OutboxPending)+`,
		       CASE WHEN i = 0 THEN 3 ELSE 0 END, now() + interval '1 minute'
		FROM aggs, generate_series(0, 100) i ORDER BY i, g`); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		insertConfigEvent(t, st, uuid.New(), fmt.Sprintf("READY%d", i))
	}
	if _, err := pool.Exec(ctx, `ANALYZE outbox_events`); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimOutbox(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range claimed {
		got = append(got, payloadLabel(c.Payload))
	}
	if !slices.Equal(got, []string{"READY0", "READY1", "READY2", "READY3", "READY4"}) {
		t.Fatalf("claimed %v, want the five ready heads in seq order", got)
	}
}

// A claim holds back at most store.OutboxHoldLimit queued rows (nearest first), so a huge backlog
// behind its heads (e.g. after a long broker outage) cannot push the claim past its timeout.
func TestOutboxClaimHoldIsCapped(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	clearOutbox(t, pool)
	tail := store.OutboxHoldLimit + 500
	mustExec(t, pool, `
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status, attempts, next_attempt_at)
		SELECT gen_random_uuid(), `+code(domain.AggregateTechnician)+`, '00000000-0000-4000-8000-0000000000aa', `+code(domain.EventConfigUpdated)+`,
		       jsonb_build_object('version', 1, 'label', CASE WHEN i = 0 THEN 'HEAD' ELSE 'TAIL' END), `+code(domain.OutboxPending)+`, 0, now()
		FROM generate_series(0, $1::int) i ORDER BY i`, tail)
	claimed, err := st.ClaimOutbox(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || payloadLabel(claimed[0].Payload) != "HEAD" {
		t.Fatalf("claimed %d rows (%v), want only the head", len(claimed), claimed)
	}
	held := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE payload->>'label' = 'TAIL' AND next_attempt_at > now()`)
	if held != store.OutboxHoldLimit {
		t.Fatalf("held %d queued rows, want the cap %d", held, store.OutboxHoldLimit)
	}
	if !scalar[bool](t, pool, `
		SELECT max(seq) FILTER (WHERE next_attempt_at > now()) < min(seq) FILTER (WHERE next_attempt_at <= now())
		FROM outbox_events WHERE payload->>'label' = 'TAIL'`) {
		t.Fatal("the hold must take the rows nearest the head first")
	}
	// The rows past the cap are due but still blocked by the in-flight head.
	if again, err := st.ClaimOutbox(ctx, 10); err != nil || len(again) != 0 {
		t.Fatalf("second claim = %v, %v; want nothing while the head is in flight", again, err)
	}
}
