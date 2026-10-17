package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// -mode bench measures what this machine's stack can serve: for each workload it offers an
// increasing open-loop rate (requests are due at fixed times, so a slow server shows up as latency,
// not as a quietly lower send rate) and finds the highest step that still meets the SLO. Every
// step reports the client view (achieved rate, latency percentiles, errors) next to the server
// view sampled from /metrics and Postgres, so the numbers can be checked against each other:
//
//   - Little's law, L = λ·W: requests in flight on the servers ≈ server throughput × server mean
//     latency (from the latency histogram); a large gap means queueing outside the handler (the
//     gateway, the client) or a broken metric. The gauge leaves WebSocket sessions out on the
//     server side (an open dashboard's socket would otherwise count as one request in flight for
//     hours), and each sampler scrape, itself in flight on the instance it reads, is subtracted.
//     It is only meaningful under steady load: at a few requests in flight the 250 ms samples are
//     noise, and the bursty "hot" rounds fall between samples (the gauge reads low);
//   - pool: when acquired connections sit at the pool size and the empty-acquire wait grows, the
//     connection pool, not Postgres, caps throughput (≈ pool size / mean transaction time);
//   - Postgres commits per booking: one booking is one write transaction, plus the outbox worker's
//     claim and mark; far more per booking means retries or wasted work;
//   - client dispatch lag: how late the generator actually sent each request after its due time.
//     A step whose p99 lag exceeds benchMaxDispatchLagMS measured the load generator, not the
//     stack, and fails.
//
// After the ramps the bench runs the SQL integrity checks over everything it booked (no overlap,
// qualified on-duty technician, right bay, snapshot, one CREATED event each) and waits for the
// outbox to drain: a fast stack that double-books or loses events must not pass.
//
// Workloads (all on the "Stress Bench" shop: benchBays express bays and lubrication technicians),
// run in this order whatever order -bench-workloads lists them in:
//
//	book  uncontended staff bookings: consecutive requests go to different days, each start holds
//	      benchBays, so every request should be 201 (the write path at its fastest)
//	read  availability of one day (a staff dispatcher's screen), over the days "book" just filled,
//	      so the read buckets real appointments instead of an empty board
//	hot   contended bookings: rounds of 5×benchBays requests on one fresh start (benchBays win,
//	      the rest 409): how fast the system says "no" while the slot fills. Every round fully sent
//	      in a step must end with exactly benchBays bookings (counted in Postgres), or the step fails
//	journey  one customer's visit, as a signed-in customer (provisioned accounts with their own
//	      cars, like the customers phase), so the customer path runs: role and ownership checks, the
//	      self-service policy (lead time, horizon), and the write/availability throttle. It reads
//	      the day's availability, then books a start of it (spread so every booking fits). The rate
//	      is users per second; each user is two requests. One load generator is one client IP, and
//	      the customer throttle allows an IP 120 bookings and 600 availability reads a minute, so
//	      from one machine its knee is that budget (a step answered 429 stops the ramp and says so);
//	      the stack's own write capacity is what "book" measures.

type benchStep struct {
	Workload        string   `json:"workload"`
	OfferedRPS      float64  `json:"offeredRps"`
	Seconds         float64  `json:"seconds"`
	Sent            int      `json:"sent"`
	Dropped         int      `json:"droppedClientSaturated"`
	AchievedRPS     float64  `json:"achievedRps"`
	OK              int      `json:"ok"`
	Refused         int      `json:"refused"`
	Busy            int      `json:"busy"`
	Throttled       int      `json:"throttled429"`
	Errors          int      `json:"errors"`
	P50, P95, P99   float64  `json:"-"`
	Latency         string   `json:"latencyMs"`
	MeanMS          float64  `json:"meanMs"`
	ServerRPS       float64  `json:"serverRps"`
	ServerMeanMS    float64  `json:"serverMeanMs"`
	ServerInFlight  float64  `json:"serverInFlightAvg"`
	LittlePredicted float64  `json:"littlePredicted"`
	PoolAcquired    float64  `json:"poolAcquiredAvg"`
	PoolMax         float64  `json:"poolMax"`
	EmptyWaitMS     float64  `json:"emptyAcquireWaitMsPerSec"`
	CommitsPerSec   float64  `json:"pgCommitsPerSec"`
	DispatchLagP99  float64  `json:"clientDispatchLagP99Ms"`
	DispatchLagMax  float64  `json:"clientDispatchLagMaxMs"`
	HotRounds       int      `json:"hotRoundsChecked,omitempty"`
	HotProblems     []string `json:"hotRoundProblems,omitempty"` // rounds over capacity, or short while requests were refused
	Passed          bool     `json:"passed"`
	Why             string   `json:"why,omitempty"`
}

// benchConfig is the bench part of the settings.
type benchConfig struct {
	workloads []string
	rates     map[string][]float64
	step      time.Duration
	sloMS     map[string]float64
}

// benchTarget is what the request generators book against.
type benchTarget struct {
	shop     workshop
	oil      []uuid.UUID
	vehicles []uuid.UUID
	// customers are the journey's signed-in customers, each with one car of its own.
	customers []stressCustomer
	firstDay  booking.Date
	days      int
	runID     string
	// One request counter per workload, so each walks its own starts.
	nRead, nBook, nHot, nJourney atomic.Int64
	// hotRefused counts the 409/422 answers per hot round: a round left short of benchBays while
	// some of its requests were refused refused them wrongly.
	hotMu      sync.Mutex
	hotRefused map[int]int
}

func (t *benchTarget) noteHotRefused(round int) {
	t.hotMu.Lock()
	defer t.hotMu.Unlock()
	if t.hotRefused == nil {
		t.hotRefused = map[int]int{}
	}
	t.hotRefused[round]++
}

func (t *benchTarget) hotRefusedIn(round int) int {
	t.hotMu.Lock()
	defer t.hotMu.Unlock()
	return t.hotRefused[round]
}

// benchMaxDispatchLagMS is the most the generator may send late (p99) before a step is judged to
// have measured the client rather than the stack.
const benchMaxDispatchLagMS = 50

// benchOrder is the order the workloads run in: read after book (non-empty days).
var benchOrder = []string{"book", "read", "hot", "journey"}

// runBench runs every workload's ramp and reports. It returns the created ids for cleanup.
func runBench(ctx context.Context, s settings, api *apiClient, conn *pgx.Conn, tgt *benchTarget, m *matrix) error {
	printContext(ctx, s, conn)
	var all []benchStep
	for _, wl := range benchOrder {
		if !slices.Contains(s.bench.workloads, wl) {
			continue
		}
		best := 0.0
		var stop benchStep
		for _, rate := range s.bench.rates[wl] {
			step, err := benchOnce(ctx, s, api, conn, tgt, wl, rate)
			if err != nil {
				return err
			}
			all = append(all, step)
			printStep(step)
			// Wrong answers fail the run whatever the load: they are bugs, not the knee.
			if len(step.HotProblems) > 0 {
				m.fail("Q1", "bench hot", "at %.0f req/s: %s", rate, strings.Join(step.HotProblems, "; "))
			}
			if (wl == "book" || wl == "journey") && step.Refused > 0 {
				m.fail("R2b", "bench "+wl, "at %.0f req/s: %d uncontended bookings refused (409/422) — every one of them had a free start", rate, step.Refused)
			}
			if !step.Passed {
				stop = step
				break
			}
			best = rate
		}
		slo := s.bench.sloMS[wl]
		if best > 0 && wl == "journey" {
			w := lastPassed(all, wl)
			capped := ""
			if stop.Throttled > 0 {
				capped = fmt.Sprintf("; the next step (%.0f users/s) was throttled (%d answers 429): the per-IP customer budget of one load generator, not the stack's capacity (see bench book)", stop.OfferedRPS, stop.Throttled)
			}
			m.pass("Q3", "bench journey", "signed-in customers (customer policy + throttle on the path): %.0f booking users/s (%.0f req/s: availability + booking) at p99 <= %.0f ms; %.1f users in flight on average (Little: λ·W)%s",
				best, 2*best, slo, best*w.MeanMS/1000, capped)
		} else if best > 0 && wl == "hot" {
			rounds := 0
			for _, st := range all {
				if st.Workload == wl && st.Passed {
					rounds += st.HotRounds
				}
			}
			m.pass("Q3", "bench hot", "sustains %.0f req/s with p99 <= %.0f ms, errors <= 1%%, achieved >= 90%% of offered; each of the %d rounds sent in full holds exactly %d bookings (counted in Postgres)",
				best, slo, rounds, benchBays)
		} else if best > 0 {
			m.pass("Q3", "bench "+wl, "sustains %.0f req/s with p99 <= %.0f ms, errors <= 1%%, achieved >= 90%% of offered", best, slo)
		} else {
			m.fail("Q3", "bench "+wl, "not even the first step (%.0f req/s) passed (p99 budget %.0f ms): %s", s.bench.rates[wl][0], slo, stop.Why)
		}
		m.metric("benchMax_"+wl, best)
	}
	m.metric("bench", all)
	if _, err := recordIntegrity(ctx, m, conn, tgt.runID, []string{mixKey(tgt.oil)}, "bench sql"); err != nil {
		return err
	}
	return recordOutbox(ctx, m, conn, tgt.runID, time.Minute, "bench outbox")
}

// benchOnce offers rate requests/s of workload for one step.
func benchOnce(ctx context.Context, s settings, api *apiClient, conn *pgx.Conn, tgt *benchTarget, workload string, rate float64) (benchStep, error) {
	st := benchStep{Workload: workload, OfferedRPS: rate}
	backends := []string{s.backendA, s.backendB}
	routes := []map[string]string{{"route": "/api/v1/appointments", "method": "POST"}}
	switch workload {
	case "read":
		routes = []map[string]string{{"route": "/api/v1/availability", "method": "GET"}}
	case "journey": // both requests of a visit (never the /metrics scrapes)
		routes = append(routes, map[string]string{"route": "/api/v1/availability", "method": "GET"})
	}
	before, err := clusterScrape(ctx, backends...)
	if err != nil {
		return st, err
	}
	commits0, err := pgCommits(ctx, conn)
	if err != nil {
		return st, err
	}
	sampler := startSampler(ctx, backends)
	hotFrom := tgt.nHot.Load()

	var mu sync.Mutex
	var lat, lag []float64
	var inFlight atomic.Int64
	var wg sync.WaitGroup
	t0 := time.Now()
	n := int(rate * s.step().Seconds())
	for i := range n {
		due := t0.Add(time.Duration(float64(i) * float64(time.Second) / rate))
		if wait := time.Until(due); wait > 0 {
			select {
			case <-ctx.Done():
				wg.Wait()
				sampler.stop()
				return st, ctx.Err()
			case <-time.After(wait):
			}
		}
		if inFlight.Load() >= maxClientInFlight {
			st.Dropped++
			continue
		}
		st.Sent++
		inFlight.Add(1)
		wg.Go(func() {
			defer inFlight.Add(-1)
			late := float64(time.Since(due)) / float64(time.Millisecond)
			code, err := tgt.send(ctx, api, workload)
			ms := float64(time.Since(due)) / float64(time.Millisecond)
			mu.Lock()
			defer mu.Unlock()
			lat = append(lat, ms)
			lag = append(lag, late)
			switch {
			case err != nil || code >= 500 && code != http.StatusServiceUnavailable && code != http.StatusGatewayTimeout && code != http.StatusBadGateway:
				st.Errors++
			case code == http.StatusOK || code == http.StatusCreated:
				st.OK++
			case code == http.StatusConflict || code == http.StatusUnprocessableEntity:
				st.Refused++
			case code == http.StatusTooManyRequests:
				st.Throttled++
			case indefinite(code, nil):
				st.Busy++
			default:
				st.Errors++
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(t0)
	samples := sampler.stop()
	after, err := clusterScrape(ctx, backends...)
	if err != nil {
		return st, err
	}
	commits1, err := pgCommits(ctx, conn)
	if err != nil {
		return st, err
	}

	st.Seconds = elapsed.Seconds()
	// Achieved: definite answers only (a fast 502 from a dead backend is not throughput).
	st.AchievedRPS = float64(st.OK+st.Refused) / st.Seconds
	sum := summarize(lat)
	st.P50, st.P95, st.P99 = sum.p50, sum.p95, sum.p99
	st.Latency = sum.String()
	st.MeanMS = mean(lat)
	var cnt, secs float64
	for _, match := range routes {
		cnt += promSum(after, "scheduler_http_request_duration_seconds_count", match) - promSum(before, "scheduler_http_request_duration_seconds_count", match)
		secs += promSum(after, "scheduler_http_request_duration_seconds_sum", match) - promSum(before, "scheduler_http_request_duration_seconds_sum", match)
	}
	if cnt > 0 {
		st.ServerRPS = cnt / st.Seconds
		st.ServerMeanMS = 1000 * secs / cnt
	}
	// Each scrape is itself one in-flight request on the instance it reads.
	st.ServerInFlight = math.Max(0, samples.inFlight-float64(len(backends)))
	st.LittlePredicted = st.ServerRPS * st.ServerMeanMS / 1000
	st.PoolAcquired, st.PoolMax = samples.acquired, promSum(after, "scheduler_db_pool_max_conns", nil)
	wait := promSum(after, "scheduler_db_pool_empty_acquire_wait_seconds_total", nil) - promSum(before, "scheduler_db_pool_empty_acquire_wait_seconds_total", nil)
	st.EmptyWaitMS = 1000 * wait / st.Seconds
	st.CommitsPerSec = float64(commits1-commits0) / st.Seconds
	lagSum := summarize(lag)
	st.DispatchLagP99, st.DispatchLagMax = lagSum.p99, lagSum.maxValue

	slo := s.bench.sloMS[workload]
	failures := []string{}
	if total := len(lat) + st.Dropped; total > 0 && float64(st.Errors+st.Busy+st.Throttled+st.Dropped) > 0.01*float64(total) {
		failures = append(failures, fmt.Sprintf("errors %d + busy %d + throttled %d + client-dropped %d > 1%%", st.Errors, st.Busy, st.Throttled, st.Dropped))
	}
	if st.DispatchLagP99 > benchMaxDispatchLagMS {
		failures = append(failures, fmt.Sprintf("client dispatch lag p99 %.0f ms > %d ms: the load generator, not the stack, fell behind", st.DispatchLagP99, benchMaxDispatchLagMS))
	}
	if workload == "hot" {
		rounds, wrong, short, err := tgt.checkHotRounds(ctx, conn, hotFrom, tgt.nHot.Load())
		if err != nil {
			return st, err
		}
		st.HotRounds, st.HotProblems = rounds, wrong
		failures = append(append(failures, wrong...), short...)
		if st.OK == 0 {
			failures = append(failures, "no booking succeeded")
		}
	}
	if st.P99 > slo {
		failures = append(failures, fmt.Sprintf("p99 %.0f ms > %.0f", st.P99, slo))
	}
	if st.AchievedRPS < 0.9*rate {
		failures = append(failures, fmt.Sprintf("achieved %.0f < 90%% of offered", st.AchievedRPS))
	}
	if (workload == "book" || workload == "journey") && st.Refused > 0 {
		failures = append(failures, fmt.Sprintf("%d uncontended bookings refused", st.Refused))
	}
	st.Passed, st.Why = len(failures) == 0, strings.Join(failures, "; ")
	return st, nil
}

// send issues one request of workload and returns its status.
func (t *benchTarget) send(ctx context.Context, api *apiClient, workload string) (int, error) {
	switch workload {
	case "read":
		n := int(t.nRead.Add(1))
		day := t.firstDay.AddDays(n % t.days)
		resp, err := api.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s",
			t.shop.dealer.ID, t.vehicles[n%len(t.vehicles)], jobQuery(t.oil), day), nil, nil)
		return resp.code, err
	case "hot":
		n := int(t.nHot.Add(1)) - 1
		perRound := 5 * benchBays
		round, k := n/perRound, n%perRound
		start := t.slot(round)
		resp, err := api.do(ctx, http.MethodPost, "/api/v1/appointments",
			bookingPayload(t.shop.dealer.ID, t.vehicles[k%len(t.vehicles)], t.oil, start),
			map[string]string{"Idempotency-Key": fmt.Sprintf("%s:bench:hot:%d", t.runID, n)})
		if err == nil && (resp.code == http.StatusConflict || resp.code == http.StatusUnprocessableEntity) {
			t.noteHotRefused(round)
		}
		return resp.code, err
	case "journey":
		n := int(t.nJourney.Add(1)) - 1
		start, k := t.spreadAcross(n, t.journeyFirstDay(), len(t.customers))
		c := t.customers[k]
		resp, err := c.api.do(ctx, http.MethodGet, fmt.Sprintf("/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s",
			t.shop.dealer.ID, c.vehicle, jobQuery(t.oil), booking.DateFrom(start, t.shop.loc)), nil, nil)
		if err != nil || resp.code != http.StatusOK {
			return resp.code, err
		}
		resp, err = c.api.do(ctx, http.MethodPost, "/api/v1/appointments", bookingPayload(t.shop.dealer.ID, c.vehicle, t.oil, start),
			map[string]string{"Idempotency-Key": fmt.Sprintf("%s:bench:journey:%d", t.runID, n)})
		return resp.code, err
	default: // book
		n := int(t.nBook.Add(1)) - 1
		start, k := t.spread(n, 0)
		resp, err := api.do(ctx, http.MethodPost, "/api/v1/appointments",
			bookingPayload(t.shop.dealer.ID, t.vehicles[k], t.oil, start),
			map[string]string{"Idempotency-Key": fmt.Sprintf("%s:bench:book:%d", t.runID, n)})
		return resp.code, err
	}
}

// Day layout from firstDay: "book" uses days [0, days), "hot" [days, days+hotDays), "journey"
// [days+hotDays, 2·days+hotDays) — no workload ever meets another's bookings.
const hotDays = 40

func (t *benchTarget) journeyFirstDay() int { return t.days + hotDays }

// lastDay is one past the last day any workload books.
func (t *benchTarget) lastDay() int { return 2*t.days + hotDays }

// spread maps request n to a start and a vehicle so consecutive requests land on different days
// (different dealership-day locks: truly parallel writes) and no start gets more than benchBays,
// from day offset first.
func (t *benchTarget) spread(n, first int) (time.Time, int) {
	day := first + n%t.days
	slot := (n / t.days) % 20
	k := (n / (t.days * 20)) % benchBays
	return clockMinutes(8*60).on(t.firstDay.AddDays(day), t.shop.loc).Add(time.Duration(slot) * slotStep), k
}

// spreadAcross maps journey request n to a start and one of k customers: consecutive requests go
// to different customers (so no one customer's own write budget runs out first) and, per
// customer, to different days, never repeating a customer's start before days × 20 of its
// requests (each customer's car books one start once).
func (t *benchTarget) spreadAcross(n, first, k int) (time.Time, int) {
	k = max(1, k)
	who, m := n%k, n/k
	day := first + m%t.days
	slot := (m / t.days) % 20
	return clockMinutes(8*60).on(t.firstDay.AddDays(day), t.shop.loc).Add(time.Duration(slot) * slotStep), who
}

// hotRoundsCapacity is how many hot rounds fit before a round would reuse a start.
func (t *benchTarget) hotRoundsCapacity() int { return hotDays * 20 }

// checkHotRounds counts, in Postgres, the bookings of every hot round whose requests were all sent
// in [from, to) (request numbers): each must hold exactly benchBays. More is overbooking; fewer
// while some of the round's requests were answered 409/422 means they were refused with room left
// (wrong: a bug, whatever the load). Fewer with no refusal means the rest never got a definite
// answer (busy, errors): the step fails, but that is saturation, not a wrong answer (short).
func (t *benchTarget) checkHotRounds(ctx context.Context, conn *pgx.Conn, from, to int64) (rounds int, wrong, short []string, err error) {
	perRound := int64(5 * benchBays)
	first, last := (from+perRound-1)/perRound, to/perRound // rounds [first, last) are complete
	if last <= first {
		return 0, nil, nil, nil
	}
	starts := make([]time.Time, 0, last-first)
	for r := first; r < last; r++ {
		starts = append(starts, t.slot(int(r)))
	}
	rows, err := conn.Query(ctx, `
		SELECT o, (SELECT count(*)::int FROM appointments a
		           WHERE a.idempotency_key LIKE $1 AND a.start_at = s AND a.status IN (`+holdingSQL+`))
		FROM unnest($2::timestamptz[]) WITH ORDINALITY AS u(s, o)`, t.runID+":bench:hot:%", starts)
	if err != nil {
		return 0, nil, nil, err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var ord int64
		var n int
		if err := row.Scan(&ord, &n); err != nil {
			return struct{}{}, err
		}
		r := int(first + ord - 1)
		at := t.slot(r).In(t.shop.loc).Format("2006-01-02 15:04")
		refused := t.hotRefusedIn(r)
		switch {
		case n > benchBays && len(wrong) < 3:
			wrong = append(wrong, fmt.Sprintf("hot round at %s holds %d bookings, more than its %d places", at, n, benchBays))
		case n < benchBays && refused > 0 && len(wrong) < 3:
			wrong = append(wrong, fmt.Sprintf("hot round at %s holds %d of %d places although %d of its requests were refused (409/422)", at, n, benchBays, refused))
		case n < benchBays && refused == 0 && len(short) < 3:
			short = append(short, fmt.Sprintf("hot round at %s holds %d of %d places (the rest of its requests never got a definite answer)", at, n, benchBays))
		}
		return struct{}{}, nil
	})
	return int(last - first), wrong, short, err
}

// slot is the start of hot round r (20 rounds a day, from day offset days).
func (t *benchTarget) slot(r int) time.Time {
	day := t.days + (r/20)%hotDays
	return clockMinutes(8*60).on(t.firstDay.AddDays(day), t.shop.loc).Add(time.Duration(r%20) * slotStep)
}

// capacityBookings is how many uncontended bookings "book" can make before it would repeat a start.
func (t *benchTarget) capacityBookings() int { return t.days * 20 * benchBays }

// sampler averages the cluster's in-flight requests and acquired pool connections during a step.
type sampler struct {
	stopCh   chan struct{}
	done     chan benchSamples
	backends []string
}

type benchSamples struct{ inFlight, acquired float64 }

func startSampler(ctx context.Context, backends []string) *sampler {
	sp := &sampler{stopCh: make(chan struct{}), done: make(chan benchSamples, 1), backends: backends}
	go func() {
		var inflight, acquired, n float64
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-sp.stopCh:
				if n == 0 {
					n = 1
				}
				sp.done <- benchSamples{inFlight: inflight / n, acquired: acquired / n}
				return
			case <-tick.C:
				s, err := clusterScrape(ctx, backends...)
				if err != nil {
					continue
				}
				inflight += promSum(s, "scheduler_http_in_flight_requests", nil)
				acquired += promSum(s, "scheduler_db_pool_acquired_conns", nil)
				n++
			}
		}
	}()
	return sp
}

func (sp *sampler) stop() benchSamples {
	close(sp.stopCh)
	return <-sp.done
}

func pgCommits(ctx context.Context, conn *pgx.Conn) (int64, error) {
	var n int64
	err := conn.QueryRow(ctx, `SELECT xact_commit FROM pg_stat_database WHERE datname = current_database()`).Scan(&n)
	return n, err
}

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func printStep(st benchStep) {
	verdict := "ok  "
	if !st.Passed {
		verdict = "STOP"
	}
	fmt.Printf("bench %-4s %s offered=%5.0f/s achieved=%6.1f/s ok=%d refused=%d busy=%d throttled=%d err=%d dropped=%d | latency %s | dispatch lag p99 %.1f ms\n",
		st.Workload, verdict, st.OfferedRPS, st.AchievedRPS, st.OK, st.Refused, st.Busy, st.Throttled, st.Errors, st.Dropped, st.Latency, st.DispatchLagP99)
	ratio := "-"
	if st.LittlePredicted > 0 {
		ratio = fmt.Sprintf("%.2f", st.ServerInFlight/st.LittlePredicted)
	}
	commitsPerReq := "-"
	if st.ServerRPS > 0 {
		commitsPerReq = fmt.Sprintf("%.2f", st.CommitsPerSec/st.ServerRPS)
	}
	fmt.Printf("           server %5.1f/s mean %.1f ms | in-flight avg %.1f vs λ·W %.1f (ratio %s) | pool acquired %.1f / %.0f, empty-acquire wait %.1f ms/s | pg commits %.0f/s (%s per request)",
		st.ServerRPS, st.ServerMeanMS, st.ServerInFlight, st.LittlePredicted, ratio, st.PoolAcquired, st.PoolMax, st.EmptyWaitMS, st.CommitsPerSec, commitsPerReq)
	if st.Why != "" {
		fmt.Printf(" | %s", st.Why)
	}
	fmt.Println()
}

// printContext prints what the numbers depend on: the host, the containers, the database, and the
// server-side limits.
func printContext(ctx context.Context, s settings, conn *pgx.Conn) {
	fmt.Println("════════ bench context ════════")
	fmt.Printf("host: %s/%s, %d logical CPUs, %s, RAM %s, client %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), cpuModel(ctx), memTotal(ctx), runtime.Version())
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.NCPU}} CPUs, {{.MemTotal}} bytes, server {{.ServerVersion}}").Output(); err == nil {
		fmt.Printf("docker engine: %s\n", strings.TrimSpace(string(out)))
	}
	var version string
	var maxConns int
	_ = conn.QueryRow(ctx, `SELECT version(), current_setting('max_connections')::int`).Scan(&version, &maxConns)
	fmt.Printf("postgres: %s, max_connections=%d\n", firstN(strings.Fields(version), 2), maxConns)
	if samples, err := clusterScrape(ctx, s.backendA, s.backendB); err == nil {
		fmt.Printf("backends: 2 (%s, %s), pool max conns total=%.0f, GOMAXPROCS total=%.0f, gateway %s\n",
			s.backendA, s.backendB, promSum(samples, "scheduler_db_pool_max_conns", nil), promSum(samples, "go_sched_gomaxprocs_threads", nil), s.base)
	}
	fmt.Printf("steps: %s each; SLO p99 read=%.0f ms book=%.0f ms hot=%.0f ms\n", s.step(), s.bench.sloMS["read"], s.bench.sloMS["book"], s.bench.sloMS["hot"])
	fmt.Println("═══════════════════════════════")
}

func cpuModel(ctx context.Context) string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for line := range strings.SplitSeq(string(b), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
				return strings.TrimSpace(v)
			}
		}
	}
	return "unknown CPU"
}

func memTotal(ctx context.Context) string {
	if runtime.GOOS == "darwin" {
		if out, err := exec.CommandContext(ctx, "sysctl", "-n", "hw.memsize").Output(); err == nil {
			if n, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
				return fmt.Sprintf("%.0f GiB", n/(1<<30))
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		var kb float64
		if _, err := fmt.Sscanf(string(b), "MemTotal: %f kB", &kb); err == nil {
			return fmt.Sprintf("%.0f GiB", kb/(1<<20))
		}
	}
	return "unknown"
}

// parseRates parses "25,50,100" into rates.
func parseRates(s string) ([]float64, error) {
	var out []float64
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("rate %q", f)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no rates")
	}
	return out, nil
}

// lastPassed is the highest passing step of workload in steps.
func lastPassed(steps []benchStep, workload string) benchStep {
	var out benchStep
	for _, st := range steps {
		if st.Workload == workload && st.Passed {
			out = st
		}
	}
	return out
}
