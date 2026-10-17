package main

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The quality day (phase "quality", qualityphase.go) is a scripted day on a workshop of its own
// where greedy first fit provably loses: the same requests, in the same order, each wanting one
// exact start (a drop-off time), are sent to the server one at a time and replayed offline by a
// first-fit allocator on the oracle's model of the shop (rules.go, never the engine's code) — in
// EVERY fixed order of the bays and of the technicians, both naive (the first fitting bay, as
// firstfit.go) and own-type-first (the first bay of the job's own type, then a fallback). The
// server must book clearly more labour minutes than the best of them, and the one-move repair
// must have been used. The script has three blocks, separated in time and by the technicians'
// shifts so they never interact:
//
//   - A, best fit (07:00–11:00, two general bays, three identical brake technicians): brake jobs
//     at 07:00, 08:00, 09:30, then 08:30. First fit puts the 09:30 job on the first bay in its
//     order — the one free since 08:30 — and the 08:30 job finds both bays taken; best fit puts the
//     09:30 job right behind the 08:00 one (no idle gap) and keeps 08:30–11:00 whole for the last.
//   - B, quick lane + bay repair (11:00–14:00): a waiter's brake job, a brake job, both 11:00, then
//     a brake job at 12:00. The engine puts the waiter in a general bay (the quick lane only as a
//     fallback) and, when the 12:00 job finds both general bays taken, moves the waiter's job to
//     the free express bay (one move, same window). Own-type-first first fit refuses it; naive
//     first fit with the express bay listed first books it (it put the waiter in the quick lane
//     from the start), so this block alone does not beat every order — it proves the bay repair.
//   - C, technician repair (14:00–19:00, two equally senior technicians, one doing lube + brakes,
//     the other lube + general): an oil change at 14:00 then a brake job at 14:00; an oil change at
//     15:30 then a major at 15:30. Whichever technician first fit lists first, the oil change of
//     one pair lands on the technician the second job of that pair needs, and that job is refused.
//     The engine books both pairs: when the brake job finds its only technician holding the oil
//     change, it moves the oil change to the other one (no more senior, no more versatile).
//
// Offline (no engine involved): every first-fit order books 780 or less of the 960 labour minutes
// asked (block A loses a brake job in every order, block C a brake job or a major), the offline
// optimum fits all 960 (an exhaustive assignment search, fixedOptimum). The DB test
// (quality_db_test.go) pins what booking.Service makes of it.

// qualityWorkshop is the quality day's shop: 07:00–19:00 without lunch, one express and two
// general bays; three brake technicians on duty 07:00–14:00 (blocks A and B), and two senior
// technicians on duty 14:00–19:00 (block C) whose skills overlap only on lubrication.
var qualityWorkshop = workshopDef{
	name: "Stress Quality", open: "07:00", close: "19:00",
	bays: []bayDef{
		{"Express 1", domain.BayExpress}, {"General 1", domain.BayGeneral}, {"General 2", domain.BayGeneral},
	},
	techs: []techDef{
		{"Brakes 1", 2, []domain.TechnicianSkill{domain.SkillBrakes}},
		{"Brakes 2", 2, []domain.TechnicianSkill{domain.SkillBrakes}},
		{"Brakes 3", 2, []domain.TechnicianSkill{domain.SkillBrakes}},
		{"Lube Brakes", 3, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes}},
		{"Lube General", 3, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillGeneral}},
	},
	shifts: map[string][]domain.TechnicianShift{
		"Brakes 1":     weeklyShift(allWeekdays, "07:00", "14:00", "", ""),
		"Brakes 2":     weeklyShift(allWeekdays, "07:00", "14:00", "", ""),
		"Brakes 3":     weeklyShift(allWeekdays, "07:00", "14:00", "", ""),
		"Lube Brakes":  weeklyShift(allWeekdays, "14:00", "19:00", "", ""),
		"Lube General": weeklyShift(allWeekdays, "14:00", "19:00", "", ""),
	},
}

// allWeekdays are the ISO weekdays Monday … Sunday.
var allWeekdays = []int16{1, 2, 3, 4, 5, 6, 7}

// qualityStep is one request of the script: its label, block, the catalog service it books (one
// line), whether the customer waits, and its local start.
type qualityStep struct {
	label, block string
	service      string
	waiter       bool
	at           string // HH:MM, local
}

// qualityScript is the quality day, in arrival order (see above).
var qualityScript = []qualityStep{
	{"A1", "A best fit", "BRAKE", false, "07:00"},
	{"A2", "A best fit", "BRAKE", false, "08:00"},
	{"A3", "A best fit", "BRAKE", false, "09:30"},
	{"A4", "A best fit", "BRAKE", false, "08:30"},
	{"B1", "B bay repair", "BRAKE", true, "11:00"},
	{"B2", "B bay repair", "BRAKE", false, "11:00"},
	{"B3", "B bay repair", "BRAKE", false, "12:00"},
	{"C1", "C technician repair", "OIL", false, "14:00"},
	{"C2", "C technician repair", "BRAKE", false, "14:00"},
	{"C3", "C technician repair", "OIL", false, "15:30"},
	{"C4", "C technician repair", "MAJOR", false, "15:30"},
}

// qualityServices are the catalog services the script books.
var qualityServices = []string{"OIL", "BRAKE", "MAJOR"}

// qualityRequest is one customer of the quality day: an exact start, nothing else will do.
type qualityRequest struct {
	label, block string
	mix          jobMix
	waiter       bool
	start        time.Time
	vehicle      uuid.UUID
}

// window is the time the request holds a bay and a technician.
func (r qualityRequest) window() timeWindow {
	return timeWindow{r.start, r.start.Add(time.Duration(r.mix.req.minutes) * time.Minute)}
}

// qualityDemand turns the script into requests on day at w (mixes by service code, one car each).
// Every window must lie inside the opening hours: the comparison is about allocation, not about
// starts the calendar refuses.
func qualityDemand(w workshop, day booking.Date, mixes map[string]jobMix, vehicles []uuid.UUID) ([]qualityRequest, error) {
	if len(vehicles) < len(qualityScript) {
		return nil, fmt.Errorf("quality: %d cars for %d requests", len(vehicles), len(qualityScript))
	}
	open, err := parseClockMinutes(w.dealer.OpenTime)
	if err != nil {
		return nil, err
	}
	closeAt, err := parseClockMinutes(w.dealer.CloseTime)
	if err != nil {
		return nil, err
	}
	out := make([]qualityRequest, len(qualityScript))
	for i, st := range qualityScript {
		mix, ok := mixes[st.service]
		if !ok {
			return nil, fmt.Errorf("quality: no job mix %s", st.service)
		}
		at, err := parseClockMinutes(st.at)
		if err != nil {
			return nil, err
		}
		r := qualityRequest{label: st.label, block: st.block, mix: mix, waiter: st.waiter, start: at.on(day, w.loc), vehicle: vehicles[i]}
		if win := r.window(); win.start.Before(open.on(day, w.loc)) || win.end.After(closeAt.on(day, w.loc)) {
			return nil, fmt.Errorf("quality: %s %s at %s runs outside the opening hours", st.label, st.service, st.at)
		}
		out[i] = r
	}
	return out, nil
}

// firstFitFixed replays demand on shop (its holds ignored: the day starts empty) with first fit
// in the shop's bay and technician order: each request is placed at its own start on the first
// free fitting bay (ownTypeFirst: the first of the job's own bay type, then the first fallback)
// and the first free qualified on-duty technician, or refused. It returns the labour minutes
// booked and which requests were.
func firstFitFixed(shop *oracleShop, demand []qualityRequest, ownTypeFirst bool) (minutes int, booked []bool) {
	sim := *shop
	sim.holds = nil
	booked = make([]bool, len(demand))
	for i, r := range demand {
		soft := softUpgrade(r.mix.req, r.waiter, sim.expressMax)
		win := r.window()
		bay, tech := sim.firstFreeIn(r.mix.req, soft, win, ownTypeFirst)
		if bay == uuid.Nil || tech == uuid.Nil {
			continue
		}
		sim.holds = append(sim.holds, oHold{id: uuid.New(), bay: bay, tech: tech, vehicle: r.vehicle, status: 1, window: win})
		minutes += r.mix.req.minutes
		booked[i] = true
	}
	return minutes, booked
}

// firstFitRun is one first-fit replay: its resource order and variant, and what it booked.
type firstFitRun struct {
	bays, techs  []uuid.UUID
	ownTypeFirst bool
	minutes      int
	booked       []bool
}

// firstFitSpread is first fit over every fixed order: the best and worst replays and how many ran.
type firstFitSpread struct {
	best, worst firstFitRun
	runs        int
}

// maxFirstFitRuns bounds firstFitEveryOrder (bays! × technicians! × 2).
const maxFirstFitRuns = 200_000

// firstFitEveryOrder replays demand with first fit in every order of the shop's bays and of its
// technicians, naive and own-type-first, and returns the best and the worst.
func firstFitEveryOrder(shop *oracleShop, demand []qualityRequest) (firstFitSpread, error) {
	n := 2 * factorial(len(shop.bays)) * factorial(len(shop.techs))
	if n > maxFirstFitRuns {
		return firstFitSpread{}, fmt.Errorf("quality: %d bays and %d technicians make %d first-fit orders (at most %d)", len(shop.bays), len(shop.techs), n, maxFirstFitRuns)
	}
	var out firstFitSpread
	sim := *shop
	for _, bays := range permutations(shop.bays) {
		for _, techs := range permutations(shop.techs) {
			sim.bays, sim.techs = bays, techs
			for _, typed := range []bool{false, true} {
				minutes, booked := firstFitFixed(&sim, demand, typed)
				run := firstFitRun{bays: bayIDs(bays), techs: techIDs(techs), ownTypeFirst: typed, minutes: minutes, booked: booked}
				if out.runs == 0 || minutes > out.best.minutes {
					out.best = run
				}
				if out.runs == 0 || minutes < out.worst.minutes {
					out.worst = run
				}
				out.runs++
			}
		}
	}
	return out, nil
}

func factorial(n int) int {
	f := 1
	for i := 2; i <= n; i++ {
		f *= i
	}
	return f
}

// permutations lists every order of items (Heap's algorithm; each a fresh slice).
func permutations[T any](items []T) [][]T {
	a := slices.Clone(items)
	out := [][]T{slices.Clone(a)}
	c := make([]int, len(a))
	for i := 0; i < len(a); {
		if c[i] < i {
			if i%2 == 0 {
				a[0], a[i] = a[i], a[0]
			} else {
				a[c[i]], a[i] = a[i], a[c[i]]
			}
			out = append(out, slices.Clone(a))
			c[i]++
			i = 0
		} else {
			c[i] = 0
			i++
		}
	}
	return out
}

func bayIDs(bays []oBay) []uuid.UUID {
	out := make([]uuid.UUID, len(bays))
	for i, b := range bays {
		out[i] = b.id
	}
	return out
}

func techIDs(techs []oTech) []uuid.UUID {
	out := make([]uuid.UUID, len(techs))
	for i, t := range techs {
		out[i] = t.id
	}
	return out
}

// fixedOptimum is the most labour minutes any assignment of bays and technicians can book for
// demand at its fixed starts on shop (its holds ignored): an exhaustive search — each request
// placed on any fitting bay and qualified on-duty technician free at that time, or left out — with
// branch and bound on the minutes still asked. The script is small (a dozen requests, a handful of
// resources), and the search stops as soon as everything fits.
func fixedOptimum(shop *oracleShop, demand []qualityRequest) int {
	type placement struct{ bay, tech uuid.UUID }
	options := make([][]placement, len(demand))
	remaining := make([]int, len(demand)+1) // minutes asked from request i on
	for i := len(demand) - 1; i >= 0; i-- {
		r := demand[i]
		remaining[i] = remaining[i+1] + r.mix.req.minutes
		soft := softUpgrade(r.mix.req, r.waiter, shop.expressMax)
		for _, b := range shop.bays {
			if !bayFits(r.mix.req, soft, b.bayType) {
				continue
			}
			for _, t := range shop.techs {
				if qualified(r.mix.req, t.level, t.skills) && t.roster.onDuty(r.window(), shop.loc) {
					options[i] = append(options[i], placement{b.id, t.id})
				}
			}
		}
	}
	chosen := make([]placement, len(demand))
	placed := make([]bool, len(demand))
	best := 0
	var search func(i, minutes int)
	search = func(i, minutes int) {
		if minutes > best {
			best = minutes
		}
		if i == len(demand) || minutes+remaining[i] <= best || best == remaining[0] {
			return
		}
		w := demand[i].window()
		for _, p := range options[i] {
			clash := false
			for j := range i {
				if placed[j] && demand[j].window().overlaps(w) && (chosen[j].bay == p.bay || chosen[j].tech == p.tech) {
					clash = true
					break
				}
			}
			if clash {
				continue
			}
			chosen[i], placed[i] = p, true
			search(i+1, minutes+demand[i].mix.req.minutes)
			placed[i] = false
		}
		search(i+1, minutes)
	}
	search(0, 0)
	return best
}

// defaultQualityMarginPct is how much more than the best first-fit replay the server must book
// on the quality day (-quality-margin-pct). Measured: the engine books all 960 labour minutes
// asked, the best first-fit order 780 (+23.1 %). Without the bay repair the engine loses B3,
// without the technician repair C2 (block A's best fit has the repair as its safety net: a wrong
// bay for A3 is moved back for A4, which the DB test's pinned move count would show) — 870 at
// most, +11.5 %; so 15 % passes only with the mechanisms working.
const defaultQualityMarginPct = 15

// qualityShortfall fails a server that booked less than marginPct percent more labour minutes
// than the best first-fit replay.
func qualityShortfall(server, bestFirstFit int, marginPct float64) error {
	if bestFirstFit <= 0 {
		return fmt.Errorf("the first-fit replays booked nothing")
	}
	if float64(server) < float64(bestFirstFit)*(1+marginPct/100) {
		return fmt.Errorf("the server booked %d labour minutes, %s vs the best first fit's %d: want at least +%.0f%%",
			server, gainPct(server, bestFirstFit), bestFirstFit, marginPct)
	}
	return nil
}
