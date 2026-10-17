package integration_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// lifecycleOp is one workshop transition as the matrix drives it.
type lifecycleOp struct {
	name  string
	to    domain.AppointmentStatus
	event domain.EventType
	run   func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error)
}

var lifecycleOps = []lifecycleOp{
	{"cancel", domain.StatusCancelled, domain.EventAppointmentCancelled, func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error) {
		return svc.Cancel(ctx, staffAdmin, id, 0)
	}},
	{"check in", domain.StatusCheckedIn, domain.EventAppointmentCheckedIn, func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error) {
		return svc.CheckIn(ctx, staffAdmin, id, 0)
	}},
	{"start", domain.StatusInProgress, domain.EventAppointmentStarted, func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error) {
		return svc.StartWork(ctx, staffAdmin, id, 0)
	}},
	{"complete", domain.StatusCompleted, domain.EventAppointmentCompleted, func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error) {
		return svc.Complete(ctx, staffAdmin, id, 0)
	}},
	{"no-show", domain.StatusNoShow, domain.EventAppointmentNoShow, func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error) {
		return svc.NoShow(ctx, staffAdmin, id, 0)
	}},
	{"reinstate", domain.StatusCheckedIn, domain.EventAppointmentReinstated, func(ctx context.Context, svc *booking.Service, id uuid.UUID) (domain.Appointment, error) {
		return svc.Reinstate(ctx, staffAdmin, id, 0)
	}},
}

// lifecycleAllowed is invariant 9: confirmed → checked_in → in_progress → completed, cancel from
// confirmed or checked_in, no-show from confirmed (after start), reinstate no-show → checked_in.
var lifecycleAllowed = map[string][]domain.AppointmentStatus{
	"cancel":    {domain.StatusConfirmed, domain.StatusCheckedIn},
	"check in":  {domain.StatusConfirmed},
	"start":     {domain.StatusCheckedIn},
	"complete":  {domain.StatusInProgress},
	"no-show":   {domain.StatusConfirmed},
	"reinstate": {domain.StatusNoShow},
}

// lifecyclePath is how the matrix reaches each status from a fresh booking.
var lifecyclePath = map[domain.AppointmentStatus][]string{
	domain.StatusConfirmed:  nil,
	domain.StatusCheckedIn:  {"check in"},
	domain.StatusInProgress: {"check in", "start"},
	domain.StatusCompleted:  {"check in", "start", "complete"},
	domain.StatusCancelled:  {"cancel"},
	domain.StatusNoShow:     {"no-show"},
}

// Every status × every transition, each on a booking of its own day with the clock 15 minutes
// past its start (so check-in, an on-time start, a no-show, and a reinstate are all due): the
// allowed cells move the status, bump the version by one, and write one outbox and one history
// row; asking for the status the appointment already has is an idempotent no-op; every other cell
// is a 409 conflict that changes nothing and leaves no event behind.
func TestTransitionMatrix(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	byName := map[string]lifecycleOp{}
	for _, op := range lifecycleOps {
		byName[op.name] = op
	}
	statuses := []domain.AppointmentStatus{
		domain.StatusConfirmed, domain.StatusCheckedIn, domain.StatusInProgress,
		domain.StatusCompleted, domain.StatusCancelled, domain.StatusNoShow,
	}
	cell := 0
	for _, from := range statuses {
		for _, op := range lifecycleOps {
			cell++
			day := londonDay(t, cell)
			label := fmt.Sprintf("%s from %s", op.name, from.Name())
			appt := mustBook(t, svc, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 10, 0)))
			late := serviceOn(t, st, day, 10, 15)
			for _, step := range lifecyclePath[from] {
				var err error
				if appt, err = byName[step].run(ctx, late, appt.ID); err != nil {
					t.Fatalf("%s: setup %s: %v", label, step, err)
				}
			}
			if appt.Status != from {
				t.Fatalf("%s: setup reached %s", label, appt.Status.Name())
			}
			before := appointmentEventRows(t, pool, appt.ID)

			got, err := op.run(ctx, late, appt.ID)
			after := appointmentEventRows(t, pool, appt.ID)
			current := mustGet(t, svc, appt.ID)
			switch {
			case slices.Contains(lifecycleAllowed[op.name], from):
				if err != nil || got.Status != op.to || got.Version != appt.Version+1 {
					t.Fatalf("%s: allowed, got %s v%d (was v%d), err %v", label, got.Status.Name(), got.Version, appt.Version, err)
				}
				if outbox, history := eventCount(t, pool, appt.ID, op.event); outbox != 1 || history != 1 {
					t.Fatalf("%s: %s outbox=%d history=%d, want 1 and 1", label, op.event, outbox, history)
				}
				if after != before+2 {
					t.Fatalf("%s: %d new event rows, want 2 (one outbox, one history)", label, after-before)
				}
			case from == op.to:
				if err != nil || got.Version != appt.Version || current.Version != appt.Version {
					t.Fatalf("%s: already there must be a no-op, got v%d (was v%d), err %v", label, current.Version, appt.Version, err)
				}
				if after != before {
					t.Fatalf("%s: a no-op wrote %d event rows", label, after-before)
				}
			default:
				expectCode(t, err, domain.CodeConflict)
				if current.Status != from || current.Version != appt.Version {
					t.Fatalf("%s: refused, but now %s v%d (was v%d)", label, current.Status.Name(), current.Version, appt.Version)
				}
				if after != before {
					t.Fatalf("%s: a refusal wrote %d event rows", label, after-before)
				}
			}
			// Riverside's oil work has one technician and one bay, and either runs one job in
			// progress at a time: finish this cell's job so the next cell can start its own.
			if final := mustGet(t, svc, appt.ID); final.Status == domain.StatusInProgress {
				if _, err := late.Complete(ctx, staffAdmin, appt.ID, 0); err != nil {
					t.Fatalf("%s: cleanup complete: %v", label, err)
				}
			}
		}
	}
	assertNoOverlaps(t, pool)
	assertNoOrphanEvents(t, pool)
}

// appointmentEventRows counts the outbox and history rows of appointment id.
func appointmentEventRows(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int {
	t.Helper()
	return scalar[int](t, pool, `
		SELECT (SELECT count(*) FROM outbox_events WHERE aggregate_id = $1)::int
		     + (SELECT count(*) FROM appointment_history WHERE appointment_id = $1)::int`, id)
}
