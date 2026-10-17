// Concurrent booking stress with side-API, WebSocket, and SQL integrity checks.
//
// Every setting is a flag whose default comes from the environment (so scripts/stress.sh and CI can
// configure it either way; a variable that does not parse, or a negative number, fails the run
// instead of falling back to the default):
//
//	-base-url       BASE_URL        gateway (default http://127.0.0.1:8080)
//	-backend-a      BACKEND_A_URL   direct backend-1 (default http://127.0.0.1:18081)
//	-backend-b      BACKEND_B_URL   direct backend-2 (default http://127.0.0.1:18082)
//	-hammer-url     HAMMER_URL      where the hammer phases send their bookings (default -base-url;
//	                                the backend-kill chaos sends them straight to the backend it
//	                                kills, so the kill always cuts requests off)
//	-concurrency    CONCURRENCY     requests per hammered slot (default 1000)
//	-vehicles       VEHICLES        distinct vehicles one slot's requests rotate through (default
//	                                CONCURRENCY), so they collide on bay/technician GiST rather than
//	                                all on one vehicle
//	-slots          SLOTS           slots to hammer per phase (default 5)
//	-workers        WORKERS         booking worker pool (default min(CONCURRENCY, 512))
//	-side-workers   SIDE_WORKERS    concurrent read/admin probes during the hammer (default 16: with
//	                                the day gate, peak pool use stays under DB_MAX_CONNS, see pool.go)
//	-ws-clients     WS_CLIENTS      WS listeners per backend (default 2, at most 10: the hub's
//	                                per-user socket cap, and every listener is the admin; 0 skips
//	                                the fan-out check, e.g. against a single backend without Kafka)
//	-start-margin   START_MARGIN    no hammered slot (or move target) starts sooner than this after
//	                                its phase is prepared (default 15m; keep it above a phase's
//	                                duration, or a slot slides into the past mid-run); a date with
//	                                nothing later rolls over to the next free date
//	-idem-racers    IDEM_RACERS     concurrent sends of one Idempotency-Key + payload after the
//	                                phases, half straight to backend A and half to backend B: exactly
//	                                one appointment per key, both instances answered (default 16; 0 skips)
//	-idem-keys      IDEM_KEYS       keys the idempotency race runs at once (default 4)
//	-customer-requests CUSTOMER_REQUESTS  concurrent bookings as the seeded customer (own vehicles,
//	                                lead time, write throttle: 409/422/429 are expected answers)
//	                                after the phases (default 40; 0 skips)
//	-customer-email/-customer-password  STRESS_CUSTOMER_EMAIL / STRESS_CUSTOMER_PASSWORD (default
//	                                the seeded demo customer; a failed login fails the burst)
//	-mixed-ops      MIXED_OPS       bookings per phase reassigned / rescheduled / cancelled with
//	                                If-Match while the hammer still runs (default 0 = off)
//	-min-instances  MIN_INSTANCES   through the gateway (-base-url is neither backend), fail unless
//	                                at least this many X-Instance-IDs gave definite booking answers
//	                                (201/200/409/422) (default 2; 0 disables)
//	-phases         PHASES          comma list: river (its own "Stress River" shop: no staging cap,
//	                                one line, exact capacity check), north (staging cap + multi-line
//	                                jobs), storm (reschedules racing new bookings for one start on the
//	                                river shop, storm.go), truth, fill, quality (a scripted day where
//	                                greedy first fit loses, qualityday.go), customers (default
//	                                river,north,storm,truth,fill,quality,customers)
//	-rate           RATE            open loop: bookings per second each phase sends at (default 0 =
//	                                closed loop, a booking goes out as soon as a worker is free);
//	                                end-to-end latency then counts from the intended send time
//	-max-attempts   MAX_ATTEMPTS    sends per booking incl. retries with the same Idempotency-Key
//	                                after a transport error / 429 / 502 / 503 / 504 (default 6)
//	-max-unresolved-pct MAX_UNRESOLVED_PCT  fail a phase when more than this percentage of its
//	                                requests never got a definite answer (default 10); a phase that
//	                                creates no booking at all always fails
//	-dsn            STRESS_DATABASE_URL  Postgres DSN for the SQL integrity pass, the oracle of the truth
//	                                and fill phases, and for cleaning up bookings that committed without
//	                                the client hearing (the phases that need it are skipped if empty)
//	-email/-password STRESS_ADMIN_EMAIL / STRESS_ADMIN_PASSWORD (default the seeded demo admin;
//	                                one login per run: the session refreshes its access token
//	                                with the refresh cookie before it expires, authclient.go)
//	-fill-requests  FILL_REQUESTS   customers of the fill day (default 150: more than the shop holds;
//	                                at most 5000, each gets a car of its own)
//	-fill-rate      FILL_RATE       open loop: customers arriving per second on the fill day (default
//	                                500: the day's demand arrives within a fraction of a second, so
//	                                customers really race for starts; each one's first read is also
//	                                timed from its intended arrival and budgeted); 0 = closed loop,
//	                                FILL_WORKERS at a time
//	-fill-workers   FILL_WORKERS    customers in flight at most on the fill day (default 0 = all of
//	                                them: truly open loop, nobody waits for the generator; a smaller
//	                                cap makes customers due while all are busy wait, and the wait
//	                                counts against the arrival budget)
//	-firstfit-margin-pct FIRSTFIT_MARGIN_PCT  the server's sequential replay of the fill demand may
//	                                book at most this percentage fewer labour minutes than the same
//	                                demand replayed offline by a naive first-fit allocator on the
//	                                oracle's model of the shop (default 0: never fewer; see firstfit.go)
//	-fill-seed      FILL_SEED       demand seed (default 1: the same day every run)
//	-fit-seed       FIT_SEED        the fit days' demand seed (default 1; fitday.go)
//	-fit-min-gain-pct FIT_MIN_GAIN_PCT  over the fit days (demand 1.0 / 1.15 / 1.3 × the capacity
//	                                bound, booked one at a time) the server must book at least this
//	                                percentage more labour minutes than first fit in the median of
//	                                24 random resource orders (default 0: never fewer; measured
//	                                +3.6 % / +5.6 % pooled over 20 seeds on a Saturday / weekday,
//	                                seed 1 +2.6 % / +4.2 %; see fitday_db_test.go)
//	-quality-margin-pct QUALITY_MARGIN_PCT  the quality day's server must book at least this
//	                                percentage more labour minutes than the best first-fit replay
//	                                of the script in any fixed resource order (default 15: measured
//	                                +23.1 %, one lost job of the script +11.5 %; see qualityday.go)
//	-fill-margin-pct FILL_MARGIN_PCT  the concurrent fill day may book at most this percentage fewer
//	                                labour minutes than the same demand replayed one customer at a
//	                                time on a fresh day (default 10, see dayphases.go)
//	-customers      CUSTOMERS       signed-in customer accounts of the customers phase (default 40; kept
//	                                within the per-IP write budget a single load generator gets)
//	-slo-book-ms / -slo-read-ms  SLO_BOOK_MS / SLO_READ_MS  p99 budgets the fill day's bookings and
//	                                availability reads must meet, each from its actual send; the first
//	                                read from the intended arrival (open loop) gets SLO_READ_MS too
//	                                (default 1000 / 500)
//	-slo-hammer-ms  SLO_HAMMER_MS   p99 budget of a hammered booking end to end, retries included
//	                                (default 3000: about 3× the p99 measured on a laptop for 1000
//	                                requests × 5 slots per phase (~0.9 s, the day gate queueing the
//	                                losers); a gate or lock that stalls requests for seconds fails it)
//	-slo-side-ms    SLO_SIDE_MS     p99 budget of the dispatcher reads during the hammer (default 1000)
//	-slo-other-day-ms SLO_OTHER_DAY_MS  p99 budget of the uncontended bookings sent to another date of
//	                                the hammered dealership during each hammer phase, one every 100 ms
//	                                (default 1000, see otherday.go)
//	-pool-wait-budget-ms POOL_WAIT_BUDGET_MS  most Σ empty-acquire wait the cluster's DB pools may
//	                                add during one hammer phase (default 2000; see pool.go)
//	-pool-cap-fails POOL_CAP_FAILS  a backend at its DB pool cap during a hammer fails Q3 (default
//	                                true; false only reports it, for a shared CI runner, see pool.go)
//	-require        STRESS_REQUIRE  comma list of what must PASS (default none): a requirement id
//	                                (R1 … Q7: a row left SKIP fails the run), id/source for one named
//	                                check (Q4/ws order: skipped or not run fails the run), or all
//	                                (every row plus the essential checks, requirements.go)
//	-chaos-restart  STRESS_CHAOS_RESTART  a backend is restarted during the hammer on purpose
//	                                (cluster-checks chaos): replays the lost requests after the
//	                                restart, tolerates 5xx/transport errors on reads, skips the metrics
//	                                reconciliation (counters reset), excuses the restarted backend's
//	                                WS union; needs -mixed-ops 0 (default false)
//	-report-json    REPORT_JSON     write the requirements matrix and headline numbers as JSON
//	-summary-md     STRESS_SUMMARY_MD  append the compact matrix as a Markdown table (CI job summary)
//	-timeout        STRESS_TIMEOUT  whole-run deadline (default 30m; cleanup still runs after it)
//	-mode           STRESS_MODE     stress (default), soak (a steady mixed load for -soak-duration:
//	                                queues, memory and goroutines must stay flat, see soak.go; flags
//	                                -soak-duration SOAK_DURATION 30m, -soak-rate SOAK_RATE 4 ops/s,
//	                                -soak-sample SOAK_SAMPLE 30s, -soak-rss-growth-pct
//	                                SOAK_RSS_GROWTH_PCT 25, -soak-queue-max SOAK_QUEUE_MAX 100), or
//	                                bench (capacity ramp, see bench.go):
//	-bench-workloads BENCH_WORKLOADS  read,book,hot,journey (always run as book, read, hot, journey)
//	-bench-read-rates / -bench-book-rates / -bench-hot-rates / -bench-journey-rates  offered rate per step
//	-bench-step     BENCH_STEP      length of one step (default 10s)
//	-bench-read-slo-ms / -bench-book-slo-ms / -bench-hot-slo-ms / -bench-journey-slo-ms  p99 budget per workload
//	-bench-days     BENCH_DAYS      days the "book" and "journey" workloads each spread over (default 150:
//	                                150 × 20 starts × 20 bays = 60000 bookings of room each)
//	                                (the journey books as signed-in customers: one load generator is
//	                                one IP, whose customer budget — 120 bookings / 600 reads a minute —
//	                                caps it, hence its low default steps 1,2,4,8,16 users/s)
//
// Phases (-phases): river and north hammer contended slots; truth fills one day of the stress
// workshop booking by booking against the oracle (oracle.go: raw rows + the documented rules,
// rules.go); fill plays a day of concurrent customers dispatched by staff, tops it up until it is
// full, and replays its demand one customer at a time (dayphases.go); quality plays a scripted day
// where greedy first fit loses, against every first-fit order and the repair (qualityphase.go);
// customers has many signed-in
// customer accounts book their own cars at once. Every 409/422 of them is judged against the end
// state (refusals.go).
// The run ends with a requirements matrix (requirements.go): every requirement of the brief with
// the evidence the run gathered, printed and (with -report-json) written whatever the outcome.
//
// Every booking of the run is cancelled at the end, pass or fail (Ctrl-C / SIGTERM included: the
// first signal stops the hammer and runs the cleanup, a second one kills the process), so repeated
// runs find free slots again.
//
// The report has two latency views: per booking end to end (first send to definite answer,
// retries and backoff included; bookings/s) and per attempt (one HTTP send; attempts/s).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/config"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func main() {
	m := newMatrix()
	s, err := parseSettings()
	if err == nil {
		err = run(s, m)
		if unmet := m.enforceRequired(s.require); len(unmet) > 0 {
			fmt.Fprintf(os.Stderr, "stress: required but never passed (only skipped or not run): %v\n", unmet)
		}
	}
	m.print()
	if jerr := m.writeJSON(s.reportJSON, err); jerr != nil {
		fmt.Fprintf(os.Stderr, "stress: report: %v\n", jerr)
	}
	if merr := m.appendMarkdown(s.summaryMD, s.title(), err); merr != nil {
		fmt.Fprintf(os.Stderr, "stress: summary: %v\n", merr)
	}
	failed := m.failed()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stress fail: %v\n", err)
	}
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "stress fail: requirements not met: %v\n", failed)
	}
	if err != nil || len(failed) > 0 {
		os.Exit(1)
	}
	fmt.Println("stress ok: every requirement the run covered is met")
}

type settings struct {
	base, backendA, backendB        string
	hammerURL                       string
	concurrency, vehicles           int
	slots, workers                  int
	sideWorkers, wsClients          int
	maxAttempts                     int
	maxUnresolvedPct                float64
	rate                            float64
	phases                          []string
	dsn                             string
	email, password                 string
	startMargin                     time.Duration
	idemRacers, idemKeys            int
	customerRequests                int
	customerEmail, customerPassword string
	mixedOps, minInstances          int
	fillRequests, fillWorkers       int
	fillRate, fillMarginPct         float64
	firstFitMarginPct               float64
	fitMinGainPct                   float64
	fitSeed                         uint64
	qualityMarginPct                float64
	customers                       int
	fillSeed                        uint64
	sloBookMS, sloReadMS            float64
	sloHammerMS, sloSideMS          float64
	sloOtherDayMS                   float64
	poolWaitBudgetMS                float64
	poolCapFails                    bool
	require                         []string
	chaosRestart                    bool
	reportJSON, summaryMD           string
	timeout                         time.Duration
	mode                            string
	bench                           benchConfig
	benchDays                       int
	soakDuration, soakSample        time.Duration
	soakRate                        float64
	soakRSSGrowthPct, soakQueueMax  float64
}

// title names the run in the Markdown summary.
func (s settings) title() string {
	t := fmt.Sprintf("Stress requirements matrix (%s: %s)", s.mode, strings.Join(s.phases, ","))
	if s.chaosRestart {
		t += ", a backend restarted mid-hammer"
	}
	return t
}

// step is the bench step length.
func (s settings) step() time.Duration { return s.bench.step }

// hasPhase reports whether name is one of the selected phases.
func (s settings) hasPhase(name string) bool { return slices.Contains(s.phases, name) }

// hammerPhases are the selected contended-slot phases (river, north), in order.
func (s settings) hammerPhases() []string {
	return slices.DeleteFunc(slices.Clone(s.phases), func(p string) bool { return p != "river" && p != "north" })
}

// viaGateway reports whether the hammer books through something other than one backend directly.
func (s settings) viaGateway() bool { return s.hammerURL != s.backendA && s.hammerURL != s.backendB }

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

// envDefaults reads the flags' defaults from the environment. An unset or empty variable gives
// the built-in default; one that does not parse (or is negative) is recorded and fails
// parseSettings instead of silently running with the default.
type envDefaults struct{ errs []error }

// lookup returns k's trimmed value and whether it is set to something.
func (e *envDefaults) lookup(k string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(k))
	return v, v != ""
}

func (e *envDefaults) bad(k, v, want string) {
	e.errs = append(e.errs, fmt.Errorf("%s=%q: want %s", k, v, want))
}

func (e *envDefaults) int(k string, def int) int {
	v, ok := e.lookup(k)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		e.bad(k, v, "a non-negative integer")
		return def
	}
	return n
}

func (e *envDefaults) uint64(k string, def uint64) uint64 {
	v, ok := e.lookup(k)
	if !ok {
		return def
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		e.bad(k, v, "a non-negative integer")
		return def
	}
	return n
}

func (e *envDefaults) duration(k string, def time.Duration) time.Duration {
	v, ok := e.lookup(k)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		e.bad(k, v, "a non-negative duration such as 90s or 12m")
		return def
	}
	return d
}

func (e *envDefaults) float(k string, def float64) float64 {
	v, ok := e.lookup(k)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		e.bad(k, v, "a non-negative number")
		return def
	}
	return f
}

// bool is true for 1/true/yes and false for 0/false/no (any case).
func (e *envDefaults) bool(k string) bool {
	v, ok := e.lookup(k)
	if !ok {
		return false
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	}
	e.bad(k, v, "1/true/yes or 0/false/no")
	return false
}

func parseSettings() (settings, error) {
	config.LoadDotEnv()
	var s settings
	var e envDefaults
	envInt, envUint64, envFloat, envDuration, envBool := e.int, e.uint64, e.float, e.duration, e.bool
	var phases string
	flag.StringVar(&s.base, "base-url", env("BASE_URL", "http://127.0.0.1:8080"), "gateway URL")
	flag.StringVar(&s.backendA, "backend-a", env("BACKEND_A_URL", "http://127.0.0.1:18081"), "backend-1 URL (WS)")
	flag.StringVar(&s.backendB, "backend-b", env("BACKEND_B_URL", "http://127.0.0.1:18082"), "backend-2 URL (WS)")
	flag.StringVar(&s.hammerURL, "hammer-url", env("HAMMER_URL", ""), "hammer bookings go here (default -base-url)")
	flag.IntVar(&s.concurrency, "concurrency", envInt("CONCURRENCY", 1000), "requests per slot")
	flag.IntVar(&s.vehicles, "vehicles", envInt("VEHICLES", 0), "distinct vehicles (0 = concurrency)")
	flag.IntVar(&s.slots, "slots", envInt("SLOTS", 5), "slots per phase")
	flag.IntVar(&s.workers, "workers", envInt("WORKERS", 0), "booking workers (0 = min(concurrency, 512))")
	flag.IntVar(&s.sideWorkers, "side-workers", envInt("SIDE_WORKERS", 16), "side probes")
	flag.IntVar(&s.wsClients, "ws-clients", envInt("WS_CLIENTS", 2), "WS listeners per backend")
	flag.IntVar(&s.maxAttempts, "max-attempts", envInt("MAX_ATTEMPTS", 6), "sends per booking incl. retries")
	flag.Float64Var(&s.maxUnresolvedPct, "max-unresolved-pct", envFloat("MAX_UNRESOLVED_PCT", 10), "max % of a phase's requests left unresolved")
	flag.Float64Var(&s.rate, "rate", envFloat("RATE", 0), "open loop: bookings/s per phase (0 = closed loop)")
	flag.StringVar(&phases, "phases", env("PHASES", defaultPhases), "phases to run")
	flag.StringVar(&s.dsn, "dsn", env("STRESS_DATABASE_URL", ""), "Postgres DSN for SQL integrity checks")
	flag.StringVar(&s.email, "email", env("STRESS_ADMIN_EMAIL", "admin@scheduler.local"), "admin email")
	flag.StringVar(&s.password, "password", env("STRESS_ADMIN_PASSWORD", "admin123"), "admin password")
	flag.DurationVar(&s.startMargin, "start-margin", envDuration("START_MARGIN", 15*time.Minute), "earliest slot start after a phase is prepared")
	flag.IntVar(&s.idemRacers, "idem-racers", envInt("IDEM_RACERS", 16), "concurrent sends of one Idempotency-Key (0 skips)")
	flag.IntVar(&s.idemKeys, "idem-keys", envInt("IDEM_KEYS", 4), "keys the idempotency race runs at once")
	flag.IntVar(&s.customerRequests, "customer-requests", envInt("CUSTOMER_REQUESTS", 40), "concurrent customer bookings (0 skips)")
	flag.StringVar(&s.customerEmail, "customer-email", env("STRESS_CUSTOMER_EMAIL", "customer@scheduler.local"), "customer email")
	flag.StringVar(&s.customerPassword, "customer-password", env("STRESS_CUSTOMER_PASSWORD", "customer123"), "customer password")
	flag.IntVar(&s.mixedOps, "mixed-ops", envInt("MIXED_OPS", 0), "bookings per phase mutated with If-Match during the hammer (0 = off)")
	flag.IntVar(&s.minInstances, "min-instances", envInt("MIN_INSTANCES", 2), "through the gateway: distinct instances that must give definite booking answers (0 disables)")
	flag.IntVar(&s.fillRequests, "fill-requests", envInt("FILL_REQUESTS", 150), "customers of the fill day")
	flag.IntVar(&s.customers, "customers", envInt("CUSTOMERS", 40), "signed-in customers of the customers phase")
	flag.IntVar(&s.fillWorkers, "fill-workers", envInt("FILL_WORKERS", 0), "fill day: customers in flight at most (0 = every customer: truly open loop)")
	flag.Float64Var(&s.fillRate, "fill-rate", envFloat("FILL_RATE", 500), "fill day: customers arriving per second (0 = closed loop)")
	flag.Float64Var(&s.fillMarginPct, "fill-margin-pct", envFloat("FILL_MARGIN_PCT", 10), "fill day: most % of labour minutes the concurrent day may book below the sequential replay")
	flag.Float64Var(&s.firstFitMarginPct, "firstfit-margin-pct", envFloat("FIRSTFIT_MARGIN_PCT", 0), "fill day: most % of labour minutes the server's replay may book below a naive first-fit replay (0: never fewer)")
	flag.Uint64Var(&s.fillSeed, "fill-seed", envUint64("FILL_SEED", 1), "fill demand seed")
	flag.Uint64Var(&s.fitSeed, "fit-seed", envUint64("FIT_SEED", 1), "fit days: demand seed")
	flag.Float64Var(&s.fitMinGainPct, "fit-min-gain-pct", envFloat("FIT_MIN_GAIN_PCT", 0), "fit days: least % more labour minutes than first fit in the median random resource order")
	flag.Float64Var(&s.qualityMarginPct, "quality-margin-pct", envFloat("QUALITY_MARGIN_PCT", defaultQualityMarginPct), "quality day: least % more labour minutes than the best first-fit replay")
	flag.Float64Var(&s.sloBookMS, "slo-book-ms", envFloat("SLO_BOOK_MS", 1000), "fill day: booking p99 budget (ms)")
	flag.Float64Var(&s.sloReadMS, "slo-read-ms", envFloat("SLO_READ_MS", 500), "fill day: availability p99 budget (ms)")
	flag.Float64Var(&s.sloHammerMS, "slo-hammer-ms", envFloat("SLO_HAMMER_MS", 3000), "hammer: booking p99 budget, end to end (ms)")
	flag.Float64Var(&s.sloSideMS, "slo-side-ms", envFloat("SLO_SIDE_MS", 1000), "hammer: side-read p99 budget (ms)")
	flag.Float64Var(&s.sloOtherDayMS, "slo-other-day-ms", envFloat("SLO_OTHER_DAY_MS", 1000), "hammer: p99 budget of bookings on another day of the hammered dealership (ms)")
	flag.Float64Var(&s.poolWaitBudgetMS, "pool-wait-budget-ms", envFloat("POOL_WAIT_BUDGET_MS", 2000), "hammer: most Σ empty-acquire wait per phase (ms)")
	flag.BoolVar(&s.poolCapFails, "pool-cap-fails", env("POOL_CAP_FAILS", "true") != "false", "hammer: a backend at its DB pool cap fails Q3 (false: noted only)")
	var require string
	flag.StringVar(&require, "require", env("STRESS_REQUIRE", ""), "what must PASS, a SKIP fails: requirement ids, id/source (Q4/ws order), or all")
	flag.BoolVar(&s.chaosRestart, "chaos-restart", envBool("STRESS_CHAOS_RESTART"), "a backend is restarted mid-hammer on purpose")
	flag.StringVar(&s.reportJSON, "report-json", env("REPORT_JSON", ""), "write the requirements matrix as JSON here")
	flag.DurationVar(&s.timeout, "timeout", envDuration("STRESS_TIMEOUT", 30*time.Minute), "whole-run deadline")
	flag.StringVar(&s.mode, "mode", env("STRESS_MODE", "stress"), "stress, bench or soak")
	flag.DurationVar(&s.soakDuration, "soak-duration", envDuration("SOAK_DURATION", 30*time.Minute), "soak: how long the mixed load runs")
	flag.DurationVar(&s.soakSample, "soak-sample", envDuration("SOAK_SAMPLE", 30*time.Second), "soak: scrape interval")
	flag.Float64Var(&s.soakRate, "soak-rate", envFloat("SOAK_RATE", 4), "soak: operations per second")
	flag.Float64Var(&s.soakRSSGrowthPct, "soak-rss-growth-pct", envFloat("SOAK_RSS_GROWTH_PCT", 25), "soak: most resident-memory growth per backend (%)")
	flag.Float64Var(&s.soakQueueMax, "soak-queue-max", envFloat("SOAK_QUEUE_MAX", 100), "soak: most outbox + notification rows pending in the last third")
	var workloads, readRates, bookRates, hotRates, journeyRates string
	var readSLO, bookSLO, hotSLO, journeySLO float64
	flag.StringVar(&workloads, "bench-workloads", env("BENCH_WORKLOADS", "read,book,hot,journey"), "bench workloads")
	flag.StringVar(&journeyRates, "bench-journey-rates", env("BENCH_JOURNEY_RATES", "1,2,4,8,16"), "bench journey steps (users/s; one IP's customer budget caps it)")
	flag.Float64Var(&journeySLO, "bench-journey-slo-ms", envFloat("BENCH_JOURNEY_SLO_MS", 750), "bench journey p99 budget (ms, both requests)")
	flag.StringVar(&readRates, "bench-read-rates", env("BENCH_READ_RATES", "100,400,1600,3200,6400"), "bench read steps (req/s)")
	flag.StringVar(&bookRates, "bench-book-rates", env("BENCH_BOOK_RATES", "50,200,400,800,1600"), "bench book steps (req/s)")
	flag.StringVar(&hotRates, "bench-hot-rates", env("BENCH_HOT_RATES", "100,400,800,1600"), "bench hot steps (req/s)")
	flag.DurationVar(&s.bench.step, "bench-step", envDuration("BENCH_STEP", 10*time.Second), "bench step length")
	flag.Float64Var(&readSLO, "bench-read-slo-ms", envFloat("BENCH_READ_SLO_MS", 250), "bench read p99 budget (ms)")
	flag.Float64Var(&bookSLO, "bench-book-slo-ms", envFloat("BENCH_BOOK_SLO_MS", 500), "bench book p99 budget (ms)")
	flag.Float64Var(&hotSLO, "bench-hot-slo-ms", envFloat("BENCH_HOT_SLO_MS", 1000), "bench hot p99 budget (ms)")
	flag.IntVar(&s.benchDays, "bench-days", envInt("BENCH_DAYS", 150), "days the book and journey workloads each spread over")
	flag.StringVar(&s.summaryMD, "summary-md", env("STRESS_SUMMARY_MD", ""), "append the compact requirements matrix as Markdown here (e.g. $GITHUB_STEP_SUMMARY)")
	flag.Parse()
	if len(e.errs) > 0 {
		return s, fmt.Errorf("environment: %w", errors.Join(e.errs...))
	}
	s.bench.workloads = splitList(workloads)
	s.bench.sloMS = map[string]float64{"read": readSLO, "book": bookSLO, "hot": hotSLO, "journey": journeySLO}
	s.bench.rates = map[string][]float64{}
	for name, raw := range map[string]string{"read": readRates, "book": bookRates, "hot": hotRates, "journey": journeyRates} {
		rates, err := parseRates(raw)
		if err != nil {
			return s, fmt.Errorf("-bench-%s-rates: %w", name, err)
		}
		s.bench.rates[name] = rates
	}
	for p := range strings.SplitSeq(phases, ",") {
		if p = strings.TrimSpace(p); p != "" {
			s.phases = append(s.phases, p)
		}
	}
	s.require = splitList(require)
	return s, s.normalize()
}

// knownPhases are the phases -phases accepts; defaultPhases runs them all (they always run in a
// fixed order: the hammer phases, storm, truth, fill, customers, quality).
var knownPhases = []string{"river", "north", "storm", "truth", "fill", "quality", "customers"}

const defaultPhases = "river,north,storm,truth,fill,quality,customers"

// maxFillRequests bounds the fill day's demand (and so the cars provisioned for it).
const maxFillRequests = 5000

// normalize applies the defaults that depend on other settings and rejects impossible ones.
func (s *settings) normalize() error {
	s.base = strings.TrimRight(s.base, "/")
	s.backendA = strings.TrimRight(s.backendA, "/")
	s.backendB = strings.TrimRight(s.backendB, "/")
	if s.hammerURL = strings.TrimRight(s.hammerURL, "/"); s.hammerURL == "" {
		s.hammerURL = s.base
	}
	s.concurrency = max(1, s.concurrency)
	if s.vehicles <= 0 {
		s.vehicles = s.concurrency
	}
	if s.workers <= 0 {
		s.workers = min(s.concurrency, 512)
	}
	s.slots = max(1, s.slots)
	s.sideWorkers = max(1, s.sideWorkers)
	s.wsClients = max(0, s.wsClients)
	if s.wsClients > hubMaxSocketsPerUser {
		return fmt.Errorf("-ws-clients %d: at most %d per backend (the hub's per-user socket cap; every listener is the admin user)",
			s.wsClients, hubMaxSocketsPerUser)
	}
	s.idemRacers = max(0, s.idemRacers)
	s.idemKeys = max(1, s.idemKeys)
	var err error
	if s.require, err = parseRequire(s.require); err != nil {
		return err
	}
	if s.chaosRestart && s.mixedOps > 0 {
		return fmt.Errorf("-chaos-restart needs -mixed-ops 0: the post-restart replay may refill room a mutation freed")
	}
	if s.fillRate < 0 || s.fillMarginPct < 0 {
		return fmt.Errorf("-fill-rate and -fill-margin-pct must be >= 0")
	}
	s.customerRequests = max(0, s.customerRequests)
	s.mixedOps = max(0, s.mixedOps)
	s.minInstances = max(0, s.minInstances)
	s.maxAttempts = max(1, s.maxAttempts)
	s.maxUnresolvedPct = max(0, s.maxUnresolvedPct)
	if s.rate < 0 {
		return fmt.Errorf("-rate must be >= 0, got %v", s.rate)
	}
	if len(s.phases) == 0 {
		return fmt.Errorf("-phases is empty (want %s)", strings.Join(knownPhases, ", "))
	}
	for _, p := range s.phases {
		if !slices.Contains(knownPhases, p) {
			return fmt.Errorf("-phases: unknown phase %q (want %s)", p, strings.Join(knownPhases, ", "))
		}
	}
	s.fillRequests = max(1, s.fillRequests)
	if s.fillRequests > maxFillRequests {
		// Every customer gets a car of its own, provisioned through the API one by one.
		return fmt.Errorf("-fill-requests %d: at most %d (each fill customer is provisioned a car of its own)", s.fillRequests, maxFillRequests)
	}
	if s.fillWorkers <= 0 {
		// Truly open loop: no customer ever waits for the load generator, so a latency counted
		// from the intended arrival is the server's alone (no coordinated omission).
		s.fillWorkers = s.fillRequests
	}
	s.customers = max(2, s.customers)
	if s.mode == "" {
		s.mode = "stress"
	}
	if s.mode != "stress" && s.mode != "bench" && s.mode != "soak" {
		return fmt.Errorf("-mode %q: want stress, bench or soak", s.mode)
	}
	if s.mode == "soak" && (s.soakRate <= 0 || s.soakDuration <= 0 || s.soakSample <= 0) {
		return fmt.Errorf("-soak-rate, -soak-duration and -soak-sample must be > 0")
	}
	for _, w := range s.bench.workloads {
		if w != "read" && w != "book" && w != "hot" && w != "journey" {
			return fmt.Errorf("-bench-workloads: unknown workload %q (want read, book, hot, journey)", w)
		}
	}
	if s.bench.step <= 0 {
		s.bench.step = 10 * time.Second
	}
	s.benchDays = max(1, s.benchDays)
	return nil
}

// splitList splits a comma list, trimming and dropping empty items.
func splitList(raw string) []string {
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

type catalog struct {
	dealers  []domain.Dealership
	services []domain.ServiceType
}

func loadCatalog(ctx context.Context, api *apiClient) (catalog, error) {
	var c catalog
	var err error
	if c.dealers, err = getAll[domain.Dealership](ctx, api, "/api/v1/dealerships", maxListLimit, nil); err != nil {
		return c, err
	}
	if c.services, err = getAll[domain.ServiceType](ctx, api, "/api/v1/service-types", maxListLimit, nil); err != nil {
		return c, err
	}
	return c, nil
}

// cleanup releases every booking the run made, so repeated runs find free slots again: the ids
// clients were told about, plus (with a DSN) the run's still-holding rows that committed while
// their request stayed unresolved, found by the run's Idempotency-Key prefix.
func cleanup(ctx context.Context, api *apiClient, dsn, runID string, created []uuid.UUID) error {
	ids := map[uuid.UUID]bool{}
	for _, id := range created {
		ids[id] = true
	}
	if dsn != "" {
		orphans, err := runHoldingIDs(ctx, dsn, runID)
		if err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
		extra := 0
		for _, id := range orphans {
			if !ids[id] {
				ids[id] = true
				extra++
			}
		}
		if extra > 0 {
			fmt.Printf("cleanup: also cancelling %d committed booking(s) whose request stayed unresolved\n", extra)
		}
	}
	return cancelAll(ctx, api, ids)
}

// cancelAll cancels every id through the API (the admin may cancel any confirmed/checked-in job).
// A cancel of an already cancelled booking replays 200, so ids the mixed phase cancelled pass too;
// a refused cancel is fine when the booking no longer holds anything. One failure does not stop
// the rest: every id is tried and the failures come back joined.
func cancelAll(ctx context.Context, api *apiClient, ids map[uuid.UUID]bool) error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, 32)
	for id := range ids {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := cancelOne(ctx, api, id); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(errs) > 0 {
		return fmt.Errorf("cleanup: %d of %d cancels failed: %w", len(errs), len(ids), errors.Join(firstErrs(errs, 5)...))
	}
	return nil
}

// cancelOne cancels one booking; a refused cancel is fine when it no longer holds anything.
func cancelOne(ctx context.Context, api *apiClient, id uuid.UUID) error {
	path := "/api/v1/appointments/" + id.String()
	resp, definite := sendUntilDefinite(ctx, api, http.MethodPost, path+"/cancel", nil, nil, probeAttempts)
	if definite && resp.code == http.StatusOK {
		return nil
	}
	if now, err := getJSON[domain.Appointment](ctx, api, "GET", path, nil); err == nil && !now.Status.HoldsResources() {
		return nil
	}
	return fmt.Errorf("cancel %s: %d %s", id, resp.code, truncate(string(resp.body), 80))
}

func firstErrs(errs []error, n int) []error { return errs[:min(len(errs), n)] }

type sideStats struct {
	ok, fiveXX, transport, bad atomic.Int64
	mu                         sync.Mutex
	lat                        []float64 // ms per probe
}

func (s *sideStats) observe(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lat = append(s.lat, float64(d)/float64(time.Millisecond))
}

// latency summarizes the probes' latency.
func (s *sideStats) latency() latencySummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return summarize(slices.Clone(s.lat))
}
