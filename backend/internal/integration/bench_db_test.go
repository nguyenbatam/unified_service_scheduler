package integration_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Database-backed benchmarks of the two hot paths, through booking.Service on a real Postgres
// (no HTTP): go test -run '^$' -bench . ./internal/integration (needs TEST_DATABASE_URL). The
// end-to-end capacity of the running stack is measured by scripts/bench.sh.

// benchShop opens a dealership with bays bays and as many lubrication technicians, open 08–18.
func benchShop(b *testing.B, svc *booking.Service, bays int) domain.Dealership {
	b.Helper()
	ctx := b.Context()
	d, err := svc.CreateDealership(ctx, staffAdmin, store.NewDealership{
		Name: "Bench " + uuid.NewString()[:8], Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "18:00",
		StagingWindowMinutes: 60, Policy: domain.Policy{CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 365, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60},
	})
	if err != nil {
		b.Fatal(err)
	}
	for i := range bays {
		if _, err := svc.CreateBay(ctx, staffAdmin, d.ID, fmt.Sprintf("Lane %d", i), domain.BayExpress); err != nil {
			b.Fatal(err)
		}
		if _, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{
			DealershipID: d.ID, Name: fmt.Sprintf("Lube %d", i), SkillLevel: 1, Skills: []domain.TechnicianSkill{domain.SkillLubrication},
		}); err != nil {
			b.Fatal(err)
		}
	}
	return d
}

// Uncontended bookings in parallel: consecutive bookings go to different days, so they never wait
// on one another's dealership-day lock, and to different cars (31, a prime: the bookings of one
// day and start never share a car), so not on a vehicle lock either; each start holds 4.
func BenchmarkBookUncontended(b *testing.B) {
	_, _, svc, ctx := openTestDB(b)
	d := benchShop(b, svc, 4)
	vehicles := make([]uuid.UUID, 31)
	for i := range vehicles {
		vehicles[i] = newVehicle(b, svc)
	}
	first := londonDay(b, 1)
	var n atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := int(n.Add(1)) - 1
			const days = 300
			start := londonOn(b, first.AddDays(i%days), 8, 0).Add(time.Duration((i/days)%20) * 30 * time.Minute)
			if _, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
				DealershipID: d.ID, VehicleID: vehicles[i%len(vehicles)], ServiceTypeID: seed.ServiceOil,
				StartAt: start, IdempotencyKey: uuid.NewString(),
			}); err != nil {
				b.Fatalf("booking %d: %v", i, err)
			}
		}
	})
}

// Same-day bookings through an instance (booking.Service) per worker, as across replicas: no
// admission gate sees the others, so only the database orders them. Each day of the shop holds 80
// oil changes (20 starts, 4 lanes). In the SameSlot shape four consecutive bookings take the four
// places of one start, so the workers in flight race for the same lanes and technicians; in the
// Spread shape consecutive bookings take different starts of the day, so they only share its
// dealership-day lock. Every booking must win: the shop has exactly one place per booking.
func BenchmarkBookSameDayAcrossInstances(b *testing.B) {
	for _, shape := range []struct {
		name string
		slot func(i int) int // the start (0–19) of booking i within its day
	}{
		{"SameSlot", func(i int) int { return (i / 4) % 20 }},
		{"Spread", func(i int) int { return i % 20 }},
	} {
		b.Run(shape.name, func(b *testing.B) {
			_, st, svc, ctx := openTestDB(b)
			d := benchShop(b, svc, 4)
			vehicles := make([]uuid.UUID, 31) // prime: a car's bookings of one day never share a start
			for i := range vehicles {
				vehicles[i] = newVehicle(b, svc)
			}
			first := londonDay(b, 1)
			var n atomic.Int64
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				instance := atStartOfLondonDay(st)
				for pb.Next() {
					i := int(n.Add(1)) - 1
					start := londonOn(b, first.AddDays(i/80), 8, 0).Add(time.Duration(shape.slot(i%80)) * 30 * time.Minute)
					if _, _, err := instance.Book(ctx, staffAdmin, booking.BookRequest{
						DealershipID: d.ID, VehicleID: vehicles[i%len(vehicles)], ServiceTypeID: seed.ServiceOil,
						StartAt: start, IdempotencyKey: uuid.NewString(),
					}); err != nil {
						b.Errorf("booking %d: %v", i, err)
						return
					}
				}
			})
		})
	}
}

// One day's availability of a busy shop (half its slots sold).
func BenchmarkAvailabilityBusyDay(b *testing.B) {
	_, _, svc, ctx := openTestDB(b)
	d := benchShop(b, svc, 4)
	day := londonDay(b, 1)
	for i := range 40 {
		if _, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
			DealershipID: d.ID, VehicleID: newVehicle(b, svc), ServiceTypeID: seed.ServiceOil,
			StartAt: londonOn(b, day, 8, 0).Add(time.Duration(i%20) * 30 * time.Minute), IdempotencyKey: uuid.NewString(),
		}); err != nil {
			b.Fatal(err)
		}
	}
	vehicle := newVehicle(b, svc)
	b.ResetTimer()
	for b.Loop() {
		if _, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
			DealershipID: d.ID, VehicleID: vehicle, ServiceTypeID: seed.ServiceOil, Day: day,
		}); err != nil {
			b.Fatal(err)
		}
	}
}
