package integration_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The appointment's customer is the vehicle's owner, read from the vehicle (Noah owns VehicleB),
// never the acting user nor a default: the stored row says so, not just the response.
func TestBookAssignsCustomerFromVehicle(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day, loc := suiteToday()
	// 13:00: the seed holds VehicleB at Northside 10:00–11:30.
	start := booking.Clock{Hour: 13}.On(day, loc)

	item, created, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleB,
		ServiceTypeID:  seed.ServiceOil,
		StartAt:        start,
		IdempotencyKey: "test:customer:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected create")
	}
	owner := scalar[uuid.UUID](t, pool, `SELECT customer_id FROM vehicles WHERE id = $1`, seed.VehicleB)
	stored := scalar[uuid.UUID](t, pool, `SELECT customer_id FROM appointments WHERE id = $1`, item.ID)
	if owner != seed.CustomerB || stored != owner || item.CustomerID != owner {
		t.Fatalf("appointment customer: stored %s, response %s, want the vehicle's owner %s (Noah, %s)",
			stored, item.CustomerID, owner, seed.CustomerB)
	}
	if item.CustomerName != "Noah Patel" {
		t.Fatalf("customer = %q, want Noah Patel", item.CustomerName)
	}
	if item.ServiceBayName == "" || item.TechnicianName == "" {
		t.Fatal("expected bay and technician allocation")
	}
}

func TestBookRejectsOffGridAndOutsideHours(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, loc := suiteToday()

	// Off the grid is a malformed request (400); on the grid but outside the hours is a slot that
	// cannot be booked (422), like a closure or lunch.
	cases := []struct {
		name  string
		start time.Time
		want  string
	}{
		{"before open", booking.Clock{Hour: 7}.On(day, loc), domain.CodeUnavailable},
		{"off grid", booking.Clock{Hour: 8, Minute: 15}.On(day, loc), domain.CodeInvalid},
		{"overflow close", booking.Clock{Hour: 16}.On(day, loc), domain.CodeUnavailable}, // brake 90m ends 17:30 > 17:00
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serviceID := seed.ServiceOil
			if tc.name == "overflow close" {
				serviceID = seed.ServiceBrake
			}
			_, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
				DealershipID:  seed.DealerNorth,
				VehicleID:     seed.VehicleA,
				ServiceTypeID: serviceID,
				StartAt:       tc.start,
			})
			domainErr, ok := domain.AsError(err)
			if !ok || domainErr.Code != tc.want {
				t.Fatalf("got %v, want code %s", err, tc.want)
			}
		})
	}
}

func TestBackToBackSameBayAllowed(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, loc := suiteToday()

	first, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleA,
		ServiceTypeID:  seed.ServiceOil,
		StartAt:        booking.Clock{Hour: 12}.On(day, loc),
		IdempotencyKey: "test:btb:1:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleB,
		ServiceTypeID:  seed.ServiceOil,
		StartAt:        booking.Clock{Hour: 12, Minute: 30}.On(day, loc),
		IdempotencyKey: "test:btb:2:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ServiceBayID != second.ServiceBayID {
		t.Fatalf("expected same express bay chain, got %s vs %s", first.ServiceBayName, second.ServiceBayName)
	}
}

func TestIdempotencyReplayAndConflict(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, loc := suiteToday()
	key := "test:idem:" + uuid.NewString()
	start := booking.Clock{Hour: 13}.On(day, loc)

	first, created, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil,
		StartAt: start, IdempotencyKey: key,
	})
	if err != nil || !created {
		t.Fatalf("create: %v created=%v", err, created)
	}
	second, created, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil,
		StartAt: start, IdempotencyKey: key,
	})
	if err != nil || created {
		t.Fatalf("replay: %v created=%v", err, created)
	}
	if first.ID != second.ID {
		t.Fatal("replay must return same id")
	}
	_, _, err = svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceOil,
		StartAt: start, IdempotencyKey: key,
	})
	domainErr, ok := domain.AsError(err)
	if !ok || domainErr.Code != domain.CodeConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestCancelThenRebook(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, loc := suiteToday()
	start := booking.Clock{Hour: 14}.On(day, loc)

	item, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil,
		StartAt: start, IdempotencyKey: "test:cancel:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := svc.Cancel(ctx, domain.SystemActor, item.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != domain.StatusCancelled {
		t.Fatalf("status=%d", cancelled.Status)
	}
	_, created, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceOil,
		StartAt: start, IdempotencyKey: "test:rebook:" + uuid.NewString(),
	})
	if err != nil || !created {
		t.Fatalf("rebook: %v created=%v", err, created)
	}
}

// Why a seeded Northside slot is refused, per reason: the two 10:00 brake jobs hold both general
// bays and both brake technicians (no_resources, 0/0), and fill the 10:00 arrival window
// (staging_capacity 2) for anything else; 12:00 is the lunch break.
func TestAvailabilitySlotReasons(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, loc := suiteToday()
	for _, tc := range []struct {
		name    string
		service uuid.UUID
		hour    int
		reason  string
		zeroed  bool // no free bay and no free technician reported
	}{
		{"brake under the seeded brakes", seed.ServiceBrake, 10, "no_resources", true},
		{"oil in the full arrival window", seed.ServiceOil, 10, "staging_full", false},
		{"oil over lunch", seed.ServiceOil, 12, "lunch_break", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			avail, err := svc.Availability(ctx, domain.SystemActor, booking.AvailabilityQuery{
				DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: tc.service, Day: day,
			})
			if err != nil {
				t.Fatal(err)
			}
			slot := slotAt(t, avail, booking.Clock{Hour: tc.hour}.On(day, loc))
			if slot.Available || slot.Reason != tc.reason {
				t.Fatalf("%02d:00 available=%v reason=%q, want reason %q", tc.hour, slot.Available, slot.Reason, tc.reason)
			}
			if tc.zeroed && (slot.FreeBays != 0 || slot.FreeTechnicians != 0) {
				t.Fatalf("%02d:00 bays=%d techs=%d, want 0/0", tc.hour, slot.FreeBays, slot.FreeTechnicians)
			}
		})
	}
}

// An oil change needs an express bay and a lubrication technician. The fixture puts the wrong
// choices first in every order the allocator uses: the diagnostic bay sorts before the express bay
// by name, and the brakes-only technician has the lowest level, the first name, and an empty day.
// So the first booking goes to the express bay with a lubrication technician only because of the
// skill filter, and a second car at the same start (express bay taken, a lubrication technician
// still free) is refused only because of the bay-type filter: the diagnostic bay is free.
func TestOilNeverUsesDiagnosticBayAndRequiresLubricationSkill(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	d := provisionShop(t, svc, "Filters")
	diagnostic := addBay(t, svc, d.ID, "A Diagnostic", domain.BayDiagnostic)
	express := addBay(t, svc, d.ID, "B Express", domain.BayExpress)
	brakesOnly := addTechnician(t, svc, d.ID, "Aaron Brakes", 1, domain.SkillBrakes)
	lube := []uuid.UUID{
		addTechnician(t, svc, d.ID, "Yan Lube", 3, domain.SkillLubrication),
		addTechnician(t, svc, d.ID, "Zed Lube", 3, domain.SkillLubrication),
	}
	start := londonOn(t, londonDay(t, 1), 8, 0)

	item, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceOil, start)
	if err != nil {
		t.Fatal(err)
	}
	if item.ServiceBayID != express || item.ServiceBayType != domain.BayExpress {
		t.Fatalf("oil bay = %s (type %d), want B Express", item.ServiceBayName, item.ServiceBayType)
	}
	if item.TechnicianID == brakesOnly || !slices.Contains(lube, item.TechnicianID) {
		t.Fatalf("oil technician = %s, want a lubrication technician (Aaron has brakes only)", item.TechnicianName)
	}

	_, err = bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceOil, start)
	expectCode(t, err, domain.CodeUnavailable)
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE service_bay_id = $1`, diagnostic); n != 0 {
		t.Fatalf("%d appointment(s) on the diagnostic bay", n)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE technician_id = $1`, brakesOnly); n != 0 {
		t.Fatalf("%d appointment(s) on the brakes-only technician", n)
	}
}

func TestMajorRequiresSkillLevelThree(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day, loc := suiteToday()
	item, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID:   seed.DealerNorth,
		VehicleID:      seed.VehicleA,
		ServiceTypeID:  seed.ServiceMajor,
		StartAt:        booking.Clock{Hour: 13}.On(day, loc),
		IdempotencyKey: "test:skill-level:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Drew is general L1; Alex general L2 — Major needs L3 → Blair.
	if item.TechnicianID != seed.TechBlair {
		t.Fatalf("tech=%s want Blair (skill level 3)", item.TechnicianName)
	}
}

// The workshop lifecycle confirmed → checked_in → in_progress → completed (and confirmed →
// no_show) bumps the version once per step, stamps each step's time, records one outbox event and
// one history row per step with the acting user (invariant 7), and a finished job releases its
// bay and technician.
func TestWorkshopLifecycleReleasesResources(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	began := time.Now()
	item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleA,
		ServiceTypeID:  seed.ServiceOil,
		StartAt:        londonOn(t, today, 8, 0),
		IdempotencyKey: "test:lifecycle:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantEvents(t, "book", pool, item.ID, domain.EventAppointmentCreated, 1)
	wantActor(t, pool, item.ID, domain.EventAppointmentCreated, seed.DemoAdminID)

	at8 := serviceOn(t, st, today, 8, 0)
	prev := item
	steps := []struct {
		event  domain.EventType
		status domain.AppointmentStatus
		run    func(id uuid.UUID) (domain.Appointment, error)
		stamp  func(domain.Appointment) *time.Time
	}{
		{domain.EventAppointmentCheckedIn, domain.StatusCheckedIn,
			func(id uuid.UUID) (domain.Appointment, error) { return at8.CheckIn(ctx, staffDispatcher, id, 0) },
			func(a domain.Appointment) *time.Time { return a.CheckedInAt }},
		{domain.EventAppointmentStarted, domain.StatusInProgress,
			func(id uuid.UUID) (domain.Appointment, error) { return at8.StartWork(ctx, staffDispatcher, id, 0) },
			func(a domain.Appointment) *time.Time { return a.StartedAt }},
		{domain.EventAppointmentCompleted, domain.StatusCompleted,
			func(id uuid.UUID) (domain.Appointment, error) { return at8.Complete(ctx, staffDispatcher, id, 0) },
			func(a domain.Appointment) *time.Time { return a.CompletedAt }},
	}
	for _, step := range steps {
		got, err := step.run(item.ID)
		if err != nil {
			t.Fatalf("%s: %v", step.event, err)
		}
		if got.Status != step.status || got.Version != prev.Version+1 {
			t.Fatalf("%s: status %s v%d, want %s v%d", step.event, got.Status.Name(), got.Version, step.status.Name(), prev.Version+1)
		}
		// Stamped by the database clock, in step order.
		if at := step.stamp(got); at == nil || at.Before(began.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
			t.Fatalf("%v: step time %v, want about now", step.event, at)
		}
		wantEvents(t, step.event.Name(), pool, item.ID, step.event, 1)
		wantActor(t, pool, item.ID, step.event, seed.DemoDispatcherID)
		prev = got
	}
	if prev.StartedAt.Before(*prev.CheckedInAt) || prev.CompletedAt.Before(*prev.StartedAt) {
		t.Fatalf("step times out of order: checked in %v, started %v, completed %v", prev.CheckedInAt, prev.StartedAt, prev.CompletedAt)
	}

	// The same slot is bookable again: completing released the bay and technician.
	again, created, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleB,
		ServiceTypeID:  seed.ServiceOil,
		StartAt:        item.StartAt,
		IdempotencyKey: "test:lifecycle:rebook:" + uuid.NewString(),
	})
	if err != nil || !created {
		t.Fatalf("rebook after complete: %v created=%v", err, created)
	}
	if again.ServiceBayID != item.ServiceBayID || again.TechnicianID != item.TechnicianID {
		t.Fatalf("expected the freed bay and technician, got %s / %s vs %s / %s",
			again.ServiceBayName, again.TechnicianName, item.ServiceBayName, item.TechnicianName)
	}

	// A no-show (after the booked start) releases the slot too.
	noShow, err := serviceOn(t, st, today, 8, 40).NoShow(ctx, staffDispatcher, again.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if noShow.Status != domain.StatusNoShow || noShow.Version != again.Version+1 {
		t.Fatalf("no-show: status %s v%d, want no_show v%d", noShow.Status.Name(), noShow.Version, again.Version+1)
	}
	wantEvents(t, "no-show", pool, again.ID, domain.EventAppointmentNoShow, 1)
	wantActor(t, pool, again.ID, domain.EventAppointmentNoShow, seed.DemoDispatcherID)
	if _, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleC, seed.ServiceOil, item.StartAt); err != nil {
		t.Fatalf("rebook after no-show: %v", err)
	}
}

// wantActor fails unless the history row of eventType for appointment id names actor as the user
// who made the change (invariant 7: request-path changes are audited with the acting user).
func wantActor(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, eventType domain.EventType, actor uuid.UUID) {
	t.Helper()
	got := scalar[*uuid.UUID](t, pool, `
		SELECT actor_id FROM appointment_history WHERE appointment_id = $1 AND event_type = $2`, id, int16(eventType))
	if got == nil || *got != actor {
		t.Fatalf("%v history actor = %v, want %s", eventType, got, actor)
	}
}

func TestWaiterSoftUpgradesBrakeToExpress(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	day, loc := suiteToday()
	start := booking.Clock{Hour: 8}.On(day, loc)

	// Default policy: only jobs up to 60 min use the express lane, so a 90 min brake job stays
	// in the general bay even for a waiter (it would block three quick-lane oil changes).
	long, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleC, ServiceTypeID: seed.ServiceBrake,
		StartAt: start, IsWaiter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if long.ServiceBayType != domain.BayGeneral {
		t.Fatalf("90 min waiter job took bay type %d, want general", long.ServiceBayType)
	}
	if _, err := svc.Cancel(ctx, domain.SystemActor, long.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerRiver, store.DealershipPatch{ExpressMaxMinutes: new(int16(90))}); err != nil {
		t.Fatal(err)
	}

	// The general bay is free, so even an eligible waiter stays in it: the quick lane is kept for
	// the jobs that can only use it.
	general, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleC, ServiceTypeID: seed.ServiceBrake,
		StartAt: start, IsWaiter: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if general.ServiceBayID != seed.BayR2 {
		t.Fatalf("waiter brake with the general bay free took %s, want Bay B (general)", general.ServiceBayName)
	}
	if _, err := svc.Cancel(ctx, domain.SystemActor, general.ID, 0); err != nil {
		t.Fatal(err)
	}

	// No general bay left (every Riverside job that needs Bay B also needs Eve, so take it out of
	// service instead): the eligible waiter falls back to the express bay.
	mustExec(t, pool, `UPDATE service_bays SET active = false WHERE id = $1`, seed.BayR2)
	// VehicleC: seed has VehicleA at Northside 09:00, and a vehicle cannot be in two places.
	waiter, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleC,
		ServiceTypeID:  seed.ServiceBrake,
		StartAt:        start,
		IsWaiter:       true,
		IdempotencyKey: "test:waiter:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !waiter.IsWaiter {
		t.Fatal("expected isWaiter")
	}
	if waiter.ServiceBayType != domain.BayExpress || waiter.ServiceBayID != seed.BayR1 {
		t.Fatalf("waiter brake bay=%s type=%d want Bay A express", waiter.ServiceBayName, waiter.ServiceBayType)
	}
	if _, err := svc.Cancel(ctx, domain.SystemActor, waiter.ID, 0); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE service_bays SET active = true WHERE id = $1`, seed.BayR2)

	non, _, err := svc.Book(ctx, domain.SystemActor, booking.BookRequest{
		DealershipID:   seed.DealerRiver,
		VehicleID:      seed.VehicleB,
		ServiceTypeID:  seed.ServiceBrake,
		StartAt:        start,
		IsWaiter:       false,
		IdempotencyKey: "test:non-waiter:" + uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if non.IsWaiter {
		t.Fatal("expected non-waiter")
	}
	if non.ServiceBayType != domain.BayGeneral || non.ServiceBayID != seed.BayR2 {
		t.Fatalf("non-waiter brake bay=%s type=%d want Bay B general", non.ServiceBayName, non.ServiceBayType)
	}
}
