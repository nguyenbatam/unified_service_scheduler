package booking

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

type BookRequest struct {
	DealershipID   uuid.UUID
	VehicleID      uuid.UUID
	ServiceTypeID  uuid.UUID   // a single job line, or
	ServiceTypeIDs []uuid.UUID // several job lines (order kept: line 1 is the primary operation)
	StartAt        time.Time
	IsWaiter       bool
	IdempotencyKey string
}

type RescheduleRequest struct {
	StartAt  time.Time
	IsWaiter *bool // nil keeps the current value
	IfMatch  int   // 0 = no precondition
}

func (s *Service) Book(ctx context.Context, actor domain.Actor, req BookRequest) (domain.Appointment, bool, error) {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.DealershipID == uuid.Nil || req.VehicleID == uuid.Nil || req.StartAt.IsZero() {
		return domain.Appointment{}, false, domain.NewError(domain.CodeInvalid, "dealership, vehicle, service type, and start time are required")
	}
	ids, err := serviceIDs(req.ServiceTypeID, req.ServiceTypeIDs)
	if err != nil {
		return domain.Appointment{}, false, err
	}
	if len(req.IdempotencyKey) > 200 {
		return domain.Appointment{}, false, domain.NewError(domain.CodeInvalid, "idempotency key is too long")
	}
	scope := idempotencyScope(actor)
	fp := fingerprint(req, ids)

	if req.IdempotencyKey != "" && s.idem != nil {
		// Redis is only an accelerator: on error fall through to the authoritative Postgres lookup.
		rec, ok, err := s.idem.Get(ctx, scope, req.IdempotencyKey)
		if err == nil && ok {
			existing, err := s.store.GetAppointment(ctx, rec.AppointmentID)
			if err == nil {
				if !actor.CanAccessAppointment(existing) {
					// Outside the actor's scope now (e.g. a dispatcher since moved to another dealership):
					// checked before the fingerprint so a 409 never confirms the key exists.
					return domain.Appointment{}, false, notFoundAppointment()
				}
				if rec.Fingerprint != fp {
					return domain.Appointment{}, false, idempotencyMismatch()
				}
				return existing, false, nil
			}
			// Cache points at a row that is gone (e.g. test DB reset): fall through to Postgres.
		}
	}

	// Before taking a pool connection: at most dayGateWidth bookings of this dealership-day per
	// instance wait on its database lock; the rest wait here without holding a connection.
	release, contended, err := s.gate.enter(ctx, s.gateKey(ctx, req.DealershipID, req.StartAt))
	if err != nil {
		return domain.Appointment{}, false, err
	}
	defer release()

	tx, err := s.store.Begin(ctx)
	if err != nil {
		return domain.Appointment{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// One round trip: with a key, its advisory lock and lookup first — requests carrying the same
	// (user, key) serialise there, so a client retry racing its own in-flight original waits, then
	// sees the committed row and replays it instead of colliding with it on the GiST constraints
	// (409) before the unique index is checked — then the dealership FOR SHARE, the day's calendar,
	// vehicle, and service types (plain read: snapshotted). The checks below run in the order a read
	// at a time would (scope before revealing which later row is missing).
	var idem *store.IdempotencyLookup
	if req.IdempotencyKey != "" {
		idem = &store.IdempotencyLookup{Scope: scope, Key: req.IdempotencyKey, CreatedBy: actor.CreatedBy()}
	}
	in, err := tx.BookingInputs(ctx, idem, req.DealershipID, req.VehicleID, ids, req.StartAt)
	if err != nil {
		return domain.Appointment{}, false, err
	}
	if in.Existing != nil {
		return s.replay(ctx, tx, actor, scope, *in.Existing, req, ids, fp)
	}
	if in.Dealer == nil {
		return domain.Appointment{}, false, domain.NewError(domain.CodeNotFound, "dealership not found")
	}
	dealer := *in.Dealer
	if !actor.CanAccessDealership(dealer.ID) {
		return domain.Appointment{}, false, otherDealership()
	}
	if in.Vehicle == nil || !actor.CanAccessCustomer(in.Vehicle.CustomerID) {
		return domain.Appointment{}, false, notFoundVehicle()
	}
	vehicle := *in.Vehicle
	if in.Services == nil {
		return domain.Appointment{}, false, domain.NewError(domain.CodeNotFound, "service type not found")
	}
	requirement, err := Combine(in.Services)
	if err != nil {
		return domain.Appointment{}, false, err
	}
	plan, err := s.planSlot(ctx, tx, actor, dealer, in.Day, requirement.DurationMinutes, req.StartAt)
	if err != nil {
		return domain.Appointment{}, false, err
	}

	var key *string
	if req.IdempotencyKey != "" {
		key = &req.IdempotencyKey
	}
	saved, err := s.allocate(ctx, tx, allocation{
		plan:           plan,
		dealer:         dealer,
		vehicle:        vehicle,
		req:            requirement,
		isWaiter:       req.IsWaiter,
		idempotencyKey: key,
		fingerprint:    fp,
		by:             actor.CreatedBy(),
		eventType:      domain.EventAppointmentCreated,
		lockFirst:      s.readsUnderLock(contended, dealer.ID),
		repairBy:       staffBy(actor),
	})
	if err != nil {
		return domain.Appointment{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Appointment{}, false, err
	}
	s.rememberKey(ctx, scope, req.IdempotencyKey, fp, saved.ID)
	return saved, true, nil
}

// Reschedule moves the appointment in place (same id and number, version bumped) to a new slot,
// re-allocating bay and technician for its booked snapshot (lines, labour time, requirements —
// catalog edits since booking do not re-price it). One TX: if the new slot cannot be allocated
// nothing changes, so the customer never loses the original. Its own current hold is excluded
// from every check, so it may move to an overlapping slot.
func (s *Service) Reschedule(ctx context.Context, actor domain.Actor, id uuid.UUID, req RescheduleRequest) (domain.Appointment, error) {
	if req.StartAt.IsZero() {
		return domain.Appointment{}, domain.NewError(domain.CodeInvalid, "startAt is required")
	}
	// A reschedule queues on the target day's lock like a booking, so it passes the same gate
	// before taking a pool connection (customers can reschedule in bursts too). The dealership is
	// read first, outside any TX; an unknown id skips the gate and gets its 404 from the TX.
	contended := false
	if dealershipID, tz, found, err := s.store.AppointmentDealership(ctx, id); err != nil {
		return domain.Appointment{}, err
	} else if found {
		var release func()
		if release, contended, err = s.gate.enter(ctx, s.gateKeyIn(dealershipID, tz, req.StartAt)); err != nil {
			return domain.Appointment{}, err
		}
		defer release()
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
	isWaiter := appt.IsWaiter
	if req.IsWaiter != nil {
		isWaiter = *req.IsWaiter
	}
	if appt.Status == domain.StatusConfirmed && sameMinute(appt.StartAt, req.StartAt) && appt.IsWaiter == isWaiter {
		// Already there: a retry of a reschedule that committed (its If-Match is now stale).
		return appt, nil
	}
	if err := domain.CheckIfMatch(req.IfMatch, appt.Version, "appointment"); err != nil {
		return domain.Appointment{}, err
	}
	if appt.Status != domain.StatusConfirmed {
		return domain.Appointment{}, domain.NewError(domain.CodeConflict,
			"only confirmed appointments can be rescheduled (current: "+appt.Status.Name()+")")
	}
	// One round trip: dealership FOR SHARE and vehicle (checked in the order one read at a time would).
	in, err := tx.MoveInputs(ctx, appt.DealershipID, appt.VehicleID, req.StartAt)
	if err != nil {
		return domain.Appointment{}, err
	}
	if in.Dealer == nil {
		return domain.Appointment{}, domain.NewError(domain.CodeNotFound, "dealership not found")
	}
	dealer := *in.Dealer
	if err := customerMayChange(actor, appt, s.now(), dealer.Policy); err != nil {
		return domain.Appointment{}, err
	}
	if in.Vehicle == nil {
		return domain.Appointment{}, notFoundVehicle()
	}
	vehicle := *in.Vehicle
	requirement := bookedRequirement(appt)
	plan, err := s.planSlot(ctx, tx, actor, dealer, in.Day, requirement.DurationMinutes, req.StartAt)
	if err != nil {
		return domain.Appointment{}, err
	}
	// Only the waiter flag changed: the window stays, so this is a reassign (maybe another bay), not
	// a move the customer is told about.
	eventType := domain.EventAppointmentRescheduled
	if sameMinute(appt.StartAt, req.StartAt) {
		eventType = domain.EventAppointmentReassigned
	}
	saved, err := s.allocate(ctx, tx, allocation{
		plan:      plan,
		dealer:    dealer,
		vehicle:   vehicle,
		req:       requirement,
		isWaiter:  isWaiter,
		by:        actor.CreatedBy(),
		moving:    &appt,
		eventType: eventType,
		lockFirst: s.readsUnderLock(contended, dealer.ID),
		repairBy:  staffBy(actor),
	})
	if err != nil {
		return domain.Appointment{}, err
	}
	return saved, tx.Commit(ctx)
}

// slotPlan is a validated, grid-aligned slot in dealership time.
type slotPlan struct {
	slot   Interval
	day    Date
	loc    *time.Location
	lo, hi time.Time // the bookable stretch around the slot (dayClocks.stretch), for ranking
}

// planSlot validates start against that day's calendar (closures, weekday hours, lunch) as read
// in the booking TX, after the dealership row was share-locked, and against the dealership's
// self-service policy for the actor. preread is the calendar the inputs batch read for start's
// day (store.DayRulesAt); it is used when it names the same day Go computes, else the day is read
// again here.
func (s *Service) planSlot(ctx context.Context, tx *store.Tx, actor domain.Actor, dealer domain.Dealership, preread store.DayRulesAt, durationMinutes int, start time.Time) (slotPlan, error) {
	loc, err := loadLocation(dealer.Timezone)
	if err != nil {
		return slotPlan{}, err
	}
	day := DateFrom(start, loc)
	var hours DayHours
	if preread.Day == day.String() {
		hours = ResolveDay(dealer, day, preread.Rules)
	} else if hours, err = dayHoursFor(ctx, tx, dealer, day); err != nil {
		return slotPlan{}, err
	}
	if hours.Closed {
		return slotPlan{}, closedError(day, hours)
	}
	clocks, err := hours.clocks()
	if err != nil {
		return slotPlan{}, err
	}
	slot, ok := MatchingSlot(windows(clocks, durationMinutes, day, loc), start)
	if !ok {
		// Off the grid is a malformed request; on the grid but outside today's hours is a slot that
		// is not (or no longer, e.g. after an hours change) bookable, like a closure or lunch.
		if !onSlotGrid(hours, day, loc, start) {
			return slotPlan{}, domain.NewError(domain.CodeInvalid, "start time is off the 30-minute grid")
		}
		// A job no weekday can hold is a malformed request too (400, as availability says), not a
		// slot that happens to be taken; read only on this refusal path.
		if err := s.checkFitsSomeWeekday(ctx, tx, dealer, durationMinutes); err != nil {
			return slotPlan{}, err
		}
		return slotPlan{}, domain.NewError(domain.CodeUnavailable, "start time is outside opening hours")
	}
	if reason := tooEarly(actor, slot.Start, s.now(), dealer.Policy); reason != "" {
		return slotPlan{}, tooEarlyError(reason, dealer.Policy)
	}
	if clocks.crossesLunch(localOf(slot, loc)) {
		return slotPlan{}, domain.NewError(domain.CodeUnavailable, "slot overlaps dealership lunch break")
	}
	lo, hi := clocks.stretch(slot, day, loc)
	return slotPlan{slot: slot, day: day, loc: loc, lo: lo, hi: hi}, nil
}

// checkFitsSomeWeekday reads the weekday overrides in tx and applies fitsSomeWeekday.
func (s *Service) checkFitsSomeWeekday(ctx context.Context, tx *store.Tx, dealer domain.Dealership, durationMinutes int) error {
	rows, err := tx.WeeklyHours(ctx, dealer.ID)
	if err != nil {
		return err
	}
	weekly := make(map[int16]domain.WeekdayHours, len(rows))
	for _, h := range rows {
		weekly[h.Weekday] = h
	}
	return fitsSomeWeekday(dealer, weekly, durationMinutes)
}

// replay answers a reused key: the same original request replays the appointment as it is now
// (even if moved or cancelled since); a different request is a 409. The replay is still an
// appointment read, so it is scoped like one: 404 once the appointment is outside the actor's
// reach (e.g. its dispatcher has since been moved to another dealership). The TX is ended before
// Redis is touched, so a slow cache never stretches a TX holding the idempotency lock.
func (s *Service) replay(ctx context.Context, tx *store.Tx, actor domain.Actor, scope string, existing domain.Appointment, req BookRequest, ids []uuid.UUID, fp string) (domain.Appointment, bool, error) {
	if !actor.CanAccessAppointment(existing) {
		return domain.Appointment{}, false, notFoundAppointment()
	}
	same := existing.IdempotencyFingerprint == fp
	if existing.IdempotencyFingerprint == "" {
		same = sameBooking(existing, req, ids) // rows booked before fingerprints were stored
	}
	if !same {
		return domain.Appointment{}, false, idempotencyMismatch()
	}
	_ = tx.Rollback(ctx) // read-only so far: nothing to keep
	s.rememberKey(ctx, scope, req.IdempotencyKey, fp, existing.ID)
	return existing, false, nil
}

// rememberKey is best effort: Postgres stays authoritative when Redis is down.
func (s *Service) rememberKey(ctx context.Context, scope, key, fp string, id uuid.UUID) {
	if s.idem == nil || key == "" {
		return
	}
	_ = s.idem.Put(ctx, scope, key, cache.IdempotencyRecord{AppointmentID: id, Fingerprint: fp})
}

// staffBy is the acting user when staff, else nil (see allocation.repairBy).
func staffBy(actor domain.Actor) *uuid.UUID {
	if !actor.IsStaff() {
		return nil
	}
	return actor.CreatedBy()
}

func idempotencyScope(actor domain.Actor) string {
	if actor.UserID == uuid.Nil {
		return "system"
	}
	return actor.UserID.String()
}

// fingerprint identifies the booking payload so a reused key with a different body is rejected.
func fingerprint(req BookRequest, ids []uuid.UUID) string {
	lines := make([]string, len(ids))
	for i, id := range ids {
		lines[i] = id.String()
	}
	return fmt.Sprintf("%s|%s|%s|%d|%t", req.DealershipID, req.VehicleID, strings.Join(lines, ","), req.StartAt.Unix()/60, req.IsWaiter)
}

func sameBooking(existing domain.Appointment, req BookRequest, ids []uuid.UUID) bool {
	return existing.DealershipID == req.DealershipID &&
		existing.VehicleID == req.VehicleID &&
		slices.Equal(lineServiceIDs(existing), ids) &&
		sameMinute(existing.StartAt, req.StartAt) &&
		existing.IsWaiter == req.IsWaiter
}

func sameMinute(a, b time.Time) bool {
	return a.Unix()/60 == b.Unix()/60
}

func idempotencyMismatch() error {
	return domain.NewError(domain.CodeConflict, "idempotency key was already used for a different booking")
}

func vehicleOverlap() error {
	return domain.NewError(domain.CodeConflict, "this vehicle already has an appointment overlapping that time")
}
