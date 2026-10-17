package integration_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func loginFrom(t *testing.T, h http.Handler, ip, email, password string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	req.RemoteAddr = ip + ":40000"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// Wrong passwords from one address lock that address out of the account, not the owner: the
// per-email bucket counts failures only, and only past its (high) threshold does it block
// everyone — then identically for right and wrong passwords.
func TestHTTPLoginLockoutIsPerAddressUntilAccountThreshold(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	limiter := newEnforcingLimiter(map[string]int{"login:fail:": 12})
	h := newAPI(st, svc, httpapi.WithRateLimiter(limiter))
	email := seed.DemoCustomerEmail

	// An attacker at one address burns its (IP, email) bucket (10 per window)...
	for i := range 10 {
		if code := loginFrom(t, h, "198.51.100.1", email, "wrong-password"); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d, want 401", i, code)
		}
	}
	if code := loginFrom(t, h, "198.51.100.1", email, seed.DemoCustomerPassword); code != http.StatusTooManyRequests {
		t.Fatalf("attacker's address after 10 attempts: %d, want 429", code)
	}
	// ...while the owner elsewhere still logs in: others' failures are below the account threshold.
	if code := loginFrom(t, h, "203.0.113.9", email, seed.DemoCustomerPassword); code != http.StatusOK {
		t.Fatalf("owner login: %d, want 200", code)
	}
	if n := limiter.count("login:email:" + email); n != 0 {
		t.Fatalf("an all-attempts per-email bucket was counted %d times", n)
	}
	if n := limiter.count("login:fail:" + h.auth.Pseudonym(email)); n != 10 {
		t.Fatalf("failure bucket = %d after 10 failures and a correct login, want 10", n)
	}

	// At the account threshold (12 here, 50 in production) every address gets 429, right
	// password or not, so the lock is no oracle.
	for _, ip := range []string{"198.51.100.2", "198.51.100.4"} {
		if code := loginFrom(t, h, ip, email, "wrong-password"); code != http.StatusUnauthorized {
			t.Fatalf("failure below the account threshold: %d, want 401", code)
		}
	}
	if code := loginFrom(t, h, "203.0.113.9", email, seed.DemoCustomerPassword); code != http.StatusTooManyRequests {
		t.Fatalf("correct password at the account threshold: %d, want 429", code)
	}
	if code := loginFrom(t, h, "198.51.100.3", email, "wrong-password"); code != http.StatusTooManyRequests {
		t.Fatalf("failure past the account threshold: %d, want 429", code)
	}
	if code := loginFrom(t, h, "203.0.113.9", email, seed.DemoCustomerPassword); code != http.StatusTooManyRequests {
		t.Fatalf("correct password past the account threshold: %d, want 429", code)
	}
	// Another account is unaffected.
	if code := loginFrom(t, h, "198.51.100.2", seed.DemoAdminEmail, seed.DemoAdminPassword); code != http.StatusOK {
		t.Fatalf("other account: %d, want 200", code)
	}
}

// With the user cache on, logout on this instance applies at once; another instance serves the
// cached row until its TTL ends or InvalidateUser (SESSION_REVOKED) reaches it.
func TestHTTPUserCacheLogoutAndInvalidate(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	a := newAPI(st, svc, httpapi.WithUserCache(time.Minute))
	b := a.peer("b", httpapi.WithUserCache(time.Minute))

	onA := login(t, a, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	onB := onA.on(b)
	onA.expect(onA.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)
	onB.expect(onB.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)

	onA.expect(onA.do(http.MethodPost, "/api/v1/auth/logout", nil, nil), http.StatusNoContent, nil)
	onA.expect(onA.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusUnauthorized, nil)
	// B still holds the pre-logout row: the bounded stale window.
	onB.expect(onB.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)
	b.InvalidateUser(seed.DemoCustomerID)
	onB.expect(onB.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusUnauthorized, nil)

	// A fresh login after the sign-out works on both, even though B cached the older row.
	fresh := login(t, a, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	freshB := fresh.on(b)
	freshB.expect(freshB.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)

	// Deactivation in SQL (no event) applies when the TTL ends.
	// A fake clock moves time, so a slow (-race) run cannot expire the entry early.
	var clockMu sync.Mutex
	clock := time.Now()
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	short := a.peer("c", httpapi.WithUserCache(time.Minute), httpapi.WithUserCacheClock(now))
	onShort := fresh.on(short)
	onShort.expect(onShort.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)
	if _, err := pool.Exec(ctx, `UPDATE users SET active = false WHERE id = $1`, seed.DemoCustomerID); err != nil {
		t.Fatal(err)
	}
	onShort.expect(onShort.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)
	clockMu.Lock()
	clock = clock.Add(time.Minute)
	clockMu.Unlock()
	onShort.expect(onShort.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusUnauthorized, nil)
}
