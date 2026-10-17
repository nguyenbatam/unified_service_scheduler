package booking

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// overrunBatch bounds how many jobs one sweep handles.
const overrunBatch = 50

// overrunLockTimeout is each overrun TX's lock_timeout (instead of the pool's, 3 s by default): an
// extension that waits on a booking still in flight over the same bay, technician, or vehicle
// (GiST waits for the other TX to finish) gives up quickly and is retried on the next sweep
// (extendRetry).
const overrunLockTimeout = 500 * time.Millisecond

// HandleOverruns stops the next slot from being sold over a car that is still on the lift. Every
// in-progress job whose window has ended is extended in steps until its end is in the future, up
// to MaxExtend beyond its booked labour time; GiST still rejects the extension when the next booking
// already holds the bay, technician, or vehicle. Jobs that cannot be extended are flagged once
// (overrun_at, APPOINTMENT_OVERRUNNING) so the dispatcher moves the next job or completes this one;
// they stay in the sweep, so once the next job is moved the extension resumes (the flag stays: the
// job is still overrunning and holds its bay and technician for the day; Complete or a manual
// Extend clears it).
// Flagged jobs already extended to the cap are left out (store.OverrunCandidates): nothing more can
// happen to them here, and they must not starve extendable jobs of the batch.
//
// Each job is locked and handled in its own short TX (one row lock, at most overrunLockTimeout of
// waiting), so a dispatcher completing one of them never waits behind the rest of the batch, and
// one job's failure neither undoes the others' nor stops the sweep: the next job is still handled,
// and the failures come back joined (each naming its job; the caller logs them) with the counts of
// every job committed.
func (s *Service) HandleOverruns(ctx context.Context, step time.Duration) (extended, flagged int, err error) {
	if step <= 0 {
		return 0, 0, nil
	}
	now := s.now()
	ids, err := s.store.OverrunCandidates(ctx, now, MaxExtend, overrunBatch)
	if err != nil {
		return 0, 0, err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err()) // shutting down: the next sweep picks the rest up
			break
		}
		outcome, err := s.handleOverrun(ctx, id, now, step)
		if err != nil {
			errs = append(errs, fmt.Errorf("overrun %s: %w", id, err))
			continue
		}
		switch outcome {
		case overrunExtended:
			extended++
		case overrunFlagged:
			flagged++
		}
	}
	return extended, flagged, errors.Join(errs...)
}

type overrunOutcome int

const (
	overrunNothing overrunOutcome = iota // gone, locked elsewhere, already alerted, or retry next sweep
	overrunExtended
	overrunFlagged
)

// handleOverrun extends (or flags) one job in its own TX.
func (s *Service) handleOverrun(ctx context.Context, id uuid.UUID, now time.Time, step time.Duration) (overrunOutcome, error) {
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Before any savepoint, so it holds for the whole TX.
	if err := tx.SetLockTimeout(ctx, overrunLockTimeout); err != nil {
		return 0, err
	}
	o, err := tx.LockOverrun(ctx, id, now, MaxExtend)
	if err != nil || o == nil {
		return overrunNothing, err
	}
	var outcome overrunOutcome
	newEnd := overrunEnd(o.EndAt, now, step)
	limit := o.StartAt.Add(time.Duration(o.DurationMinutes)*time.Minute + MaxExtend)
	target := newEnd
	if target.After(limit) {
		target = limit
	}
	result := extendBlocked
	partialDone, dayLocked := false, false
	if o.OverrunAt != nil && target.After(o.EndAt) {
		// A flagged job still blocked right at its end (it was already extended up to the next
		// hold) can do nothing this tick: say so without queueing on the day lock, which it would
		// otherwise take every tick only to roll back. A stale answer is safe: it only defers the
		// resumed extension to the next tick.
		if first, ok, err := tx.FirstHoldAfter(ctx, o.ID, o.EndAt, target); err != nil {
			return 0, err
		} else if ok && !first.After(o.EndAt) {
			return overrunNothing, nil
		}
	}
	if target.After(o.EndAt) {
		// The hold grows (bay, technician, vehicle): queue with the bookings of its day(s), like a
		// manual extend. Waiting past the TX's short lock_timeout means a booking holds the day:
		// try again next sweep.
		if err := tx.LockDealershipDay(ctx, o.DealershipID, o.VehicleID, o.StartAt, target); err != nil {
			if store.IsLockTimeout(err) {
				return overrunNothing, nil
			}
			return 0, err
		}
		dayLocked = true
		if target.Equal(newEnd) {
			// A flagged job keeps its flag when the extension resumes (its blocker moved): it is
			// still past its booked end with the car on the lift, so it keeps holding its bay and
			// technician for the day, and the dispatcher was alerted once. Only Complete or a
			// manual Extend clears it.
			if result, err = s.autoExtend(ctx, tx, *o, newEnd, o.OverrunAt != nil); err != nil {
				return 0, err
			}
		}
		if result == extendBlocked {
			// A whole step does not fit (the next booking, or the cap): still extend as far as it
			// goes, so the bay and technician stay held for as long as possible while the car is on
			// the lift, instead of looking free from the old end on. The job stays flagged.
			upTo := target
			if first, ok, err := tx.FirstHoldAfter(ctx, o.ID, o.EndAt, target); err != nil {
				return 0, err
			} else if ok {
				upTo = first
			}
			if upTo.After(o.EndAt) {
				partial, err := s.autoExtend(ctx, tx, *o, upTo, true)
				if err != nil {
					return 0, err
				}
				switch partial {
				case extendRetry:
					return overrunNothing, nil
				case extendDone:
					partialDone = true
				}
			}
		}
	}
	switch {
	case result == extendDone:
		outcome = overrunExtended
	case result == extendRetry:
		return overrunNothing, nil // lost a race with a booking; the next sweep tries again
	case o.OverrunAt != nil:
		if !partialDone {
			return overrunNothing, nil // already alerted, nothing changed
		}
		outcome = overrunExtended // already alerted; held a little longer
	default:
		// Flagging makes the job hold its bay and technician until it is completed, which
		// Reassign, Reinstate, and the repair read after taking the day lock (Tx.OverrunHolds):
		// queue with that day's writers even when nothing grew (a job already at the cap), so none
		// of them can read "not held" and commit after this flag. Writers of later days are not
		// queued with it; for them the hold is a pre-check like the ranking, and GiST stays the
		// guarantee. The UPDATE also re-checks GiST, like any
		// write to a hold of that day.
		if !dayLocked {
			if err := tx.LockDealershipDay(ctx, o.DealershipID, o.VehicleID, o.StartAt, o.EndAt); err != nil {
				if store.IsLockTimeout(err) {
					return overrunNothing, nil
				}
				return 0, err
			}
		}
		item, err := tx.MarkOverrun(ctx, o.ID)
		if err != nil {
			return 0, err
		}
		if err := tx.RecordAppointmentEvent(ctx, domain.EventAppointmentOverrunning, item, nil); err != nil {
			return 0, err
		}
		outcome = overrunFlagged
	}
	return outcome, tx.Commit(ctx)
}

type extendResult int

const (
	extendDone    extendResult = iota
	extendBlocked              // the next booking holds the bay, technician, or vehicle
	extendRetry                // deadlock or lock timeout with a concurrent booking; try on the next sweep
)

// autoExtend tries one extension in a savepoint so a GiST violation does not abort the sweep.
// keepFlag extends without clearing overrun_at (a partial extension up to the next booking, or any
// extension of a job already flagged); otherwise the step clears it.
func (s *Service) autoExtend(ctx context.Context, tx *store.Tx, o store.Overrun, end time.Time, keepFlag bool) (extendResult, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	var updated domain.Appointment
	if keepFlag {
		updated, err = sp.ExtendEndFlagged(ctx, o.ID, end)
	} else {
		updated, err = sp.ExtendEnd(ctx, o.ID, end, s.now())
	}
	if err != nil {
		_ = sp.Rollback(ctx)
		switch {
		case store.IsExclusionViolation(err), store.IsCheckViolation(err):
			// Next booking in the way, or the 24h span cap: flag it rather than fail every sweep.
			return extendBlocked, nil
		case store.IsDeadlock(err), store.IsLockTimeout(err):
			// Lost a race with (or waited overrunLockTimeout behind) a booking still in flight: it
			// may yet roll back, so neither extend nor flag; retry on the next sweep instead of
			// rolling back the whole batch and its alerts.
			return extendRetry, nil
		}
		return 0, err
	}
	if err := sp.RecordAppointmentEvent(ctx, domain.EventAppointmentExtended, updated, nil); err != nil {
		_ = sp.Rollback(ctx)
		return 0, err
	}
	return extendDone, sp.Commit(ctx)
}

// overrunEnd is the first end, in whole steps after end, that lies strictly after now.
func overrunEnd(end, now time.Time, step time.Duration) time.Time {
	n := now.Sub(end)/step + 1
	return end.Add(n * step)
}
