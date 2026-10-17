package integration_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A 90 min brake job at 09:00 needs its technician and its bay until 10:30. A hold starting at
// 10:00 — free at the start, busy only in the second half of the window — must refuse it (422),
// and availability must show 09:00 unavailable, while 08:30 (ending exactly at 10:00) still fits.
// Riverside's only brake technician is Eve and its only general bay is Bay B: each case leaves
// exactly one of them busy from 10:00.
func TestBookingRefusedWhenResourceBusyOnlyInSecondHalf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		hold func(t *testing.T, svc *booking.Service, day booking.Date)
	}{
		{"only brake technician", func(t *testing.T, svc *booking.Service, day booking.Date) {
			// An oil change at 10:00 takes Eve and the express bay; Bay B stays free.
			job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, day, 10, 0))
			if err != nil {
				t.Fatal(err)
			}
			if job.TechnicianID != seed.TechEve || job.ServiceBayID != seed.BayR1 {
				t.Fatalf("10:00 oil on %s / %s, want Eve on Bay A", job.TechnicianName, job.ServiceBayName)
			}
		}},
		{"only general bay", func(t *testing.T, svc *booking.Service, day booking.Date) {
			// A second brake technician, so a 10:00 brake job leaves one brake technician free and
			// only Bay B busy.
			addTechnician(t, svc, seed.DealerRiver, "Gus Brakes", 2, domain.SkillBrakes)
			job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, day, 10, 0))
			if err != nil {
				t.Fatal(err)
			}
			if job.ServiceBayID != seed.BayR2 {
				t.Fatalf("10:00 brake on %s, want Bay B", job.ServiceBayName)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, svc, ctx := openTestDB(t)
			day := londonDay(t, 1)
			tc.hold(t, svc, day)
			vehicle := newVehicle(t, svc)

			avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
				DealershipID: seed.DealerRiver, VehicleID: vehicle, ServiceTypeID: seed.ServiceBrake, Day: day,
			})
			if err != nil {
				t.Fatal(err)
			}
			if slot := slotAt(t, avail, londonOn(t, day, 9, 0)); slot.Available {
				t.Fatalf("09:00 brake reported available (%+v), but its window runs into the 10:00 hold", slot)
			}
			if slot := slotAt(t, avail, londonOn(t, day, 8, 30)); !slot.Available {
				t.Fatalf("08:30 brake reported unavailable (%+v), but it ends as the 10:00 hold starts", slot)
			}

			_, err = bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceBrake, londonOn(t, day, 9, 0))
			expectCode(t, err, domain.CodeUnavailable)
			if _, err := bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceBrake, londonOn(t, day, 8, 30)); err != nil {
				t.Fatalf("08:30–10:00 brake, back to back with the hold: %v", err)
			}
		})
	}
}

// The express lane is a fallback for waiters only, and only for jobs up to the dealership's
// express_max_minutes. With Riverside's general bay out of play (inactive, or held by another job
// while Eve stays free) and its express bay free: a non-waiter's brake job is refused rather than
// put in the express bay; a waiter's 90 min brake job is refused too while the limit is 60 min; once
// an admin raises the limit to 90 the waiter's job goes to the express bay, and a non-waiter's is
// still refused.
func TestExpressLaneOnlyForShortWaiterJobs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		disable func(t *testing.T, pool *pgxpool.Pool, svc *booking.Service, day booking.Date)
	}{
		{"general bay inactive", func(t *testing.T, pool *pgxpool.Pool, _ *booking.Service, _ booking.Date) {
			mustExec(t, pool, `UPDATE service_bays SET active = false WHERE id = $1`, seed.BayR2)
		}},
		{"general bay held", func(t *testing.T, pool *pgxpool.Pool, svc *booking.Service, day booking.Date) {
			// Held 08:00–09:30 by a raw row on Finn, so Eve (the only brake technician) stays free.
			holdRiversideBayB(t, pool, newVehicle(t, svc), day)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, _, svc, ctx := openTestDB(t)
			day := londonDay(t, 1)
			tc.disable(t, pool, svc, day)
			brake := func(waiter bool) (domain.Appointment, error) {
				item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
					DealershipID: seed.DealerRiver, VehicleID: newVehicleCtx(ctx, t, svc), ServiceTypeID: seed.ServiceBrake,
					StartAt: londonOn(t, day, 8, 0), IsWaiter: waiter, IdempotencyKey: uuid.NewString(),
				})
				return item, err
			}

			_, err := brake(false)
			expectCode(t, err, domain.CodeUnavailable)
			_, err = brake(true)
			expectCode(t, err, domain.CodeUnavailable)

			if _, err := svc.UpdateDealership(ctx, staffAdmin, seed.DealerRiver, store.DealershipPatch{ExpressMaxMinutes: new(int16(90))}); err != nil {
				t.Fatal(err)
			}
			_, err = brake(false)
			expectCode(t, err, domain.CodeUnavailable)
			waiter, err := brake(true)
			if err != nil {
				t.Fatalf("waiter brake with a 90 min express limit: %v", err)
			}
			if waiter.ServiceBayID != seed.BayR1 || waiter.ServiceBayType != domain.BayExpress || !waiter.IsWaiter {
				t.Fatalf("waiter brake on %s (type %d, waiter %v), want Bay A express", waiter.ServiceBayName, waiter.ServiceBayType, waiter.IsWaiter)
			}
		})
	}
}

// holdRiversideBayB inserts a confirmed hold of Riverside's general bay on day 08:00–09:30 for
// vehicle on Finn, straight into the table: Finn has no brake skill, so Eve stays free and only the
// bay is taken (the GiST constraints still apply).
func holdRiversideBayB(t *testing.T, pool *pgxpool.Pool, vehicle uuid.UUID, day booking.Date) {
	t.Helper()
	mustExec(t, pool, `
		INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id,
			service_bay_id, technician_id, start_at, end_at, status,
			booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
		SELECT $1, $2, v.customer_id, v.id, $3, $4, $5, $6, $7, 1, 90, 2, '{3}', 1 FROM vehicles v WHERE v.id = $8`,
		uuid.New(), seed.DealerRiver, seed.ServiceMajor, seed.BayR2, seed.TechFinn,
		londonOn(t, day, 8, 0), londonOn(t, day, 9, 30), vehicle)
}

// Bays are ranked best fit: a job goes where it leaves the smallest idle gap, so a long job still
// finds a free run later. Two general bays, Bay A and Bay B (first fit by name would pick Bay A),
// and technicians to spare. Bay B holds 09:30–11:00. A brake job at 08:00–09:30 fits Bay B exactly
// (adjacent, no gap) and must go there, which leaves Bay A free all morning for a 3-hour major at
// 08:00. First fit by name would put the brake job on Bay A and then refuse the major (Bay A busy
// until 09:30, Bay B from 09:30): capacity lost to a bad choice, not to demand.
func TestBestFitBayPreservesLongJobCapacity(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	d := provisionShop(t, svc, "Best fit")
	bayA := addBay(t, svc, d.ID, "Bay A", domain.BayGeneral)
	bayB := addBay(t, svc, d.ID, "Bay B", domain.BayGeneral)
	for _, name := range []string{"Tech 1", "Tech 2", "Tech 3"} {
		addTechnician(t, svc, d.ID, name, 3, domain.SkillBrakes, domain.SkillGeneral)
	}
	day := londonDay(t, 1)

	// Put the 09:30 job on Bay B: with Bay A out of service for the moment, it is the only choice.
	mustExec(t, pool, `UPDATE service_bays SET active = false WHERE id = $1`, bayA)
	late, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, day, 9, 30))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE service_bays SET active = true WHERE id = $1`, bayA)
	if late.ServiceBayID != bayB {
		t.Fatalf("09:30 brake on %s, want Bay B (Bay A was inactive)", late.ServiceBayName)
	}

	early, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, day, 8, 0))
	if err != nil {
		t.Fatal(err)
	}
	if early.ServiceBayID != bayB {
		t.Fatalf("08:00–09:30 brake on %s, want Bay B (adjacent to its 09:30 job, no idle gap)", early.ServiceBayName)
	}
	major, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceMajor, londonOn(t, day, 8, 0))
	if err != nil {
		t.Fatalf("08:00–11:00 major after the brake jobs: %v (best fit must have kept Bay A free)", err)
	}
	if major.ServiceBayID != bayA {
		t.Fatalf("major on %s, want Bay A", major.ServiceBayName)
	}

	// The other side of the gap: on the next day a 09:30 job follows an 08:00–09:30 one on Bay B
	// back to back, where name order would have picked Bay A and left 08:00–09:30 idle on B.
	next := londonDay(t, 2)
	mustExec(t, pool, `UPDATE service_bays SET active = false WHERE id = $1`, bayA)
	first, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, next, 8, 0))
	mustExec(t, pool, `UPDATE service_bays SET active = true WHERE id = $1`, bayA)
	if err != nil || first.ServiceBayID != bayB {
		t.Fatalf("08:00 brake on %s (err %v), want Bay B (Bay A was inactive)", first.ServiceBayName, err)
	}
	after, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceBrake, londonOn(t, next, 9, 30))
	if err != nil || after.ServiceBayID != bayB {
		t.Fatalf("09:30 brake on %s (err %v), want Bay B (back to back after its 08:00 job)", after.ServiceBayName, err)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
