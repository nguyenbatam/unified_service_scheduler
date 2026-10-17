package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func failRecorder(t *testing.T, err error, presetRetryAfter string) (*httptest.ResponseRecorder, errorBody) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	s := &Server{log: slog.New(slog.DiscardHandler)}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	if presetRetryAfter != "" {
		c.Header("Retry-After", presetRetryAfter)
	}
	s.fail(c, err)
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return rec, body
}

func TestFailMapsEveryCode(t *testing.T) {
	lockTimeout := fmt.Errorf("lock dealership: %w", &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"})
	stmtTimeout := &pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}
	tests := []struct {
		name           string
		err            error
		presetRetry    string
		wantStatus     int
		wantCode       string
		wantMessage    string
		wantRetryAfter string
	}{
		{name: "invalid", err: domain.NewError(domain.CodeInvalid, "bad id"), wantStatus: 400, wantCode: domain.CodeInvalid, wantMessage: "bad id"},
		{name: "unauthorized", err: domain.NewError(domain.CodeUnauthorized, "log in"), wantStatus: 401, wantCode: domain.CodeUnauthorized, wantMessage: "log in"},
		{name: "forbidden", err: domain.NewError(domain.CodeForbidden, "staff only"), wantStatus: 403, wantCode: domain.CodeForbidden, wantMessage: "staff only"},
		{name: "not found", err: domain.NewError(domain.CodeNotFound, "no such appointment"), wantStatus: 404, wantCode: domain.CodeNotFound, wantMessage: "no such appointment"},
		{name: "conflict", err: domain.NewError(domain.CodeConflict, "slot taken"), wantStatus: 409, wantCode: domain.CodeConflict, wantMessage: "slot taken"},
		{name: "wrapped conflict", err: fmt.Errorf("book: %w", domain.NewError(domain.CodeConflict, "slot taken")), wantStatus: 409, wantCode: domain.CodeConflict, wantMessage: "slot taken"},
		{name: "precondition", err: domain.NewError(domain.CodePrecondition, "stale version"), wantStatus: 412, wantCode: domain.CodePrecondition, wantMessage: "stale version"},
		{name: "unavailable", err: domain.NewError(domain.CodeUnavailable, "lunch break"), wantStatus: 422, wantCode: domain.CodeUnavailable, wantMessage: "lunch break"},
		{name: "rate limited keeps the limiter's Retry-After", err: domain.NewError(domain.CodeRateLimited, "slow down"), presetRetry: "300",
			wantStatus: 429, wantCode: domain.CodeRateLimited, wantMessage: "slow down", wantRetryAfter: "300"},
		{name: "rate limited defaults Retry-After", err: domain.NewError(domain.CodeRateLimited, "slow down"),
			wantStatus: 429, wantCode: domain.CodeRateLimited, wantMessage: "slow down", wantRetryAfter: "60"},
		{name: "busy", err: domain.NewError(domain.CodeBusy, "retry"), wantStatus: 503, wantCode: domain.CodeBusy, wantMessage: "retry", wantRetryAfter: "1"},
		{name: "pg lock timeout", err: lockTimeout, wantStatus: 503, wantCode: domain.CodeBusy, wantMessage: "the dealership is busy; retry in a moment", wantRetryAfter: "1"},
		{name: "pg statement timeout", err: stmtTimeout, wantStatus: 503, wantCode: domain.CodeBusy, wantMessage: "the dealership is busy; retry in a moment", wantRetryAfter: "1"},
		{name: "other pg error", err: &pgconn.PgError{Code: "42P01", Message: `relation "secret_table" does not exist`}, wantStatus: 500, wantCode: domain.CodeInternal, wantMessage: "internal error"},
		{name: "internal domain", err: domain.NewError(domain.CodeInternal, "pq: secret"), wantStatus: 500, wantCode: domain.CodeInternal, wantMessage: "internal error"},
		{name: "unmapped domain code", err: domain.NewError("weird", "stack trace"), wantStatus: 500, wantCode: domain.CodeInternal, wantMessage: "internal error"},
		{name: "unknown", err: errors.New("dial tcp 10.0.0.5:5432: refused"), wantStatus: 500, wantCode: domain.CodeInternal, wantMessage: "internal error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := failRecorder(t, tt.err, tt.presetRetry)
			if rec.Code != tt.wantStatus || body.Error.Code != tt.wantCode || body.Error.Message != tt.wantMessage {
				t.Fatalf("got %d %q %q, want %d %q %q", rec.Code, body.Error.Code, body.Error.Message, tt.wantStatus, tt.wantCode, tt.wantMessage)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.wantRetryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tt.wantRetryAfter)
			}
			if tt.wantStatus == 500 && (strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "10.0.0.5")) {
				t.Fatalf("500 leaks detail: %s", rec.Body.String())
			}
		})
	}
}

func TestBindJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type payload struct {
		Name string `json:"name"`
	}
	big := `{"name":"` + strings.Repeat("a", maxBodyBytes) + `"}`
	tests := []struct {
		name       string
		body       string
		wantOK     bool
		wantStatus int
		wantCode   string
	}{
		{name: "valid", body: `{"name":"x"}`, wantOK: true},
		{name: "trailing whitespace", body: "{\"name\":\"x\"}\n  \t", wantOK: true},
		{name: "unknown field", body: `{"name":"x","extra":1}`, wantStatus: 400, wantCode: domain.CodeInvalid},
		{name: "second value", body: `{"name":"x"}{"name":"y"}`, wantStatus: 400, wantCode: domain.CodeInvalid},
		{name: "trailing garbage", body: `{"name":"x"} trailing`, wantStatus: 400, wantCode: domain.CodeInvalid},
		{name: "trailing number", body: `{"name":"x"} 1`, wantStatus: 400, wantCode: domain.CodeInvalid},
		{name: "malformed", body: `{"name":`, wantStatus: 400, wantCode: domain.CodeInvalid},
		{name: "empty", body: ``, wantStatus: 400, wantCode: domain.CodeInvalid},
		{name: "over 1 MiB", body: big, wantStatus: 413, wantCode: codeTooLarge},
		{name: "over 1 MiB in trailing data", body: `{"name":"x"} "` + strings.Repeat("a", maxBodyBytes) + `"`, wantStatus: 413, wantCode: codeTooLarge},
	}
	s := &Server{log: slog.New(slog.DiscardHandler)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/x", bytes.NewBufferString(tt.body))
			var dst payload
			ok := s.bindJSON(c, &dst, "body must be a payload")
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (%d %s)", ok, tt.wantOK, rec.Code, rec.Body.String())
			}
			if ok {
				if dst.Name != "x" {
					t.Fatalf("decoded %+v", dst)
				}
				return
			}
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if rec.Code != tt.wantStatus || body.Error.Code != tt.wantCode || !c.IsAborted() {
				t.Fatalf("got %d %q (aborted %v), want %d %q", rec.Code, body.Error.Code, c.IsAborted(), tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestPathUUID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{log: slog.New(slog.DiscardHandler)}
	for _, raw := range []string{"not-a-uuid", "", "123", "6ba7b810-9dad-11d1-80b4-00c04fd430c"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
		c.Params = gin.Params{{Key: "id", Value: raw}}
		if _, ok := s.pathUUID(c, "id", "appointment id is invalid"); ok {
			t.Fatalf("%q accepted", raw)
		}
		var body errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 400 || body.Error.Code != domain.CodeInvalid || body.Error.Message != "appointment id is invalid" {
			t.Fatalf("%q: %d %+v", raw, rec.Code, body)
		}
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
	c.Params = gin.Params{{Key: "id", Value: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}
	if id, ok := s.pathUUID(c, "id", "bad"); !ok || id.String() != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
		t.Fatalf("valid id rejected: %v %v", id, ok)
	}
}
