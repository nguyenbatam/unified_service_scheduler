package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

type outcome int

const (
	outCreated     outcome = iota // 201
	outReplayed                   // 200: an earlier attempt of this key committed (its answer was lost)
	outConflict                   // 409
	outUnavailable                // 422
	outUnresolved                 // still 429/502/503/504/transport after every attempt
	outError                      // anything else (500, bad body): a server bug
)

// String is the outcome's label in the end-to-end latency report.
func (o outcome) String() string {
	switch o {
	case outCreated:
		return "created"
	case outReplayed:
		return "replayed"
	case outConflict:
		return "conflict"
	case outUnavailable:
		return "unavail"
	case outUnresolved:
		return "unresolved"
	default:
		return "error"
	}
}

type bookResult struct {
	outcome outcome
	id      uuid.UUID
	// cut: some attempt lost its answer on the way (transport error, or 502/504 from the gateway):
	// the request may have reached a backend that died under it (-chaos-restart replays these).
	cut bool
}

const (
	retryBase = 100 * time.Millisecond
	retryCap  = 3 * time.Second
)

// bookWithRetry sends one booking and, while the answer is not definite (transport error, timeout,
// 429, 502, 503, 504: the booking may or may not have committed), resends it with the SAME
// Idempotency-Key — the server replays a committed booking as 200 instead of booking twice. Waits
// honour Retry-After (capped), else exponential backoff with jitter.
//
// Each send is recorded as one attempt; the booking as a whole is recorded once, end to end, from
// its first send to its definite answer (or giving up), so retries and backoff are visible.
func bookWithRetry(ctx context.Context, api *apiClient, payload map[string]any, key string, maxAttempts int, stats *runStats) bookResult {
	return bookWithRetryFrom(ctx, api, payload, key, maxAttempts, stats, time.Time{})
}

// bookWithRetryFrom is bookWithRetry with the end-to-end clock started at origin: the intended send
// time in open-loop (-rate) mode, so time spent queued behind a busy worker pool counts too
// (coordinated omission). A zero origin starts the clock at the first send.
func bookWithRetryFrom(ctx context.Context, api *apiClient, payload map[string]any, key string, maxAttempts int, stats *runStats, origin time.Time) bookResult {
	if origin.IsZero() {
		origin = time.Now()
	}
	r := sendWithRetry(ctx, api, payload, key, maxAttempts, stats)
	stats.observeBooking(r.outcome, time.Since(origin))
	return r
}

func sendWithRetry(ctx context.Context, api *apiClient, payload map[string]any, key string, maxAttempts int, stats *runStats) (result bookResult) {
	backoff := retryBase
	cut := false
	defer func() { result.cut = cut }()
	for attempt := 1; ; attempt++ {
		t0 := time.Now()
		resp, err := api.do(ctx, http.MethodPost, "/api/v1/appointments", payload, map[string]string{"Idempotency-Key": key})
		if (err != nil && !errors.Is(err, errClientAuth)) || resp.code == http.StatusBadGateway || resp.code == http.StatusGatewayTimeout {
			cut = true
		}
		label := statusLabel(resp.code, err)
		stats.observe(label, time.Since(t0))
		if err == nil {
			switch resp.code {
			case http.StatusCreated, http.StatusOK:
				var appt domain.Appointment
				if jerr := json.Unmarshal(resp.body, &appt); jerr != nil || appt.ID == uuid.Nil {
					stats.fail(fmt.Sprintf("%d bad body: %s", resp.code, truncate(string(resp.body), 120)))
					return bookResult{outcome: outError}
				}
				stats.answered(resp.header.Get("X-Instance-ID"), resp.code)
				if resp.code == http.StatusOK {
					stats.replayed.Add(1)
					return bookResult{outcome: outReplayed, id: appt.ID}
				}
				stats.created.Add(1)
				return bookResult{outcome: outCreated, id: appt.ID}
			case http.StatusConflict:
				stats.answered(resp.header.Get("X-Instance-ID"), resp.code)
				stats.conflicts.Add(1)
				return bookResult{outcome: outConflict}
			case http.StatusUnprocessableEntity:
				stats.answered(resp.header.Get("X-Instance-ID"), resp.code)
				stats.unavailable.Add(1)
				return bookResult{outcome: outUnavailable}
			case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				// not definite: retry below
			default: // a 500 (or any other status) is a server bug, never "capacity"
				stats.fail(fmt.Sprintf("%d:%s", resp.code, truncate(string(resp.body), 120)))
				return bookResult{outcome: outError}
			}
		}
		if errors.Is(err, errClientAuth) {
			// The client lost its session (the run is aborting): not an answer about the booking,
			// so neither a refusal nor a failure of it; unresolved keeps it out of every verdict.
			stats.unresolved.Add(1)
			stats.note("client auth: " + err.Error())
			return bookResult{outcome: outUnresolved}
		}
		if attempt >= maxAttempts || ctx.Err() != nil {
			stats.unresolved.Add(1)
			if err != nil {
				stats.note("transport: " + err.Error())
			}
			return bookResult{outcome: outUnresolved}
		}
		stats.retries.Add(1)
		wait := retryAfter(resp.header)
		if wait <= 0 {
			wait = backoff/2 + rand.N(backoff/2+1) //nolint:gosec // jitter, not security
			backoff = min(backoff*2, retryCap)
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(wait, retryCap)):
		}
	}
}

func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	secs, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

func statusLabel(code int, err error) string {
	if errors.Is(err, errClientAuth) {
		return "client-auth"
	}
	if err != nil {
		return "transport"
	}
	return strconv.Itoa(code)
}

// runStats counts outcomes across phases and keeps two latency views: every attempt (one HTTP
// send) per status label, and every booking end to end (first send, or intended send time in
// open-loop mode, to its definite answer, retries and backoff included) per final outcome.
type runStats struct {
	created, replayed, conflicts, unavailable atomic.Int64
	unresolved, retries                       atomic.Int64

	mu        sync.Mutex
	lat       map[string][]float64 // attempt: status label → latencies in ms (float, not truncated)
	e2e       map[string][]float64 // booking: outcome label → end-to-end latencies in ms
	errs      []string
	notes     []string
	attempt   int
	bookings  int
	instances map[string]instanceTally // X-Instance-ID ("" = header missing) → its definite answers
}

// instanceTally is one backend's share of the definite booking answers: all of them
// (201/200/409/422), and the 201s among them.
type instanceTally struct{ answers, created int }

func newRunStats() *runStats {
	return &runStats{lat: map[string][]float64{}, e2e: map[string][]float64{}, instances: map[string]instanceTally{}}
}

// answered counts a definite booking answer (201/200/409/422) by the backend instance that sent
// it. Refusals count too: each one ran the booking TX on that backend, so a run with few winners
// still shows every backend at work.
func (s *runStats) answered(instance string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := strings.TrimSpace(instance)
	t := s.instances[name]
	t.answers++
	if code == http.StatusCreated {
		t.created++
	}
	s.instances[name] = t
}

// answeredBy returns a copy of the per-instance tallies.
func (s *runStats) answeredBy() map[string]instanceTally {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.instances)
}

// instanceShortfall fails a run through the gateway whose definite booking answers came from
// fewer than minInstances distinct backends (X-Instance-ID): the other backend took no writes, so
// the run proved nothing about multi-instance booking. Answers without the header name no
// instance.
func instanceShortfall(answered map[string]instanceTally, minInstances int) error {
	var names []string
	for name, t := range answered {
		if name != "" && t.answers > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) >= minInstances {
		return nil
	}
	return fmt.Errorf("definite booking answers (201/200/409/422) came from %d backend instance(s) %v, want >= %d (-min-instances; 0 disables): is every backend behind the gateway up?",
		len(names), names, minInstances)
}

// instanceSummary is "a=120 (created 3) b=118 (created 2)" for the report.
func instanceSummary(answered map[string]instanceTally) string {
	names := slices.Sorted(maps.Keys(answered))
	parts := make([]string, len(names))
	for i, name := range names {
		label := name
		if label == "" {
			label = "(no X-Instance-ID)"
		}
		parts[i] = fmt.Sprintf("%s=%d (created %d)", label, answered[name].answers, answered[name].created)
	}
	return strings.Join(parts, " ")
}

func (s *runStats) observeBooking(o outcome, d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	s.mu.Lock()
	s.e2e[o.String()] = append(s.e2e[o.String()], ms)
	s.bookings++
	s.mu.Unlock()
}

func (s *runStats) observe(label string, d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	s.mu.Lock()
	s.lat[label] = append(s.lat[label], ms)
	s.attempt++
	s.mu.Unlock()
}

func (s *runStats) fail(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) < 5 {
		s.errs = append(s.errs, msg)
	}
}

func (s *runStats) note(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notes) < 5 {
		s.notes = append(s.notes, msg)
	}
}

// bookingLatency summarizes every hammered booking end to end, whatever its outcome.
func (s *runStats) bookingLatency() latencySummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []float64
	for _, v := range s.e2e {
		all = append(all, v...)
	}
	return summarize(all)
}

// attemptCount is how many booking sends the hammer made.
func (s *runStats) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempt
}

func (s *runStats) errorsSample() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.errs...)
}

// percentile is the nearest-rank percentile of sorted: the smallest value with at least p of the
// samples at or below it (index ceil(n·p) − 1).
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(float64(len(sorted))*p)) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

// latencySummary is one latency distribution (ms), nearest-rank percentiles.
type latencySummary struct {
	n                       int
	p50, p95, p99, maxValue float64
}

// summarize sorts samples in place and summarizes them.
func summarize(samples []float64) latencySummary {
	sort.Float64s(samples)
	return latencySummary{
		n: len(samples), p50: percentile(samples, 0.50), p95: percentile(samples, 0.95),
		p99: percentile(samples, 0.99), maxValue: percentile(samples, 1),
	}
}

func (l latencySummary) String() string {
	return fmt.Sprintf("n=%d p50=%.1f p95=%.1f p99=%.1f max=%.1f", l.n, l.p50, l.p95, l.p99, l.maxValue)
}

// perSecond is count/elapsed (0 for a zero elapsed).
func perSecond(count int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(count) / elapsed.Seconds()
}

// latencyBlock prints the all-labels distribution, then one line per label in label order.
func latencyBlock(kind string, byLabel map[string][]float64) {
	var all []float64
	labels := make([]string, 0, len(byLabel))
	for label, v := range byLabel {
		all = append(all, v...)
		labels = append(labels, label)
	}
	sort.Strings(labels)
	fmt.Printf("latency %s %-10s (ms): %s\n", kind, "all", summarize(all))
	for _, label := range labels {
		fmt.Printf("latency %s %-10s (ms): %s\n", kind, label, summarize(byLabel[label]))
	}
}

// report prints the hammer's counts and latency (elapsed: time spent hammering, preparation
// excluded). It prints nothing when no booking was sent.
func (s *runStats) report(elapsed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt == 0 {
		return
	}
	fmt.Printf("hammer: created=%d replayed_200=%d conflict_409=%d unavailable_422=%d unresolved=%d retries=%d bookings=%d attempts=%d wall=%.2fs bookings/s≈%.0f attempts/s≈%.0f\n",
		s.created.Load(), s.replayed.Load(), s.conflicts.Load(), s.unavailable.Load(), s.unresolved.Load(),
		s.retries.Load(), s.bookings, s.attempt, elapsed.Seconds(), perSecond(s.bookings, elapsed), perSecond(s.attempt, elapsed))
	// End to end is what a client waits for one booking (retries, backoff, Retry-After included);
	// per attempt is one HTTP round trip, by status.
	latencyBlock("booking", s.e2e)
	latencyBlock("attempt", s.lat)
	fmt.Printf("definite booking answers (201/200/409/422) per instance: %s\n", instanceSummary(s.instances))
	if s.unresolved.Load() > 0 || s.retries.Load() > 0 {
		fmt.Printf("note: saturation answered with 429/502/503/504/timeouts was retried with the same key (retries=%d, unresolved=%d) %v\n",
			s.retries.Load(), s.unresolved.Load(), s.notes)
	}
}
