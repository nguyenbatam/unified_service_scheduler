package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestPercentileNearestRank(t *testing.T) {
	ten := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"empty", nil, 0.5, 0},
		{"single", []float64{7}, 0.99, 7},
		{"p50 of ten", ten, 0.50, 5},
		{"p95 of ten", ten, 0.95, 10},
		{"p90 of ten", ten, 0.90, 9},
		{"p0 clamps to min", ten, 0, 1},
		{"p100 is max", ten, 1, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.sorted, tc.p); got != tc.want {
				t.Fatalf("percentile(%v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

// A 503 (or a lost answer) is not definite: the client resends with the same key, and a replay
// (200) of the booking that did commit counts as created.
func TestBookWithRetryResendsSameKeyUntilDefinite(t *testing.T) {
	id := uuid.New()
	var mu sync.Mutex
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		n := len(keys)
		mu.Unlock()
		switch n {
		case 1:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
		}
	}))
	defer srv.Close()

	stats := newRunStats()
	r := bookWithRetry(t.Context(), newAPI(srv.URL), map[string]any{}, "k-1", 5, stats)
	if r.outcome != outReplayed || r.id != id {
		t.Fatalf("outcome %v id %s, want replayed %s", r.outcome, r.id, id)
	}
	if stats.retries.Load() != 2 || stats.replayed.Load() != 1 || len(keys) != 3 {
		t.Fatalf("retries=%d replayed=%d sends=%d", stats.retries.Load(), stats.replayed.Load(), len(keys))
	}
	for _, k := range keys {
		if k != "k-1" {
			t.Fatalf("resent with key %q, want the original k-1", k)
		}
	}
}

func TestBookWithRetryGivesUpAsUnresolvedAndFlags500(t *testing.T) {
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer busy.Close()
	stats := newRunStats()
	if r := bookWithRetry(t.Context(), newAPI(busy.URL), map[string]any{}, "k", 2, stats); r.outcome != outUnresolved {
		t.Fatalf("outcome %v, want unresolved", r.outcome)
	}
	if stats.unresolved.Load() != 1 || len(stats.errorsSample()) != 0 {
		t.Fatalf("unresolved=%d errors=%v", stats.unresolved.Load(), stats.errorsSample())
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()
	if r := bookWithRetry(t.Context(), newAPI(broken.URL), map[string]any{}, "k", 5, stats); r.outcome != outError {
		t.Fatalf("outcome %v, want error (a 500 is a bug, never retried)", r.outcome)
	}
	if len(stats.errorsSample()) != 1 {
		t.Fatalf("errors=%v", stats.errorsSample())
	}
}

func TestPhaseOutcome(t *testing.T) {
	cases := []struct {
		name                          string
		created, unresolved, requests int
		maxPct                        float64
		wantErr                       bool
	}{
		{"nothing created, all unresolved", 0, 100, 100, 10, true},
		{"nothing created, all refused", 0, 0, 100, 10, true},
		{"created, nothing unresolved", 5, 0, 100, 10, false},
		{"unresolved exactly at the limit", 5, 10, 100, 10, false},
		{"unresolved just over the limit", 5, 11, 100, 10, true},
		{"limit 0 tolerates none", 5, 1, 100, 0, true},
		{"limit 100 tolerates all but one", 1, 99, 100, 100, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := phaseOutcome(tc.created, tc.unresolved, tc.requests, tc.maxPct)
			if (err != nil) != tc.wantErr {
				t.Fatalf("phaseOutcome(%d, %d, %d, %v) = %v, wantErr %v", tc.created, tc.unresolved, tc.requests, tc.maxPct, err, tc.wantErr)
			}
		})
	}
}

// End to end covers the whole booking: the backoff between attempts counts, the per-attempt
// samples do not include it, and each booking is counted once however many sends it took.
func TestBookingLatencyIncludesRetriesAndBackoff(t *testing.T) {
	id := uuid.New()
	var mu sync.Mutex
	sends := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		sends++
		n := sends
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway) // no Retry-After: backoff 50–100 ms
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	}))
	defer srv.Close()

	stats := newRunStats()
	if r := bookWithRetry(t.Context(), newAPI(srv.URL), map[string]any{}, "k", 3, stats); r.outcome != outCreated {
		t.Fatalf("outcome %v, want created", r.outcome)
	}
	if stats.bookings != 1 || stats.attempt != 2 {
		t.Fatalf("bookings=%d attempts=%d, want 1 booking over 2 attempts", stats.bookings, stats.attempt)
	}
	e2e := stats.e2e["created"]
	if len(e2e) != 1 {
		t.Fatalf("e2e samples %v, want one under created", stats.e2e)
	}
	attempts := 0.0
	for _, v := range stats.lat {
		for _, ms := range v {
			attempts += ms
		}
	}
	if e2e[0] < attempts+float64(retryBase/2/time.Millisecond) {
		t.Fatalf("e2e %.1f ms, want >= attempts %.1f ms + the %v minimum backoff", e2e[0], attempts, retryBase/2)
	}
}

// Open loop: the clock starts at the intended send time, so a request that left late (queued
// behind a busy pool) reports the delay.
func TestBookingLatencyFromIntendedSendTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	stats := newRunStats()
	origin := time.Now().Add(-500 * time.Millisecond)
	if r := bookWithRetryFrom(t.Context(), newAPI(srv.URL), map[string]any{}, "k", 3, stats, origin); r.outcome != outConflict {
		t.Fatalf("outcome %v, want conflict", r.outcome)
	}
	if got := stats.e2e["conflict"]; len(got) != 1 || got[0] < 500 {
		t.Fatalf("e2e conflict samples %v, want one >= 500 ms", got)
	}
}

func TestIntendedStartAndRates(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if got := intendedStart(t0, 0, 50); !got.Equal(t0) {
		t.Fatalf("request 0 due %v, want t0", got)
	}
	if got := intendedStart(t0, 25, 50); !got.Equal(t0.Add(500 * time.Millisecond)) {
		t.Fatalf("request 25 at 50/s due %v, want t0+500ms", got)
	}
	if got := perSecond(300, 2*time.Second); got != 150 {
		t.Fatalf("perSecond = %v, want 150", got)
	}
	if got := perSecond(10, 0); got != 0 {
		t.Fatalf("perSecond with zero elapsed = %v, want 0", got)
	}
	s := summarize([]float64{30, 10, 20})
	if s.n != 3 || s.p50 != 20 || s.maxValue != 30 {
		t.Fatalf("summarize = %+v", s)
	}
}

func TestCancelAllTriesEveryIDAndJoinsErrors(t *testing.T) {
	var mu sync.Mutex
	cancels := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodPost {
			cancels++
		}
		mu.Unlock()
		w.WriteHeader(http.StatusConflict)
	}))
	defer srv.Close()
	ids := map[uuid.UUID]bool{uuid.New(): true, uuid.New(): true, uuid.New(): true}
	err := cancelAll(t.Context(), newAPI(srv.URL), ids)
	if err == nil || cancels != 3 {
		t.Fatalf("err=%v cancels=%d, want an error after trying all 3", err, cancels)
	}
	if !strings.Contains(err.Error(), "3 of 3 cancels failed") {
		t.Fatalf("err = %v", err)
	}
}

// A refused cancel is fine when the booking no longer holds anything (the mixed phase cancelled
// it, or it moved on): cleanup only fails for bookings it leaves holding.
func TestCancelAllAcceptsBookingsThatNoLongerHold(t *testing.T) {
	released, holding := uuid.New(), uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			return
		}
		status := domain.StatusCompleted
		if strings.Contains(r.URL.Path, holding.String()) {
			status = domain.StatusInProgress
		}
		_ = json.NewEncoder(w).Encode(domain.Appointment{ID: released, Status: status})
	}))
	defer srv.Close()
	err := cancelAll(t.Context(), newAPI(srv.URL), map[uuid.UUID]bool{released: true, holding: true})
	if err == nil || !strings.Contains(err.Error(), "1 of 2 cancels failed") || !strings.Contains(err.Error(), holding.String()) {
		t.Fatalf("err = %v, want only the still-holding booking reported", err)
	}
}
