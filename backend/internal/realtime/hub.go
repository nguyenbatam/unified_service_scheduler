package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

const (
	defaultWriteWait  = 10 * time.Second
	defaultPongWait   = 60 * time.Second
	defaultPingPeriod = (defaultPongWait * 9) / 10
	maxMessageSize    = 512
	sendBuffer        = 64
	validateTimeout   = 5 * time.Second
	// defaultMaxPerUser caps one user's sockets on one instance: a few tabs and devices fit, a
	// runaway client or a stolen token cannot exhaust the hub.
	defaultMaxPerUser = 10
	// fullRetryAfter is the Retry-After (seconds) of a handshake refused at the per-user cap.
	fullRetryAfter = "30"
	// revokeMemory is how long a revocation is remembered for handshakes still under way (see
	// Hub.revoked): far longer than a handshake takes, and longer than a sweep period, which
	// catches anything after that.
	revokeMemory = 2 * time.Minute
	// maxRevocations bounds that memory; past it the oldest entry is forgotten (its user is then
	// left to the sweep, as before revocations were remembered).
	maxRevocations = 10_000
)

// CloseSlowConsumer is the close code (with reason "slow consumer") of a socket evicted because its
// send buffer was full: the client should reconnect and resync. A private-use code (4000–4999), so
// clients and the stress listener can tell this designed eviction from every other close.
const CloseSlowConsumer = 4000

// welcomeMessage is the first frame of every socket.
var welcomeMessage = func() []byte {
	raw, err := json.Marshal(Message{Type: domain.EventConnected, Payload: map[string]string{"status": "ok"}})
	if err != nil {
		panic(err)
	}
	return raw
}()

type client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	session Session
	// accepted is when ServeWS took the handshake, after the session's user was read: a revoke
	// without a version (Event.TokenVersion 0) refuses sockets accepted before it.
	accepted time.Time
	// subscribed is the dealership filter; guarded by hub.mu.
	subscribed uuid.UUID
	// closeCode/closeText are set (under hub.mu) before send is closed; writePump reads them
	// after observing the closed channel, so the channel close orders the accesses.
	closeCode int
	closeText string
}

// Validator re-checks connected sessions in one batch (user still active, same role, customer,
// dealership scope, and token version) and returns, per session, whether it is still valid. An
// error keeps every socket open: infrastructure hiccups fail open and the next sweep retries.
type Validator func(ctx context.Context, sessions []Session) ([]bool, error)

// revocation is what a SESSION_REVOKED left behind for handshakes still under way: sockets of the
// user opened with a token version below floor, or (for a revoke without a version) accepted
// before allAt, are refused at register.
type revocation struct {
	floor int
	allAt time.Time
	at    time.Time // last revoke, for expiry
}

// Hub fans events out to local sockets. One mutex guards the client set and every close(send):
// Broadcast must send under it (a send racing removeLocked's close would panic), so it is held
// across the whole loop. The loop only does non-blocking channel sends, events arrive from a
// single consumer goroutine, and the expensive work (marshalling, network writes) happens
// elsewhere, so a read lock or a snapshot would buy little and complicate the close ordering.
type Hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
	// perUser indexes clients by user (the per-user cap, and closeUser touches only that user's
	// sockets); guarded by mu. A user with no socket has no entry.
	perUser map[uuid.UUID]map[*client]struct{}
	// revoked remembers recent revocations (at most maxRevocations, each for revokeMemory) so a
	// handshake that read the user just before a logout cannot register after the logout's
	// closeUser ran and stay open until the next sweep; guarded by mu.
	revoked   map[uuid.UUID]revocation
	lastPrune time.Time
	now       func() time.Time // tests
	closed    bool
	// writers counts running writePumps so Close can wait for their close frames. Add happens
	// under mu while !closed, so it never races Close's Wait.
	writers    sync.WaitGroup
	log        *slog.Logger
	upgrader   websocket.Upgrader
	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration
	validate   Validator
	maxPerUser int           // 0 = unlimited
	rejected   atomic.Uint64 // sockets refused by maxPerUser
	sweeps     atomic.Uint64 // finished session sweeps (Sweeps)
	// allowedOrigins, when non-empty, replaces the same-host Origin rule (lower-case scheme://host).
	allowedOrigins []string
}

func NewHub(log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{
		clients:    map[*client]struct{}{},
		perUser:    map[uuid.UUID]map[*client]struct{}{},
		revoked:    map[uuid.UUID]revocation{},
		now:        time.Now,
		maxPerUser: defaultMaxPerUser,
		log:        log,
		writeWait:  defaultWriteWait,
		pongWait:   defaultPongWait,
		pingPeriod: defaultPingPeriod,
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}
	return h
}

// SetKeepAlive overrides write/pong/ping intervals. Call before ServeWS (tests).
func (h *Hub) SetKeepAlive(writeWait, pongWait, pingPeriod time.Duration) {
	h.writeWait = writeWait
	h.pongWait = pongWait
	h.pingPeriod = pingPeriod
}

// SetMaxConnsPerUser caps how many sockets one user may hold on this hub (default 10; <= 0 means
// unlimited). A handshake over the cap is answered 429 with Retry-After and never upgraded, so a
// browser sees a failed connection (not an open one it would treat as success and retry at once);
// one that slips past that check in a race is closed right after the upgrade with 1013 (try again
// later). Call before serving.
func (h *Hub) SetMaxConnsPerUser(n int) {
	h.maxPerUser = max(n, 0)
}

// RejectedConnections counts sockets refused because their user was at the cap, before or after
// the upgrade (metrics).
func (h *Hub) RejectedConnections() uint64 {
	return h.rejected.Load()
}

// Sweeps is how many session sweeps have finished, whatever their outcome, so a test can wait
// for one to run after a change instead of sleeping.
func (h *Hub) Sweeps() uint64 {
	return h.sweeps.Load()
}

// SetValidator installs the periodic session re-check run by Run (call before serving). It
// catches account deactivation and role changes made outside the API; logout is pushed via
// KindSession events.
func (h *Hub) SetValidator(v Validator) {
	h.validate = v
}

// SetAllowedOrigins restricts browser pages that may open a socket to these origins
// (scheme://host[:port], compared case-insensitively). Empty keeps the same-host rule. Call
// before serving.
func (h *Hub) SetAllowedOrigins(origins []string) {
	h.allowedOrigins = make([]string, 0, len(origins))
	for _, o := range origins {
		h.allowedOrigins = append(h.allowedOrigins, strings.ToLower(strings.TrimSuffix(o, "/")))
	}
}

// checkOrigin allows non-browser clients (no Origin), then either an allowlisted origin or, with
// no allowlist, a page served from the same host:port the browser connected to.
func (h *Hub) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if len(h.allowedOrigins) > 0 {
		return slices.Contains(h.allowedOrigins, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return strings.EqualFold(u.Host, r.Host)
}

// ServeWS upgrades an already-authenticated request and streams events allowed for session.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, session Session) {
	accepted := h.now()
	// Refuse at the cap before upgrading. register re-checks under the same lock and stays the
	// authority: two handshakes of one user can both pass here.
	if h.full(session.UserID) {
		h.RefuseFull(w)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Error("websocket upgrade", "err", err)
		return
	}
	c := &client{
		hub:        h,
		conn:       conn,
		send:       make(chan []byte, sendBuffer),
		session:    session,
		accepted:   accepted,
		subscribed: session.DealershipID,
	}
	// Queue the welcome before the client is visible to Broadcast/closeUser/Close, which may
	// close send at any moment after register. The buffer is empty, so this never blocks.
	c.send <- welcomeMessage
	switch h.register(c) {
	case registered:
	case registerFull:
		h.rejected.Add(1)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many connections"),
			time.Now().Add(h.writeWait))
		_ = conn.Close()
		return
	case registerRevoked:
		// Signed out while the handshake was under way: same close as a socket closeUser hit.
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "signed out"),
			time.Now().Add(h.writeWait))
		_ = conn.Close()
		return
	default:
		_ = conn.Close()
		return
	}

	go func() {
		defer h.writers.Done()
		c.writePump()
	}()
	c.readPump()
}

// Broadcast delivers ev to every local client according to route(): full notice, anonymous
// availability ping, or nothing. Each variant is marshalled at most once.
func (h *Hub) Broadcast(ev Event) {
	if ev.Kind == KindSession {
		h.closeUser(ev.UserID, ev.TokenVersion, "signed out")
		return
	}
	var notice, avail []byte
	encode := func(d delivery) []byte {
		var err error
		switch d {
		case deliverNotice:
			if notice == nil {
				notice, err = json.Marshal(noticeMessage(ev))
			}
			if err == nil {
				return notice
			}
		case deliverAvailability:
			if avail == nil {
				avail, err = json.Marshal(availabilityMessage(ev))
			}
			if err == nil {
				return avail
			}
		}
		if err != nil {
			h.log.Error("websocket marshal", "err", err, "type", ev.Type)
		}
		return nil
	}

	var stale []*client
	h.mu.Lock()
	for c := range h.clients {
		raw := encode(route(ev, c.session, c.subscribed))
		if raw == nil {
			continue
		}
		select {
		case c.send <- raw:
		default:
			// Slow / stuck client: drop to protect the hub. The browser reconnects and resyncs.
			c.closeCode, c.closeText = CloseSlowConsumer, "slow consumer"
			h.removeLocked(c)
			stale = append(stale, c)
		}
	}
	h.mu.Unlock()

	// Close outside the hub lock so readPump's unregister cannot deadlock. The close frame goes out
	// as a control message (allowed concurrently with writePump's writes) without waiting for
	// writePump to drain the messages still queued, in a goroutine of its own: a stuck client's full socket must not stall the
	// fan-out for its write deadline.
	for _, c := range stale {
		go func(conn *websocket.Conn) {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(CloseSlowConsumer, "slow consumer"), time.Now().Add(h.writeWait))
			_ = conn.Close()
		}(c.conn)
	}
}

// Close refuses new clients and disconnects every client with a close frame (graceful shutdown),
// waiting until ctx ends for the write pumps to send those frames.
func (h *Hub) Close(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	for c := range h.clients {
		h.removeLocked(c)
	}
	h.mu.Unlock()

	done := make(chan struct{})
	go func() {
		h.writers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		h.log.Warn("websocket clients did not close in time")
	}
}

// Run re-validates every connected session about once per ping period (jittered, so replicas do
// not query in lockstep) with one batched Validator call, until ctx ends. It is a no-op without a
// validator.
func (h *Hub) Run(ctx context.Context) {
	if h.validate == nil {
		return
	}
	timer := time.NewTimer(jitter(h.pingPeriod))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.sweep(ctx)
			timer.Reset(jitter(h.pingPeriod))
		}
	}
}

// jitter spreads d by ±10%.
func jitter(d time.Duration) time.Duration {
	return d - d/10 + rand.N(d/5+1) //nolint:gosec // G404: scheduling jitter, not a secret
}

// sweep closes sockets whose session the validator rejects. The database call runs without the
// hub lock, so broadcasts and write pumps never wait on it.
func (h *Hub) sweep(ctx context.Context) {
	defer h.sweeps.Add(1)
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	sessions := make([]Session, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
		sessions = append(sessions, c.session)
	}
	h.mu.Unlock()
	if len(clients) == 0 {
		return
	}

	checkCtx, cancel := context.WithTimeout(ctx, validateTimeout)
	defer cancel()
	valid, err := h.validate(checkCtx, sessions)
	if err != nil || len(valid) != len(sessions) {
		if ctx.Err() == nil { // not shutting down
			h.log.Warn("websocket session sweep", "err", err, "sessions", len(sessions))
		}
		return
	}
	h.mu.Lock()
	for i, c := range clients {
		if _, open := h.clients[c]; !open || valid[i] {
			// Left since the snapshot: its writePump may already be reading closeCode.
			continue
		}
		c.closeCode, c.closeText = websocket.ClosePolicyViolation, "session revoked"
		h.removeLocked(c)
	}
	h.mu.Unlock()
}

// closeUser disconnects userID's local sockets opened with a token version older than
// tokenVersion (0 = every socket) with a policy-violation close frame. A socket opened after the
// revocation carries the new version, so a late or replayed revoke leaves it alone. The
// revocation is also remembered for revokeMemory, so a handshake that read the user before the
// logout committed but registers after this ran is refused too (register).
func (h *Hub) closeUser(userID uuid.UUID, tokenVersion int, reason string) {
	if userID == uuid.Nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rememberRevocationLocked(userID, tokenVersion)
	for c := range h.perUser[userID] { // removeLocked deletes from this set: allowed while ranging
		if revokes(tokenVersion, c.session.TokenVersion) {
			c.closeCode, c.closeText = websocket.ClosePolicyViolation, reason
			h.removeLocked(c)
		}
	}
}

// revokes reports whether a revoke carrying tokenVersion (0 = all) covers a session opened with
// sessionVersion.
func revokes(tokenVersion, sessionVersion int) bool {
	return tokenVersion == 0 || sessionVersion < tokenVersion
}

// rememberRevocationLocked records a revoke for register. Versions only ratchet up, so a late or
// replayed revoke never lowers the floor; expired entries are pruned at most once per
// revokeMemory, and a full map forgets its oldest entry.
func (h *Hub) rememberRevocationLocked(userID uuid.UUID, tokenVersion int) {
	now := h.now()
	if now.Sub(h.lastPrune) >= revokeMemory {
		h.lastPrune = now
		for id, r := range h.revoked {
			if now.Sub(r.at) >= revokeMemory {
				delete(h.revoked, id)
			}
		}
	}
	r, ok := h.revoked[userID]
	if !ok && len(h.revoked) >= maxRevocations {
		oldest, oldestAt := uuid.Nil, now
		for id, e := range h.revoked {
			if !e.at.After(oldestAt) {
				oldest, oldestAt = id, e.at
			}
		}
		delete(h.revoked, oldest)
	}
	if !ok || now.Sub(r.at) >= revokeMemory {
		r = revocation{}
	}
	r.at = now
	if tokenVersion == 0 {
		r.allAt = now
	} else {
		r.floor = max(r.floor, tokenVersion)
	}
	h.revoked[userID] = r
}

// revokedLocked reports whether a remembered revoke covers c: its token version is below a
// revoke's floor, or a revoke without a version came after c was accepted.
func (h *Hub) revokedLocked(c *client) bool {
	r, ok := h.revoked[c.session.UserID]
	if !ok || h.now().Sub(r.at) >= revokeMemory {
		return false
	}
	if r.floor > 0 && c.session.TokenVersion < r.floor {
		return true
	}
	return !r.allAt.IsZero() && c.accepted.Before(r.allAt)
}

func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// full reports whether userID already holds the per-user cap of sockets.
// Full reports whether userID already holds the per-user cap of sockets here, so a handshake can be
// refused before it spends anything (its single-use ticket); register stays the authority.
func (h *Hub) Full(userID uuid.UUID) bool { return h.full(userID) }

// RefuseFull answers a handshake over the per-user cap: HTTP 429 + Retry-After, no upgrade.
func (h *Hub) RefuseFull(w http.ResponseWriter) {
	h.rejected.Add(1)
	w.Header().Set("Retry-After", fullRetryAfter)
	http.Error(w, "too many connections", http.StatusTooManyRequests)
}

func (h *Hub) full(userID uuid.UUID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxPerUser > 0 && len(h.perUser[userID]) >= h.maxPerUser
}

type registerResult int

const (
	registered registerResult = iota
	registerClosed
	registerFull
	registerRevoked
)

func (h *Hub) register(c *client) registerResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return registerClosed
	}
	if h.revokedLocked(c) {
		return registerRevoked
	}
	userClients := h.perUser[c.session.UserID]
	if h.maxPerUser > 0 && len(userClients) >= h.maxPerUser {
		return registerFull
	}
	if userClients == nil {
		userClients = map[*client]struct{}{}
		h.perUser[c.session.UserID] = userClients
	}
	h.clients[c] = struct{}{}
	userClients[c] = struct{}{}
	h.writers.Add(1)
	return registered
}

func (h *Hub) unregister(c *client) {
	h.mu.Lock()
	h.removeLocked(c)
	h.mu.Unlock()
}

func (h *Hub) subscribe(c *client, dealershipID uuid.UUID) {
	h.mu.Lock()
	c.subscribed = dealershipID
	h.mu.Unlock()
}

func (h *Hub) removeLocked(c *client) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	if userClients := h.perUser[c.session.UserID]; userClients != nil {
		delete(userClients, c)
		if len(userClients) == 0 {
			delete(h.perUser, c.session.UserID)
		}
	}
	close(c.send)
}

// clientMessage is the only inbound message: change the dealership filter ("" = all).
type clientMessage struct {
	Type         string `json:"type"`
	DealershipID string `json:"dealershipId"`
}

func (c *client) readPump() {
	defer func() {
		c.hub.unregister(c)
		_ = c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(c.hub.pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(c.hub.pongWait))
	})
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var msg clientMessage
		if json.Unmarshal(raw, &msg) != nil || msg.Type != "subscribe" {
			continue
		}
		id := uuid.Nil
		if msg.DealershipID != "" {
			parsed, err := uuid.Parse(msg.DealershipID)
			if err != nil {
				continue
			}
			id = parsed
		}
		c.hub.subscribe(c, id)
	}
}

func (c *client) writePump() {
	ticker := time.NewTicker(c.hub.pingPeriod)
	var expired <-chan time.Time
	if !c.session.ExpiresAt.IsZero() {
		timer := time.NewTimer(time.Until(c.session.ExpiresAt))
		defer timer.Stop()
		expired = timer.C
	}
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
	}()
	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.hub.writeWait))
			if !ok {
				code, text := c.closeCode, c.closeText
				if code == 0 {
					code = websocket.CloseGoingAway
				}
				_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, text))
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.hub.writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-expired:
			_ = c.conn.SetWriteDeadline(time.Now().Add(c.hub.writeWait))
			_ = c.conn.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "session expired"))
			return
		}
	}
}
