package integration_test

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// The one-move repair (booking/repair.go): when no bay or technician is free, a booking moves one
// confirmed, not yet started appointment to another bay or technician that fits it, same window,
// and takes the freed one. Availability offers exactly those slots.

// repairShop is a quick lane (Bay A) and a general bay (Bay B) with an oil change sitting in the
// general bay (an express spill a dispatcher left there): a brake job at the same time finds no
// general bay free, but the oil change fits the idle quick lane.
func repairShop(t *testing.T, svc *booking.Service, day booking.Date) (dealer uuid.UUID, oil domain.Appointment, bays map[string]uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	dealer, _ = policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral},
		[]policyTech{{name: "Aaron", level: 2, skills: lubeBrakes}, {name: "Bea", level: 2, skills: lubeBrakes}})
	oil, err := policyBook(ctx, t, svc, dealer, seed.ServiceOil, londonOn(t, day, 10, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	bays = shopBays(t, svc, dealer, day)
	if oil, err = svc.Reassign(ctx, staffAdmin, oil.ID, booking.ReassignRequest{ServiceBayID: bays["Bay B"], IfMatch: oil.Version}); err != nil {
		t.Fatalf("spill the oil change into the general bay: %v", err)
	}
	return dealer, oil, bays
}

// shopBays maps the dealership's bay names to ids (from the day board).
func shopBays(t *testing.T, svc *booking.Service, dealer uuid.UUID, day booking.Date) map[string]uuid.UUID {
	t.Helper()
	board, err := svc.Schedule(t.Context(), staffAdmin, dealer, day)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uuid.UUID{}
	for _, b := range board.Bays {
		out[b.Name] = b.ID
	}
	return out
}

func TestRepairMovesABookingToAnotherBay(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, oil, bays := repairShop(t, svc, day)
	start := londonOn(t, day, 10, 0)

	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: dealer, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake, Day: day})
	if err != nil {
		t.Fatal(err)
	}
	if s := slotAt(t, avail, start); !s.Available || s.FreeBays != 0 {
		t.Fatalf("10:00 brake: available=%v freeBays=%d reason=%q, want offered with no bay free (by a move)", s.Available, s.FreeBays, s.Reason)
	}
	brake, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, start, false)
	if err != nil {
		t.Fatalf("brake job refused although the oil change could move to the quick lane: %v", err)
	}
	if brake.ServiceBayID != bays["Bay B"] {
		t.Fatalf("brake on %s, want Bay B (freed)", brake.ServiceBayName)
	}
	moved := mustGet(t, svc, oil.ID)
	if moved.ServiceBayID != bays["Bay A"] {
		t.Fatalf("oil change on %s, want moved to Bay A", moved.ServiceBayName)
	}
	// Nothing the customer sees changes: same window, car, technician, status; only the bay.
	if !moved.StartAt.Equal(oil.StartAt) || !moved.EndAt.Equal(oil.EndAt) || moved.VehicleID != oil.VehicleID ||
		moved.TechnicianID != oil.TechnicianID || moved.Status != domain.StatusConfirmed || moved.Version != oil.Version+1 {
		t.Fatalf("moved oil change changed more than its bay: before %+v after %+v", oil, moved)
	}
	wantEvents(t, "repair move", pool, oil.ID, domain.EventAppointmentReassigned, 2) // the dispatcher's spill + the repair
	assertEventsFor(t, pool, brake.ID)

	avail, err = svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: dealer, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake, Day: day})
	if err != nil {
		t.Fatal(err)
	}
	if s := slotAt(t, avail, start); s.Available {
		t.Fatalf("10:00 brake still offered after the repair: %+v", s)
	}
	_, err = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, start, false)
	expectCode(t, err, domain.CodeUnavailable)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// The technician side: the only technician with brakes holds an oil change another lubrication
// technician could do; the brake job moves it and takes him.
func TestRepairMovesABookingToAnotherTechnician(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, techs := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral},
		[]policyTech{{name: "Aaron", level: 2, skills: lubeBrakes}, {name: "Bea", level: 1, skills: lube}})
	start := londonOn(t, day, 10, 0)
	oil, err := policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, false)
	if err != nil {
		t.Fatal(err)
	}
	if oil.TechnicianID != techs["Bea"] {
		t.Fatalf("oil change on %s, want Bea (lowest level)", oil.TechnicianName)
	}
	if oil, err = svc.Reassign(ctx, staffAdmin, oil.ID, booking.ReassignRequest{TechnicianID: techs["Aaron"], IfMatch: oil.Version}); err != nil {
		t.Fatal(err)
	}
	brake, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, start, false)
	if err != nil {
		t.Fatalf("brake job refused although the oil change could move to Bea: %v", err)
	}
	if brake.TechnicianID != techs["Aaron"] {
		t.Fatalf("brake with %s, want Aaron (freed)", brake.TechnicianName)
	}
	moved := mustGet(t, svc, oil.ID)
	if moved.TechnicianID != techs["Bea"] || moved.ServiceBayID != oil.ServiceBayID || !moved.StartAt.Equal(oil.StartAt) {
		t.Fatalf("oil change after the repair: %s / %s at %s, want Bea in the same bay and window",
			moved.TechnicianName, moved.ServiceBayName, moved.StartAt)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Work that is checked in or in progress never moves (the car may be in the bay); neither does a
// booking that has already started.
func TestRepairNeverMovesArrivedOrStartedWork(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for i, status := range []domain.AppointmentStatus{domain.StatusCheckedIn, domain.StatusInProgress} {
		day := londonDay(t, 1+i)
		dealer, oil, _ := repairShop(t, svc, day)
		mustExec(t, pool, `UPDATE appointments SET status = $2 WHERE id = $1`, oil.ID, int16(status))
		avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: dealer, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceBrake, Day: day})
		if err != nil {
			t.Fatal(err)
		}
		if s := slotAt(t, avail, londonOn(t, day, 10, 0)); s.Available {
			t.Fatalf("%s: 10:00 offered by moving %s work", day, status.Name())
		}
		_, err = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 10, 0), false)
		expectCode(t, err, domain.CodeUnavailable)
		if got := mustGet(t, svc, oil.ID); got.ServiceBayID != oil.ServiceBayID {
			t.Fatalf("%s work moved to %s", status.Name(), got.ServiceBayName)
		}
	}
	// Started: the clock is past the oil change's 10:00 start; a brake job at 10:00 is in the past,
	// so one overlapping it later is needed — a 90-minute brake at 10:30 overlaps nothing at 10:00,
	// hence the oil change is made a 60-minute job (10:00–11:00) first.
	day := londonDay(t, 0)
	dealer, oil, _ := repairShop(t, svc, day)
	mustExec(t, pool, `UPDATE appointments SET end_at = start_at + interval '60 minutes', booked_duration_minutes = 60 WHERE id = $1`, oil.ID)
	clock := serviceOn(t, st, day, 10, 5)
	_, err := policyBook(ctx, t, clock, dealer, seed.ServiceBrake, londonOn(t, day, 10, 30), false)
	expectCode(t, err, domain.CodeUnavailable)
	if got := mustGet(t, svc, oil.ID); got.ServiceBayID != oil.ServiceBayID {
		t.Fatalf("started booking moved to %s", got.ServiceBayName)
	}
	assertNoOverlaps(t, pool)
}

// Two bookings race for the one bay a repair can free: exactly one wins, on every round, and the
// moved booking is moved once.
func TestRepairRaceOneWinner(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 5; offset++ {
		day := londonDay(t, offset)
		dealer, oil, _ := repairShop(t, svc, day)
		start := londonOn(t, day, 10, 0)
		errs := make([]error, 2)
		raceN(2, func(i int) {
			_, errs[i] = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, start, false)
		})
		oneWinner(t, fmt.Sprintf("round %d", offset), allLose(errs, refusals...)...)
		wantEvents(t, "repair move", pool, oil.ID, domain.EventAppointmentReassigned, 2)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}

// A customer's booking repairs too, but the move's history names no user (the system made it).
func TestRepairByCustomerRecordsNoActor(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 2)
	dealer, oil, _ := repairShop(t, svc, day)
	customer := domain.Actor{UserID: seed.DemoCustomerID, Role: domain.RoleCustomer, CustomerID: seed.CustomerA}
	if _, _, err := svc.Book(ctx, customer, booking.BookRequest{
		DealershipID: dealer, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake, StartAt: londonOn(t, day, 10, 0), IdempotencyKey: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	actors := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointment_history
		WHERE appointment_id = $1 AND event_type = `+code(domain.EventAppointmentReassigned)+` AND actor_id IS NULL`, oil.ID)
	if actors != 1 {
		t.Fatalf("%d system-made moves in the history, want 1", actors)
	}
}

// Availability and booking agree on the starts only a move makes bookable, on random days built to
// need moves: a quick lane and two general bays, technicians of equal standing (so a move between
// them is allowed), half the bookings waiters, then a dispatcher reassigning work at random. Every
// start of every mix is booked as staff and as a customer (assertParity: offered exactly when it
// books, again and again on a few starts); the runs must make moves, or they prove nothing.
func TestAvailabilityParityRepairHeavy(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	customer := domain.Actor{UserID: seed.DemoCustomerID, Role: domain.RoleCustomer, CustomerID: seed.CustomerA}
	mixes := [][]uuid.UUID{{seed.ServiceOil}, {seed.ServiceBrake}, {seed.ServiceOil, seed.ServiceBrake}}
	moves := 0
	for sd := range uint64(6) {
		r := rand.New(rand.NewPCG(sd, 0xb0a7)) //nolint:gosec // reproducible fixture
		dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral, domain.BayGeneral},
			[]policyTech{{name: "Aaron", level: 2, skills: lubeBrakes}, {name: "Bea", level: 2, skills: lubeBrakes}, {name: "Cal", level: 1, skills: lube}})
		day := londonDay(t, 1+int(sd))
		var booked []domain.Appointment
		for range 14 {
			appt, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
				DealershipID: dealer, VehicleID: newVehicle(t, svc), ServiceTypeIDs: mixes[r.IntN(len(mixes))],
				StartAt: londonOn(t, day, 8, 0).Add(time.Duration(r.IntN(18)) * 30 * time.Minute), IdempotencyKey: uuid.NewString(),
				IsWaiter: r.IntN(2) == 0,
			})
			if err == nil {
				booked = append(booked, appt)
			} else if !isCleanLoss(err) {
				t.Fatal(err)
			}
		}
		board, err := svc.Schedule(ctx, staffAdmin, dealer, day)
		if err != nil {
			t.Fatal(err)
		}
		for i := range 3 * len(booked) {
			req := booking.ReassignRequest{ServiceBayID: board.Bays[r.IntN(len(board.Bays))].ID}
			if r.IntN(2) == 0 {
				req.TechnicianID = board.Technicians[r.IntN(len(board.Technicians))].ID
			}
			if _, err := svc.Reassign(ctx, staffAdmin, booked[i%len(booked)].ID, req); err != nil && !isCleanLoss(err) {
				t.Fatal(err)
			}
		}
		before := scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE event_type = `+code(domain.EventAppointmentReassigned))
		for _, actor := range []domain.Actor{staffAdmin, customer} {
			for _, mix := range mixes {
				for _, waiter := range []bool{false, true} {
					assertParity(t, svc, actor, dealer, day, mix, waiter, 0)
				}
			}
		}
		moves += scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE event_type = `+code(domain.EventAppointmentReassigned)) - before
	}
	t.Logf("repair moves over the runs: %d", moves)
	if moves == 0 {
		t.Fatal("no booking needed a move: the fixture no longer exercises the repair")
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// A reschedule whose only move would land on the place the rescheduled appointment still holds:
// the move runs before the reschedule's own, so that place is not free for it. Availability must
// not offer the start and the reschedule must refuse it (422), not collide in GiST (409).
//
//	Bay A: R 08:00–09:30, E 10:30–12:00     Bay B: x 09:00–10:30, y 11:30–13:00   (brakes: 90 min)
//
// R moves to 10:00. Bay A is held by E (which cannot move: y holds Bay B during it), Bay B only by
// x; x would fit Bay A only once R has left it.
func TestRescheduleRepairNeverTargetsItsOwnPlace(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral},
		[]policyTech{{name: "T1", level: 2, skills: lubeBrakes}, {name: "T2", level: 2, skills: lubeBrakes},
			{name: "T3", level: 2, skills: lubeBrakes}, {name: "T4", level: 2, skills: lubeBrakes}})
	bays := shopBays(t, svc, dealer, day)
	place := func(hour, minute int, bay string) domain.Appointment {
		t.Helper()
		a, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, hour, minute), false)
		if err != nil {
			t.Fatal(err)
		}
		if a.ServiceBayID != bays[bay] {
			if a, err = svc.Reassign(ctx, staffAdmin, a.ID, booking.ReassignRequest{ServiceBayID: bays[bay], IfMatch: a.Version}); err != nil {
				t.Fatalf("place %s on %s: %v", a.StartAt, bay, err)
			}
		}
		return a
	}
	r := place(8, 0, "Bay A")
	x := place(9, 0, "Bay B")
	e := place(10, 30, "Bay A")
	y := place(11, 30, "Bay B")
	target := londonOn(t, day, 10, 0)

	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: dealer, VehicleID: r.VehicleID,
		ServiceTypeID: seed.ServiceBrake, Day: day, ExcludeAppointmentID: r.ID})
	if err != nil {
		t.Fatal(err)
	}
	if s := slotAt(t, avail, target); s.Available {
		t.Fatalf("10:00 offered for R although its only move lands on R's own place: %+v", s)
	}
	_, err = svc.Reschedule(ctx, staffAdmin, r.ID, booking.RescheduleRequest{StartAt: target, IfMatch: r.Version})
	expectCode(t, err, domain.CodeUnavailable)
	for _, a := range []domain.Appointment{r, x, e, y} {
		if got := mustGet(t, svc, a.ID); got.Version != a.Version || got.ServiceBayID != a.ServiceBayID {
			t.Fatalf("%s changed by a refused reschedule: before %+v after %+v", a.StartAt, a, got)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
