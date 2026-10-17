package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

// The deadline middleware skips the socket, so the handshake bounds its own user read: a stuck
// pool answers 503 busy + Retry-After instead of holding the handshake open.
func TestWSHandshakeUserReadIsBounded(t *testing.T) {
	authSvc := auth.New("ws-deadline-test-secret-0123456789abcdef")
	s := New(nil, nil, authSvc, realtime.NewHub(nil), nil, "test", WithRequestTimeout(50*time.Millisecond))
	s.loadUser = func(ctx context.Context, _ uuid.UUID) (domain.User, error) {
		<-ctx.Done() // a pool that never hands out a connection
		return domain.User{}, ctx.Err()
	}
	staff := domain.User{ID: uuid.New(), Role: domain.RoleDispatcher}
	token, _, err := authSvc.Issue(staff)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/ws/dispatch", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	start := time.Now()
	s.Handler().ServeHTTP(rec, req)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("handshake took %s, want it bounded by the request timeout", took)
	}
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d Retry-After %q, want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
}
