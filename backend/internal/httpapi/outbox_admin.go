package httpapi

import (
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// failedOutboxEvent is the JSON shape of store.FailedOutboxEvent.
type failedOutboxEvent struct {
	ID            uuid.UUID            `json:"id"`
	Seq           int64                `json:"seq"`
	AggregateType domain.AggregateType `json:"aggregateType"`
	AggregateID   uuid.UUID            `json:"aggregateId"`
	EventType     domain.EventType     `json:"eventType"`
	Attempts      int                  `json:"attempts"`
	Rejections    int                  `json:"rejections"`
	LastError     string               `json:"lastError"`
	CreatedAt     time.Time            `json:"createdAt"`
}

// listFailedOutbox serves GET /admin/outbox/failed (group admin; booking.Service checks).
func (s *Server) listFailedOutbox(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	limit, ok := s.pageLimit(c, store.DefaultPageLimit, booking.MaxFailedOutboxList)
	if !ok {
		return
	}
	scope := newPageScope("outbox-failed")
	after, ok := cursor(s, c, scope, 2, parseOutboxCursor)
	if !ok {
		return
	}
	items, next, err := s.svc.ListFailedOutbox(c.Request.Context(), actor, limit, after)
	out := make([]failedOutboxEvent, len(items))
	for i, e := range items {
		out[i] = failedOutboxEvent(e)
	}
	s.page(c, out, nextToken(scope, next, outboxCursorKeys), err)
}

// requeueOutbox serves POST /admin/outbox/requeue (group admin; booking.Service checks).
func (s *Server) requeueOutbox(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var body struct {
		IDs []uuid.UUID `json:"ids"`
	}
	if !s.bindJSON(c, &body, "request body must be {\"ids\": [outbox event ids]}") {
		return
	}
	n, skipped, err := s.svc.RequeueFailedOutbox(c.Request.Context(), actor, body.IDs)
	if err != nil {
		s.fail(c, err)
		return
	}
	s.log.Info("outbox events requeued", "actor", actor.UserID, "requested", len(body.IDs), "requeued", n, "skipped", len(skipped))
	s.ok(c, gin.H{"requeued": n, "skipped": skipped})
}
