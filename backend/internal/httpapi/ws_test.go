package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

func newWSTestServer(t *testing.T, authSvc *auth.Service, opts ...httpapi.Option) (*httptest.Server, string) {
	t.Helper()
	api := httpapi.New(nil, nil, authSvc, realtime.NewHub(nil), nil, "test", opts...)
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws/dispatch"
}

func dispatcher() domain.User {
	return domain.User{ID: uuid.New(), Email: "ws@test.local", Name: "WS", Role: domain.RoleDispatcher}
}

func TestWSRequiresAuth(t *testing.T) {
	_, base := newWSTestServer(t, auth.New("ws-test-secret"))
	_, resp, err := websocket.DefaultDialer.Dial(base, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected dial without credentials to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401", resp)
	}
}

func TestWSAcceptsBearerAndTicket(t *testing.T) {
	authSvc := auth.New("ws-test-secret")
	token, exp, err := authSvc.Issue(dispatcher())
	if err != nil {
		t.Fatal(err)
	}
	user := dispatcher()
	ticket, _, err := authSvc.IssueTicket(domain.Actor{UserID: user.ID, Role: user.Role}, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	_, base := newWSTestServer(t, authSvc)

	t.Run("bearer", func(t *testing.T) {
		conn, resp, err := websocket.DefaultDialer.Dial(base, http.Header{"Authorization": []string{"Bearer " + token}})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("bearer dial: %v", err)
		}
		defer conn.Close()
		assertConnected(t, conn)
	})

	t.Run("ticket", func(t *testing.T) {
		conn, resp, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket, nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("ticket dial: %v", err)
		}
		defer conn.Close()
		assertConnected(t, conn)
	})
}

// The long-lived access token must not work from a URL, and a ticket must not work as an API token.
func TestWSTokenAudiencesAreSeparate(t *testing.T) {
	authSvc := auth.New("ws-test-secret")
	token, exp, err := authSvc.Issue(dispatcher())
	if err != nil {
		t.Fatal(err)
	}
	srv, base := newWSTestServer(t, authSvc)

	for _, q := range []string{"?ticket=" + token, "?token=" + token} {
		_, resp, err := websocket.DefaultDialer.Dial(base+q, nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: want 401, got %v", q[:7], resp)
		}
	}

	user := dispatcher()
	ticket, _, err := authSvc.IssueTicket(domain.Actor{UserID: user.ID, Role: user.Role}, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+ticket)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ticket as API token: status %d, want 401", resp.StatusCode)
	}
}

func TestWSRejectsBadDealershipFilter(t *testing.T) {
	authSvc := auth.New("ws-test-secret")
	token, _, err := authSvc.Issue(dispatcher())
	if err != nil {
		t.Fatal(err)
	}
	_, base := newWSTestServer(t, authSvc)
	_, resp, err := websocket.DefaultDialer.Dial(base+"?dealershipId=nope", http.Header{"Authorization": []string{"Bearer " + token}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %v", resp)
	}
}

// memTickets is an in-memory cache.Tickets; err makes every Redeem fail (Redis down).
type memTickets struct {
	mu   sync.Mutex
	used map[string]time.Duration
	err  error
}

func (m *memTickets) Redeem(_ context.Context, id string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	if _, ok := m.used[id]; ok {
		return false, nil
	}
	m.used[id] = ttl
	return true, nil
}

func TestWSTicketIsSingleUse(t *testing.T) {
	authSvc := auth.New("ws-test-secret")
	user := dispatcher()
	_, exp, err := authSvc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	newTicket := func() string {
		ticket, _, err := authSvc.IssueTicket(domain.Actor{UserID: user.ID, Role: user.Role}, 0, exp)
		if err != nil {
			t.Fatal(err)
		}
		return ticket
	}

	t.Run("replay rejected", func(t *testing.T) {
		store := &memTickets{used: map[string]time.Duration{}}
		_, base := newWSTestServer(t, authSvc, httpapi.WithTicketRedeemer(store))
		ticket := newTicket()
		conn, resp, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket, nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("first use: %v", err)
		}
		defer conn.Close()
		assertConnected(t, conn)
		_, replay, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket, nil)
		if replay != nil {
			_ = replay.Body.Close()
		}
		if err == nil || replay == nil || replay.StatusCode != http.StatusUnauthorized {
			t.Fatalf("replay: want 401, got %v", replay)
		}
		for _, ttl := range store.used {
			if ttl <= 0 || ttl > 62*time.Second {
				t.Fatalf("marker ttl = %v, want the ticket's remaining lifetime", ttl)
			}
		}
	})

	// A handshake refused for another reason (here a bad filter; a 503 reading the user alike)
	// does not burn the ticket, so the client's retry with it still opens the socket.
	t.Run("refused handshake keeps the ticket", func(t *testing.T) {
		store := &memTickets{used: map[string]time.Duration{}}
		_, base := newWSTestServer(t, authSvc, httpapi.WithTicketRedeemer(store))
		ticket := newTicket()
		_, bad, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket+"&dealershipId=nope", nil)
		if bad != nil {
			_ = bad.Body.Close()
		}
		if err == nil || bad == nil || bad.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad filter: want 400, got %v", bad)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket, nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("retry with the same ticket: %v", err)
		}
		defer conn.Close()
		assertConnected(t, conn)
	})

	// Refused at the per-user socket cap (429): the ticket is not burned either.
	t.Run("socket cap keeps the ticket", func(t *testing.T) {
		store := &memTickets{used: map[string]time.Duration{}}
		hub := realtime.NewHub(nil)
		hub.SetMaxConnsPerUser(1)
		srv := httptest.NewServer(httpapi.New(nil, nil, authSvc, hub, nil, "test", httpapi.WithTicketRedeemer(store)).Handler())
		t.Cleanup(srv.Close)
		base := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws/dispatch"
		first, resp, err := websocket.DefaultDialer.Dial(base+"?ticket="+newTicket(), nil)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("first socket: %v", err)
		}
		defer first.Close()
		assertConnected(t, first)
		_, full, err := websocket.DefaultDialer.Dial(base+"?ticket="+newTicket(), nil)
		if full != nil {
			_ = full.Body.Close()
		}
		if err == nil || full == nil || full.StatusCode != http.StatusTooManyRequests || full.Header.Get("Retry-After") == "" {
			t.Fatalf("over the cap: want 429 + Retry-After, got %v", full)
		}
		store.mu.Lock()
		burned := len(store.used)
		store.mu.Unlock()
		if burned != 1 {
			t.Fatalf("%d tickets burned, want 1 (the refused handshake keeps its ticket)", burned)
		}
	})

	t.Run("redis down fails open", func(t *testing.T) {
		_, base := newWSTestServer(t, authSvc, httpapi.WithTicketRedeemer(&memTickets{err: errors.New("redis down")}))
		ticket := newTicket()
		for range 2 {
			conn, resp, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket, nil)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			assertConnected(t, conn)
			_ = conn.Close()
		}
	})
}

func assertConnected(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg realtime.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("welcome: %v", err)
	}
	if msg.Type != domain.EventConnected {
		t.Fatalf("welcome = %+v, want type connected", msg)
	}
}

// Customers open sockets only with tickets (minted by the throttled POST /auth/ws-ticket); the
// Bearer header path is for staff service clients.
func TestWSBearerIsStaffOnly(t *testing.T) {
	authSvc := auth.New("ws-test-secret")
	_, base := newWSTestServer(t, authSvc)
	customer := domain.User{ID: uuid.New(), Email: "c@test.local", Role: domain.RoleCustomer, CustomerID: uuid.New()}
	token, exp, err := authSvc.Issue(customer)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := websocket.DefaultDialer.Dial(base, http.Header{"Authorization": []string{"Bearer " + token}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("customer Bearer socket opened")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %v, want 403", resp)
	}
	ticket, _, err := authSvc.IssueTicket(domain.Actor{UserID: customer.ID, Role: customer.Role, CustomerID: customer.CustomerID}, 0, exp)
	if err != nil {
		t.Fatal(err)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(base+"?ticket="+ticket, nil)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("customer ticket dial: %v", err)
	}
	_ = conn.Close()
}
