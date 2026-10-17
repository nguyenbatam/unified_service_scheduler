package integration_test

import (
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The no-show sweep and a late check-in race for the same confirmed appointment (past Riverside's
// 30-minute grace): the row lock (SKIP LOCKED on the sweep's side) lets exactly one of them change
// it, and the database agrees with whichever won.
func TestNoShowSweepRacingCheckInExactlyOneWins(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 6; offset++ {
		day := londonDay(t, offset)
		// A job longer than the no-show grace (30 min), so check-in is still open when the sweep runs.
		job, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceMajor, londonOn(t, day, 9, 0))
		if err != nil {
			t.Fatal(err)
		}
		late := serviceOn(t, st, day, 9, 40)
		var swept int
		var sweepErr, checkInErr error
		race(func() {
			swept, sweepErr = late.MarkNoShows(ctx)
		}, func() {
			_, checkInErr = late.CheckIn(ctx, staffDispatcher, job.ID, 0)
		})
		if sweepErr != nil {
			t.Fatalf("%s: sweep: %v", day, sweepErr)
		}
		status := domain.AppointmentStatus(scalar[int16](t, pool, `SELECT status FROM appointments WHERE id = $1`, job.ID))
		noShows, _ := eventCount(t, pool, job.ID, domain.EventAppointmentNoShow)
		checkIns, _ := eventCount(t, pool, job.ID, domain.EventAppointmentCheckedIn)
		if checkInErr == nil {
			if status != domain.StatusCheckedIn || noShows != 0 || checkIns != 1 {
				t.Fatalf("%s: check-in won but status %s, %d no-show and %d check-in events", day, status.Name(), noShows, checkIns)
			}
		} else {
			expectCode(t, checkInErr, domain.CodeConflict)
			if status != domain.StatusNoShow || noShows != 1 || checkIns != 0 || swept == 0 {
				t.Fatalf("%s: sweep won (%d swept) but status %s, %d no-show and %d check-in events", day, swept, status.Name(), noShows, checkIns)
			}
		}
	}
	assertNoOrphanEvents(t, pool)
}

// overrunJob books Riverside oil 08:00–08:30 on day and checks it in and starts it at 08:00, so
// at 08:40 it is an in-progress job past its end.
func overrunJob(t *testing.T, st *store.Store, svc *booking.Service, day booking.Date) domain.Appointment {
	t.Helper()
	job, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, day, 8, 0))
	if err != nil {
		t.Fatal(err)
	}
	at0800 := serviceOn(t, st, day, 8, 0)
	if _, err := at0800.CheckIn(t.Context(), staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	started, err := at0800.StartWork(t.Context(), staffDispatcher, job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return started
}

// The overrun sweep and a dispatcher's manual 15-minute extension race on the same overrunning job.
// Both may apply (the sweep first, then the manual step on top) or only the manual one (the sweep
// then skips the locked row or finds it no longer due), but no extension is lost: the version and
// end move once per recorded APPOINTMENT_EXTENDED event.
func TestOverrunSweepRacingManualExtend(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 6; offset++ {
		day := londonDay(t, offset)
		job := overrunJob(t, st, svc, day)
		at0840 := serviceOn(t, st, day, 8, 40)
		var extended int
		var sweepErr, extendErr error
		race(func() {
			extended, _, sweepErr = at0840.HandleOverruns(ctx, 15*time.Minute)
		}, func() {
			_, extendErr = at0840.Extend(ctx, staffDispatcher, job.ID, 15, 0)
		})
		if sweepErr != nil || extendErr != nil {
			t.Fatalf("%s: sweep %v, manual extend %v", day, sweepErr, extendErr)
		}
		if extended > 1 {
			t.Fatalf("%s: the sweep extended %d jobs, only one overruns", day, extended)
		}
		events, history := eventCount(t, pool, job.ID, domain.EventAppointmentExtended)
		wantEnd := londonOn(t, day, 8, 45) // manual step only
		if extended == 1 {
			wantEnd = londonOn(t, day, 9, 0) // the sweep's 08:45, then the manual 15 minutes
		}
		version := scalar[int](t, pool, `SELECT version FROM appointments WHERE id = $1`, job.ID)
		end := scalar[time.Time](t, pool, `SELECT end_at FROM appointments WHERE id = $1`, job.ID)
		if events != 1+extended || history != events || version != job.Version+events || !end.Equal(wantEnd) {
			t.Fatalf("%s: swept %d: %d extended events (%d history), version %d (from %d), end %s want %s",
				day, extended, events, history, version, job.Version, end, wantEnd)
		}
		if _, err := at0840.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// The overrun sweep races completing the job: completion always succeeds (a sweep that went first
// only extended it), and a completed job is never extended afterwards.
func TestOverrunSweepRacingComplete(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 6; offset++ {
		day := londonDay(t, offset)
		job := overrunJob(t, st, svc, day)
		at0840 := serviceOn(t, st, day, 8, 40)
		var extended int
		var sweepErr, completeErr error
		race(func() {
			extended, _, sweepErr = at0840.HandleOverruns(ctx, 15*time.Minute)
		}, func() {
			_, completeErr = at0840.Complete(ctx, staffDispatcher, job.ID, 0)
		})
		if sweepErr != nil || completeErr != nil {
			t.Fatalf("%s: sweep %v, complete %v", day, sweepErr, completeErr)
		}
		events, _ := eventCount(t, pool, job.ID, domain.EventAppointmentExtended)
		completed, _ := eventCount(t, pool, job.ID, domain.EventAppointmentCompleted)
		status := domain.AppointmentStatus(scalar[int16](t, pool, `SELECT status FROM appointments WHERE id = $1`, job.ID))
		version := scalar[int](t, pool, `SELECT version FROM appointments WHERE id = $1`, job.ID)
		if status != domain.StatusCompleted || completed != 1 || events != extended || version != job.Version+extended+1 {
			t.Fatalf("%s: swept %d: status %s, %d completed and %d extended events, version %d (from %d)",
				day, extended, status.Name(), completed, events, version, job.Version)
		}
		// A later sweep leaves the completed job alone.
		if n, _, err := serviceOn(t, st, day, 9, 30).HandleOverruns(ctx, 15*time.Minute); err != nil || n != 0 {
			t.Fatalf("%s: a sweep after completion extended %d jobs (%v)", day, n, err)
		}
	}
	assertNoOrphanEvents(t, pool)
}
