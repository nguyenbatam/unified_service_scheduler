package integration_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Login answers an access token bound to a new refresh family (sid, sexp) and sets the refresh
// cookie with exactly the contract's attributes; register does the same; the row stores only the
// token's hash.
func TestSessionLoginSetsRefreshCookie(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	before := time.Now()
	rec := authCall(t, h, "/api/v1/auth/login",
		map[string]string{"email": seed.DemoCustomerEmail, "password": seed.DemoCustomerPassword}, "", nil)
	after := time.Now() // login hashes a password: on a slow runner it takes seconds
	body, token := expectSession(t, rec, http.StatusOK)

	raw := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(raw, httpapi.SecureRefreshCookieName+"=") {
		t.Fatalf("Set-Cookie %q: want the __Host- cookie with Secure cookies", raw)
	}
	for _, attr := range []string{"Path=/;", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(raw, "; "+attr) {
			t.Fatalf("Set-Cookie %q lacks %s", raw, attr)
		}
	}
	if strings.Contains(raw, "Domain=") {
		t.Fatalf("Set-Cookie %q: a __Host- cookie has no Domain", raw)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	cookie, _ := setRefreshCookie(rec)
	if cookie.MaxAge < 86400-5 || cookie.MaxAge > 86400 {
		t.Fatalf("Max-Age %d, want the 24h idle TTL", cookie.MaxAge)
	}

	row := sessionRow(t, pool, token)
	if row.UserID != seed.DemoCustomerID || row.RotatedAt != nil || row.RevokedAt != nil ||
		row.UserAgent == nil || *row.UserAgent != "integration-test/1.0" {
		t.Fatalf("session row %+v", row)
	}
	if d := row.ExpiresAt.Sub(before); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("idle expiry in %s, want 24h", d)
	}
	if d := row.FamilyExpiresAt.Sub(before); d < 168*time.Hour-time.Minute || d > 168*time.Hour+time.Minute {
		t.Fatalf("family expiry in %s, want 168h", d)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions WHERE position(convert_to($1, 'UTF8') IN token_hash) > 0`, token); n != 0 {
		t.Fatal("the raw refresh token is stored")
	}

	claims := parseAccess(t, body.Token)
	if claims.SessionID != row.FamilyID || claims.SessionExp != row.FamilyExpiresAt.Unix() {
		t.Fatalf("claims sid %s sexp %d, want family %s ending %d", claims.SessionID, claims.SessionExp, row.FamilyID, row.FamilyExpiresAt.Unix())
	}
	if !mustRFC3339(t, body.SessionExpiresAt).Equal(row.FamilyExpiresAt.Truncate(time.Second)) {
		t.Fatalf("sessionExpiresAt %s, want %s", body.SessionExpiresAt, row.FamilyExpiresAt)
	}
	if exp := mustRFC3339(t, body.ExpiresAt); exp.Sub(after) > 15*time.Minute+time.Second || exp.Sub(before) < 14*time.Minute {
		t.Fatalf("access expiry %s, want the 15m default after a login between %s and %s", exp, before, after)
	}
	if body.User.PasswordHash != "" || body.User.ID != seed.DemoCustomerID {
		t.Fatalf("user %+v", body.User)
	}

	// Register starts a session too; Secure follows the config.
	plain := newAPI(st, svc, httpapi.WithSessions(httpapi.SessionConfig{CookieSecure: false}))
	rec = authCall(t, plain, "/api/v1/auth/register",
		map[string]string{"email": "session-" + strings.ToLower(t.Name()) + "@example.com", "password": "long-enough", "name": "New"}, "", nil)
	_, regToken := expectSession(t, rec, http.StatusCreated)
	if raw := rec.Header().Get("Set-Cookie"); strings.Contains(raw, "Secure") || !strings.HasPrefix(raw, httpapi.RefreshCookieName+"=") ||
		!strings.Contains(raw, "; Path=/api/v1/auth;") {
		t.Fatalf("AUTH_COOKIE_SECURE=false: Set-Cookie %q, want uss_refresh on the auth path without Secure", raw)
	}
	if sessionRow(t, pool, regToken).UserID == seed.DemoCustomerID {
		t.Fatal("register's session belongs to another user")
	}
}

// Refresh rotates: a successor cookie in the same family, the old token answered within the grace
// without a second rotation, and presented after the grace it is reuse — every family of the user
// revoked, token_version bumped with SESSION_REVOKED, 401 with the cookie cleared.
func TestSessionRefreshRotatesAndDetectsReuse(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	_, laptop := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	phoneBody, phone := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	first := sessionRow(t, pool, laptop)

	rec := refreshWith(t, h, laptop)
	body, next := expectSession(t, rec, http.StatusOK)
	if next == laptop {
		t.Fatal("refresh did not rotate the token")
	}
	succ := sessionRow(t, pool, next)
	if succ.FamilyID != first.FamilyID || succ.FamilyExpiresAt != first.FamilyExpiresAt || sessionRow(t, pool, laptop).RotatedAt == nil {
		t.Fatalf("successor %+v of %+v: want the same family, the old row rotated", succ, first)
	}
	if claims := parseAccess(t, body.Token); claims.SessionID != first.FamilyID || claims.SessionExp != first.FamilyExpiresAt.Unix() {
		t.Fatalf("refreshed claims sid %s sexp %d", claims.SessionID, claims.SessionExp)
	}
	if code := bearerStatus(t, h, body.Token); code != http.StatusOK {
		t.Fatalf("refreshed access token: /auth/me %d", code)
	}

	// Within the grace (another tab racing the same cookie, or the first response was lost): an
	// access token and the same successor cookie again, no second rotation.
	rec = refreshWith(t, h, laptop)
	graceBody, again := expectSession(t, rec, http.StatusOK)
	if again != next {
		t.Fatal("the grace answer set another cookie than the rotation's successor")
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions WHERE family_id = $1`, first.FamilyID); n != 2 {
		t.Fatalf("family has %d tokens after a grace refresh, want 2 (no second rotation)", n)
	}
	if claims := parseAccess(t, graceBody.Token); claims.SessionID != first.FamilyID {
		t.Fatalf("grace token for family %s, want %s", claims.SessionID, first.FamilyID)
	}

	// After the grace: reuse.
	version := tokenVersion(t, pool, seed.DemoCustomerID)
	mustExec(t, pool, `UPDATE auth_sessions SET rotated_at = now() - interval '61 seconds' WHERE id = $1`, first.ID)
	expectEnded(t, "reused token", refreshWith(t, h, laptop))
	if n := liveFamilies(t, pool, seed.DemoCustomerID); n != 0 {
		t.Fatalf("%d live families after reuse, want 0", n)
	}
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version+1 {
		t.Fatalf("token_version %d after reuse, want %d", got, version+1)
	}
	if n := sessionRevokedEvents(t, pool, seed.DemoCustomerID, version+1); n != 1 {
		t.Fatalf("%d SESSION_REVOKED events with version %d, want 1", n, version+1)
	}
	for name, token := range map[string]string{"refreshed": body.Token, "grace": graceBody.Token, "phone": phoneBody.Token} {
		if code := bearerStatus(t, h, token); code != http.StatusUnauthorized {
			t.Fatalf("%s access token after reuse: %d, want 401", name, code)
		}
	}
	// The successor and the other device's family are revoked too; presenting a revoked token is
	// only a 401, never another revocation.
	expectEnded(t, "successor after reuse", refreshWith(t, h, next))
	expectEnded(t, "other device after reuse", refreshWith(t, h, phone))
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version+1 {
		t.Fatalf("token_version %d after presenting revoked tokens, want %d (no second revocation)", got, version+1)
	}

	// Past the grace the successor itself was rotated: a grace answer for the old token sends no
	// cookie (the client already holds a newer one).
	_, a := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	_, b := expectSession(t, refreshWith(t, h, a), http.StatusOK)
	expectSession(t, refreshWith(t, h, b), http.StatusOK)
	rec = refreshWith(t, h, a)
	decodeSession(t, rec, http.StatusOK)
	expectNoCookie(t, "grace after the successor rotated", rec)
}

// Two tabs refreshing the same cookie at once: both get an access token, exactly one rotation
// happens (the row lock serialises them; the later one reads the rotation and takes the grace).
func TestSessionConcurrentRefreshRotatesOnce(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	_, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	family := sessionRow(t, pool, token).FamilyID
	version := tokenVersion(t, pool, seed.DemoCustomerID)

	const tabs = 8
	recs := make([]*httptest.ResponseRecorder, tabs)
	raceN(tabs, func(i int) { recs[i] = refreshWith(t, h, token) })
	cookies := map[string]int{}
	for i, rec := range recs {
		body, cookie := expectSession(t, rec, http.StatusOK)
		cookies[cookie]++
		if code := bearerStatus(t, h, body.Token); code != http.StatusOK {
			t.Fatalf("tab %d access token: %d", i, code)
		}
	}
	if len(cookies) != 1 {
		t.Fatalf("tabs got %d different cookies, want every one the single successor", len(cookies))
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions WHERE family_id = $1`, family); n != 2 {
		t.Fatalf("family has %d tokens, want 2", n)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions WHERE family_id = $1 AND rotated_at IS NOT NULL`, family); n != 1 {
		t.Fatalf("%d rotated tokens, want 1", n)
	}
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version {
		t.Fatalf("token_version %d, want %d (no reuse)", got, version)
	}
}

// An idle-expired token and a family past its absolute end are 401 with the cookie cleared, and
// nothing else of the user is revoked; a refresh near the family's end never extends it.
func TestSessionRefreshExpiry(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	_, idle := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	_, ended := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	_, ending := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	version := tokenVersion(t, pool, seed.DemoCustomerID)

	mustExec(t, pool, `UPDATE auth_sessions SET expires_at = now() - interval '1 second' WHERE id = $1`, sessionRow(t, pool, idle).ID)
	expectEnded(t, "idle-expired", refreshWith(t, h, idle))
	mustExec(t, pool, `UPDATE auth_sessions SET expires_at = now() - interval '1 second', family_expires_at = now() - interval '1 second' WHERE id = $1`,
		sessionRow(t, pool, ended).ID)
	expectEnded(t, "family ended", refreshWith(t, h, ended))
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version {
		t.Fatalf("expiry bumped token_version to %d (want %d): expiry is not reuse", got, version)
	}

	// Ten minutes left in the family: the successor (24h idle) is capped at its end, and so are
	// the cookie and the access token (15m).
	row := sessionRow(t, pool, ending)
	mustExec(t, pool, `UPDATE auth_sessions SET expires_at = now() + interval '5 minutes', family_expires_at = now() + interval '10 minutes' WHERE id = $1`, row.ID)
	familyEnd := sessionRow(t, pool, ending).FamilyExpiresAt
	rec := refreshWith(t, h, ending)
	body, next := expectSession(t, rec, http.StatusOK)
	succ := sessionRow(t, pool, next)
	if !succ.ExpiresAt.Equal(familyEnd) || !succ.FamilyExpiresAt.Equal(familyEnd) {
		t.Fatalf("successor expires %s (family %s), want both at the family's end %s", succ.ExpiresAt, succ.FamilyExpiresAt, familyEnd)
	}
	if cookie, _ := setRefreshCookie(rec); cookie.MaxAge > 600 || cookie.MaxAge < 590 {
		t.Fatalf("cookie Max-Age %d, want capped at the ~600 s left", cookie.MaxAge)
	}
	if exp := mustRFC3339(t, body.ExpiresAt); exp.After(familyEnd) || !mustRFC3339(t, body.SessionExpiresAt).Equal(familyEnd.Truncate(time.Second)) {
		t.Fatalf("access expiry %s / session %s past the family's end %s", body.ExpiresAt, body.SessionExpiresAt, familyEnd)
	}
}

// A deactivated user's refresh is 401: every family revoked, cookie cleared.
func TestSessionRefreshInactiveUser(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	_, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	mustExec(t, pool, `UPDATE users SET active = false WHERE id = $1`, seed.DemoCustomerID)
	expectEnded(t, "inactive user", refreshWith(t, h, token))
	if n := liveFamilies(t, pool, seed.DemoCustomerID); n != 0 {
		t.Fatalf("%d live families of a deactivated user, want 0", n)
	}
	expectEnded(t, "unknown token", refreshWith(t, h, strings.Repeat("A", 43)))
}

// CSRF: a cookie request from a foreign Origin or without X-Requested-With is 403 and changes
// nothing; same-origin and allowlisted origins pass. Login from a foreign origin is refused too.
func TestSessionRefreshCSRFGuard(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc, httpapi.WithAllowedOrigins([]string{"https://app.example.com"}))
	_, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	family := sessionRow(t, pool, token).FamilyID

	for name, headers := range map[string]map[string]string{
		"foreign origin":           {"Origin": "https://evil.example.net", httpapi.CSRFHeader: httpapi.CSRFHeaderValue},
		"missing X-Requested-With": {"Origin": "http://example.com"},
		"no header at all":         nil,
	} {
		rec := authCall(t, h, "/api/v1/auth/refresh", nil, token, headers)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"forbidden"`) {
			t.Fatalf("%s: %d %s, want 403 forbidden", name, rec.Code, rec.Body.String())
		}
		expectNoCookie(t, name, rec)
		rec = authCall(t, h, "/api/v1/auth/logout", nil, token, headers)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s logout by cookie: %d, want 403", name, rec.Code)
		}
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions WHERE family_id = $1`, family); n != 1 || liveFamilies(t, pool, seed.DemoCustomerID) != 1 {
		t.Fatal("a refused cookie request changed the session")
	}

	// httptest requests target http://example.com: that is the same origin.
	rec := authCall(t, h, "/api/v1/auth/refresh", nil, token,
		map[string]string{"Origin": "http://example.com", httpapi.CSRFHeader: httpapi.CSRFHeaderValue})
	_, token = expectSession(t, rec, http.StatusOK)
	rec = authCall(t, h, "/api/v1/auth/refresh", nil, token,
		map[string]string{"Origin": "https://app.example.com", httpapi.CSRFHeader: httpapi.CSRFHeaderValue})
	expectSession(t, rec, http.StatusOK)

	rec = authCall(t, h, "/api/v1/auth/login", map[string]string{"email": seed.DemoCustomerEmail, "password": seed.DemoCustomerPassword}, "",
		map[string]string{"Origin": "https://evil.example.net"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login: %d, want 403", rec.Code)
	}
}
