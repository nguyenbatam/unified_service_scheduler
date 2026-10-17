package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The technician ranking (store.freeTechniciansSQL) is tested on purpose-built shops whose names
// sort the wrong way: "Aaron" is always the technician the policy must NOT pick, so a "first free
// by name" (or by id) allocator fails every test here, and so does the former "least loaded"
// tie-break where it matters (TestTechnicianBestFitKeepsLongStretch).

type policyTech struct {
	name   string
	level  int16
	skills []domain.TechnicianSkill
	shift  *domain.TechnicianShift // every weekday; nil = the opening hours
}

// policyShop provisions a dealership open 08:00–18:00 every day (no lunch, no staging cap) with
// the bays and technicians given, and returns the technicians' ids by name.
func policyShop(ctx context.Context, t *testing.T, svc *booking.Service, bays []domain.BayType, techs []policyTech) (uuid.UUID, map[string]uuid.UUID) {
	t.Helper()
	d, err := svc.CreateDealership(ctx, staffAdmin, store.NewDealership{
		Name: "Policy " + uuid.NewString()[:8], Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "18:00", StagingWindowMinutes: 60,
		Policy: domain.Policy{CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 90, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, bt := range bays {
		if _, err := svc.CreateBay(ctx, staffAdmin, d.ID, "Bay "+string(rune('A'+i)), bt); err != nil {
			t.Fatal(err)
		}
	}
	ids := map[string]uuid.UUID{}
	for _, pt := range techs {
		tech, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{DealershipID: d.ID, Name: pt.name, SkillLevel: domain.SkillLevel(pt.level), Skills: pt.skills})
		if err != nil {
			t.Fatal(err)
		}
		ids[pt.name] = tech.ID
		if pt.shift != nil {
			week := make([]domain.TechnicianShift, 7)
			for i := range week {
				week[i] = *pt.shift
				week[i].Weekday = int16(i + 1)
			}
			if _, err := svc.ReplaceShifts(ctx, staffAdmin, tech.ID, week, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	return d.ID, ids
}

func policyBook(ctx context.Context, t *testing.T, svc *booking.Service, dealer, service uuid.UUID, start time.Time, waiter bool) (domain.Appointment, error) {
	t.Helper()
	appt, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: dealer, VehicleID: newVehicleCtx(ctx, t, svc), ServiceTypeID: service, StartAt: start, IsWaiter: waiter,
		IdempotencyKey: uuid.NewString(),
	})
	return appt, err
}

func wantTech(t *testing.T, label string, appt domain.Appointment, err error, ids map[string]uuid.UUID, want string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	if appt.TechnicianID != ids[want] {
		t.Fatalf("%s: technician %s, want %s", label, appt.TechnicianName, want)
	}
}

var (
	lube       = []domain.TechnicianSkill{domain.SkillLubrication}
	brakes     = []domain.TechnicianSkill{domain.SkillBrakes}
	lubeBrakes = []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes}
)

// Non-waiter: the lowest qualifying level, whatever the names say.
func TestNonWaiterTechnicianLowestLevelNotName(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	dealer, ids := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{
		{name: "Aaron", level: 3, skills: lube},
		{name: "Zoe", level: 1, skills: lube},
	})
	appt, err := policyBook(ctx, t, svc, dealer, seed.ServiceOil, londonOn(t, londonDay(t, 1), 10, 0), false)
	wantTech(t, "oil change", appt, err, ids, "Zoe")
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Equal level: the technician with no skill beyond the job's takes it, so the one who can also do
// brakes is still free for work needing both at the same start — an oil + brake job (two lines,
// one technician with both skills) — or a brake job (waiters get the same best fit). Best fit is
// judged by that outcome, not only the choice.
func TestTechnicianSpareSkillsKeptFree(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 10, 0)

	dealer, ids := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral}, []policyTech{
		{name: "Aaron", level: 2, skills: lubeBrakes},
		{name: "Zoe", level: 2, skills: lube},
	})
	appt, err := policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, false)
	wantTech(t, "non-waiter oil change", appt, err, ids, "Zoe")
	appt, _, err = svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: dealer, VehicleID: newVehicleCtx(ctx, t, svc), ServiceTypeIDs: []uuid.UUID{seed.ServiceOil, seed.ServiceBrake},
		StartAt: start, IdempotencyKey: uuid.NewString(),
	})
	wantTech(t, "oil + brake job at the same start (capacity kept)", appt, err, ids, "Aaron")

	// Waiter: the junior Zoe is the fit; Aaron is more senior and also holds brakes.
	dealer, ids = policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral}, []policyTech{
		{name: "Aaron", level: 3, skills: lubeBrakes},
		{name: "Zoe", level: 2, skills: lube},
	})
	appt, err = policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, true)
	wantTech(t, "waiter oil change", appt, err, ids, "Zoe")
	appt, err = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, start, false)
	wantTech(t, "brake job at the same start", appt, err, ids, "Aaron")
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Best fit on the technician's own day, as for bays: a brake job right after another goes to the
// same technician, which keeps the other's morning whole, so a brake job overlapping both still
// fits. Least loaded first (the former tie-break) would put the second job on Zoe and split both
// mornings: 08:00–09:30 on Aaron and 09:30–11:00 on Zoe leave no technician free for 08:30–10:00.
func TestTechnicianBestFitKeepsLongStretch(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, ids := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral, domain.BayGeneral}, []policyTech{
		{name: "Aaron", level: 2, skills: brakes},
		{name: "Zoe", level: 2, skills: brakes},
	})
	appt, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 8, 0), false)
	wantTech(t, "first brake job (tie: by name)", appt, err, ids, "Aaron")
	appt, err = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 9, 30), false)
	wantTech(t, "back-to-back brake job", appt, err, ids, "Aaron")
	appt, err = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 8, 30), false)
	wantTech(t, "brake job overlapping both (capacity kept)", appt, err, ids, "Zoe")
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// The technician's shift bounds the fit: 10:30–12:00 closes Zoe's 08:00–12:00 shift exactly,
// while on Aaron (on all day) it would cut 12:00–18:00 off his morning.
func TestTechnicianBestFitHonoursShiftEdges(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, ids := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral}, []policyTech{
		{name: "Aaron", level: 2, skills: brakes},
		{name: "Zoe", level: 2, skills: brakes, shift: &domain.TechnicianShift{StartTime: "08:00", EndTime: "12:00"}},
	})
	appt, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 10, 30), false)
	wantTech(t, "brake job at the end of Zoe's shift", appt, err, ids, "Zoe")
	appt, err = policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 13, 0), false)
	wantTech(t, "brake job after Zoe's shift", appt, err, ids, "Aaron")
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
