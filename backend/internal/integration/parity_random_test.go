package integration_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Availability and booking must agree on every start, whatever the workshop looks like. Each seed
// opens a dealership of its own with random bays, technicians (skills, levels, weekly shifts with
// breaks, an absence), lunch, and bookings already on the day — once without a staging cap and
// once with a random cap of 1–3 arrivals an hour — then checks, for a staff dispatcher and for a
// customer, every job mix as waiter and not:
//
//   - every start offered books, every start refused is refused (409/422);
//   - for up to three offered starts, booking again and again: before each booking availability
//     must say whether it succeeds (the one-move repair may fit more than the free counts, so the
//     count is checked step by step: at least min(free bays, free technicians) without a cap, at
//     most the cap with one);
//   - for a booked job, availability excluding it offers exactly the starts it can be rescheduled to.
//
// This is a self-consistency check: availability and booking are the same engine's two paths, so a
// rule both get wrong passes here. The independent check is cmd/stress TestOracleAgreesWithAvailability
// (raw rows and the documented rules restated outside the engine, against availability).
//
// The seed is logged; PARITY_SEED reruns one.
func TestAvailabilityParityRandomWorkshops(t *testing.T) {
	t.Parallel()
	seeds := []uint64{1, 2, 3, 4, 5}
	if raw := os.Getenv("PARITY_SEED"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			t.Fatalf("PARITY_SEED: %v", err)
		}
		seeds = []uint64{n}
	}
	for _, sd := range seeds {
		for _, staged := range []bool{false, true} {
			t.Run(fmt.Sprintf("seed=%d/staging=%v", sd, staged), func(t *testing.T) {
				t.Parallel()
				parityRandomWorkshop(t, sd, staged)
			})
		}
	}
}

// parityRandomWorkshop is one seed of TestAvailabilityParityRandomWorkshops.
func parityRandomWorkshop(t *testing.T, sd uint64, staged bool) {
	pool, _, svc, ctx := openTestDB(t)
	rng := rand.New(rand.NewPCG(sd, 99)) //nolint:gosec // reproducible fixture
	capacity := int16(0)
	if staged {
		capacity = int16(1 + rand.New(rand.NewPCG(sd, 7)).IntN(3)) //nolint:gosec // reproducible fixture
	}
	dealer, err := svc.CreateDealership(ctx, staffAdmin, store.NewDealership{
		Name: fmt.Sprintf("Parity %d", sd), Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "17:00",
		LunchStart: "12:00", LunchEnd: "12:30", StagingWindowMinutes: 60, StagingCapacity: capacity,
		Policy: domain.Policy{CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 90, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 + rng.IntN(4) {
		bayType := []domain.BayType{domain.BayExpress, domain.BayGeneral, domain.BayGeneral, domain.BayDiagnostic}[rng.IntN(4)]
		switch i { // at least one express and one general bay
		case 0:
			bayType = domain.BayExpress
		case 1:
			bayType = domain.BayGeneral
		}
		if _, err := svc.CreateBay(ctx, staffAdmin, dealer.ID, fmt.Sprintf("Bay %d", i), bayType); err != nil {
			t.Fatal(err)
		}
	}
	day := londonDay(t, 1)
	for i := range 2 + rng.IntN(4) {
		var skills []domain.TechnicianSkill
		for _, sk := range []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral} {
			if rng.IntN(2) == 0 {
				skills = append(skills, sk)
			}
		}
		if len(skills) == 0 {
			skills = []domain.TechnicianSkill{domain.SkillGeneral}
		}
		tech, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{
			DealershipID: dealer.ID, Name: fmt.Sprintf("Tech %d", i), SkillLevel: []domain.SkillLevel{domain.LevelApprentice, domain.LevelTechnician, domain.LevelSenior, domain.LevelMaster}[rng.IntN(4)], Skills: skills,
		})
		if err != nil {
			t.Fatal(err)
		}
		switch rng.IntN(3) {
		case 0: // a weekly shift with a break, every day of the week
			var shifts []domain.TechnicianShift
			for wd := int16(1); wd <= 7; wd++ {
				shifts = append(shifts, domain.TechnicianShift{Weekday: wd, StartTime: fmt.Sprintf("%02d:00", 8+rng.IntN(2)),
					EndTime: fmt.Sprintf("%02d:00", 15+rng.IntN(3)), BreakStart: "10:30", BreakEnd: "11:00"})
			}
			if _, err := svc.ReplaceShifts(ctx, staffAdmin, tech.ID, shifts, 0); err != nil {
				t.Fatal(err)
			}
		case 1: // an absence on the day
			from := londonOn(t, day, 9+rng.IntN(5), 0)
			if _, err := svc.AddAbsence(ctx, staffAdmin, tech.ID, domain.TechnicianAbsence{StartsAt: from, EndsAt: from.Add(90 * time.Minute), Reason: "parity"}, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	mixes := [][]uuid.UUID{{seed.ServiceOil}, {seed.ServiceBrake}, {seed.ServiceMajor}, {seed.ServiceOil, seed.ServiceBrake}}
	var booked []domain.Appointment
	for range 10 + rng.IntN(10) { // some of the day already sold (refusals are fine)
		appt, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
			DealershipID: dealer.ID, VehicleID: newVehicle(t, svc), ServiceTypeIDs: mixes[rng.IntN(len(mixes))],
			StartAt: londonOn(t, day, 8, 0).Add(time.Duration(rng.IntN(16)) * 30 * time.Minute), IdempotencyKey: uuid.NewString(),
			IsWaiter: rng.IntN(2) == 0,
		})
		if err == nil {
			booked = append(booked, appt)
		} else if !isCleanLoss(err) {
			t.Fatal(err)
		}
	}
	// A dispatcher has moved some of them since (any valid bay or technician, not the best fit):
	// the states the one-move repair exists for, so the parity below covers it too.
	board, err := svc.Schedule(ctx, staffAdmin, dealer.ID, day)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 * len(booked) {
		appt := booked[i%len(booked)]
		req := booking.ReassignRequest{}
		if rng.IntN(2) == 0 {
			req.ServiceBayID = board.Bays[rng.IntN(len(board.Bays))].ID
		}
		if rng.IntN(2) == 0 {
			req.TechnicianID = board.Technicians[rng.IntN(len(board.Technicians))].ID
		}
		if req.ServiceBayID == uuid.Nil && req.TechnicianID == uuid.Nil {
			continue
		}
		if _, err := svc.Reassign(ctx, staffAdmin, appt.ID, req); err != nil && !isCleanLoss(err) {
			t.Fatal(err)
		}
	}
	movesBefore := scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE event_type = `+code(domain.EventAppointmentReassigned))
	customer := domain.Actor{UserID: seed.DemoCustomerID, Role: domain.RoleCustomer, CustomerID: seed.CustomerA}
	free, refused := 0, 0
	for _, actor := range []domain.Actor{staffAdmin, customer} {
		for _, mix := range mixes {
			for _, waiter := range []bool{false, true} {
				f, r := assertParity(t, svc, actor, dealer.ID, day, mix, waiter, int(capacity))
				free, refused = free+f, refused+r
			}
		}
	}
	if free == 0 || refused == 0 {
		t.Fatalf("free=%d refused=%d: the random day must mix both", free, refused)
	}
	if len(booked) > 0 {
		assertRescheduleParity(t, svc, booked[rng.IntN(len(booked))], day)
	}
	// Moves made by the one-move repair (every reassignment after the dispatcher's): logged, so a
	// run shows the parity above covered it.
	t.Logf("repair moves: %d", scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE event_type = `+code(domain.EventAppointmentReassigned))-movesBefore)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertStagingWithinCapacity(t, pool)
}

// assertParity books every start of day for mix as actor (a fresh car each time) and checks it
// succeeds exactly when availability offered it; for up to three offered starts it books again and
// again until refused, asking availability before each booking whether it will succeed (capacity
// is the staging cap, 0 = none). Everything it books is cancelled again (moves the repair made
// stay, so availability is read again before every start). It returns the offered and refused
// counts.
func assertParity(t *testing.T, svc *booking.Service, actor domain.Actor, dealer uuid.UUID, day booking.Date, mix []uuid.UUID, waiter bool, capacity int) (free, refused int) {
	t.Helper()
	ctx := t.Context()
	label := fmt.Sprintf("%v mix=%d waiter=%v", actor.Role, len(mix), waiter)
	availability := func() domain.Availability {
		avail, err := svc.Availability(ctx, actor, booking.AvailabilityQuery{
			DealershipID: dealer, VehicleID: newVehicleCtx(ctx, t, svc), ServiceTypeIDs: mix, Day: day, IsWaiter: waiter,
		})
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return avail
	}
	counted := 0
	for _, start := range availability().Slots {
		// Read again for each start: a move the repair made for an earlier start outlives that
		// booking's cancellation, so the first read may be stale by now.
		slot := slotAt(t, availability(), start.Start)
		book := func() (domain.Appointment, error) {
			appt, _, err := svc.Book(ctx, actor, booking.BookRequest{
				DealershipID: dealer, VehicleID: newVehicleCtx(ctx, t, svc), ServiceTypeIDs: mix, StartAt: slot.Start,
				IsWaiter: waiter, IdempotencyKey: uuid.NewString(),
			})
			return appt, err
		}
		appt, err := book()
		if slot.Available != (err == nil) {
			t.Fatalf("%s %s: available=%v (reason %q, bays %d techs %d) but booking err=%v",
				label, slot.Start.Format("15:04"), slot.Available, slot.Reason, slot.FreeBays, slot.FreeTechnicians, err)
		}
		if err != nil {
			if !isCleanLoss(err) {
				t.Fatalf("%s %s: refused with %v", label, slot.Start.Format("15:04"), err)
			}
			refused++
			continue
		}
		free++
		made := []domain.Appointment{appt}
		if counted < 3 {
			counted++
			for {
				next := slotAt(t, availability(), slot.Start)
				appt, err := book()
				if next.Available != (err == nil) {
					t.Fatalf("%s %s after %d bookings: available=%v (reason %q, bays %d techs %d) but booking err=%v",
						label, slot.Start.Format("15:04"), len(made), next.Available, next.Reason, next.FreeBays, next.FreeTechnicians, err)
				}
				if err != nil {
					if !isCleanLoss(err) {
						t.Fatalf("%s %s: refused with %v", label, slot.Start.Format("15:04"), err)
					}
					break
				}
				made = append(made, appt)
			}
			if capacity == 0 && len(made) < min(slot.FreeBays, slot.FreeTechnicians) {
				t.Fatalf("%s %s: offered %d bays / %d technicians free, held only %d bookings",
					label, slot.Start.Format("15:04"), slot.FreeBays, slot.FreeTechnicians, len(made))
			}
			if capacity > 0 && len(made) > capacity {
				t.Fatalf("%s %s: %d bookings arrive in one window, cap %d", label, slot.Start.Format("15:04"), len(made), capacity)
			}
		}
		for _, a := range made {
			if _, err := svc.Cancel(ctx, staffAdmin, a.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	return free, refused
}

// assertRescheduleParity checks that availability excluding appt's own hold offers exactly the
// starts appt can be rescheduled to (moving it back after each successful move).
func assertRescheduleParity(t *testing.T, svc *booking.Service, appt domain.Appointment, day booking.Date) {
	t.Helper()
	ctx := t.Context()
	ids := make([]uuid.UUID, len(appt.Lines))
	for i, l := range appt.Lines {
		ids[i] = l.ServiceTypeID
	}
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: appt.DealershipID, VehicleID: appt.VehicleID, ServiceTypeIDs: ids, Day: day,
		IsWaiter: appt.IsWaiter, ExcludeAppointmentID: appt.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	home := appt.StartAt
	for _, slot := range avail.Slots {
		moved, err := svc.Reschedule(ctx, staffAdmin, appt.ID, booking.RescheduleRequest{StartAt: slot.Start})
		if slot.Available != (err == nil) {
			t.Fatalf("reschedule to %s: available=%v (reason %q) but err=%v", slot.Start.Format("15:04"), slot.Available, slot.Reason, err)
		}
		if err != nil {
			if !isCleanLoss(err) {
				t.Fatalf("reschedule to %s: %v", slot.Start.Format("15:04"), err)
			}
			continue
		}
		if !moved.StartAt.Equal(home) {
			if _, err := svc.Reschedule(ctx, staffAdmin, appt.ID, booking.RescheduleRequest{StartAt: home}); err != nil {
				t.Fatalf("move back to %s: %v", home.Format("15:04"), err)
			}
		}
	}
}

// newVehicleCtx is newVehicle with an explicit context (a fresh car of the demo customer).
func newVehicleCtx(ctx context.Context, t testing.TB, svc *booking.Service) uuid.UUID {
	t.Helper()
	v, err := svc.CreateVehicle(ctx, staffAdmin, booking.VehicleInput{
		CustomerID: seed.CustomerA, VIN: strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:17], Make: "Test", Model: "Car", Year: 2024,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v.ID
}
