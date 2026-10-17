package booking

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Bounds of the outbox operator endpoints.
const (
	MaxFailedOutboxList = 200
	MaxOutboxRequeue    = 500
)

// outboxOperator allows only a group admin: the outbox carries every dealership's events.
func outboxOperator(actor domain.Actor) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	if actor.DealershipID != uuid.Nil {
		return domain.NewError(domain.CodeForbidden, "the event outbox is shared by every dealership; a group admin must manage it")
	}
	return nil
}

// ListFailedOutbox lists one page of failed (poisoned) outbox rows, oldest first, for a group admin.
func (s *Service) ListFailedOutbox(ctx context.Context, actor domain.Actor, limit int, after *store.OutboxCursor) ([]store.FailedOutboxEvent, *store.OutboxCursor, error) {
	if err := outboxOperator(actor); err != nil {
		return nil, nil, err
	}
	if limit < 1 || limit > MaxFailedOutboxList {
		return nil, nil, domain.NewError(domain.CodeInvalid, fmt.Sprintf("limit must be 1–%d", MaxFailedOutboxList))
	}
	items, next, err := s.store.ListFailedOutbox(ctx, limit, after)
	if items == nil {
		items = []store.FailedOutboxEvent{}
	}
	return items, next, err
}

// RequeueFailedOutbox returns failed outbox rows to pending once their cause is fixed and reports
// how many moved, plus the ids skipped because their aggregate has an event in flight right now
// (retry them shortly). Ids that are not failed (already requeued, published, unknown) are
// ignored. A requeued event may reach consumers after newer, already published events of its
// aggregate (see store.RequeueFailedOutboxIdle); consumers ignore stale versions.
func (s *Service) RequeueFailedOutbox(ctx context.Context, actor domain.Actor, ids []uuid.UUID) (int64, []uuid.UUID, error) {
	if err := outboxOperator(actor); err != nil {
		return 0, nil, err
	}
	if len(ids) == 0 || len(ids) > MaxOutboxRequeue {
		return 0, nil, domain.NewError(domain.CodeInvalid, fmt.Sprintf("ids must hold 1–%d outbox event ids", MaxOutboxRequeue))
	}
	n, skipped, err := s.store.RequeueFailedOutboxIdle(ctx, ids)
	if skipped == nil {
		skipped = []uuid.UUID{}
	}
	return n, skipped, err
}
