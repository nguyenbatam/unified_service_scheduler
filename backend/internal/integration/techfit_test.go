package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Northside lubrication techs: Alex (level 2) and Casey (level 3). Tomorrow Alex already has one
// 3h job and Casey two 30-minute jobs, both in the morning: by booked minutes Casey is lighter.
// Waiter or not, an afternoon oil change goes to Alex: the lowest qualifying level comes before
// load (the senior Casey stays free for work only he can do). Waiters used to get the
// least-loaded technician; best fit booked more on random streams (store.freeTechniciansSQL).
func TestTechnicianChoiceIsBestFitForWaitersToo(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	insert := func(vehicle, bay, tech uuid.UUID, start time.Time, minutes int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id,
				service_bay_id, technician_id, start_at, end_at, status,
				booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
			SELECT $1, $2, v.customer_id, v.id, $3, $4, $5, $6, $7, 1, $9, 1, '{1}', 1 FROM vehicles v WHERE v.id = $8`,
			uuid.New(), seed.DealerNorth, seed.ServiceOil, bay, tech, start,
			start.Add(time.Duration(minutes)*time.Minute), vehicle, minutes); err != nil {
			t.Fatal(err)
		}
	}
	insert(seed.VehicleA, seed.BayN2, seed.TechAlex, londonOn(t, londonDay(t, 1), 8, 0), 180)
	insert(seed.VehicleB, seed.BayN1, seed.TechCasey, londonOn(t, londonDay(t, 1), 8, 0), 30)
	insert(seed.VehicleC, seed.BayN1, seed.TechCasey, londonOn(t, londonDay(t, 1), 8, 30), 30)

	book := func(vehicle uuid.UUID, hour int, waiter bool) domain.Appointment {
		t.Helper()
		item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
			DealershipID: seed.DealerNorth, VehicleID: vehicle, ServiceTypeID: seed.ServiceOil,
			StartAt: londonOn(t, londonDay(t, 1), hour, 0), IsWaiter: waiter, IdempotencyKey: uuid.NewString(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	if got := book(seed.VehicleB, 14, true); got.TechnicianID != seed.TechAlex {
		t.Fatalf("waiter tech=%s want Alex (lowest qualifying level, despite more booked minutes)", got.TechnicianName)
	}
	// Non-waiter: best fit — the level-2 tech is enough for an oil change, keep level 3 free.
	if got := book(seed.VehicleC, 15, false); got.TechnicianID != seed.TechAlex {
		t.Fatalf("non-waiter tech=%s want Alex (lowest qualifying level)", got.TechnicianName)
	}
}
