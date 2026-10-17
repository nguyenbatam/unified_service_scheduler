package httpapi

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
)

func newSessionTestServer(opts ...Option) *Server {
	return New(nil, nil, auth.New("test-secret"), nil, slog.New(slog.DiscardHandler), "test", opts...)
}

// The refresh cookie is HttpOnly and SameSite=Strict, lives until the token's expiry (already
// capped at the family's end), and is __Host-uss_refresh on Path=/ with Secure (the prefix's
// rules: Secure, Path=/, no Domain), uss_refresh on the auth path without; clearing keeps the
// attributes with Max-Age=0.
func TestRefreshCookieAttributes(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		secure  bool
		token   string
		expires time.Time
		want    string
	}{
		{name: "secure", secure: true, token: "tok", expires: now.Add(24 * time.Hour),
			want: "__Host-uss_refresh=tok; Path=/; Max-Age=86400; HttpOnly; Secure; SameSite=Strict"},
		{name: "plain http stack", secure: false, token: "tok", expires: now.Add(90*time.Minute + 500*time.Millisecond),
			want: "uss_refresh=tok; Path=/api/v1/auth; Max-Age=5400; HttpOnly; SameSite=Strict"},
		{name: "about to end", secure: true, token: "tok", expires: now.Add(200 * time.Millisecond),
			want: "__Host-uss_refresh=tok; Path=/; Max-Age=1; HttpOnly; Secure; SameSite=Strict"},
		{name: "clear secure", secure: true,
			want: "__Host-uss_refresh=; Path=/; Max-Age=0; HttpOnly; Secure; SameSite=Strict"},
		{name: "clear plain", secure: false,
			want: "uss_refresh=; Path=/api/v1/auth; Max-Age=0; HttpOnly; SameSite=Strict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSessionTestServer(WithSessions(SessionConfig{CookieSecure: tt.secure}))
			got := s.newRefreshCookie(tt.token, tt.expires, now).String()
			if got != tt.want {
				t.Fatalf("cookie\n got %s\nwant %s", got, tt.want)
			}
		})
	}
}

// Only the configured name is read; the other mode's name is ignored, and two cookies of the
// configured name are a duplicate (never one of them picked).
func TestRefreshCookieReading(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	token, _, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		secure bool
		cookie string
		want   cookieState
	}{
		{name: "secure name", secure: true, cookie: "__Host-uss_refresh=" + token, want: cookiePresent},
		{name: "plain name on a secure server", secure: true, cookie: "uss_refresh=" + token, want: cookieAbsent},
		{name: "plain name", secure: false, cookie: "uss_refresh=" + token, want: cookiePresent},
		{name: "secure name on a plain server", secure: false, cookie: "__Host-uss_refresh=" + token, want: cookieAbsent},
		{name: "duplicate secure", secure: true, cookie: "__Host-uss_refresh=" + token + "; __Host-uss_refresh=" + other, want: cookieDuplicate},
		{name: "duplicate plain, same value", secure: false, cookie: "uss_refresh=" + token + "; uss_refresh=" + token, want: cookieDuplicate},
		{name: "both modes' names", secure: false, cookie: "__Host-uss_refresh=" + other + "; uss_refresh=" + token, want: cookiePresent},
		{name: "malformed", secure: false, cookie: "uss_refresh=not-a-token", want: cookieAbsent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSessionTestServer(WithSessions(SessionConfig{CookieSecure: tt.secure}))
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/refresh", nil)
			c.Request.Header.Set("Cookie", tt.cookie)
			value, state := s.refreshCookie(c)
			if state != tt.want || (state == cookiePresent) != (value != "") {
				t.Fatalf("state %d value %q, want state %d", state, value, tt.want)
			}
		})
	}
}

// Login and register take a JSON body only: an HTML form's text/plain, urlencoded, or multipart
// body is refused before it is read, so a cross-site form cannot forge one.
func TestLoginRequiresJSONContentType(t *testing.T) {
	h := newSessionTestServer().Handler()
	for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/register"} {
		for _, ct := range []string{"", "text/plain", "text/plain;charset=UTF-8", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", "application/jsonx"} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"email":"a@b.test","password":"secret-pass"}`))
			if ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Content-Type must be application/json") {
				t.Fatalf("%s with Content-Type %q: %d %s, want 400", path, ct, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestWithSessionsDefaults(t *testing.T) {
	s := newSessionTestServer()
	if s.sessions != DefaultSessionConfig() || !s.sessions.CookieSecure {
		t.Fatalf("default sessions = %+v, want %+v (Secure on)", s.sessions, DefaultSessionConfig())
	}
	s = newSessionTestServer(WithSessions(SessionConfig{IdleTTL: 2 * time.Hour, CookieSecure: false}))
	if s.sessions.IdleTTL != 2*time.Hour || s.sessions.MaxTTL != auth.DefaultSessionMaxTTL || s.sessions.CookieSecure ||
		s.sessions.RefreshLimitPerIP != DefaultRefreshLimitPerIP {
		t.Fatalf("sessions = %+v", s.sessions)
	}
}

// The cookie guard: Origin absent (non-browser), same-origin (scheme from X-Forwarded-Proto only
// when the peer is a trusted proxy), or allowlisted; plus X-Requested-With: fetch.
func TestCookieGuard(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	tests := []struct {
		name    string
		opts    []Option
		remote  string
		tls     bool
		headers map[string]string
		want    int
	}{
		{name: "no origin", headers: map[string]string{"X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "same origin", headers: map[string]string{"Origin": "http://scheduler.test", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "header case-insensitive", headers: map[string]string{"Origin": "HTTP://Scheduler.Test", "X-Requested-With": "Fetch"}, want: http.StatusOK},
		{name: "missing X-Requested-With", headers: map[string]string{"Origin": "http://scheduler.test"}, want: http.StatusForbidden},
		{name: "wrong X-Requested-With", headers: map[string]string{"X-Requested-With": "XMLHttpRequest"}, want: http.StatusForbidden},
		{name: "foreign origin", headers: map[string]string{"Origin": "http://evil.test", "X-Requested-With": "fetch"}, want: http.StatusForbidden},
		{name: "other port", headers: map[string]string{"Origin": "http://scheduler.test:8443", "X-Requested-With": "fetch"}, want: http.StatusForbidden},
		{name: "null origin", headers: map[string]string{"Origin": "null", "X-Requested-With": "fetch"}, want: http.StatusForbidden},
		{name: "scheme mismatch", headers: map[string]string{"Origin": "https://scheduler.test", "X-Requested-With": "fetch"}, want: http.StatusForbidden},
		{name: "https connection", tls: true, headers: map[string]string{"Origin": "https://scheduler.test", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "forwarded proto from trusted proxy", opts: []Option{WithTrustedProxies([]string{"10.0.0.0/8"})}, remote: "10.1.2.3:5000",
			headers: map[string]string{"Origin": "https://scheduler.test", "X-Forwarded-Proto": "https", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "forwarded proto from untrusted peer", opts: []Option{WithTrustedProxies([]string{"10.0.0.0/8"})}, remote: "192.0.2.7:5000",
			headers: map[string]string{"Origin": "https://scheduler.test", "X-Forwarded-Proto": "https", "X-Requested-With": "fetch"}, want: http.StatusForbidden},
		{name: "forwarded proto list, trusted single ip", opts: []Option{WithTrustedProxies([]string{"10.1.2.3"})}, remote: "10.1.2.3:5000",
			headers: map[string]string{"Origin": "https://scheduler.test", "X-Forwarded-Proto": "https, http", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "allowlisted origin", opts: []Option{WithAllowedOrigins([]string{"https://app.example.com/"})},
			headers: map[string]string{"Origin": "https://App.Example.com", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "allowlist keeps same origin", opts: []Option{WithAllowedOrigins([]string{"https://app.example.com"})},
			headers: map[string]string{"Origin": "http://scheduler.test", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "sec-fetch-site cross-site", headers: map[string]string{"Sec-Fetch-Site": "cross-site", "X-Requested-With": "fetch"}, want: http.StatusForbidden},
		{name: "sec-fetch-site same-origin", headers: map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "http://scheduler.test", "X-Requested-With": "fetch"}, want: http.StatusOK},
		{name: "sec-fetch-site none", headers: map[string]string{"Sec-Fetch-Site": "none", "X-Requested-With": "fetch"}, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSessionTestServer(tt.opts...)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://scheduler.test/api/v1/auth/refresh", nil)
			if tt.remote != "" {
				req.RemoteAddr = tt.remote
			}
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			c.Request = req
			if s.cookieGuard(c) {
				c.Status(http.StatusOK)
			}
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusForbidden {
				var body struct {
					Error struct{ Code string } `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "forbidden" {
					t.Fatalf("body %s, want error code forbidden", rec.Body.String())
				}
			}
		})
	}
}

// Without a cookie, refresh and logout are 401 and clear the cookie; a malformed cookie is never
// looked up (this server has no store).
func TestRefreshWithoutCookie(t *testing.T) {
	h := newSessionTestServer().Handler()
	for _, cookie := range []string{"", "__Host-uss_refresh=not-a-token"} {
		for _, path := range []string{"/api/v1/auth/refresh", "/api/v1/auth/logout"} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, nil)
			req.Header.Set("X-Requested-With", "fetch")
			if cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s with cookie %q: %d, want 401", path, cookie, rec.Code)
			}
			if got := rec.Header().Get("Set-Cookie"); !strings.HasPrefix(got, "__Host-uss_refresh=;") || !strings.Contains(got, "Max-Age=0") {
				t.Fatalf("%s: Set-Cookie %q, want the cookie cleared", path, got)
			}
		}
	}
}
