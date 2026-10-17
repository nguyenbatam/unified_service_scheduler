package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type healthBody struct {
	Status    string            `json:"status"`
	Instance  string            `json:"instance"`
	Checks    map[string]string `json:"checks"`
	WSClients *int              `json:"wsClients"`
}

func getHealth(t *testing.T, h http.Handler, path string) (int, healthBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	var body healthBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v (%s)", path, err, rec.Body.String())
	}
	return rec.Code, body
}

func TestHealthLivenessAndReadiness(t *testing.T) {
	var dbErr, redisErr error
	var probes atomic.Int32
	probe := func(context.Context) (error, error) {
		probes.Add(1)
		return dbErr, redisErr
	}
	tests := []struct {
		name       string
		db, redis  error
		saturated  bool
		wantCode   int
		wantStatus string
	}{
		{name: "all up", wantCode: http.StatusOK, wantStatus: "ok"},
		{name: "redis down is not fatal", redis: errors.New("redis down"), wantCode: http.StatusOK, wantStatus: "degraded"},
		{name: "postgres down", db: errors.New("db down"), wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable"},
		// A full request pool is load, not an outage: Postgres stays "ok" (own probe connection).
		{name: "saturated pool is degraded, not down", saturated: true, wantCode: http.StatusOK, wantStatus: "degraded"},
		{name: "postgres down wins over saturation", db: errors.New("db down"), saturated: true, wantCode: http.StatusServiceUnavailable, wantStatus: "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(nil, nil, auth.New("health-test-secret-0123456789abcdef"), realtime.NewHub(nil), nil, "backend-1")
			h := s.Handler()
			dbErr, redisErr = tt.db, tt.redis
			s.poolSaturated = func() bool { return tt.saturated }
			wantPool, wantPostgres := checkOK, checkOK
			if tt.saturated {
				wantPool = checkSaturated
			}
			if tt.db != nil {
				wantPostgres = checkDown
			}
			probes.Store(0)
			for _, path := range []string{"/api/v1/health", "/api/v1/health/ready"} {
				s.health.probe = probe
				code, body := getHealth(t, h, path)
				if code != tt.wantCode || body.Status != tt.wantStatus || body.Instance != "backend-1" {
					t.Fatalf("%s = %d %+v, want %d %s", path, code, body, tt.wantCode, tt.wantStatus)
				}
				if body.Checks["pool"] != wantPool || body.Checks["postgres"] != wantPostgres {
					t.Fatalf("%s checks = %v, want pool %s postgres %s", path, body.Checks, wantPool, wantPostgres)
				}
				if body.WSClients != nil {
					t.Fatalf("%s exposes wsClients", path)
				}
			}
			// Both calls within the TTL share one probe.
			if n := probes.Load(); n != 1 {
				t.Fatalf("probes = %d, want 1", n)
			}
			code, body := getHealth(t, h, "/api/v1/health/live")
			if code != http.StatusOK || body.Status != "ok" {
				t.Fatalf("live = %d %+v, want 200 ok whatever the dependencies", code, body)
			}
			if n := probes.Load(); n != 1 {
				t.Fatalf("liveness probed dependencies")
			}
		})
	}
}

func TestHealthCacheExpires(t *testing.T) {
	var probes int
	h := newHealthCache(time.Second, nil, func(context.Context) (error, error) { probes++; return nil, nil })
	now := time.Now()
	h.now = func() time.Time { return now }
	h.get(context.Background())
	h.get(context.Background())
	now = now.Add(time.Second)
	h.get(context.Background())
	if probes != 2 {
		t.Fatalf("probes = %d, want 2", probes)
	}
}

// scheduler_dependency_up reports each dependency from the shared probe, so a Redis outage that
// every caller rides out by failing open is still visible to alerting.
func TestDependencyUpMetric(t *testing.T) {
	probe := func(context.Context) (error, error) { return nil, errors.New("redis down") }
	c := newDependencyCollector(newHealthCache(time.Second, nil, probe), prometheus.Labels{"instance_id": "backend-1"})
	want := `
# HELP scheduler_dependency_up Whether this instance's last readiness probe reached the dependency (1) or not (0).
# TYPE scheduler_dependency_up gauge
scheduler_dependency_up{dep="postgres",instance_id="backend-1"} 1
scheduler_dependency_up{dep="redis",instance_id="backend-1"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

// Without a store there is no pool to report.
func TestReadinessWithoutStoreOmitsPool(t *testing.T) {
	s := New(nil, nil, auth.New("health-test-secret-0123456789abcdef"), realtime.NewHub(nil), nil, "backend-1")
	s.health.probe = func(context.Context) (error, error) { return nil, nil }
	if _, body := getHealth(t, s.Handler(), "/api/v1/health/ready"); body.Status != "ok" || len(body.Checks) != 2 {
		t.Fatalf("ready = %+v, want ok with postgres and redis only", body)
	}
}
