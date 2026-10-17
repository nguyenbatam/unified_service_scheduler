package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// dayLockPaths are the two ways a booking's first pass takes the dealership-day lock before reading
// its candidates: tried (an instance that has not seen the day contended) or waited for (one whose
// gate saw others of the day, or whose bookings there collided lately — forced here).
var dayLockPaths = []struct {
	name     string
	instance func(st *store.Store) *booking.Service
}{
	{"day lock tried", atStartOfLondonDay},
	{"day lock waited for", func(st *store.Store) *booking.Service {
		return booking.New(st, nil, booking.WithClock(startOfLondonDay), booking.WithLockFirst())
	}},
}

// rowUnlocked reports whether no other TX holds a lock on appointment id that FOR UPDATE conflicts
// with (it tries one NOWAIT, in a TX of its own that it rolls back).
func rowUnlocked(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT 1 FROM appointments WHERE id = $1 FOR UPDATE NOWAIT`, id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// dayLockFree reports whether nobody holds dealer's day lock for the day containing at: it tries
// the lock the way store.LockArrival takes it, in a TX of its own that it rolls back.
func dayLockFree(t *testing.T, pool *pgxpool.Pool, dealer uuid.UUID, at time.Time) bool {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var free bool
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended('dealer-day|' || d.id::text || '|' || ($2::timestamptz AT TIME ZONE d.timezone)::date::text, 0))
		FROM dealerships d WHERE d.id = $1`, dealer, at).Scan(&free); err != nil {
		t.Fatal(err)
	}
	return free
}

// A booking whose first pass holds the dealership-day lock (tried and free, or waited for) and meets
// a technician a roster edit holds (FOR UPDATE) never waits for it under that lock: it gives the
// lock back and waits for the technician outside the day's queue. Other bookings of that day go
// through meanwhile, and once the edit ends the booking gets the technician it ranked first — the
// skip changes nothing about the outcome.
func TestRosterEditNeverHoldsTheDayLock(t *testing.T) {
	t.Parallel()
	for _, path := range dayLockPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			pool, st, _, ctx := openTestDB(t)
			instance := path.instance(st)
			roster, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = roster.Rollback(ctx) }()
			// Blair ranks first for a brake job (level 3, ties Casey on load, then by name).
			if _, err := roster.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`, seed.TechBlair); err != nil {
				t.Fatal(err)
			}
			at := londonAt(t, 14, 0)
			type result struct {
				appt domain.Appointment
				err  error
			}
			blocked := make(chan result, 1)
			go func() {
				waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				appt, _, err := instance.Book(waitCtx, staffAdmin, booking.BookRequest{
					DealershipID: seed.DealerNorth, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceBrake,
					StartAt: at, IdempotencyKey: uuid.NewString(),
				})
				blocked <- result{appt, err}
			}()
			awaitLockWaiter(t, pool, "FOR SHARE")
			if !dayLockFree(t, pool, seed.DealerNorth, at) {
				t.Fatal("the booking waiting for the technician holds the dealership-day lock")
			}

			// Bookings of the same day, through the same path on another instance (this one's gate
			// holds the blocked booking, so its followers would read under the lock whatever the path),
			// finish well before lock_timeout.
			follower := path.instance(st)
			quick, cancel := context.WithTimeout(ctx, store.DefaultPoolOptions().LockTimeout*2/3)
			defer cancel()
			for _, req := range []booking.BookRequest{
				oilAt(seed.DealerNorth, seed.VehicleA, at), // Alex: Blair cannot do it
				{DealershipID: seed.DealerNorth, VehicleID: newVehicle(t, instance), ServiceTypeID: seed.ServiceOil, StartAt: londonAt(t, 16, 0), IdempotencyKey: uuid.NewString()},
			} {
				if _, _, err := follower.Book(quick, staffAdmin, req); err != nil {
					t.Fatalf("booking of the same day stalled behind the roster edit: %v", err)
				}
			}
			select {
			case r := <-blocked:
				t.Fatalf("the brake booking should still be waiting for its technician, got %v", r.err)
			default:
			}
			if err := roster.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-blocked
			if r.err != nil {
				t.Fatalf("brake booking after the roster edit ended: %v", r.err)
			}
			if r.appt.TechnicianID != seed.TechBlair {
				t.Fatalf("brake booking got technician %s, want Blair (ranked first; a technician mid-edit is waited for, not passed over)", r.appt.TechnicianID)
			}
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
		})
	}
}

// Racers for a slot with room for one, each through an instance of its own (as across replicas),
// half trying the day lock and half waiting for it, while a roster edit holds the only technician
// who can do the job. Every racer gives the day lock back (or never took it) and waits for the
// technician — none waits on the day lock, so none holds it — and when the edit ends exactly one
// wins, the rest lose cleanly, and nothing overlaps.
func TestRosterEditDuringBurstOneWinner(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	start := londonOn(t, day, 10, 0)
	const n = 8
	vehicles := make([]uuid.UUID, n)
	for i := range vehicles {
		vehicles[i] = newVehicle(t, svc)
	}
	// Riverside's oil slot: one express bay, one lubrication technician (Eve).
	if got := slotCapacity(t, svc, seed.DealerRiver, vehicles[0], seed.ServiceOil, day, start); got != 1 {
		t.Fatalf("the slot holds %d bookings, want 1 (fixture changed?)", got)
	}
	roster, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = roster.Rollback(ctx) }()
	if _, err := roster.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`, seed.TechEve); err != nil {
		t.Fatal(err)
	}
	errs := make([]error, n)
	done := make(chan struct{})
	go func() {
		defer close(done)
		raceN(n, func(i int) {
			waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			_, _, errs[i] = dayLockPaths[i%len(dayLockPaths)].instance(st).Book(waitCtx, staffAdmin, booking.BookRequest{
				DealershipID: seed.DealerRiver, VehicleID: vehicles[i], ServiceTypeID: seed.ServiceOil, StartAt: start, IdempotencyKey: uuid.NewString(),
			})
		})
	}()
	awaitLockWaiters(t, pool, "FOR SHARE", n)
	if !dayLockFree(t, pool, seed.DealerRiver, start) {
		t.Fatal("a racer waiting for the technician holds the dealership-day lock")
	}
	if err := roster.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	expectWinners(t, "one place, technician mid-edit", 1, allLose(errs, refusals...)...)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// The one-move repair's target technician is checked like a candidate, before anything moves: a
// booking that needs to move a confirmed job onto a technician a roster edit holds gives its day
// lock back (and the moved row's lock with it) and waits for that technician outside the day's
// queue. Another booking of the day goes through meanwhile; once the edit ends the repair is made
// and the booking placed.
func TestRosterEditOnRepairTargetNeverHoldsTheDayLock(t *testing.T) {
	t.Parallel()
	for _, path := range dayLockPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			pool, st, svc, ctx := openTestDB(t)
			instance := path.instance(st)
			d := provisionShop(t, svc, "Repair target")
			for _, bay := range []domain.BayType{domain.BayExpress, domain.BayGeneral} {
				if _, err := svc.CreateBay(ctx, staffAdmin, d.ID, bay.Name(), bay); err != nil {
					t.Fatal(err)
				}
			}
			newTech := func(name string, level int16, skills ...domain.TechnicianSkill) uuid.UUID {
				tech, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{DealershipID: d.ID, Name: name, SkillLevel: domain.SkillLevel(level), Skills: skills})
				if err != nil {
					t.Fatal(err)
				}
				return tech.ID
			}
			day := londonDay(t, 1)
			at := londonOn(t, day, 10, 0)
			// Morgan (brakes too) takes the 10:00 oil change while alone; Lee (oil only) joins after.
			morgan := newTech("Morgan", 2, domain.SkillLubrication, domain.SkillBrakes)
			oil, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceOil, at)
			if err != nil {
				t.Fatal(err)
			}
			lee := newTech("Lee", 1, domain.SkillLubrication)

			roster, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = roster.Rollback(ctx) }()
			if _, err := roster.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`, lee); err != nil {
				t.Fatal(err)
			}
			type result struct {
				appt domain.Appointment
				err  error
			}
			blocked := make(chan result, 1)
			brakeCar := newVehicle(t, svc)
			go func() {
				waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				// Only Morgan does brakes: the oil change must move to Lee, who is mid-edit.
				appt, _, err := instance.Book(waitCtx, staffAdmin, booking.BookRequest{
					DealershipID: d.ID, VehicleID: brakeCar, ServiceTypeID: seed.ServiceBrake, StartAt: at, IdempotencyKey: uuid.NewString(),
				})
				blocked <- result{appt, err}
			}()
			awaitLockWaiter(t, pool, "FOR SHARE")
			if !dayLockFree(t, pool, d.ID, at) {
				t.Fatal("the booking waiting for the repair's target technician holds the dealership-day lock")
			}
			quick, cancel := context.WithTimeout(ctx, store.DefaultPoolOptions().LockTimeout*2/3)
			defer cancel()
			if !rowUnlocked(t, pool, oil.ID) {
				t.Fatal("the booking waiting for the repair's target technician still locks the job it would move")
			}
			if _, _, err := path.instance(st).Book(quick, staffAdmin, booking.BookRequest{
				DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake, StartAt: londonOn(t, day, 14, 0), IdempotencyKey: uuid.NewString(),
			}); err != nil {
				t.Fatalf("booking of the same day stalled behind the roster edit: %v", err)
			}
			if err := roster.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-blocked
			if r.err != nil {
				t.Fatalf("brake booking after the roster edit ended: %v", r.err)
			}
			if r.appt.TechnicianID != morgan {
				t.Fatalf("brake booking got technician %s, want Morgan", r.appt.TechnicianID)
			}
			if got := scalar[uuid.UUID](t, pool, `SELECT technician_id FROM appointments WHERE id = $1`, oil.ID); got != lee {
				t.Fatalf("the oil change is on %s, want moved to Lee", got)
			}
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
		})
	}
}
