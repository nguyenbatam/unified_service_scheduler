package integration_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func TestFindUsersByIDs(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	if _, err := pool.Exec(ctx, `UPDATE users SET active = false WHERE id = $1`, seed.DemoCustomerID); err != nil {
		t.Fatal(err)
	}
	users, err := st.FindUsersByIDs(ctx, []uuid.UUID{seed.DemoAdminID, seed.DemoDispatcherID, seed.DemoCustomerID, uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users, want admin + dispatcher only (inactive and unknown ids absent)", len(users))
	}
	if d := users[seed.DemoDispatcherID]; d.Email != seed.DemoDispatcherEmail || d.DealershipID != seed.DealerNorth {
		t.Fatalf("dispatcher = %+v", d)
	}
	if empty, err := st.FindUsersByIDs(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("no ids: %v, %v", empty, err)
	}
}

// The hub's periodic sweep validates every socket in one query and closes exactly the sessions
// whose user changed: dealership scope, token version (logout elsewhere), or deactivation.
func TestWSSessionSweepClosesChangedUsers(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	hub := realtime.NewHub(nil)
	hub.SetKeepAlive(time.Second, 5*time.Second, 150*time.Millisecond)
	api := newAPIWithHub(st, svc, hub)
	authSvc := api.auth
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go hub.Run(runCtx)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws/dispatch"
	open := func(id uuid.UUID) <-chan error {
		t.Helper()
		user, err := st.FindUserByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		// A single-use ticket, as browsers open sockets (the Bearer header is staff-only).
		ticket, _, err := authSvc.IssueTicket(domain.Actor{UserID: user.ID, Role: user.Role, CustomerID: user.CustomerID},
			user.TokenVersion, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(url+"?ticket="+ticket, nil)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if err != nil {
			t.Fatalf("dial %s: %v", user.Email, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		closed := make(chan error, 1)
		go func() {
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					closed <- err
					return
				}
			}
		}()
		return closed
	}
	expectRevoked := func(name string, closed <-chan error) {
		t.Helper()
		select {
		case err := <-closed:
			if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
				t.Fatalf("%s: want close 1008, got %v", name, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s: socket not closed by the sweep", name)
		}
	}
	// expectOpen waits for two sweeps to finish (the second one started after the caller's last
	// change committed), then requires the socket still open and wantClients registered: a sweep
	// unregisters a socket it revokes before returning.
	expectOpen := func(name string, closed <-chan error, wantClients int) {
		t.Helper()
		target := hub.Sweeps() + 2
		deadline := time.Now().Add(5 * time.Second)
		for hub.Sweeps() < target {
			if time.Now().After(deadline) {
				t.Fatalf("%s: the hub ran no sweep within 5s", name)
			}
			time.Sleep(5 * time.Millisecond) // polls the sweep counter; no fixed wait
		}
		select {
		case err := <-closed:
			t.Fatalf("%s: closed unexpectedly: %v", name, err)
		default:
		}
		if n := hub.ClientCount(); n != wantClients {
			t.Fatalf("%s: %d clients after two sweeps, want %d", name, n, wantClients)
		}
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	admin, dispatcher, customer := open(seed.DemoAdminID), open(seed.DemoDispatcherID), open(seed.DemoCustomerID)
	expectOpen("unchanged users", admin, 3)

	exec(`UPDATE users SET dealership_id = $2 WHERE id = $1`, seed.DemoDispatcherID, seed.DealerRiver)
	expectRevoked("dispatcher moved dealership", dispatcher)
	expectOpen("customer", customer, 2)

	exec(`UPDATE users SET token_version = token_version + 1 WHERE id = $1`, seed.DemoCustomerID)
	expectRevoked("customer signed out elsewhere", customer)
	expectOpen("admin", admin, 1)

	exec(`UPDATE users SET active = false WHERE id = $1`, seed.DemoAdminID)
	expectRevoked("admin deactivated", admin)
	if n := hub.ClientCount(); n != 0 {
		t.Fatalf("clients = %d, want 0", n)
	}
}
