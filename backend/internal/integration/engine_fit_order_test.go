package integration_test

import (
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// placeOn books service at start and reassigns it to bay and technician (uuid.Nil keeps the
// engine's choice), so a test can lay out a board the engine would not have chosen itself.
func placeOn(t *testing.T, svc *booking.Service, dealer, service uuid.UUID, start time.Time, bay, tech uuid.UUID) domain.Appointment {
	t.Helper()
	appt, err := bookAt(t, svc, dealer, newVehicle(t, svc), service, start)
	if err != nil {
		t.Fatalf("book %s: %v", start, err)
	}
	req := booking.ReassignRequest{}
	if bay != uuid.Nil && bay != appt.ServiceBayID {
		req.ServiceBayID = bay
	}
	if tech != uuid.Nil && tech != appt.TechnicianID {
		req.TechnicianID = tech
	}
	if req == (booking.ReassignRequest{}) {
		return appt
	}
	moved, err := svc.Reassign(t.Context(), staffAdmin, appt.ID, req)
	if err != nil {
		t.Fatalf("reassign %s to bay %s / tech %s: %v", start, bay, tech, err)
	}
	return moved
}

// The review's example, end to end: a 60-minute general job at 09:30 with lunch at 12:00, G1 holding
// 08:00–09:00 and 11:00–12:00, G2 holding 08:00–09:30. The only technician for the job works
// 09:30–12:00 (their side is flush at 09:30 and 11:00). The engine books G1 at 09:30 (30 + 30 idle
// beats 0 + 90) and leaves two 30-minute holes, so availability must not recommend 09:30 even
// though G2 butts against its hold there (the old hint did); 11:00 (G2 up to lunch) is. With
// PreferRecommended and a share of two, NextAvailable takes 11:00 and the earliest other start, in
// time order.
func TestEngineRecommendedHintIsTheEnginesPick(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	job := uuid.New()
	mustExec(t, pool, `INSERT INTO service_types (id, code, name, duration_minutes, required_skill, required_bay_type, min_skill_level)
		VALUES ($1, 'ENGINE60', 'Engine test hour', 60, 2, 2, 2)`, job)
	d := provisionShop(t, svc, "Hint")
	if _, err := svc.UpdateDealership(ctx, staffAdmin, d.ID, store.DealershipPatch{LunchStart: new("12:00"), LunchEnd: new("13:00")}); err != nil {
		t.Fatal(err)
	}
	g1, g2 := addBay(t, svc, d.ID, "G1", domain.BayGeneral), addBay(t, svc, d.ID, "G2", domain.BayGeneral)
	lube1 := addTechnician(t, svc, d.ID, "Lube 1", 1, domain.SkillLubrication)
	lube2 := addTechnician(t, svc, d.ID, "Lube 2", 1, domain.SkillLubrication)
	brakes := addTechnician(t, svc, d.ID, "Brakes", 2, domain.SkillBrakes)
	week := make([]domain.TechnicianShift, 7)
	for i := range week {
		week[i] = domain.TechnicianShift{Weekday: int16(i + 1), StartTime: "09:30", EndTime: "12:00"}
	}
	if _, err := svc.ReplaceShifts(ctx, staffAdmin, brakes, week, 0); err != nil {
		t.Fatal(err)
	}
	day := londonDay(t, 1)
	at := func(h, m int) time.Time { return londonOn(t, day, h, m) }
	for _, p := range []struct {
		h, m      int
		bay, tech uuid.UUID
	}{
		{8, 0, g1, lube1}, {8, 0, g2, lube2}, {8, 30, g1, lube1}, {8, 30, g2, lube2}, {9, 0, g2, lube2},
		{11, 0, g1, lube1}, {11, 30, g1, lube1},
	} {
		placeOn(t, svc, d.ID, seed.ServiceOil, at(p.h, p.m), p.bay, p.tech)
	}

	q := booking.AvailabilityQuery{DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: job, Day: day}
	avail, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	var offered, recommended []string
	for _, s := range avail.Slots {
		if s.Available {
			offered = append(offered, s.Start.In(time.UTC).Format(time.RFC3339))
		}
		if s.Recommended {
			recommended = append(recommended, s.Start.In(time.UTC).Format(time.RFC3339))
		}
	}
	iso := func(h, m int) string { return at(h, m).In(time.UTC).Format(time.RFC3339) }
	if want := []string{iso(9, 30), iso(10, 0), iso(10, 30), iso(11, 0)}; !slices.Equal(offered, want) {
		t.Fatalf("offered %v, want %v", offered, want)
	}
	if want := []string{iso(11, 0)}; !slices.Equal(recommended, want) {
		t.Fatalf("recommended %v, want %v (09:30 lands on G1 between two 30-minute holes)", recommended, want)
	}

	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: day, Days: 1, Limit: 3, PerDay: 2, PreferRecommended: true})
	if err != nil {
		t.Fatal(err)
	}
	var starts []string
	for _, s := range next.Slots {
		starts = append(starts, s.Start.In(time.UTC).Format(time.RFC3339))
	}
	if want := []string{iso(9, 30), iso(11, 0)}; !slices.Equal(starts, want) {
		t.Fatalf("next available preferring recommended = %v, want %v", starts, want)
	}

	appt, err := bookAt(t, svc, d.ID, q.VehicleID, job, at(9, 30))
	if err != nil {
		t.Fatal(err)
	}
	if appt.ServiceBayID != g1 {
		t.Fatalf("09:30 booked on %s, want G1 (the bay the hint judged)", appt.ServiceBayName)
	}
}

// A technician repair frees the less versatile technician for the new job, as allocation would
// pick: Aaron (brakes, lubrication, general) and Zoe (brakes, lubrication) each hold an oil change
// at 09:00 that Lube (level 1) could take; a brake job at 09:00 must move Zoe's (one spare skill for
// it, Aaron has two), although Aaron's name sorts first (the old order chose by name).
func TestEngineTechRepairFreesTheLessVersatileTechnician(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	d := provisionShop(t, svc, "Versatile")
	addBay(t, svc, d.ID, "EX", domain.BayExpress)
	for _, name := range []string{"G1", "G2", "G3"} {
		addBay(t, svc, d.ID, name, domain.BayGeneral)
	}
	aaron := addTechnician(t, svc, d.ID, "Aaron", 2, domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral)
	zoe := addTechnician(t, svc, d.ID, "Zoe", 2, domain.SkillLubrication, domain.SkillBrakes)
	lube := addTechnician(t, svc, d.ID, "Lube", 1, domain.SkillLubrication)
	day := londonDay(t, 1)
	nine := londonOn(t, day, 9, 0)
	onAaron := placeOn(t, svc, d.ID, seed.ServiceOil, nine, uuid.Nil, aaron)
	onZoe := placeOn(t, svc, d.ID, seed.ServiceOil, nine, uuid.Nil, zoe)

	appt, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceBrake, nine)
	if err != nil {
		t.Fatal(err)
	}
	if appt.TechnicianID != zoe {
		t.Fatalf("brake job got %s, want Zoe (the freed technician with the fewest spare skills)", appt.TechnicianName)
	}
	if got := mustGet(t, svc, onZoe.ID); got.TechnicianID != lube {
		t.Fatalf("Zoe's oil change went to %s, want Lube", got.TechnicianName)
	}
	if got := mustGet(t, svc, onAaron.ID); got.TechnicianID != aaron || got.Version != onAaron.Version {
		t.Fatalf("Aaron's oil change moved (%s, version %d → %d)", got.TechnicianName, onAaron.Version, got.Version)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// A bay repair frees the bay the new job fits best, not the first by name: GA and GB each hold an
// oil change at 10:00 that the empty quick lane could take; GB also holds brake jobs ending at
// 10:00 and starting at 11:30, so a brake job at 10:00–11:30 fills GB exactly, while on GA it would
// leave 2 h before and 6.5 h after. The move must free GB.
func TestEngineBayRepairChoosesByFit(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	d := provisionShop(t, svc, "Fit")
	ex := addBay(t, svc, d.ID, "EX", domain.BayExpress)
	ga, gb := addBay(t, svc, d.ID, "GA", domain.BayGeneral), addBay(t, svc, d.ID, "GB", domain.BayGeneral)
	addTechnician(t, svc, d.ID, "Lube 1", 1, domain.SkillLubrication)
	addTechnician(t, svc, d.ID, "Lube 2", 1, domain.SkillLubrication)
	addTechnician(t, svc, d.ID, "Brakes 1", 2, domain.SkillBrakes)
	addTechnician(t, svc, d.ID, "Brakes 2", 2, domain.SkillBrakes)
	day := londonDay(t, 1)
	at := func(h, m int) time.Time { return londonOn(t, day, h, m) }
	placeOn(t, svc, d.ID, seed.ServiceBrake, at(8, 30), gb, uuid.Nil)
	placeOn(t, svc, d.ID, seed.ServiceBrake, at(11, 30), gb, uuid.Nil)
	onGB := placeOn(t, svc, d.ID, seed.ServiceOil, at(10, 0), gb, uuid.Nil)
	onGA := placeOn(t, svc, d.ID, seed.ServiceOil, at(10, 0), ga, uuid.Nil)

	appt, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceBrake, at(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	if appt.ServiceBayID != gb {
		t.Fatalf("brake job got %s, want GB (it fits there exactly)", appt.ServiceBayName)
	}
	if got := mustGet(t, svc, onGB.ID); got.ServiceBayID != ex {
		t.Fatalf("GB's oil change went to %s, want EX", got.ServiceBayName)
	}
	if got := mustGet(t, svc, onGA.ID); got.ServiceBayID != ga || got.Version != onGA.Version {
		t.Fatalf("GA's oil change moved (%s, version %d → %d)", got.ServiceBayName, onGA.Version, got.Version)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// NextAvailable reads its first days, and only the rest when they do not fill the limit: with the
// first three days closed, the answer comes from the second read and equals day 4's availability.
func TestEngineNextAvailableBeyondTheFirstRead(t *testing.T) {
	t.Parallel()
	_, _, svc, ctx := openTestDB(t)
	d := provisionShop(t, svc, "Chunks")
	addBay(t, svc, d.ID, "Lane", domain.BayExpress)
	addTechnician(t, svc, d.ID, "Lube", 1, domain.SkillLubrication)
	for i := 1; i <= 3; i++ {
		if _, err := svc.AddClosure(ctx, staffAdmin, d.ID, domain.Closure{Date: londonDay(t, i).String(), Reason: "Closed"}, 0); err != nil {
			t.Fatal(err)
		}
	}
	q := booking.AvailabilityQuery{DealershipID: d.ID, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceOil}
	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: londonDay(t, 1), Days: 7, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	fourth := londonDay(t, 4)
	q.Day = fourth
	avail, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Slots) != 4 || next.SearchedTo != fourth.String() {
		t.Fatalf("next = %d slots searched to %s, want 4 on %s", len(next.Slots), next.SearchedTo, fourth)
	}
	for i, s := range next.Slots {
		a := avail.Slots[i]
		if s.Date != fourth.String() || !s.Start.Equal(a.Start) || s.FreeBays != a.FreeBays || s.Recommended != a.Recommended {
			t.Fatalf("slot %d = %+v, want day 4's %+v", i, s, a)
		}
	}
}

// GET /availability/next?prefer=recommended spends a day's perDay share on its recommended starts,
// listed in time order; any other prefer value is a 400.
func TestEngineHTTPNextAvailablePrefer(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	handler := newAPI(st, svc, httpapi.WithRateLimiter(newTestLimiter()))
	dispatcher := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	base := "/api/v1/availability/next?dealershipId=" + seed.DealerNorth.String() + "&vehicleId=" + seed.VehicleC.String() +
		"&serviceTypeId=" + seed.ServiceOil.String() + "&from=" + londonDay(t, 1).String() + "&days=1&limit=20"
	var plain, preferred domain.NextAvailability
	dispatcher.expect(dispatcher.do(http.MethodGet, base, nil, nil), http.StatusOK, &plain)
	var want []domain.NextSlot
	for _, s := range plain.Slots {
		if s.Recommended {
			want = append(want, s)
		}
	}
	if len(want) == 0 || len(want) == len(plain.Slots) {
		t.Fatalf("want some but not all starts recommended: %+v", plain.Slots)
	}
	perDay := "&perDay=" + strconv.Itoa(len(want))
	dispatcher.expect(dispatcher.do(http.MethodGet, base+perDay+"&prefer=recommended", nil, nil), http.StatusOK, &preferred)
	if !reflect.DeepEqual(preferred.Slots, want) {
		t.Fatalf("prefer=recommended%s = %+v, want the recommended starts in time order: %+v", perDay, preferred.Slots, want)
	}
	// Without prefer, the same share goes to the earliest starts.
	dispatcher.expect(dispatcher.do(http.MethodGet, base+perDay, nil, nil), http.StatusOK, &preferred)
	if !reflect.DeepEqual(preferred.Slots, plain.Slots[:len(want)]) {
		t.Fatalf("perDay%s = %+v, want the earliest %d", perDay, preferred.Slots, len(want))
	}
	dispatcher.expect(dispatcher.do(http.MethodGet, base+"&prefer=soonest", nil, nil), http.StatusBadRequest, nil)
}
