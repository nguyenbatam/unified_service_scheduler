package booking

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// One-move repair (store/repair.go has the rules and the SQL). When no bay or no technician is free
// for a slot, the booking moves ONE confirmed, not yet started appointment off a bay (technician)
// that fits the new job onto another bay (technician) that fits it, same window, and takes the
// freed one; a slot short of both makes one move per resource (possibly the same appointment).
// Availability offers a slot exactly when the booking would find such moves (bayRepairable,
// techRepairable here are the Go twins of store.bayRepairSQL and store.techRepairSQL), so the
// two keep agreeing on every start.

// movable is store.movableSQL in Go: a confirmed appointment that has not started (the range read
// already leaves out the appointment being rescheduled).
func movable(h store.Hold, now time.Time) bool {
	return h.Status == domain.StatusConfirmed && h.Start.After(now)
}

// soleHold is the row (in availRange.holds) of the only hold in l overlapping w; false when none
// or several do.
func (l holdList) soleHold(w Interval) (int, bool) {
	lo, hi := l.span(w.Start, w.End)
	ref, found := -1, false
	for k := lo; k < hi; k++ {
		if !Overlaps(l.holds[k], w) {
			continue
		}
		if found {
			return -1, false
		}
		ref, found = l.refs[k], true
	}
	return ref, found
}

// bayRepairable reports whether a bay that fits the job holds only one movable booking in slot,
// and another bay that fits that booking (its own type, waiter upgrade, or express spill) is free
// for its window. Bay holds already carry a flagged overrun to the end of the range (it holds until
// completed), like store.overrunHeldSQL.
func (rng availRange) bayRepairable(in availInputs, slot Interval, now time.Time) bool {
	for i := range rng.bayIDs {
		if !rng.bayFit[i] {
			continue
		}
		ref, ok := rng.bayHolds[i].soleHold(slot)
		if !ok || !movable(rng.holds[ref], now) {
			continue
		}
		x := rng.holds[ref]
		window := Interval{Start: x.Start, End: x.End}
		upgrade := Requirement{BayType: x.RequiredBayType, DurationMinutes: x.DurationMinutes}.SoftUpgrade(x.IsWaiter, in.dealer.Policy)
		for j := range rng.bayIDs {
			if j != i && BayFits(rng.bayTypes[j], x.RequiredBayType, upgrade) && !rng.bayHolds[j].busy(window) &&
				!in.excludedHoldsBay(rng.bayIDs[j], window) {
				return true
			}
		}
	}
	return false
}

// techRepairable reports whether a technician qualified and on duty for slot holds only one
// movable booking in it, and another technician qualified for that booking's booked skills and
// level, no scarcer for it (level, spare skills), is on duty and free for its window.
func (rng availRange) techRepairable(in availInputs, slot Interval, local localWindow, now time.Time) bool {
	for i := range rng.techs {
		if !rng.techFit[i] || !rng.rosters[i].onDuty(slot, local) {
			continue
		}
		ref, ok := rng.techHolds[i].soleHold(slot)
		if !ok || !movable(rng.holds[ref], now) {
			continue
		}
		x := rng.holds[ref]
		window := Interval{Start: x.Start, End: x.End}
		xLocal := localOf(window, in.loc)
		from := rng.techs[i]
		for j, t := range rng.techs {
			if j != i && qualifies(t.SkillLevel, t.Skills, x.RequiredSkills, x.MinSkillLevel) &&
				t.SkillLevel <= from.SkillLevel && spare(t.Skills, x.RequiredSkills) <= spare(from.Skills, x.RequiredSkills) &&
				rng.rosters[j].onDuty(window, xLocal) && !rng.techHolds[j].busy(window) &&
				!in.excludedHoldsTechnician(t.ID, window) {
				return true
			}
		}
	}
	return false
}

// makeRoom is allocate's fallback when nothing free was read for the slot (no bay, no technician,
// or neither): it finds the moves (store.LockRepairs: both searches in one round trip), and when
// every missing resource has one, applies them and returns candidates naming the freed bay and/or
// technician. Moves need the dealership-day lock, so a booking that does not hold it yet first
// probes without locks (a refusal no move could help costs no lock), then takes the vehicle + day
// locks and re-reads its candidates (exact now; something may have been freed meanwhile). The
// staging cap is judged before anything moves, so a refused booking never leaves a move behind
// (its TX rolls back anyway). It runs at most once per allocation: first read, or the re-read
// after a lost race (placement.reread).
func (p *placement) makeRoom(ctx context.Context, cand store.Candidates) (store.Candidates, error) {
	q := store.RepairQuery{
		DealershipID: p.a.dealer.ID, Slot: p.bq.Slot, Now: p.s.now(),
		BayType: p.a.req.BayType, SoftUpgrade: p.bq.SoftUpgrade, Skills: p.a.req.Skills, MinLevel: p.a.req.MinSkillLevel,
	}
	if !p.staged {
		ok, err := p.tx.RepairPossible(ctx, q, len(cand.Bays) == 0, len(cand.Techs) == 0)
		if err != nil {
			return store.Candidates{}, err
		}
		if !ok {
			return store.Candidates{}, nothingFree()
		}
		if p.arrivals, cand, err = p.tx.LockArrivalAndCandidates(ctx, p.a.arrivalLock(), p.a.vehicle.ID, p.bq, p.tq); err != nil {
			return store.Candidates{}, err
		}
		p.staged = true
		if cand.VehicleBusy {
			return store.Candidates{}, vehicleOverlap()
		}
	}
	// Both searches in one round trip (they are independent); each locks the row it would move.
	bayMove, techMove, err := p.tx.LockRepairs(ctx, q, len(cand.Bays) == 0, len(cand.Techs) == 0)
	if err != nil {
		return store.Candidates{}, err
	}
	if (len(cand.Bays) == 0 && bayMove == nil) || (len(cand.Techs) == 0 && techMove == nil) {
		return store.Candidates{}, nothingFree()
	}
	if err := stagingFull(p.a.dealer, p.arrivals, ""); err != nil {
		return store.Candidates{}, err
	}
	if err := p.applyMoves(ctx, bayMove, techMove); err != nil {
		return store.Candidates{}, err
	}
	p.repaired = bayMove != nil || techMove != nil
	if bayMove != nil {
		cand.Bays = []domain.Bay{{ID: bayMove.From}}
	}
	if techMove != nil {
		cand.Techs = []domain.Technician{{ID: techMove.From}}
	}
	return cand, nil
}

// applyMoves reassigns the moved appointment(s) — one UPDATE each, merged when both moves pick the
// same appointment — each with its APPOINTMENT_REASSIGNED event and history row, all in one round
// trip (store.ApplyRepairMoves). The technician target is re-checked under its share lock first,
// like Reassign (LockBookableTechnician against the moved booking's own snapshot and window; NoWait
// under a revocable day lock, like claim). That check stays a round trip of its own: the move's
// foreign-key check would take a key-share lock on the target technician's row and wait for a
// roster writer holding it, which a NoWait check must never do under the day lock.
// Under the day lock nothing else of that day can commit, so GiST refusing a move means a race with
// a writer outside that lock; it surfaces as the usual 409.
func (p *placement) applyMoves(ctx context.Context, bayMove, techMove *store.Repair) error {
	if techMove != nil {
		ok, err := p.tx.LockBookableTechnician(ctx, store.TechCheck{
			TechnicianID: techMove.To, DealershipID: p.a.dealer.ID, Skills: techMove.Skills, MinLevel: techMove.MinLevel,
			Start: techMove.Start, End: techMove.End, NoWait: p.noWait(),
		})
		if errors.Is(err, store.ErrTechnicianLocked) {
			p.lockedTech = techMove.To
		}
		if err != nil {
			return err
		}
		if !ok {
			return nothingFree()
		}
	}
	var moves []store.RepairMove
	if bayMove != nil {
		moves = append(moves, store.RepairMove{ID: bayMove.AppointmentID, ServiceBayID: bayMove.To})
	}
	if techMove != nil {
		if len(moves) == 1 && moves[0].ID == techMove.AppointmentID {
			moves[0].TechnicianID = techMove.To
		} else {
			moves = append(moves, store.RepairMove{ID: techMove.AppointmentID, TechnicianID: techMove.To})
		}
	}
	if err := p.tx.ApplyRepairMoves(ctx, moves, domain.EventAppointmentReassigned, p.a.repairBy); err != nil {
		if store.IsExclusionViolation(err) || store.IsDeadlock(err) {
			return raceLost()
		}
		return err
	}
	return nil
}

// spare counts the skills beyond need (store.spareSkillsSQL).
func spare(skills, need []domain.TechnicianSkill) int {
	n := 0
	for _, sk := range skills {
		if !slices.Contains(need, sk) {
			n++
		}
	}
	return n
}

func nothingFree() error {
	return domain.NewError(domain.CodeUnavailable, "no service bay and qualified technician are free for the full duration")
}

// arrivalLock is the store.ArrivalLock of a booking's vehicle and start: its vehicle and day, and
// the staging count of its arrival window when the dealership caps it.
func arrivalLock(dealer domain.Dealership, loc *time.Location, vehicleID uuid.UUID, start time.Time, exclude uuid.UUID) store.ArrivalLock {
	from, to := ArrivalWindow(start, loc, dealer.StagingWindowMinutes)
	return store.ArrivalLock{
		VehicleID: vehicleID, DealershipID: dealer.ID, At: start, From: from, To: to, Exclude: exclude,
		Count: dealer.StagingCapacity > 0,
	}
}
