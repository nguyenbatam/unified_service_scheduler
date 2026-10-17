package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// failOn runs fail for err on req.
func failOn(t *testing.T, req *http.Request, err error) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := &Server{log: slog.New(slog.DiscardHandler)}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	s.fail(c, err)
	return rec
}

func canceledContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func expiredContext(t *testing.T) context.Context {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	t.Cleanup(cancel)
	return ctx
}

// What happened to the request decides how a server-side failure is answered: a client that went
// away is 499 (not a 500), an expired request deadline is 503 busy; client errors are unchanged.
func TestFailClassifiesEndedRequests(t *testing.T) {
	queryCanceled := &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
	tests := []struct {
		name           string
		ctx            func(*testing.T) context.Context
		err            error
		wantStatus     int
		wantRetryAfter string
	}{
		{name: "client gone, context error", ctx: canceledContext, err: fmt.Errorf("list: %w", context.Canceled), wantStatus: statusClientClosed},
		{name: "client gone, query canceled", ctx: canceledContext, err: queryCanceled, wantStatus: statusClientClosed},
		{name: "client gone, internal domain error", ctx: canceledContext, err: domain.NewError(domain.CodeInternal, "x"), wantStatus: statusClientClosed},
		{name: "client gone, client error kept", ctx: canceledContext, err: domain.NewError(domain.CodeConflict, "slot taken"), wantStatus: http.StatusConflict},
		{name: "deadline, context error", ctx: expiredContext, err: fmt.Errorf("acquire: %w", context.DeadlineExceeded), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "1"},
		{name: "deadline, query canceled", ctx: expiredContext, err: queryCanceled, wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "1"},
		{name: "deadline, unknown error", ctx: expiredContext, err: errors.New("conn closed"), wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "1"},
		{name: "live, unknown error", ctx: func(t *testing.T) context.Context { return t.Context() }, err: errors.New("boom"), wantStatus: http.StatusInternalServerError},
		{name: "live, statement timeout", ctx: func(t *testing.T) context.Context { return t.Context() }, err: queryCanceled, wantStatus: http.StatusServiceUnavailable, wantRetryAfter: "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := failOn(t, httptest.NewRequestWithContext(tt.ctx(t), http.MethodGet, "/x", nil), tt.err)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tt.wantRetryAfter)
			}
			if tt.wantStatus == statusClientClosed && rec.Body.Len() != 0 {
				t.Fatalf("499 has a body: %s", rec.Body.String())
			}
		})
	}
}

func TestBookingResult(t *testing.T) {
	live := t.Context()
	lockTimeout := &pgconn.PgError{Code: "55P03"}
	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{name: "conflict", ctx: live, err: domain.NewError(domain.CodeConflict, "x"), want: domain.CodeConflict},
		{name: "conflict after disconnect", ctx: canceledContext(t), err: domain.NewError(domain.CodeConflict, "x"), want: domain.CodeConflict},
		{name: "lock timeout", ctx: live, err: lockTimeout, want: domain.CodeBusy},
		{name: "disconnect", ctx: canceledContext(t), err: context.Canceled, want: bookingResultCanceled},
		{name: "disconnect mid-statement", ctx: canceledContext(t), err: &pgconn.PgError{Code: "57014"}, want: bookingResultCanceled},
		{name: "request deadline", ctx: expiredContext(t), err: context.DeadlineExceeded, want: domain.CodeBusy},
		{name: "server error", ctx: live, err: errors.New("boom"), want: domain.CodeInternal},
		{name: "unmapped code", ctx: live, err: domain.NewError("weird", "x"), want: domain.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bookingResult(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("bookingResult = %q, want %q", got, tt.want)
			}
		})
	}
}

// The deadline middleware bounds API requests, skips the socket and /metrics, and an expired
// deadline is counted as a 503, never a 500.
func TestDeadlineMiddleware(t *testing.T) {
	s, engine := newTestServer(t, WithRequestTimeout(30*time.Millisecond))
	deadlines := map[string]bool{}
	engine.GET("/api/v1/test/deadline", func(c *gin.Context) {
		_, ok := c.Request.Context().Deadline()
		deadlines["api"] = ok
		<-c.Request.Context().Done() // a database wait that never ends on its own
		s.fail(c, fmt.Errorf("acquire: %w", c.Request.Context().Err()))
	})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/test/deadline", nil))
	if !deadlines["api"] || rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("deadline %v status %d Retry-After %q, want a deadline and 503 + Retry-After", deadlines["api"], rec.Code, rec.Header().Get("Retry-After"))
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("GET", "/api/v1/test/deadline", "503")); got != 1 {
		t.Fatalf("503 counted %v times, want 1", got)
	}

	for _, path := range []string{"/metrics", "/api/v1/ws/dispatch"} {
		gotDeadline := true
		h := func(c *gin.Context) {
			_, gotDeadline = c.Request.Context().Deadline()
			c.Status(http.StatusNoContent)
		}
		probe := gin.New()
		probe.Use(s.deadline())
		probe.GET(path, h)
		probe.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if gotDeadline {
			t.Errorf("%s got a request deadline", path)
		}
	}

	off, engineOff := newTestServer(t, WithRequestTimeout(0))
	engineOff.GET("/api/v1/test/none", func(c *gin.Context) {
		_, ok := c.Request.Context().Deadline()
		deadlines["off"] = ok
		c.Status(http.StatusNoContent)
	})
	engineOff.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/test/none", nil))
	if deadlines["off"] || off.requestTimeout != 0 {
		t.Fatal("WithRequestTimeout(0) still set a deadline")
	}
	if s2, _ := newTestServer(t); s2.requestTimeout != DefaultRequestTimeout {
		t.Fatalf("default request timeout %v, want %v", s2.requestTimeout, DefaultRequestTimeout)
	}
}

// A client that disconnects mid-request is recorded as 499, not 500, and its booking as canceled.
func TestClientDisconnectIsNotAServerError(t *testing.T) {
	s, engine := newTestServer(t)
	ctx, disconnect := context.WithCancel(t.Context())
	engine.GET("/api/v1/test/slow", func(c *gin.Context) {
		disconnect() // the client goes away while the handler waits on Postgres
		err := &pgconn.PgError{Code: "57014", Message: "canceling statement due to user request"}
		s.countBooking(c.Request.Context(), err)
		s.fail(c, err)
	})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/test/slow", nil))
	if rec.Code != statusClientClosed {
		t.Fatalf("status %d, want %d", rec.Code, statusClientClosed)
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("GET", "/api/v1/test/slow", "499")); got != 1 {
		t.Fatalf("499 counted %v times, want 1", got)
	}
	if got := testutil.ToFloat64(s.requests.WithLabelValues("GET", "/api/v1/test/slow", "500")); got != 0 {
		t.Fatalf("500 counted %v times, want 0", got)
	}
	if got := testutil.ToFloat64(s.bookings.WithLabelValues(bookingResultCanceled)); got != 1 {
		t.Fatalf("canceled bookings = %v, want 1", got)
	}
	if got := testutil.ToFloat64(s.bookings.WithLabelValues(domain.CodeInternal)); got != 0 {
		t.Fatalf("internal bookings = %v, want 0", got)
	}
}
