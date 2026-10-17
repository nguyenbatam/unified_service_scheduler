package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Availability reads everything in two round trips (plain reads from the pool, no locks): first
// what sizes the search, then every day-dependent input for the whole range of days.

// AvailabilityBase is what availability reads before it can size the search. A nil pointer (or
// nil Services: any line unknown) means not found; the caller applies its authorization checks in
// order before saying which, so a missing later row is never revealed to an actor who may not see
// an earlier one.
type AvailabilityBase struct {
	Dealer   *domain.Dealership
	Vehicle  *domain.Vehicle
	Exclude  *domain.Appointment  // nil when not asked for (uuid.Nil) or not found
	Services []domain.ServiceType // in the order of the requested ids
	Weekly   []domain.WeekdayHours
}

// AvailabilityBase reads the dealership, vehicle, the appointment to exclude (when excludeID is
// set), the service types, and the weekday overrides in one round trip.
func (s *Store) AvailabilityBase(ctx context.Context, dealershipID, vehicleID, excludeID uuid.UUID, serviceIDs []uuid.UUID) (AvailabilityBase, error) {
	var out AvailabilityBase
	b := &pgx.Batch{}
	queueOptional(b, &out.Dealer, scanDealership, dealershipSelect+` WHERE id = $1`, dealershipID)
	queueOptional(b, &out.Vehicle, scanVehicle, vehicleSelect+` WHERE v.id = $1`, vehicleID)
	if excludeID != uuid.Nil {
		queueOptional(b, &out.Exclude, scanAppointment, appointmentSelect+` WHERE a.id = $1`, excludeID)
	}
	queueServiceTypes(b, &out.Services, serviceIDs)
	queueWeeklyHours(b, &out.Weekly, dealershipID)
	return out, sendBatch(ctx, s.pool, b)
}

// RangeQuery is the day-dependent part of an availability search over the local days
// [FromDay, ToDay] = instants [From, To).
type RangeQuery struct {
	DealershipID uuid.UUID
	VehicleID    uuid.UUID
	Exclude      uuid.UUID // the appointment being rescheduled (uuid.Nil = none)
	FromDay      string    // YYYY-MM-DD
	ToDay        string
	From, To     time.Time
	// StagingTo ends the arrivals read [From, StagingTo); zero when the dealership has no staging cap.
	StagingTo time.Time
}

// AvailabilityRange is every input availability buckets per day in Go.
type AvailabilityRange struct {
	// Bays and Techs are every active bay and technician of the dealership, not only those that fit
	// the job: a slot with none free may still be bookable by moving a confirmed booking to another
	// bay or technician that fits it (booking's one-move repair), and the booking filters them in
	// Go (booking.BayFits, Requirement.qualifies).
	Bays         []domain.Bay
	Techs        []domain.Technician // with skills and level
	Closures     []domain.Closure
	Busy         []Hold // dealership holds, by start
	VehicleHolds []Busy // the vehicle's holds at any dealership, by start
	Arrivals     []time.Time
	// Rosters covers every technician at the dealership.
	Rosters Rosters
}

// AvailabilityRange reads the bays, technicians, closures, holds, vehicle holds, staging arrivals,
// and rosters for q in one round trip (one queued statement each, e.g. queueConfirmedBusy and
// queueVehicleHolds).
func (s *Store) AvailabilityRange(ctx context.Context, q RangeQuery) (AvailabilityRange, error) {
	out := AvailabilityRange{Rosters: Rosters{Shifts: map[uuid.UUID][]domain.TechnicianShift{}, Absences: map[uuid.UUID][]Busy{}}}
	b := &pgx.Batch{}
	sql, args := baysSQL(q.DealershipID, 0, false)
	queueRows(b, &out.Bays, scanBay, sql, args...)
	queueTechnicians(b, &out.Techs, q.DealershipID)
	queueClosures(b, &out.Closures, q.DealershipID, q.FromDay, q.ToDay)
	queueConfirmedBusy(b, &out.Busy, q.DealershipID, q.From, q.To, q.Exclude)
	queueVehicleHolds(b, &out.VehicleHolds, q.VehicleID, q.From, q.To, q.Exclude)
	if !q.StagingTo.IsZero() {
		queueStagingArrivals(b, &out.Arrivals, q.DealershipID, q.From, q.StagingTo, q.Exclude)
	}
	queueRosters(b, &out.Rosters, `technician_id IN (SELECT id FROM technicians WHERE dealership_id = $1)`, q.DealershipID, q.From, q.To)
	if err := sendBatch(ctx, s.pool, b); err != nil {
		return AvailabilityRange{}, translate(err)
	}
	return out, nil
}
