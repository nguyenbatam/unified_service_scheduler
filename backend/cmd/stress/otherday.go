package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Other days stay parallel. Every writer that could collide with a booking queues on its
// dealership-day lock, and each instance lets at most a few bookings of one day into the database
// (the admission gate): a hot day must cost only itself. While a phase hammers its date, an
// uncontended writer books the same dealership on another date, one booking at a time every
// otherDayEvery, each starting at a free start with its own car; its bookings must be accepted
// (nothing else books that date) and their p99 must stay within -slo-other-day-ms. A day lock or a
// gate keyed too coarsely (per dealership, or a pool drained by the hot day) shows up here as
// seconds of latency or 503s.

const (
	otherDayEvery = 100 * time.Millisecond
	otherDayMax   = 60
)

// otherDayWriter books another date of p's dealership while p is hammered.
type otherDayWriter struct {
	date     string
	stopCh   chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	lat      []float64 // ms per booking, end to end (retries included)
	ids      []uuid.UUID
	refused  int
	unknown  int // no definite answer
	errs     []string
	planned  int
	prepared error
	stats    *runStats // its own: the hammer's latency and error checks stay the hammer's
}

// startOtherDay plans up to otherDayMax bookings on the first free date after p's (one per free
// place of each start, one car each) and starts sending them; stop ends it.
func startOtherDay(ctx context.Context, api *apiClient, p *plan, pool []uuid.UUID, keyPrefix string, maxAttempts int) *otherDayWriter {
	w := &otherDayWriter{stopCh: make(chan struct{}), stats: newRunStats()}
	starts, err := w.plan(ctx, api, p, pool)
	if err != nil {
		w.prepared = err
		return w
	}
	w.planned = len(starts)
	w.wg.Go(func() {
		tick := time.NewTicker(otherDayEvery)
		defer tick.Stop()
		for i, start := range starts {
			if i > 0 { // the first booking goes at once: a short hammer phase still overlaps one
				select {
				case <-ctx.Done():
					return
				case <-w.stopCh:
					return
				case <-tick.C:
				}
			}
			t0 := time.Now()
			payload := bookingPayload(p.dealer.ID, pool[i], p.jobs[0], start)
			r := bookWithRetry(ctx, api, payload, fmt.Sprintf("%s:%d", keyPrefix, i), maxAttempts, w.stats)
			w.record(r, time.Since(t0))
		}
	})
	return w
}

// plan lists the free places on the first free date after p.date, at most otherDayMax and no more
// than there are cars; under a staging cap, one start per arrival window.
func (w *otherDayWriter) plan(ctx context.Context, api *apiClient, p *plan, pool []uuid.UUID) ([]time.Time, error) {
	from, err := nextDate(p.date)
	if err != nil {
		return nil, err
	}
	if w.date, err = nextFreeDate(ctx, api, p.dealer.ID, pool[0], p.jobs[0], from); err != nil {
		return nil, err
	}
	avail, err := availability(ctx, api, p.dealer.ID, pool[0], p.jobs[0], w.date)
	if err != nil {
		return nil, err
	}
	var starts []time.Time
	perWindow := map[int64]int{} // with a staging cap: one arrival per window (an offered start has room for one)
	for _, s := range avail.Slots {
		if !s.Available || s.Start.Before(p.cutoff) {
			continue
		}
		places := min(s.FreeBays, s.FreeTechnicians)
		if p.dealer.StagingCapacity > 0 {
			from := arrivalWindowStart(s.Start, p.loc, int(p.dealer.StagingWindowMinutes))
			if perWindow[from.Unix()] > 0 {
				continue
			}
			perWindow[from.Unix()]++
			places = 1
		}
		for range places {
			starts = append(starts, s.Start)
		}
	}
	starts = starts[:min(len(starts), otherDayMax, len(pool))]
	if len(starts) == 0 {
		return nil, fmt.Errorf("no free place on %s", w.date)
	}
	return starts, nil
}

func (w *otherDayWriter) record(r bookResult, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lat = append(w.lat, float64(d)/float64(time.Millisecond))
	switch r.outcome {
	case outCreated, outReplayed:
		w.ids = append(w.ids, r.id)
	case outConflict, outUnavailable:
		w.refused++
	case outUnresolved:
		w.unknown++
	default:
		w.errs = w.stats.errorsSample()
	}
}

// stop ends the writer and returns its bookings.
func (w *otherDayWriter) stop() []uuid.UUID {
	close(w.stopCh)
	w.wg.Wait()
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ids)
}

// verdict judges the writer: something was booked, nothing refused (the date had room for every
// planned booking and nobody else books it), no error, p99 within budget.
func (w *otherDayWriter) verdict(phase string, budgetMS float64) (ok bool, detail string) {
	if w.prepared != nil {
		return false, fmt.Sprintf("could not plan bookings on another date: %v", w.prepared)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	lat := summarize(slices.Clone(w.lat))
	ok = len(w.ids) > 0 && w.refused == 0 && len(w.errs) == 0 && lat.p99 <= budgetMS
	return ok, fmt.Sprintf("while %s was hammered, %d of %d planned bookings went to %s (the same dealership, another day) one every %s: %d booked, %d refused, %d unresolved, errors %v; p99 %.0f ms (budget %.0f ms) — (ms) %s",
		phase, len(w.lat), w.planned, w.date, otherDayEvery, len(w.ids), w.refused, w.unknown, w.errs, lat.p99, budgetMS, lat)
}

// unresolved is how many of its requests never got a definite answer (they may have committed).
func (w *otherDayWriter) unresolved() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unknown
}
