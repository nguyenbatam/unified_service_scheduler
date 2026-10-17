package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

var (
	dealerA  = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	dealerB  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	ownerID  = uuid.MustParse("c1000000-0000-4000-8000-000000000001")
	otherID  = uuid.MustParse("c1000000-0000-4000-8000-000000000002")
	staffSes = realtime.Session{UserID: uuid.New(), Role: domain.RoleDispatcher}
)

// serve exposes the hub with a fixed session per ?as= value.
func serve(t *testing.T, hub *realtime.Hub, sessions map[string]realtime.Session) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.ServeWS(w, r, sessions[r.URL.Query().Get("as")])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wsClient reads in one goroutine: gorilla treats a read-deadline timeout as fatal for the conn,
// so "nothing arrived" must be observed on a channel, not with SetReadDeadline.
type wsClient struct {
	conn *websocket.Conn
	msgs chan wireMessage
	errs chan error
}

func dial(t *testing.T, srv *httptest.Server, as, origin string) *wsClient {
	t.Helper()
	c := connect(t, srv, as, origin)
	expectType(t, c, domain.EventConnected)
	return c
}

// connect opens a socket like dial without waiting for the welcome message (for sockets the hub
// may close before or right after it).
func connect(t *testing.T, srv *httptest.Server, as, origin string) *wsClient {
	t.Helper()
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?as=" + as
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	conn, resp, err := websocket.DefaultDialer.Dial(u, header)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	c := &wsClient{conn: conn, msgs: make(chan wireMessage, 64), errs: make(chan error, 1)}
	go func() {
		for {
			var msg wireMessage
			if err := conn.ReadJSON(&msg); err != nil {
				c.errs <- err
				return
			}
			c.msgs <- msg
		}
	}()
	t.Cleanup(func() { _ = conn.Close() })
	return c
}

type wireMessage struct {
	Type    domain.EventType `json:"type"`
	Payload json.RawMessage  `json:"payload"`
}

func read(c *wsClient, wait time.Duration) (wireMessage, bool) {
	select {
	case msg := <-c.msgs:
		return msg, true
	case <-c.errs:
		return wireMessage{}, false
	case <-time.After(wait):
		return wireMessage{}, false
	}
}

func expectType(t *testing.T, c *wsClient, want domain.EventType) wireMessage {
	t.Helper()
	msg, ok := read(c, 2*time.Second)
	if !ok {
		t.Fatalf("expected %s, got nothing", want)
	}
	if msg.Type != want {
		t.Fatalf("type = %v, want %v", msg.Type, want)
	}
	return msg
}

func expectSilence(t *testing.T, c *wsClient) {
	t.Helper()
	if msg, ok := read(c, 300*time.Millisecond); ok {
		t.Fatalf("expected no message, got %s %s", msg.Type, msg.Payload)
	}
}

func expectClose(t *testing.T, c *wsClient, code int) {
	t.Helper()
	for {
		select {
		case <-c.msgs:
		case err := <-c.errs:
			if !websocket.IsCloseError(err, code) {
				t.Fatalf("want close %d, got %v", code, err)
			}
			return
		case <-time.After(3 * time.Second):
			t.Fatalf("want close %d, got nothing", code)
		}
	}
}

func appointmentEvent(typ domain.EventType, dealer, customer uuid.UUID) realtime.Event {
	id := uuid.New()
	return realtime.Event{
		Type:         typ,
		Kind:         realtime.KindAppointment,
		DealershipID: dealer,
		CustomerID:   customer,
		Appointment: &realtime.AppointmentNotice{
			ID: id, DealershipID: dealer, Status: domain.StatusConfirmed, Version: 1,
			StartAt: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC), EndAt: time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC),
		},
		Payload: json.RawMessage(`{"customerName":"Amelia Clarke","vin":"WBA00000000000001"}`),
	}
}

func TestHubRoutesByAudience(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{
		"staff": staffSes,
		"owner": {UserID: uuid.New(), Role: domain.RoleCustomer, CustomerID: ownerID},
		"other": {UserID: uuid.New(), Role: domain.RoleCustomer, CustomerID: otherID},
	})
	staff := dial(t, srv, "staff", "")
	owner := dial(t, srv, "owner", "")
	other := dial(t, srv, "other", "")

	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, ownerID))

	for _, conn := range []*wsClient{staff, owner} {
		msg := expectType(t, conn, domain.EventAppointmentCreated)
		if strings.Contains(string(msg.Payload), "Amelia") || strings.Contains(string(msg.Payload), "WBA") {
			t.Fatalf("notice leaked PII: %s", msg.Payload)
		}
		var notice realtime.AppointmentNotice
		if err := json.Unmarshal(msg.Payload, &notice); err != nil || notice.ID == uuid.Nil || notice.Version != 1 {
			t.Fatalf("bad notice %s: %v", msg.Payload, err)
		}
	}
	anon := expectType(t, other, domain.EventAvailabilityChanged)
	if strings.Contains(string(anon.Payload), `"id"`) || strings.Contains(string(anon.Payload), "Amelia") {
		t.Fatalf("availability ping must not identify the booking: %s", anon.Payload)
	}

	// Check-in does not change capacity: other customers hear nothing.
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCheckedIn, dealerA, ownerID))
	expectType(t, staff, domain.EventAppointmentCheckedIn)
	expectType(t, owner, domain.EventAppointmentCheckedIn)
	expectSilence(t, other)

	// Config changes reach everyone.
	hub.Broadcast(realtime.Event{Type: domain.EventConfigUpdated, Kind: realtime.KindConfig,
		Config: &realtime.ConfigNotice{Entity: domain.AggregateDealership, ID: dealerA, Version: 2}})
	for _, conn := range []*wsClient{staff, owner, other} {
		expectType(t, conn, domain.EventConfigUpdated)
	}
}

func TestHubDealershipSubscription(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{
		"a": {UserID: uuid.New(), Role: domain.RoleDispatcher, DealershipID: dealerA},
	})
	conn := dial(t, srv, "a", "")

	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerB, ownerID))
	expectSilence(t, conn)
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, ownerID))
	expectType(t, conn, domain.EventAppointmentCreated)

	if err := conn.conn.WriteJSON(map[string]string{"type": "subscribe", "dealershipId": dealerB.String()}); err != nil {
		t.Fatal(err)
	}
	// Subscription is applied asynchronously by the read pump; retry until it lands.
	deadline := time.Now().Add(2 * time.Second)
	for {
		hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerB, ownerID))
		if msg, ok := read(conn, 200*time.Millisecond); ok && msg.Type == domain.EventAppointmentCreated {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscription change not applied")
		}
	}
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, ownerID))
	expectSilence(t, conn)
}

// The expiry may fire before or after the welcome reaches the client (a slow handshake under -race
// can outlast a short session), so the test waits for the close only, whatever came before it.
func TestHubClosesExpiredSession(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{
		"short":   {UserID: uuid.New(), Role: domain.RoleDispatcher, ExpiresAt: time.Now().Add(300 * time.Millisecond)},
		"expired": {UserID: uuid.New(), Role: domain.RoleDispatcher, ExpiresAt: time.Now().Add(-time.Second)},
	})
	expectClose(t, connect(t, srv, "short", ""), websocket.ClosePolicyViolation)
	expectClose(t, connect(t, srv, "expired", ""), websocket.ClosePolicyViolation)
}

func TestHubCloseDisconnectsClients(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})
	conn := dial(t, srv, "staff", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hub.Close(ctx) // returns once the close frame is written
	expectClose(t, conn, websocket.CloseGoingAway)
	if hub.ClientCount() != 0 {
		t.Fatalf("clients = %d after Close", hub.ClientCount())
	}
}

func TestHubConcurrentBroadcast(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})

	const n = 8
	clients := make([]*wsClient, 0, n)
	for range n {
		clients = append(clients, dial(t, srv, "staff", ""))
	}
	// Stay under sendBuffer per client so this exercises write races, not backpressure drops.
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, ownerID)) })
	}
	wg.Wait()
	for _, c := range clients {
		for range 10 {
			expectType(t, c, domain.EventAppointmentCreated)
		}
	}
	if hub.ClientCount() != n {
		t.Fatalf("clients = %d, want %d after concurrent broadcast", hub.ClientCount(), n)
	}
}

func TestHubRejectsCrossOrigin(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?as=staff"
	dialer := websocket.Dialer{HandshakeTimeout: 2 * time.Second}
	_, resp, err := dialer.Dial(u, http.Header{"Origin": []string{"http://evil.example"}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected cross-origin dial to fail")
	}
	if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestHubAllowsSameOrigin(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})
	dial(t, srv, "staff", srv.URL)
}

func TestHubPingKeepsAlive(t *testing.T) {
	hub := realtime.NewHub(nil)
	hub.SetKeepAlive(500*time.Millisecond, 400*time.Millisecond, 100*time.Millisecond)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})
	conn := dial(t, srv, "staff", "")

	// gorilla auto-replies to server pings while the read loop runs.
	select {
	case err := <-conn.errs:
		t.Fatalf("connection died during keepalive window: %v", err)
	case <-time.After(1200 * time.Millisecond):
	}
	if hub.ClientCount() != 1 {
		t.Fatalf("clients = %d after keepalive window", hub.ClientCount())
	}
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCancelled, dealerA, ownerID))
	expectType(t, conn, domain.EventAppointmentCancelled)
}

func TestHubSessionRevokedClosesOnlyThatUser(t *testing.T) {
	hub := realtime.NewHub(nil)
	victim := realtime.Session{UserID: uuid.New(), Role: domain.RoleCustomer, CustomerID: ownerID}
	srv := serve(t, hub, map[string]realtime.Session{"victim": victim, "staff": staffSes})
	a := dial(t, srv, "victim", "")
	b := dial(t, srv, "victim", "") // second tab of the same user
	other := dial(t, srv, "staff", "")

	hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: victim.UserID})
	expectClose(t, a, websocket.ClosePolicyViolation)
	expectClose(t, b, websocket.ClosePolicyViolation)
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, otherID))
	expectType(t, other, domain.EventAppointmentCreated)
}

func TestHubValidatorSweepClosesRevokedSessions(t *testing.T) {
	hub := realtime.NewHub(nil)
	hub.SetKeepAlive(500*time.Millisecond, 2*time.Second, 100*time.Millisecond)
	victim := realtime.Session{UserID: uuid.New(), Role: domain.RoleDispatcher}
	var revoked atomic.Bool
	var calls, maxBatch atomic.Int64
	hub.SetValidator(func(_ context.Context, sessions []realtime.Session) ([]bool, error) {
		calls.Add(1)
		if n := int64(len(sessions)); n > maxBatch.Load() {
			maxBatch.Store(n)
		}
		if !revoked.Load() {
			return nil, errors.New("db hiccup") // infrastructure errors must not drop sockets
		}
		valid := make([]bool, len(sessions))
		for i, s := range sessions {
			valid[i] = s.UserID != victim.UserID
		}
		return valid, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go hub.Run(ctx)

	srv := serve(t, hub, map[string]realtime.Session{"victim": victim, "staff": staffSes})
	a := dial(t, srv, "victim", "")
	b := dial(t, srv, "victim", "")
	other := dial(t, srv, "staff", "")
	expectSilence(t, a) // several sweeps with a failing-but-not-invalid validator
	if hub.ClientCount() != 3 {
		t.Fatal("socket dropped on a transient validator error")
	}
	if calls.Load() == 0 || maxBatch.Load() != 3 {
		t.Fatalf("validator calls = %d, max batch = %d; want batched sweeps of 3", calls.Load(), maxBatch.Load())
	}
	revoked.Store(true)
	expectClose(t, a, websocket.ClosePolicyViolation)
	expectClose(t, b, websocket.ClosePolicyViolation)
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, otherID))
	expectType(t, other, domain.EventAppointmentCreated)
}

// Closing the hub (or a user) while sockets are still connecting must never send on a closed
// channel (run with -race).
func TestHubCloseWhileConnecting(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?as=staff"
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				_ = conn.Close()
			}
		})
		wg.Go(func() {
			hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: staffSes.UserID})
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hub.Close(ctx)
	wg.Wait()
	if hub.ClientCount() != 0 {
		t.Fatalf("clients = %d after Close", hub.ClientCount())
	}
}

func TestHubAllowedOrigins(t *testing.T) {
	hub := realtime.NewHub(nil)
	hub.SetAllowedOrigins([]string{"https://Scheduler.Example.com"})
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})
	dial(t, srv, "staff", "https://scheduler.example.com")

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?as=staff"
	// With an allowlist, even the page's own host is refused unless listed.
	for _, origin := range []string{srv.URL, "http://scheduler.example.com"} {
		_, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Origin": []string{origin}})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %s: want 403, got %v", origin, resp)
		}
	}
}

// A client that disconnects while the session sweep's query is in flight is left alone when the
// sweep rejects it: its writePump may already be reading the close code (caught by -race).
func TestSweepSkipsClientThatLeftDuringValidation(t *testing.T) {
	hub := realtime.NewHub(nil)
	hub.SetKeepAlive(time.Second, 2*time.Second, 20*time.Millisecond) // sweep about every 20ms
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	hub.SetValidator(func(_ context.Context, s []realtime.Session) ([]bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return make([]bool, len(s)), nil // reject everyone
	})
	srv := serve(t, hub, map[string]realtime.Session{
		"a": {UserID: uuid.New(), Role: domain.RoleDispatcher},
		"b": {UserID: uuid.New(), Role: domain.RoleDispatcher},
	})
	c := dial(t, srv, "a", "")
	go hub.Run(t.Context())

	<-entered // the sweep has snapshotted the client and is waiting on the validator
	_ = c.conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("client was not unregistered after closing")
		}
		time.Sleep(time.Millisecond)
	}
	close(release) // the sweep now rejects a client that already left
	// Sweeps run one after another, so a later sweep closing a fresh socket proves the first one
	// has finished (without touching the client that left, or -race would have flagged it).
	expectClose(t, dial(t, srv, "b", ""), websocket.ClosePolicyViolation)
	if n := hub.RejectedConnections(); n != 0 {
		t.Fatalf("rejected connections = %d: revoked sessions are not cap rejections", n)
	}
	deadline = time.Now().Add(2 * time.Second)
	for hub.ClientCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("revoked client still registered")
		}
		time.Sleep(time.Millisecond)
	}
}

// One user can hold at most the cap on one hub; the extra handshake is refused with HTTP 429 and
// Retry-After before any upgrade (a browser must not see it open), and a closed socket frees its
// slot. Other users are unaffected.
func TestHubCapsSocketsPerUser(t *testing.T) {
	hub := realtime.NewHub(nil)
	hub.SetMaxConnsPerUser(2)
	user := realtime.Session{UserID: uuid.New(), Role: domain.RoleCustomer, CustomerID: ownerID}
	srv := serve(t, hub, map[string]realtime.Session{"user": user, "staff": staffSes})
	a := dial(t, srv, "user", "")
	dial(t, srv, "user", "")

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?as=user"
	conn, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, websocket.ErrBadHandshake) || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third socket: want HTTP 429 handshake refusal, got err %v, resp %v", err, resp)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("429 must carry Retry-After")
	}
	if hub.RejectedConnections() != 1 || hub.ClientCount() != 2 {
		t.Fatalf("rejected = %d, clients = %d", hub.RejectedConnections(), hub.ClientCount())
	}
	dial(t, srv, "staff", "") // another user still gets in

	_ = a.conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for hub.ClientCount() != 2 {
		if time.Now().After(deadline) {
			t.Fatal("closed socket was not unregistered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	dial(t, srv, "user", "") // the freed slot is usable again
}

// A revoke carrying the new token version closes only the sockets opened before it: a socket
// opened after the logout (new token) stays, so a late or replayed revoke cannot sign the user
// out again.
func TestHubSessionRevokedSparesNewerSessions(t *testing.T) {
	hub := realtime.NewHub(nil)
	user := uuid.New()
	srv := serve(t, hub, map[string]realtime.Session{
		"old":   {UserID: user, Role: domain.RoleDispatcher, TokenVersion: 3},
		"new":   {UserID: user, Role: domain.RoleDispatcher, TokenVersion: 4},
		"staff": staffSes,
	})
	old := dial(t, srv, "old", "")
	fresh := dial(t, srv, "new", "")
	other := dial(t, srv, "staff", "")

	hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: user, TokenVersion: 4})
	expectClose(t, old, websocket.ClosePolicyViolation)

	// Replay of an older revoke (version 3: every socket then open had version 2 or less).
	hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: user, TokenVersion: 3})
	hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: user, TokenVersion: 4})
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, otherID))
	expectType(t, fresh, domain.EventAppointmentCreated)
	expectType(t, other, domain.EventAppointmentCreated)
	if hub.ClientCount() != 2 {
		t.Fatalf("clients = %d, want the new session and the other user", hub.ClientCount())
	}

	// No version (older producers): every socket of the user goes, as before.
	hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: user})
	expectClose(t, fresh, websocket.ClosePolicyViolation)
}

// A handshake whose user was read before a logout but that reaches the hub after the logout's
// SESSION_REVOKED gets the same close as a socket the revoke hit, right after the upgrade.
func TestHubRefusesSessionRevokedDuringHandshake(t *testing.T) {
	hub := realtime.NewHub(nil)
	user := uuid.New()
	srv := serve(t, hub, map[string]realtime.Session{
		"stale": {UserID: user, Role: domain.RoleDispatcher, TokenVersion: 3},
		"new":   {UserID: user, Role: domain.RoleDispatcher, TokenVersion: 4},
	})
	hub.Broadcast(realtime.Event{Type: domain.EventSessionRevoked, Kind: realtime.KindSession, UserID: user, TokenVersion: 4})
	expectClose(t, connect(t, srv, "stale", ""), websocket.ClosePolicyViolation)
	fresh := dial(t, srv, "new", "")
	hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, otherID))
	expectType(t, fresh, domain.EventAppointmentCreated)
	if hub.ClientCount() != 1 {
		t.Fatalf("clients = %d, want the new session only", hub.ClientCount())
	}
}

// A client that stops reading is evicted once its send buffer is full, with the slow-consumer
// close code (4000, "slow consumer"), so it can be told apart from every other close.
func TestHubEvictsSlowConsumerWithItsCloseCode(t *testing.T) {
	hub := realtime.NewHub(nil)
	srv := serve(t, hub, map[string]realtime.Session{"staff": staffSes})
	slow := dial(t, srv, "staff", "") // its reader parks once its 64-message channel is full
	deadline := time.Now().Add(10 * time.Second)
	for hub.ClientCount() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the stuck client was never evicted")
		}
		for range 200 {
			hub.Broadcast(appointmentEvent(domain.EventAppointmentCreated, dealerA, ownerID))
		}
	}
	for {
		select {
		case <-slow.msgs:
		case err := <-slow.errs:
			var ce *websocket.CloseError
			if !errors.As(err, &ce) || ce.Code != realtime.CloseSlowConsumer || ce.Text != "slow consumer" {
				t.Fatalf("want close %d \"slow consumer\", got %v", realtime.CloseSlowConsumer, err)
			}
			return
		case <-time.After(15 * time.Second):
			t.Fatal("no close frame after the eviction")
		}
	}
}
