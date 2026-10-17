package integration_test

import (
	"context"
	"log/slog"
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
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

// Logout with the refresh cookie only (the access token is gone, e.g. a reloaded tab whose boot
// refresh has not run): every session of the user ends, the cookie is cleared, SESSION_REVOKED.
func TestSessionLogoutWithCookieOnly(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	body, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	_, phone := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	version := tokenVersion(t, pool, seed.DemoCustomerID)

	rec := authCall(t, h, "/api/v1/auth/logout", nil, token, fetchHeaders)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout by cookie: %d %s", rec.Code, rec.Body.String())
	}
	expectCleared(t, "logout by cookie", rec)
	if n := liveFamilies(t, pool, seed.DemoCustomerID); n != 0 {
		t.Fatalf("%d live families after logout", n)
	}
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version+1 || sessionRevokedEvents(t, pool, seed.DemoCustomerID, version+1) != 1 {
		t.Fatalf("token_version %d (want %d) or SESSION_REVOKED missing", got, version+1)
	}
	if code := bearerStatus(t, h, body.Token); code != http.StatusUnauthorized {
		t.Fatalf("access token after logout: %d", code)
	}
	expectEnded(t, "cookie after logout", refreshWith(t, h, phone))
	// A cookie that is no longer live does not authenticate a logout.
	expectEnded(t, "logout with a revoked cookie", authCall(t, h, "/api/v1/auth/logout", nil, token, fetchHeaders))
	expectEnded(t, "logout with nothing", authCall(t, h, "/api/v1/auth/logout", nil, "", fetchHeaders))
}

// Logout with the Bearer token only (non-browser clients): same effect, the cookie is cleared
// anyway, and no CSRF header is needed.
func TestSessionLogoutWithBearerOnly(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	body, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	version := tokenVersion(t, pool, seed.DemoCustomerID)

	rec := authCall(t, h, "/api/v1/auth/logout", nil, "", map[string]string{"Authorization": "Bearer " + body.Token})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout by bearer: %d %s", rec.Code, rec.Body.String())
	}
	expectCleared(t, "logout by bearer", rec)
	if liveFamilies(t, pool, seed.DemoCustomerID) != 0 || tokenVersion(t, pool, seed.DemoCustomerID) != version+1 {
		t.Fatal("bearer logout left a family live or token_version unchanged")
	}
	expectEnded(t, "cookie after bearer logout", refreshWith(t, h, token))

	// A signed-out Bearer falls back to the cookie; with neither valid it is 401.
	again, cookie := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	rec = authCall(t, h, "/api/v1/auth/logout", nil, cookie,
		map[string]string{"Authorization": "Bearer " + body.Token, httpapi.CSRFHeader: httpapi.CSRFHeaderValue})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("stale bearer + live cookie: %d %s", rec.Code, rec.Body.String())
	}
	expectEnded(t, "stale bearer only", authCall(t, h, "/api/v1/auth/logout", nil, "", map[string]string{"Authorization": "Bearer " + again.Token}))
}

// A login from a browser that still holds a refresh cookie replaces that session: the old family
// is revoked, the new one is live, and nothing else is touched.
func TestSessionLoginReplacesBrowserFamily(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	_, old := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	_, otherDevice := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	version := tokenVersion(t, pool, seed.DemoCustomerID)

	// Same browser, another account.
	_, fresh := sessionLogin(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword, old)
	if r := sessionRow(t, pool, old); r.RevokedAt == nil {
		t.Fatal("the browser's previous family is still live")
	}
	if r := sessionRow(t, pool, fresh); r.RevokedAt != nil || r.UserID != seed.DemoAdminID || r.FamilyID == sessionRow(t, pool, old).FamilyID {
		t.Fatalf("new session %+v", r)
	}
	if sessionRow(t, pool, otherDevice).RevokedAt != nil || tokenVersion(t, pool, seed.DemoCustomerID) != version {
		t.Fatal("replacing the browser's session touched another device")
	}
	expectSession(t, refreshWith(t, h, fresh), http.StatusOK)
	expectSession(t, refreshWith(t, h, otherDevice), http.StatusOK)
}

// A socket opened with a ticket from a session's access token lives until the session's absolute
// end (sexp), not the access token's expiry.
func TestSessionWebSocketLivesUntilSessionEnd(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	hub := realtime.NewHub(nil)
	hub.SetKeepAlive(time.Second, 10*time.Second, 200*time.Millisecond)
	// A 2 s access token (JWT expiries are whole seconds: at least 1 s left to mint the ticket) in
	// a 5 s session.
	api := serveAPI(st, svc, auth.New(testSecret, auth.WithAccessTTL(2*time.Second)), hub, "test",
		httpapi.WithSessions(httpapi.SessionConfig{IdleTTL: 5 * time.Second, MaxTTL: 5 * time.Second}))
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go hub.Run(runCtx) // after httpapi.New installed the session validator
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	body, _ := sessionLogin(t, api, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	access := parseAccess(t, body.Token)
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	c := apiClient{t: t, handler: api, token: body.Token}
	c.expect(c.do(http.MethodPost, "/api/v1/auth/ws-ticket", nil, nil), http.StatusOK, &ticket)
	if claims, err := auth.New(testSecret).Parse(ticket.Ticket, auth.AudienceWS); err != nil || claims.SessionExp != access.SessionExp {
		t.Fatalf("ticket sexp %d (%v), want the session's %d", claims.SessionExp, err, access.SessionExp)
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
	accessEnd := access.ExpiresAt.Time
	sessionEnd := time.Unix(access.SessionExp, 0)
	if !sessionEnd.After(accessEnd.Add(time.Second)) {
		t.Fatalf("fixture: session end %s not after the access token's %s", sessionEnd, accessEnd)
	}
	select {
	case err := <-closed:
		t.Fatalf("socket closed at %s, before the session end %s (access token ended %s): %v", time.Now(), sessionEnd, accessEnd, err)
	case <-time.After(time.Until(accessEnd.Add(time.Second))):
	}
	select {
	case err := <-closed:
		if !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
			t.Fatalf("want close 1008 at the session end, got %v", err)
		}
		if time.Now().Before(sessionEnd.Add(-100 * time.Millisecond)) {
			t.Fatalf("closed at %s, before the session end %s", time.Now(), sessionEnd)
		}
	case <-time.After(time.Until(sessionEnd.Add(3 * time.Second))):
		t.Fatal("socket still open after the session end")
	}
}

// The maintenance purge deletes refresh tokens whose family ended or whose idle expiry passed more
// than a day ago, and keeps the rest (a rotated token stays for reuse detection meanwhile).
func TestSessionPurge(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	h := newAPI(st, svc)
	tokens := map[string]string{}
	for _, name := range []string{"live", "rotated-recent", "idle-recent", "idle-old", "family-ended"} {
		_, tokens[name] = sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	}
	set := func(name, sql string) {
		t.Helper()
		mustExec(t, pool, `UPDATE auth_sessions SET `+sql+` WHERE id = $1`, sessionRow(t, pool, tokens[name]).ID)
	}
	set("rotated-recent", `rotated_at = now() - interval '1 hour', expires_at = now() - interval '1 hour'`)
	set("idle-recent", `expires_at = now() - interval '23 hours'`)
	set("idle-old", `expires_at = now() - interval '25 hours'`)
	set("family-ended", `expires_at = now() - interval '1 minute', family_expires_at = now() - interval '1 minute'`)
	// A backlog larger than one purge batch.
	mustExec(t, pool, `
		INSERT INTO auth_sessions (id, family_id, user_id, token_hash, expires_at, family_expires_at)
		SELECT gen_random_uuid(), gen_random_uuid(), $1, sha256(convert_to(g::text, 'UTF8')), now() - interval '2 days', now() - interval '1 day'
		FROM generate_series(1, 2500) g`, seed.DemoCustomerID)

	worker.NewMaintenance(st, nil, nil, slog.Default(), worker.DefaultMaintenanceConfig()).Purge(ctx)

	if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions`); n != 3 {
		t.Fatalf("%d rows after the purge, want 3 (live, rotated-recent, idle-recent)", n)
	}
	for _, name := range []string{"live", "rotated-recent", "idle-recent"} {
		sessionRow(t, pool, tokens[name]) // fails the test when purged
	}
}

// A refresh racing a logout of the same user never leaves a live session: whichever commits
// second sees the other (both take the user's row lock first), so afterwards no family is live and
// no access token the refresh handed out still works.
func TestSessionRefreshRacingLogout(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	h := newAPI(st, svc)
	for round := range 8 {
		body, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
		var refreshed, logout *httptest.ResponseRecorder
		race(
			func() { refreshed = refreshWith(t, h, token) },
			func() {
				logout = authCall(t, h, "/api/v1/auth/logout", nil, "", map[string]string{"Authorization": "Bearer " + body.Token})
			},
		)
		if logout.Code != http.StatusNoContent {
			t.Fatalf("round %d: logout %d %s", round, logout.Code, logout.Body.String())
		}
		switch refreshed.Code {
		case http.StatusOK:
			if code := bearerStatus(t, h, decodeSession(t, refreshed, http.StatusOK).Token); code != http.StatusUnauthorized {
				t.Fatalf("round %d: the refresh's access token survived the logout: %d", round, code)
			}
		case http.StatusUnauthorized:
		default:
			t.Fatalf("round %d: refresh %d %s", round, refreshed.Code, refreshed.Body.String())
		}
		if n := liveFamilies(t, pool, seed.DemoCustomerID); n != 0 {
			t.Fatalf("round %d: %d live families after logout", round, n)
		}
	}
}

// A refresh racing a login that replaces its family (same browser) never leaves the replaced
// family live: the login's revoke waits for the refresh's successor and revokes it too. Raced at
// the store, so both transactions really overlap (over HTTP the login's bcrypt runs first).
func TestSessionRefreshRacingReplacingLogin(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	h := newAPI(st, svc)
	signer := auth.New(testSecret)
	for round := range 16 {
		_, token := sessionLogin(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
		family := sessionRow(t, pool, token).FamilyID
		_, successor := signer.SuccessorRefreshToken(token)
		_, fresh, err := auth.NewRefreshToken()
		if err != nil {
			t.Fatal(err)
		}
		var refreshErr, loginErr error
		var res store.RefreshResult
		race(
			func() {
				res, refreshErr = st.RefreshSession(ctx, store.RefreshRequest{
					TokenHash: auth.HashRefreshToken(token), NewHash: successor,
					IdleTTL: time.Hour, Grace: auth.RefreshReuseGrace,
				})
			},
			func() {
				_, loginErr = st.StartSession(ctx, store.NewSession{
					UserID: seed.DemoAdminID, TokenHash: fresh, IdleTTL: time.Hour, MaxTTL: 2 * time.Hour,
					Replace: auth.HashRefreshToken(token),
				})
			},
		)
		if refreshErr != nil || loginErr != nil {
			t.Fatalf("round %d: refresh %v, login %v", round, refreshErr, loginErr)
		}
		if res.Outcome != store.RefreshRotated && res.Outcome != store.RefreshRevoked {
			t.Fatalf("round %d: refresh outcome %d, want rotated or revoked", round, res.Outcome)
		}
		if n := scalar[int](t, pool, `SELECT count(*)::int FROM auth_sessions WHERE family_id = $1 AND revoked_at IS NULL`, family); n != 0 {
			t.Fatalf("round %d: the replaced family still has %d live tokens (refresh outcome %d)", round, n, res.Outcome)
		}
	}
}
