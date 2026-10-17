package booking

import (
	"bytes"
	"cmp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Recommended starts. The engine picks the best-fit bay and technician for the start a customer
// chose, but the start itself is the customer's: one in the middle of a free stretch splits it in
// two, and the pieces may be too short for the next long job. A slot is recommended when booking
// it leaves no new idle gap on the bay and on the technician the engine would pick for it — the
// first of store.freeBaysSQL's and store.freeTechniciansSQL's orders, restated here exactly
// (pickBay, pickTech) so the hint describes the booking the customer would really get: on each,
// the booking butts against a hold or an edge of the bookable stretch (opening, lunch, closing;
// for a technician also their shift's start, end, and break) on at least one side. It is a hint for
// the UI to steer customers by, computed from what availability already read; slots offered only
// through the one-move repair are never recommended (they move someone).

// recommended reports whether slot (available with free bays and technicians) fills a gap, per the
// rules above. lo and hi bound the slot's bookable stretch (dayClocks.stretch); loads are the
// technicians' booked minutes that day (techLoads).
func (rng availRange) recommended(in availInputs, loads []int, slot Interval, local localWindow, lo, hi time.Time) bool {
	bay, ok := rng.pickBay(in, slot, lo, hi)
	if !ok || !bay.flush() {
		return false
	}
	tech, ok := rng.pickTech(in, loads, slot, local, lo, hi)
	return ok && tech.flush()
}

// gaps is the idle time a resource keeps before and after a window (store.sideGapsSQL).
type gaps struct{ before, after time.Duration }

// flush reports whether the window leaves no new idle gap: it butts against a hold or an edge on
// at least one side.
func (g gaps) flush() bool { return g.before == 0 || g.after == 0 }

// fit is the best-fit key of g (store.fitRankSQL): the total idle time left, smallest first.
func (g gaps) fit() time.Duration { return g.before + g.after }

// sideGaps is the Go twin of store.sideGapsSQL: the idle time l's resource keeps before w (from
// the later of lo and the end of its last hold before w) and after it (up to the earlier of hi and
// the start of its next hold). The resource is free for w.
func sideGaps(l holdList, w Interval, lo, hi time.Time) gaps {
	prev, next := lo, hi
	for _, h := range l.overlapping(lo, hi) {
		if lo.Before(w.Start) && h.Start.Before(w.Start) && h.End.After(lo) && h.End.After(prev) {
			prev = h.End
		}
		if w.End.Before(hi) && h.End.After(w.End) && h.Start.Before(hi) && h.Start.Before(next) {
			next = h.Start
		}
	}
	return gaps{before: w.Start.Sub(prev), after: next.Sub(w.End)}
}

// pickBay is the bay store.freeBaysSQL lists first for slot — a free bay that fits the job, of
// its own type before a fallback, best fit, then by name (byte order) and id — and its gaps.
func (rng availRange) pickBay(in availInputs, slot Interval, lo, hi time.Time) (gaps, bool) {
	best, bestGaps := -1, gaps{}
	for i := range rng.bayIDs {
		if !rng.bayFit[i] || rng.bayHolds[i].busy(slot) {
			continue
		}
		g := sideGaps(rng.bayHolds[i], slot, lo, hi)
		if best < 0 || rng.compareBays(in, i, g, best, bestGaps) < 0 {
			best, bestGaps = i, g
		}
	}
	return bestGaps, best >= 0
}

func (rng availRange) compareBays(in availInputs, i int, gi gaps, j int, gj gaps) int {
	group := func(k int) int {
		if rng.bayTypes[k] == in.req.BayType {
			return 0
		}
		return 1
	}
	return cmp.Or(
		cmp.Compare(group(i), group(j)),
		cmp.Compare(gi.fit(), gj.fit()),
		strings.Compare(rng.bayNames[i], rng.bayNames[j]),
		compareIDs(rng.bayIDs[i], rng.bayIDs[j]),
	)
}

// pickTech is the technician store.freeTechniciansSQL lists first for slot — qualified, on duty
// and free; the lowest level, the fewest spare skills, best fit on their own day (the stretch
// bounded by their shift and break, roster.stretch), the least loaded, then by name (byte order)
// and id — and their gaps.
func (rng availRange) pickTech(in availInputs, loads []int, slot Interval, local localWindow, lo, hi time.Time) (gaps, bool) {
	best, bestGaps := -1, gaps{}
	for i := range rng.techs {
		if !rng.techFit[i] || rng.techHolds[i].busy(slot) || !rng.rosters[i].onDuty(slot, local) {
			continue
		}
		tlo, thi := rng.rosters[i].stretch(slot, local, in.loc, lo, hi)
		g := sideGaps(rng.techHolds[i], slot, tlo, thi)
		if best < 0 || rng.compareTechs(in, loads, i, g, best, bestGaps) < 0 {
			best, bestGaps = i, g
		}
	}
	return bestGaps, best >= 0
}

func (rng availRange) compareTechs(in availInputs, loads []int, i int, gi gaps, j int, gj gaps) int {
	a, b := rng.techs[i], rng.techs[j]
	return cmp.Or(
		cmp.Compare(a.SkillLevel, b.SkillLevel),
		cmp.Compare(spare(a.Skills, in.req.Skills), spare(b.Skills, in.req.Skills)),
		cmp.Compare(gi.fit(), gj.fit()),
		cmp.Compare(loads[i], loads[j]),
		strings.Compare(a.Name, b.Name),
		compareIDs(a.ID, b.ID),
	)
}

// compareIDs orders uuids as Postgres does (bytewise).
func compareIDs(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }

// techLoads is each technician's booked minutes on the local day [dayStart, dayEnd): the holds
// starting in it, as freeTechniciansSQL's load (rounded to the minute like its ::int).
func (rng availRange) techLoads(dayStart, dayEnd time.Time) []int {
	loads := make([]int, len(rng.techs))
	for i, l := range rng.techHolds {
		var total time.Duration
		lo, hi := l.span(dayStart, dayEnd)
		for _, h := range l.holds[lo:hi] {
			if !h.Start.Before(dayStart) && h.Start.Before(dayEnd) {
				total += h.End.Sub(h.Start)
			}
		}
		loads[i] = int((total + 30*time.Second) / time.Minute)
	}
	return loads
}

// stretch is the Go twin of store.techStretchSQL: [lo, hi] narrowed to the technician's shift on
// the slot's local weekday and to its break on the slot's side, as wall clocks on the slot's local
// date, then widened back to the slot. Without a shift pattern it is [lo, hi] (the opening hours).
func (r *roster) stretch(slot Interval, local localWindow, loc *time.Location, lo, hi time.Time) (time.Time, time.Time) {
	if sh := r.byWeekday[local.weekday]; r.hasShifts && sh != nil {
		y, m, d := slot.Start.In(loc).Date()
		at := func(clock time.Duration) time.Time { return pgLocalInstant(y, m, d, clock, loc) }
		lo = latest(lo, at(sh.start))
		if sh.hasBreak && sh.breakEnd <= local.start {
			lo = latest(lo, at(sh.breakEnd))
		}
		hi = earliest(hi, at(sh.end))
		if sh.hasBreak && sh.breakStart >= local.end {
			hi = earliest(hi, at(sh.breakStart))
		}
	}
	return earliest(lo, slot.Start), latest(hi, slot.End)
}

// pgLocalInstant is the instant Postgres gives the wall clock on that local date in loc
// ((date + clock) AT TIME ZONE tz): an ordinary clock is the instant showing it; a clock the
// change repeats is its later occurrence, and a clock it skips is read with the offset of
// either side that gives the later instant (02:30 on a New York spring-forward day is 07:30Z,
// 03:30 EDT). time.Date leaves both cases unspecified (it gives the earlier ones), which would put
// a shift edge in such an hour an hour away from where techStretchSQL puts it.
func pgLocalInstant(y int, m time.Month, d int, clock time.Duration, loc *time.Location) time.Time {
	wall := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Add(clock)
	guess := time.Date(y, m, d, 0, 0, 0, 0, loc).Add(clock)
	var shown, later time.Time
	for _, probe := range []time.Time{guess.Add(-3 * time.Hour), guess.Add(3 * time.Hour)} {
		_, offset := probe.In(loc).Zone()
		at := wall.Add(-time.Duration(offset) * time.Second)
		if at.After(later) {
			later = at
		}
		if local := at.In(loc); local.Hour() == wall.Hour() && local.Minute() == wall.Minute() && local.Second() == wall.Second() && at.After(shown) {
			shown = at
		}
	}
	if !shown.IsZero() {
		return shown
	}
	return later
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earliest(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
