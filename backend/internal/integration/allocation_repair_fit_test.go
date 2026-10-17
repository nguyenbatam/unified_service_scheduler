package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// insertOilHold writes a confirmed 30-minute oil change on bay and tech at start straight into the
// table (the allocator would place it by its own ranking; these tests need it exactly there).
func insertOilHold(t *testing.T, pool *pgxpool.Pool, dealer, vehicle, bay, tech uuid.UUID, start time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	mustExec(t, pool, `
		INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id,
			service_bay_id, technician_id, start_at, end_at, status,
			booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
		SELECT $1, $2, v.customer_id, v.id, $3, $4, $5, $6, $7, 1, 30, 1, '{1}', 1 FROM vehicles v WHERE v.id = $8`,
		id, dealer, seed.ServiceOil, bay, tech, start, start.Add(30*time.Minute), vehicle)
	return id
}

// The one-move repair puts the moved booking where it fits best, like allocation: on the target
// whose neighbouring bookings leave the smallest idle gap around its window, not the first by
// name. Here the oil change in the only general bay (or on the only brake technician) moves next
// to the 09:30 job on Lane B (Zoe), keeping Lane A's (Aaron's) day whole.
func TestRepairMovesOntoBestFitTarget(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	start := londonOn(t, londonDay(t, 1), 10, 0)
	t.Run("bay", func(t *testing.T) {
		d := provisionShop(t, svc, "Repair bay fit")
		general := addBay(t, svc, d.ID, "General", domain.BayGeneral)
		addBay(t, svc, d.ID, "Lane A", domain.BayExpress)
		laneB := addBay(t, svc, d.ID, "Lane B", domain.BayExpress)
		lube := addTechnician(t, svc, d.ID, "Lube", 1, domain.SkillLubrication)
		addTechnician(t, svc, d.ID, "Brakes", 2, domain.SkillBrakes)
		insertOilHold(t, pool, d.ID, newVehicle(t, svc), laneB, lube, start.Add(-30*time.Minute))
		spilled := insertOilHold(t, pool, d.ID, newVehicle(t, svc), general, lube, start)

		job := mustBook(t, svc, brakeAt(d.ID, newVehicle(t, svc), start))
		if job.ServiceBayID != general {
			t.Fatalf("brake job on %s, want the freed General bay", job.ServiceBayName)
		}
		if moved := mustGet(t, svc, spilled); moved.ServiceBayID != laneB {
			t.Fatalf("moved oil change on %s, want Lane B (next to its 09:30 job)", moved.ServiceBayName)
		}
	})
	t.Run("technician", func(t *testing.T) {
		d := provisionShop(t, svc, "Repair tech fit")
		addBay(t, svc, d.ID, "General", domain.BayGeneral)
		laneA := addBay(t, svc, d.ID, "Lane A", domain.BayExpress)
		laneB := addBay(t, svc, d.ID, "Lane B", domain.BayExpress)
		tom := addTechnician(t, svc, d.ID, "Tom", 2, domain.SkillLubrication, domain.SkillBrakes)
		addTechnician(t, svc, d.ID, "Aaron", 2, domain.SkillLubrication)
		zoe := addTechnician(t, svc, d.ID, "Zoe", 2, domain.SkillLubrication)
		insertOilHold(t, pool, d.ID, newVehicle(t, svc), laneB, zoe, start.Add(-30*time.Minute))
		held := insertOilHold(t, pool, d.ID, newVehicle(t, svc), laneA, tom, start)

		job := mustBook(t, svc, brakeAt(d.ID, newVehicle(t, svc), start))
		if job.TechnicianID != tom {
			t.Fatalf("brake job on %s, want Tom (the only brake technician, freed)", job.TechnicianName)
		}
		if moved := mustGet(t, svc, held); moved.TechnicianID != zoe {
			t.Fatalf("moved oil change on %s, want Zoe (next to her 09:30 job)", moved.TechnicianName)
		}
	})
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
