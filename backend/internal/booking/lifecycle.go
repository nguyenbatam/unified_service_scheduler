package booking

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// transition describes one workshop state change.
type transition struct {
	verb      string
	from      []domain.AppointmentStatus
	to        domain.AppointmentStatus
	event     domain.EventType
	staffOnly bool
	// check enforces extra timing/role rules after authorization and state checks.
	check func(ctx context.Context, tx *store.Tx, actor domain.Actor, appt domain.Appointment) error
}

// policyFor reads the dealership's policy on the transition's own TX connection: a plain read, so
// transitions never queue behind an admin PATCH, and never a second pool connection, which a pool
// full of transitions each holding one (and a locked appointment row) would never hand out.
func policyFor(ctx context.Context, tx *store.Tx, dealershipID uuid.UUID) (domain.Dealership, *time.Location, error) {
	dealer, err := tx.ReadDealership(ctx, dealershipID)
	if err != nil {
		return domain.Dealership{}, nil, err
	}
	loc, err := loadLocation(dealer.Timezone)
	if err != nil {
		return domain.Dealership{}, nil, err
	}
	return dealer, loc, nil
}

// Cancel: staff from confirmed/checked_in; customers only their own confirmed booking and only
// before the online change cutoff. Work that has started cannot be cancelled (complete it instead).
func (s *Service) Cancel(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error) {
	return s.transition(ctx, actor, id, ifMatch, transition{
		verb:  "cancel",
		from:  []domain.AppointmentStatus{domain.StatusConfirmed, domain.StatusCheckedIn},
		to:    domain.StatusCancelled,
		event: domain.EventAppointmentCancelled,
		check: func(ctx context.Context, tx *store.Tx, actor domain.Actor, appt domain.Appointment) error {
			if !actor.IsStaff() && appt.Status != domain.StatusConfirmed {
				return domain.NewError(domain.CodeConflict, "vehicle is already checked in; please speak to the service desk")
			}
			if actor.IsStaff() {
				return nil
			}
			dealer, _, err := policyFor(ctx, tx, appt.DealershipID)
			if err != nil {
				return err
			}
			return customerMayChange(actor, appt, s.now(), dealer.Policy)
		},
	})
}

// CheckIn is allowed from the appointment's local day onward (early arrival same day is fine)
// until the booked window ends: after that the bay and technician may already be sold to the next
// job, like Reinstate, so the visit is rescheduled instead.
func (s *Service) CheckIn(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error) {
	return s.transition(ctx, actor, id, ifMatch, transition{
		verb:      "check in",
		from:      []domain.AppointmentStatus{domain.StatusConfirmed},
		to:        domain.StatusCheckedIn,
		event:     domain.EventAppointmentCheckedIn,
		staffOnly: true,
		check: func(ctx context.Context, tx *store.Tx, _ domain.Actor, appt domain.Appointment) error {
			_, loc, err := policyFor(ctx, tx, appt.DealershipID)
			if err != nil {
				return err
			}
			now := s.now()
			if DateFrom(now, loc).Before(DateFrom(appt.StartAt, loc)) {
				return domain.NewError(domain.CodeUnavailable, "check-in opens on the appointment day")
			}
			if !now.Before(appt.EndAt) {
				return domain.NewError(domain.CodeUnavailable, "the booked window has ended; reschedule the appointment instead")
			}
			return lockHoldDay(ctx, tx, appt)
		},
	})
}

// StartWork clocks the technician onto the job. A technician works one job at a time and a bay
// holds one car: if an earlier job on either is still in progress (an overrun nobody extended or
// completed), starting is refused until that job is completed. Starting before the booked start
// (pull-ahead: the car is in and the technician is idle) moves the window to [now, now+length)
// so the hold matches when the work really happens; GiST refuses it if the technician, bay, or
// vehicle is booked in that earlier window. Starting after the booked window has ended (checked
// in on time, started late) moves it the same way: the old window is in the past and the next
// job may hold the bay or technician, so keeping it would put two cars on one lift.
func (s *Service) StartWork(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error) {
	return s.transition(ctx, actor, id, ifMatch, transition{
		verb:      "start",
		from:      []domain.AppointmentStatus{domain.StatusCheckedIn},
		to:        domain.StatusInProgress,
		event:     domain.EventAppointmentStarted,
		staffOnly: true,
		check: func(ctx context.Context, tx *store.Tx, _ domain.Actor, appt domain.Appointment) error {
			techBusy, bayBusy, err := tx.LockWorkStart(ctx, appt.TechnicianID, appt.ServiceBayID, appt.ID)
			if err != nil {
				return err
			}
			if techBusy {
				return domain.NewError(domain.CodeConflict, "the technician is still on another job in progress; complete it first")
			}
			if bayBusy {
				return domain.NewError(domain.CodeConflict, "the bay still holds a job in progress; complete it first")
			}
			now := s.now()
			if !now.Before(appt.StartAt) && now.Before(appt.EndAt) {
				return lockHoldDay(ctx, tx, appt)
			}
			early := now.Before(appt.StartAt)
			start := now.Truncate(time.Minute)
			end := start.Add(appt.EndAt.Sub(appt.StartAt))
			// The window moves (bay, technician, and vehicle holds): queue with the bookings of every
			// day it touches — the new window's start and end days and the day it leaves (a checked-in
			// window never crosses midnight: extensions are held to the opening hours), in day order.
			if _, _, err := tx.LockArrival(ctx, store.ArrivalLock{
				VehicleID: appt.VehicleID, DealershipID: appt.DealershipID, At: start, Until: end, Leaving: appt.StartAt,
			}); err != nil {
				return err
			}
			err = tx.SetWindow(ctx, appt.ID, start, end)
			bayMsg, techMsg := "cannot start late: the bay is booked now; reassign this job to a free bay first",
				"cannot start late: the technician is booked now; reassign this job to a free technician first"
			if early {
				bayMsg, techMsg = "cannot start early: the bay is booked before this job's slot; wait for the slot or reassign",
					"cannot start early: the technician is booked before this job's slot; wait for the slot or reassign"
			}
			if conflict := holdConflict(err, bayMsg, techMsg); conflict != nil {
				return conflict
			}
			if store.IsDeadlock(err) {
				return domain.NewError(domain.CodeConflict, "that window was being booked at the same moment; retry")
			}
			return err
		},
	})
}

// lockHoldDay queues a status change between holding statuses (check-in 1 → 2, an on-time start
// 2 → 3) with the bookings of the appointment's day. status is in the GiST predicates, so the
// UPDATE writes new index entries and re-runs the exclusion check: a booking that read stale
// candidates and inserted over this hold waits for this TX (the old tuple may die) while this
// TX's new entry waits for the booking's — a deadlock Postgres breaks only after
// deadlock_timeout, with every booking of the day queued behind the one holding the day lock.
// Releases (complete, cancel, no-show) never wait on anyone's entry and take no lock.
func lockHoldDay(ctx context.Context, tx *store.Tx, appt domain.Appointment) error {
	return tx.LockDealershipDay(ctx, appt.DealershipID, appt.VehicleID, appt.StartAt, appt.EndAt)
}

// Complete releases bay + technician + vehicle (status 4 is outside the exclusion predicate).
func (s *Service) Complete(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error) {
	return s.transition(ctx, actor, id, ifMatch, transition{
		verb:      "complete",
		from:      []domain.AppointmentStatus{domain.StatusInProgress},
		to:        domain.StatusCompleted,
		event:     domain.EventAppointmentCompleted,
		staffOnly: true,
	})
}

// NoShow releases the slot of a customer who never arrived; only once the start time has passed.
func (s *Service) NoShow(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error) {
	return s.transition(ctx, actor, id, ifMatch, transition{
		verb:      "mark as no-show",
		from:      []domain.AppointmentStatus{domain.StatusConfirmed},
		to:        domain.StatusNoShow,
		event:     domain.EventAppointmentNoShow,
		staffOnly: true,
		check: func(_ context.Context, _ *store.Tx, _ domain.Actor, appt domain.Appointment) error {
			if s.now().Before(appt.StartAt) {
				return domain.NewError(domain.CodeUnavailable, "cannot mark a no-show before the appointment start time")
			}
			return nil
		},
	})
}

// Reinstate undoes a no-show when the customer turns up late: back to checked_in on the booked bay
// and technician, while the booked window has not ended yet. It re-enters the holding statuses,
// so GiST rejects it if the bay, technician, or vehicle was re-booked after the no-show; the
// technician must still be active and qualified (deactivation does not wait for no-shows), the
// booked window must still fit that day's calendar (config changes only refuse to strand holding
// appointments, so hours, lunch, or a closure may have changed since the no-show), and its
// arrival hour must still have staging room (the freed place may have been sold meanwhile).
func (s *Service) Reinstate(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error) {
	return s.transition(ctx, actor, id, ifMatch, transition{
		verb:      "reinstate",
		from:      []domain.AppointmentStatus{domain.StatusNoShow},
		to:        domain.StatusCheckedIn,
		event:     domain.EventAppointmentReinstated,
		staffOnly: true,
		check: func(ctx context.Context, tx *store.Tx, _ domain.Actor, appt domain.Appointment) error {
			now := s.now()
			if !now.Before(appt.EndAt) {
				return domain.NewError(domain.CodeUnavailable, "the booked window has ended; book a new appointment instead")
			}
			// Only the part of the window still ahead of now, like the config guards and Extend: a
			// no-show is always past its start, and an edit since then that only touches the
			// window's past (allowed, the row was not holding) must not refuse the reinstate.
			ahead := Interval{Start: appt.StartAt, End: appt.EndAt}
			if now.After(ahead.Start) {
				ahead.Start = now
			}
			ok, err := tx.LockBookableTechnician(ctx, store.TechCheck{
				TechnicianID: appt.TechnicianID, DealershipID: appt.DealershipID,
				Skills: appt.RequiredSkills, MinLevel: appt.MinSkillLevel, Start: ahead.Start, End: ahead.End,
			})
			if err != nil {
				return err
			}
			if !ok {
				return domain.NewError(domain.CodeConflict, "the booked technician is no longer available; book a new appointment instead")
			}
			return s.checkReinstateRules(ctx, tx, appt, ahead)
		},
	})
}

// checkReinstateRules re-applies the day's calendar and the arrival-window cap for an appointment
// re-entering a holding status (Reinstate: appointment row → technician share → dealership share →
// staging window; the lock order Book uses too, see allocate). Every exclusive locker holds a
// single row and never waits afterwards, so this order is safe.
func (s *Service) checkReinstateRules(ctx context.Context, tx *store.Tx, appt domain.Appointment, ahead Interval) error {
	dealer, err := tx.GetDealership(ctx, appt.DealershipID)
	if err != nil {
		return err
	}
	loc, err := loadLocation(dealer.Timezone)
	if err != nil {
		return err
	}
	// The calendar is read after the dealership FOR SHARE, so it cannot change before commit.
	if err := checkBookedWindowFits(ctx, tx, dealer, loc, ahead, reinstateMisfits); err != nil {
		return err
	}
	if err := claimStagingRoom(ctx, tx, dealer, loc, appt.VehicleID, appt.StartAt, appt.ID, "; book a new appointment instead"); err != nil {
		return err
	}
	// Under the day lock taken just above: the booked bay or technician may since be held by an
	// overrun flagged after the no-show (GiST knows only its end_at).
	return refuseOverrunHeld(ctx, tx, appt, appt.ServiceBayID, appt.TechnicianID, ahead.End)
}

// misfitMessages are the 422 messages of checkBookedWindowFits for one caller.
type misfitMessages struct {
	hours, lunch string
}

var (
	reinstateMisfits = misfitMessages{
		hours: "the booked window is now outside the dealership's opening hours; book a new appointment instead",
		lunch: "the booked window now overlaps the dealership lunch break; book a new appointment instead",
	}
	extendMisfits = misfitMessages{
		hours: "cannot extend past the dealership's closing time; book the remaining work as a new appointment",
		lunch: "cannot extend into the dealership lunch break; book the remaining work as a new appointment",
	}
)

// checkBookedWindowFits refuses a window that does not fit its local day: a closure or closed
// weekday, outside the opening hours that now apply, or across lunch — the same wall-clock rules
// as Tx.refuseStranded for holding appointments (dayClocks), so a window it accepts is never one a
// later config change counts as stranded. The caller holds the dealership FOR SHARE.
func checkBookedWindowFits(ctx context.Context, tx *store.Tx, dealer domain.Dealership, loc *time.Location, window Interval, msg misfitMessages) error {
	day := DateFrom(window.Start, loc)
	hours, err := dayHoursFor(ctx, tx, dealer, day)
	if err != nil {
		return err
	}
	if hours.Closed {
		return closedError(day, hours)
	}
	clocks, err := hours.clocks()
	if err != nil {
		return err
	}
	local := localOf(window, loc)
	if !clocks.withinHours(local) {
		return domain.NewError(domain.CodeUnavailable, msg.hours)
	}
	if clocks.crossesLunch(local) {
		return domain.NewError(domain.CodeUnavailable, msg.lunch)
	}
	return nil
}

// MarkNoShows is the background sweeper: confirmed appointments not checked in within their
// dealership's no-show grace after start.
func (s *Service) MarkNoShows(ctx context.Context) (int, error) {
	return s.store.MarkNoShows(ctx, s.now(), 100)
}

func (s *Service) transition(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int, t transition) (domain.Appointment, error) {
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return domain.Appointment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	appt, err := tx.LockAppointment(ctx, id)
	if err != nil {
		return domain.Appointment{}, err
	}
	if !actor.CanAccessAppointment(appt) {
		return domain.Appointment{}, notFoundAppointment()
	}
	if t.staffOnly && !actor.IsStaff() {
		return domain.Appointment{}, staffOnly()
	}
	if appt.Status == t.to {
		return appt, nil // idempotent retry: already there
	}
	if err := domain.CheckIfMatch(ifMatch, appt.Version, "appointment"); err != nil {
		return domain.Appointment{}, err
	}
	if !slices.Contains(t.from, appt.Status) {
		return domain.Appointment{}, domain.NewError(domain.CodeConflict,
			"cannot "+t.verb+" an appointment that is "+appt.Status.Name())
	}
	if t.check != nil {
		if err := t.check(ctx, tx, actor, appt); err != nil {
			return domain.Appointment{}, err
		}
	}
	updated, err := tx.SetStatus(ctx, appt.ID, t.to)
	if err != nil {
		// Only a move back into a holding status (reinstate) can hit the exclusion constraints.
		if conflict := holdConflict(err, "the bay has been re-booked over this slot", "the technician has been re-booked over this slot"); conflict != nil {
			return domain.Appointment{}, conflict
		}
		if store.IsDeadlock(err) {
			// Racing a booking of the same slot on a GiST entry: a lost race, not a server error.
			return domain.Appointment{}, domain.NewError(domain.CodeConflict, "the slot was being booked at the same moment; reload and retry")
		}
		return domain.Appointment{}, err
	}
	if err := tx.RecordAppointmentEvent(ctx, t.event, updated, actor.CreatedBy()); err != nil {
		return domain.Appointment{}, err
	}
	return updated, tx.Commit(ctx)
}

// Extend stretches a job so the bay/technician stay held and the next slot is not sold over a car
// that is still in the workshop. GiST rejects the extension if the next booking already holds the
// bay, technician, or vehicle.
//
// In-progress work is an overrun (the car is on the lift): only GiST and the 24h span apply, as
// for the overrun sweeper. Checked-in work has not started, so the longer window [start, end+by)
// must still be bookable: the technician on duty for all of it (LockBookableTechnician against the
// booked snapshot, so a shift end, break, or absence refuses it) and the day's calendar (closure,
// hours, lunch; read after the dealership FOR SHARE). Otherwise a checked-in hold could run past
// the shift or across lunch/closing, and refuseStranded / refuseStrandedTechnician would then
// block every roster and calendar edit that touches it. Lock order: appointment row FOR NO KEY
// UPDATE → technician and dealership share locks → vehicle + dealership-day locks of the longer
// window (both days when it crosses midnight) → GiST update (see allocate).
func (s *Service) Extend(ctx context.Context, actor domain.Actor, id uuid.UUID, minutes, ifMatch int) (domain.Appointment, error) {
	if !actor.IsStaff() {
		return domain.Appointment{}, staffOnly()
	}
	by := time.Duration(minutes) * time.Minute
	if by <= 0 || by > MaxExtend || by%ExtendStep != 0 {
		return domain.Appointment{}, domain.NewError(domain.CodeInvalid, "minutes must be a multiple of 15 between 15 and 240")
	}
	tx, err := s.store.Begin(ctx)
	if err != nil {
		return domain.Appointment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	appt, err := tx.LockAppointment(ctx, id)
	if err != nil {
		return domain.Appointment{}, err
	}
	if !actor.CanAccessAppointment(appt) {
		return domain.Appointment{}, notFoundAppointment()
	}
	if err := domain.CheckIfMatch(ifMatch, appt.Version, "appointment"); err != nil {
		return domain.Appointment{}, err
	}
	if appt.Status != domain.StatusCheckedIn && appt.Status != domain.StatusInProgress {
		return domain.Appointment{}, domain.NewError(domain.CodeConflict,
			"only checked-in or in-progress work can be extended (current: "+appt.Status.Name()+")")
	}
	end := appt.EndAt.Add(by)
	if end.Sub(appt.StartAt) > store.MaxAppointmentSpan {
		return domain.Appointment{}, domain.NewError(domain.CodeUnavailable,
			"an appointment cannot span more than 24 hours; book the remaining work as a new appointment")
	}
	if appt.Status == domain.StatusCheckedIn {
		if err := checkExtendable(ctx, tx, appt, s.now(), end); err != nil {
			return domain.Appointment{}, err
		}
	}
	// The window grows (bay, technician, and vehicle holds): queue with that day's bookings.
	if err := tx.LockDealershipDay(ctx, appt.DealershipID, appt.VehicleID, appt.StartAt, end); err != nil {
		return domain.Appointment{}, err
	}
	updated, err := tx.ExtendEnd(ctx, appt.ID, end, s.now())
	if err != nil {
		if conflict := holdConflict(err, "the bay is booked right after; move the next appointment first",
			"the technician is booked right after; reassign the next job first"); conflict != nil {
			return domain.Appointment{}, conflict
		}
		if store.IsDeadlock(err) {
			return domain.Appointment{}, domain.NewError(domain.CodeConflict, "the next slot was being booked at the same moment; retry")
		}
		return domain.Appointment{}, err
	}
	if err := tx.RecordAppointmentEvent(ctx, domain.EventAppointmentExtended, updated, actor.CreatedBy()); err != nil {
		return domain.Appointment{}, err
	}
	return updated, tx.Commit(ctx)
}

// checkExtendable re-checks the part still ahead of now of a checked-in appointment's longer
// window [start, end): the technician (share lock, then the roster in a fresh snapshot) and the
// day's calendar (dealership share lock, then the calendar read). Only the part ahead of now, like
// the config guards (Tx.refuseStranded*): a roster or hours edit made since check-in that only
// touches the window's past must not refuse the extension. Both refusals are 422: the job may
// still be extended by less, or the rest booked as a new appointment.
func checkExtendable(ctx context.Context, tx *store.Tx, appt domain.Appointment, now, end time.Time) error {
	from := appt.StartAt
	if now.After(from) {
		from = now
	}
	if !from.Before(end) {
		return nil // the whole extended window is already in the past: nothing ahead to re-check
	}
	ok, err := tx.LockBookableTechnician(ctx, store.TechCheck{
		TechnicianID: appt.TechnicianID, DealershipID: appt.DealershipID,
		Skills: appt.RequiredSkills, MinLevel: appt.MinSkillLevel, Start: from, End: end,
	})
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewError(domain.CodeUnavailable,
			"the technician is not on duty for the extended window (shift end, break, or absence); extend by less or reassign")
	}
	dealer, err := tx.GetDealership(ctx, appt.DealershipID)
	if err != nil {
		return err
	}
	loc, err := loadLocation(dealer.Timezone)
	if err != nil {
		return err
	}
	return checkBookedWindowFits(ctx, tx, dealer, loc, Interval{Start: from, End: end}, extendMisfits)
}

// holdConflict maps a GiST exclusion violation on a status/window change to a 409 with a
// resource-specific message, or returns nil when err is not one.
func holdConflict(err error, bayMsg, techMsg string) error {
	switch store.ExclusionConstraint(err) {
	case store.ConstraintBayOverlap:
		return domain.NewError(domain.CodeConflict, bayMsg)
	case store.ConstraintTechOverlap:
		return domain.NewError(domain.CodeConflict, techMsg)
	case store.ConstraintVehicleOverlap:
		return vehicleOverlap()
	}
	return nil
}
