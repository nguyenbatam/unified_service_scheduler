package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// holdDealershipDay takes dealership d's day lock for the local day of at (the key of
// store.dealershipDayLockSQL) in a TX of the test's own and returns its release.
func holdDealershipDay(t *testing.T, pool *pgxpool.Pool, d uuid.UUID, at time.Time) func() {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('dealer-day|' || d.id::text || '|' || ($2::timestamptz AT TIME ZONE d.timezone)::date::text, 0))
		FROM dealerships d WHERE d.id = $1`, d, at); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(release)
	return release
}

// A booking that lost the race for the last free bay and finds nothing free when it re-reads under
// the day lock still gets the one-move repair, as availability (which offers the slot) and a retry
// would: here an oil change spilled into a general bay moves back to the quick lane, freed since.
// The interleaving is pinned: the test holds the day lock while both brake bookings (instances of
// their own, so neither meets the other in a gate and both read before the lock) read the same free
// bay, then lets them through. The loser used to get 409 "booked by a concurrent request".
func TestLostRaceStillMakesRoom(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	d := provisionShop(t, svc, "Lost race")
	lane := addBay(t, svc, d.ID, "Lane", domain.BayExpress)
	general1 := addBay(t, svc, d.ID, "General 1", domain.BayGeneral)
	addBay(t, svc, d.ID, "General 2", domain.BayGeneral)
	addTechnician(t, svc, d.ID, "Lube 1", 1, domain.SkillLubrication)
	addTechnician(t, svc, d.ID, "Lube 2", 1, domain.SkillLubrication)
	addTechnician(t, svc, d.ID, "Brakes 1", 2, domain.SkillBrakes)
	addTechnician(t, svc, d.ID, "Brakes 2", 2, domain.SkillBrakes)
	start := londonOn(t, londonDay(t, 1), 10, 0)

	// The quick lane is taken, so the second oil change spills into General 1; then the lane frees.
	first := mustBook(t, svc, oilAt(d.ID, newVehicle(t, svc), start))
	spilled := mustBook(t, svc, oilAt(d.ID, newVehicle(t, svc), start))
	if first.ServiceBayID != lane || spilled.ServiceBayID != general1 {
		t.Fatalf("setup: oil changes on %s and %s, want Lane then General 1", first.ServiceBayName, spilled.ServiceBayName)
	}
	if _, err := svc.Cancel(ctx, staffAdmin, first.ID, 0); err != nil {
		t.Fatal(err)
	}

	vehicles := []uuid.UUID{newVehicle(t, svc), newVehicle(t, svc)}
	racers := []*booking.Service{booking.New(st, nil, booking.WithClock(startOfLondonDay)), booking.New(st, nil, booking.WithClock(startOfLondonDay))}
	release := holdDealershipDay(t, pool, d.ID, start)
	errs := make([]error, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		raceN(2, func(i int) {
			_, _, errs[i] = racers[i].Book(ctx, staffAdmin, brakeAt(d.ID, vehicles[i], start))
		})
	}()
	awaitLockWaiters(t, pool, "dealer-day", 2)
	release()
	<-done
	expectWinners(t, "two brake jobs, one general bay free and one freed by a move", 2, allLose(errs)...)
	if moved := mustGet(t, svc, spilled.ID); moved.ServiceBayID != lane || !moved.StartAt.Equal(start) {
		t.Fatalf("the spilled oil change is on %s at %s, want moved back to the Lane, same time", moved.ServiceBayName, moved.StartAt)
	}
	wantEvents(t, "moved oil change", pool, spilled.ID, domain.EventAppointmentReassigned, 1)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// A reschedule to another day rewrites its GiST entries on both days, so it queues on both days'
// locks, earliest day first like every writer: here it waits while the day it leaves is locked,
// although its target day is free. And reschedules crossing days in opposite directions — each
// pair swapping starts, today ↔ tomorrow, on a shop with room for both — all complete, cleanly and
// quickly: taking two days each, in day order, they queue and never wait on each other in a cycle.
func TestCrossDayReschedulesLockBothDays(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	d := provisionShop(t, svc, "Cross day")
	for _, name := range []string{"General 1", "General 2"} {
		addBay(t, svc, d.ID, name, domain.BayGeneral)
	}
	addTechnician(t, svc, d.ID, "Brakes 1", 2, domain.SkillBrakes)
	addTechnician(t, svc, d.ID, "Brakes 2", 2, domain.SkillBrakes)
	day1, day2 := londonDay(t, 1), londonDay(t, 2)

	leaving := mustBook(t, svc, brakeAt(d.ID, newVehicle(t, svc), londonOn(t, day1, 8, 0)))
	release := holdDealershipDay(t, pool, d.ID, leaving.StartAt)
	moved := make(chan error, 1)
	go func() {
		_, err := svc.Reschedule(ctx, staffAdmin, leaving.ID, booking.RescheduleRequest{StartAt: londonOn(t, day2, 8, 0), IfMatch: leaving.Version})
		moved <- err
	}()
	awaitLockWaiter(t, pool, "dealer-day")
	select {
	case err := <-moved:
		t.Fatalf("the reschedule did not wait for the day it leaves: %v", err)
	default:
	}
	release()
	if err := <-moved; err != nil {
		t.Fatalf("reschedule after the day it leaves was released: %v", err)
	}

	// Each pair swaps starts across the two days: a today → tomorrow and b tomorrow → today.
	const pairs = 4
	type move struct {
		appt domain.Appointment
		to   time.Time
	}
	var moves []move
	for i := range pairs {
		hour := 10 + 2*i
		a := mustBook(t, svc, brakeAt(d.ID, newVehicle(t, svc), londonOn(t, day1, hour, 0)))
		b := mustBook(t, svc, brakeAt(d.ID, newVehicle(t, svc), londonOn(t, day2, hour, 0)))
		moves = append(moves, move{a, b.StartAt}, move{b, a.StartAt})
	}
	errs := make([]error, len(moves))
	began := time.Now()
	raceN(len(moves), func(i int) {
		_, errs[i] = svc.Reschedule(ctx, staffAdmin, moves[i].appt.ID, booking.RescheduleRequest{StartAt: moves[i].to, IfMatch: moves[i].appt.Version})
	})
	if took := time.Since(began); took > burstBudget {
		t.Fatalf("%d cross-day swaps took %v (deadlocks?)", pairs, took)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("move %d: %v, want every move to fit (room for both on each day)", i, err)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
