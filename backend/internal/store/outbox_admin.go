package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// FailedOutboxEvent is a poisoned outbox row as an operator sees it. The payload is left out: it
// may carry customer PII and the operator only needs to decide whether to requeue.
type FailedOutboxEvent struct {
	ID            uuid.UUID
	Seq           int64
	AggregateType domain.AggregateType
	AggregateID   uuid.UUID
	EventType     domain.EventType
	Attempts      int
	Rejections    int
	LastError     string
	CreatedAt     time.Time
}

// ListFailedOutbox returns one page of failed rows, oldest first ((created_at, id):
// outbox_events_failed_page_idx).
func (s *Store) ListFailedOutbox(ctx context.Context, limit int, after *OutboxCursor) ([]FailedOutboxEvent, *OutboxCursor, error) {
	sql, args := failedOutboxPageSQL(limit, after)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, translate(err)
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (FailedOutboxEvent, error) {
		var e FailedOutboxEvent
		var aggregate, event int16
		if err := row.Scan(&e.ID, &e.Seq, &aggregate, &e.AggregateID, &event,
			&e.Attempts, &e.Rejections, &e.LastError, &e.CreatedAt); err != nil {
			return e, err
		}
		e.AggregateType, e.EventType = domain.AggregateType(aggregate), domain.EventType(event)
		return e, nil
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := cutPage(items, pageLimit(limit), func(e FailedOutboxEvent) OutboxCursor {
		return OutboxCursor{CreatedAt: e.CreatedAt, ID: e.ID}
	})
	return items, next, nil
}

func failedOutboxPageSQL(limit int, after *OutboxCursor) (string, []any) {
	var head = `
		SELECT id, seq, aggregate_type, aggregate_id, event_type, attempts, rejections,
			COALESCE(last_error, ''), created_at
		FROM outbox_events
		WHERE status = ` + outboxFailed
	const order = ` ORDER BY created_at, id LIMIT `
	limit = pageLimit(limit) + 1
	if after == nil {
		return head + order + `$1`, []any{limit}
	}
	return head + ` AND created_at >= $1 AND (created_at, id) > ($1, $2)` + order + `$3`, []any{after.CreatedAt, after.ID, limit}
}

// RequeueFailedOutboxIdle is the operator's tool once the cause of a poisoned row is fixed (a
// broker limit raised, a payload repaired): it returns the given failed rows to pending, with their
// rejection count reset, except those whose aggregate has an event in flight (processing), which it
// reports as skipped so the operator can retry once that publish ends. Rows in any other status are
// left alone. ClaimOutbox already refuses to claim a row while any row of its aggregate is
// processing; this keeps the operator's answer honest (skipped, not silently delayed) and is a
// second guard.
//
// Ordering: a failed row no longer blocks its aggregate (ClaimOutbox only waits on pending and
// processing rows), so newer events of that aggregate may already be published; a requeued row
// then reaches consumers after them. It keeps its seq, so it still goes out before any newer event
// of the aggregate that is not published yet. Consumers ignore stale versions (AppointmentNotice
// carries the appointment version, notifications dedupe and check staleness), so a late event is
// harmless, but it is the operator's call.
//
// Two statements on purpose: first lock the aggregates' pending rows (a claim that already holds
// one makes us wait until it is processing; one we hold is skipped by the claim's SKIP LOCKED),
// then requeue in a fresh snapshot that sees those statuses as committed. Processing rows are only
// read, never locked: the outbox marks lock their processing rows and then the pending rows behind
// them, so locking both kinds here could deadlock with a mark. Pending rows are locked in id order,
// the order the marks use (store.shiftedSQL).
func (s *Store) RequeueFailedOutboxIdle(ctx context.Context, ids []uuid.UUID) (requeued int64, skipped []uuid.UUID, err error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		SELECT p.id
		FROM outbox_events p
		WHERE p.aggregate_id IN (SELECT f.aggregate_id FROM outbox_events f WHERE f.id = ANY($1) AND f.status = `+outboxFailed+`)
		  AND p.status = `+outboxPending+`
		ORDER BY p.id
		FOR UPDATE OF p`, ids); err != nil {
		return 0, nil, translate(err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE outbox_events o
		SET status = `+outboxPending+`, rejections = 0, next_attempt_at = now(), claimed_at = NULL
		WHERE o.id = ANY($1) AND o.status = `+outboxFailed+`
		  AND NOT EXISTS (
			SELECT 1 FROM outbox_events p
			WHERE p.aggregate_id = o.aggregate_id AND p.status = `+outboxProcessing+`)`, ids)
	if err != nil {
		return 0, nil, translate(err)
	}
	rows, err := tx.Query(ctx, `
		SELECT o.id FROM outbox_events o
		WHERE o.id = ANY($1) AND o.status = `+outboxFailed+`
		  AND EXISTS (
			SELECT 1 FROM outbox_events p
			WHERE p.aggregate_id = o.aggregate_id AND p.status = `+outboxProcessing+`)
		ORDER BY o.id`, ids)
	if err != nil {
		return 0, nil, translate(err)
	}
	if skipped, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID]); err != nil {
		return 0, nil, translate(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, translate(err)
	}
	if tag.RowsAffected() > 0 {
		s.signalOutbox() // due now: publish them without waiting for the next poll
	}
	return tag.RowsAffected(), skipped, nil
}
