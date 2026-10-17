package integration_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// The dealership-day lock is keyed on the dealership's own local day, never the UTC date. In
// Australia/Sydney (UTC+10/+11) a morning start is still the previous UTC day, so the two halves
// of one local day have different UTC dates, and one local day's afternoon shares its UTC date
// with the next local day's morning. A UTC key would let the morning booking of the held day
// straight through (two writers of one day uncontended) and queue the next day's morning behind
// it. TestDayLockQueuesOnlyThatDay pins the same rule in London, where the two never differ in
// opening hours.
func TestDayLockFollowsTheDealershipsLocalDay(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	dealer, bay, tech := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, pool, `INSERT INTO dealerships (id, name, timezone, open_time, close_time, staging_capacity)
		VALUES ($1, 'Harbour Motors', 'Australia/Sydney', TIME '06:00', TIME '20:00', 0)`, dealer)
	mustExec(t, pool, `INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES ($1, $2, 'Lane 1', 1)`, bay, dealer)
	mustExec(t, pool, `INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES ($1, $2, 'Shane Harbour', 1)`, tech, dealer)
	mustExec(t, pool, `INSERT INTO technician_skills (technician_id, skill) VALUES ($1, 1)`, tech)

	today, _ := suiteToday()
	day := today.AddDays(3)
	svc := booking.New(st, nil, booking.WithClock(func() time.Time { return today.Start(loc) }))
	at := func(d booking.Date, hour int) time.Time { return booking.Clock{Hour: hour}.On(d, loc) }
	held := at(day, 15)              // the held lock's instant: on UTC date `day`
	sameDay := at(day, 7)            // same local day, previous UTC date
	nextDay := at(day.AddDays(1), 7) // next local day, the held instant's UTC date
	utcDate := func(t time.Time) booking.Date { return booking.DateFrom(t, time.UTC) }
	if utcDate(sameDay) == utcDate(held) || utcDate(nextDay) != utcDate(held) {
		t.Fatalf("fixture: UTC dates %s / %s / %s do not split the local days as the test needs",
			utcDate(sameDay), utcDate(held), utcDate(nextDay))
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, dayLockSQL, dealer, held); err != nil {
		t.Fatal(err)
	}

	// The next local day goes straight through although it shares the held instant's UTC date: it
	// never waits on an advisory lock (pg_stat_activity, not the clock).
	nextCar, sameCar := newVehicle(t, svc), newVehicle(t, svc) // t.Fatal only from the test goroutine
	other := make(chan error, 1)
	go func() {
		if _, err := bookAt(t, svc, dealer, nextCar, seed.ServiceOil, nextDay); err != nil {
			other <- fmt.Errorf("booking of the next local day (same UTC date) while the day's lock is held: %w", err)
			return
		}
		other <- nil
	}()
	var queued bool
	var otherErr error
	for done := false; !done; {
		select {
		case otherErr = <-other:
			done = true
		case <-time.After(2 * time.Millisecond):
			queued = queued || lockWaiters(t, pool, "pg_advisory_xact_lock") > 0
		}
	}
	if queued {
		t.Fatalf("a booking of the next local day queued on the held day's lock: the key follows the UTC date (%v)", otherErr)
	}
	if otherErr != nil {
		t.Fatal(otherErr)
	}

	// The same local day's morning, on the previous UTC date, waits for the lock.
	var sameDone atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := bookAt(t, svc, dealer, sameCar, seed.ServiceOil, sameDay)
		sameDone.Store(true)
		done <- err
	}()
	awaitLockWaiter(t, pool, "pg_advisory_xact_lock")
	if sameDone.Load() {
		t.Fatal("a booking of the held local day (previous UTC date) finished without waiting for its lock")
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("same-local-day booking after the lock was released: %v", err)
	}
}

// Racers for the one place of a Sydney dealership's early-morning slot (still the previous UTC
// day) and its afternoon slot of the same local day, through an instance each (no shared gate):
// exactly one wins each slot, nobody fails busy, and nothing overlaps — the day lock orders writers
// across the UTC midnight that falls inside the opening hours.
func TestSydneyDayRacesOneWinnerPerSlot(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	dealer, bay, tech := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, pool, `INSERT INTO dealerships (id, name, timezone, open_time, close_time, staging_capacity)
		VALUES ($1, 'Harbour Motors', 'Australia/Sydney', TIME '06:00', TIME '20:00', 0)`, dealer)
	mustExec(t, pool, `INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES ($1, $2, 'Lane 1', 1)`, bay, dealer)
	mustExec(t, pool, `INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES ($1, $2, 'Shane Harbour', 1)`, tech, dealer)
	mustExec(t, pool, `INSERT INTO technician_skills (technician_id, skill) VALUES ($1, 1)`, tech)
	today, _ := suiteToday()
	day := today.AddDays(3)
	clock := func() time.Time { return today.Start(loc) }
	slots := []time.Time{booking.Clock{Hour: 7}.On(day, loc), booking.Clock{Hour: 15}.On(day, loc)}
	if booking.DateFrom(slots[0], time.UTC) == booking.DateFrom(slots[1], time.UTC) {
		t.Fatal("fixture: both slots on one UTC date")
	}
	const perSlot = 6
	n := perSlot * len(slots)
	racers := make([]*booking.Service, n)
	vehicles := make([]uuid.UUID, n)
	for i := range racers {
		racers[i] = booking.New(st, nil, booking.WithClock(clock))
		vehicles[i] = newVehicle(t, racers[i])
	}
	errs := make([]error, n)
	raceN(n, func(i int) {
		_, _, errs[i] = racers[i].Book(ctx, staffAdmin, booking.BookRequest{
			DealershipID: dealer, VehicleID: vehicles[i], ServiceTypeID: seed.ServiceOil,
			StartAt: slots[i%len(slots)], IdempotencyKey: uuid.NewString(),
		})
	})
	wins := make([]int, len(slots))
	for i, err := range errs {
		switch {
		case err == nil:
			wins[i%len(slots)]++
		case !isCleanLoss(err):
			t.Fatalf("racer %d: %v (want a booking or a clean 409/422, never busy)", i, err)
		}
	}
	for k, w := range wins {
		if w != 1 {
			t.Fatalf("slot %s: %d winners, want exactly 1", slots[k].In(loc).Format("15:04 MST"), w)
		}
	}
	if overlaps := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a JOIN appointments b ON a.id < b.id
		  AND (a.service_bay_id = b.service_bay_id OR a.technician_id = b.technician_id)
		  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange(b.start_at, b.end_at, '[)')
		WHERE a.dealership_id = $1 AND a.status IN (`+holdingCodes+`) AND b.status IN (`+holdingCodes+`)`, dealer); overlaps != 0 {
		t.Fatalf("%d overlapping holds", overlaps)
	}
}
