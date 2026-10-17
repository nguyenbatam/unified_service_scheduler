package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Signed-in sessions of the stress client (auth contract: short-lived access token + rotating
// refresh cookie). An access token lives ACCESS_TOKEN_TTL (15 min by default); login also sets the
// HttpOnly refresh cookie uss_refresh (Path=/api/v1/auth), and POST /api/v1/auth/refresh with that
// cookie rotates it and answers a fresh access token. Presenting a refresh token that was already
// rotated (outside a 60 s grace) revokes every session of the user, so:
//
//   - each signed-in user has ONE authSession, with its own cookie jar, shared by every apiClient
//     that acts as that user (gateway, backend A, backend B, the hammer, WS listeners);
//   - renewals are serialised by the session's mutex and keyed on the token they replace: however
//     many goroutines find the token due (or get a 401) at once, one refresh is sent and the rest
//     take its result, so the same refresh token is never presented twice;
//   - a refresh whose answer was lost (transport error, 5xx) may have rotated the cookie server
//     side while the jar still holds the old one: that cookie is never presented again, the
//     session signs in afresh with an empty jar instead (when it has credentials).
//
// A session that cannot be renewed is lost for good: every later request fails with errClientAuth
// (never a booking answer) and onLost aborts the run, so the failure is reported as the client's,
// not judged as a 409/422 or a missing booking.

// refreshCookie is the refresh token's cookie name.
const refreshCookie = "uss_refresh"

// renewAtFraction is how far into an access token's lifetime the client refreshes it proactively.
const renewAtFraction = 0.8

// renewAttempts bounds the sends of one renewal (refresh or login) while they are throttled (429).
const renewAttempts = 5

// renewWaitCap caps one wait between renewal attempts (the refresh bucket is per minute).
const renewWaitCap = 30 * time.Second

// errClientAuth marks a request the client could not authenticate: its session is lost (refresh
// refused, re-login failed). It is a client failure, never a server verdict on the request.
var errClientAuth = errors.New("stress client: session lost")

// tokenGrant is one access token and when to replace it, on the local clock.
type tokenGrant struct {
	token      string
	received   time.Time
	expires    time.Time // zero: the server named no expiry (then only a 401 renews it)
	sessionEnd time.Time // the session family's absolute end (zero: unknown)
	renewAt    time.Time
}

// newGrant builds a grant received now. A lifetime too short to be real (clock skew between the
// client and the server's expiresAt) is taken as one second, so renewals cannot spin.
func newGrant(token string, expires, sessionEnd, now time.Time) *tokenGrant {
	g := &tokenGrant{token: token, received: now, expires: expires, sessionEnd: sessionEnd}
	if expires.IsZero() {
		g.renewAt = now.Add(100 * 365 * 24 * time.Hour)
		return g
	}
	life := max(expires.Sub(now), time.Second)
	g.renewAt = now.Add(time.Duration(float64(life) * renewAtFraction))
	return g
}

// lifetime is the grant's access-token lifetime as received (0 when unknown).
func (g *tokenGrant) lifetime() time.Duration {
	if g.expires.IsZero() {
		return 0
	}
	return max(g.expires.Sub(g.received), 0)
}

// sessionGrant is the body of login, register and refresh (times in RFC 3339).
type sessionGrant struct {
	Token            string `json:"token"`
	ExpiresAt        string `json:"expiresAt"`
	SessionExpiresAt string `json:"sessionExpiresAt"`
}

// parseTime reads an RFC 3339 time; empty or unreadable is zero (unknown).
func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// authSession is one signed-in user, shared by every apiClient acting as that user.
type authSession struct {
	label string // the user's email, for messages
	base  string // where login and refresh go

	// web carries the session's cookie jar: login and refresh go through it (and only they need
	// it: the cookie's path is /api/v1/auth). Replaced (with a fresh jar) by a re-login.
	web             *http.Client
	transport       http.RoundTripper
	email, password string // for a re-login; empty: none (refresh only)

	// mint renews a minted session (customers phase: tokens signed with JWT_SECRET, no cookie).
	mint func() (string, time.Time, error)

	onLost   func(error) // called once when the session is lost for good (nil: nothing)
	lostOnce sync.Once

	mu   sync.Mutex // serialises renewals
	cur  atomic.Pointer[tokenGrant]
	lost atomic.Pointer[error]

	firstTTL                      atomic.Int64 // first grant's lifetime (ns)
	refreshes, relogins, remints  atomic.Int64
	retried401, refused401        atomic.Int64 // business calls: retried after a 401 / 401 left to the caller
	refreshThrottled, lostAnswers atomic.Int64
	noCookie                      atomic.Bool // login set no refresh cookie the jar would send back
}

// newJarClient is an http.Client with an empty cookie jar over transport.
func newJarClient(transport http.RoundTripper) *http.Client {
	jar, _ := cookiejar.New(nil) // never fails without options
	return &http.Client{Transport: transport, Jar: jar, Timeout: 30 * time.Second}
}

// newPasswordSession is a session for email at base; it signs in with login.
func newPasswordSession(base, email, password string, transport http.RoundTripper, onLost func(error)) *authSession {
	return &authSession{label: email, base: base, transport: transport, web: newJarClient(transport),
		email: email, password: password, onLost: onLost}
}

// newMintedSession is a session whose tokens mint signs locally (renewal re-mints, no cookie).
func newMintedSession(label string, mint func() (string, time.Time, error), onLost func(error)) (*authSession, error) {
	s := &authSession{label: label, mint: mint, onLost: onLost}
	token, exp, err := mint()
	if err != nil {
		return nil, err
	}
	s.store(newGrant(token, exp, time.Time{}, time.Now()))
	return s, nil
}

func (s *authSession) store(g *tokenGrant) {
	s.firstTTL.CompareAndSwap(0, int64(g.lifetime()))
	s.cur.Store(g)
}

// lostErr is the error that lost the session (nil while it is alive).
func (s *authSession) lostErr() error {
	if p := s.lost.Load(); p != nil {
		return *p
	}
	return nil
}

// fail loses the session for good and reports it once.
func (s *authSession) fail(err error) error {
	// wrapped is never written after it is published: lostErr reads it from other goroutines.
	wrapped := fmt.Errorf("%w: %s: %w", errClientAuth, s.label, err)
	s.lost.CompareAndSwap(nil, &wrapped)
	first := s.lostErr()
	s.lostOnce.Do(func() {
		if s.onLost != nil {
			s.onLost(first)
		}
	})
	return first
}

// accessToken is a valid access token, refreshed first when it is due.
func (s *authSession) accessToken(ctx context.Context) (string, error) {
	if err := s.lostErr(); err != nil {
		return "", err
	}
	g := s.cur.Load()
	if g == nil {
		return "", fmt.Errorf("%w: %s: not signed in", errClientAuth, s.label)
	}
	if time.Now().Before(g.renewAt) {
		return g.token, nil
	}
	return s.renew(ctx, g.token)
}

// renew replaces stale (the token a caller found due, or saw refused with 401) and returns the
// current token. Only the first caller for a given token renews; the others, waiting on the
// mutex, find the token already replaced and take the new one.
func (s *authSession) renew(ctx context.Context, stale string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.lostErr(); err != nil {
		return "", err
	}
	if g := s.cur.Load(); g != nil && g.token != stale {
		return g.token, nil
	}
	if s.mint != nil {
		token, exp, err := s.mint()
		if err != nil {
			return "", s.fail(fmt.Errorf("re-mint: %w", err))
		}
		s.remints.Add(1)
		s.store(newGrant(token, exp, time.Time{}, time.Now()))
		return token, nil
	}
	g, err := s.refreshLocked(ctx)
	if err != nil {
		return "", err
	}
	s.store(g)
	return g.token, nil
}

// login signs in with the session's credentials through an empty jar (a fresh family), sets the
// current grant and reports whether the jar holds a refresh cookie it would send back.
func (s *authSession) login(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, err := s.loginLocked(ctx)
	if err != nil {
		return err
	}
	s.store(g)
	return nil
}

func (s *authSession) loginLocked(ctx context.Context) (*tokenGrant, error) {
	web := newJarClient(s.transport)
	body := map[string]string{"email": s.email, "password": s.password}
	resp, err := s.post(ctx, web, "/api/v1/auth/login", body)
	if err != nil {
		return nil, fmt.Errorf("login %s: %w", s.label, err)
	}
	if resp.code != http.StatusOK {
		return nil, fmt.Errorf("login %s: status %d: %s", s.label, resp.code, truncate(string(resp.body), 160))
	}
	g, err := parseGrant(resp.body)
	if err != nil {
		return nil, fmt.Errorf("login %s: %w", s.label, err)
	}
	s.web = web
	s.noCookie.Store(!hasRefreshCookie(web, s.base))
	return g, nil
}

// refreshLocked renews the access token with the refresh cookie (s.mu held). 429 is retried with
// the same cookie (the server did not look at it); an answer lost or failed (transport error, 5xx)
// leaves the cookie possibly rotated, so the session signs in again instead of replaying it. A 401
// is a refused session: lost, unless its absolute end has passed (then a re-login is by design).
func (s *authSession) refreshLocked(ctx context.Context) (*tokenGrant, error) {
	if s.noCookie.Load() {
		return s.reloginLocked(ctx, errors.New("no refresh cookie in the jar (AUTH_COOKIE_SECURE=true needs an https base URL)"))
	}
	wait := time.Second
	for attempt := 1; ; attempt++ {
		resp, err := s.post(ctx, s.web, "/api/v1/auth/refresh", nil)
		switch {
		case ctx.Err() != nil:
			return nil, fmt.Errorf("refresh %s: %w", s.label, context.Cause(ctx))
		case err != nil || resp.code >= 500:
			s.lostAnswers.Add(1)
			why := fmt.Sprintf("status %d", resp.code)
			if err != nil {
				why = err.Error()
			}
			return s.reloginLocked(ctx, fmt.Errorf("refresh answer lost or failed (%s): the cookie may have rotated", why))
		case resp.code == http.StatusOK:
			g, err := parseGrant(resp.body)
			if err != nil {
				return nil, s.fail(fmt.Errorf("refresh: %w", err))
			}
			s.refreshes.Add(1)
			return g, nil
		case resp.code == http.StatusTooManyRequests && attempt < renewAttempts:
			s.refreshThrottled.Add(1)
			if d := retryAfter(resp.header); d > 0 {
				wait = d
			}
			if err := sleepCtx(ctx, min(wait, renewWaitCap)); err != nil {
				return nil, fmt.Errorf("refresh %s: %w", s.label, err)
			}
			wait *= 2
		case resp.code == http.StatusUnauthorized:
			if g := s.cur.Load(); g != nil && !g.sessionEnd.IsZero() && time.Now().After(g.sessionEnd) {
				return s.reloginLocked(ctx, errors.New("session reached its absolute expiry"))
			}
			return nil, s.fail(fmt.Errorf("refresh refused 401 (session revoked, or a rotated refresh token was replayed): %s", truncate(string(resp.body), 160)))
		default:
			return nil, s.fail(fmt.Errorf("refresh: status %d: %s", resp.code, truncate(string(resp.body), 160)))
		}
	}
}

// reloginLocked signs in afresh (empty jar) after why made the refresh cookie unusable.
func (s *authSession) reloginLocked(ctx context.Context, why error) (*tokenGrant, error) {
	if s.password == "" {
		return nil, s.fail(why)
	}
	wait := time.Second
	for attempt := 1; ; attempt++ {
		g, err := s.loginLocked(ctx)
		if err == nil {
			s.relogins.Add(1)
			return g, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("re-login %s: %w", s.label, context.Cause(ctx))
		}
		if attempt >= renewAttempts {
			return nil, s.fail(fmt.Errorf("%w; re-login: %w", why, err))
		}
		if err := sleepCtx(ctx, min(wait, renewWaitCap)); err != nil {
			return nil, err
		}
		wait *= 2
	}
}

// post sends one auth request through web: JSON body, X-Requested-With: fetch (the refresh CSRF
// guard), no Origin and no Authorization; the jar adds the refresh cookie.
func (s *authSession) post(ctx context.Context, web *http.Client, path string, body any) (response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{}, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, rdr)
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "fetch")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := web.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return response{code: resp.StatusCode, header: resp.Header, body: raw}, err
}

func parseGrant(body []byte) (*tokenGrant, error) {
	var sg sessionGrant
	if err := json.Unmarshal(body, &sg); err != nil || sg.Token == "" {
		return nil, fmt.Errorf("bad session body: %s", truncate(string(body), 160))
	}
	return newGrant(sg.Token, parseTime(sg.ExpiresAt), parseTime(sg.SessionExpiresAt), time.Now()), nil
}

// hasRefreshCookie reports whether web's jar would send the refresh cookie to base's refresh URL.
func hasRefreshCookie(web *http.Client, base string) bool {
	u, err := url.Parse(base + "/api/v1/auth/refresh")
	if err != nil || web.Jar == nil {
		return false
	}
	for _, c := range web.Jar.Cookies(u) {
		if (c.Name == refreshCookie || c.Name == "__Host-"+refreshCookie) && c.Value != "" { // __Host-: AUTH_COOKIE_SECURE=true
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-t.C:
		return nil
	}
}

// retrySafeAfter401 reports whether a request refused 401 may be sent again with a fresh token:
// reads and PUTs are idempotent, and a write carrying an Idempotency-Key or If-Match cannot apply
// twice. (A 401 comes from the auth middleware before any handler runs, but a write without
// either header is still left to its caller rather than resent behind its back.)
func retrySafeAfter401(method string, headers map[string]string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut:
		return true
	}
	for k := range headers {
		switch http.CanonicalHeaderKey(k) {
		case "Idempotency-Key", "If-Match":
			return true
		}
	}
	return false
}

// sessionReport is a session's renewal counters for the report.
func (s *authSession) report() string {
	return fmt.Sprintf("refreshes=%d re-logins=%d re-mints=%d 401-retries=%d 401-unretried=%d refresh-throttled=%d refresh-answers-lost=%d",
		s.refreshes.Load(), s.relogins.Load(), s.remints.Load(), s.retried401.Load(), s.refused401.Load(),
		s.refreshThrottled.Load(), s.lostAnswers.Load())
}

// recordSession reports whether the run's signed-in session lasted the run (Q4, "client
// session"): a run that outlasted one access-token lifetime must have refreshed (PASS) — the
// current token is made current first, so a run that went quiet near its end refreshes here — and
// a shorter run needed none (INFO). A lost session fails.
func recordSession(ctx context.Context, m *matrix, s *authSession, ran time.Duration) {
	const src = "client session"
	if s == nil {
		return
	}
	if err := s.lostErr(); err != nil {
		m.fail("Q4", src, "the stress client lost its signed-in session (a client failure, not a booking answer): %v (%s)", err, s.report())
		return
	}
	ttl := time.Duration(s.firstTTL.Load())
	if ttl <= 0 {
		m.info("Q4", src, "login named no access-token expiry: nothing to refresh (%s)", s.report())
		return
	}
	if ran <= ttl {
		m.info("Q4", src, "run %s within one %s access-token lifetime: no refresh needed (%s)", ran.Round(time.Second), ttl.Round(time.Second), s.report())
		return
	}
	_, err := s.accessToken(ctx)
	m.check("Q4", src, err == nil && s.refreshes.Load() > 0,
		"run %s outlasted the %s access token: the session kept working through POST /api/v1/auth/refresh (%s)%s",
		ran.Round(time.Second), ttl.Round(time.Second), s.report(), errSuffix(err))
}
