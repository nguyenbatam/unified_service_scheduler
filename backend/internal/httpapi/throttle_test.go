package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

// throttled sends one request as user through a limiter that denies everything and returns the
// response plus the buckets the request was counted in.
func throttled(t *testing.T, user domain.User, method, path, body string) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	authSvc := auth.New("throttle-test-secret-0123456789abcdef")
	limiter := &denyLimiter{}
	h := httpapi.New(nil, nil, authSvc, realtime.NewHub(nil), nil, "test", httpapi.WithRateLimiter(limiter)).Handler()
	token, _, err := authSvc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.RemoteAddr = "192.0.2.10:4321"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, limiter.buckets
}

func count(buckets []string, key string) int {
	n := 0
	for _, b := range buckets {
		if b == key {
			n++
		}
	}
	return n
}

func TestThrottles(t *testing.T) {
	customer := domain.User{ID: uuid.New(), Role: domain.RoleCustomer, CustomerID: uuid.New()}
	staff := domain.User{ID: uuid.New(), Role: domain.RoleDispatcher}
	avail := "/api/v1/availability/next?dealershipId=" + uuid.NewString() + "&vehicleId=" + uuid.NewString() + "&serviceTypeId=" + uuid.NewString()
	userKey := func(name string, u domain.User) string { return name + ":user:" + u.ID.String() }
	ipKey := func(name string) string { return name + ":ip:192.0.2.10" }
	tests := []struct {
		name     string
		user     domain.User
		method   string
		path     string
		body     string
		want429  bool
		wantKeys map[string]int // bucket → hits
	}{
		{
			name: "customer vehicle registration is a throttled write", user: customer,
			method: http.MethodPost, path: "/api/v1/vehicles", body: `{"vin":"WBA00000000000001","make":"BMW","model":"3","year":2022}`,
			want429: true, wantKeys: map[string]int{userKey("write", customer): 1, ipKey("write"): 1},
		},
		{
			name: "availability per user and per IP", user: customer, method: http.MethodGet,
			path: strings.Replace(avail, "/next", "", 1) + "&date=2030-01-01", want429: true,
			wantKeys: map[string]int{userKey("availability", customer): 1, ipKey("availability"): 1},
		},
		{
			name: "next availability default 14 days costs 2", user: customer, method: http.MethodGet, path: avail, want429: true,
			wantKeys: map[string]int{userKey("availability", customer): 2, ipKey("availability"): 2},
		},
		{
			name: "next availability 31 days costs 5", user: customer, method: http.MethodGet, path: avail + "&days=31", want429: true,
			wantKeys: map[string]int{userKey("availability", customer): 5, ipKey("availability"): 5},
		},
		{
			name: "next availability 7 days costs 1", user: customer, method: http.MethodGet, path: avail + "&days=7", want429: true,
			wantKeys: map[string]int{userKey("availability", customer): 1, ipKey("availability"): 1},
		},
		{
			name: "appointment list is a throttled read", user: customer, method: http.MethodGet, path: "/api/v1/appointments", want429: true,
			wantKeys: map[string]int{userKey("read", customer): 1, ipKey("read"): 1},
		},
		{
			name: "appointment detail is a throttled read", user: customer, method: http.MethodGet, path: "/api/v1/appointments/" + uuid.NewString(), want429: true,
			wantKeys: map[string]int{userKey("read", customer): 1, ipKey("read"): 1},
		},
		{
			name: "vehicle list is a throttled read", user: customer, method: http.MethodGet, path: "/api/v1/vehicles?q=bmw", want429: true,
			wantKeys: map[string]int{userKey("read", customer): 1, ipKey("read"): 1},
		},
		{
			name: "vehicle detail is a throttled read", user: customer, method: http.MethodGet, path: "/api/v1/vehicles/" + uuid.NewString(), want429: true,
			wantKeys: map[string]int{userKey("read", customer): 1, ipKey("read"): 1},
		},
		{
			name: "dealership catalog is a throttled read", user: customer, method: http.MethodGet, path: "/api/v1/dealerships", want429: true,
			wantKeys: map[string]int{userKey("read", customer): 1, ipKey("read"): 1},
		},
		{
			name: "service catalog is a throttled read", user: customer, method: http.MethodGet, path: "/api/v1/service-types?cursor=x", want429: true,
			wantKeys: map[string]int{userKey("read", customer): 1, ipKey("read"): 1},
		},
		{
			// The web app signs out on a failed session check, so a dry shared-IP bucket must not
			// reach /auth/me.
			name: "me is never throttled", user: customer, method: http.MethodGet, path: "/api/v1/auth/me",
			wantKeys: map[string]int{},
		},
		{
			name: "ws tickets are throttled for customers", user: customer, method: http.MethodPost, path: "/api/v1/auth/ws-ticket", want429: true,
			wantKeys: map[string]int{userKey("ws-ticket", customer): 1},
		},
		{
			name: "ws tickets are throttled for staff too", user: staff, method: http.MethodPost, path: "/api/v1/auth/ws-ticket", want429: true,
			wantKeys: map[string]int{userKey("ws-ticket", staff): 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, buckets := throttled(t, tt.user, tt.method, tt.path, tt.body)
			if tt.want429 && rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status %d, want 429 (%s)", rec.Code, rec.Body.String())
			}
			total := 0
			for key, hits := range tt.wantKeys {
				if got := count(buckets, key); got != hits {
					t.Fatalf("bucket %s counted %d times, want %d (all: %v)", key, got, hits, buckets)
				}
				total += hits
			}
			if len(buckets) != total {
				t.Fatalf("buckets %v, want only %v", buckets, tt.wantKeys)
			}
		})
	}
}

// Staff desk work is not counted by the self-service throttles (writes and availability).
func TestThrottlesSkipStaffSelfServiceBuckets(t *testing.T) {
	staff := domain.User{ID: uuid.New(), Role: domain.RoleAdmin}
	// The empty body fails validation (400, no customerId) after the throttle: no bucket touched.
	rec, buckets := throttled(t, staff, http.MethodPost, "/api/v1/vehicles", `{}`)
	if rec.Code != http.StatusBadRequest || slices.ContainsFunc(buckets, func(b string) bool { return strings.HasPrefix(b, "write:") }) {
		t.Fatalf("staff throttled: %d %v", rec.Code, buckets)
	}
}

// Staff are not counted by the plain-read throttle either. The bad limit is refused (400) after
// the (skipped) throttle, before any store call.
func TestReadThrottleSkipsStaff(t *testing.T) {
	staff := domain.User{ID: uuid.New(), Role: domain.RoleDispatcher}
	for _, path := range []string{"/api/v1/dealerships?limit=0", "/api/v1/service-types?limit=0", "/api/v1/vehicles?limit=0"} {
		rec, buckets := throttled(t, staff, http.MethodGet, path, "")
		if rec.Code != http.StatusBadRequest || len(buckets) != 0 {
			t.Fatalf("%s: staff read throttled: %d %v", path, rec.Code, buckets)
		}
	}
}

// The read budget uses the configured limits (WithReadLimits) and the limiter's Retry-After.
func TestReadThrottleLimits(t *testing.T) {
	customer := domain.User{ID: uuid.New(), Role: domain.RoleCustomer, CustomerID: uuid.New()}
	authSvc := auth.New("throttle-test-secret-0123456789abcdef")
	tests := []struct {
		name           string
		opts           []httpapi.Option
		perUser, perIP int
	}{
		{name: "defaults", perUser: httpapi.DefaultReadLimitPerUser, perIP: httpapi.DefaultReadLimitPerIP},
		{name: "configured", opts: []httpapi.Option{httpapi.WithReadLimits(7, 70)}, perUser: 7, perIP: 70},
		{name: "non-positive keeps defaults", opts: []httpapi.Option{httpapi.WithReadLimits(0, -1)}, perUser: httpapi.DefaultReadLimitPerUser, perIP: httpapi.DefaultReadLimitPerIP},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limiter := &denyLimiter{}
			opts := append([]httpapi.Option{httpapi.WithRateLimiter(limiter)}, tt.opts...)
			h := httpapi.New(nil, nil, authSvc, realtime.NewHub(nil), nil, "test", opts...).Handler()
			token, _, err := authSvc.Issue(customer)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/appointments", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "60" {
				t.Fatalf("got %d Retry-After %q, want 429 60", rec.Code, rec.Header().Get("Retry-After"))
			}
			if !slices.Equal(limiter.limits, []int{tt.perUser, tt.perIP}) {
				t.Fatalf("limits %v, want [%d %d]", limiter.limits, tt.perUser, tt.perIP)
			}
		})
	}
}
