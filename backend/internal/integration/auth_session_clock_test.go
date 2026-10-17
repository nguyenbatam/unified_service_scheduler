package integration_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Every session time and verdict is the database's clock, so two instances whose clocks are ten
// minutes apart (either way) agree: stored times sit on the database clock, the cookie still lives
// the idle TTL, and the grace / reuse / idle-expiry verdicts follow the rows alone. With instance
// clocks, the fast instance would call a 59 s old rotation reuse and a token 30 s from its idle
// expiry expired, and the slow one would call a 61 s old rotation grace.
func TestSessionVerdictsUseTheDatabaseClock(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	skewed := func(name string, by time.Duration) *testAPI {
		return serveAPI(st, svc, auth.New(testSecret, auth.WithClock(func() time.Time { return time.Now().Add(by) })), nil, name)
	}
	fast, slow := skewed("fast", 10*time.Minute), skewed("slow", -10*time.Minute)
	nearDBNow := func(label string, at time.Time) {
		t.Helper()
		if d := scalar[float64](t, pool, `SELECT abs(extract(epoch FROM clock_timestamp() - $1::timestamptz))::float8`, at); d > 5 {
			t.Fatalf("%s is %.0f s off the database clock", label, d)
		}
	}
	maxAgeNearIdle := func(label string, cookieMaxAge int) {
		t.Helper()
		if cookieMaxAge < 86400-5 || cookieMaxAge > 86400 {
			t.Fatalf("%s: cookie Max-Age %d, want the 24h idle TTL whatever the instance clock", label, cookieMaxAge)
		}
	}

	rec := authCall(t, fast, "/api/v1/auth/login", map[string]string{"email": seed.DemoCustomerEmail, "password": seed.DemoCustomerPassword}, "", nil)
	_, token := expectSession(t, rec, http.StatusOK)
	cookie, _ := setRefreshCookie(rec)
	maxAgeNearIdle("login on the fast instance", cookie.MaxAge)
	first := sessionRow(t, pool, token)
	nearDBNow("created_at", scalar[time.Time](t, pool, `SELECT created_at FROM auth_sessions WHERE id = $1`, first.ID))

	rec = refreshWith(t, slow, token)
	_, next := expectSession(t, rec, http.StatusOK)
	cookie, _ = setRefreshCookie(rec)
	maxAgeNearIdle("rotation on the slow instance", cookie.MaxAge)
	nearDBNow("rotated_at", *sessionRow(t, pool, token).RotatedAt)
	nearDBNow("successor created_at", scalar[time.Time](t, pool, `SELECT created_at FROM auth_sessions WHERE id = $1`, sessionRow(t, pool, next).ID))

	version := tokenVersion(t, pool, seed.DemoCustomerID)
	mustExec(t, pool, `UPDATE auth_sessions SET rotated_at = clock_timestamp() - interval '59 seconds' WHERE id = $1`, first.ID)
	if _, again := expectSession(t, refreshWith(t, fast, token), http.StatusOK); again != next {
		t.Fatal("59 s after the rotation the fast instance did not answer the grace (same successor)")
	}
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version {
		t.Fatalf("a grace refresh on the fast instance bumped token_version to %d", got)
	}

	// A live token 30 s from its idle expiry still rotates on the fast instance.
	_, other := sessionLogin(t, slow, seed.DemoCustomerEmail, seed.DemoCustomerPassword, "")
	mustExec(t, pool, `UPDATE auth_sessions SET expires_at = clock_timestamp() + interval '30 seconds' WHERE id = $1`, sessionRow(t, pool, other).ID)
	expectSession(t, refreshWith(t, fast, other), http.StatusOK)

	// 61 s after the rotation the slow instance calls it reuse.
	mustExec(t, pool, `UPDATE auth_sessions SET rotated_at = clock_timestamp() - interval '61 seconds' WHERE id = $1`, first.ID)
	expectEnded(t, "reuse on the slow instance", refreshWith(t, slow, token))
	if got := tokenVersion(t, pool, seed.DemoCustomerID); got != version+1 || liveFamilies(t, pool, seed.DemoCustomerID) != 0 {
		t.Fatalf("reuse on the slow instance: token_version %d (want %d), %d live families", got, version+1, liveFamilies(t, pool, seed.DemoCustomerID))
	}
}
