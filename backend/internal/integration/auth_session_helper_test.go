package integration_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
)

// sessionBody is the login / register / refresh response.
type sessionBody struct {
	Token            string      `json:"token"`
	ExpiresAt        string      `json:"expiresAt"`
	SessionExpiresAt string      `json:"sessionExpiresAt"`
	User             domain.User `json:"user"`
}

// authCall sends a JSON POST to an auth endpoint with the refresh cookie (when not empty) and
// headers, as a browser on the API's own origin would (no Origin unless headers set one).
func authCall(t *testing.T, h http.Handler, path string, body any, cookie string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "integration-test/1.0")
	if cookie != "" {
		req.Header.Set("Cookie", cookieNameOf(h)+"="+cookie) // what a browser sends back
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// cookieNameOf is the refresh cookie name h uses (__Host-uss_refresh with Secure cookies, the
// default; uss_refresh on a plain-http configuration).
func cookieNameOf(h http.Handler) string {
	if named, ok := h.(interface{ RefreshCookieName() string }); ok {
		return named.RefreshCookieName()
	}
	return httpapi.SecureRefreshCookieName
}

// fetchHeaders is what the web app sends on a cookie request.
var fetchHeaders = map[string]string{httpapi.CSRFHeader: httpapi.CSRFHeaderValue}

// refreshWith calls POST /auth/refresh with cookie and the CSRF header.
func refreshWith(t *testing.T, h http.Handler, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	return authCall(t, h, "/api/v1/auth/refresh", nil, cookie, fetchHeaders)
}

// sessionLogin logs in (carrying cookie, when not empty) and returns the body and the new
// refresh cookie's value.
func sessionLogin(t *testing.T, h http.Handler, email, password, cookie string) (sessionBody, string) {
	t.Helper()
	rec := authCall(t, h, "/api/v1/auth/login", map[string]string{"email": email, "password": password}, cookie, nil)
	return expectSession(t, rec, http.StatusOK)
}

// expectSession requires status and a session body with a refresh cookie; it returns both.
func expectSession(t *testing.T, rec *httptest.ResponseRecorder, status int) (sessionBody, string) {
	t.Helper()
	body := decodeSession(t, rec, status)
	c, ok := setRefreshCookie(rec)
	if !ok || c.Value == "" || c.MaxAge <= 0 {
		t.Fatalf("no refresh cookie set: %q", rec.Header().Values("Set-Cookie"))
	}
	return body, c.Value
}

// decodeSession requires status and decodes a session body with a token.
func decodeSession(t *testing.T, rec *httptest.ResponseRecorder, status int) sessionBody {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body.String())
	}
	var body sessionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Token == "" || body.SessionExpiresAt == "" {
		t.Fatalf("session body %s (%v)", rec.Body.String(), err)
	}
	return body
}

// setRefreshCookie is the refresh cookie a response set, if any.
func setRefreshCookie(rec *httptest.ResponseRecorder) (*http.Cookie, bool) {
	for _, c := range rec.Result().Cookies() {
		if c.Name == httpapi.SecureRefreshCookieName || c.Name == httpapi.RefreshCookieName {
			return c, true
		}
	}
	return nil, false
}

// expectEnded requires 401 and the refresh cookie cleared with its attributes.
func expectEnded(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s: status %d, want 401: %s", label, rec.Code, rec.Body.String())
	}
	expectCleared(t, label, rec)
}

// expectCleared requires the response to clear the refresh cookie with its own attributes:
// Max-Age=0, HttpOnly, SameSite=Strict, and the mode's name and path (__Host-: Secure, Path=/).
func expectCleared(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	raw := rec.Header().Get("Set-Cookie")
	c, ok := setRefreshCookie(rec)
	wantPath := map[string]string{httpapi.SecureRefreshCookieName: "/", httpapi.RefreshCookieName: httpapi.RefreshCookiePath}
	if !ok || c.Value != "" || c.MaxAge >= 0 || !strings.Contains(raw, "Max-Age=0") || c.Path != wantPath[c.Name] ||
		!c.HttpOnly || c.SameSite != http.SameSiteStrictMode || (c.Name == httpapi.SecureRefreshCookieName) != c.Secure {
		t.Fatalf("%s: Set-Cookie %q, want the refresh cookie cleared", label, raw)
	}
}

// expectNoCookie requires that the response set no refresh cookie.
func expectNoCookie(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	if c, ok := setRefreshCookie(rec); ok {
		t.Fatalf("%s: unexpected Set-Cookie %v", label, c)
	}
}

// bearerStatus is the status of GET /auth/me with token.
func bearerStatus(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	return apiClient{t: t, handler: h, token: token}.do(http.MethodGet, "/api/v1/auth/me", nil, nil).Code
}

// sessionRowOf is the auth_sessions row of a refresh token.
type sessionRowOf struct {
	ID, FamilyID, UserID       uuid.UUID
	ExpiresAt, FamilyExpiresAt time.Time
	RotatedAt, RevokedAt       *time.Time
	UserAgent                  *string
}

func sessionRow(t *testing.T, pool *pgxpool.Pool, token string) sessionRowOf {
	t.Helper()
	var r sessionRowOf
	if err := pool.QueryRow(t.Context(), `
		SELECT id, family_id, user_id, expires_at, family_expires_at, rotated_at, revoked_at, user_agent
		FROM auth_sessions WHERE token_hash = $1`, auth.HashRefreshToken(token)).Scan(
		&r.ID, &r.FamilyID, &r.UserID, &r.ExpiresAt, &r.FamilyExpiresAt, &r.RotatedAt, &r.RevokedAt, &r.UserAgent); err != nil {
		t.Fatalf("session row: %v", err)
	}
	return r
}

// liveFamilies counts the user's refresh families with an unrevoked token.
func liveFamilies(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	return scalar[int](t, pool, `SELECT count(DISTINCT family_id)::int FROM auth_sessions WHERE user_id = $1 AND revoked_at IS NULL`, user)
}

func tokenVersion(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	return scalar[int](t, pool, `SELECT token_version FROM users WHERE id = $1`, user)
}

// sessionRevokedEvents counts SESSION_REVOKED outbox rows of user carrying version.
func sessionRevokedEvents(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, version int) int {
	t.Helper()
	return scalar[int](t, pool, `
		SELECT count(*)::int FROM outbox_events
		WHERE aggregate_type = $1 AND aggregate_id = $2 AND event_type = $3 AND (payload->>'tokenVersion')::int = $4`,
		domain.AggregateUser, user, domain.EventSessionRevoked, version)
}

// parseAccess parses an access token signed with the test secret.
func parseAccess(t *testing.T, token string) auth.Claims {
	t.Helper()
	claims, err := auth.New(testSecret).Parse(token, auth.AudienceAPI)
	if err != nil {
		t.Fatalf("parse access token: %v", err)
	}
	return claims
}

func mustRFC3339(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return at
}
