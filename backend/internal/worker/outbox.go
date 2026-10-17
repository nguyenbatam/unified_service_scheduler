package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Publisher is the slice of events.Bus the worker needs (fakeable in tests). Per-message errors
// count as rejections unless events.IsRetriable says otherwise; see events.Bus.PublishBatch.
type Publisher interface {
	PublishBatch(ctx context.Context, batch []events.Envelope) ([]error, error)
}

// bookkeepingTimeout bounds recording publish outcomes (see OutboxConfig.Validate).
const bookkeepingTimeout = 5 * time.Second

// leaseMargin is slack for clock drift between this process and Postgres: the lease is measured
// from the database's claimed_at, the publish deadline from this process's clock just before the
// claim was sent (so it never starts later than the lease).
const leaseMargin = 2 * time.Second

// claimTimeout bounds the claim statement. It runs inside the publish deadline (which starts before
// the claim), so Validate keeps PublishTimeout long enough for it as well.
const claimTimeout = time.Second

// listenHealthy: a LISTEN session that lasted this long was healthy, so its failure starts the
// reconnect backoff over instead of continuing from where an earlier outage left it.
const listenHealthy = time.Minute

type OutboxConfig struct {
	BatchSize    int           // rows claimed per round
	PollInterval time.Duration // fallback when no NOTIFY arrives
	// MaxRejections: after this many per-message rejections (the broker refused the message for
	// what it contains) a row becomes "failed". Transient errors — whole-batch or per message —
	// never count; they only back off.
	MaxRejections int
	// PublishTimeout is the deadline for one round's claim and publish, measured from just before
	// the claim (so from no later than the lease's claimed_at). Every write the bus starts finishes
	// within it, and one abandoned at the deadline is dead events.MaxWriteLifetime later; Validate
	// ties it to the lease.
	PublishTimeout time.Duration
}

func DefaultOutboxConfig() OutboxConfig {
	return OutboxConfig{BatchSize: 50, PollInterval: time.Second, MaxRejections: 10, PublishTimeout: 16 * time.Second}
}

// Validate checks the timing against the maintenance lease (ReclaimStaleOutbox). Rows must never
// be reclaimed — and so published by another instance — while this one may still write them or
// record their outcome:
//   - PublishTimeout + events.MaxWriteLifetime + margin <= lease: kafka-go keeps retrying a batch
//     after a timed-out call returns, for at most MaxWriteLifetime;
//   - PublishTimeout + bookkeepingTimeout + margin <= lease: outcomes land before the lease ends;
//   - PublishTimeout >= claimTimeout + 2 * events.MaxWriteLifetime: after the slowest claim and a
//     worst-case first write there is still time to resend one suspect alone (events.Bus isolates
//     content rejections that way), so a poison message is always found;
//   - BatchSize <= events.WriterBatchSize: a claim fits one kafka-go batch per partition, so it goes
//     out in one call and MaxWriteLifetime bounds it (the bus would otherwise split it into
//     consecutive calls, each needing its own MaxWriteLifetime of the deadline).
func (c OutboxConfig) Validate(lease time.Duration) error {
	if c.BatchSize <= 0 || c.PollInterval <= 0 || c.MaxRejections <= 0 {
		return fmt.Errorf("outbox: batch size, poll interval and max rejections must be positive")
	}
	if c.BatchSize > events.WriterBatchSize {
		return fmt.Errorf("outbox: batch size %d exceeds the kafka writer batch size %d",
			c.BatchSize, events.WriterBatchSize)
	}
	if c.PublishTimeout < claimTimeout+2*events.MaxWriteLifetime {
		return fmt.Errorf("outbox: publish timeout %s must cover the claim (%s) and twice the kafka writer lifetime %s",
			c.PublishTimeout, claimTimeout, events.MaxWriteLifetime)
	}
	if tail := max(events.MaxWriteLifetime, bookkeepingTimeout); c.PublishTimeout+tail+leaseMargin > lease {
		return fmt.Errorf("outbox: publish timeout %s + %s + %s margin exceeds the lease %s",
			c.PublishTimeout, tail, leaseMargin, lease)
	}
	return nil
}

// DrainTimeout is the longest one in-flight Flush can still run after shutdown cancels its context:
// the publish deadline (measured from just before the claim) plus the bookkeeping that records the
// outcome.
// Shutdown should wait at least this long for the worker, or rows stay processing until the lease
// ends and go out twice.
func (c OutboxConfig) DrainTimeout() time.Duration {
	return c.PublishTimeout + bookkeepingTimeout
}

type Outbox struct {
	store      *store.Store
	bus        Publisher
	log        *slog.Logger
	instanceID string
	cfg        OutboxConfig
}

func NewOutbox(st *store.Store, bus Publisher, log *slog.Logger, instanceID string, cfg OutboxConfig) *Outbox {
	return &Outbox{store: st, bus: bus, log: log, instanceID: instanceID, cfg: cfg}
}

// Run publishes committed outbox rows until ctx ends. It wakes right after a commit of this
// process (Store.OutboxWakeups) or of another instance (LISTEN, fed by Store.RunOutboxNotifier);
// the poll ticker covers missed notifications and retry backoff windows.
func (o *Outbox) Run(ctx context.Context) {
	wake := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Go(func() { o.listen(ctx, wake) })
	defer wg.Wait()

	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		o.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		case <-o.store.OutboxWakeups():
		}
	}
}

func (o *Outbox) listen(ctx context.Context, wake chan<- struct{}) {
	var backoff time.Duration
	for {
		started := time.Now()
		err := o.store.ListenOutbox(ctx, func() {
			select {
			case wake <- struct{}{}:
			default:
			}
		})
		if ctx.Err() != nil {
			return
		}
		backoff = listenBackoff(backoff, time.Since(started))
		o.log.Warn("outbox listen", "err", err, "instance", o.instanceID, "retry_in", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// listenBackoff is the wait before reconnecting LISTEN after a session that lasted session: 1s,
// doubling up to 30s while sessions keep failing quickly, and back to 1s after a healthy one (an
// outage hours ago must not slow down the reconnect after today's blip).
func listenBackoff(prev, session time.Duration) time.Duration {
	if prev <= 0 || session >= listenHealthy {
		return time.Second
	}
	return min(prev*2, 30*time.Second)
}

// drain keeps flushing until a claim comes back empty. A short batch is not enough to stop: the
// claim takes at most one event per aggregate, so the next event of an aggregate just published
// only becomes claimable in the following round. Rows that failed are backing off and are not
// re-claimed, so the loop ends once nothing is ready.
func (o *Outbox) drain(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := o.Flush(ctx)
		if err != nil {
			o.log.Error("outbox flush", "err", err, "instance", o.instanceID)
			return
		}
		if n == 0 {
			return
		}
	}
}

// Flush claims one batch, publishes it in a single Kafka write, and records per-row outcomes.
// It returns how many rows were claimed. The batch holds at most one event per aggregate
// (store.ClaimOutbox), so a partial rejection can never let a newer event overtake an older one.
func (o *Outbox) Flush(ctx context.Context) (int, error) {
	// The lease runs from claimed_at, the database's now() when the claim statement starts. Taking
	// the publish deadline from a clock read before the claim is sent keeps it inside the lease
	// however long the claim takes (Validate charges claimTimeout to PublishTimeout).
	start := time.Now()
	deadline := start.Add(o.cfg.PublishTimeout)
	claimCtx, cancelClaim := context.WithTimeout(ctx, claimTimeout)
	rows, err := o.store.ClaimOutbox(claimCtx, o.cfg.BatchSize)
	cancelClaim()
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	// Publishing and bookkeeping ignore ctx cancellation (shutdown): kafka-go does not stop a
	// queued batch when its call returns, so abandoning it would only lose track of an outcome
	// (rows wait for the lease and go out twice, possibly overtaken by the stale original). Both
	// phases are bounded by their own deadlines instead.
	detached := context.WithoutCancel(ctx)

	claimedAt := rows[0].ClaimedAt // one UPDATE claims the batch, so all rows share it
	batch := make([]events.Envelope, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	var poison []store.OutboxPoison
	for _, row := range rows {
		ev, err := BuildEvent(row, o.instanceID)
		if err != nil {
			o.log.Error("outbox payload", "err", err, "id", row.ID)
			poison = append(poison, store.OutboxPoison{ID: row.ID, Reason: err.Error()})
			continue
		}
		batch = append(batch, events.Envelope{Key: row.AggregateID.String(), Event: ev})
		ids = append(ids, row.ID)
	}
	// Poisoned rows are recorded with the rest of the bookkeeping, after the publish, so marking
	// them never eats into the publish deadline.
	markPoison := func(bk context.Context) {
		if len(poison) == 0 {
			return
		}
		// On failure the rows stay leased: the maintenance reclaim returns them to pending once the
		// lease ends, and the next claim poisons them again.
		if err := o.store.MarkOutboxPoison(bk, poison, claimedAt); err != nil {
			o.log.Error("outbox mark poison", "err", err, "rows", len(poison), "instance", o.instanceID)
		}
	}
	if len(batch) == 0 {
		bk, cancel := context.WithTimeout(detached, bookkeepingTimeout)
		defer cancel()
		markPoison(bk)
		return len(rows), nil
	}
	pubCtx, cancelPub := context.WithDeadline(detached, deadline)
	defer cancelPub()
	perMessage, err := o.bus.PublishBatch(pubCtx, batch)
	bk, cancelAfter := context.WithTimeout(detached, bookkeepingTimeout)
	defer cancelAfter()
	markPoison(bk)
	if err == nil && len(perMessage) != len(batch) {
		err = fmt.Errorf("publisher returned %d results for %d messages", len(perMessage), len(batch))
	}
	if err != nil {
		if markErr := o.store.MarkOutboxRetryAfter(bk, ids, claimedAt, err.Error(), 0, unknownDelay(err)); markErr != nil {
			o.log.Error("outbox mark retry", "err", markErr)
		}
		return len(rows), fmt.Errorf("publish batch: %w", err)
	}
	out := sortOutcomes(perMessage)
	pick := func(idx []int) []uuid.UUID {
		picked := make([]uuid.UUID, len(idx))
		for j, i := range idx {
			picked[j] = ids[i]
		}
		return picked
	}
	if err := o.store.MarkOutboxPublished(bk, pick(out.published), claimedAt); err != nil {
		return len(rows), err
	}
	for _, g := range []struct {
		idx           []int
		maxRejections int
		minDelay      time.Duration
	}{
		{out.rejected, o.cfg.MaxRejections, 0},
		{out.retry, 0, 0},
		{out.unknown, 0, events.MaxWriteLifetime},
	} {
		if len(g.idx) == 0 {
			continue
		}
		reason := perMessage[g.idx[0]].Error()
		if err := o.store.MarkOutboxRetryAfter(bk, pick(g.idx), claimedAt, reason, g.maxRejections, g.minDelay); err != nil {
			return len(rows), err
		}
	}
	if failed := len(out.rejected) + len(out.retry) + len(out.unknown); failed > 0 {
		o.log.Warn("outbox partial publish", "failed", failed, "rejected", len(out.rejected), "err", out.firstErr)
	}
	return len(rows), nil
}

// outcomes groups a batch's per-message results by what the bookkeeping must do.
type outcomes struct {
	published []int
	rejected  []int // the broker refused the message itself: counts toward poisoning
	retry     []int // transient: back off only
	unknown   []int // transient, and the message may still land: back off past the writer lifetime
	firstErr  error
}

func sortOutcomes(perMessage []error) outcomes {
	var out outcomes
	for i, err := range perMessage {
		switch {
		case err == nil:
			out.published = append(out.published, i)
			continue
		case outcomeUnknown(err):
			out.unknown = append(out.unknown, i)
		case events.IsRetriable(err):
			out.retry = append(out.retry, i)
		default:
			out.rejected = append(out.rejected, i)
		}
		if out.firstErr == nil {
			out.firstErr = err
		}
	}
	return out
}

// unknownDelay keeps rows back while a batch abandoned with an unknown outcome may still land, so
// no instance publishes them again before kafka-go has given up on the original.
func unknownDelay(err error) time.Duration {
	if outcomeUnknown(err) {
		return events.MaxWriteLifetime
	}
	return 0
}

// outcomeUnknown: the write may still land. events.Bus says so explicitly; a context error that is
// not marked retriable (a publisher whose write was cut short) is treated the same way.
func outcomeUnknown(err error) bool {
	if events.IsOutcomeUnknown(err) {
		return true
	}
	return !events.IsRetriable(err) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
}

// BuildEvent turns an outbox row into the Kafka envelope with routing metadata and a thin notice.
func BuildEvent(row store.OutboxEvent, instanceID string) (realtime.Event, error) {
	ev := realtime.Event{ID: row.ID, Type: row.EventType, InstanceID: instanceID, Payload: row.Payload}
	switch row.AggregateType {
	case domain.AggregateAppointment:
		var a domain.Appointment
		if err := json.Unmarshal(row.Payload, &a); err != nil {
			return realtime.Event{}, err
		}
		ev.Kind = realtime.KindAppointment
		ev.DealershipID = a.DealershipID
		ev.CustomerID = a.CustomerID
		ev.Appointment = &realtime.AppointmentNotice{
			ID:           a.ID,
			DealershipID: a.DealershipID,
			Status:       a.Status,
			Version:      a.Version,
			StartAt:      a.StartAt,
			EndAt:        a.EndAt,
		}
	case domain.AggregateDealership, domain.AggregateServiceType, domain.AggregateTechnician:
		var c struct {
			Version      int       `json:"version"`
			DealershipID uuid.UUID `json:"dealershipId"` // technicians (domain.TechnicianAdmin)
		}
		if err := json.Unmarshal(row.Payload, &c); err != nil {
			return realtime.Event{}, err
		}
		ev.Kind = realtime.KindConfig
		ev.Config = &realtime.ConfigNotice{Entity: row.AggregateType, ID: row.AggregateID, Version: c.Version}
		// The dealership routes it (realtime.routeConfig); service types are group-wide.
		switch row.AggregateType {
		case domain.AggregateDealership:
			ev.DealershipID = row.AggregateID
		case domain.AggregateTechnician:
			ev.DealershipID = c.DealershipID
		}
	case domain.AggregateUser:
		ev.Kind = realtime.KindSession
		ev.UserID = row.AggregateID
		// The new token version lets hubs spare sockets opened after this revocation. A revoke
		// must never be dropped, so an unreadable payload degrades to 0 (close every socket).
		var u struct {
			TokenVersion int `json:"tokenVersion"`
		}
		if json.Unmarshal(row.Payload, &u) == nil && u.TokenVersion > 0 {
			ev.TokenVersion = u.TokenVersion
		}
		ev.Payload = nil // nothing else about the user needs to travel further
	default:
		return realtime.Event{}, fmt.Errorf("unknown aggregate type %d", int16(row.AggregateType))
	}
	return ev, nil
}
