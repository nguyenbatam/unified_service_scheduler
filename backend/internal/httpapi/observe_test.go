package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestServer(t *testing.T, opts ...Option) (*Server, *gin.Engine) {
	t.Helper()
	s := New(nil, nil, auth.New("observe-test-secret-0123456789abcdef"), realtime.NewHub(nil),
		slog.New(slog.DiscardHandler), "test", opts...)
	return s, s.engine()
}

func TestMetricMethod(t *testing.T) {
	tests := []struct{ in, want string }{
		{http.MethodGet, "GET"}, {http.MethodPost, "POST"}, {http.MethodPut, "PUT"},
		{http.MethodPatch, "PATCH"}, {http.MethodDelete, "DELETE"}, {http.MethodOptions, "OPTIONS"},
		{http.MethodHead, "HEAD"}, {"get", "OTHER"}, {"PROPFIND", "OTHER"}, {"X-RANDOM-1234", "OTHER"}, {"", "OTHER"},
	}
	for _, tt := range tests {
		if got := metricMethod(tt.in); got != tt.want {
			t.Errorf("metricMethod(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Arbitrary method tokens land on the 404 chain under one bounded label.
func TestUnknownMethodsShareOneSeries(t *testing.T) {
	s, h := newTestServer(t)
	for _, m := range []string{"FOO", "BAR", "BAZ"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), m, "/api/v1/nowhere", nil))
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("OTHER", "unmatched", "404")); got != 3 {
		t.Fatalf("OTHER/unmatched/404 = %v, want 3", got)
	}
	if n := testutil.CollectAndCount(s.requests); n != 1 {
		t.Fatalf("%d request series, want 1", n)
	}
}

func TestPanicIsJSON500AndCounted(t *testing.T) {
	s, h := newTestServer(t)
	h.GET("/boom", func(*gin.Context) { panic("kaboom: secret detail") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	if body.Error.Code != domain.CodeInternal || body.Error.Message != "internal error" {
		t.Fatalf("body %+v, want internal / internal error", body)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Fatal("no X-Request-ID on the panic response")
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("GET", "/boom", "500")); got != 1 {
		t.Fatalf("GET /boom 500 counted %v times, want 1", got)
	}
	if got := testutil.CollectAndCount(s.latency); got != 1 {
		t.Fatalf("latency series = %d, want 1", got)
	}
}

// A panic after the handler wrote its response cannot become a JSON 500: the connection is
// dropped (http.ErrAbortHandler, which net/http swallows) and the request is counted as a 500.
func TestPanicAfterWriteAbortsAndCounts500(t *testing.T) {
	s, h := newTestServer(t)
	h.GET("/late", func(c *gin.Context) {
		c.String(http.StatusAccepted, "done")
		panic("after write")
	})
	rec := httptest.NewRecorder()
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler { //nolint:errorlint // recover() value, not a wrapped error
				t.Fatalf("recovered %v, want http.ErrAbortHandler", got)
			}
		}()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/late", nil))
	}()
	if got := testutil.ToFloat64(s.requests.WithLabelValues("GET", "/late", "500")); got != 1 {
		t.Fatalf("GET /late 500 counted %v times, want 1", got)
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("GET", "/late", "202")); got != 0 {
		t.Fatalf("GET /late 202 counted %v times, want 0", got)
	}
}

func TestNewRequiresAuthService(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New without an auth service did not panic")
		}
	}()
	New(nil, nil, nil, nil, nil, "test")
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Probes and scrapes are not logged when they succeed, successful reads go to Debug, and writes
// and every failure stay at Info.
func TestAccessLogLevel(t *testing.T) {
	tests := []struct {
		method, route string
		status        int
		want          slog.Level
		logged        bool
	}{
		{http.MethodGet, "/metrics", 200, 0, false},
		{http.MethodGet, "/api/v1/health/ready", 200, 0, false},
		{http.MethodGet, "/api/v1/health", 503, slog.LevelInfo, true},
		{http.MethodGet, "/api/v1/appointments", 200, slog.LevelDebug, true},
		{http.MethodGet, "/api/v1/appointments", 304, slog.LevelDebug, true},
		{http.MethodGet, "/api/v1/appointments", 404, slog.LevelInfo, true},
		{http.MethodPost, "/api/v1/appointments", 201, slog.LevelInfo, true},
		{http.MethodPost, "/api/v1/appointments", 499, slog.LevelInfo, true},
	}
	for _, tt := range tests {
		got, logged := accessLogLevel(tt.method, tt.route, tt.status)
		if logged != tt.logged || (logged && got != tt.want) {
			t.Errorf("%s %s %d: level %v logged %v, want %v %v", tt.method, tt.route, tt.status, got, logged, tt.want, tt.logged)
		}
	}
}
