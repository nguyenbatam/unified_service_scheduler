package integration_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func dealershipVersion(t *testing.T, rec *httptest.ResponseRecorder, c apiClient) int {
	t.Helper()
	var items []domain.Dealership
	c.expect(rec, http.StatusOK, &items)
	for _, d := range items {
		if d.ID == seed.DealerNorth {
			return d.Version
		}
	}
	t.Fatal("Northside missing from GET /dealerships")
	return 0
}

// GET /dealerships carries a content ETag (304 on If-None-Match). A change made through an
// instance shows there at once; another instance may serve its cached body until its TTL ends.
func TestHTTPCatalogETagAndPerInstanceCache(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	a := newAPI(st, svc)
	b := a.peer("b", httpapi.WithCatalogCache(time.Minute))
	adminA := login(t, a, seed.DemoAdminEmail, seed.DemoAdminPassword)
	adminB := adminA.on(b)

	first := adminA.do(http.MethodGet, "/api/v1/dealerships", nil, nil)
	etag := first.Header().Get("ETag")
	version := dealershipVersion(t, first, adminA)
	if etag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("headers %v", first.Header())
	}
	adminA.expect(adminA.do(http.MethodGet, "/api/v1/dealerships", nil, map[string]string{"If-None-Match": etag}), http.StatusNotModified, nil)
	// B caches the pre-change body.
	if got := dealershipVersion(t, adminB.do(http.MethodGet, "/api/v1/dealerships", nil, nil), adminB); got != version {
		t.Fatalf("B version %d, want %d", got, version)
	}

	adminA.expect(adminA.do(http.MethodPatch, "/api/v1/admin/dealerships/"+seed.DealerNorth.String(),
		map[string]any{"stagingCapacity": 7}, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)}), http.StatusOK, nil)

	after := adminA.do(http.MethodGet, "/api/v1/dealerships", nil, map[string]string{"If-None-Match": etag})
	if got := dealershipVersion(t, after, adminA); got != version+1 || after.Header().Get("ETag") == etag {
		t.Fatalf("A after patch: version %d etag %q, want %d and a new tag", got, after.Header().Get("ETag"), version+1)
	}
	// Bounded staleness on the other instance (documented): still the cached body.
	if got := dealershipVersion(t, adminB.do(http.MethodGet, "/api/v1/dealerships", nil, nil), adminB); got != version {
		t.Fatalf("B within its TTL: version %d, want the cached %d", got, version)
	}

	var types []domain.ServiceType
	rec := adminA.do(http.MethodGet, "/api/v1/service-types", nil, nil)
	adminA.expect(rec, http.StatusOK, &types)
	if len(types) == 0 || rec.Header().Get("ETag") == "" {
		t.Fatalf("service types %d, etag %q", len(types), rec.Header().Get("ETag"))
	}
	adminA.expect(adminA.do(http.MethodGet, "/api/v1/service-types", nil, map[string]string{"If-None-Match": rec.Header().Get("ETag")}), http.StatusNotModified, nil)
}

func rawPost(t *testing.T, h http.Handler, token, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Request bodies are exactly one JSON value of at most 1 MiB, on every route that reads one.
func TestHTTPBodyLimitsAndTrailingData(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	h := newAPI(st, svc)
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	book := `{"dealershipId":"` + seed.DealerNorth.String() + `","vehicleId":"` + seed.VehicleA.String() + `","serviceTypeId":"` + seed.ServiceOil.String() + `","startAt":"2030-01-01T09:00:00Z"}`

	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	tests := []struct {
		name, path, body string
		want             int
		wantCode         string
	}{
		{name: "trailing object", path: "/api/v1/appointments", body: book + `{"startAt":"2030-01-02T09:00:00Z"}`, want: 400, wantCode: domain.CodeInvalid},
		{name: "trailing garbage", path: "/api/v1/vehicles", body: `{"vin":"1HGCM82633A004352","make":"Honda","model":"Accord","year":2020} x`, want: 400, wantCode: domain.CodeInvalid},
		{name: "unknown field", path: "/api/v1/vehicles", body: `{"vin":"1HGCM82633A004352","colour":"red"}`, want: 400, wantCode: domain.CodeInvalid},
		{name: "over 1 MiB", path: "/api/v1/vehicles", body: `{"make":"` + strings.Repeat("x", 1<<20) + `"}`, want: 413, wantCode: "too_large"},
		{name: "login over 1 MiB", path: "/api/v1/auth/login", body: `{"email":"` + strings.Repeat("x", 1<<20) + `"}`, want: 413, wantCode: "too_large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			customer.expect(rawPost(t, h, customer.token, tt.path, tt.body), tt.want, &errBody)
			if errBody.Error.Code != tt.wantCode {
				t.Fatalf("code %q, want %q", errBody.Error.Code, tt.wantCode)
			}
		})
	}
	// Nothing was booked or registered by the rejected bodies.
	var appts []domain.Appointment
	customer.expect(customer.do(http.MethodGet, "/api/v1/appointments?vehicleId="+seed.VehicleA.String(), nil, nil), http.StatusOK, &appts)
	for _, a := range appts {
		if a.StartAt.Year() == 2030 {
			t.Fatalf("rejected body booked %s", a.ID)
		}
	}
}

// Customers' plain reads are throttled per user (the "read" rule); staff reads and the session
// check (/auth/me) are not counted.
func TestHTTPCustomerReadsAreThrottled(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	limiter := newTestLimiter()
	h := newAPI(st, svc, httpapi.WithRateLimiter(limiter))
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	admin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)

	paths := []string{"/api/v1/appointments", "/api/v1/vehicles", "/api/v1/vehicles/" + seed.VehicleA.String(),
		"/api/v1/dealerships", "/api/v1/service-types"}
	for _, p := range paths {
		customer.expect(customer.do(http.MethodGet, p, nil, nil), http.StatusOK, nil)
	}
	limiter.denyPrefix("read:user:")
	for _, p := range paths {
		rec := customer.do(http.MethodGet, p, nil, nil)
		customer.expect(rec, http.StatusTooManyRequests, nil)
		if rec.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: 429 without Retry-After", p)
		}
		admin.expect(admin.do(http.MethodGet, p, nil, nil), http.StatusOK, nil)
	}
	// The session check is never throttled (the web app signs out when it fails).
	customer.expect(customer.do(http.MethodGet, "/api/v1/auth/me", nil, nil), http.StatusOK, nil)
}
