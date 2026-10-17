package integration_test

import (
	"fmt"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// The express spill (booking.BayFits): an oil change takes the quick lane first and a general bay
// only when the quick lane is taken, so oil changes are no longer turned away while a general bay
// stands idle; availability counts both.
func TestExpressSpillsToGeneralOnlyWhenTheQuickLaneIsTaken(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral},
		[]policyTech{{name: "Aaron", level: 1, skills: lube}, {name: "Bea", level: 1, skills: lube}, {name: "Cal", level: 1, skills: lube}})
	start := londonOn(t, day, 10, 0)
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: dealer, VehicleID: newVehicle(t, svc), ServiceTypeID: seed.ServiceOil, Day: day})
	if err != nil {
		t.Fatal(err)
	}
	if s := slotAt(t, avail, start); !s.Available || s.FreeBays != 2 {
		t.Fatalf("10:00 oil: available=%v freeBays=%d, want both bays", s.Available, s.FreeBays)
	}
	first, err := policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ServiceBayName != "Bay A" || second.ServiceBayName != "Bay B" {
		t.Fatalf("oil changes on %s then %s, want the quick lane first, then the general bay", first.ServiceBayName, second.ServiceBayName)
	}
	_, err = policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, false)
	expectCode(t, err, domain.CodeUnavailable)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Three oil changes race for a quick lane and one general bay: exactly two win, one per bay.
func TestExpressSpillRaceTwoWinners(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral},
		[]policyTech{{name: "Aaron", level: 1, skills: lube}, {name: "Bea", level: 1, skills: lube}, {name: "Cal", level: 1, skills: lube}})
	for offset := 1; offset <= 5; offset++ {
		start := londonOn(t, londonDay(t, offset), 9, 0)
		items := make([]domain.Appointment, 3)
		errs := make([]error, 3)
		raceN(3, func(i int) {
			items[i], errs[i] = policyBook(ctx, t, svc, dealer, seed.ServiceOil, start, false)
		})
		won := expectWinners(t, fmt.Sprintf("round %d", offset), 2, allLose(errs, refusals...)...)
		if items[won[0]].ServiceBayID == items[won[1]].ServiceBayID {
			t.Fatalf("round %d: both winners on %s", offset, items[won[0]].ServiceBayName)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
