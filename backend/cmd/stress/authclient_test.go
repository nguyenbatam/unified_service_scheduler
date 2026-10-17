package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// authFake is the auth contract in miniature: login sets the uss_refresh cookie, access tokens
// expire after ttl, POST /api/v1/auth/refresh rotates the cookie (a token rotated less than grace
// ago answers a fresh access token without rotating again; one rotated longer ago, or unknown, is
// reuse: every family revoked, 401), and /api/v1/thing and /api/v1/ws/dispatch accept only a live
// access token.
type authFake struct {
	ttl, grace time.Duration
	// refreshCode, when set, answers every refresh with it (after rotating, for a 5xx: the answer is
	// lost on the way, as when the gateway loses the backend).
	refreshCode atomic.Int64
	always401   atomic.Bool // /api/v1/thing refuses every token

	mu                                   sync.Mutex
	seq                                  int
	access                               map[string]time.Time // access token → expiry
	rotated                              map[string]time.Time // refresh token → rotation time (zero: current)
	presented                            map[string]int       // refresh token → times presented
	revoked                              bool
	logins, refreshes, reuses, graceHits int
	thingCalls                           map[string]int // method → sends
	thing401s, wsAccepted, wsRefused     int
	refreshedTokens                      map[string]bool // access tokens minted by a refresh
	wsTokens                             []string
}

func newAuthFake(ttl time.Duration) *authFake {
	return &authFake{ttl: ttl, grace: 60 * time.Second, access: map[string]time.Time{}, rotated: map[string]time.Time{},
		presented: map[string]int{}, thingCalls: map[string]int{}, refreshedTokens: map[string]bool{}}
}

// mint issues an access token and a refresh token (f.mu held).
func (f *authFake) mint(w http.ResponseWriter, rotate bool) string {
	f.seq++
	tok := fmt.Sprintf("access-%d", f.seq)
	f.access[tok] = time.Now().Add(f.ttl)
	if rotate {
		rt := fmt.Sprintf("refresh-%d", f.seq)
		f.rotated[rt] = time.Time{}
		http.SetCookie(w, &http.Cookie{Name: refreshCookie, Value: rt, Path: "/api/v1/auth", HttpOnly: true, SameSite: http.SameSiteStrictMode}) //nolint:gosec // test fake on plain http: the jar never sends a Secure cookie back over http
	}
	return tok
}

func (f *authFake) grant(w http.ResponseWriter, tok string) {
	writeJSON(w, map[string]any{"token": tok, "expiresAt": f.access[tok], "sessionExpiresAt": time.Now().Add(time.Hour), "user": map[string]any{}})
}

func (f *authFake) live(r *http.Request) (string, bool) {
	tok := r.Header.Get("Authorization")
	if len(tok) < 7 {
		return "", false
	}
	tok = tok[7:]
	exp, ok := f.access[tok]
	return tok, ok && !f.revoked && time.Now().Before(exp)
}

// expireAll ends every access token now (a server-side expiry the client cannot see coming).
func (f *authFake) expireAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.access {
		f.access[k] = time.Now().Add(-time.Second)
	}
}

func (f *authFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	switch r.URL.Path {
	case "/api/v1/auth/login":
		defer f.mu.Unlock()
		f.logins++
		f.revoked = false
		f.grant(w, f.mint(w, true))
	case "/api/v1/auth/refresh":
		defer f.mu.Unlock()
		f.refresh(w, r)
	case "/api/v1/thing":
		f.thingCalls[r.Method]++
		_, ok := f.live(r)
		if !ok || f.always401.Load() {
			f.thing401s++
			f.mu.Unlock()
			http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Unlock()
		writeJSON(w, map[string]string{"ok": "yes"})
	case "/api/v1/ws/dispatch":
		tok, ok := f.live(r)
		if !ok {
			f.wsRefused++
			f.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.wsAccepted++
		f.wsTokens = append(f.wsTokens, tok)
		f.mu.Unlock()
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":15}`))
		time.Sleep(150 * time.Millisecond) // then drop it: the listener redials
		_ = conn.Close()
	default:
		f.mu.Unlock()
		http.NotFound(w, r)
	}
}

func (f *authFake) refresh(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "fetch" || r.Header.Get("Authorization") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	c, err := r.Cookie(refreshCookie)
	if err != nil {
		http.Error(w, "no cookie", http.StatusUnauthorized)
		return
	}
	f.presented[c.Value]++
	at, known := f.rotated[c.Value]
	switch {
	case !known || f.revoked:
		f.reuses++
		f.revoked = true
		http.Error(w, "unknown", http.StatusUnauthorized)
		return
	case !at.IsZero() && time.Since(at) < f.grace:
		f.graceHits++
		f.grant(w, f.mint(w, false))
		return
	case !at.IsZero():
		f.reuses++
		f.revoked = true
		http.Error(w, "reuse", http.StatusUnauthorized)
		return
	}
	if code := int(f.refreshCode.Load()); code != 0 && code < 500 {
		http.Error(w, "refused", code)
		return
	}
	f.rotated[c.Value] = time.Now()
	f.refreshes++
	tok := f.mint(w, true)
	f.refreshedTokens[tok] = true
	if code := int(f.refreshCode.Load()); code >= 500 {
		w.Header().Del("Set-Cookie") // the answer (and its cookie) never reaches the client
		http.Error(w, "bad gateway", code)
		return
	}
	f.grant(w, tok)
}

// stats reads the fake's counters under its lock.
func (f *authFake) stats(read func(f *authFake)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	read(f)
}

func signedIn(t *testing.T, srv *httptest.Server, onLost func(error)) *apiClient {
	t.Helper()
	api := newAPI(srv.URL)
	if err := api.login(t.Context(), "admin@example.com", "pw", onLost); err != nil {
		t.Fatal(err)
	}
	return api
}

// A 1 s access-token TTL: many goroutines of one user, through two clients sharing the session,
// keep calling across several expiries; every call succeeds, the token is refreshed (ahead of its
// expiry, or after a 401 the retry absorbs), and each refresh token is presented exactly once
// (never replayed: no reuse detection, no grace needed).
func TestSessionKeepsWorkingAcrossAccessExpiry(t *testing.T) {
	f := newAuthFake(time.Second)
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := signedIn(t, srv, nil)
	direct := api.asUser(srv.URL) // another client of the same user shares the session
	began := time.Now()
	var wg sync.WaitGroup
	var failed atomic.Int64
	for g := range 16 {
		c := api
		if g%2 == 1 {
			c = direct
		}
		wg.Go(func() {
			for time.Since(began) < 3500*time.Millisecond {
				resp, err := c.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil)
				if err != nil || resp.code != http.StatusOK {
					failed.Add(1)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
	wg.Wait()
	if n := failed.Load(); n > 0 {
		t.Fatalf("%d calls failed across the access-token expiries", n)
	}
	f.stats(func(f *authFake) {
		if f.refreshes < 3 || f.reuses != 0 || f.graceHits != 0 || f.logins != 1 {
			t.Fatalf("refreshes=%d reuses=%d graceHits=%d logins=%d; want ≥3 refreshes and no reuse, grace or re-login",
				f.refreshes, f.reuses, f.graceHits, f.logins)
		}
		for rt, n := range f.presented {
			if n != 1 {
				t.Fatalf("refresh token %s presented %d times", rt, n)
			}
		}
	})
	m := newMatrix()
	recordSession(t.Context(), m, api.auth, time.Since(began))
	if ev := m.rows["Q4"]; len(ev) != 1 || ev[0].Verdict != verdictPass {
		t.Fatalf("client session evidence = %+v, want one PASS", ev)
	}
}

// A run shorter than one access-token lifetime needs no refresh: INFO, never a PASS or FAIL.
func TestRecordSessionShortRunIsInfo(t *testing.T) {
	srv := httptest.NewServer(newAuthFake(time.Hour))
	defer srv.Close()
	api := signedIn(t, srv, nil)
	m := newMatrix()
	recordSession(t.Context(), m, api.auth, time.Minute)
	if ev := m.rows["Q4"]; len(ev) != 1 || ev[0].Verdict != verdictInfo {
		t.Fatalf("evidence = %+v, want one INFO", ev)
	}
}

// Tokens the server stops accepting before their expiry: every concurrent caller gets a 401, and
// together they send exactly one refresh, then each retries once and succeeds.
func TestConcurrent401sShareOneRefresh(t *testing.T) {
	f := newAuthFake(time.Hour)
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := signedIn(t, srv, nil)
	f.expireAll()
	var wg sync.WaitGroup
	var ok atomic.Int64
	start := make(chan struct{})
	for range 50 {
		wg.Go(func() {
			<-start
			resp, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil)
			if err == nil && resp.code == http.StatusOK {
				ok.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if ok.Load() != 50 {
		t.Fatalf("%d of 50 calls succeeded", ok.Load())
	}
	f.stats(func(f *authFake) {
		if f.refreshes != 1 || f.reuses != 0 || f.graceHits != 0 {
			t.Fatalf("refreshes=%d reuses=%d graceHits=%d, want exactly one refresh", f.refreshes, f.reuses, f.graceHits)
		}
	})
	if got := api.auth.retried401.Load(); got == 0 || got > 50 {
		t.Fatalf("retried401 = %d", got)
	}
}

// After a 401 the token is renewed, but only a request safe to resend is sent again: reads, PUT,
// and writes carrying an Idempotency-Key or If-Match. A plain POST gets its 401 back. A second 401
// is final (no loop).
func TestRetryAfter401OnlyWhenSafe(t *testing.T) {
	cases := []struct {
		name      string
		method    string
		headers   map[string]string
		wantCode  int
		wantSends int
	}{
		{"read", http.MethodGet, nil, http.StatusOK, 2},
		{"idempotent PUT", http.MethodPut, nil, http.StatusOK, 2},
		{"write with Idempotency-Key", http.MethodPost, map[string]string{"Idempotency-Key": "k"}, http.StatusOK, 2},
		{"write with If-Match", http.MethodPost, map[string]string{"if-match": `"3"`}, http.StatusOK, 2},
		{"plain write: not resent", http.MethodPost, nil, http.StatusUnauthorized, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAuthFake(time.Hour)
			srv := httptest.NewServer(f)
			defer srv.Close()
			api := signedIn(t, srv, nil)
			f.expireAll()
			resp, err := api.do(t.Context(), tc.method, "/api/v1/thing", map[string]int{"a": 1}, tc.headers)
			if err != nil || resp.code != tc.wantCode {
				t.Fatalf("do = %d, %v; want %d", resp.code, err, tc.wantCode)
			}
			f.stats(func(f *authFake) {
				if f.thingCalls[tc.method] != tc.wantSends || f.refreshes != 1 {
					t.Fatalf("sends=%d refreshes=%d, want %d sends and one refresh", f.thingCalls[tc.method], f.refreshes, tc.wantSends)
				}
			})
			// The session is renewed either way: the next call goes through.
			if resp, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil); err != nil || resp.code != http.StatusOK {
				t.Fatalf("next call = %d, %v", resp.code, err)
			}
		})
	}
	t.Run("second 401 is final", func(t *testing.T) {
		f := newAuthFake(time.Hour)
		srv := httptest.NewServer(f)
		defer srv.Close()
		api := signedIn(t, srv, nil)
		f.always401.Store(true)
		resp, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil)
		if err != nil || resp.code != http.StatusUnauthorized {
			t.Fatalf("do = %d, %v; want the 401", resp.code, err)
		}
		f.stats(func(f *authFake) {
			if f.thingCalls[http.MethodGet] != 2 || f.refreshes != 1 {
				t.Fatalf("sends=%d refreshes=%d, want 2 sends, 1 refresh", f.thingCalls[http.MethodGet], f.refreshes)
			}
		})
	})
}

// A refresh the server refuses loses the session: the call fails with errClientAuth (a client
// failure, never a booking answer), the run's hook fires once however many callers hit it, later
// calls fail at once without a send, and a booking through it is neither a 409 nor a 422.
func TestRefusedRefreshIsAClientFailure(t *testing.T) {
	f := newAuthFake(time.Hour)
	srv := httptest.NewServer(f)
	defer srv.Close()
	var lost atomic.Int64
	var lostErr atomic.Pointer[error]
	api := signedIn(t, srv, func(err error) { lost.Add(1); lostErr.Store(&err) })
	f.refreshCode.Store(http.StatusUnauthorized)
	f.expireAll()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil); !errors.Is(err, errClientAuth) {
				t.Errorf("err = %v, want errClientAuth", err)
			}
		})
	}
	wg.Wait()
	if lost.Load() != 1 || !errors.Is(*lostErr.Load(), errClientAuth) {
		t.Fatalf("onLost called %d times", lost.Load())
	}
	var sends int
	f.stats(func(f *authFake) { sends = f.thingCalls[http.MethodGet] })
	stats := newRunStats()
	r := sendWithRetry(t.Context(), api, map[string]any{}, "key", 6, stats)
	if r.outcome == outConflict || r.outcome == outUnavailable || r.outcome == outCreated || stats.conflicts.Load()+stats.unavailable.Load() != 0 {
		t.Fatalf("booking through a lost session = %v (conflicts %d, unavailable %d)", r.outcome, stats.conflicts.Load(), stats.unavailable.Load())
	}
	if _, definite := sendUntilDefinite(t.Context(), api, http.MethodGet, "/api/v1/thing", nil, nil, probeAttempts); definite {
		t.Fatal("a lost session gave a definite answer")
	}
	f.stats(func(f *authFake) {
		if f.thingCalls[http.MethodGet] != sends || f.thingCalls[http.MethodPost] != 0 || f.presented["refresh-1"] != 1 {
			t.Fatalf("sends after the loss: GET %d→%d POST %d; refresh token presented %d times", sends, f.thingCalls[http.MethodGet], f.thingCalls[http.MethodPost], f.presented["refresh-1"])
		}
	})
	m := newMatrix()
	recordSession(t.Context(), m, api.auth, 2*time.Hour)
	if ev := m.rows["Q4"]; len(ev) != 1 || ev[0].Verdict != verdictFail {
		t.Fatalf("evidence = %+v, want one FAIL", ev)
	}
}

// A refresh whose answer is lost (the server rotated, the client never saw the new cookie) is not
// retried with the old cookie, which would be reuse once the grace has passed: the session signs
// in afresh with an empty jar, and the next refresh uses that family's cookie.
func TestLostRefreshAnswerSignsInAgain(t *testing.T) {
	f := newAuthFake(time.Hour)
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := signedIn(t, srv, nil)
	f.refreshCode.Store(http.StatusBadGateway)
	f.expireAll()
	if resp, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil); err != nil || resp.code != http.StatusOK {
		t.Fatalf("do = %d, %v", resp.code, err)
	}
	f.refreshCode.Store(0)
	f.expireAll()
	if resp, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil); err != nil || resp.code != http.StatusOK {
		t.Fatalf("second renewal: do = %d, %v", resp.code, err)
	}
	f.stats(func(f *authFake) {
		if f.logins != 2 || f.reuses != 0 || f.graceHits != 0 || f.presented["refresh-1"] != 1 {
			t.Fatalf("logins=%d reuses=%d graceHits=%d first token presented %d times", f.logins, f.reuses, f.graceHits, f.presented["refresh-1"])
		}
	})
	if api.auth.relogins.Load() != 1 || api.auth.refreshes.Load() != 1 {
		t.Fatalf("session: %s", api.auth.report())
	}
}

// A throttled refresh (429) was not looked at: it is resent with the same cookie after the wait.
func TestThrottledRefreshIsResent(t *testing.T) {
	f := newAuthFake(time.Hour)
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := signedIn(t, srv, nil)
	f.refreshCode.Store(http.StatusTooManyRequests)
	f.expireAll()
	time.AfterFunc(300*time.Millisecond, func() { f.refreshCode.Store(0) })
	if resp, err := api.do(t.Context(), http.MethodGet, "/api/v1/thing", nil, nil); err != nil || resp.code != http.StatusOK {
		t.Fatalf("do = %d, %v", resp.code, err)
	}
	if api.auth.refreshThrottled.Load() == 0 || api.auth.refreshes.Load() != 1 || api.auth.relogins.Load() != 0 {
		t.Fatalf("session: %s", api.auth.report())
	}
}

// A minted session (customers phase: no cookie) renews by minting the next token before expiry.
func TestMintedSessionRemints(t *testing.T) {
	var n atomic.Int64
	sess, err := newMintedSession("c@example.com", func() (string, time.Time, error) {
		return fmt.Sprintf("minted-%d", n.Add(1)), time.Now().Add(time.Second), nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := sess.accessToken(t.Context())
	time.Sleep(850 * time.Millisecond)
	next, err := sess.accessToken(t.Context())
	if err != nil || next == first || sess.remints.Load() != 1 {
		t.Fatalf("after 85%% of the lifetime: %q → %q, %v (%s)", first, next, err, sess.report())
	}
}

// A WS listener redials with the session's current token: across access-token expiries every
// handshake is accepted, and the later ones carry refreshed tokens.
func TestWSRedialUsesFreshToken(t *testing.T) {
	f := newAuthFake(time.Second)
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := signedIn(t, srv, nil)
	w := newWSWatch(t.Context(), api.auth)
	if err := w.start(srv.URL, srv.URL, 1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond)
	w.stop()
	f.stats(func(f *authFake) {
		fresh := 0
		for _, tok := range f.wsTokens {
			if f.refreshedTokens[tok] {
				fresh++
			}
		}
		if f.wsAccepted < 4 || f.wsRefused != 0 || fresh == 0 || f.reuses != 0 {
			t.Fatalf("handshakes accepted=%d refused=%d with refreshed tokens=%d reuses=%d", f.wsAccepted, f.wsRefused, fresh, f.reuses)
		}
	})
}

// tokenSession is a session holding a fixed token (no renewal source) for tests.
func tokenSession(token string) *authSession {
	s := &authSession{label: "test"}
	s.store(newGrant(token, time.Time{}, time.Time{}, time.Now()))
	return s
}
