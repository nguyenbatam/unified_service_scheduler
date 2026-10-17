package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Riverside oil has one express bay and one lubrication technician (Eve): two different
// appointments moving into the same free slot cannot both land there.
func TestTwoReschedulesIntoOneFreeSlotOneWins(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 5; offset++ {
		day := londonDay(t, offset)
		a, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 8, 0)))
		if err != nil {
			t.Fatal(err)
		}
		b, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonOn(t, day, 9, 0)))
		if err != nil {
			t.Fatal(err)
		}
		target := londonOn(t, day, 10, 0)
		appts := []domain.Appointment{a, b}
		errs := make([]error, 2)
		raceN(2, func(i int) {
			_, errs[i] = svc.Reschedule(ctx, staffAdmin, appts[i].ID, booking.RescheduleRequest{StartAt: target, IfMatch: appts[i].Version})
		})
		oneWinner(t, day.String(), allLose(errs, refusals...)...)
		for i, appt := range appts {
			now, err := svc.GetAppointment(ctx, staffAdmin, appt.ID)
			if err != nil {
				t.Fatal(err)
			}
			moved := 0
			wantStart := appt.StartAt
			if errs[i] == nil {
				moved, wantStart = 1, target
			}
			if !now.StartAt.Equal(wantStart) || now.Version != appt.Version+moved {
				t.Fatalf("%s: appointment %d at %s v%d, want %s v%d", day, i, now.StartAt, now.Version, wantStart, appt.Version+moved)
			}
			wantEvents(t, day.String(), pool, appt.ID, domain.EventAppointmentRescheduled, moved)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// A cancel and a reschedule of one appointment at once. With the same If-Match exactly one wins
// (the other gets 409/412); without it they serialise, and the row must match the last change.
func TestCancelRacingRescheduleSameAppointment(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 6; offset++ {
		withIfMatch := offset%2 == 1
		day := londonDay(t, offset)
		orig, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 8, 0)))
		if err != nil {
			t.Fatal(err)
		}
		ifMatch := 0
		if withIfMatch {
			ifMatch = orig.Version
		}
		target := londonOn(t, day, 11, 0)
		var cancelErr, moveErr error
		race(func() {
			_, cancelErr = svc.Cancel(ctx, staffAdmin, orig.ID, ifMatch)
		}, func() {
			_, moveErr = svc.Reschedule(ctx, staffAdmin, orig.ID, booking.RescheduleRequest{StartAt: target, IfMatch: ifMatch})
		})
		// With If-Match exactly one wins. Without it both may: they serialise, the move first (a
		// cancelled appointment can never be moved). Never neither.
		want := 1
		if !withIfMatch && cancelErr == nil && moveErr == nil {
			want = 2
		}
		expectWinners(t, day.String(), want, loses(cancelErr), loses(moveErr))
		cancelled, moved := cancelErr == nil, moveErr == nil
		now, err := svc.GetAppointment(ctx, staffAdmin, orig.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus, wantStart := domain.StatusConfirmed, orig.StartAt
		if moved {
			wantStart = target
		}
		if cancelled {
			wantStatus = domain.StatusCancelled
		}
		if now.Status != wantStatus || !now.StartAt.Equal(wantStart) || now.Version != orig.Version+want {
			t.Fatalf("%s: row status=%s start=%s v%d after cancel=%v move=%v", day, now.Status.Name(), now.StartAt, now.Version, cancelErr, moveErr)
		}
		wantEvents(t, day.String(), pool, orig.ID, domain.EventAppointmentCancelled, boolInt(cancelled))
		wantEvents(t, day.String(), pool, orig.ID, domain.EventAppointmentRescheduled, boolInt(moved))
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Two dispatchers reassign two different jobs onto the same free technician at once: GiST lets one
// hold the technician, the other gets 409 and stays where it was.
func TestTwoReassignsOntoSameTechnicianOneWins(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	// A third Northside brake technician: Drew learns brakes at level 3 (fixture, before any booking).
	if _, err := pool.Exec(ctx, `UPDATE technicians SET skill_level = `+code(domain.LevelSenior)+` WHERE id = $1`, seed.TechDrew); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO technician_skills (technician_id, skill) VALUES ($1, 2)`, seed.TechDrew); err != nil {
		t.Fatal(err)
	}
	brakeTechs := []uuid.UUID{seed.TechBlair, seed.TechCasey, seed.TechDrew}
	for offset := 1; offset <= 4; offset++ {
		day := londonDay(t, offset)
		start := londonOn(t, day, 10, 0)
		first, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleA, start))
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleB, start))
		if err != nil {
			t.Fatal(err)
		}
		var free uuid.UUID
		for _, tech := range brakeTechs {
			if tech != first.TechnicianID && tech != second.TechnicianID {
				free = tech
			}
		}
		jobs := []domain.Appointment{first, second}
		errs := make([]error, 2)
		raceN(2, func(i int) {
			_, errs[i] = svc.Reassign(ctx, northDispatcher, jobs[i].ID, booking.ReassignRequest{TechnicianID: free, IfMatch: jobs[i].Version})
		})
		oneWinner(t, day.String(), allLose(errs, refusals...)...)
		for i, err := range errs {
			if err != nil {
				expectCode(t, err, domain.CodeConflict)
			}
			now, gerr := svc.GetAppointment(ctx, staffAdmin, jobs[i].ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if (now.TechnicianID == free) != (err == nil) {
				t.Fatalf("%s: job %d on %s after reassign err=%v", day, i, now.TechnicianName, err)
			}
			wantEvents(t, day.String(), pool, jobs[i].ID, domain.EventAppointmentReassigned, boolInt(err == nil))
		}
		onFree := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments
			WHERE technician_id = $1 AND status IN (`+holdingCodes+`) AND tstzrange(start_at, end_at) && tstzrange($2, $3)`,
			free, start, start.Add(90*time.Minute))
		if onFree != 1 {
			t.Fatalf("%s: %d holds on the target technician, want 1", day, onFree)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}
