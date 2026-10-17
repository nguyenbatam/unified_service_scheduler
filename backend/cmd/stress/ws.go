package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// hubMaxSocketsPerUser is the hub's per-user socket cap on one instance (realtime
// defaultMaxPerUser): every stress listener uses the admin token, so -ws-clients cannot exceed it,
// and sockets the admin already holds elsewhere (an open dashboard) count against it too.
const hubMaxSocketsPerUser = 10

// hubCloseSlowConsumer is the close code the hub evicts a socket with when its send buffer is full
// (realtime.CloseSlowConsumer, reason "slow consumer"): an eviction by design, not a failure.
const hubCloseSlowConsumer = 4000

// errWSFull is a handshake refused with 429 at the hub's per-user socket cap.
var errWSFull = errors.New("ws handshake refused 429 (hub per-user socket cap)")

// wsListener is one socket (re-dialled on error) and every APPOINTMENT_CREATED id it delivered.
type wsListener struct {
	name     string       // "A#0", "B#1", …
	seen     sync.Map     // APPOINTMENT_CREATED ids
	events   sync.Map     // every APPOINTMENT_* notice as "id|version"
	drops    atomic.Int64 // sessions lost after the connected frame (events in the gap are missed)
	evicted  atomic.Int64 // of the sessions lost, those the hub closed as a slow consumer (by design)
	rejected atomic.Int64 // handshakes refused 429 at the per-user cap
	errs     *atomic.Int64

	mu          sync.Mutex
	arrivals    []wsArrival // every APPOINTMENT_* notice in the order this listener received it
	dropReasons []string    // why the first few non-eviction sessions ended
}

// lost is every session the listener lost after the connected frame: drops and evictions.
func (l *wsListener) lost() int64 { return l.drops.Load() + l.evicted.Load() }

// sessionEnded classifies why a session ended after its connected frame: a slow-consumer close is
// an eviction, anything else a drop (its reason kept for the report).
func (l *wsListener) sessionEnded(err error) {
	if ce, ok := errors.AsType[*websocket.CloseError](err); ok && ce.Code == hubCloseSlowConsumer {
		l.evicted.Add(1)
		return
	}
	l.drops.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.dropReasons) < 3 {
		reason := "closed"
		if err != nil {
			reason = err.Error()
		}
		l.dropReasons = append(l.dropReasons, reason)
	}
}

func (l *wsListener) reasons() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.dropReasons)
}

// wsArrival is one appointment notice as a listener received it.
type wsArrival struct {
	id      uuid.UUID
	version int
	typ     string
}

func (l *wsListener) arrived(a wsArrival) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.arrivals = append(l.arrivals, a)
}

// sequence returns a copy of the listener's arrivals.
func (l *wsListener) sequence() []wsArrival {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.arrivals)
}

// orderReport is judgeOrder's verdict over one listener's arrivals.
type orderReport struct {
	events, duplicates, outOfOrder int
	changed                        int // appointments seen at two or more versions: what order could be judged on
	samples                        []string
}

// judgeOrder checks one listener's arrival sequence per appointment: versions must never go back
// (one appointment's events are published in commit order to one Kafka partition and fanned out in
// order, so a lower version after a higher one is a reordering bug). The same notice (id, version,
// type) arriving twice is a duplicate: at-least-once delivery allows it (clients drop stale and
// repeated versions), so it is counted, not failed. Two different notices may share a version
// (none of the run's operations does that, but it is not a reordering).
func judgeOrder(seq []wsArrival) orderReport {
	rep := orderReport{events: len(seq)}
	latest := map[uuid.UUID]int{}
	seen := map[wsArrival]bool{}
	versions := map[uuid.UUID]map[int]bool{}
	for _, a := range seq {
		if versions[a.id] == nil {
			versions[a.id] = map[int]bool{}
		}
		versions[a.id][a.version] = true
		if seen[a] {
			rep.duplicates++
			continue
		}
		seen[a] = true
		if v, ok := latest[a.id]; ok && a.version < v {
			rep.outOfOrder++
			if len(rep.samples) < 3 {
				rep.samples = append(rep.samples, fmt.Sprintf("%s v%d %s after v%d", a.id, a.version, a.typ, v))
			}
			continue
		}
		latest[a.id] = max(latest[a.id], a.version)
	}
	for _, vs := range versions {
		if len(vs) >= 2 {
			rep.changed++
		}
	}
	return rep
}

// orderCheck judges every listener's arrival order: total events, duplicates, reorderings (with
// samples), and the largest number of changed appointments any one listener saw.
func (w *wsWatch) orderCheck() (events, duplicates, outOfOrder, changed int, samples []string) {
	for _, l := range append(slices.Clone(w.listenersA), w.listenersB...) {
		rep := judgeOrder(l.sequence())
		events += rep.events
		duplicates += rep.duplicates
		changed = max(changed, rep.changed)
		outOfOrder += rep.outOfOrder
		for _, s := range rep.samples {
			if len(samples) < 5 {
				samples = append(samples, l.name+": "+s)
			}
		}
	}
	return events, duplicates, outOfOrder, changed, samples
}

// wsWatch keeps WS listeners on both backends and records, per listener, every APPOINTMENT_CREATED
// id it was delivered: each listener must get every event, not just some listener per backend.
type wsWatch struct {
	ctx        context.Context
	cancel     context.CancelFunc
	auth       *authSession // every listener dials with this session's current access token
	listenersA []*wsListener
	listenersB []*wsListener
	errA, errB atomic.Int64
	wg         sync.WaitGroup
	// chaos (-chaos-restart): a backend is killed on purpose, so a listener may lose its session
	// (excused from the events sent while it was away), and a backend whose every listener dropped
	// was the one restarted: the events published while its hub was down never reach its sockets,
	// so its all-listeners union is not required. Without chaos, any lost session is a failure.
	chaos bool
}

func newWSWatch(parent context.Context, auth *authSession) *wsWatch {
	ctx, cancel := context.WithCancel(parent)
	return &wsWatch{ctx: ctx, cancel: cancel, auth: auth}
}

// start opens perBackend listeners on each backend and returns once every one of them has
// received the hub's CONNECTED frame (the socket is registered for fan-out), or fails after
// timeout instead of guessing with a sleep.
func (w *wsWatch) start(backendA, backendB string, perBackend int, timeout time.Duration) error {
	ready := make(chan struct{}, 2*perBackend)
	refused := make(chan error, 2*perBackend)
	for i := range perBackend {
		a := &wsListener{name: fmt.Sprintf("A#%d", i), errs: &w.errA}
		b := &wsListener{name: fmt.Sprintf("B#%d", i), errs: &w.errB}
		w.listenersA = append(w.listenersA, a)
		w.listenersB = append(w.listenersB, b)
		w.wg.Go(func() { listenWS(w.ctx, backendA, w.auth, a, ready, refused) })
		w.wg.Go(func() { listenWS(w.ctx, backendB, w.auth, b, ready, refused) })
	}
	deadline := time.After(timeout)
	for i := range 2 * perBackend {
		select {
		case <-ready:
		case err := <-refused:
			return fmt.Errorf("ws: %w after %d of %d listeners connected: the admin user already holds sockets there "+
				"(max %d per user per backend: close other dashboards or lower -ws-clients)", err, i, 2*perBackend, hubMaxSocketsPerUser)
		case <-deadline:
			return fmt.Errorf("ws: only %d of %d listeners got the connected frame within %s (dial errors A=%d B=%d)",
				i, 2*perBackend, timeout, w.errA.Load(), w.errB.Load())
		}
	}
	return nil
}

func (w *wsWatch) stop() {
	w.cancel()
	w.wg.Wait()
}

// waitCovered waits until every listener that must be complete delivered every id, and returns
// what is still missing (empty when covered).
func (w *wsWatch) waitCovered(want map[uuid.UUID]bool, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		gaps := append(coverageGaps("A", w.listenersA, want, w.chaos), coverageGaps("B", w.listenersB, want, w.chaos)...)
		if len(gaps) == 0 || time.Now().After(deadline) {
			return gaps
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// coverageGaps checks one backend's listeners. A listener whose session never dropped must have
// every id. Without chaos a dropped session is itself a gap (nothing in the run should close a
// socket). Under chaos one that reconnected may legitimately have missed the events sent while it
// was away, so it is excused, but then the backend's listeners together must still cover every id
// (the hub got each event even if that socket did not) — unless every listener of the backend
// dropped (the backend itself was restarted).
func coverageGaps(backend string, listeners []*wsListener, want map[uuid.UUID]bool, chaos bool) []string {
	var gaps []string
	union := map[uuid.UUID]bool{}
	for _, l := range listeners {
		miss := missing(want, &l.seen)
		for id := range want {
			if _, ok := l.seen.Load(id); ok {
				union[id] = true
			}
		}
		if gap := dropGap(l, chaos); gap != "" {
			gaps = append(gaps, gap)
		}
		if l.lost() == 0 && len(miss) > 0 {
			gaps = append(gaps, fmt.Sprintf("listener %s missed %d of %d %v", l.name, len(miss), len(want), firstN(miss, 5)))
		}
	}
	if len(listeners) > 0 && (!chaos || !allDropped(listeners)) {
		var miss []string
		for id := range want {
			if !union[id] {
				miss = append(miss, id.String())
			}
		}
		sort.Strings(miss)
		if len(miss) > 0 {
			gaps = append(gaps, fmt.Sprintf("backend %s (all listeners) missed %d of %d %v", backend, len(miss), len(want), firstN(miss, 5)))
		}
	}
	return gaps
}

// summary is per-listener coverage for the report, e.g. "A#0=40/40 A#1=40/40(drops=1) …".
func (w *wsWatch) summary(want map[uuid.UUID]bool) string {
	var parts []string
	for _, l := range append(append([]*wsListener(nil), w.listenersA...), w.listenersB...) {
		got := 0
		for id := range want {
			if _, ok := l.seen.Load(id); ok {
				got++
			}
		}
		part := fmt.Sprintf("%s=%d/%d", l.name, got, len(want))
		if d := l.drops.Load(); d > 0 {
			part += fmt.Sprintf("(drops=%d)", d)
		}
		if e := l.evicted.Load(); e > 0 {
			part += fmt.Sprintf("(evicted=%d)", e)
		}
		if r := l.rejected.Load(); r > 0 {
			part += fmt.Sprintf("(refused_429=%d)", r)
		}
		parts = append(parts, part)
	}
	return "created_seen " + strings.Join(parts, " ")
}

// dropGap is the gap a lost session makes outside chaos ("" under chaos or without a drop). A
// slow-consumer eviction is the hub's design (the client reconnects and resyncs), not a drop: it
// only excuses the listener's own misses, and is reported apart (INFO).
func dropGap(l *wsListener, chaos bool) string {
	if d := l.drops.Load(); d > 0 && !chaos {
		return fmt.Sprintf("listener %s lost its session %d time(s) with no chaos in the run (a dropped socket misses events) %v", l.name, d, l.reasons())
	}
	return ""
}

// evictions counts the slow-consumer evictions of every listener.
func (w *wsWatch) evictions() int64 {
	var n int64
	for _, l := range append(slices.Clone(w.listenersA), w.listenersB...) {
		n += l.evicted.Load()
	}
	return n
}

// allDropped reports whether every listener lost its session at least once.
func allDropped(listeners []*wsListener) bool {
	for _, l := range listeners {
		if l.lost() == 0 {
			return false
		}
	}
	return true
}

func firstN(s []string, n int) []string {
	return s[:min(len(s), n)]
}

func missing(want map[uuid.UUID]bool, seen *sync.Map) []string {
	var out []string
	for id := range want {
		if _, ok := seen.Load(id); !ok {
			out = append(out, id.String())
		}
	}
	sort.Strings(out)
	return out
}

// listenWS keeps one socket open (reconnecting on error) until ctx ends. ready gets one value the
// first time the server's CONNECTED frame arrives; a session lost after that counts as a drop.
// A handshake refused 429 (the hub's per-user cap) before that is reported on refused and ends the
// listener (start fails at once instead of timing out); after it, the redial waits Retry-After.
// Every dial carries the session's current access token (Bearer: staff only), so a reconnect after
// the access TTL is not refused; a handshake refused 401 renews the token before the redial. The
// socket itself lives until the session's absolute end (sexp), not the access token's.
func listenWS(ctx context.Context, backendHTTP string, sess *authSession, l *wsListener, ready chan<- struct{}, refused chan<- error) {
	u, err := url.Parse(backendHTTP)
	if err != nil {
		l.errs.Add(1)
		return
	}
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := scheme + "://" + u.Host + "/api/v1/ws/dispatch"
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	signalled := false
	for ctx.Err() == nil {
		token, err := sess.accessToken(ctx)
		if err != nil {
			if ctx.Err() == nil {
				l.errs.Add(1) // the session is lost: the run is aborting with that cause
			}
			return
		}
		header := http.Header{"Authorization": []string{"Bearer " + token}}
		conn, resp, dialErr := dialer.DialContext(ctx, wsURL, header)
		full := resp != nil && resp.StatusCode == http.StatusTooManyRequests
		refusedToken := resp != nil && resp.StatusCode == http.StatusUnauthorized
		var wait time.Duration
		if resp != nil {
			wait = retryAfter(resp.Header)
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		}
		if refusedToken {
			if _, err := sess.renew(ctx, token); err != nil {
				if ctx.Err() == nil {
					l.errs.Add(1)
				}
				return
			}
		}
		if dialErr != nil {
			if ctx.Err() == nil {
				l.errs.Add(1)
			}
			if full {
				l.rejected.Add(1)
				if !signalled {
					refused <- fmt.Errorf("listener %s on %s: %w", l.name, backendHTTP, errWSFull)
					return
				}
			}
			if !full || wait <= 0 {
				wait = 250 * time.Millisecond
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		err = readSocket(ctx, conn, l, func() {
			if !signalled {
				signalled = true
				ready <- struct{}{}
			}
		})
		if signalled && ctx.Err() == nil {
			l.sessionEnded(err)
		}
	}
}

// readSocket reads one session until it ends and returns why (the close frame's error, if any).
func readSocket(ctx context.Context, conn *websocket.Conn, l *wsListener, onConnected func()) error {
	seen, errCount := &l.seen, l.errs
	defer conn.Close()
	// Explicit pong so the server keepalive (ping ~54s / pongWait 60s) stays healthy.
	conn.SetPingHandler(func(appData string) error {
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		return err
	})
	// Unblock ReadMessage on cancel by closing the socket — not SetReadDeadline: gorilla treats a
	// deadline error as permanent and panics on a later read.
	doneRead := make(chan struct{})
	defer close(doneRead)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-doneRead:
		}
	}()
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				errCount.Add(1)
			}
			return err
		}
		var ev struct {
			Type    domain.EventType `json:"type"`
			Payload json.RawMessage  `json:"payload"`
		}
		if json.Unmarshal(msg, &ev) != nil {
			continue
		}
		if ev.Type == domain.EventConnected {
			onConnected()
			continue
		}
		if !ev.Type.IsAppointment() {
			continue
		}
		var appt struct {
			ID      uuid.UUID `json:"id"`
			Version int       `json:"version"`
		}
		if json.Unmarshal(ev.Payload, &appt) != nil || appt.ID == uuid.Nil {
			continue
		}
		l.events.Store(fmt.Sprintf("%s|%d", appt.ID, appt.Version), struct{}{})
		l.arrived(wsArrival{id: appt.ID, version: appt.Version, typ: ev.Type.Name()})
		if ev.Type == domain.EventAppointmentCreated {
			seen.Store(appt.ID, struct{}{})
		}
	}
}

// waitEvents waits until every listener that never dropped delivered every event of want ("id|version"),
// and each backend's listeners together delivered all of them; it returns what is still missing.
func (w *wsWatch) waitEvents(want map[string]bool, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for {
		gaps := append(eventGaps("A", w.listenersA, want, w.chaos), eventGaps("B", w.listenersB, want, w.chaos)...)
		if len(gaps) == 0 || time.Now().After(deadline) {
			return gaps
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// eventGaps is coverageGaps for the "id|version" event keys.
func eventGaps(backend string, listeners []*wsListener, want map[string]bool, chaos bool) []string {
	var gaps []string
	union := map[string]bool{}
	for _, l := range listeners {
		var miss []string
		for k := range want {
			if _, ok := l.events.Load(k); ok {
				union[k] = true
			} else {
				miss = append(miss, k)
			}
		}
		sort.Strings(miss)
		if gap := dropGap(l, chaos); gap != "" {
			gaps = append(gaps, gap)
		}
		if l.lost() == 0 && len(miss) > 0 {
			gaps = append(gaps, fmt.Sprintf("listener %s missed %d of %d events %v", l.name, len(miss), len(want), firstN(miss, 3)))
		}
	}
	if len(listeners) > 0 && len(union) < len(want) && (!chaos || !allDropped(listeners)) {
		gaps = append(gaps, fmt.Sprintf("backend %s (all listeners) missed %d of %d events", backend, len(want)-len(union), len(want)))
	}
	return gaps
}
