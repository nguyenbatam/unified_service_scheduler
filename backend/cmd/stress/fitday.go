package main

import (
	"context"
	"fmt"
	"math/rand/v2"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// The fit day: where allocation quality shows. The fill day asks several times what the shop holds,
// so any allocator saturates it and best fit cannot beat first fit there (the gain is ~0 by
// construction). The fit day sizes the same kind of demand — the fill day's job mixes and weights
// (oil 30 min, brake 90, major 180, oil + brake 120), a quarter of the customers waiting, wishes
// spread over the opening hours — to fitLoad × the shop's capacity bound (quality.go: max flow
// over bays and on-duty technicians for every job mix offered), so the day is just about full
// and where each job is placed decides what still fits. The same customers in the same order are
// then booked one at a time by the server (each takes the offered start nearest its wish, like
// the fill day) and placed offline by naive first fit on the oracle's model (firstfit.go), in the
// workshop's listed resource order and in 24 random orders. Each day the server must book at least
// as many labour minutes as first fit in the listed order (-firstfit-margin-pct, 0), and over the
// days at least -fit-min-gain-pct (0) more than first fit in the median random order; that gain is
// the Q5 headline. Measured against the engine (fitday_db_test.go, 20 seeds × 2 rosters): +5.6 %
// (weekday) and +3.6 % (Saturday) over the median order pooled, never below the listed order but
// only +0.0 … +0.3 % above it (listing the quick lane and the junior technicians first is already
// most of best fit on random demand); the scripted quality day is where best fit provably wins.

// fitLoads are the demand sizes the fit day plays, as a multiple of the capacity bound: just
// under, at, and a little over what the shop can hold.
var fitLoads = []float64{1.0, 1.15, 1.3}

// fitDemand draws customers for day (reproducible for a seed: the fill day's mixes, weights, waiter
// share and wish times) until they ask at least load × bound labour minutes, one car each.
func fitDemand(seed uint64, load float64, bound int, w workshop, day booking.Date, mixes []jobMix, weights []int, vehicles []uuid.UUID) []fillRequest {
	rng := rand.New(rand.NewPCG(seed, uint64(load*1000))) //nolint:gosec // reproducible demand, not security
	starts, _ := dayStarts(w, day, slotMinutes)
	total := 0
	for _, wt := range weights {
		total += wt
	}
	var out []fillRequest
	asked := 0
	for asked < int(load*float64(bound)) && len(out) < len(vehicles) && len(starts) > 0 {
		pick, k := rng.IntN(total), 0
		for pick >= weights[k] {
			pick -= weights[k]
			k++
		}
		r := fillRequest{mix: mixes[k], waiter: rng.IntN(4) == 0, desired: starts[rng.IntN(len(starts))].start, vehicle: vehicles[len(out)]}
		out = append(out, r)
		asked += r.mix.req.minutes
	}
	return out
}

// fullDayClasses is every job mix offered, waiting or not, with unbounded demand: the capacity
// bound of the day itself.
func fullDayClasses(mixes []jobMix) []demandClass {
	var out []demandClass
	for _, mix := range mixes {
		for _, waiter := range []bool{false, true} {
			out = append(out, demandClass{mix: mix, waiter: waiter, minutes: 1 << 20})
		}
	}
	return out
}

// fitOutcome is one fit day: the demand, what the server's one-at-a-time replay booked, and first
// fit in the listed order and over the random orders.
type fitOutcome struct {
	day                     booking.Date
	load                    float64
	bound, asked, customers int
	server, serverBooked    int
	turnedAway, gaveUp      int
	firstFit                orderSpread
}

// fitDays plays one fit day per load on days, each prepared like the other day phases, and records
// the verdicts. It returns the bookings made and the requests left unresolved.
func (ss *session) fitDays(ctx context.Context, w workshop, days []booking.Date, mixes []jobMix, weights []int, fleet []uuid.UUID) ([]uuid.UUID, int, error) {
	var ids []uuid.UUID
	unresolved := 0
	var out []fitOutcome
	for i, day := range days[:min(len(days), len(fitLoads))] {
		if err := ss.prepareDay(ctx, w, day); err != nil {
			return ids, unresolved, err
		}
		from, to := dayBounds(day, w.loc)
		shop, err := loadShop(ctx, ss.conn, w.dealer.ID, from, to)
		if err != nil {
			return ids, unresolved, err
		}
		o := fitOutcome{day: day, load: fitLoads[i]}
		if o.bound, _, _, err = capacityBound(shop, w, day, fullDayClasses(mixes)); err != nil {
			return ids, unresolved, err
		}
		demand := fitDemand(ss.s.fitSeed, o.load, o.bound, w, day, mixes, weights, fleet)
		for _, r := range demand {
			o.asked += r.mix.req.minutes
		}
		o.customers = len(demand)
		fmt.Printf("phase fit day: %d customers asking %d labour minutes (%.2f × the capacity bound %d) one at a time on %s\n",
			len(demand), o.asked, o.load, o.bound, day)
		key := fmt.Sprintf("%s:fit%d", ss.runID, i)
		st := runFill(ctx, ss.api, w, day, demand, 1, 0, key)
		ids = append(ids, st.ids...)
		unresolved += st.unresolved
		if err := queryOne(ctx, ss.conn, bookedMinutesSQL, []any{key + ":%"}, &o.server); err != nil {
			return ids, unresolved, err
		}
		o.serverBooked, o.turnedAway, o.gaveUp = st.booked, st.turnedAway, st.gaveUp
		if len(st.unexpected) > 0 || st.errored > 0 {
			ss.m.fail("R1", "fit day", "%s: unexpected answers %v (%d errors)", day, st.unexpected, st.errored)
		}
		if o.firstFit, err = firstFitAcross(shop, w, day, demand, w.names()); err != nil {
			return ids, unresolved, err
		}
		out = append(out, o)
	}
	recordFitDays(ss.m, ss.s, out)
	return ids, unresolved, nil
}

// recordFitDays writes the fit days' evidence: each day no worse than first fit in the listed
// order (-firstfit-margin-pct), and over all of them a gain over the median first-fit order of at
// least -fit-min-gain-pct, which is the Q5 headline.
func recordFitDays(m *matrix, s settings, out []fitOutcome) {
	if len(out) == 0 {
		return
	}
	var server, listed, median int
	for _, o := range out {
		server += o.server
		listed += o.firstFit.listed
		median += o.firstFit.median
		err := firstFitShortfall(o.server, o.firstFit.listed, s.firstFitMarginPct)
		m.check("Q5", "fit day", err == nil,
			"%s: %d customers asking %d labour minutes (%.2f × the capacity bound %d), booked one at a time: the server booked %d minutes (%d customers; %d offered nothing, %d gave up); naive first fit offline in the listed resource order %d (%d customers, gain %s), over %d random orders %d … %d … %d (gain %s vs the median)%s",
			o.day, o.customers, o.asked, o.load, o.bound, o.server, o.serverBooked, o.turnedAway, o.gaveUp,
			o.firstFit.listed, o.firstFit.listedBooked, gainPct(o.server, o.firstFit.listed),
			firstFitOrders, o.firstFit.min, o.firstFit.median, o.firstFit.max, gainPct(o.server, o.firstFit.median), errSuffix(err))
	}
	gain := 0.0
	if median > 0 {
		gain = 100 * float64(server-median) / float64(median)
	}
	m.check("Q5", "fit gain", median > 0 && gain >= s.fitMinGainPct,
		"over the %d fit days (demand just about what the shop holds, so placement decides what fits): the server booked %d labour minutes, %s vs first fit in the median of %d random resource orders (%d), %s vs the listed order (%d); required ≥ %+.0f%%",
		len(out), server, gainPct(server, median), firstFitOrders, median, gainPct(server, listed), listed, s.fitMinGainPct)
	m.prependHeadline("Q5", "fit days %s vs median first-fit order (%s vs listed order)", gainPct(server, median), gainPct(server, listed))
	m.metric("fit", map[string]any{"server": server, "firstFitListed": listed, "firstFitMedian": median, "gainVsMedianPct": gain})
}
