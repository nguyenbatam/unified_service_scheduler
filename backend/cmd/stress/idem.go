package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The idempotency race proves one appointment per Idempotency-Key across instances: for each of K
// keys, racers send the same key and payload at once, half of them straight to backend A and half
// to backend B (through the gateway only when the direct backends are not configured), so the two
// instances race each other on Postgres (per-user key lock + unique index), not just goroutines of
// one process. Every racer records the X-Instance-ID that answered it; both instances must have
// answered, every key must end with exactly one appointment (the answers agree, and Postgres holds
// one row per key), and the same key with another payload must be 409.

// idemAnswer is one racer's final answer: a definite status (with the appointment id for 200/201),
// or unresolved (code 0) after every retry. retried marks a racer that resent at least once (its
// earlier attempt may have committed without the answer reaching it). instance is the
// X-Instance-ID of the definite answer.
type idemAnswer struct {
	code     int
	id       uuid.UUID
	retried  bool
	body     string
	instance string
}

// judgeIdempotentRace checks the answers of N concurrent sends of one key + payload: every
// definite answer is 201 or 200 for one and the same appointment, at most one is 201, and some
// answer is definite. A 201 may be missing only when a racer retried (the committing attempt's
// answer was lost and its retry replayed). 409 (the key colliding with itself), 422, or any 5xx is
// a bug.
func judgeIdempotentRace(answers []idemAnswer) (uuid.UUID, error) {
	var id uuid.UUID
	created, definite, retried := 0, 0, false
	for _, a := range answers {
		retried = retried || a.retried
		switch a.code {
		case 0:
			continue
		case http.StatusCreated, http.StatusOK:
			definite++
			if a.code == http.StatusCreated {
				created++
			}
			if id == uuid.Nil {
				id = a.id
			} else if a.id != id {
				return uuid.Nil, fmt.Errorf("one key booked two appointments: %s and %s", id, a.id)
			}
		default:
			return uuid.Nil, fmt.Errorf("racer answered %d: %s", a.code, a.body)
		}
	}
	switch {
	case definite == 0:
		return uuid.Nil, fmt.Errorf("no racer got a definite answer (%d sent)", len(answers))
	case created > 1:
		return uuid.Nil, fmt.Errorf("%d racers got 201 for one key", created)
	case created == 0 && !retried:
		return uuid.Nil, fmt.Errorf("no 201 among %d answers and no racer retried", definite)
	}
	return id, nil
}

// sendIdem sends one booking, resending on 429/502/503/504/transport with the same key.
func sendIdem(ctx context.Context, api *apiClient, payload map[string]any, key string, maxAttempts int) idemAnswer {
	var out idemAnswer
	for attempt := 1; ; attempt++ {
		resp, err := api.do(ctx, http.MethodPost, "/api/v1/appointments", payload, map[string]string{"Idempotency-Key": key})
		if err == nil {
			switch resp.code {
			case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			default:
				out.code, out.body = resp.code, truncate(string(resp.body), 160)
				out.instance = strings.TrimSpace(resp.header.Get("X-Instance-ID"))
				var appt domain.Appointment
				if (resp.code == http.StatusCreated || resp.code == http.StatusOK) && json.Unmarshal(resp.body, &appt) == nil {
					out.id = appt.ID
				}
				return out
			}
		}
		if attempt >= maxAttempts || ctx.Err() != nil {
			out.code = 0
			return out
		}
		out.retried = true
		wait := retryAfter(resp.header)
		if wait <= 0 {
			wait = retryBase * time.Duration(attempt)
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(wait, retryCap)):
		}
	}
}

// idemTarget is where the race books: a dealership, a job, and cars (one per key).
type idemTarget struct {
	dealer   uuid.UUID
	job      []uuid.UUID
	vehicles []uuid.UUID
}

// idemResult is what the race proved.
type idemResult struct {
	ids       []uuid.UUID
	keys      []string
	instances map[string]int // X-Instance-ID → definite answers
	crossed   bool           // racers were split across two direct backends
}

func (r idemResult) String() string {
	names := slices.Sorted(maps.Keys(r.instances))
	parts := make([]string, len(names))
	for i, n := range names {
		label := n
		if label == "" {
			label = "(no X-Instance-ID)"
		}
		parts[i] = fmt.Sprintf("%s=%d", label, r.instances[n])
	}
	return strings.Join(parts, " ")
}

// idempotencyRace runs the race over keys keys with racers racers each, alternating racers over
// apis (backend A and B, or just the gateway), then checks Postgres (q may be nil: no DSN) and the
// same key with another payload. It returns the booked ids even on failure (for cleanup).
func idempotencyRace(ctx context.Context, apis []*apiClient, q querier, tgt idemTarget, keys, racers int, runID string, s settings) (idemResult, error) {
	res := idemResult{instances: map[string]int{}, crossed: len(apis) > 1}
	if len(apis) == 0 || len(tgt.vehicles) == 0 || keys <= 0 {
		return res, fmt.Errorf("idempotency race: nothing to race with")
	}
	type keyRun struct {
		key     string
		payload map[string]any
		start   time.Time
		answers []idemAnswer
	}
	runs := make([]*keyRun, keys)
	for k := range runs {
		vehicle := tgt.vehicles[k%len(tgt.vehicles)]
		// Every key gets a start of its own (keys must race their own racers, not each other for
		// the last place at one start), and a car reused across keys a start three hours after
		// its previous one, clear of that window.
		cutoff := time.Now().Add(s.startMargin)
		if k > 0 {
			cutoff = runs[k-1].start.Add(30 * time.Minute)
		}
		if k >= len(tgt.vehicles) {
			cutoff = maxTime(cutoff, runs[k-len(tgt.vehicles)].start.Add(3*time.Hour))
		}
		start, err := freeStart(ctx, apis[0], tgt.dealer, vehicle, tgt.job, cutoff)
		if err != nil {
			return res, fmt.Errorf("idempotency race: %w", err)
		}
		runs[k] = &keyRun{
			key: fmt.Sprintf("%s:idem:%d", runID, k), payload: bookingPayload(tgt.dealer, vehicle, tgt.job, start),
			start: start, answers: make([]idemAnswer, racers),
		}
		res.keys = append(res.keys, runs[k].key)
	}
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for _, r := range runs {
		for i := range racers {
			api := apis[i%len(apis)]
			wg.Go(func() {
				<-gate
				r.answers[i] = sendIdem(ctx, api, r.payload, r.key, s.maxAttempts)
			})
		}
	}
	close(gate)
	wg.Wait()
	var errs []string
	for _, r := range runs {
		for _, a := range r.answers {
			if a.code != 0 {
				res.instances[a.instance]++
			}
		}
		id, err := judgeIdempotentRace(r.answers)
		if id != uuid.Nil {
			res.ids = append(res.ids, id)
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("key %s (%d racers, %s): %v", r.key, racers, r.start.Format(time.RFC3339), err))
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("idempotency race: %s", strings.Join(errs, "; "))
	}
	if err := crossInstance(res); err != nil {
		return res, err
	}
	if q != nil {
		if err := onePerKey(ctx, q, res.keys); err != nil {
			return res, err
		}
	}
	first := runs[0]
	other := bookingPayload(tgt.dealer, tgt.vehicles[0], tgt.job, first.start.Add(30*time.Minute))
	if a := sendIdem(ctx, apis[len(apis)-1], other, first.key, s.maxAttempts); a.code != http.StatusConflict {
		return res, fmt.Errorf("idempotency race: same key with another start answered %d, want 409: %s", a.code, a.body)
	}
	fmt.Printf("idempotency ok: %d keys × %d concurrent sends → one appointment per key; answers by instance %s; same key, other payload → 409\n",
		keys, racers, res)
	return res, nil
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// crossInstance fails a race split across two direct backends that only one instance answered.
func crossInstance(res idemResult) error {
	if !res.crossed {
		return nil
	}
	named := 0
	for name, n := range res.instances {
		if name != "" && n > 0 {
			named++
		}
	}
	if named < 2 {
		return fmt.Errorf("idempotency race: racers were split across backend A and B but definite answers came from %d named instance(s) (%s)", named, res)
	}
	return nil
}

// onePerKey checks Postgres holds exactly one appointment under each key.
func onePerKey(ctx context.Context, q querier, keys []string) error {
	rows, err := q.Query(ctx, `
		SELECT k, (SELECT count(*)::int FROM appointments a WHERE a.idempotency_key = k)
		FROM unnest($1::text[]) AS k`, keys)
	if err != nil {
		return fmt.Errorf("idempotency race: count rows: %w", err)
	}
	var bad []string
	if _, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var k string
		var n int
		err := row.Scan(&k, &n)
		if err == nil && n != 1 {
			bad = append(bad, fmt.Sprintf("%s: %d rows", k, n))
		}
		return struct{}{}, err
	}); err != nil {
		return fmt.Errorf("idempotency race: count rows: %w", err)
	}
	if len(bad) > 0 {
		return fmt.Errorf("idempotency race: want exactly one appointment per key in Postgres: %v", bad)
	}
	return nil
}
