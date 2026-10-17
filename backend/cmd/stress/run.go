package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// session is what every part of a run shares.
type session struct {
	s       settings
	api     *apiClient   // through the gateway
	direct  []*apiClient // backend A, backend B (same admin session)
	conn    *pgx.Conn    // nil without -dsn
	owner   uuid.UUID    // customer owning the stress vehicles
	catalog catalog
	runID   string
	m       *matrix
	// authLost aborts the run when one of its signed-in sessions is lost for good (the cause).
	authLost func(error)
}

func run(s settings, m *matrix) (err error) {
	// Ctrl-C / SIGTERM cancels ctx: the phases stop, and the deferred cleanups still run (on a
	// context detached from ctx). stop() restores the default handling, so a second signal during
	// the cleanup kills the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	// A signed-in session the client cannot renew aborts the run with that cause (authclient.go):
	// its requests would otherwise look like unanswered bookings.
	ctx, abort := context.WithCancelCause(ctx)
	defer abort(nil)

	ss := &session{s: s, m: m, api: newAPI(s.base), runID: "stress:" + uuid.NewString()[:8], authLost: abort}
	if err := ss.api.login(ctx, s.email, s.password, abort); err != nil {
		return err
	}
	started := time.Now()
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		recordSession(rctx, m, ss.api.auth, time.Since(started))
	}()
	for _, base := range []string{s.backendA, s.backendB} {
		ss.direct = append(ss.direct, ss.api.asUser(base))
	}
	if s.dsn != "" {
		if ss.conn, err = pgx.Connect(ctx, s.dsn); err != nil {
			return fmt.Errorf("connect %s: %w", "STRESS_DATABASE_URL", err)
		}
		defer ss.conn.Close(context.WithoutCancel(ctx))
	}
	if ss.catalog, err = loadCatalog(ctx, ss.api); err != nil {
		return err
	}
	if ss.owner, err = ensureStressOwner(ctx, s.base, abort); err != nil {
		return err
	}
	switch s.mode {
	case "bench":
		return ss.bench(ctx, stop)
	case "soak":
		return ss.soak(ctx, stop)
	}
	return ss.stress(ctx, stop)
}

// dayMixes are the job mixes of the truth and fill days, with their demand weights.
func (ss *session) dayMixes() ([]jobMix, []int, error) {
	specs := []struct {
		name   string
		codes  []string
		weight int
	}{
		{"MAJOR", []string{"MAJOR"}, 15},
		{"OIL+BRAKE", []string{"OIL", "BRAKE"}, 20},
		{"BRAKE", []string{"BRAKE"}, 30},
		{"OIL", []string{"OIL"}, 35},
	}
	var mixes []jobMix
	var weights []int
	for _, sp := range specs {
		mix, err := newJobMix(ss.catalog, sp.name, sp.codes...)
		if err != nil {
			return nil, nil, err
		}
		mixes = append(mixes, mix)
		weights = append(weights, sp.weight)
	}
	return mixes, weights, nil
}

// resetDay cancels whatever still holds resources at dealer on day (left by an interrupted run).
func (ss *session) resetDay(ctx context.Context, w workshop, from, to time.Time) error {
	rows, err := ss.conn.Query(ctx, `
		SELECT id FROM appointments WHERE dealership_id = $1 AND status IN (`+notStartedSQL+`)
		  AND start_at >= $2 AND start_at < $3`, w.dealer.ID, from, to)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil || len(ids) == 0 {
		return err
	}
	fmt.Printf("%s: cancelling %d booking(s) left by an earlier run\n", w.dealer.Name, len(ids))
	set := map[uuid.UUID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return cancelAll(ctx, ss.api, set)
}

// stress runs the selected phases, then every cross-check, and fills the matrix.
func (ss *session) stress(ctx context.Context, stop func()) (err error) {
	s, m := ss.s, ss.m
	backends := []string{s.backendA, s.backendB}
	metricsBefore, metricsErr := clusterScrape(ctx, backends...)

	stats := newRunStats()
	mix := newMixStats()
	ws := newWSWatch(ctx, ss.api.auth)
	ws.chaos = s.chaosRestart
	var (
		side       sideStats
		plans      []*plan
		extraIDs   []uuid.UUID // bookings of the idempotency, customer, truth and fill scenarios
		mixWG      sync.WaitGroup
		hammerWall time.Duration
		mixes      = map[string]bool{}
		river      domain.Dealership
	)
	defer func() {
		interrupted := ctx.Err() != nil
		stop()
		if interrupted {
			fmt.Fprintln(os.Stderr, "stress: interrupted or timed out, cancelling this run's bookings (signal again to abort)")
		}
		mixWG.Wait()
		stats.report(hammerWall)
		ids := slices.Clone(extraIDs)
		for _, p := range plans {
			ids = append(ids, p.createdIDs()...)
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		if cerr := cleanup(cctx, ss.api, s.dsn, ss.runID, ids); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()

	var pool []uuid.UUID
	if hammer := s.hammerPhases(); len(hammer) > 0 || s.idemRacers > 0 || s.customerRequests > 0 || s.hasPhase("storm") {
		// The river shop is also where the idempotency race and the customer burst book when no
		// hammer phase was selected.
		w, err := ensureWorkshop(ctx, ss.api, riverWorkshop)
		if err != nil {
			return err
		}
		river = w.dealer
		cars := s.idemKeys // the idempotency race alone needs one car per key
		if len(hammer) > 0 {
			cars = max(s.vehicles, s.idemKeys)
		}
		if s.hasPhase("storm") {
			cars = max(cars, stormCars)
		}
		if pool, err = ensureVehicles(ctx, ss.api, ss.owner, vinPhases, cars, s.workers); err != nil {
			return err
		}
		if len(hammer) > 0 {
			first, err := preparePhase(ctx, ss.api, ss.querier(), ss.catalog, hammer[0], pool, s, river)
			if err != nil {
				return fmt.Errorf("phase %s: %w", hammer[0], err)
			}
			plans = append(plans, first)
		}
	}

	// WS listeners on both backends must be connected (server CONNECTED frame) before the
	// first booking, or a miss would be a test artefact rather than a fan-out bug.
	if s.wsClients > 0 {
		if err := ws.start(s.backendA, s.backendB, s.wsClients, 20*time.Second); err != nil {
			ws.stop()
			return err
		}
	}
	defer ws.stop()

	sideDone := make(chan struct{})
	var sideWG sync.WaitGroup
	if len(plans) > 0 {
		probes := sideProbes(plans[0], pool[0])
		for w := range s.sideWorkers {
			// Each worker starts at another probe, so even a short phase reads every endpoint.
			sideWG.Go(func() { probeSide(ctx, ss.api, probes, w, &side, sideDone) })
		}
	}
	stopSide := sync.OnceFunc(func() { close(sideDone); sideWG.Wait() })
	defer stopSide()

	mode := "closed loop"
	if s.rate > 0 {
		mode = fmt.Sprintf("open loop %.0f bookings/s", s.rate)
	}
	for i, name := range s.hammerPhases() {
		p := plans[0]
		if i > 0 {
			if p, err = preparePhase(ctx, ss.api, ss.querier(), ss.catalog, name, pool, s, river); err != nil {
				return fmt.Errorf("phase %s: %w", name, err)
			}
			plans = append(plans, p)
		}
		fmt.Printf("phase %s start: %s at %s, %d slots × %d requests, vehicles=%d workers=%d, %s\n",
			p.name, p.dealer.Name, p.date, len(p.slots), s.concurrency, len(pool), s.workers, mode)
		if s.mixedOps > 0 {
			p.slotDone = make(chan int, len(p.slots))
			mixWG.Go(func() { mixPhase(ctx, ss.api, p, s.mixedOps, mix, pool[len(pool)-1], ss.runID+":burst-"+p.name) })
		}
		poolBefore, poolErr := clusterScrape(ctx, backends...)
		watch := startPoolWatch(ctx, backends)
		var other *otherDayWriter
		if !s.chaosRestart { // a killed backend would refuse the other day too: nothing to judge
			other = startOtherDay(ctx, ss.api, p, pool, ss.runID+":otherday-"+p.name, s.maxAttempts)
		}
		t0 := time.Now()
		hammer(ctx, ss.hammerAPI(), p, pool, s, ss.runID, stats)
		hammerWall += time.Since(t0)
		samples := watch.stop()
		ss.recordPool(ctx, p.name, poolBefore, poolErr, samples)
		if other != nil {
			for _, id := range other.stop() {
				p.addExtra(id)
			}
			stats.unresolved.Add(int64(other.unresolved()))
			ok, detail := other.verdict(p.name, s.sloOtherDayMS)
			m.check("Q3", "other day "+p.name, ok, "%s", detail)
		}
	}
	mixWG.Wait()
	stopSide()
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted: %w", context.Cause(ctx))
	}
	if s.chaosRestart && len(plans) > 0 {
		if err := ss.afterRestart(ctx, plans, stats); err != nil {
			return err
		}
	}
	created := map[uuid.UUID]bool{}
	ss.recordHammer(ctx, plans, stats, mix, created, mixes)

	unresolved := stats.unresolved.Load()
	if s.idemRacers > 0 {
		ids, err := ss.idempotency(ctx, plans, pool, river)
		extraIDs = append(extraIDs, ids...)
		for _, id := range ids {
			created[id] = true
		}
		if err != nil && errors.Is(err, context.Canceled) {
			return err
		}
	}
	if s.customerRequests > 0 {
		target := plans
		if len(target) == 0 {
			target = []*plan{{dealer: river, jobs: [][]uuid.UUID{ss.oilJob()}}}
		}
		ids, custUnresolved, err := customerBurst(ctx, s, target[0], ss.runID, ss.authLost)
		extraIDs = append(extraIDs, ids...)
		for _, id := range ids {
			created[id] = true
		}
		unresolved += int64(custUnresolved)
		m.check("Q1", "customer", err == nil, "customer self-service burst: lead time refused (422), at most one booking per own vehicle, throttle answers 429%s", errSuffix(err))
	}

	if s.hasPhase("storm") {
		from := ""
		if len(plans) > 0 && plans[0].name == "river" {
			if from, err = nextDate(plans[0].date); err != nil {
				return err
			}
		}
		// Cars from the middle of the pool: the other-day writer books the first ones, the burst
		// the last one.
		st, err := ss.storm(ctx, river, ss.oilJob(), pool[len(pool)/2:len(pool)-1], from)
		extraIDs = append(extraIDs, st.ids...)
		for _, id := range st.ids {
			created[id] = true
		}
		unresolved += int64(st.unresolved)
		if err != nil {
			m.fail("Q1", "storm", "%v", err)
		} else {
			st.record(m)
		}
	}

	if s.hasPhase("truth") || s.hasPhase("fill") || s.hasPhase("customers") {
		if ss.conn == nil {
			m.skip("R2b", "truth/fill", "STRESS_DATABASE_URL not set: the oracle needs Postgres")
			m.skip("Q7", "customers", "STRESS_DATABASE_URL not set: the customer accounts are provisioned in Postgres")
		} else {
			ids, n, err := ss.dayPhases(ctx, mixes)
			extraIDs = append(extraIDs, ids...)
			for _, id := range ids {
				created[id] = true
			}
			unresolved += int64(n)
			if err != nil {
				return err
			}
		}
	}

	if s.hasPhase("quality") {
		if ss.conn == nil {
			for _, check := range []string{"beats first fit", "repair"} {
				m.skip("Q5", check, "STRESS_DATABASE_URL not set: the quality day attributes repairs and reads the oracle's shop from Postgres")
			}
		} else {
			ids, err := ss.qualityDay(ctx, mixes)
			extraIDs = append(extraIDs, ids...)
			for _, id := range ids {
				created[id] = true
			}
			if err != nil {
				return fmt.Errorf("phase quality: %w", err)
			}
		}
	}

	if ss.conn == nil {
		for _, id := range []string{"R2a", "R3", "Q4", "Q6"} {
			m.skip(id, "sql", "STRESS_DATABASE_URL not set — SQL integrity, outbox, and metrics reconciliation skipped")
		}
	} else {
		if err := ss.sqlChecks(ctx, created, unresolved, slices.Sorted(maps.Keys(mixes))); err != nil {
			return err
		}
		switch {
		case s.chaosRestart:
			m.skip("Q6", "metrics", "-chaos-restart: a backend was restarted mid-run, its counters reset, so the metrics cannot be reconciled with the rows")
		case metricsErr != nil:
			m.fail("Q6", "metrics", "scrape before the run: %v", metricsErr)
		default:
			if err := checkMetrics(ctx, m, ss.conn, metricsBefore, backends, ss.runID); err != nil {
				return err
			}
		}
	}
	ss.recordWS(ctx, ws, created)
	if len(plans) > 0 {
		ss.recordSide(&side)
	}
	if s.viaGateway() && s.minInstances > 0 && len(plans) > 0 {
		err := instanceShortfall(stats.answeredBy(), s.minInstances)
		m.check("Q2", "gateway", err == nil, "definite booking answers per instance: %s%s", instanceSummary(stats.answeredBy()), errSuffix(err))
	}
	return nil
}

// querier is the run's Postgres connection as a querier (nil without -dsn, never a typed nil).
func (ss *session) querier() querier {
	if ss.conn == nil {
		return nil
	}
	return ss.conn
}

// hammerAPI is the client the hammer books through: the gateway, or -hammer-url with the same
// admin session.
func (ss *session) hammerAPI() *apiClient {
	if ss.s.hammerURL == ss.s.base {
		return ss.api
	}
	return ss.api.asUser(ss.s.hammerURL)
}

// oilJob is the catalog's oil change as a one-line job (nil when the catalog lacks it).
func (ss *session) oilJob() []uuid.UUID {
	id, err := serviceByCode(ss.catalog, "OIL")
	if err != nil {
		return nil
	}
	return []uuid.UUID{id}
}

// idempotency runs the cross-instance idempotency race on the first hammer phase's dealership and
// job (or the river shop's oil change when no hammer phase ran) and records it.
func (ss *session) idempotency(ctx context.Context, plans []*plan, pool []uuid.UUID, river domain.Dealership) ([]uuid.UUID, error) {
	s, m := ss.s, ss.m
	tgt := idemTarget{dealer: river.ID, job: ss.oilJob(), vehicles: pool}
	if len(plans) > 0 {
		tgt.dealer, tgt.job = plans[0].dealer.ID, plans[0].jobs[0]
	}
	apis := []*apiClient{ss.api}
	where := "through the gateway"
	if s.backendA != "" && s.backendB != "" && s.backendA != s.backendB {
		apis = ss.direct
		where = "half straight to backend A, half to backend B"
	}
	q := ss.querier()
	res, err := idempotencyRace(ctx, apis, q, tgt, s.idemKeys, s.idemRacers, ss.runID, s)
	rows := ""
	if q != nil {
		rows = ", one row per key in Postgres"
	}
	m.check("Q1", "idempotency", err == nil,
		"%d keys × %d concurrent sends of one Idempotency-Key + payload (%s) → one appointment per key (answers agree%s); definite answers by instance: %s; same key, other payload → 409%s",
		s.idemKeys, s.idemRacers, where, rows, res, errSuffix(err))
	if err == nil && res.crossed {
		m.pass("Q2", "idempotency", "one Idempotency-Key raced on both instances at once still makes one appointment (%s)", res)
	}
	return res.ids, err
}
