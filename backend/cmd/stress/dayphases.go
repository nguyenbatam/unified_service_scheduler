package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// The day phases (truth, fill, customers) run on the stress workshop, each on a day of its own
// (today+2, +3, +4; the fill day's sequential replay on +10, the same weekday as the fill day so
// the weekly roster matches). Every day is reset first (bookings an interrupted run left are
// cancelled) and given the shop's dated absences, so each phase sees the same roster.
//
// The fill day's allocation-quality verdicts (Q5), beyond rule conformance:
//
//   - capacity bound (quality.go): booked/bound for the day's demand, and for the full day (after
//     the top-up) against the shop's capacity for every job mix offered, reported as INFO (GiST and
//     the SQL pass already keep a day under them; exceeding one fails, as the bound would be wrong);
//   - sequential replay: the same demand, the same customers in the same order, replayed one
//     customer at a time (no contention: every customer books the start nearest its wish that is
//     free when it asks) on a fresh day with the same roster. The concurrent day may book at most
//     -fill-margin-pct (default 10 %) fewer labour minutes. Concurrency changes only the order in
//     which customers are served and costs the customers who lose fillTries offered starts in a row;
//     both greedy days end up near the shop's capacity (the demand is about twice what a day
//     holds), so the gap is fragmentation, a few jobs' worth. 10 % of a day (~2.5 MAJORs on the
//     stress workshop) is well above what contention costs a correct allocator and well below what
//     a broken one loses (e.g. starts refused while free, or jobs parked on the wrong resources);
//     the concurrent day may also book more (a different order can pack better), which passes;
//   - first fit (firstfit.go): the server's sequential replay books no fewer labour minutes than a
//     naive first-fit allocator replaying the demand offline in the workshop's listed resource
//     order (-firstfit-margin-pct, default 0). The demand is ~4.6× what the day holds, so every
//     greedy allocator saturates it and the gain is ~0 here by construction; the fit days
//     (fitday.go), sized to what the shop holds, measure it.

// prepareDay resets day at w and adds the shop's dated absences for it.
func (ss *session) prepareDay(ctx context.Context, w workshop, day booking.Date) error {
	if err := ss.resetDay(ctx, w, day.Start(w.loc), day.Next(w.loc)); err != nil {
		return err
	}
	return ensureDayRoster(ctx, ss.api, w, day)
}

// dayPhases runs truth, fill, and customers on the stress workshop and returns their bookings and
// the requests left unresolved.
func (ss *session) dayPhases(ctx context.Context, mixes map[string]bool) ([]uuid.UUID, int, error) {
	s, m := ss.s, ss.m
	w, err := ensureWorkshop(ctx, ss.api, fillWorkshop)
	if err != nil {
		return nil, 0, err
	}
	clearUpcoming := func(ctx context.Context) error {
		return ss.resetDay(ctx, w, time.Now(), time.Now().AddDate(1, 0, 1))
	}
	if err := ensureShifts(ctx, ss.api, w, clearUpcoming); err != nil {
		return nil, 0, err
	}
	dayMixes, weights, err := ss.dayMixes()
	if err != nil {
		return nil, 0, err
	}
	for _, mx := range dayMixes {
		mixes[mixKey(mx.ids)] = true
	}
	// One car per fill customer and per top-up customer (a car reused the same day would be busy:
	// a refusal nobody could judge), at least dayFleet for truth's probes.
	cars := dayFleet
	if s.hasPhase("fill") {
		cars = max(cars, s.fillRequests+2*len(dayMixes)*topUpMaxRounds)
	}
	fleet, err := ensureVehicles(ctx, ss.api, ss.owner, vinDay, cars, s.workers)
	if err != nil {
		return nil, 0, err
	}
	today, err := booking.ParseDate(w.dealer.Today)
	if err != nil {
		return nil, 0, err
	}
	var ids []uuid.UUID
	unresolved := 0
	if s.hasPhase("truth") {
		day := today.AddDays(2)
		if err := ss.prepareDay(ctx, w, day); err != nil {
			return ids, unresolved, err
		}
		fmt.Printf("phase truth start: %s on %s, %d job mixes, availability on %s, bookings on %s\n", w.dealer.Name, day, len(dayMixes), s.backendA, s.backendB)
		st, err := runTruth(ctx, ss.conn, ss.direct[0], ss.direct[1], w, dayMixes, fleet, day, ss.runID)
		if st != nil {
			ids = append(ids, st.ids...)
			st.record(m, day)
		}
		if err != nil {
			return ids, unresolved, err
		}
	}
	if s.hasPhase("fill") {
		fillIDs, n, err := ss.fillDay(ctx, w, today.AddDays(3), dayMixes, weights, fleet)
		ids = append(ids, fillIDs...)
		unresolved += n
		if err != nil {
			return ids, unresolved, err
		}
		// The fit days (fitday.go): demand about what the shop holds, where placement matters.
		fitIDs, n, err := ss.fitDays(ctx, w, []booking.Date{today.AddDays(11), today.AddDays(12), today.AddDays(13)}, dayMixes, weights, fleet)
		ids = append(ids, fitIDs...)
		unresolved += n
		if err != nil {
			return ids, unresolved, err
		}
	}
	if s.hasPhase("customers") {
		day := today.AddDays(4)
		if err := ss.prepareDay(ctx, w, day); err != nil {
			return ids, unresolved, err
		}
		customers, err := provisionCustomers(ctx, ss.conn, ss.api, s.base, s.customers, s.workers)
		if err != nil {
			// The phase was asked for and could not run: that is a failure, not a skip.
			m.fail("Q7", "customers", "customer accounts could not be provisioned: %v", err)
			return ids, unresolved, nil
		}
		fmt.Printf("phase customers start: %d signed-in customers book their own cars at %s on %s, all at once\n", len(customers), w.dealer.Name, day)
		st := runCustomers(ctx, customers, w, day, dayMixes, ss.runID)
		ids = append(ids, st.ids...)
		unresolved += st.unresolved
		if err := st.checkIsolation(ctx, ss.conn, customers, w, day, dayMixes[len(dayMixes)-1], 10, ss.runID); err != nil {
			return ids, unresolved, err
		}
		st.record(m, day)
	}
	return ids, unresolved, nil
}

// fillVerdict is everything the fill day is judged on.
type fillVerdict struct {
	main, top, seq            *fillStats
	started                   time.Time // when the demand began (refusals.go: holds' positions since)
	topRounds                 int
	mainMinutes, fullMinutes  int // labour minutes booked by the demand, and on the full day
	seqMinutes                int
	firstFitMinutes, firstFit int // the naive first-fit replay of the demand in the listed order (firstfit.go): minutes, customers booked
	firstFitSpread            orderSpread
	bound, boundBays, boundTk int            // capacity bound of the demand
	capBound                  int            // capacity bound of the full day (every job mix offered)
	refusals, topRefusals     refusalVerdict // the demand's (judged before the top-up) and the top-up's
	feasibleClasses           []string       // classes the oracle still finds a start for after the top-up
	classes                   int
	overQualified, quickLane  []string
	spill                     []string
	bayUse, techUse           map[string][2]int
}

// fillDay plays the fill day, tops it up, replays its demand sequentially, and records the
// verdicts. It returns the bookings made and the requests left unresolved.
func (ss *session) fillDay(ctx context.Context, w workshop, day booking.Date, mixes []jobMix, weights []int, fleet []uuid.UUID) ([]uuid.UUID, int, error) {
	s, m := ss.s, ss.m
	if err := ss.prepareDay(ctx, w, day); err != nil {
		return nil, 0, err
	}
	demand := fillDemand(s.fillSeed, s.fillRequests, w, day, mixes, weights, fleet)
	mode := fmt.Sprintf("%.0f arriving per second (open loop, at most %d in flight)", s.fillRate, s.fillWorkers)
	if s.fillRate <= 0 {
		mode = fmt.Sprintf("%d at a time (closed loop)", s.fillWorkers)
	}
	fmt.Printf("phase fill start: %s on %s, %d customers, %s, through %s\n", w.dealer.Name, day, len(demand), mode, s.base)
	v := &fillVerdict{started: time.Now()}
	v.main = runFill(ctx, ss.api, w, day, demand, s.fillWorkers, s.fillRate, ss.runID+":fill")
	ids := slices.Clone(v.main.ids)
	unresolved := v.main.unresolved
	if err := queryOne(ctx, ss.conn, bookedMinutesSQL, []any{ss.runID + ":fill:%"}, &v.mainMinutes); err != nil {
		return ids, unresolved, err
	}
	// Quality is judged right after the demand, before the top-up books what it left free (it would
	// fill exactly the junior technician or the general bay a misplaced job should have used, and
	// hide it). A resource counts as free only if no hold had it over the job's window at any time
	// since the demand began (heldSinceSQL): bookings only add, but a one-move repair can free a
	// resource that was busy when an earlier job was placed.
	if err := ss.judgeQuality(ctx, w, ss.runID+":fill:%", v); err != nil {
		return ids, unresolved, err
	}
	// The demand's refusals, also before the top-up, each against the holds when it was decided
	// (refusals.go): the top-up fills the day, which would justify almost any refusal.
	var err error
	if v.refusals, err = judgeRefusals(ctx, ss.conn, w.dealer.ID, v.main.refusals.list(), nil, v.started); err != nil {
		return ids, unresolved, err
	}
	// Top-up: offer every job mix until a round finds nothing offered at all.
	topStarted := time.Now()
	v.top, v.topRounds, err = runTopUp(ctx, ss.api, w, day, mixes, fleet[min(len(demand), len(fleet)):], ss.runID+":filltop")
	ids = append(ids, v.top.ids...)
	unresolved += v.top.unresolved
	if err != nil {
		return ids, unresolved, fmt.Errorf("fill top-up: %w", err)
	}
	// The top-up's own bookings and refusals, judged after it (it only adds too).
	if err := ss.judgeQuality(ctx, w, ss.runID+":filltop:%", v); err != nil {
		return ids, unresolved, err
	}
	if v.topRefusals, err = judgeRefusals(ctx, ss.conn, w.dealer.ID, v.top.refusals.list(), nil, topStarted); err != nil {
		return ids, unresolved, err
	}
	if err := ss.judgeFillDay(ctx, w, day, mixes, demand, v); err != nil {
		return ids, unresolved, err
	}
	// The same demand, one customer at a time, on a fresh day of the same weekday.
	replay := day.AddDays(7)
	if err := ss.prepareDay(ctx, w, replay); err != nil {
		return ids, unresolved, err
	}
	fmt.Printf("phase fill replay: the same %d customers one at a time on %s (same weekday and roster)\n", len(demand), replay)
	v.seq = runFill(ctx, ss.api, w, replay, onDay(demand, w, day, replay), 1, 0, ss.runID+":fillseq")
	ids = append(ids, v.seq.ids...)
	unresolved += v.seq.unresolved
	if err := queryOne(ctx, ss.conn, bookedMinutesSQL, []any{ss.runID + ":fillseq:%"}, &v.seqMinutes); err != nil {
		return ids, unresolved, err
	}
	v.record(m, s, day, replay)
	return ids, unresolved, nil
}

// judgeFillDay reads the full day back: the day-full verdict, quality, and the bounds.
func (ss *session) judgeFillDay(ctx context.Context, w workshop, day booking.Date, mixes []jobMix, demand []fillRequest, v *fillVerdict) error {
	from, to := localDay(day, w.loc)
	lo, hi := dayBounds(day, w.loc)
	shop, err := loadShop(ctx, ss.conn, w.dealer.ID, lo, hi)
	if err != nil {
		return err
	}
	expressMax := int(w.dealer.Policy.ExpressMaxMinutes)
	var full []demandClass
	for _, mix := range mixes {
		for _, waiter := range []bool{false, true} {
			v.classes++
			slots, err := dayStarts(w, day, mix.req.minutes)
			if err != nil {
				return err
			}
			shop.evaluate(slots, mix.req, softUpgrade(mix.req, waiter, expressMax), mix.req.minutes)
			if anyFeasible(slots) {
				v.feasibleClasses = append(v.feasibleClasses, fillClassKey(mix, waiter))
			}
			full = append(full, demandClass{mix: mix, waiter: waiter, minutes: 1 << 20})
		}
	}
	asked := map[string]int{} // class → index in classes
	var classes []demandClass
	for _, r := range demand {
		k := fillClassKey(r.mix, r.waiter)
		i, ok := asked[k]
		if !ok {
			i = len(classes)
			asked[k] = i
			classes = append(classes, demandClass{mix: r.mix, waiter: r.waiter})
		}
		classes[i].minutes += r.mix.req.minutes
	}
	if v.bound, v.boundBays, v.boundTk, err = capacityBound(shop, w, day, classes); err != nil {
		return err
	}
	if v.capBound, _, _, err = capacityBound(shop, w, day, full); err != nil {
		return err
	}
	if v.firstFitSpread, err = firstFitAcross(shop, w, day, demand, w.names()); err != nil {
		return err
	}
	v.firstFitMinutes, v.firstFit = v.firstFitSpread.listed, v.firstFitSpread.listedBooked
	if err := queryOne(ctx, ss.conn, `
		SELECT coalesce(sum(booked_duration_minutes), 0)::int FROM appointments
		WHERE dealership_id = $1 AND status IN (`+holdingSQL+`) AND start_at >= $2 AND start_at < $3`,
		[]any{w.dealer.ID, from, to}, &v.fullMinutes); err != nil {
		return err
	}
	if v.bayUse, err = utilisation(ctx, ss.conn, bayUseSQL, w, from, to); err != nil {
		return fmt.Errorf("fill: bay use: %w", err)
	}
	if v.techUse, err = utilisation(ctx, ss.conn, techUseSQL, w, from, to); err != nil {
		return fmt.Errorf("fill: technician use: %w", err)
	}
	return nil
}

// judgeQuality appends the run's bookings under key that a best-fit allocator would not have made
// while the state is what they were placed into: a job (waiter or not) on a more senior technician
// than a free qualified one, a waiter's general job in the quick lane while a general bay is free, an
// express job spilled into a general bay while an express bay is free.
func (ss *session) judgeQuality(ctx context.Context, w workshop, key string, v *fillVerdict) error {
	for _, c := range []struct {
		name string
		sql  string
		dst  *[]string
	}{
		{"over-qualified", overQualifiedSQL, &v.overQualified},
		{"quick lane", quickLaneMisuseSQL, &v.quickLane},
		{"express spill", spillMisuseSQL, &v.spill},
	} {
		rows, err := textRows(ctx, ss.conn, c.sql, w.dealer.ID, key, v.started)
		if err != nil {
			return fmt.Errorf("fill: %s: %w", c.name, err)
		}
		*c.dst = append(*c.dst, rows...)
	}
	return nil
}

// replayShortfall fails a concurrent day that booked more than marginPct percent fewer labour
// minutes than the sequential replay of the same demand.
func replayShortfall(concurrent, sequential int, marginPct float64) error {
	if sequential <= 0 {
		return fmt.Errorf("the sequential replay booked nothing")
	}
	if float64(concurrent) < float64(sequential)*(1-marginPct/100) {
		return fmt.Errorf("the concurrent day booked %d labour minutes, %.1f%% below the sequential replay's %d (margin %.0f%%)",
			concurrent, 100*float64(sequential-concurrent)/float64(sequential), sequential, marginPct)
	}
	return nil
}

// record writes the fill day's evidence into the matrix.
func (v *fillVerdict) record(m *matrix, s settings, day, replay booking.Date) {
	src := "fill"
	st := v.main
	m.check("R1", src, st.booked > 0, "%d customers, %d booked (%d first try, %d after losing a start to another customer), %d offered nothing, %d gave up after losing %d starts in a row, %d unresolved, %d errors",
		st.requests, st.booked, st.firstTry, st.retried, st.turnedAway, st.gaveUp, fillTries, st.unresolved, st.errored)
	unexpected := append(append(slices.Clone(st.unexpected), v.top.unexpected...), v.seq.unexpected...)
	m.check("R1", src, len(unexpected) == 0 && st.errored+v.top.errored+v.seq.errored == 0, "unexpected answers (5xx, 400, unreadable 2xx …) on the day, its top-up and its replay: %v", unexpected)
	v.refusals.record(m, src)
	v.topRefusals.record(m, src+" top-up")
	m.check("R2b", src, len(v.feasibleClasses) == 0 && v.top.turnedAway > 0,
		"day ends full: after the demand, %d top-up round(s) offered every job mix (waiting and not) until a round found nothing offered at all (%d customers offered nothing in all); the oracle then finds no fitting start for any of the %d classes%s",
		v.topRounds, st.turnedAway+v.top.turnedAway, v.classes, feasibleSuffix(v.feasibleClasses))
	m.check("Q5", src, len(v.overQualified) == 0, "jobs (waiters' included) on a more senior technician than a free qualified one (judged right after the demand, before the top-up fills the day): %d %v", len(v.overQualified), v.overQualified)
	waiterGeneral, waiterExpress := st.waiterGeneral+v.top.waiterGeneral, st.waiterExpress+v.top.waiterExpress
	m.check("Q5", src, len(v.quickLane) == 0 && waiterGeneral > 0,
		"%d waiter general jobs booked (express limit %d min), %d of them in the quick lane; in the quick lane while a general bay was free (judged right after the demand, and the top-up's after it): %d %v",
		waiterGeneral, workshopExpressMax, waiterExpress, len(v.quickLane), v.quickLane)
	expressJobs, spilled := st.expressJobs+v.top.expressJobs, st.expressSpilled+v.top.expressSpilled
	m.check("Q5", src, len(v.spill) == 0 && expressJobs > 0,
		"%d express jobs booked, %d of them spilled into a general bay; spilled while an express bay was free: %d %v",
		expressJobs, spilled, len(v.spill), v.spill)
	// Utilisation against the offline bounds: a fact (GiST plus the SQL pass's fit and duty checks
	// already keep the day under them), failing only if a bound or that pass is wrong.
	m.infoUnless("Q5", src, v.mainMinutes <= v.bound && v.mainMinutes > 0,
		"utilisation: the demand (%d labour minutes asked) booked %d minutes = %s of the offline upper bound %d for that demand (bays %d, technicians on duty %d)",
		st.offeredMinutes, v.mainMinutes, pct(v.mainMinutes, v.bound), v.bound, v.boundBays, v.boundTk)
	m.infoUnless("Q5", src, v.fullMinutes <= v.capBound && v.fullMinutes > 0,
		"full day: %d labour minutes booked after the top-up = %s of the shop's capacity bound %d for the job mixes offered; bays: %s; technicians: %s",
		v.fullMinutes, pct(v.fullMinutes, v.capBound), v.capBound, useSummary(v.bayUse), useSummary(v.techUse))
	m.addHeadline("Q5", "fill day (demand %.1f× its bound: any allocator saturates it) booked %s of its max-flow bound, replay %s vs first fit",
		float64(st.offeredMinutes)/float64(max(v.bound, 1)), pct(v.mainMinutes, v.bound), gainPct(v.seqMinutes, v.firstFitMinutes))
	err := replayShortfall(v.mainMinutes, v.seqMinutes, s.fillMarginPct)
	m.check("Q5", src, err == nil,
		"sequential replay: the same %d customers one at a time on %s booked %d labour minutes (%d customers booked); the concurrent day on %s booked %d (%s of it, margin %.0f%%)%s",
		v.seq.requests, replay, v.seqMinutes, v.seq.booked,
		day, v.mainMinutes, pct(v.mainMinutes, v.seqMinutes), s.fillMarginPct, errSuffix(err))
	ffErr := firstFitShortfall(v.seqMinutes, v.firstFitMinutes, s.firstFitMarginPct)
	m.check("Q5", src, ffErr == nil,
		"no worse than naive first fit (margin %.0f%%): the same demand replayed offline by a first-fit allocator (first free fitting bay and technician, no best fit; the workshop's listed resource order) on the oracle's model of the shop booked %d labour minutes (%d customers; over %d random orders %d … %d … %d); the server's replay booked %d = %s of it (gain %s), the concurrent day %d; capacity bound %d — the demand asks %.1f× that, so every greedy allocator saturates the day and the gain is ~0 here by construction (the fit days measure it)%s",
		s.firstFitMarginPct,
		v.firstFitMinutes, v.firstFit, firstFitOrders, v.firstFitSpread.min, v.firstFitSpread.median, v.firstFitSpread.max,
		v.seqMinutes, pct(v.seqMinutes, v.firstFitMinutes), gainPct(v.seqMinutes, v.firstFitMinutes),
		v.mainMinutes, v.bound, float64(st.offeredMinutes)/float64(max(v.bound, 1)), errSuffix(ffErr))
	if len(st.distanceFromDesired) > 0 {
		d := summarize(slices.Clone(st.distanceFromDesired))
		m.info("Q5", src, "booked start vs the customer's wish (minutes): p50=%.0f p95=%.0f max=%.0f", d.p50, d.p95, d.maxValue)
	}
	book, read := summarize(slices.Clone(st.bookMS)), summarize(slices.Clone(st.availMS))
	loop := fmt.Sprintf("open loop, %.0f customers arriving per second, at most %d in flight", s.fillRate, s.fillWorkers)
	if s.fillRate <= 0 {
		loop = fmt.Sprintf("closed loop, %d concurrent customers", s.fillWorkers)
	}
	m.check("Q3", src, book.p99 <= s.sloBookMS && read.p99 <= s.sloReadMS && book.n > 0,
		"p99 booking %.0f ms (budget %.0f), p99 availability %.0f ms (budget %.0f), each from its actual send, %s; availability (ms) %s; booking (ms) %s",
		book.p99, s.sloBookMS, read.p99, s.sloReadMS, loop, read, book)
	if s.fillRate > 0 {
		// The customer's view: the first read timed from when the customer was due, so a server
		// (or a generator cap) that falls behind the arrivals cannot hide it (no coordinated omission).
		queue, arrival := summarize(slices.Clone(st.queueMS)), summarize(slices.Clone(st.arrivalMS))
		m.check("Q3", src, arrival.n > 0 && arrival.p99 <= s.sloReadMS,
			"first availability read from the customer's intended arrival: p99 %.0f ms (budget %.0f) — (ms) %s; of it, waiting for the generator's in-flight cap (%d) (ms) %s",
			arrival.p99, s.sloReadMS, arrival, s.fillWorkers, queue)
	}
	m.metric("fill", map[string]any{
		"requests": st.requests, "booked": st.booked, "turnedAway": st.turnedAway, "gaveUp": st.gaveUp, "unresolved": st.unresolved,
		"errors": st.errored, "staleConflicts": st.staleConflicts, "refusalsJudged": v.refusals.judged + v.topRefusals.judged, "falseRefusals": v.refusals.falseRefusals + v.topRefusals.falseRefusals,
		"offeredMinutes": st.offeredMinutes, "bookedMinutes": v.mainMinutes, "bound": v.bound, "fullDayMinutes": v.fullMinutes,
		"capacityBound": v.capBound, "sequentialMinutes": v.seqMinutes, "firstFitMinutes": v.firstFitMinutes, "topUpRounds": v.topRounds, "bayUse": v.bayUse, "techUse": v.techUse,
	})
}

// gainPct is how much more a booked than b, as a signed percentage.
func gainPct(a, b int) string {
	if b == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", 100*float64(a-b)/float64(b))
}

func feasibleSuffix(classes []string) string {
	if len(classes) == 0 {
		return ""
	}
	return " — still feasible: " + strings.Join(classes, ", ")
}
