package main

import (
	"github.com/google/uuid"
)

// The one-move repair, restated from its documented rule (backend/internal/store/repair.go's
// comment, not its SQL): when no fitting bay (or no qualified on-duty technician) is free for a
// window, a booking may still fit by moving ONE other appointment off a bay (technician) that fits
// the new job onto another bay (technician) that fits the moved one, in the same window — one move
// per missing resource. What may move: a confirmed appointment (status 1: no car in the bay yet)
// that has not started, and that is the only hold of the freed resource overlapping the window.
// Where it may go: another active bay fitting its own booked requirement (its own waiter soft
// upgrade, or the express spill), or another active technician holding its booked skills at its
// level and on duty for its window, no scarcer for it than the one it leaves (level no higher,
// no more spare skills: a move never spends a senior or versatile technician on work a junior
// was doing) — free for its whole window. Availability offers a start
// exactly when the booking would find such moves, so the oracle's verdicts include it.

// fits reports whether r can be placed over w: a fitting bay free (bays > 0) or freed by a move, and
// a qualified on-duty technician free (techs > 0) or freed by a move. exclude is left out.
func (s *oracleShop) fits(r jobRequirement, soft bool, w timeWindow, exclude uuid.UUID, bays, techs int) bool {
	return (bays > 0 || s.bayRepairable(r, soft, w, exclude)) && (techs > 0 || s.techRepairable(r, w, exclude))
}

// movable reports whether h may be moved by a repair now.
func (s *oracleShop) movable(h oHold) bool {
	return !h.phantom && h.status == 1 && h.window.start.After(s.now) && !s.pinned[h.id]
}

// soleHold returns the only appointment holding a resource (owned reports ownership) over w,
// exclude left out; false when none or several do. Its positions are one appointment (a phantom
// next to its live hold is not a second holder); the live one is returned when there is one, so a
// phantom alone is never moved.
func (s *oracleShop) soleHold(owned func(oHold) bool, w timeWindow, exclude uuid.UUID) (oHold, bool) {
	var found oHold
	n := 0
	for _, h := range s.holds {
		if h.id == exclude || !owned(h) || !h.window.overlaps(w) {
			continue
		}
		switch {
		case n == 0:
			found, n = h, 1
		case h.id != found.id:
			return oHold{}, false
		case found.phantom && !h.phantom:
			found = h
		}
	}
	return found, n == 1
}

// freeFor reports whether no hold on a resource (owned) overlaps w, exclude left out. A move's
// target passes uuid.Nil: the appointment being rescheduled still holds its old place when the
// moved one lands, so the target must be free of it too.
func (s *oracleShop) freeFor(owned func(oHold) bool, w timeWindow, exclude uuid.UUID) bool {
	for _, h := range s.holds {
		if h.id != exclude && owned(h) && h.window.overlaps(w) {
			return false
		}
	}
	return true
}

// bayRepairable reports whether a bay fitting r holds only one movable appointment over w and
// another bay fitting that appointment is free for its window.
func (s *oracleShop) bayRepairable(r jobRequirement, soft bool, w timeWindow, exclude uuid.UUID) bool {
	for _, b := range s.bays {
		if !bayFits(r, soft, b.bayType) {
			continue
		}
		x, ok := s.soleHold(func(h oHold) bool { return h.bay == b.id }, w, exclude)
		if !ok || !s.movable(x) {
			continue
		}
		xSoft := softUpgrade(x.req, x.waiter, s.expressMax)
		for _, nb := range s.bays {
			if nb.id != b.id && bayFits(x.req, xSoft, nb.bayType) && s.freeFor(func(h oHold) bool { return h.bay == nb.id }, x.window, uuid.Nil) {
				return true
			}
		}
	}
	return false
}

// techRepairable reports whether a technician qualified and on duty for r over w holds only one
// movable appointment over it and another technician qualified for that appointment is on duty and
// free for its window.
func (s *oracleShop) techRepairable(r jobRequirement, w timeWindow, exclude uuid.UUID) bool {
	for _, t := range s.techs {
		if !qualified(r, t.level, t.skills) || !t.roster.onDuty(w, s.loc) {
			continue
		}
		x, ok := s.soleHold(func(h oHold) bool { return h.tech == t.id }, w, exclude)
		if !ok || !s.movable(x) {
			continue
		}
		for _, nt := range s.techs {
			if nt.id != t.id && qualified(x.req, nt.level, nt.skills) &&
				nt.level <= t.level && spareSkills(x.req, nt.skills) <= spareSkills(x.req, t.skills) &&
				nt.roster.onDuty(x.window, s.loc) &&
				s.freeFor(func(h oHold) bool { return h.tech == nt.id }, x.window, uuid.Nil) {
				return true
			}
		}
	}
	return false
}
