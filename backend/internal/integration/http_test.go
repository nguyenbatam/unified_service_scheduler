package integration_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// errorEnvelope is the body of every API error.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// slotAvailable reads the availability of start from c's GET /availability for VehicleA's oil
// change at Riverside on start's day.
func slotAvailable(t *testing.T, c apiClient, start time.Time) bool {
	t.Helper()
	day := londonDay(t, 0)
	var avail domain.Availability
	c.expect(c.do(http.MethodGet, fmt.Sprintf(
		"/api/v1/availability?dealershipId=%s&vehicleId=%s&serviceTypeId=%s&date=%s",
		seed.DealerRiver, seed.VehicleA, seed.ServiceOil, day), nil, nil), http.StatusOK, &avail)
	if avail.Date != day.String() || avail.Timezone != "Europe/London" || avail.DurationMinutes != 30 {
		t.Fatalf("availability = date %s tz %s duration %d", avail.Date, avail.Timezone, avail.DurationMinutes)
	}
	for _, slot := range avail.Slots {
		if slot.Start.Equal(start) {
			return slot.Available
		}
	}
	t.Fatalf("no %s slot in %+v", start.Format(time.RFC3339), avail.Slots)
	return false
}

func TestHTTPBookReplayAndCancel(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	api := newAPI(st, svc)
	admin := login(t, api, seed.DemoAdminEmail, seed.DemoAdminPassword)
	start := londonAt(t, 11, 30)
	key := map[string]string{"Idempotency-Key": "http:idem:" + uuid.NewString()}
	body := map[string]string{
		"dealershipId":  seed.DealerRiver.String(),
		"vehicleId":     seed.VehicleA.String(),
		"serviceTypeId": seed.ServiceOil.String(),
		"startAt":       start.Format(time.RFC3339),
	}
	if !slotAvailable(t, admin, start) {
		t.Fatal("precondition: Riverside 11:30 must be free")
	}

	var created domain.Appointment
	first := admin.do(http.MethodPost, "/api/v1/appointments", body, key)
	admin.expect(first, http.StatusCreated, &created)
	if created.ID == uuid.Nil || created.Status != domain.StatusConfirmed || !created.StartAt.Equal(start) || created.Version != 1 {
		t.Fatalf("created = %+v", created)
	}
	location := "/api/v1/appointments/" + created.ID.String()
	if got := first.Header().Get("Location"); got != location {
		t.Fatalf("Location = %q, want %q", got, location)
	}
	if slotAvailable(t, admin, start) {
		t.Fatal("the booked slot is still offered")
	}

	// Same key + payload: the original appointment, 200 instead of 201.
	var replayed domain.Appointment
	again := admin.do(http.MethodPost, "/api/v1/appointments", body, key)
	admin.expect(again, http.StatusOK, &replayed)
	if replayed.ID != created.ID || replayed.Version != created.Version {
		t.Fatalf("replay = %s v%d, want %s v%d", replayed.ID, replayed.Version, created.ID, created.Version)
	}
	if got := again.Header().Get("Location"); got != location {
		t.Fatalf("replay Location = %q, want %q", got, location)
	}

	// A waiter flag that is not a boolean is refused, not read as false.
	admin.expect(admin.do(http.MethodGet, "/api/v1/availability?dealershipId="+seed.DealerRiver.String()+
		"&vehicleId="+seed.VehicleA.String()+"&serviceTypeId="+seed.ServiceOil.String()+
		"&date="+londonDay(t, 0).String()+"&isWaiter=yes", nil, nil), http.StatusBadRequest, nil)

	var cancelled domain.Appointment
	admin.expect(admin.do(http.MethodPost, "/api/v1/appointments/"+created.ID.String()+"/cancel", nil, nil), http.StatusOK, &cancelled)
	if cancelled.ID != created.ID || cancelled.Status != domain.StatusCancelled || cancelled.Version != created.Version+1 {
		t.Fatalf("cancelled = %s status %s v%d", cancelled.ID, cancelled.Status.Name(), cancelled.Version)
	}
	if !slotAvailable(t, admin, start) {
		t.Fatal("the cancelled slot is not offered again")
	}

	anon := apiClient{t: t, handler: api}
	anon.expect(anon.do(http.MethodGet, "/api/v1/health", nil, nil), http.StatusOK, nil)
}

func TestHTTPErrorEnvelope(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	admin := login(t, newAPI(st, svc), seed.DemoAdminEmail, seed.DemoAdminPassword)
	var failure errorEnvelope
	admin.expect(admin.do(http.MethodPost, "/api/v1/appointments", map[string]any{}, nil), http.StatusBadRequest, &failure)
	if failure.Error.Code != domain.CodeInvalid || failure.Error.Message == "" {
		t.Fatalf("error = %+v", failure.Error)
	}
}

func TestHTTPRegisterLoginMe(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	anon := apiClient{t: t, handler: newAPI(st, svc)}
	email := fmt.Sprintf("cust-%s@example.com", uuid.NewString()[:8])

	var session struct {
		Token string      `json:"token"`
		User  domain.User `json:"user"`
	}
	anon.expect(anon.do(http.MethodPost, "/api/v1/auth/register",
		map[string]string{"email": email, "password": "secret123", "name": "Casey Customer"}, nil), http.StatusCreated, &session)
	if session.Token == "" || session.User.Role != domain.RoleCustomer || session.User.Email != email || session.User.CustomerID == uuid.Nil {
		t.Fatalf("register session = %+v", session)
	}

	me := apiClient{t: t, handler: anon.handler, token: session.Token}
	var user domain.User
	me.expect(me.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, &user)
	if user.ID != session.User.ID || user.Email != email || user.Role != domain.RoleCustomer {
		t.Fatalf("me = %+v, want %+v", user, session.User)
	}
	// The new password logs in.
	if again := login(t, anon.handler, email, "secret123"); again.token == "" {
		t.Fatal("login after register returned no token")
	}
}
