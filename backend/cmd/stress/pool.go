package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Pool exhaustion and the admission gate. A hammered dealership-day must never drain a backend's
// connection pool: the per-instance day gate (booking.dayGate) lets at most a few bookings of one
// day into the database at once and parks the rest in memory, so the dispatcher's reads keep
// getting connections. Around every hammer phase the run scrapes each backend's
// scheduler_db_pool_* series and samples them during the phase, then asserts:
//
//   - the cluster's empty-acquire wait (time requests spent waiting for a connection because none
//     was idle) grew by at most -pool-wait-budget-ms (default 2 s) over the phase. A pool that
//     merely grows pays a few milliseconds per new connection; the defect this guards against —
//     losers holding the day lock while every request queued for a connection — cost tens of
//     seconds of waiting per phase;
//   - no sample saw a backend with every connection acquired (acquired < max): at the cap, the
//     next request waits regardless of what it is. The budget is arithmetic: a hammered day holds
//     at most dayGateWidth (4) connections per instance, the side probes (-side-workers, default
//     16, spread over two instances by the gateway) about 8 more, the outbox worker and hub
//     sweeps a couple — well under the default DB_MAX_CONNS of 20. A backend at its cap means
//     booking work got past the gate (or the side load was raised past what the pool holds).
//     -pool-cap-fails=false (POOL_CAP_FAILS, for a shared CI runner) reports the peak without
//     failing on it: there a starved CPU stretches the short queries outside the gate, and the
//     wait budget alone still catches the defect above.
//
// The dispatcher-read p99 during the hammer has its own budget (-slo-side-ms).

// poolSamples is what the sampler saw during one phase, per instance_id.
type poolSamples struct {
	maxAcquired map[string]float64
	maxConns    map[string]float64
	samples     int // ticks at which at least one backend answered
	attempts    int // scrapes tried (ticks × backends)
	scrapeErrs  int
	// per backend URL (a failed scrape has no instance_id): scrapes tried and failed
	backendTries, backendErrs map[string]int
}

// poolWatch samples the backends' pool gauges every 100 ms until stopped.
type poolWatch struct {
	stopCh chan struct{}
	done   chan poolSamples
}

func startPoolWatch(ctx context.Context, backends []string) *poolWatch {
	pw := &poolWatch{stopCh: make(chan struct{}), done: make(chan poolSamples, 1)}
	go func() {
		out := poolSamples{maxAcquired: map[string]float64{}, maxConns: map[string]float64{},
			backendTries: map[string]int{}, backendErrs: map[string]int{}}
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for first := true; ; first = false {
			if !first { // the first sample is taken at once: a short phase still gets one
				select {
				case <-pw.stopCh:
					pw.done <- out
					return
				case <-ctx.Done():
					pw.done <- out
					return
				case <-tick.C:
				}
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			answered := false
			for _, b := range backends {
				wg.Go(func() {
					s, err := scrape(ctx, b)
					mu.Lock()
					defer mu.Unlock()
					out.attempts++
					out.backendTries[b]++
					if err != nil {
						out.scrapeErrs++
						out.backendErrs[b]++
						return
					}
					answered = true
					out.observe(s)
				})
			}
			wg.Wait()
			if answered {
				out.samples++ // a tick where every scrape failed saw nothing: not a sample
			}
		}
	}()
	return pw
}

// observe folds one backend scrape into the maxima.
func (p *poolSamples) observe(s []promSample) {
	for _, x := range s {
		id := x.labels["instance_id"]
		switch x.name {
		case "scheduler_db_pool_acquired_conns":
			p.maxAcquired[id] = max(p.maxAcquired[id], x.value)
		case "scheduler_db_pool_max_conns":
			p.maxConns[id] = max(p.maxConns[id], x.value)
		}
	}
}

func (pw *poolWatch) stop() poolSamples {
	close(pw.stopCh)
	return <-pw.done
}

// poolVerdict is judgePool's result.
type poolVerdict struct {
	ok      bool
	waitMS  float64 // Σ empty-acquire wait added during the phase (ms)
	detail  string
	unknown bool // an instance restarted during the phase (counters reset): its wait is not measurable
}

// judgePool compares the scrapes before and after a phase with the samples taken during it.
// capFails makes a backend at its pool cap a problem (otherwise it is noted in the detail only).
func judgePool(before, after []promSample, samples poolSamples, budgetMS float64, capFails bool) poolVerdict {
	var v poolVerdict
	instances := map[string]bool{}
	for _, s := range append(slices.Clone(before), after...) {
		if id := s.labels["instance_id"]; id != "" && strings.HasPrefix(s.name, "scheduler_db_pool_") {
			instances[id] = true
		}
	}
	var parts, problems, notes []string
	for _, id := range slices.Sorted(maps.Keys(instances)) {
		match := map[string]string{"instance_id": id}
		d := promSum(after, "scheduler_db_pool_empty_acquire_wait_seconds_total", match) -
			promSum(before, "scheduler_db_pool_empty_acquire_wait_seconds_total", match)
		// A restart resets the counter: it reads lower than before, or — when the new process queued
		// longer than the old one had (a fresh pool under the replayed burst) — higher, which only
		// the changed process start time shows.
		start0, start1 := promSum(before, "process_start_time_seconds", match), promSum(after, "process_start_time_seconds", match)
		if d < 0 || (start0 > 0 && start1 > 0 && start0 != start1) {
			v.unknown = true
			d = 0
		}
		v.waitMS += 1000 * d
		peak, limit := samples.maxAcquired[id], samples.maxConns[id]
		if limit == 0 {
			limit = promSum(after, "scheduler_db_pool_max_conns", match)
		}
		parts = append(parts, fmt.Sprintf("%s peak acquired %.0f/%.0f, empty-acquire wait +%.0f ms", id, peak, limit, 1000*d))
		if limit > 0 && peak >= limit {
			msg := fmt.Sprintf("%s reached its pool cap (%.0f of %.0f acquired)", id, peak, limit)
			if capFails {
				problems = append(problems, msg)
			} else {
				notes = append(notes, msg+", not judged: -pool-cap-fails=false")
			}
		}
	}
	if len(instances) == 0 {
		problems = append(problems, "no scheduler_db_pool_* series scraped")
	}
	if samples.samples == 0 {
		problems = append(problems, "no sample taken during the phase")
	}
	// "Never at the cap" is only evidence for a backend whose gauges were read most of the time:
	// judged per backend, so one that failed every scrape (its peak 0) cannot hide behind the
	// other's successes.
	for _, b := range slices.Sorted(maps.Keys(samples.backendTries)) {
		if tries, errs := samples.backendTries[b], samples.backendErrs[b]; 2*errs >= tries {
			problems = append(problems, fmt.Sprintf("%d of %d scrapes of %s failed: its pool was not watched", errs, tries, b))
		}
	}
	if v.waitMS > budgetMS {
		problems = append(problems, fmt.Sprintf("Σ empty-acquire wait +%.0f ms > budget %.0f ms (requests queued for a connection)", v.waitMS, budgetMS))
	}
	v.ok = len(problems) == 0
	v.detail = fmt.Sprintf("%s (%d samples, %d scrape errors)", strings.Join(parts, "; "), samples.samples, samples.scrapeErrs)
	if len(problems) > 0 {
		v.detail += " — " + strings.Join(problems, "; ")
	}
	if len(notes) > 0 {
		v.detail += " (" + strings.Join(notes, "; ") + ")"
	}
	return v
}
