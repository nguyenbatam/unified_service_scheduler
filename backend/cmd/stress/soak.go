package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Soak (-mode soak, scripts/soak.sh): the other modes last minutes, so slow leaks never show —
// outbox or notification rows that stop draining, a hub or cache that keeps growing, goroutines
// left behind per request. The soak runs a low, steady mixed workload on the river shop for
// -soak-duration (book, read availability, reschedule and cancel its own bookings with If-Match,
// keeping at most soakLive alive) and samples both backends every -soak-sample. Judged:
//
//   - queues stay flat: the outbox and notification backlog (scheduler_queue_rows, pending) never
//     exceeds -soak-queue-max in the last third of the run, and no row is poisoned (failed) during it;
//   - memory stays flat: each backend's resident memory, median of the last quarter of samples, is
//     at most -soak-rss-growth-pct above the median of the first quarter (after warm-up); goroutines
//     likewise at most double (+50);
//   - answers stay sane and fast: no 5xx or malformed answer, p99 of writes within -slo-book-ms and
//     of reads within -slo-read-ms;
//   - afterwards the SQL integrity pass over its bookings and the outbox drain, as in a stress run.

const (
	soakLive = 40 // bookings kept alive at most
	soakDays = 14 // days ahead it books into
	soakCars = 64
)

// soakSample is one scrape of the cluster.
type soakSample struct {
	at         time.Time
	rss        map[string]float64 // instance_id → resident bytes
	goroutines map[string]float64
	pending    float64 // outbox + notification rows pending (cluster-wide: the max any instance reported)
	failed     float64
}

// soakStats is what the workload saw.
type soakStats struct {
	mu         sync.Mutex
	ops        map[string]int
	writeMS    []float64
	readMS     []float64
	unexpected []string
	samples    []soakSample
	scrapeErrs int
}

func (st *soakStats) count(op string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ops[op]++
}

func (st *soakStats) observe(write bool, d time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ms := float64(d) / float64(time.Millisecond)
	if write {
		st.writeMS = append(st.writeMS, ms)
	} else {
		st.readMS = append(st.readMS, ms)
	}
}

func (st *soakStats) bad(format string, args ...any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.ops["unexpected"]++
	if len(st.unexpected) < 5 {
		st.unexpected = append(st.unexpected, fmt.Sprintf(format, args...))
	}
}

// soak runs the soak and records its verdicts.
func (ss *session) soak(ctx context.Context, stop func()) (err error) {
	if ss.conn == nil {
		return errors.New("soak needs STRESS_DATABASE_URL (integrity pass, outbox drain, cleanup)")
	}
	s, m := ss.s, ss.m
	w, err := ensureWorkshop(ctx, ss.api, riverWorkshop)
	if err != nil {
		return err
	}
	oil, err := newJobMix(ss.catalog, "OIL", "OIL")
	if err != nil {
		return err
	}
	cars, err := ensureVehicles(ctx, ss.api, ss.owner, vinSoak, soakCars, 16)
	if err != nil {
		return err
	}
	today, err := booking.ParseDate(w.dealer.Today)
	if err != nil {
		return err
	}
	first, last := today.AddDays(1), today.AddDays(soakDays+1)
	if err := ss.resetDay(ctx, w, first.Start(w.loc), last.Start(w.loc)); err != nil {
		return err
	}
	defer func() {
		stop()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if cerr := ss.resetDay(cctx, w, first.Start(w.loc), last.Start(w.loc)); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	st := &soakStats{ops: map[string]int{}}
	backends := []string{s.backendA, s.backendB}
	runCtx, cancel := context.WithTimeout(ctx, s.soakDuration)
	defer cancel()
	fmt.Printf("soak start: %s, %.1f ops/s for %s, sampling every %s\n", w.dealer.Name, s.soakRate, s.soakDuration, s.soakSample)
	var wg sync.WaitGroup
	wg.Go(func() { st.sample(runCtx, backends, s.soakSample) })
	ss.soakLoad(runCtx, w, oil.ids, cars, first, st)
	wg.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted: %w", context.Cause(ctx))
	}
	st.record(m, s)
	if _, err := recordIntegrity(ctx, m, ss.conn, ss.runID, nil, "soak"); err != nil {
		return err
	}
	return recordOutbox(ctx, m, ss.conn, ss.runID, time.Minute, "soak outbox")
}

// soakLoad sends the mixed workload at s.soakRate until ctx ends.
func (ss *session) soakLoad(ctx context.Context, w workshop, job []uuid.UUID, cars []uuid.UUID, first booking.Date, st *soakStats) {
	rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 11)) //nolint:gosec // workload mix, not security
	var live []domain.Appointment
	tick := time.NewTicker(time.Duration(float64(time.Second) / ss.s.soakRate))
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		day := first.AddDays(rng.IntN(soakDays))
		switch op := i % 10; {
		case op < 4 && len(live) < soakLive:
			if a, ok := ss.soakBook(ctx, w, job, cars[i%len(cars)], day, i, st); ok {
				live = append(live, a)
			}
		case op < 7:
			t0 := time.Now()
			resp, definite := sendUntilDefinite(ctx, ss.api, http.MethodGet, fmt.Sprintf(
				"/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s", w.dealer.ID, cars[i%len(cars)], jobQuery(job), day), nil, nil, probeAttempts)
			st.observe(false, time.Since(t0))
			st.judge(ctx, "read", resp, definite)
		case op == 7 && len(live) > 0:
			k := rng.IntN(len(live))
			live[k] = ss.soakMove(ctx, w, job, live[k], st)
		case len(live) > 0:
			if a, ok := ss.soakChange(ctx, live[0], "cancel", nil, st); ok || a.Status == domain.StatusCancelled {
				live = live[1:]
			} else {
				live[0] = a
			}
		}
	}
}

// soakBook books the first free start of day with car.
func (ss *session) soakBook(ctx context.Context, w workshop, job []uuid.UUID, car uuid.UUID, day booking.Date, i int, st *soakStats) (domain.Appointment, bool) {
	avail, err := availability(ctx, ss.api, w.dealer.ID, car, job, day.String())
	if err != nil {
		return domain.Appointment{}, false
	}
	slot, ok := nearestOffered(avail.Slots, day.Start(w.loc).Add(12*time.Hour))
	if !ok {
		return domain.Appointment{}, false
	}
	t0 := time.Now()
	resp, definite := sendUntilDefinite(ctx, ss.api, http.MethodPost, "/api/v1/appointments", bookingPayload(w.dealer.ID, car, job, slot.Start),
		map[string]string{"Idempotency-Key": fmt.Sprintf("%s:soak:%d", ss.runID, i)}, probeAttempts)
	st.observe(true, time.Since(t0))
	if !st.judge(ctx, "book", resp, definite) {
		return domain.Appointment{}, false
	}
	var a domain.Appointment
	if resp.code != http.StatusCreated && resp.code != http.StatusOK || json.Unmarshal(resp.body, &a) != nil || a.ID == uuid.Nil {
		return domain.Appointment{}, false
	}
	return a, true
}

// soakMove reschedules a to a free start of its day and returns it as it now is (re-read after a
// stale version).
func (ss *session) soakMove(ctx context.Context, w workshop, job []uuid.UUID, a domain.Appointment, st *soakStats) domain.Appointment {
	day, err := booking.ParseDate(a.StartAt.In(w.loc).Format(time.DateOnly))
	if err != nil {
		return a
	}
	avail, err := getJSON[domain.Availability](ctx, ss.api, "GET", fmt.Sprintf(
		"/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s&excludeAppointmentId=%s", w.dealer.ID, a.VehicleID, jobQuery(job), day, a.ID), nil)
	if err != nil {
		return a
	}
	slot, ok := nearestOffered(avail.Slots, a.StartAt.Add(2*time.Hour))
	if !ok || slot.Start.Equal(a.StartAt) {
		return a
	}
	moved, _ := ss.soakChange(ctx, a, "reschedule", map[string]any{"startAt": slot.Start}, st)
	return moved
}

// soakChange posts op on a with If-Match and reports whether it applied (200), with the
// appointment as it now is. A 412 is not an error: another soak booking's one-move repair bumped
// the version, or a retry of a write whose first attempt committed unheard carried the old one;
// the appointment is re-read so the next change uses the current version.
func (ss *session) soakChange(ctx context.Context, a domain.Appointment, op string, body any, st *soakStats) (domain.Appointment, bool) {
	t0 := time.Now()
	resp, definite := sendUntilDefinite(ctx, ss.api, http.MethodPost, "/api/v1/appointments/"+a.ID.String()+"/"+op, body,
		map[string]string{"If-Match": `"` + strconv.Itoa(a.Version) + `"`}, probeAttempts)
	st.observe(true, time.Since(t0))
	if !st.judge(ctx, op, resp, definite) {
		return a, false
	}
	switch resp.code {
	case http.StatusOK:
		var updated domain.Appointment
		if json.Unmarshal(resp.body, &updated) != nil || updated.ID != a.ID {
			return a, false
		}
		return updated, true
	case http.StatusPreconditionFailed:
		if fresh, ok := readAppointment(ctx, ss.api, a.ID); ok {
			return fresh, false
		}
	}
	return a, false
}

// judge counts one answer: definite 2xx/409/412/422 are fine (a busy car, a start taken, a
// version moved on), indefinite ones are counted, anything else is unexpected. It reports whether the answer was definite.
func (st *soakStats) judge(ctx context.Context, op string, resp response, definite bool) bool {
	switch {
	case ctx.Err() != nil:
		return false
	case !definite:
		st.count(op + "/busy")
		return false
	case resp.code/100 == 2:
		st.count(op + "/ok")
	case resp.code == http.StatusConflict || resp.code == http.StatusUnprocessableEntity:
		st.count(op + "/refused")
	case resp.code == http.StatusPreconditionFailed:
		st.count(op + "/stale") // the version moved on (a repair, or a retried write): re-read
	default:
		st.bad("%s answered %d: %s", op, resp.code, truncate(string(resp.body), 100))
	}
	return true
}

// sample scrapes the backends every interval until ctx ends (one more at the end).
func (st *soakStats) sample(ctx context.Context, backends []string, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		smp := soakSample{at: time.Now(), rss: map[string]float64{}, goroutines: map[string]float64{}}
		got := false
		for _, b := range backends {
			samples, err := scrape(sctx, b)
			if err != nil {
				st.mu.Lock()
				st.scrapeErrs++
				st.mu.Unlock()
				continue
			}
			got = true
			id := instanceOf(samples)
			smp.rss[id] = promSum(samples, "process_resident_memory_bytes", nil)
			smp.goroutines[id] = promSum(samples, "go_goroutines", nil)
			smp.pending = max(smp.pending, promSum(samples, "scheduler_queue_rows", map[string]string{"status": "pending"}))
			smp.failed = max(smp.failed, promSum(samples, "scheduler_queue_rows", map[string]string{"status": "failed"}))
		}
		cancel()
		if got {
			st.mu.Lock()
			st.samples = append(st.samples, smp)
			st.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// instanceOf is the instance_id label a backend puts on its own series ("" when none does).
func instanceOf(samples []promSample) string {
	for _, s := range samples {
		if id := s.labels["instance_id"]; id != "" {
			return id
		}
	}
	return ""
}

// soakGrowth compares the median of the first and of the last quarter of values (in order) and
// reports the growth as a fraction (0.1 = +10 %); ok is false with fewer than 4 values.
func soakGrowth(values []float64) (growth float64, ok bool) {
	if len(values) < 4 {
		return 0, false
	}
	q := len(values) / 4
	median := func(v []float64) float64 {
		c := slices.Clone(v)
		sort.Float64s(c)
		return c[len(c)/2]
	}
	head, tail := median(values[:q]), median(values[len(values)-q:])
	if head <= 0 {
		return 0, false
	}
	return tail/head - 1, true
}

// record writes the soak's verdicts.
func (st *soakStats) record(m *matrix, s settings) {
	st.mu.Lock()
	defer st.mu.Unlock()
	src := "soak"
	ops := make([]string, 0, len(st.ops))
	for k, v := range st.ops {
		ops = append(ops, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(ops)
	m.check("Q1", src, len(st.unexpected) == 0 && st.ops["book/ok"] > 0,
		"%s of mixed load (%s): no 5xx or malformed answer%s", s.soakDuration, strings.Join(ops, " "), sampleSuffix(st.unexpected))
	write, read := summarize(slices.Clone(st.writeMS)), summarize(slices.Clone(st.readMS))
	m.check("Q3", src, write.n > 0 && read.n > 0 && write.p99 <= s.sloBookMS && read.p99 <= s.sloReadMS,
		"p99 write %.0f ms (budget %.0f), p99 read %.0f ms (budget %.0f); writes (ms) %s; reads (ms) %s",
		write.p99, s.sloBookMS, read.p99, s.sloReadMS, write, read)
	if len(st.samples) < 8 {
		m.skip("Q4", src, "%d samples (%d scrape errors): run longer than 8 × -soak-sample to judge memory and queues", len(st.samples), st.scrapeErrs)
		return
	}
	tail := st.samples[len(st.samples)*2/3:]
	peak, failedGrowth := 0.0, tail[len(tail)-1].failed-st.samples[0].failed
	for _, smp := range tail {
		peak = max(peak, smp.pending)
	}
	m.check("Q4", src, peak <= s.soakQueueMax && failedGrowth <= 0,
		"queues flat: outbox + notification rows pending peaked at %.0f in the last third of the run (max %.0f); failed rows %+.0f during it (%d samples, %d scrape errors)",
		peak, s.soakQueueMax, failedGrowth, len(st.samples), st.scrapeErrs)
	parts, ok := soakMemory(st.samples, soakBackends(s), s.soakRSSGrowthPct)
	m.check("Q4", src, ok, "memory flat (last quarter vs first quarter medians; resident memory at most +%.0f%%, goroutines at most double): %s",
		s.soakRSSGrowthPct, strings.Join(parts, "; "))
}

// soakBackends is how many backends the soak samples.
func soakBackends(s settings) int {
	n := 0
	for _, b := range []string{s.backendA, s.backendB} {
		if b != "" {
			n++
		}
	}
	return n
}

// soakMemory judges each backend's resident memory and goroutines over the samples (the first one
// is before any load: warm-up). Instances are collected across every sample, so a backend whose
// first scrape failed is still judged, each on its own samples; one with too few samples, or fewer
// instances seen than backends sampled, fails (no evidence is not a pass).
func soakMemory(samples []soakSample, backends int, rssGrowthPct float64) (parts []string, ok bool) {
	ok = true
	ids := map[string]bool{}
	for _, smp := range samples {
		for id := range smp.rss {
			ids[id] = true
		}
	}
	if len(ids) < backends {
		ok = false
		parts = append(parts, fmt.Sprintf("only %d of %d backends answered a scrape", len(ids), backends))
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		var rss, gor []float64
		for _, smp := range samples[min(1, len(samples)):] {
			if v, found := smp.rss[id]; found {
				rss = append(rss, v)
				gor = append(gor, smp.goroutines[id])
			}
		}
		g, okR := soakGrowth(rss)
		gg, okG := soakGrowth(gor)
		if !okR || !okG {
			ok = false
			parts = append(parts, fmt.Sprintf("%s: too few samples (%d)", id, len(rss)))
			continue
		}
		if g*100 > rssGrowthPct || (gg > 1 && gor[len(gor)-1]-gor[0] > 50) {
			ok = false
		}
		parts = append(parts, fmt.Sprintf("%s resident memory %+.1f%%, goroutines %+.0f%%", id, 100*g, 100*gg))
	}
	return parts, ok
}
