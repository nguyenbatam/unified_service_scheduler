package httpapi_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

// denyLimiter records every bucket and denies all of them, so login stops before the store.
type denyLimiter struct {
	mu      sync.Mutex
	calls   int
	buckets []string
	limits  []int
}

func (d *denyLimiter) AllowAll(_ context.Context, buckets ...cache.Bucket) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	for _, b := range buckets {
		for range b.Hits() { // a bucket of cost n is n hits
			d.buckets = append(d.buckets, b.Key)
			d.limits = append(d.limits, b.Limit)
		}
	}
	return false, nil
}

func (d *denyLimiter) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, error) {
	return d.AllowAll(ctx, cache.Bucket{Key: bucket, Limit: limit, Window: window})
}

// singleLimiter has no AllowAll: buckets are checked one call each.
type singleLimiter struct {
	buckets []string
}

func (s *singleLimiter) Allow(_ context.Context, bucket string, _ int, _ time.Duration) (bool, error) {
	s.buckets = append(s.buckets, bucket)
	return true, nil
}

func login(t *testing.T, h http.Handler, email string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"email":"`+email+`","password":"secret-pass"}`))
	req.RemoteAddr = "192.0.2.10:4321" // the TCP peer: the gateway, or a client hitting us directly
	req.Header.Set("Content-Type", "application/json")
	maps.Copy(req.Header, header)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestLoginRateLimitClientIP(t *testing.T) {
	spoofed := http.Header{"X-Forwarded-For": []string{"203.0.113.7"}}
	tests := []struct {
		name    string
		proxies []string
		header  http.Header
		wantIP  string
	}{
		{name: "no trusted proxies ignores X-Forwarded-For", header: spoofed, wantIP: "192.0.2.10"},
		{name: "untrusted peer cannot pick its bucket", proxies: []string{"10.0.0.0/8"}, header: spoofed, wantIP: "192.0.2.10"},
		{name: "trusted gateway forwards the client", proxies: []string{"192.0.2.0/24"}, header: spoofed, wantIP: "203.0.113.7"},
		{name: "trusted gateway, X-Real-IP", proxies: []string{"192.0.2.10"},
			header: http.Header{"X-Real-Ip": []string{"203.0.113.8"}}, wantIP: "203.0.113.8"},
		{name: "IPv6 client is bucketed by its /64", proxies: []string{"192.0.2.10"},
			header: http.Header{"X-Forwarded-For": []string{"2001:db8:1:2:aaaa:bbbb:cccc:dddd"}}, wantIP: "2001:db8:1:2::/64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := &denyLimiter{}
			h := httpapi.New(nil, nil, auth.New("test-secret"), realtime.NewHub(nil), nil, "test",
				httpapi.WithRateLimiter(limiter), httpapi.WithTrustedProxies(tt.proxies)).Handler()
			rec := login(t, h, "a@b.test", tt.header)
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "300" {
				t.Fatalf("status %d, Retry-After %q; want 429 / 300", rec.Code, rec.Header().Get("Retry-After"))
			}
			// The IP and the (IP, email) pair are counted together in one round trip; no bucket
			// keyed on the email alone is touched before the password is checked, and the email
			// appears only as its keyed pseudonym.
			want := []string{"login:ip:" + tt.wantIP, "login:ip-email:" + tt.wantIP + "|" + auth.New("test-secret").Pseudonym("a@b.test")}
			if limiter.calls != 1 || !slices.Equal(limiter.buckets, want) || strings.Contains(strings.Join(limiter.buckets, " "), "a@b.test") {
				t.Fatalf("calls %d buckets %v, want 1 call %v", limiter.calls, limiter.buckets, want)
			}
		})
	}
}

// retryLimiter denies and reports how long until the exceeded window ends.
type retryLimiter struct{ retry time.Duration }

func (r retryLimiter) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return false, nil
}

func (r retryLimiter) AllowAllRetry(context.Context, ...cache.Bucket) (bool, time.Duration, error) {
	return false, r.retry, nil
}

// Retry-After is the exceeded window's remaining time (rounded up), not a full window.
func TestRetryAfterIsRemainingWindow(t *testing.T) {
	for _, tt := range []struct {
		retry time.Duration
		want  string
	}{{1500 * time.Millisecond, "2"}, {10 * time.Millisecond, "1"}, {42 * time.Second, "42"}} {
		h := httpapi.New(nil, nil, auth.New("test-secret"), realtime.NewHub(nil), nil, "test",
			httpapi.WithRateLimiter(retryLimiter{retry: tt.retry})).Handler()
		rec := login(t, h, "a@b.test", nil)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != tt.want {
			t.Fatalf("retry %s: status %d Retry-After %q, want 429 / %s", tt.retry, rec.Code, rec.Header().Get("Retry-After"), tt.want)
		}
	}
}

func TestLoginOverlongEmailIsNotARedisKey(t *testing.T) {
	limiter := &singleLimiter{}
	h := httpapi.New(nil, nil, auth.New("test-secret"), realtime.NewHub(nil), nil, "test",
		httpapi.WithRateLimiter(limiter)).Handler()
	for _, email := range []string{"", strings.Repeat("x", 250) + "@b.test"} {
		limiter.buckets = nil
		rec := login(t, h, email, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("email len %d: status %d, want 401", len(email), rec.Code)
		}
		if !slices.Equal(limiter.buckets, []string{"login:ip:192.0.2.10"}) {
			t.Fatalf("email len %d: buckets %v, want only the IP bucket", len(email), limiter.buckets)
		}
	}
}

// downLimiter is a shared limiter whose backend (Redis) is unreachable.
type downLimiter struct{}

func (downLimiter) Allow(context.Context, string, int, time.Duration) (bool, error) {
	return false, errors.New("dial tcp 127.0.0.1:6379: connection refused")
}

// With Redis down, login keeps its per-IP limit (in memory, per instance) instead of failing open.
func TestLoginLimitSurvivesRedisOutage(t *testing.T) {
	h := httpapi.New(nil, nil, auth.New("fallback-test-secret-0123456789abcdef"), realtime.NewHub(nil), nil, "test",
		httpapi.WithRateLimiter(downLimiter{})).Handler()
	// An empty email is rejected after counting the per-IP bucket only (no store needed).
	for i := range 21 {
		rec := login(t, h, "", nil)
		want := http.StatusUnauthorized
		if i == 20 {
			want = http.StatusTooManyRequests
		}
		if rec.Code != want {
			t.Fatalf("attempt %d: status %d, want %d", i+1, rec.Code, want)
		}
		if want == http.StatusTooManyRequests {
			// The in-memory window just started: the wait is what is left of it (5 min), not more.
			if got := rec.Header().Get("Retry-After"); got != "300" && got != "299" {
				t.Fatalf("Retry-After %q, want the remaining ~300 s", got)
			}
		}
	}
}

// Other buckets still fail open while Redis is down: an outage must not lock customers out.
func TestOtherThrottlesFailOpenOnRedisOutage(t *testing.T) {
	authSvc := auth.New("fallback-test-secret-0123456789abcdef")
	h := httpapi.New(nil, nil, authSvc, realtime.NewHub(nil), nil, "test", httpapi.WithRateLimiter(downLimiter{})).Handler()
	customer := domain.User{ID: uuid.New(), Role: domain.RoleCustomer, CustomerID: uuid.New()}
	token, _, err := authSvc.Issue(customer)
	if err != nil {
		t.Fatal(err)
	}
	for range 40 { // over the 30/min ws-ticket limit
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/auth/ws-ticket", nil)
		req.Method = http.MethodPost
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatal("ws-ticket throttled while Redis is down; only login/register fall back")
		}
	}
}

// Register answers 409 for a taken email, so its limits bound probing for accounts: per client IP
// and cluster-wide, counted together in one round trip before any password hashing.
func TestRegisterRateLimits(t *testing.T) {
	limiter := &denyLimiter{}
	h := httpapi.New(nil, nil, auth.New("test-secret"), realtime.NewHub(nil), nil, "test",
		httpapi.WithRateLimiter(limiter)).Handler()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/auth/register",
		strings.NewReader(`{"email":"new@b.test","password":"secret-pass","name":"New Customer"}`))
	req.RemoteAddr = "192.0.2.10:4321"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "900" {
		t.Fatalf("status %d, Retry-After %q; want 429 / 900", rec.Code, rec.Header().Get("Retry-After"))
	}
	wantBuckets := []string{"register:ip:192.0.2.10", "register:global"}
	if limiter.calls != 1 || !slices.Equal(limiter.buckets, wantBuckets) || !slices.Equal(limiter.limits, []int{5, 300}) {
		t.Fatalf("calls %d buckets %v limits %v, want 1 call %v limits [5 300]",
			limiter.calls, limiter.buckets, limiter.limits, wantBuckets)
	}
}
