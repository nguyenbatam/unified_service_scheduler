package booking

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// ReassignRequest moves an appointment to another technician and/or bay in the same window — the
// dispatcher's drag-and-drop on the day board (an overrun ahead, a technician off sick).
type ReassignRequest struct {
	TechnicianID uuid.UUID // uuid.Nil keeps the current technician
	ServiceBayID uuid.UUID // uuid.Nil keeps the current bay
	IfMatch      int
}

// Reassign is staff-only and works on confirmed or checked-in work. The new technician must hold
// the booked skills at the booked level and be on duty for the window still ahead (re-checked under
// LockBookableTechnician); the new bay must be active at the same dealership and fit the booked
// type (BayFits: express for a short waiter job, general for an express job), and neither target
// may be held by flagged overrunning work that day. GiST rejects a target already held in the
// window.
func (s *Service) Reassign(ctx context.Context, actor domain.Actor, id uuid.UUID, req ReassignRequest) (domain.Appointment, error) {
	if !actor.IsStaff() {
		return domain.Appointment{}, staffOnly()
	}
	if req.TechnicianID == uuid.Nil && req.ServiceBayID == uuid.Nil {
		return domain.Appointment{}, domain.NewError(domain.CodeInvalid, "technicianId or serviceBayId is required")
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
	techID, bayID := appt.TechnicianID, appt.ServiceBayID
	if req.TechnicianID != uuid.Nil {
		techID = req.TechnicianID
	}
	if req.ServiceBayID != uuid.Nil {
		bayID = req.ServiceBayID
	}
	if techID == appt.TechnicianID && bayID == appt.ServiceBayID {
		return appt, nil // already there (idempotent retry)
	}
	if err := domain.CheckIfMatch(req.IfMatch, appt.Version, "appointment"); err != nil {
		return domain.Appointment{}, err
	}
	if appt.Status != domain.StatusConfirmed && appt.Status != domain.StatusCheckedIn {
		return domain.Appointment{}, domain.NewError(domain.CodeConflict,
			"only confirmed or checked-in work can be reassigned (current: "+appt.Status.Name()+")")
	}
	if techID != appt.TechnicianID {
		// Only the part of the window still ahead of now, like Reinstate and Extend: a late,
		// checked-in job may move to a technician whose shift began after its booked start. With
		// nothing ahead (checked in past its end), the new technician must be on duty right now.
		ahead := Interval{Start: appt.StartAt, End: appt.EndAt}
		if now := s.now(); now.After(ahead.Start) {
			ahead.Start = now
			if !ahead.Start.Before(ahead.End) {
				ahead.End = now.Add(time.Minute)
			}
		}
		ok, err := tx.LockBookableTechnician(ctx, store.TechCheck{
			TechnicianID: techID, DealershipID: appt.DealershipID, Skills: appt.RequiredSkills,
			MinLevel: appt.MinSkillLevel, Start: ahead.Start, End: ahead.End,
		})
		if err != nil {
			return domain.Appointment{}, err
		}
		if !ok {
			return domain.Appointment{}, domain.NewError(domain.CodeUnavailable,
				"that technician is not at this dealership, not qualified for the booked work, or not on duty for the slot")
		}
	}
	if bayID != appt.ServiceBayID {
		// The dealership is read (FOR SHARE, so its express-lane limit cannot change before commit)
		// only for a bay change: a technician-only move does not depend on any dealership rule.
		dealer, err := tx.GetDealership(ctx, appt.DealershipID)
		if err != nil {
			return domain.Appointment{}, err
		}
		bay, bayDealer, active, err := tx.ReadBay(ctx, bayID)
		if err != nil {
			return domain.Appointment{}, err
		}
		upgrade := Requirement{BayType: appt.RequiredBayType, DurationMinutes: appt.DurationMinutes}.SoftUpgrade(appt.IsWaiter, dealer.Policy)
		fits := BayFits(bay.BayType, appt.RequiredBayType, upgrade)
		if bayDealer != appt.DealershipID || !active || !fits {
			return domain.Appointment{}, domain.NewError(domain.CodeUnavailable,
				"that bay is not an active bay at this dealership that fits "+appt.RequiredBayType.Name()+" work")
		}
	}
	// Queue with that day's bookings (the window and the vehicle do not change).
	if err := tx.LockDealershipDay(ctx, appt.DealershipID, uuid.Nil, appt.StartAt, appt.EndAt); err != nil {
		return domain.Appointment{}, err
	}
	if err := refuseOverrunHeld(ctx, tx, appt, changed(bayID, appt.ServiceBayID), changed(techID, appt.TechnicianID), appt.EndAt); err != nil {
		return domain.Appointment{}, err
	}
	updated, err := tx.Reassign(ctx, appt.ID, bayID, techID)
	if err != nil {
		if conflict := holdConflict(err, "that bay is already booked in this slot", "that technician is already booked in this slot"); conflict != nil {
			return domain.Appointment{}, conflict
		}
		if store.IsDeadlock(err) {
			return domain.Appointment{}, domain.NewError(domain.CodeConflict, "the target was being booked at the same moment; retry")
		}
		return domain.Appointment{}, err
	}
	if err := tx.RecordAppointmentEvent(ctx, domain.EventAppointmentReassigned, updated, actor.CreatedBy()); err != nil {
		return domain.Appointment{}, err
	}
	return updated, tx.Commit(ctx)
}

// changed is target when it differs from current, else uuid.Nil (not asked about).
func changed(target, current uuid.UUID) uuid.UUID {
	if target == current {
		return uuid.Nil
	}
	return target
}

// refuseOverrunHeld is 409 when bayID or techID (uuid.Nil: skipped) is held by flagged overrunning
// work started before end (store.Tx.OverrunHolds): allocation and availability treat such a job as
// holding its bay and technician until it is completed, so a reassign or reinstate must not put
// a job on the car still on the lift either. The caller holds the dealership-day lock.
func refuseOverrunHeld(ctx context.Context, tx *store.Tx, appt domain.Appointment, bayID, techID uuid.UUID, end time.Time) error {
	if bayID == uuid.Nil && techID == uuid.Nil {
		return nil
	}
	bayHeld, techHeld, err := tx.OverrunHolds(ctx, bayID, techID, appt.ID, end)
	switch {
	case err != nil:
		return err
	case bayHeld:
		return domain.NewError(domain.CodeConflict, "that bay still holds an overrunning job; complete or extend it first")
	case techHeld:
		return domain.NewError(domain.CodeConflict, "that technician is still on an overrunning job; complete or extend it first")
	}
	return nil
}

// History is one page of the audit trail of one appointment, oldest first (staff only).
func (s *Service) History(ctx context.Context, actor domain.Actor, id uuid.UUID, limit int, after *store.HistoryCursor) ([]domain.AppointmentChange, *store.HistoryCursor, error) {
	if !actor.IsStaff() {
		return nil, nil, staffOnly()
	}
	appt, err := s.store.GetAppointment(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !actor.CanAccessAppointment(appt) {
		return nil, nil, notFoundAppointment()
	}
	return s.store.AppointmentHistory(ctx, id, limit, after)
}
