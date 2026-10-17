package integration_test

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func TestHTTPDMSEndpoints(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	limiter := newTestLimiter()
	handler := newAPI(st, svc, httpapi.WithRateLimiter(limiter))
	admin := login(t, handler, seed.DemoAdminEmail, seed.DemoAdminPassword)
	dispatcher := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	customer := login(t, handler, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	tomorrow := londonDay(t, 1).String()

	// Multi-line booking.
	var appt domain.Appointment
	admin.expect(admin.do(http.MethodPost, "/api/v1/appointments", map[string]any{
		"dealershipId": seed.DealerNorth, "vehicleId": seed.VehicleB,
		"serviceTypeIds": []string{seed.ServiceOil.String(), seed.ServiceBrake.String()},
		"startAt":        londonOn(t, londonDay(t, 1), 10, 0).Format(time.RFC3339),
	}, nil), http.StatusCreated, &appt)
	if len(appt.Lines) != 2 || appt.DurationMinutes != 120 {
		t.Fatalf("appointment = %+v", appt)
	}
	admin.expect(admin.do(http.MethodPost, "/api/v1/appointments", map[string]any{
		"dealershipId": seed.DealerNorth, "vehicleId": seed.VehicleB, "serviceTypeId": seed.ServiceOil,
		"serviceTypeIds": []string{seed.ServiceBrake.String()}, "startAt": londonOn(t, londonDay(t, 1), 14, 0).Format(time.RFC3339),
	}, nil), http.StatusBadRequest, nil)

	// First available, across days, for several lines.
	var next domain.NextAvailability
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/availability/next?dealershipId="+seed.DealerNorth.String()+
		"&vehicleId="+seed.VehicleC.String()+"&serviceTypeIds="+seed.ServiceOil.String()+","+seed.ServiceBrake.String()+
		"&from="+tomorrow+"&limit=2", nil, nil), http.StatusOK, &next)
	if len(next.Slots) != 2 || next.DurationMinutes != 120 {
		t.Fatalf("next = %+v", next)
	}
	// One start per day: the two come from different days, in order.
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/availability/next?dealershipId="+seed.DealerNorth.String()+
		"&vehicleId="+seed.VehicleC.String()+"&serviceTypeId="+seed.ServiceOil.String()+
		"&from="+tomorrow+"&limit=2&perDay=1", nil, nil), http.StatusOK, &next)
	if len(next.Slots) != 2 || next.Slots[0].Date >= next.Slots[1].Date {
		t.Fatalf("perDay=1: %+v", next.Slots)
	}
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/availability/next?dealershipId="+seed.DealerNorth.String()+
		"&vehicleId="+seed.VehicleC.String()+"&serviceTypeId="+seed.ServiceOil.String()+"&perDay=0", nil, nil), http.StatusBadRequest, nil)
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/availability/next?dealershipId="+seed.DealerNorth.String()+
		"&vehicleId="+seed.VehicleC.String()+"&serviceTypeId="+seed.ServiceOil.String()+"&limit=50", nil, nil), http.StatusBadRequest, nil)

	// Reassign to the other general bay, then the audit trail.
	otherBay := seed.BayN3
	if appt.ServiceBayID == seed.BayN3 {
		otherBay = seed.BayN2
	}
	path := "/api/v1/appointments/" + appt.ID.String()
	var moved domain.Appointment
	rec := dispatcher.do(http.MethodPost, path+"/reassign", map[string]string{"serviceBayId": otherBay.String()}, map[string]string{"If-Match": `"1"`})
	dispatcher.expect(rec, http.StatusOK, &moved)
	if moved.ServiceBayID != otherBay || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("moved bay=%s etag=%s", moved.ServiceBayName, rec.Header().Get("ETag"))
	}
	customer.expect(customer.do(http.MethodPost, path+"/reassign", map[string]string{"serviceBayId": otherBay.String()}, nil), http.StatusForbidden, nil)
	var history []domain.AppointmentChange
	dispatcher.expect(dispatcher.do(http.MethodGet, path+"/history", nil, nil), http.StatusOK, &history)
	if len(history) != 2 || history[1].EventType != domain.EventAppointmentReassigned {
		t.Fatalf("history = %+v", history)
	}
	customer.expect(customer.do(http.MethodGet, path+"/history", nil, nil), http.StatusForbidden, nil)

	// Technician roster.
	roster := "/api/v1/admin/technicians/" + seed.TechEve.String()
	var r domain.TechnicianRoster
	rec = admin.do(http.MethodPut, roster+"/shifts", map[string]any{"shifts": []map[string]any{
		{"weekday": 1, "startTime": "08:00", "endTime": "16:00", "breakStart": "12:00", "breakEnd": "12:30"},
	}}, nil)
	admin.expect(rec, http.StatusOK, &r)
	if len(r.Shifts) != 1 || rec.Header().Get("ETag") == "" {
		t.Fatalf("roster = %+v", r)
	}
	admin.expect(admin.do(http.MethodPut, roster+"/shifts", map[string]any{"shifts": []map[string]any{
		{"weekday": 9, "startTime": "08:00", "endTime": "16:00"},
	}}, nil), http.StatusBadRequest, nil)
	far := londonDay(t, 20)
	admin.expect(admin.do(http.MethodPost, roster+"/absences", map[string]string{
		"startsAt": londonOn(t, far, 8, 0).Format(time.RFC3339), "endsAt": londonOn(t, far, 18, 0).Format(time.RFC3339), "reason": "Leave",
	}, nil), http.StatusCreated, &r)
	if len(r.Absences) != 1 {
		t.Fatalf("absences = %+v", r.Absences)
	}
	admin.expect(admin.do(http.MethodDelete, roster+"/absences/"+r.Absences[0].ID.String(), nil,
		map[string]string{"If-Match": `"` + strconv.Itoa(r.Version) + `"`}), http.StatusOK, &r)
	dispatcher.expect(dispatcher.do(http.MethodGet, roster+"/roster", nil, nil), http.StatusForbidden, nil)

	// Dealership policy.
	dealer := "/api/v1/admin/dealerships/" + seed.DealerRiver.String()
	admin.expect(admin.do(http.MethodPatch, dealer, map[string]any{"policy": map[string]any{"bookingHorizonDays": 0}}, nil), http.StatusBadRequest, nil)
	admin.expect(admin.do(http.MethodPatch, dealer, map[string]any{"stagingWindowMinutes": 45}, nil), http.StatusBadRequest, nil)
	var d domain.Dealership
	admin.expect(admin.do(http.MethodPatch, dealer, map[string]any{
		"stagingWindowMinutes": 30, "policy": map[string]any{"bookingHorizonDays": 30, "expressMaxMinutes": 0},
	}, nil), http.StatusOK, &d)
	if d.StagingWindowMinutes != 30 || d.Policy.BookingHorizonDays != 30 || d.Policy.ExpressMaxMinutes != 0 || d.Policy.CustomerLeadMinutes != 60 {
		t.Fatalf("dealer = %+v", d)
	}

	// Customers are throttled per user on availability; staff are not.
	limiter.denyPrefix("availability:user:")
	q := "/api/v1/availability?dealershipId=" + seed.DealerRiver.String() + "&vehicleId=" + seed.VehicleA.String() +
		"&serviceTypeId=" + seed.ServiceOil.String() + "&date=" + tomorrow
	rec = customer.do(http.MethodGet, q, nil, nil)
	customer.expect(rec, http.StatusTooManyRequests, nil)
	if rec.Header().Get("Retry-After") != "60" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	admin.expect(admin.do(http.MethodGet, q, nil, nil), http.StatusOK, nil)
}

// A booking that waits on a lock longer than lock_timeout fails fast with 503 + Retry-After
// instead of piling up on the pool.
func TestHTTPBusyDatabaseReturns503(t *testing.T) {
	t.Parallel()
	pool, _, _, ctx := openTestDB(t)
	busyPool := newPoolLike(t, pool, store.PoolOptions{MaxConns: 4, LockTimeout: 200 * time.Millisecond, StatementTimeout: 5 * time.Second})
	busySt := store.New(busyPool)
	handler := newAPI(busySt, atStartOfLondonDay(busySt))
	admin := login(t, handler, seed.DemoAdminEmail, seed.DemoAdminPassword)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM dealerships WHERE id = $1 FOR UPDATE`, seed.DealerRiver); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"dealershipId": seed.DealerRiver, "vehicleId": seed.VehicleA, "serviceTypeId": seed.ServiceOil,
		"startAt": londonOn(t, londonDay(t, 1), 9, 0).Format(time.RFC3339),
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	rec := admin.do(http.MethodPost, "/api/v1/appointments", body, nil)
	admin.expect(rec, http.StatusServiceUnavailable, &failure)
	if failure.Error.Code != domain.CodeBusy || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("code=%s retry-after=%q", failure.Error.Code, rec.Header().Get("Retry-After"))
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	admin.expect(admin.do(http.MethodPost, "/api/v1/appointments", body, nil), http.StatusCreated, nil)
}
