package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Opening a dealership is for group admins; adding bays and technicians for admins of that
// dealership (or group admins). The new resources are bookable at once: availability counts them
// and a booking lands on them, with the CONFIG_UPDATED events in the outbox.
func TestHTTPProvisionWorkshop(t *testing.T) {
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
	groupAdmin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)
	scopedAdmin := login(t, h, "north-admin@scheduler.test", "scoped-admin-pass")
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	anonymous := apiClient{t: t, handler: h}

	newDealer := map[string]any{
		"name": "Eastside Motors", "timezone": "Europe/London", "openTime": "08:00", "closeTime": "17:00",
		"lunchStart": "12:00", "lunchEnd": "12:30", "policy": map[string]any{"expressMaxMinutes": 30},
	}
	anonymous.expect(anonymous.do(http.MethodPost, "/api/v1/admin/dealerships", newDealer, nil), http.StatusUnauthorized, nil)
	for name, c := range map[string]apiClient{"customer": customer, "dispatcher": dispatcher, "dealership admin": scopedAdmin} {
		t.Run(name+" cannot open a dealership", func(t *testing.T) {
			c.t = t
			c.expect(c.do(http.MethodPost, "/api/v1/admin/dealerships", newDealer, nil), http.StatusForbidden, nil)
		})
	}
	for name, body := range map[string]map[string]any{
		"bad timezone":    {"name": "X", "timezone": "Mars/Olympus", "openTime": "08:00", "closeTime": "17:00"},
		"no timezone":     {"name": "X", "openTime": "08:00", "closeTime": "17:00"},
		"local timezone":  {"name": "X", "timezone": "Local", "openTime": "08:00", "closeTime": "17:00"},
		"blank name":      {"name": "  ", "timezone": "Europe/London", "openTime": "08:00", "closeTime": "17:00"},
		"close <= open":   {"name": "X", "timezone": "Europe/London", "openTime": "17:00", "closeTime": "08:00"},
		"half a lunch":    {"name": "X", "timezone": "Europe/London", "openTime": "08:00", "closeTime": "17:00", "lunchStart": "12:00"},
		"lunch outside":   {"name": "X", "timezone": "Europe/London", "openTime": "08:00", "closeTime": "17:00", "lunchStart": "18:00", "lunchEnd": "18:30"},
		"staging window":  {"name": "X", "timezone": "Europe/London", "openTime": "08:00", "closeTime": "17:00", "stagingWindowMinutes": 45},
		"policy in range": {"name": "X", "timezone": "Europe/London", "openTime": "08:00", "closeTime": "17:00", "policy": map[string]any{"noShowGraceMinutes": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			c := groupAdmin
			c.t = t
			c.expect(c.do(http.MethodPost, "/api/v1/admin/dealerships", body, nil), http.StatusBadRequest, nil)
		})
	}
	var dealer domain.Dealership
	rec := groupAdmin.do(http.MethodPost, "/api/v1/admin/dealerships", newDealer, nil)
	groupAdmin.expect(rec, http.StatusCreated, &dealer)
	if dealer.ID == uuid.Nil || dealer.Version != 1 || rec.Header().Get("ETag") != `"1"` || dealer.LunchEnd != "12:30" ||
		dealer.Policy.ExpressMaxMinutes != 30 || dealer.Policy.CustomerLeadMinutes != 60 || dealer.StagingWindowMinutes != 60 || dealer.Today == "" {
		t.Fatalf("created dealership = %+v (ETag %s)", dealer, rec.Header().Get("ETag"))
	}

	bays := "/api/v1/admin/dealerships/" + dealer.ID.String() + "/bays"
	scopedAdmin.expect(scopedAdmin.do(http.MethodPost, bays, map[string]any{"name": "Lift 1", "bayType": 1}, nil), http.StatusForbidden, nil)
	dispatcher.expect(dispatcher.do(http.MethodPost, bays, map[string]any{"name": "Lift 1", "bayType": 1}, nil), http.StatusForbidden, nil)
	groupAdmin.expect(groupAdmin.do(http.MethodPost, bays, map[string]any{"name": "Lift 1", "bayType": 9}, nil), http.StatusBadRequest, nil)
	groupAdmin.expect(groupAdmin.do(http.MethodPost, "/api/v1/admin/dealerships/"+uuid.NewString()+"/bays",
		map[string]any{"name": "Lift 1", "bayType": 1}, nil), http.StatusNotFound, nil)
	var bay domain.Bay
	groupAdmin.expect(groupAdmin.do(http.MethodPost, bays, map[string]any{"name": "Lift 1", "bayType": 1}, nil), http.StatusCreated, &bay)
	if bay.ID == uuid.Nil || bay.BayType != domain.BayExpress || bay.Name != "Lift 1" {
		t.Fatalf("bay = %+v", bay)
	}
	groupAdmin.expect(groupAdmin.do(http.MethodPost, bays, map[string]any{"name": "Lift 1", "bayType": 2}, nil), http.StatusConflict, nil)
	// A dealership admin adds bays at their own dealership.
	scopedAdmin.expect(scopedAdmin.do(http.MethodPost, "/api/v1/admin/dealerships/"+seed.DealerNorth.String()+"/bays",
		map[string]any{"name": "Bay 5", "bayType": 2}, nil), http.StatusCreated, nil)

	techs := "/api/v1/admin/technicians"
	newTech := map[string]any{"dealershipId": dealer.ID, "name": "Gale Ito", "skillLevel": 2, "skills": []int{1, 1, 3}}
	scopedAdmin.expect(scopedAdmin.do(http.MethodPost, techs, newTech, nil), http.StatusForbidden, nil)
	customer.expect(customer.do(http.MethodPost, techs, newTech, nil), http.StatusForbidden, nil)
	for name, body := range map[string]map[string]any{
		"no skills":      {"dealershipId": dealer.ID, "name": "X", "skillLevel": 2, "skills": []int{}},
		"bad skill":      {"dealershipId": dealer.ID, "name": "X", "skillLevel": 2, "skills": []int{7}},
		"bad level":      {"dealershipId": dealer.ID, "name": "X", "skillLevel": 5, "skills": []int{1}},
		"no dealership":  {"name": "X", "skillLevel": 2, "skills": []int{1}},
		"blank name":     {"dealershipId": dealer.ID, "name": "", "skillLevel": 2, "skills": []int{1}},
		"unknown fields": {"dealershipId": dealer.ID, "name": "X", "skillLevel": 2, "skills": []int{1}, "active": false},
	} {
		t.Run(name, func(t *testing.T) {
			c := groupAdmin
			c.t = t
			c.expect(c.do(http.MethodPost, techs, body, nil), http.StatusBadRequest, nil)
		})
	}
	groupAdmin.expect(groupAdmin.do(http.MethodPost, techs, map[string]any{
		"dealershipId": uuid.New(), "name": "X", "skillLevel": 2, "skills": []int{1},
	}, nil), http.StatusNotFound, nil)
	var tech domain.TechnicianAdmin
	groupAdmin.expect(groupAdmin.do(http.MethodPost, techs, newTech, nil), http.StatusCreated, &tech)
	if tech.DealershipID != dealer.ID || !tech.Active || tech.SkillLevel != 2 || len(tech.Skills) != 2 ||
		tech.Skills[0] != domain.SkillLubrication || tech.Skills[1] != domain.SkillGeneral {
		t.Fatalf("technician = %+v (skills deduplicated and sorted)", tech)
	}

	// Bookable at once: availability counts the bay and the technician, and an oil change lands on them.
	day := londonDay(t, 1)
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: dealer.ID, VehicleID: seed.VehicleC, ServiceTypeID: seed.ServiceOil, Day: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(avail.Slots) == 0 || !avail.Slots[0].Available || avail.Slots[0].FreeBays != 1 || avail.Slots[0].FreeTechnicians != 1 {
		t.Fatalf("first slot = %+v, want 1 bay + 1 technician free", avail.Slots)
	}
	var appt domain.Appointment
	groupAdmin.expect(groupAdmin.do(http.MethodPost, "/api/v1/appointments", map[string]any{
		"dealershipId": dealer.ID, "vehicleId": seed.VehicleC, "serviceTypeId": seed.ServiceOil,
		"startAt": londonOn(t, day, 8, 0).Format(time.RFC3339),
	}, nil), http.StatusCreated, &appt)
	if appt.ServiceBayID != bay.ID || appt.TechnicianID != tech.ID {
		t.Fatalf("booked on bay %s / technician %s, want the new ones", appt.ServiceBayName, appt.TechnicianName)
	}
	// Dealership created, then bumped by its bay; the technician has its own event.
	if n := scalar[int](t, pool, `
		SELECT count(*)::int FROM outbox_events WHERE event_type = `+code(domain.EventConfigUpdated)+` AND aggregate_id = ANY($1)`,
		[]uuid.UUID{dealer.ID, tech.ID}); n != 3 {
		t.Fatalf("CONFIG_UPDATED events = %d, want 3 (dealership created, bay added, technician added)", n)
	}
	if v := scalar[int](t, pool, `SELECT version FROM dealerships WHERE id = $1`, dealer.ID); v != 2 {
		t.Fatalf("dealership version = %d, want 2 after adding a bay", v)
	}
}
