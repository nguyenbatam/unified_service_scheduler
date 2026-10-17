package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

var (
	northDispatcher = domain.Actor{UserID: seed.DemoDispatcherID, Role: domain.RoleDispatcher, DealershipID: seed.DealerNorth}
	riverAdmin      = domain.Actor{UserID: seed.DemoAdminID, Role: domain.RoleAdmin, DealershipID: seed.DealerRiver}
)

func brakeAt(dealer, vehicle uuid.UUID, start time.Time) booking.BookRequest {
	return booking.BookRequest{DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: seed.ServiceBrake, StartAt: start}
}

// Starting work before the booked slot moves the hold to when the work really happens, so the
// original slot is sold again and the earlier one is protected.
func TestStartWorkEarlyPullsWindowForward(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	job, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	at10 := serviceOn(t, st, londonDay(t, 0), 10, 0)
	if _, err := at10.CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	started, err := at10.StartWork(ctx, staffDispatcher, job.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !started.StartAt.Equal(londonAt(t, 10, 0)) || !started.EndAt.Equal(londonAt(t, 10, 30)) {
		t.Fatalf("window = %s–%s, want 10:00–10:30", started.StartAt, started.EndAt)
	}
	// Riverside has one express bay and one lubrication technician: 14:00 is free again.
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 14, 0))); err != nil {
		t.Fatalf("the vacated 14:00 slot must be bookable: %v", err)
	}
	if _, err := at10.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}

	// Early start into a window someone else holds is refused and changes nothing.
	later, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleC, londonAt(t, 15, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 11, 0))); err != nil {
		t.Fatal(err)
	}
	at11 := serviceOn(t, st, londonDay(t, 0), 11, 0)
	if _, err := at11.CheckIn(ctx, staffDispatcher, later.ID, 0); err != nil {
		t.Fatal(err)
	}
	_, err = at11.StartWork(ctx, staffDispatcher, later.ID, 0)
	expectCode(t, err, domain.CodeConflict)
	still := mustGet(t, svc, later.ID)
	if still.Status != domain.StatusCheckedIn || !still.StartAt.Equal(londonAt(t, 15, 0)) {
		t.Fatalf("refused early start must not move the job: %s at %s", still.Status.Name(), still.StartAt)
	}
}

func TestReassignMovesWorkBetweenTechniciansAndBays(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	start := londonOn(t, londonDay(t, 1), 10, 0)
	job, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleB, start))
	if err != nil {
		t.Fatal(err)
	}
	// Northside brake technicians: Blair and Casey (both level 3).
	other := seed.TechCasey
	if job.TechnicianID == seed.TechCasey {
		other = seed.TechBlair
	}
	moved, err := svc.Reassign(ctx, northDispatcher, job.ID, booking.ReassignRequest{TechnicianID: other, IfMatch: job.Version})
	if err != nil {
		t.Fatal(err)
	}
	if moved.TechnicianID != other || moved.Version != job.Version+1 || !moved.StartAt.Equal(start) {
		t.Fatalf("moved = %+v", moved)
	}
	again, err := svc.Reassign(ctx, northDispatcher, job.ID, booking.ReassignRequest{TechnicianID: other, IfMatch: job.Version})
	if err != nil || again.Version != moved.Version {
		t.Fatalf("retry must be a no-op: %v %v", again.Version, err)
	}

	cases := []struct {
		name string
		req  booking.ReassignRequest
		code string
	}{
		{"technician without brakes skill", booking.ReassignRequest{TechnicianID: seed.TechAlex}, domain.CodeUnavailable},
		{"technician of another dealership", booking.ReassignRequest{TechnicianID: seed.TechEve}, domain.CodeUnavailable},
		{"express bay for a non-waiter brake job", booking.ReassignRequest{ServiceBayID: seed.BayN1}, domain.CodeUnavailable},
		{"diagnostic bay", booking.ReassignRequest{ServiceBayID: seed.BayN4}, domain.CodeUnavailable},
		{"bay of another dealership", booking.ReassignRequest{ServiceBayID: seed.BayR2}, domain.CodeUnavailable},
		{"nothing to change", booking.ReassignRequest{}, domain.CodeInvalid},
		{"stale If-Match", booking.ReassignRequest{TechnicianID: job.TechnicianID, IfMatch: job.Version}, domain.CodePrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Reassign(ctx, northDispatcher, job.ID, tc.req)
			expectCode(t, err, tc.code)
		})
	}
	otherBay := seed.BayN3
	if moved.ServiceBayID == seed.BayN3 {
		otherBay = seed.BayN2
	}
	if moved, err = svc.Reassign(ctx, northDispatcher, job.ID, booking.ReassignRequest{ServiceBayID: otherBay}); err != nil || moved.ServiceBayID != otherBay {
		t.Fatalf("bay move: %v %v", moved.ServiceBayID, err)
	}

	// The second brake job takes the remaining technician and bay; moving onto them is a 409.
	second, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleC, start))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.Reassign(ctx, northDispatcher, job.ID, booking.ReassignRequest{TechnicianID: second.TechnicianID})
	expectCode(t, err, domain.CodeConflict)
	_, err = svc.Reassign(ctx, northDispatcher, job.ID, booking.ReassignRequest{ServiceBayID: second.ServiceBayID})
	expectCode(t, err, domain.CodeConflict)

	_, err = svc.Reassign(ctx, customerAmelia, job.ID, booking.ReassignRequest{TechnicianID: other})
	expectCode(t, err, domain.CodeForbidden)
	history, _, err := svc.History(ctx, northDispatcher, job.ID, 0, nil)
	if err != nil || len(history) != 3 || history[1].EventType != domain.EventAppointmentReassigned || history[1].TechnicianID != other {
		t.Fatalf("history = %+v err=%v", history, err)
	}
}

// A reassign and a new booking racing for the same bay: GiST lets exactly one hold it, and the
// loser gets a clean 409/422, never a 500.
func TestReassignRacingBookingNeverDoubleBooks(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 10, 0)
	job, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleA, start))
	if err != nil {
		t.Fatal(err)
	}
	target := seed.BayN3
	if job.ServiceBayID == seed.BayN3 {
		target = seed.BayN2
	}
	var reassignErr, bookErr error
	var walkIn domain.Appointment
	race(func() {
		_, reassignErr = svc.Reassign(ctx, staffDispatcher, job.ID, booking.ReassignRequest{ServiceBayID: target})
	}, func() {
		walkIn, _, bookErr = svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleB, start))
	})
	for _, err := range []error{reassignErr, bookErr} {
		if err != nil && !isCleanLoss(err) {
			t.Fatalf("unexpected: %v", err)
		}
	}
	if reassignErr != nil && bookErr != nil {
		t.Fatalf("at least one side must win: reassign %v, book %v", reassignErr, bookErr)
	}
	end := start.Add(90 * time.Minute)
	holdsOn := func(column string, id uuid.UUID) int {
		return scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments
			WHERE `+column+` = $1 AND status IN (`+holdingCodes+`) AND tstzrange(start_at, end_at) && tstzrange($2, $3)`,
			id, start, end)
	}
	// Recount every resource the two writers touched: the target bay, the job's old bay, and both
	// technicians (the walk-in took the other brake technician, or nobody).
	for _, bay := range []uuid.UUID{target, job.ServiceBayID} {
		if n := holdsOn("service_bay_id", bay); n > 1 {
			t.Fatalf("%d holds on bay %s in one window", n, bay)
		}
	}
	for _, tech := range []uuid.UUID{seed.TechBlair, seed.TechCasey} {
		if n := holdsOn("technician_id", tech); n > 1 {
			t.Fatalf("%d holds for technician %s in one window", n, tech)
		}
	}
	now, err := svc.GetAppointment(ctx, staffAdmin, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if onTarget := now.ServiceBayID == target; onTarget != (reassignErr == nil) {
		t.Fatalf("job on bay %s after reassign err=%v", now.ServiceBayName, reassignErr)
	}
	wantMoves := 0
	if reassignErr == nil {
		wantMoves = 1
	}
	wantEvents(t, fmt.Sprintf("reassign err=%v", reassignErr), pool, job.ID, domain.EventAppointmentReassigned, wantMoves)
	if bookErr == nil {
		assertEventsFor(t, pool, walkIn.ID)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

func TestStaffAreScopedToTheirDealership(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day := londonDay(t, 1)

	_, err := svc.Schedule(ctx, northDispatcher, seed.DealerRiver, day)
	expectCode(t, err, domain.CodeForbidden)
	if _, err := svc.Schedule(ctx, northDispatcher, seed.DealerNorth, day); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.Book(ctx, northDispatcher, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 9, 0)))
	expectCode(t, err, domain.CodeForbidden)
	_, err = svc.Availability(ctx, northDispatcher, booking.AvailabilityQuery{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: day,
	})
	expectCode(t, err, domain.CodeForbidden)

	river, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.GetAppointment(ctx, northDispatcher, river.ID)
	expectCode(t, err, domain.CodeNotFound)
	_, err = svc.Cancel(ctx, northDispatcher, river.ID, 0)
	expectCode(t, err, domain.CodeNotFound)
	_, err = svc.Reschedule(ctx, northDispatcher, river.ID, booking.RescheduleRequest{StartAt: londonOn(t, londonDay(t, 1), 10, 0)})
	expectCode(t, err, domain.CodeNotFound)
	list, _, err := svc.ListAppointmentsPage(ctx, northDispatcher, booking.AppointmentQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		if a.DealershipID != seed.DealerNorth {
			t.Fatalf("north dispatcher listed %s at %s", a.ID, a.DealershipName)
		}
	}
	_, _, err = svc.ListAppointmentsPage(ctx, northDispatcher, booking.AppointmentQuery{DealershipID: seed.DealerRiver})
	expectCode(t, err, domain.CodeForbidden)

	// A dealership admin runs their own store but not the shared catalog or other stores.
	_, err = svc.UpdateDealership(ctx, riverAdmin, seed.DealerNorth, store.DealershipPatch{StagingCapacity: new(int16(3))})
	expectCode(t, err, domain.CodeForbidden)
	if _, err := svc.UpdateDealership(ctx, riverAdmin, seed.DealerRiver, store.DealershipPatch{StagingCapacity: new(int16(3))}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.UpdateServiceType(ctx, riverAdmin, seed.ServiceOil, store.ServiceTypePatch{MinSkillLevel: new(domain.LevelApprentice)})
	expectCode(t, err, domain.CodeForbidden)
	_, err = svc.UpdateTechnician(ctx, riverAdmin, seed.TechAlex, store.TechnicianPatch{SkillLevel: new(domain.LevelTechnician)})
	expectCode(t, err, domain.CodeNotFound)
	techs, _, err := svc.ListTechniciansAdmin(ctx, riverAdmin, uuid.Nil, 0, nil)
	if err != nil || len(techs) != 2 {
		t.Fatalf("river admin sees %d technicians (err %v), want Riverside's 2", len(techs), err)
	}
	_, err = svc.AddClosure(ctx, riverAdmin, seed.DealerNorth, domain.Closure{Date: day.String()}, 0)
	expectCode(t, err, domain.CodeForbidden)
}
