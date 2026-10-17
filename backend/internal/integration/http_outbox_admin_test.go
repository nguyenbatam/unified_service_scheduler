package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Only a group admin may list and requeue failed outbox events; a requeued event is pending again
// with its rejections reset, requeuing it twice moves nothing, and an event whose aggregate has a
// newer event in flight is skipped (two in flight would break per-aggregate order).
func TestHTTPOutboxAdminListAndRequeue(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	h := newAPI(st, svc)

	hash, err := auth.HashPassword("scoped-admin-pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureUser(ctx, domain.User{
		ID: uuid.New(), Email: "north-admin@scheduler.test", Name: "North Admin", PasswordHash: hash,
		Role: domain.RoleAdmin, DealershipID: seed.DealerNorth,
	}); err != nil {
		t.Fatal(err)
	}

	appt, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 8, 0)))
	if err != nil {
		t.Fatal(err)
	}
	eventID := scalar[uuid.UUID](t, pool, `
		UPDATE outbox_events SET status = `+code(domain.OutboxFailed)+`, rejections = 3, attempts = 3, last_error = 'message too large'
		WHERE aggregate_id = $1 AND event_type = `+code(domain.EventAppointmentCreated)+` RETURNING id`, appt.ID)

	groupAdmin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)
	scopedAdmin := login(t, h, "north-admin@scheduler.test", "scoped-admin-pass")
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	requeue := map[string]any{"ids": []uuid.UUID{eventID}}
	for name, c := range map[string]apiClient{"customer": customer, "dispatcher": dispatcher, "scoped admin": scopedAdmin} {
		t.Run(name+" is refused", func(t *testing.T) {
			c.t = t
			c.expect(c.do(http.MethodGet, "/api/v1/admin/outbox/failed", nil, nil), http.StatusForbidden, nil)
			c.expect(c.do(http.MethodPost, "/api/v1/admin/outbox/requeue", requeue, nil), http.StatusForbidden, nil)
		})
	}

	groupAdmin.expect(groupAdmin.do(http.MethodGet, "/api/v1/admin/outbox/failed?limit=201", nil, nil), http.StatusBadRequest, nil)
	groupAdmin.expect(groupAdmin.do(http.MethodPost, "/api/v1/admin/outbox/requeue", map[string]any{"ids": []uuid.UUID{}}, nil), http.StatusBadRequest, nil)

	var failed []struct {
		ID          uuid.UUID        `json:"id"`
		AggregateID uuid.UUID        `json:"aggregateId"`
		EventType   domain.EventType `json:"eventType"`
		Rejections  int              `json:"rejections"`
		LastError   string           `json:"lastError"`
	}
	groupAdmin.expect(groupAdmin.do(http.MethodGet, "/api/v1/admin/outbox/failed", nil, nil), http.StatusOK, &failed)
	found := false
	for _, e := range failed {
		if e.ID == eventID {
			found = true
			if e.AggregateID != appt.ID || e.EventType != domain.EventAppointmentCreated || e.Rejections != 3 || e.LastError != "message too large" {
				t.Fatalf("listed row = %+v", e)
			}
		}
	}
	if !found {
		t.Fatalf("failed event %s not listed: %+v", eventID, failed)
	}

	var moved struct {
		Requeued int         `json:"requeued"`
		Skipped  []uuid.UUID `json:"skipped"`
	}
	// A newer event of the same appointment is being published: the failed one must wait.
	if _, err := svc.Cancel(ctx, staffAdmin, appt.ID, 0); err != nil {
		t.Fatal(err)
	}
	cancelEvent := scalar[uuid.UUID](t, pool, `
		UPDATE outbox_events SET status = `+code(domain.OutboxProcessing)+`, claimed_at = now()
		WHERE aggregate_id = $1 AND event_type = `+code(domain.EventAppointmentCancelled)+` RETURNING id`, appt.ID)
	groupAdmin.expect(groupAdmin.do(http.MethodPost, "/api/v1/admin/outbox/requeue", requeue, nil), http.StatusOK, &moved)
	if moved.Requeued != 0 || len(moved.Skipped) != 1 || moved.Skipped[0] != eventID {
		t.Fatalf("requeue beside an in-flight event = %+v, want 0 requeued, %s skipped", moved, eventID)
	}
	if status := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` FROM outbox_events WHERE id = $1`, eventID); status != "failed" {
		t.Fatalf("skipped event is %s, want still failed", status)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET status = `+code(domain.OutboxPublished)+`, published_at = now() WHERE id = $1`, cancelEvent); err != nil {
		t.Fatal(err)
	}

	groupAdmin.expect(groupAdmin.do(http.MethodPost, "/api/v1/admin/outbox/requeue", requeue, nil), http.StatusOK, &moved)
	if moved.Requeued != 1 || len(moved.Skipped) != 0 {
		t.Fatalf("requeue = %+v, want 1 requeued, none skipped", moved)
	}
	if status := scalar[string](t, pool, `SELECT `+outboxStatusText("status")+` || '/' || rejections FROM outbox_events WHERE id = $1`, eventID); status != "pending/0" {
		t.Fatalf("after requeue = %s, want pending/0", status)
	}
	groupAdmin.expect(groupAdmin.do(http.MethodPost, "/api/v1/admin/outbox/requeue", requeue, nil), http.StatusOK, &moved)
	if moved.Requeued != 0 {
		t.Fatalf("second requeue moved %d, want 0 (no longer failed)", moved.Requeued)
	}
}
