package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// An idle-expired token is a plain 401 even when it was rotated long ago: an old cookie of a
// closed tab proves nothing about theft, so nothing of the user is revoked.
func TestSessionExpiredRotatedTokenIsNotReuse(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	_, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	_, next := expectSession(t, refreshWith(t, h, token), http.StatusOK)
	version := tokenVersion(t, pool, seed.DemoCustomerID)
	mustExec(t, pool, `UPDATE auth_sessions SET rotated_at = now() - interval '2 days', expires_at = now() - interval '1 day' WHERE id = $1`,
		sessionRow(t, pool, token).ID)
	expectEnded(t, "idle-expired rotated token", refreshWith(t, h, token))
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version || liveFamilies(t, pool, seed.DemoCustomerID) != 1 {
		t.Fatalf("an expired rotated token revoked the user's sessions (token_version %d, want %d)", got, version)
	}
	expectSession(t, refreshWith(t, h, next), http.StatusOK)
}

// Cookie tossing: two refresh cookies of the configured name (a second one set for another path
// or a parent domain) are refused with the cookie cleared, in both cookie modes, and the session
// is left alone; the other mode's name is ignored.
func TestSessionDuplicateRefreshCookiesAreRefused(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	for _, secure := range []bool{true, false} {
		h := newAPI(st, svc, httpapi.WithSessions(httpapi.SessionConfig{CookieSecure: secure}))
		name := cookieNameOf(h)
		if want := map[bool]string{true: httpapi.SecureRefreshCookieName, false: httpapi.RefreshCookieName}[secure]; name != want {
			t.Fatalf("secure=%v: cookie name %s, want %s", secure, name, want)
		}
		_, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
		tossed, _, err := auth.NewRefreshToken()
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/logout"} {
			for _, header := range []string{name + "=" + tossed + "; " + name + "=" + token, name + "=" + token + "; " + name + "=" + token} {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
				req.Header.Set("Cookie", header)
				req.Header.Set(httpapi.CSRFHeader, httpapi.CSRFHeaderValue)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				expectEnded(t, path+" with two cookies", rec)
			}
		}
		if r := sessionRow(t, pool, token); r.RotatedAt != nil || r.RevokedAt != nil {
			t.Fatalf("secure=%v: a refused duplicate changed the session: %+v", secure, r)
		}
		// The other mode's name next to the real one is not read: the request is not a duplicate.
		other := map[bool]string{true: httpapi.RefreshCookieName, false: httpapi.SecureRefreshCookieName}[secure]
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/refresh", nil)
		req.Header.Set("Cookie", other+"="+tossed+"; "+name+"="+token)
		req.Header.Set(httpapi.CSRFHeader, httpapi.CSRFHeaderValue)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		expectSession(t, rec, http.StatusOK)
	}
}

// Login CSRF: login and register take a JSON Content-Type only, and every auth write refuses
// Sec-Fetch-Site: cross-site — a Bearer logout included.
func TestSessionAuthWritesRefuseCrossSiteForms(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	form := func(path, contentType string, headers map[string]string) *httptest.ResponseRecorder {
		body := `{"email":"` + seed.DemoCustomerEmail + `","password":"` + seed.DemoCustomerPassword + `"}`
		if strings.HasSuffix(path, "/register") {
			body = `{"email":"cross-site@example.com","password":"long-enough","name":"X"}`
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/register"} {
		for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			if rec := form(path, ct, nil); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Content-Type must be application/json") {
				t.Fatalf("%s as %q: %d %s, want 400", path, ct, rec.Code, rec.Body.String())
			}
		}
		if rec := form(path, "application/json", map[string]string{"Sec-Fetch-Site": "cross-site"}); rec.Code != http.StatusForbidden {
			t.Fatalf("%s cross-site: %d, want 403", path, rec.Code)
		}
	}
	if rec := form("/api/v1/auth/login", "application/json; charset=utf-8", map[string]string{"Sec-Fetch-Site": "same-origin"}); rec.Code != http.StatusOK {
		t.Fatalf("same-origin JSON login with a charset: %d %s", rec.Code, rec.Body.String())
	}

	body, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	crossSite := map[string]string{"Sec-Fetch-Site": "cross-site", httpapi.CSRFHeader: httpapi.CSRFHeaderValue}
	if rec := authCall(t, h, "/api/v1/auth/refresh", nil, token, crossSite); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site refresh: %d, want 403", rec.Code)
	}
	crossSite["Authorization"] = "Bearer " + body.Token
	if rec := authCall(t, h, "/api/v1/auth/logout", nil, token, crossSite); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site logout with a Bearer: %d, want 403", rec.Code)
	}
	if liveFamilies(t, pool, seed.DemoCustomerID) == 0 {
		t.Fatal("a refused cross-site request revoked the session")
	}
}

// Cookie-only logout spends the refresh endpoint's per-IP budget; a Bearer logout does not.
func TestSessionCookieLogoutIsThrottledPerIP(t *testing.T) {
	t.Parallel()
	_, st, svc, _ := openTestDB(t)
	limiter := newTestLimiter()
	h := newAPI(st, svc, httpapi.WithRateLimiter(limiter))
	body, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	limiter.denyPrefix("refresh:ip:")
	if rec := authCall(t, h, "/api/v1/auth/logout", nil, token, fetchHeaders); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("cookie-only logout over the refresh budget: %d, want 429", rec.Code)
	}
	if rec := authCall(t, h, "/api/v1/auth/logout", nil, "", map[string]string{"Authorization": "Bearer " + body.Token}); rec.Code != http.StatusNoContent {
		t.Fatalf("bearer logout: %d, want 204 (not throttled)", rec.Code)
	}
}

// A login replacing the browser's family ends that family's access tokens — at once on this
// instance (its cache entry is dropped), within USER_CACHE_TTL on another — and its sockets close
// on the next sweep. The ticket answer is never cached.
func TestSessionReplacedFamilyEndsItsTokensAndSockets(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	hub := realtime.NewHub(nil)
	hub.SetKeepAlive(time.Second, 5*time.Second, 150*time.Millisecond)
	api := newAPIWithHub(st, svc, hub, httpapi.WithUserCache(time.Minute))
	other := api.peer("other") // no cache: reads the family on every request
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go hub.Run(runCtx)
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	old, cookie := sessionLogin(t, api, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	if code := bearerStatus(t, api, old.Token); code != http.StatusOK { // cached: family live
		t.Fatalf("access token before the replacement: %d", code)
	}
	c := apiClient{t: t, handler: api, token: old.Token}
	rec := c.do(http.MethodPost, "/api/v1/auth/ws-ticket", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("ws-ticket: %d, Cache-Control %q, want 200 no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	c.expect(rec, http.StatusOK, &ticket)
	if claims, err := auth.New(testSecret).Parse(ticket.Ticket, auth.AudienceWS); err != nil || claims.SessionID != parseAccess(t, old.Token).SessionID {
		t.Fatalf("ticket sid %v (%v), want the access token's", claims.SessionID, err)
	}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v1/ws/dispatch?ticket="+ticket.Ticket, nil)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
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

	// Same browser, the same account signs in again: the old family is revoked.
	fresh, _ := sessionLogin(t, api, seed.DemoCustomerEmail, seed.DemoCustomerPassword, cookie)
	for name, h := range map[string]http.Handler{"this instance (cache dropped)": api, "another instance": other} {
		if code := bearerStatus(t, h, old.Token); code != http.StatusUnauthorized {
			t.Fatalf("old access token on %s after the replacing login: %d, want 401", name, code)
		}
		if code := bearerStatus(t, h, fresh.Token); code != http.StatusOK {
			t.Fatalf("new access token on %s: %d", name, code)
		}
	}
	select {
	case err := <-closed:
		if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			t.Fatalf("old socket: want close 1008, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced family's socket was not closed by the sweep")
	}
}
