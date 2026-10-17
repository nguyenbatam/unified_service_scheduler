package integration_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
)

// Redis is optional (every use fails open), so a Redis that accepts connections but never answers
// must not make readiness slow: the probe reports it down within its own short budget while
// Postgres is probed alongside, instead of every readiness caller waiting out the whole probe.
func TestReadinessStaysFastWhenRedisHangs(t *testing.T) {
	t.Parallel()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { // accept and hold connections, never reply
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	client, err := cache.New("redis://" + ln.Addr().String() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	st, _, _ := openTestStore(t)
	api := newAPI(st, booking.New(st, cache.NewIdempotency(client)))

	t0 := time.Now()
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil))
	took := time.Since(t0)
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || body.Status != "degraded" || body.Checks["redis"] != "down" || body.Checks["postgres"] != "ok" {
		t.Fatalf("health = %d %+v, want 200 degraded (redis down, postgres ok)", rec.Code, body)
	}
	if took > time.Second {
		t.Fatalf("readiness took %s with a hanging Redis, want about the 250 ms Redis budget", took)
	}
}
