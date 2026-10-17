package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func TestHTTPCustomerSelfServiceFlow(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	handler := newAPI(st, svc)
	anon := apiClient{t: t, handler: handler}

	var session struct {
		Token string `json:"token"`
		User  struct {
			Role       int    `json:"role"`
			CustomerID string `json:"customerId"`
		} `json:"user"`
	}
	anon.expect(anon.do(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email": fmt.Sprintf("new-%s@example.com", uuid.NewString()[:8]), "password": "short", "name": "N",
	}, nil), http.StatusBadRequest, nil)
	anon.expect(anon.do(http.MethodPost, "/api/v1/auth/register", map[string]string{
		"email": fmt.Sprintf("new-%s@example.com", uuid.NewString()[:8]), "password": "long-enough", "name": "New Customer",
	}, nil), http.StatusCreated, &session)
	if session.User.CustomerID == "" {
		t.Fatal("registered customer must be linked to a customers row")
	}
	me := apiClient{t: t, handler: handler, token: session.Token}

	var vehicles []map[string]any
	me.expect(me.do(http.MethodGet, "/api/v1/vehicles", nil, nil), http.StatusOK, &vehicles)
	if len(vehicles) != 0 {
		t.Fatalf("new customer must not see other people's cars: %d", len(vehicles))
	}
	var car struct {
		ID string `json:"id"`
	}
	me.expect(me.do(http.MethodPost, "/api/v1/vehicles", map[string]any{
		"vin": "SALAA2BD4FA000001", "make": "Land Rover", "model": "Defender", "year": 2024,
	}, nil), http.StatusCreated, &car)

	rec := me.do(http.MethodPost, "/api/v1/appointments", map[string]any{
		"dealershipId": seed.DealerRiver, "vehicleId": car.ID, "serviceTypeId": seed.ServiceOil,
		"startAt": londonAt(t, 14, 0).Format(time.RFC3339),
	}, map[string]string{"Idempotency-Key": uuid.NewString()})
	var appt struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
	}
	me.expect(rec, http.StatusCreated, &appt)
	if rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("ETag = %q", rec.Header().Get("ETag"))
	}

	day, _ := suiteToday()
	me.expect(me.do(http.MethodGet, "/api/v1/schedule?dealershipId="+seed.DealerRiver.String()+"&date="+day.String(), nil, nil), http.StatusForbidden, nil)
	me.expect(me.do(http.MethodPost, "/api/v1/appointments/"+appt.ID+"/check-in", nil, nil), http.StatusForbidden, nil)
	me.expect(me.do(http.MethodPost, "/api/v1/appointments/"+appt.ID+"/cancel", nil, map[string]string{"If-Match": `"9"`}), http.StatusPreconditionFailed, nil)
	me.expect(me.do(http.MethodPost, "/api/v1/appointments/"+appt.ID+"/cancel", nil, map[string]string{"If-Match": "abc"}), http.StatusBadRequest, nil)
	rec = me.do(http.MethodPost, "/api/v1/appointments/"+appt.ID+"/cancel", nil, map[string]string{"If-Match": `"1"`})
	me.expect(rec, http.StatusOK, nil)
	if rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("ETag after cancel = %q", rec.Header().Get("ETag"))
	}

	var mine []map[string]any
	me.expect(me.do(http.MethodGet, "/api/v1/appointments", nil, nil), http.StatusOK, &mine)
	if len(mine) != 1 {
		t.Fatalf("my appointments = %d", len(mine))
	}

	var ticket struct {
		Ticket string `json:"ticket"`
	}
	me.expect(me.do(http.MethodPost, "/api/v1/auth/ws-ticket", nil, nil), http.StatusOK, &ticket)
	if ticket.Ticket == "" || ticket.Ticket == session.Token {
		t.Fatal("expected a distinct WS ticket")
	}
}

func TestHTTPStaffRolesAndDeactivation(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	handler := newAPI(st, svc)
	day, _ := suiteToday()

	dispatcher := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/schedule?dealershipId="+seed.DealerNorth.String()+"&date="+day.String(), nil, nil), http.StatusOK, nil)
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/admin/technicians", nil, nil), http.StatusForbidden, nil)

	admin := login(t, handler, seed.DemoAdminEmail, seed.DemoAdminPassword)
	var dealer struct {
		Version int `json:"version"`
	}
	rec := admin.do(http.MethodPatch, "/api/v1/admin/dealerships/"+seed.DealerRiver.String(), map[string]any{"lunchStart": "12:00"}, nil)
	admin.expect(rec, http.StatusBadRequest, nil)
	rec = admin.do(http.MethodPatch, "/api/v1/admin/dealerships/"+seed.DealerRiver.String(), map[string]any{"openTime": "7:5"}, nil)
	admin.expect(rec, http.StatusBadRequest, nil)
	rec = admin.do(http.MethodPatch, "/api/v1/admin/dealerships/"+seed.DealerRiver.String(), map[string]any{"stagingCapacity": 4}, map[string]string{"If-Match": `"1"`})
	admin.expect(rec, http.StatusOK, &dealer)
	if dealer.Version != 2 || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("version=%d etag=%q", dealer.Version, rec.Header().Get("ETag"))
	}
	admin.expect(admin.do(http.MethodPatch, "/api/v1/admin/dealerships/"+seed.DealerRiver.String(), map[string]any{"stagingCapacity": 5}, map[string]string{"If-Match": `"1"`}), http.StatusPreconditionFailed, nil)

	// Deactivation takes effect on the next request, not when the token expires.
	if _, err := pool.Exec(ctx, `UPDATE users SET active = false WHERE id = $1`, seed.DemoDispatcherID); err != nil {
		t.Fatal(err)
	}
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusUnauthorized, nil)

	// …and the disabled dispatcher can no longer open the staff event stream with the old token.
	srv := httptest.NewServer(handler)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws/dispatch"
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer " + dispatcher.token}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled user WS: want 401, got %v (err %v)", resp, err)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer " + admin.token}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("active admin WS: %v", err)
	}
	_ = conn.Close()
}

// Logout revokes every session of the user (other tabs/devices too), including unused WS tickets.
func TestHTTPLogoutRevokesAllSessions(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	handler := newAPI(st, svc)
	laptop := login(t, handler, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	phone := login(t, handler, seed.DemoCustomerEmail, seed.DemoCustomerPassword)

	var ticket struct {
		Ticket string `json:"ticket"`
	}
	phone.expect(phone.do(http.MethodPost, "/api/v1/auth/ws-ticket", nil, nil), http.StatusOK, &ticket)

	laptop.expect(laptop.do(http.MethodPost, "/api/v1/auth/logout", nil, nil), http.StatusNoContent, nil)
	laptop.expect(laptop.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusUnauthorized, nil)
	phone.expect(phone.do(http.MethodGet, "/api/v1/vehicles", nil, nil), http.StatusUnauthorized, nil)

	srv := httptest.NewServer(handler)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws/dispatch?ticket=" + ticket.Ticket
	_, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticket minted before logout: want 401, got %v", resp)
	}

	again := login(t, handler, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	again.expect(again.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)

	// Open sockets on other instances are closed through the outbox → Kafka → hub path.
	claimed, err := st.ClaimOutbox(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range claimed {
		if row.EventType == domain.EventSessionRevoked && row.AggregateID == seed.DemoCustomerID {
			return
		}
	}
	t.Fatal("logout must emit SESSION_REVOKED for the user")
}
