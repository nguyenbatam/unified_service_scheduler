package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func oilAt(dealer, vehicle uuid.UUID, start time.Time) booking.BookRequest {
	return booking.BookRequest{
		DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: seed.ServiceOil,
		StartAt: start, IdempotencyKey: "test:" + uuid.NewString(),
	}
}

func TestBookRejectsPastAndCustomerLeadTime(t *testing.T) {
	t.Parallel()
	st, _, ctx := openTestStore(t)
	svc := serviceOn(t, st, londonDay(t, 0), 12, 0)

	_, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 11, 0)))
	expectCode(t, err, domain.CodeUnavailable)

	_, _, err = svc.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 12, 30)))
	expectCode(t, err, domain.CodeUnavailable)

	if _, created, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 12, 30))); err != nil || !created {
		t.Fatalf("staff may book within the lead time: %v", err)
	}
	if _, created, err := svc.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 13, 0))); err != nil || !created {
		t.Fatalf("customer outside lead time: %v", err)
	}

	day, _ := suiteToday()
	avail, err := svc.Availability(ctx, customerAmelia, booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: day, IsWaiter: false})
	if err != nil {
		t.Fatal(err)
	}
	// Keyed by Unix: time.Time map keys also compare the *Location pointer.
	want := map[int64]string{londonAt(t, 11, 30).Unix(): booking.ReasonPast, londonAt(t, 12, 30).Unix(): booking.ReasonLeadTime}
	for _, slot := range avail.Slots {
		if reason, ok := want[slot.Start.Unix()]; ok {
			if slot.Available || slot.Reason != reason {
				t.Fatalf("%s: available=%v reason=%q want %q", slot.Start, slot.Available, slot.Reason, reason)
			}
			delete(want, slot.Start.Unix())
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing slots %v", want)
	}
}

// Northside allows 2 arrivals per hour. Six bookings race into the 14:00 hour (two half-hour
// slots) with bays and technicians for more, so only the staging cap decides: exactly the
// capacity wins, now that the staging advisory lock is taken after the candidates. Without that
// lock concurrent requests would all pass the count check.
func TestStagingCapacityHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	// Precondition: the cap, not the resources, decides. 2 arrivals per 60-minute window, and at
	// 14:00 three fitting bays and two lubrication technicians for an oil change, two general bays
	// and two brake technicians for a brake job (Alex and Bay 1 take an oil change while Blair and
	// Casey take the two brake jobs in Bays 2 and 3): without the cap at least three of the 14:00
	// racers alone would book. A seed change fails here instead of testing the bays.
	if capacity, window := scalar[int](t, pool, `SELECT staging_capacity::int FROM dealerships WHERE id = $1`, seed.DealerNorth),
		scalar[int](t, pool, `SELECT staging_window_minutes::int FROM dealerships WHERE id = $1`, seed.DealerNorth); capacity != 2 || window != 60 {
		t.Fatalf("precondition: Northside staging %d per %d min, want 2 per 60", capacity, window)
	}
	today := londonDay(t, 0)
	for _, start := range []time.Time{londonAt(t, 14, 0), londonAt(t, 14, 30)} {
		requireCapacity(t, svc, booking.AvailabilityQuery{DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: today}, start, 3, 2)
		requireCapacity(t, svc, booking.AvailabilityQuery{DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake, Day: today}, start, 2, 2)
	}
	vehicles := []uuid.UUID{seed.VehicleA, seed.VehicleB, seed.VehicleC, newVehicle(t, svc), newVehicle(t, svc), newVehicle(t, svc)}
	reqs := []booking.BookRequest{
		oilAt(seed.DealerNorth, vehicles[0], londonAt(t, 14, 0)),
		brakeAt(seed.DealerNorth, vehicles[1], londonAt(t, 14, 0)),
		brakeAt(seed.DealerNorth, vehicles[2], londonAt(t, 14, 0)),
		oilAt(seed.DealerNorth, vehicles[3], londonAt(t, 14, 30)),
		oilAt(seed.DealerNorth, vehicles[4], londonAt(t, 14, 30)),
		brakeAt(seed.DealerNorth, vehicles[5], londonAt(t, 14, 30)),
	}
	items := make([]domain.Appointment, len(reqs))
	errs := make([]error, len(reqs))
	raceN(len(reqs), func(i int) {
		items[i], _, errs[i] = svc.Book(ctx, staffAdmin, reqs[i])
	})
	for _, i := range expectWinners(t, "14:00 window", 2, allLose(errs, refusals...)...) {
		assertEventsFor(t, pool, items[i].ID)
	}
	assertStagingWithinCapacity(t, pool)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	arrivals := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments
		WHERE dealership_id = $1 AND status IN (`+holdingCodes+`) AND start_at >= $2 AND start_at < $3`,
		seed.DealerNorth, londonAt(t, 14, 0), londonAt(t, 15, 0))
	if arrivals != 2 {
		t.Fatalf("arrivals=%d, want exactly the staging capacity (2)", arrivals)
	}
}

func TestRescheduleIsAtomicAndIdempotent(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	orig, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Reschedule(ctx, staffAdmin, orig.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0), IfMatch: orig.Version + 5})
	expectCode(t, err, domain.CodePrecondition)

	moved, err := svc.Reschedule(ctx, staffAdmin, orig.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0), IfMatch: orig.Version})
	if err != nil {
		t.Fatal(err)
	}
	// In place: same appointment (number), new window, version bumped.
	if moved.ID != orig.ID || moved.Version != orig.Version+1 || moved.Status != domain.StatusConfirmed ||
		!moved.StartAt.Equal(londonAt(t, 15, 0)) {
		t.Fatalf("moved = %+v", moved)
	}
	// A client retry carrying the old If-Match replays the result instead of failing with 412.
	again, err := svc.Reschedule(ctx, staffAdmin, orig.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0), IfMatch: orig.Version})
	if err != nil || again.ID != moved.ID || again.Version != moved.Version {
		t.Fatalf("retry must replay the move: %+v %v", again, err)
	}
	history, _, err := svc.History(ctx, staffAdmin, orig.ID, 0, nil)
	if err != nil || len(history) != 2 || history[0].EventType != domain.EventAppointmentCreated ||
		history[1].EventType != domain.EventAppointmentRescheduled || history[1].ActorID == nil || *history[1].ActorID != staffAdmin.UserID {
		t.Fatalf("history = %+v err=%v", history, err)
	}

	// Target slot taken: nothing changes, the original booking survives.
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 16, 0))); err != nil {
		t.Fatal(err)
	}
	keep, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, londonAt(t, 16, 30)))
	if err != nil {
		t.Fatal(err)
	}
	// Riverside oil holds one car: no bay and technician are free at 16:00 (422, not a lost race).
	_, err = svc.Reschedule(ctx, staffAdmin, keep.ID, booking.RescheduleRequest{StartAt: londonAt(t, 16, 0)})
	expectCode(t, err, domain.CodeUnavailable)
	still := mustGet(t, svc, keep.ID)
	if still.Status != domain.StatusConfirmed || !still.StartAt.Equal(londonAt(t, 16, 30)) {
		t.Fatalf("original must be untouched, got %s at %s", still.Status.Name(), still.StartAt)
	}
}

func TestExtendOverrunRespectsNextBooking(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	first, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 8, 0)))
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 8, 30)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckIn(ctx, staffDispatcher, first.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceOn(t, st, londonDay(t, 0), 8, 0).StartWork(ctx, staffDispatcher, first.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Extend(ctx, staffDispatcher, first.ID, 15, 0)
	expectCode(t, err, domain.CodeConflict)
	_, err = svc.Extend(ctx, staffDispatcher, first.ID, 10, 0)
	expectCode(t, err, domain.CodeInvalid)
	_, err = svc.Extend(ctx, customerAmelia, first.ID, 15, 0)
	expectCode(t, err, domain.CodeForbidden)

	if _, err := svc.Cancel(ctx, staffDispatcher, next.ID, 0); err != nil {
		t.Fatal(err)
	}
	extended, err := svc.Extend(ctx, staffDispatcher, first.ID, 15, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !extended.EndAt.Equal(londonAt(t, 8, 45)) || extended.StartedAt == nil {
		t.Fatalf("end=%s startedAt=%v", extended.EndAt, extended.StartedAt)
	}
	// The overrun now blocks a new 08:30 booking on the same bay/tech.
	_, _, err = svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, londonAt(t, 8, 30)))
	expectCode(t, err, domain.CodeUnavailable)
}

func TestNoShowReleasesSlot(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	// Seed holds VehicleA at 09:00 and VehicleB/C 10:00–11:30 (Northside); pick non-overlapping cars.
	item, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.NoShow(ctx, staffDispatcher, item.ID, 0)
	expectCode(t, err, domain.CodeUnavailable) // clock is 00:00, before start

	late := serviceOn(t, st, londonDay(t, 0), 9, 20)
	marked, err := late.NoShow(ctx, staffDispatcher, item.ID, 0)
	if err != nil || marked.Status != domain.StatusNoShow {
		t.Fatalf("no-show: %v %v", marked.Status, err)
	}
	if _, created, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, londonAt(t, 9, 0))); err != nil || !created {
		t.Fatalf("slot must be free after no-show: %v", err)
	}

	sweep, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	n, err := serviceOn(t, st, londonDay(t, 0), 11, 0).MarkNoShows(ctx)
	if err != nil || n == 0 {
		t.Fatalf("sweeper marked %d: %v", n, err)
	}
	swept := mustGet(t, svc, sweep.ID)
	if swept.Status != domain.StatusNoShow {
		t.Fatalf("sweeper status = %s", swept.Status.Name())
	}
	events := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE event_type = `+code(domain.EventAppointmentNoShow)+` AND aggregate_id = $1`, sweep.ID)
	if events != 1 {
		t.Fatalf("no-show events = %d", events)
	}
}

func TestCancelRules(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	done, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 8, 0)))
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []func() (domain.Appointment, error){
		func() (domain.Appointment, error) { return svc.CheckIn(ctx, staffDispatcher, done.ID, 0) },
		func() (domain.Appointment, error) { return svc.StartWork(ctx, staffDispatcher, done.ID, 0) },
	} {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	_, err = svc.Cancel(ctx, staffDispatcher, done.ID, 0)
	expectCode(t, err, domain.CodeConflict) // work started
	if _, err := svc.Complete(ctx, staffDispatcher, done.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Cancel(ctx, staffDispatcher, done.ID, 0)
	expectCode(t, err, domain.CodeConflict) // completed is final

	soon, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 10, 30)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = serviceOn(t, st, londonDay(t, 0), 10, 0).Cancel(ctx, customerAmelia, soon.ID, 0)
	expectCode(t, err, domain.CodeUnavailable) // inside the online cutoff

	_, err = svc.Cancel(ctx, staffAdmin, soon.ID, soon.Version+1)
	expectCode(t, err, domain.CodePrecondition)

	first, err := svc.Cancel(ctx, customerAmelia, soon.ID, soon.Version)
	if err != nil || first.Status != domain.StatusCancelled || first.Version != soon.Version+1 {
		t.Fatalf("customer cancel: %+v %v", first, err)
	}
	second, err := svc.Cancel(ctx, customerAmelia, soon.ID, soon.Version)
	if err != nil || second.Version != first.Version {
		t.Fatalf("cancel retry must be a no-op: %v", err)
	}
}

func TestIdempotencyKeysAreScopedPerUser(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	key := "shared-key"
	a, createdA, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil,
		StartAt: londonAt(t, 14, 0), IdempotencyKey: key,
	})
	if err != nil || !createdA {
		t.Fatal(err)
	}
	b, createdB, err := svc.Book(ctx, staffDispatcher, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceOil,
		StartAt: londonAt(t, 15, 0), IdempotencyKey: key,
	})
	if err != nil || !createdB || a.ID == b.ID {
		t.Fatalf("another user's identical key must not replay: created=%v err=%v", createdB, err)
	}
	_, _, err = svc.Book(ctx, staffDispatcher, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleC, ServiceTypeID: seed.ServiceOil,
		StartAt: londonAt(t, 16, 0), IdempotencyKey: key,
	})
	expectCode(t, err, domain.CodeConflict)
}

// Availability must not offer a slot the selected car cannot attend (booked elsewhere).
func TestAvailabilityFlagsVehicleBusy(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, _ := suiteToday()
	// Seed: VehicleA has an oil change at Northside 09:00–09:30.
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: day, IsWaiter: false})
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range avail.Slots {
		if slot.Start.Unix() == londonAt(t, 9, 0).Unix() {
			if slot.Available || slot.Reason != booking.ReasonVehicleBusy {
				t.Fatalf("09:00 available=%v reason=%q, want vehicle_busy", slot.Available, slot.Reason)
			}
			return
		}
	}
	t.Fatal("missing 09:00 slot")
}

// A late retry of an old reschedule returns the appointment that reschedule created, in its
// current state, and never creates another one.
func TestRescheduleStaleRetryNeverMovesBack(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	a, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Reschedule(ctx, staffAdmin, a.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0), IfMatch: a.Version})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Reschedule(ctx, staffAdmin, a.ID, booking.RescheduleRequest{StartAt: londonAt(t, 16, 0), IfMatch: b.Version}); err != nil {
		t.Fatal(err)
	}
	// The first request's late retry (stale If-Match) must not drag it back to 15:00.
	_, err = svc.Reschedule(ctx, staffAdmin, a.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0), IfMatch: a.Version})
	expectCode(t, err, domain.CodePrecondition)
	now := mustGet(t, svc, a.ID)
	if !now.StartAt.Equal(londonAt(t, 16, 0)) {
		t.Fatalf("start = %s, want 16:00", now.StartAt)
	}
}

func TestRescheduleMayOverlapItsOwnWindow(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	// Riverside has one general bay: a 90 min brake job can only slide if its own hold is ignored.
	brake, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake, StartAt: londonAt(t, 10, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := svc.Reschedule(ctx, staffAdmin, brake.ID, booking.RescheduleRequest{StartAt: londonAt(t, 10, 30)})
	if err != nil {
		t.Fatal(err)
	}
	if moved.ID != brake.ID || !moved.StartAt.Equal(londonAt(t, 10, 30)) || moved.ServiceBayID != brake.ServiceBayID {
		t.Fatalf("moved = %+v", moved)
	}
}
