package integration_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Concurrent bookings whose windows overlap but arrive in different staging windows (a 3-hour
// service at 09:00, a brake job at 10:30, oil changes in between) all compete for Riverside's one
// all-round technician and its general bay. Each GiST insert checks its entries against the
// others' uncommitted ones, so two such inserts can wait on each other: a deadlock Postgres only
// breaks after deadlock_timeout, stalling every writer behind them into lock_timeout (503). The
// dealership-day lock queues them instead: no deadlock, no busy answer, every refusal a clean 409/422,
// and the day ends with the holds the bookings were told about and nothing overlapping.
func TestOverlappingBookingsOfOneDayNeverDeadlock(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	services := []uuid.UUID{seed.ServiceMajor, seed.ServiceBrake, seed.ServiceOil}
	const n = 24
	// One service per racer, like as many backend instances: each has its own admission gate, so
	// all n reach the database at once and only the database lock can order them.
	racers := make([]*booking.Service, n)
	for i := range racers {
		racers[i] = atStartOfLondonDay(st)
	}
	vehicles := make([]uuid.UUID, n)
	for i := range vehicles {
		vehicles[i] = newVehicle(t, svc)
	}
	deadlocksBefore := scalar[int64](t, pool, `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`)
	errs := make([]error, n)
	booked := make([]bool, n)
	start := time.Now()
	raceN(n, func(i int) {
		at := londonOn(t, day, 8, 0).Add(time.Duration(i%12) * 30 * time.Minute)
		_, _, errs[i] = racers[i].Book(ctx, staffAdmin, booking.BookRequest{
			DealershipID: seed.DealerRiver, VehicleID: vehicles[i], ServiceTypeID: services[i%len(services)],
			StartAt: at, IdempotencyKey: uuid.NewString(),
		})
		booked[i] = errs[i] == nil
	})
	elapsed := time.Since(start)
	wins := 0
	for i, err := range errs {
		if err != nil && !isCleanLoss(err) {
			t.Fatalf("booking %d: %v (want a booking or a clean 409/422, never busy or a deadlock)", i, err)
		}
		if booked[i] {
			wins++
		}
	}
	if wins == 0 {
		t.Fatal("no booking won: the race proved nothing")
	}
	// Book retries a deadlock and reports a lost race (409), so only Postgres's own counter can tell.
	// A backend reports its deadlocks to pg_stat_database lazily (at most once a second, or when it
	// exits, before it leaves pg_stat_activity): close the racers' connections and wait until every
	// one of them is gone, then the count is final.
	pool.Reset()
	for gone := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		lingering := scalar[int](t, pool, `
			SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid() AND backend_type = 'client backend'`)
		if lingering == 0 {
			break
		}
		if time.Now().After(gone) {
			t.Fatalf("%d racer connection(s) still open 10s after the pool reset", lingering)
		}
	}
	deadlocks := scalar[int64](t, pool, `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()`) - deadlocksBefore
	if deadlocks != 0 {
		t.Fatalf("%d deadlock(s) among bookings of one dealership-day (the day lock must queue them)", deadlocks)
	}
	// Queued writers take milliseconds each; a deadlock cascade takes a deadlock_timeout (1 s) per
	// cycle. The deadlock count above is the precise check and a lock timeout already failed the
	// clean-loss check: this only catches a stall, with the burst tests' budget (headroom for -race
	// on a loaded runner with parallel test databases).
	if elapsed > burstBudget {
		t.Fatalf("%d bookings took %s: writers stalled", n, elapsed)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// dayLockSQL takes Riverside's dealership-day lock for the day containing $1 the way
// store.LockArrival does, so a test can hold it from outside a booking.
const dayLockSQL = `SELECT pg_advisory_xact_lock(hashtextextended('dealer-day|' || d.id::text || '|' || ($2::timestamptz AT TIME ZONE d.timezone)::date::text, 0))
	FROM dealerships d WHERE d.id = $1`

// The dealership-day lock queues only the writers of that day: a booking of another day (or another
// dealership) goes straight through while it is held, and one of the same day waits for it.
func TestDayLockQueuesOnlyThatDay(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, dayLockSQL, seed.DealerRiver, londonOn(t, day, 9, 0)); err != nil {
		t.Fatal(err)
	}

	// Bookings of another day and of another dealership the same day finish while the lock is held,
	// and never queue on a lock on their way: checked in pg_stat_activity, not by the clock (a slow
	// -race run must not fail, and a wait shorter than any threshold must not pass).
	otherDayCar, otherShopCar := newVehicle(t, svc), newVehicle(t, svc) // t.Fatal only from the test goroutine
	others := make(chan error, 1)
	go func() {
		if _, err := bookAt(t, svc, seed.DealerRiver, otherDayCar, seed.ServiceOil, londonOn(t, londonDay(t, 2), 9, 0)); err != nil {
			others <- fmt.Errorf("booking of the next day while this day's lock is held: %w", err)
			return
		}
		if _, err := bookAt(t, svc, seed.DealerNorth, otherShopCar, seed.ServiceOil, londonOn(t, day, 9, 0)); err != nil {
			others <- fmt.Errorf("booking at another dealership the same day: %w", err)
			return
		}
		others <- nil
	}()
	// A booking stuck behind the held lock would wait there until lock_timeout and then fail (503):
	// it cannot finish while the lock is held, so the poll cannot miss it.
	var queued bool
	var othersErr error
	for done := false; !done; {
		select {
		case othersErr = <-others:
			done = true
		case <-time.After(2 * time.Millisecond):
			queued = queued || lockWaiters(t, pool, "pg_advisory_xact_lock") > 0
		}
	}
	if queued {
		t.Fatalf("a booking of another day or dealership queued on an advisory lock while this day's was held (%v)", othersErr)
	}
	if othersErr != nil {
		t.Fatal(othersErr)
	}

	var sameDayDone atomic.Bool
	done := make(chan error, 1)
	sameDayCar := newVehicle(t, svc) // t.Fatal only from the test goroutine
	go func() {
		_, err := bookAt(t, svc, seed.DealerRiver, sameDayCar, seed.ServiceOil, londonOn(t, day, 15, 0))
		sameDayDone.Store(true)
		done <- err
	}()
	awaitLockWaiter(t, pool, "pg_advisory_xact_lock")
	if sameDayDone.Load() {
		t.Fatal("a booking of the held day finished without waiting for its lock")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("same-day booking after the lock was released: %v", err)
	}
}

// Racers for one slot with room for three: exactly three win, nobody fails busy, and a booking
// afterwards is refused — on both allocation paths. Through one instance the racers meet in its
// gate, so all but the first read their candidates under the day lock (allocation.lockFirst): the
// read is exact. Through an instance each (no shared gate, as across replicas) the racers only try
// the lock before reading, and all but the one that finds it free read before it, so the losers
// collide on stale candidates and re-read under the lock instead of walking the stale list. Either way no place is left unused and nothing is over-booked. (How much
// lock time a loser saves is measured by the bench's hot workload, not here.)
func TestCandidateRereadFillsEveryPlace(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for _, mode := range []struct {
		name     string
		instance func(i int) *booking.Service
	}{
		{"one instance: read under the lock", func(int) *booking.Service { return svc }},
		{"instance per racer: stale read, re-read", func(int) *booking.Service {
			return booking.New(st, nil, booking.WithClock(startOfLondonDay))
		}},
	} {
		d, err := svc.CreateDealership(ctx, staffAdmin, store.NewDealership{
			Name: "Reread " + mode.name, Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "18:00", StagingWindowMinutes: 60,
			Policy: domain.Policy{CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 90, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60},
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 {
			if _, err := svc.CreateBay(ctx, staffAdmin, d.ID, fmt.Sprintf("Lane %d", i), domain.BayExpress); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{
				DealershipID: d.ID, Name: fmt.Sprintf("Lube %d", i), SkillLevel: 1, Skills: []domain.TechnicianSkill{domain.SkillLubrication},
			}); err != nil {
				t.Fatal(err)
			}
		}
		start := londonOn(t, londonDay(t, 1), 10, 0)
		const n = 16
		vehicles := make([]uuid.UUID, n+1)
		instances := make([]*booking.Service, n)
		for i := range vehicles {
			vehicles[i] = newVehicle(t, svc)
		}
		for i := range instances {
			instances[i] = mode.instance(i)
		}
		errs := make([]error, n)
		raceN(n, func(i int) {
			_, _, errs[i] = instances[i].Book(ctx, staffAdmin, booking.BookRequest{
				DealershipID: d.ID, VehicleID: vehicles[i], ServiceTypeID: seed.ServiceOil, StartAt: start, IdempotencyKey: uuid.NewString(),
			})
		})
		expectWinners(t, mode.name+": three places", 3, allLose(errs, refusals...)...)
		_, _, err = svc.Book(ctx, staffAdmin, booking.BookRequest{
			DealershipID: d.ID, VehicleID: vehicles[n], ServiceTypeID: seed.ServiceOil, StartAt: start, IdempotencyKey: uuid.NewString(),
		})
		expectCode(t, err, domain.CodeUnavailable)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Extending a checked-in job into the next slot races a booking of that slot needing the same
// (only) technician: both queue on the dealership-day lock, so exactly one wins, cleanly — no
// deadlock, no busy answer — and the holds never overlap.
func TestExtendRacingBookingOneWins(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	day := londonDay(t, 0)
	clock := func(hour int) *booking.Service { return serviceOn(t, st, day, hour, 5) } // five minutes into the hour
	for k, hour := range []int{9, 11, 13, 15} {
		job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, day, hour, 0)) // Eve, express bay
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusCheckedIn)+` WHERE id = $1`, job.ID); err != nil {
			t.Fatal(err)
		}
		var extendErr, bookErr error
		race(func() {
			_, extendErr = clock(hour).Extend(ctx, staffAdmin, job.ID, 30, 0)
		}, func() {
			_, bookErr = bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, day, hour, 30))
		})
		oneWinner(t, fmt.Sprintf("round %d", k), loses(extendErr, refusals...), loses(bookErr, refusals...))
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
