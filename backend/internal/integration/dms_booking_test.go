package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// One appointment, several operations: one technician holding every skill, one bay that fits
// all lines, summed labour time.
func TestMultiLineAppointment(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	lines := []uuid.UUID{seed.ServiceOil, seed.ServiceBrake}
	req := booking.BookRequest{
		DealershipID: seed.DealerNorth, VehicleID: seed.VehicleB, ServiceTypeIDs: lines,
		StartAt: londonOn(t, londonDay(t, 1), 10, 0), IdempotencyKey: "multi:" + uuid.NewString(),
	}
	item, created, err := svc.Book(ctx, staffAdmin, req)
	if err != nil || !created {
		t.Fatalf("book: %v created=%v", err, created)
	}
	if item.DurationMinutes != 120 || !item.EndAt.Equal(londonOn(t, londonDay(t, 1), 12, 0)) || len(item.Lines) != 2 ||
		item.Lines[0].ServiceTypeID != seed.ServiceOil || item.Lines[1].DurationMinutes != 90 ||
		item.ServiceName != "Oil Change + Brake Service" || item.ServiceTypeID != seed.ServiceOil {
		t.Fatalf("appointment = %+v", item)
	}
	// Northside: only Casey holds both lubrication and brakes; brakes need a general bay.
	if item.TechnicianID != seed.TechCasey || item.ServiceBayType != domain.BayGeneral || item.MinSkillLevel != 2 {
		t.Fatalf("allocated %s on %s (bay type %d, level %d)", item.TechnicianName, item.ServiceBayName, item.ServiceBayType, item.MinSkillLevel)
	}

	replay, created, err := svc.Book(ctx, staffAdmin, req)
	if err != nil || created || replay.ID != item.ID {
		t.Fatalf("replay: %v created=%v", err, created)
	}
	swapped := req
	swapped.ServiceTypeIDs = []uuid.UUID{seed.ServiceBrake, seed.ServiceOil}
	_, _, err = svc.Book(ctx, staffAdmin, swapped)
	expectCode(t, err, domain.CodeConflict)
	both := req
	both.ServiceTypeID = seed.ServiceOil
	_, _, err = svc.Book(ctx, staffAdmin, both)
	expectCode(t, err, domain.CodeInvalid)

	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: seed.DealerNorth, VehicleID: seed.VehicleC, ServiceTypeIDs: lines, Day: londonDay(t, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if avail.DurationMinutes != 120 {
		t.Fatalf("availability duration = %d", avail.DurationMinutes)
	}
	for _, slot := range avail.Slots {
		if slot.Start.Equal(londonOn(t, londonDay(t, 1), 10, 0)) && slot.Available {
			t.Fatal("Casey is busy at 10:00: the multi-line slot must not be offered")
		}
	}

	moved, err := svc.Reschedule(ctx, staffAdmin, item.ID, booking.RescheduleRequest{StartAt: londonOn(t, londonDay(t, 1), 13, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Lines) != 2 || moved.DurationMinutes != 120 || !moved.EndAt.Equal(londonOn(t, londonDay(t, 1), 15, 0)) {
		t.Fatalf("moved = %+v", moved)
	}
}

// Catalog changes apply to new bookings only: the appointment keeps its booked labour time.
func TestBookedSnapshotSurvivesCatalogChange(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	item, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerRiver, seed.VehicleA, londonOn(t, londonDay(t, 1), 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateServiceType(ctx, staffAdmin, seed.ServiceBrake, store.ServiceTypePatch{
		DurationMinutes: new(120), MinSkillLevel: new(domain.LevelSenior),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetAppointment(ctx, staffAdmin, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DurationMinutes != 90 || got.Lines[0].DurationMinutes != 90 || got.MinSkillLevel != 2 || got.EndAt.Sub(got.StartAt) != 90*time.Minute {
		t.Fatalf("booked snapshot changed: %+v", got)
	}
	// Moving it keeps the booked snapshot: the customer is not re-priced at the new catalog.
	moved, err := svc.Reschedule(ctx, staffAdmin, item.ID, booking.RescheduleRequest{StartAt: londonOn(t, londonDay(t, 1), 8, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if moved.DurationMinutes != 90 || moved.MinSkillLevel != 2 || moved.EndAt.Sub(moved.StartAt) != 90*time.Minute {
		t.Fatalf("reschedule re-priced the booking: %+v", moved)
	}
	fresh, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerRiver, seed.VehicleB, londonOn(t, londonDay(t, 1), 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.DurationMinutes != 120 || fresh.MinSkillLevel != 3 {
		t.Fatalf("new bookings use the new catalog: %+v", fresh)
	}
}

// Staging counts arrivals per configurable window (advisor write-up slots), not per clock hour.
func TestStagingWindowIsConfigurable(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{
		StagingCapacity: new(int16(1)), StagingWindowMinutes: new(int16(30)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerNorth, seed.VehicleA, londonOn(t, londonDay(t, 1), 15, 0))); err != nil {
		t.Fatal(err)
	}
	_, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleB, londonOn(t, londonDay(t, 1), 15, 0)))
	expectCode(t, err, domain.CodeUnavailable)
	// 15:30 is the next 30-minute window (an hourly window would still be full).
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerNorth, seed.VehicleC, londonOn(t, londonDay(t, 1), 15, 30))); err != nil {
		t.Fatalf("15:30 is a new arrival window: %v", err)
	}
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: seed.DealerNorth, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceBrake, Day: londonDay(t, 1),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range avail.Slots {
		if slot.Start.Equal(londonOn(t, londonDay(t, 1), 15, 0)) && slot.Reason != "staging_full" {
			t.Fatalf("15:00 reason = %q, want staging_full", slot.Reason)
		}
	}
}

func TestDealershipPolicyLeadTimeAndHorizon(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerRiver, store.DealershipPatch{
		BookingHorizonDays: new(int16(7)), CustomerLeadMinutes: new(int16(180)),
	}); err != nil {
		t.Fatal(err)
	}
	far := londonDay(t, 10)
	avail, err := svc.Availability(ctx, customerAmelia, booking.AvailabilityQuery{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: far,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, slot := range avail.Slots {
		if slot.Available || slot.Reason != booking.ReasonBeyondHorizon {
			t.Fatalf("slot %s: available=%v reason=%q, want beyond_horizon", slot.Start, slot.Available, slot.Reason)
		}
	}
	_, _, err = svc.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, far, 10, 0)))
	expectCode(t, err, domain.CodeUnavailable)
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, far, 10, 0))); err != nil {
		t.Fatalf("staff are not bound by the online horizon: %v", err)
	}

	at9 := serviceOn(t, st, londonDay(t, 0), 9, 0)
	_, _, err = at9.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 11, 0)))
	expectCode(t, err, domain.CodeUnavailable) // 2 h notice < this dealership's 3 h
	if _, _, err := at9.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 12, 0))); err != nil {
		t.Fatalf("3 h notice is enough: %v", err)
	}
}

func TestNextAvailableSearchesAcrossDays(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	q := booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil}
	tomorrow := londonDay(t, 1)
	if _, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonOn(t, londonDay(t, 1), 8, 0))); err != nil {
		t.Fatal(err)
	}
	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: tomorrow, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Slots) != 3 || !next.Slots[0].Start.Equal(londonOn(t, londonDay(t, 1), 8, 30)) || !next.Slots[2].Start.Equal(londonOn(t, londonDay(t, 1), 9, 30)) {
		t.Fatalf("next = %+v", next.Slots)
	}

	closed := londonDay(t, 2)
	if _, err := svc.AddClosure(ctx, staffAdmin, seed.DealerRiver, domain.Closure{Date: closed.String(), Reason: "Holiday"}, 0); err != nil {
		t.Fatal(err)
	}
	next, err = svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: closed, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Slots) != 1 || next.Slots[0].Date != londonDay(t, 3).String() || next.SearchedFrom != closed.String() {
		t.Fatalf("closed day must be skipped: %+v", next)
	}
}

// Each dealership sets its own no-show grace.
func TestNoShowGraceIsPerDealership(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{NoShowGraceMinutes: new(int16(120))}); err != nil {
		t.Fatal(err)
	}
	river, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 10, 0)))
	if err != nil {
		t.Fatal(err)
	}
	// 10:40: Riverside's 30 min grace has passed for 10:00; Northside's 09:00 seed oil has 2 h.
	n, err := serviceOn(t, st, londonDay(t, 0), 10, 40).MarkNoShows(ctx)
	if err != nil || n != 1 {
		t.Fatalf("marked %d (err %v), want only the Riverside booking", n, err)
	}
	got := mustGet(t, svc, river.ID)
	if got.Status != domain.StatusNoShow {
		t.Fatalf("riverside status = %s", got.Status.Name())
	}
}
