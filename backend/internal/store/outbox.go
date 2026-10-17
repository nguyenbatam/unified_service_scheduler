package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// OutboxChannel is the LISTEN/NOTIFY channel that wakes every instance's outbox worker after a
// commit wrote outbox rows (sent by Store.RunOutboxNotifier, outside the writing TX).
const OutboxChannel = "outbox_events"

type OutboxEvent struct {
	ID            uuid.UUID
	Seq           int64 // commit order within one aggregate (drawn at insert, after the aggregate's row lock)
	AggregateType domain.AggregateType
	AggregateID   uuid.UUID
	EventType     domain.EventType
	Payload       []byte
	CreatedAt     time.Time
	Attempts      int
	// ClaimedAt fences bookkeeping: marks only apply while this worker's lease is current.
	ClaimedAt time.Time
}

// InsertOutbox records an event in the caller's TX. The workers are woken once it commits
// (Tx.Commit → Store.signalOutbox).
func (t *Tx) InsertOutbox(ctx context.Context, aggregateType domain.AggregateType, aggregateID uuid.UUID, eventType domain.EventType, payload any) error {
	if !aggregateType.Valid() || !eventType.IsStored() {
		return fmt.Errorf("store: outbox event %d about aggregate %d cannot be stored", eventType, aggregateType)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(ctx, `
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status)
		VALUES ($1, $2, $3, $4, $5::jsonb, `+outboxPending+`)`,
		uuid.New(), int16(aggregateType), aggregateID, int16(eventType), raw)
	if err == nil {
		t.markEvents()
	}
	return err
}

// ClaimOutbox leases up to limit ready rows to this worker (status processing + claimed_at). A row
// is claimable only when no older row of the same aggregate is still pending (backing off, or
// skipped as locked by another claimer) or processing, and no row of it at all is processing (a
// requeued failed row is older than events published after it, so it must also wait for a newer one
// being published right now). So one aggregate has at most one event in flight, its events are
// claimed in seq order, and a retried event is never overtaken by a newer one.
//
// Across aggregates rows are claimed oldest-due first (next_attempt_at, then seq). The scan
// (outbox_events_ready_idx) only reads rows that are due, and the rows queued behind a head are
// kept out of it while the head is in flight or backing off: the claim holds them back for
// OutboxInflightHold, a backed-off head pushes them to its own retry time (MarkOutboxRetryAfter),
// and a resolved head releases the next one (see shiftedSQL). So a backlog queued behind heads
// costs the claim nothing; the two head-of-line probes are each one index lookup. Holding back
// skips rows another statement has locked (it is only a hint) and so never waits.
func (s *Store) ClaimOutbox(ctx context.Context, limit int) ([]OutboxEvent, error) {
	rows, err := s.pool.Query(ctx, `
		WITH cte AS (
			SELECT o.id
			FROM outbox_events o
			WHERE o.status = `+outboxPending+` AND o.next_attempt_at <= now()
			  -- The first probe's processing overlaps the second, on purpose: its predicate is
			  -- exactly outbox_events_inflight_idx's, so it needs no heap recheck of status.
			  AND NOT EXISTS ( -- an older row of the aggregate still queued or in flight
				SELECT 1 FROM outbox_events p
				WHERE p.aggregate_id = o.aggregate_id AND p.seq < o.seq
				  AND p.status IN (`+outboxPending+`, `+outboxProcessing+`))
			  AND NOT EXISTS ( -- any row of the aggregate in flight (a newer one, after a requeue)
				SELECT 1 FROM outbox_events p
				WHERE p.aggregate_id = o.aggregate_id AND p.status = `+outboxProcessing+`)
			ORDER BY o.next_attempt_at, o.seq
			FOR UPDATE OF o SKIP LOCKED
			LIMIT $1
		), claimed AS (
			UPDATE outbox_events o
			SET status = `+outboxProcessing+`, attempts = o.attempts + 1, claimed_at = now()
			FROM cte
			WHERE o.id = cte.id
			RETURNING o.id, o.seq, o.aggregate_type, o.aggregate_id, o.event_type, o.payload, o.created_at, o.attempts, o.claimed_at
		), held AS (
			-- Only rows the claim's scan would read now (due), so a long queue is not rewritten on
			-- every claim of its next head; never claimed ones (attempts = 0), so no backoff of a
			-- row's own is raised. At most $3 rows (nearest first), so the claim stays well inside
			-- its timeout however long the queues behind its heads are: rows past the cap are only
			-- read by later scans (and blocked by the head-of-line check) until a head resolves.
			SELECT l.id
			FROM claimed c
			JOIN outbox_events l ON l.aggregate_id = c.aggregate_id AND l.seq > c.seq
			WHERE l.status = `+outboxPending+` AND l.attempts = 0 AND l.next_attempt_at <= now()
			ORDER BY l.seq
			LIMIT $3
			FOR UPDATE OF l SKIP LOCKED
		), hold AS (
			UPDATE outbox_events o SET next_attempt_at = now() + make_interval(secs => $2::float8)
			FROM held WHERE o.id = held.id
		)
		SELECT id, seq, aggregate_type, aggregate_id, event_type, payload, created_at, attempts, claimed_at
		FROM claimed`, limit, OutboxInflightHold.Seconds(), OutboxHoldLimit)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OutboxEvent, error) {
		var item OutboxEvent
		var aggregate, event int16
		if err := row.Scan(&item.ID, &item.Seq, &aggregate, &item.AggregateID, &event, &item.Payload,
			&item.CreatedAt, &item.Attempts, &item.ClaimedAt); err != nil {
			return item, err
		}
		item.AggregateType, item.EventType = domain.AggregateType(aggregate), domain.EventType(event)
		return item, nil
	})
	if err != nil {
		return nil, err
	}
	// RETURNING order is unspecified. Per-aggregate order is already enforced by the claim (one row
	// per aggregate per batch); seq order just keeps the batch close to commit order overall.
	slices.SortFunc(items, func(a, b OutboxEvent) int { return cmp.Compare(a.Seq, b.Seq) })
	return items, nil
}

// OutboxInflightHold is how long ClaimOutbox holds back the never-claimed rows queued behind a
// claimed head (about one lease: a crashed worker's head is reclaimed around then).
const OutboxInflightHold = 30 * time.Second

// OutboxHoldLimit caps how many queued rows one claim holds back, and how many a backed-off head
// pushes (MarkOutboxRetryAfter), so a huge backlog behind its heads (e.g. after a long broker
// outage) can never push the claim or a mark past its timeout, or rewrite the whole backlog on every
// retry; the hold is only a cost hint.
const OutboxHoldLimit = 2000

// The rows queued behind a head (same aggregate, higher seq, pending) are blocked by the claim's
// head-of-line check whatever their next_attempt_at says, so for them next_attempt_at is only a
// cost hint: it keeps them out of the claim's scan while the head is in flight or backs off. Three
// rules keep that hint from delaying a row past its predecessor:
//   - the claim holds back only rows never claimed (attempts = 0), which have no backoff of their
//     own, and for OutboxInflightHold at most;
//   - a head that goes back to pending pushes the rows behind it up to its own next_attempt_at
//     (never down), and a head cannot resolve before it is claimed again at that time;
//   - a head that resolves (published, failed, poisoned) releases the next pending row if it still
//     carries a future time and was never claimed (attempts = 0: a claimed row's next_attempt_at
//     may be its own backoff, e.g. the wait after an unknown publish outcome, and must stand).
//     The rows after that one stay blocked by it as the new head, and are released in turn.
//
// shiftedSQL completes a statement whose CTE "shift" lists (id, until) for such rows. It locks them
// in id order first, the order RequeueFailedOutboxIdle locks pending rows in, so the two never
// deadlock (the marks hold only their own processing rows before this; the requeue never locks
// processing rows).
var shiftedSQL = `, locked AS (
		SELECT o.id, s.until
		FROM outbox_events o
		JOIN shift s ON s.id = o.id
		WHERE o.status = ` + outboxPending + `
		ORDER BY o.id
		FOR UPDATE OF o
	)
	UPDATE outbox_events o SET next_attempt_at = k.until
	FROM locked k
	WHERE o.id = k.id AND o.status = ` + outboxPending

// releaseNextSQL lists, for every row of CTE "done" (aggregate_id, seq) that resolved, the next
// pending row of its aggregate if it is still pushed into the future (see shiftedSQL).
var releaseNextSQL = `
		SELECT n.id, now() AS until
		FROM done d
		CROSS JOIN LATERAL (
			SELECT l.id, l.next_attempt_at, l.attempts FROM outbox_events l
			WHERE l.aggregate_id = d.aggregate_id AND l.seq > d.seq AND l.status = ` + outboxPending + `
			ORDER BY l.seq
			LIMIT 1
		) n
		WHERE n.attempts = 0 AND n.next_attempt_at > now()`

// MarkOutboxPublished marks rows published if this worker's lease (claimedAt) is still current;
// rows reclaimed by another worker after a lease expiry are left to that worker. The next event of
// each aggregate is released if it is still held back (see shiftedSQL).
func (s *Store) MarkOutboxPublished(ctx context.Context, ids []uuid.UUID, claimedAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		WITH done AS (
			UPDATE outbox_events
			SET status = `+outboxPublished+`, published_at = now(), claimed_at = NULL, last_error = NULL
			WHERE id = ANY($1) AND status = `+outboxProcessing+` AND claimed_at = $2
			RETURNING aggregate_id, seq
		), shift AS (`+releaseNextSQL+`
		)`+shiftedSQL, ids, claimedAt)
	return err
}

// MarkOutboxRetryAfter returns rows to pending with exponential backoff (2^attempts s, max 60s;
// the exponent is capped first, since power(2, attempts) overflows float8 for attempts >= 1024),
// and at least minDelay: the outbox passes MaxWriteLifetime when a publish was abandoned with its
// outcome unknown, so the rows cannot be published again while the abandoned batch may still land.
// maxRejections > 0 means the broker rejected these specific messages: count a rejection and
// poison (status failed) once maxRejections is reached. maxRejections <= 0 is a transient failure
// (broker unreachable, leader election, timeouts): back off only, never count toward poisoning.
//
// A row that goes back to pending pushes the rows queued behind it (at most OutboxHoldLimit, the
// nearest) to its next_attempt_at, and a row that fails releases the next one (see shiftedSQL);
// both happen in the same statement.
func (s *Store) MarkOutboxRetryAfter(ctx context.Context, ids []uuid.UUID, claimedAt time.Time, reason string, maxRejections int, minDelay time.Duration) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		WITH marked AS (
			UPDATE outbox_events
			SET rejections = rejections + CASE WHEN $4 > 0 THEN 1 ELSE 0 END,
			    status = CASE WHEN $4 > 0 AND rejections + 1 >= $4 THEN `+outboxFailed+` ELSE `+outboxPending+` END,
			    next_attempt_at = now() + make_interval(secs => GREATEST($5::float8, LEAST(60, power(2, LEAST(attempts, 6)))::float8)),
			    claimed_at = NULL,
			    last_error = left($3, 500)
			WHERE id = ANY($1) AND status = `+outboxProcessing+` AND claimed_at = $2
			RETURNING aggregate_id, seq, status, next_attempt_at
		), done AS (
			SELECT aggregate_id, seq FROM marked WHERE status = `+outboxFailed+`
		), shift AS (
			-- At most $6 rows per head (nearest first), like the claim's hold: during a broker
			-- outage every retry would otherwise rewrite the whole backlog behind every head. Rows
			-- past the cap stay due and are only read (and blocked by the head-of-line check) by
			-- the claim's scan until they come within the cap of a later retry or a release.
			SELECT l.id, m.next_attempt_at AS until
			FROM marked m
			CROSS JOIN LATERAL (
				SELECT q.id FROM outbox_events q
				WHERE q.aggregate_id = m.aggregate_id AND q.seq > m.seq
				  AND q.status = `+outboxPending+` AND q.next_attempt_at < m.next_attempt_at
				ORDER BY q.seq
				LIMIT $6
			) l
			WHERE m.status = `+outboxPending+`
			UNION ALL`+releaseNextSQL+`
		)`+shiftedSQL, ids, claimedAt, reason, maxRejections, minDelay.Seconds(), OutboxHoldLimit)
	return err
}

// OutboxPoison is a claimed row that cannot be turned into an event at all (unreadable payload).
type OutboxPoison struct {
	ID     uuid.UUID
	Reason string
}

// MarkOutboxPoison fails claimed rows at once (no retry can help), in one statement, fenced by the
// lease like every other mark, and releases the next event of each aggregate (see shiftedSQL).
func (s *Store) MarkOutboxPoison(ctx context.Context, rows []OutboxPoison, claimedAt time.Time) error {
	if len(rows) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(rows))
	reasons := make([]string, len(rows))
	for i, r := range rows {
		ids[i], reasons[i] = r.ID, r.Reason
	}
	_, err := s.pool.Exec(ctx, `
		WITH done AS (
			UPDATE outbox_events o
			SET status = `+outboxFailed+`, claimed_at = NULL, last_error = left(f.reason, 500)
			FROM unnest($1::uuid[], $2::text[]) AS f(id, reason)
			WHERE o.id = f.id AND o.status = `+outboxProcessing+` AND o.claimed_at = $3
			RETURNING o.aggregate_id, o.seq
		), shift AS (`+releaseNextSQL+`
		)`+shiftedSQL, ids, reasons, claimedAt)
	return err
}

// ReclaimStaleOutbox returns rows whose lease expired (worker crashed mid-publish) to pending.
// The lease is measured from claimed_at, never created_at, so old backlog rows that are being
// published right now are not stolen by another instance.
func (s *Store) ReclaimStaleOutbox(ctx context.Context, lease time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE outbox_events
		SET status = `+outboxPending+`, claimed_at = NULL
		WHERE status = `+outboxProcessing+`
		  AND claimed_at < now() - make_interval(secs => $1::float8)`, lease.Seconds())
	return tag.RowsAffected(), err
}

// PurgePublishedOutbox deletes published rows older than retention, at most limit per call. Rows
// another instance's purge is deleting right now are skipped, not waited on (every instance purges
// on the same schedule; waiting would end in lock_timeout).
func (s *Store) PurgePublishedOutbox(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM outbox_events
		WHERE id IN (
			SELECT id FROM outbox_events
			WHERE status = `+outboxPublished+`
			  AND published_at < now() - make_interval(secs => $1::float8)
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)`, retention.Seconds(), limit)
	return tag.RowsAffected(), err
}

// ListenOutbox blocks on LISTEN and calls wake for every commit that wrote outbox rows.
// It returns when ctx ends or the connection fails; callers reconnect with backoff.
func (s *Store) ListenOutbox(ctx context.Context, wake func()) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN `+OutboxChannel); err != nil {
		return err
	}
	defer func() {
		// Best effort: do not hand a LISTENing connection back to the pool.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanupCtx, `UNLISTEN *`)
	}()
	for {
		if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
			return err
		}
		wake()
	}
}
