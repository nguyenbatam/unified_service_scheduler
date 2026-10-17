package integration_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// slotAt returns the grid slot starting at start, failing when the grid has none.
func slotAt(t *testing.T, avail domain.Availability, start time.Time) domain.Slot {
	t.Helper()
	for _, slot := range avail.Slots {
		if slot.Start.Equal(start) {
			return slot
		}
	}
	t.Fatalf("no slot at %s in %+v", start, avail.Slots)
	return domain.Slot{}
}

// The reschedule view must not be blocked by the appointment being moved: a 10:00–11:30 brake
// job at Riverside (one general bay) can slide to 10:30 only if its own hold is ignored.
func TestAvailabilityExcludesRescheduledAppointment(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day := londonDay(t, 1)
	brake, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake,
		StartAt: londonOn(t, day, 10, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	q := booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake, Day: day}
	plain, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	if slot := slotAt(t, plain, londonOn(t, day, 10, 30)); slot.Available || slot.Reason != booking.ReasonVehicleBusy {
		t.Fatalf("without exclusion 10:30 = %+v, want vehicle_busy", slot)
	}

	q.ExcludeAppointmentID = brake.ID
	moving, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	for _, start := range []time.Time{londonOn(t, day, 10, 0), londonOn(t, day, 10, 30)} {
		if slot := slotAt(t, moving, start); !slot.Available || slot.FreeBays != 1 {
			t.Fatalf("excluding its own hold %s = %+v, want available on the one general bay", start, slot)
		}
	}
	next, err := svc.NextAvailable(ctx, staffAdmin, booking.NextQuery{AvailabilityQuery: q, From: day, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, slot := range next.Slots {
		found = found || slot.Start.Equal(londonOn(t, day, 10, 30))
	}
	if !found {
		t.Fatalf("next-available with exclusion must offer 10:30: %+v", next.Slots)
	}
	if _, err := svc.Reschedule(ctx, staffAdmin, brake.ID, booking.RescheduleRequest{StartAt: londonOn(t, day, 10, 30), IfMatch: brake.Version}); err != nil {
		t.Fatalf("the slot the grid offered must be bookable: %v", err)
	}
}

// Staging arrivals count the moving appointment once: excluded from the grid like the booking TX.
func TestAvailabilityExcludeFreesItsStagingArrival(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day := londonDay(t, 1)
	// Northside: 2 arrivals per hour. VehicleA's oil at 14:00 and VehicleB's brake at 14:30 fill 14:00–15:00.
	oil, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerNorth, seed.VehicleA, londonOn(t, day, 14, 0)))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Book(ctx, staffAdmin, brakeAt(seed.DealerNorth, seed.VehicleB, londonOn(t, day, 14, 30))); err != nil {
		t.Fatal(err)
	}
	q := booking.AvailabilityQuery{DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: day}
	plain, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	if slot := slotAt(t, plain, londonOn(t, day, 14, 30)); slot.Available || slot.Reason != "staging_full" {
		t.Fatalf("without exclusion 14:30 = %+v, want staging_full", slot)
	}
	q.ExcludeAppointmentID = oil.ID
	moving, err := svc.Availability(ctx, staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	if slot := slotAt(t, moving, londonOn(t, day, 14, 30)); !slot.Available {
		t.Fatalf("excluding its own arrival 14:30 = %+v, want available", slot)
	}
	if _, err := svc.Reschedule(ctx, staffAdmin, oil.ID, booking.RescheduleRequest{StartAt: londonOn(t, day, 14, 30)}); err != nil {
		t.Fatalf("reschedule into the offered slot: %v", err)
	}
}

// Excluding an appointment is an appointment read: invisible ones are 404, and it must be the
// queried vehicle's appointment at the queried dealership.
func TestAvailabilityExcludeIsAuthorised(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	day := londonDay(t, 1)
	riverB, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleB, londonOn(t, day, 9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	riverA, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 11, 0)))
	if err != nil {
		t.Fatal(err)
	}
	northB, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerNorth, seed.VehicleB, londonOn(t, day, 11, 0)))
	if err != nil {
		t.Fatal(err)
	}
	query := func(dealer, vehicle, exclude uuid.UUID) booking.AvailabilityQuery {
		return booking.AvailabilityQuery{
			DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: seed.ServiceOil, Day: day, ExcludeAppointmentID: exclude,
		}
	}
	for _, tc := range []struct {
		name  string
		actor domain.Actor
		q     booking.AvailabilityQuery
		code  string
	}{
		{"another customer's appointment", customerAmelia, query(seed.DealerRiver, seed.VehicleA, riverB.ID), domain.CodeNotFound},
		{"other dealership's appointment for scoped staff", northDispatcher, query(seed.DealerNorth, seed.VehicleA, riverA.ID), domain.CodeNotFound},
		{"unknown appointment", staffAdmin, query(seed.DealerRiver, seed.VehicleA, uuid.New()), domain.CodeNotFound},
		{"another vehicle's appointment", staffAdmin, query(seed.DealerRiver, seed.VehicleA, riverB.ID), domain.CodeInvalid},
		{"another dealership's appointment", staffAdmin, query(seed.DealerRiver, seed.VehicleB, northB.ID), domain.CodeInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Availability(ctx, tc.actor, tc.q)
			expectCode(t, err, tc.code)
			_, err = svc.NextAvailable(ctx, tc.actor, booking.NextQuery{AvailabilityQuery: tc.q, From: day})
			expectCode(t, err, tc.code)
		})
	}
	if _, err := svc.Availability(ctx, customerAmelia, query(seed.DealerRiver, seed.VehicleA, riverA.ID)); err != nil {
		t.Fatalf("the owner may exclude their own appointment: %v", err)
	}
}

func TestHTTPAvailabilityExcludeParam(t *testing.T) {
	t.Parallel()
	st, svc, ctx := openTestStore(t)
	admin := login(t, newAPI(st, svc), seed.DemoAdminEmail, seed.DemoAdminPassword)
	day := londonDay(t, 1)
	appt, _, err := svc.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, seed.VehicleA, londonOn(t, day, 9, 0)))
	if err != nil {
		t.Fatal(err)
	}
	get := func(exclude string) int {
		params := url.Values{
			"dealershipId": {seed.DealerRiver.String()}, "vehicleId": {seed.VehicleA.String()},
			"serviceTypeId": {seed.ServiceOil.String()}, "date": {day.String()}, "excludeAppointmentId": {exclude},
		}
		return admin.do(http.MethodGet, "/api/v1/availability?"+params.Encode(), nil, nil).Code
	}
	if code := get(appt.ID.String()); code != http.StatusOK {
		t.Fatalf("valid exclusion status=%d", code)
	}
	if code := get("not-a-uuid"); code != http.StatusBadRequest {
		t.Fatalf("malformed exclusion status=%d, want 400", code)
	}
	if code := get(uuid.NewString()); code != http.StatusNotFound {
		t.Fatalf("unknown exclusion status=%d, want 404", code)
	}
}
