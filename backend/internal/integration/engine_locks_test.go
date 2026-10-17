package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A transition holding its TX connection (and the appointment row lock) must never need a second
// pool connection: with every connection held by such a TX, none would ever be handed out. Three
// times as many concurrent customer cancels and check-ins as the pool has connections must all
// complete.
func TestTransitionsNeverWaitForASecondPoolConnection(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	const conns = 4
	var cancels, checkIns []uuid.UUID
	for i := range conns * 3 / 2 {
		// Riverside: one express bay, no staging cap, no lunch. Amelia's car tomorrow (customer
		// cancel), VehicleB today after its 10:00–11:30 seed hold (check-in opens on the day).
		c, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 8+i, 0)))
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, c.ID)
		k, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 12+i, 0)))
		if err != nil {
			t.Fatal(err)
		}
		checkIns = append(checkIns, k.ID)
	}

	opts := store.DefaultPoolOptions()
	opts.MaxConns = conns
	small := newPoolLike(t, pool, opts)
	tight := booking.New(store.New(small), nil, booking.WithClock(startOfLondonDay))

	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var done atomic.Int32
	var ops []func()
	run := func(name string, op func() (domain.Appointment, error)) {
		ops = append(ops, func() {
			if _, err := op(); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			done.Add(1)
		})
	}
	for _, id := range cancels {
		run("customer cancel", func() (domain.Appointment, error) { return tight.Cancel(deadline, customerAmelia, id, 0) })
	}
	for _, id := range checkIns {
		run("check-in", func() (domain.Appointment, error) { return tight.CheckIn(deadline, staffDispatcher, id, 0) })
	}
	race(ops...)
	if want := len(cancels) + len(checkIns); int(done.Load()) != want {
		t.Fatalf("completed %d of %d transitions on a %d-connection pool", done.Load(), want, conns)
	}
}

// A booking waiting behind a roster edit (technician FOR UPDATE) must not hold the arrival
// window's staging lock meanwhile: a booking for the same window that cannot use that technician
// goes through at once.
func TestRosterEditDoesNotStallOtherBookingsInTheWindow(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	roster, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roster.Rollback(ctx) }()
	// Blair ranks first for a brake job (level 3, ties Casey on load, then by name).
	if _, err := roster.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`, seed.TechBlair); err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		_, _, err := svc.Book(waitCtx, staffAdmin, booking.BookRequest{
			DealershipID: seed.DealerNorth, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceBrake,
			StartAt: londonAt(t, 14, 0), IdempotencyKey: uuid.NewString(),
		})
		blocked <- err
	}()
	awaitLockWaiter(t, pool, "FOR SHARE")

	// An oil change in the same arrival hour: Alex (level 2) ranks first, Blair cannot do it.
	// Waiting on the locked technician would end in the pool's lock_timeout; give up well before it.
	quick, cancel := context.WithTimeout(ctx, store.DefaultPoolOptions().LockTimeout*2/3)
	defer cancel()
	began := time.Now()
	if _, _, err := svc.Book(quick, staffAdmin, oilAt(seed.DealerNorth, seed.VehicleA, londonAt(t, 14, 0))); err != nil {
		t.Fatalf("booking that cannot use the locked technician waited on it: %v (after %s)", err, time.Since(began))
	}
	select {
	case err := <-blocked:
		t.Fatalf("the brake booking should still be waiting on the technician lock, got %v", err)
	default:
	}
	if err := roster.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-blocked; err != nil {
		t.Fatalf("brake booking after the roster edit ended: %v", err)
	}
}
