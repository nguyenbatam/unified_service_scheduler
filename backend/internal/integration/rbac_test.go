package integration_test

import (
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func TestCustomerSeesAndActsOnlyOnOwnData(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day, _ := suiteToday()

	vehicles, _, err := svc.ListVehicles(ctx, customerAmelia, booking.VehicleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(vehicles) != 1 || vehicles[0].ID != seed.VehicleA {
		t.Fatalf("customer vehicles = %+v", vehicles)
	}
	all, _, err := svc.ListVehicles(ctx, staffDispatcher, booking.VehicleQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 3 {
		t.Fatalf("staff must see every vehicle, got %d", len(all))
	}

	_, err = svc.Availability(ctx, customerAmelia, booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleB, ServiceTypeID: seed.ServiceOil, Day: day, IsWaiter: false})
	expectCode(t, err, domain.CodeNotFound)
	_, _, err = svc.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleB, londonAt(t, 14, 0)))
	expectCode(t, err, domain.CodeNotFound)

	own, created, err := svc.Book(ctx, customerAmelia, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)))
	if err != nil || !created {
		t.Fatal(err)
	}
	creator := scalar[string](t, pool, `SELECT created_by::text FROM appointments WHERE id = $1`, own.ID)
	if creator != seed.DemoCustomerID.String() {
		t.Fatalf("created_by = %s", creator)
	}

	schedule, err := svc.Schedule(ctx, staffDispatcher, seed.DealerNorth, day)
	if err != nil {
		t.Fatal(err)
	}
	var someoneElse domain.Appointment
	for _, a := range schedule.Appointments {
		if a.CustomerID != seed.CustomerA {
			someoneElse = a
			break
		}
	}
	if someoneElse.ID == [16]byte{} {
		t.Fatal("seed should have another customer's booking")
	}
	_, err = svc.GetAppointment(ctx, customerAmelia, someoneElse.ID)
	expectCode(t, err, domain.CodeNotFound)
	_, err = svc.Cancel(ctx, customerAmelia, someoneElse.ID, 0)
	expectCode(t, err, domain.CodeNotFound)
	_, err = svc.Reschedule(ctx, customerAmelia, someoneElse.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0)})
	expectCode(t, err, domain.CodeNotFound)

	_, err = svc.Schedule(ctx, customerAmelia, seed.DealerNorth, day)
	expectCode(t, err, domain.CodeForbidden)
	_, err = svc.CheckIn(ctx, customerAmelia, own.ID, 0)
	expectCode(t, err, domain.CodeForbidden)

	mine, _, err := svc.ListAppointmentsPage(ctx, customerAmelia, booking.AppointmentQuery{CustomerID: seed.CustomerB})
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) == 0 {
		t.Fatal("expected own appointments")
	}
	for _, a := range mine {
		if a.CustomerID != seed.CustomerA {
			t.Fatalf("customer filter override leaked %s", a.CustomerName)
		}
	}

	if _, err := svc.Reschedule(ctx, customerAmelia, own.ID, booking.RescheduleRequest{StartAt: londonAt(t, 15, 0)}); err != nil {
		t.Fatalf("customer reschedules own booking: %v", err)
	}
}

func TestCreateVehicleOwnershipAndValidation(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)

	// A customer cannot register a car for someone else: owner is forced to themselves.
	v, err := svc.CreateVehicle(ctx, customerAmelia, booking.VehicleInput{
		CustomerID: seed.CustomerB, VIN: "vf1aaaaa555555555", Make: "Renault", Model: "Clio", Year: 2020,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.CustomerID != seed.CustomerA || v.VIN != "VF1AAAAA555555555" {
		t.Fatalf("vehicle = %+v", v)
	}
	_, err = svc.CreateVehicle(ctx, customerAmelia, booking.VehicleInput{VIN: "VF1AAAAA555555555", Make: "R", Model: "C", Year: 2020})
	expectCode(t, err, domain.CodeConflict)
	_, err = svc.CreateVehicle(ctx, customerAmelia, booking.VehicleInput{VIN: "IOQ", Make: "R", Model: "C", Year: 2020})
	expectCode(t, err, domain.CodeInvalid)
	_, err = svc.CreateVehicle(ctx, staffAdmin, booking.VehicleInput{VIN: "VF1AAAAA555555556", Make: "R", Model: "C", Year: 2020})
	expectCode(t, err, domain.CodeInvalid) // staff must name the customer
}
