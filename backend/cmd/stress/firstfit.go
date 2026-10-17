package main

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// A lower bound for allocation quality. The capacity bound (quality.go) only says the allocator
// never booked more than physically fits, and the sequential replay compares the server with
// itself; neither shows that best fit is worth anything. So the fill day's demand is also replayed
// offline, on the oracle's model of the same shop and roster (rules.go, never the engine's code),
// by a naive allocator: the same customers in the same order, each offered every start where some
// fitting bay and some qualified on-duty technician are free for the whole window (and staging has
// room), taking the one nearest its wish like the real customers do, and then placed first fit —
// the first free fitting bay and the first free qualified technician in a fixed (id) order, with
// no preference for the required bay type, the most junior technician, or the tightest gap. The
// server's sequential replay of that demand (one customer at a time, so the order is the same) must
// book at least as many labour minutes, within -firstfit-margin-pct (dayphases.go).

// firstFitDay replays demand on day at shop (its bays, technicians and rosters; its holds are
// ignored: the day starts empty, like the replay day) and returns the labour minutes and customers
// the naive allocator booked.
func firstFitDay(shop *oracleShop, w workshop, day booking.Date, demand []fillRequest) (minutes, booked int, err error) {
	sim := *shop
	sim.holds = nil
	for _, r := range demand {
		slots, err := dayStarts(w, day, r.mix.req.minutes)
		if err != nil {
			return 0, 0, err
		}
		soft := softUpgrade(r.mix.req, r.waiter, sim.expressMax)
		length := time.Duration(r.mix.req.minutes) * time.Minute
		best, bestGap := -1, time.Duration(0)
		for i, sl := range slots {
			if sl.lunch || !sl.inHours || !sim.stagingRoom(sl.start, uuid.Nil) {
				continue
			}
			if b, t := sim.counts(r.mix.req, soft, timeWindow{sl.start, sl.start.Add(length)}, uuid.Nil); b == 0 || t == 0 {
				continue
			}
			gap := sl.start.Sub(r.desired)
			if gap < 0 {
				gap = -gap
			}
			if best < 0 || gap < bestGap {
				best, bestGap = i, gap
			}
		}
		if best < 0 {
			continue // turned away
		}
		win := timeWindow{slots[best].start, slots[best].start.Add(length)}
		bay, tech := sim.firstFree(r.mix.req, soft, win)
		sim.holds = append(sim.holds, oHold{id: uuid.New(), bay: bay, tech: tech, vehicle: r.vehicle, status: 1, window: win})
		minutes += r.mix.req.minutes
		booked++
	}
	return minutes, booked, nil
}

// firstFree is the first fitting bay and the first qualified on-duty technician (shop order) free
// for the whole of w. The caller has checked that both exist.
func (s *oracleShop) firstFree(r jobRequirement, soft bool, w timeWindow) (bay, tech uuid.UUID) {
	return s.firstFreeIn(r, soft, w, false)
}

// firstFreeIn is firstFree; with ownTypeFirst, a free bay of the job's own type is taken before
// any fallback (the express spill, a waiter's soft upgrade), else the first fitting one. uuid.Nil
// for a resource none of which is free.
func (s *oracleShop) firstFreeIn(r jobRequirement, soft bool, w timeWindow, ownTypeFirst bool) (bay, tech uuid.UUID) {
	busyBay, busyTech := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for _, h := range s.holds {
		if h.window.overlaps(w) {
			busyBay[h.bay], busyTech[h.tech] = true, true
		}
	}
	for pass := range 2 {
		for _, b := range s.bays {
			if !busyBay[b.id] && bayFits(r, soft, b.bayType) && (pass == 1 || !ownTypeFirst || b.bayType == r.bayType) {
				bay = b.id
				break
			}
		}
		if bay != uuid.Nil {
			break
		}
	}
	for _, t := range s.techs {
		if !busyTech[t.id] && qualified(r, t.level, t.skills) && t.roster.onDuty(w, s.loc) {
			tech = t.id
			break
		}
	}
	return bay, tech
}

// firstFitShortfall fails a server replay that booked more than marginPct percent fewer labour
// minutes than the naive first-fit replay of the same demand.
func firstFitShortfall(server, firstFit int, marginPct float64) error {
	if firstFit <= 0 {
		return fmt.Errorf("the first-fit replay booked nothing")
	}
	if float64(server) < float64(firstFit)*(1-marginPct/100) {
		return fmt.Errorf("the server's replay booked %d labour minutes, %.1f%% below naive first fit's %d (margin %.0f%%)",
			server, 100*float64(firstFit-server)/float64(firstFit), firstFit, marginPct)
	}
	return nil
}

// First fit depends on the order it walks the bays and technicians in. The baseline the server is
// held to is the listed order — the workshop as a person writes it down (workshopDef: quick-lane
// bays first, technicians junior first), a fixed and sensible naive allocator — and the run also
// reports first fit over firstFitOrders random orders (seeded: reproducible), so a lucky or
// unlucky order cannot make the comparison.

// firstFitOrders is how many random resource orders the first-fit spread replays.
const firstFitOrders = 24

// orderSpread is first fit in the listed order and over the random orders.
type orderSpread struct {
	listed, listedBooked int
	min, median, max     int // labour minutes over the random orders
}

// names maps the shop's bay and technician ids to their names (the workshop as provisioned).
func (w workshop) names() map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, b := range w.bays {
		out[b.ID] = b.Name
	}
	for _, t := range w.techs {
		out[t.ID] = t.Name
	}
	return out
}

// inListedOrder is shop with its bays and technicians in the order def lists them (names: id →
// name); unlisted ones follow in their current order.
func inListedOrder(shop *oracleShop, def workshopDef, names map[uuid.UUID]string) *oracleShop {
	rank := map[string]int{}
	for i, b := range def.bays {
		rank[b.name] = i
	}
	for i, t := range def.techs {
		rank[t.name] = i
	}
	pos := func(id uuid.UUID) int {
		if r, ok := rank[names[id]]; ok {
			return r
		}
		return 1 << 20
	}
	out := *shop
	out.bays = slices.Clone(shop.bays)
	out.techs = slices.Clone(shop.techs)
	slices.SortStableFunc(out.bays, func(a, b oBay) int { return pos(a.id) - pos(b.id) })
	slices.SortStableFunc(out.techs, func(a, b oTech) int { return pos(a.id) - pos(b.id) })
	return &out
}

// firstFitAcross replays demand by first fit in the listed order and in firstFitOrders random
// orders (seed 1: the same orders every run).
func firstFitAcross(shop *oracleShop, w workshop, day booking.Date, demand []fillRequest, names map[uuid.UUID]string) (orderSpread, error) {
	var sp orderSpread
	var err error
	shop = inListedOrder(shop, w.def, names) // the shuffles start from it: the same orders on every database
	if sp.listed, sp.listedBooked, err = firstFitDay(shop, w, day, demand); err != nil {
		return sp, err
	}
	rng := rand.New(rand.NewPCG(1, uint64(len(demand)))) //nolint:gosec // reproducible orders, not security
	minutes := make([]int, 0, firstFitOrders)
	for range firstFitOrders {
		s := *shop
		s.bays, s.techs = slices.Clone(shop.bays), slices.Clone(shop.techs)
		rng.Shuffle(len(s.bays), func(i, j int) { s.bays[i], s.bays[j] = s.bays[j], s.bays[i] })
		rng.Shuffle(len(s.techs), func(i, j int) { s.techs[i], s.techs[j] = s.techs[j], s.techs[i] })
		m, _, err := firstFitDay(&s, w, day, demand)
		if err != nil {
			return sp, err
		}
		minutes = append(minutes, m)
	}
	slices.Sort(minutes)
	sp.min, sp.median, sp.max = minutes[0], minutes[len(minutes)/2], minutes[len(minutes)-1]
	return sp, nil
}
