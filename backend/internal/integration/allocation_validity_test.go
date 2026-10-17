package integration_test

import (
	"strings"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// assertAllocationsValid must not be vacuous: on a valid board it finds nothing, and each rule it
// re-states catches a hold that breaks it (fixtures applied in a TX that is rolled back).
func TestAllocationValidityCheckCatchesEveryRule(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	oil := mustBook(t, svc, oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 10, 0)))
	brake := brakeAt(seed.DealerNorth, seed.VehicleB, londonAt(t, 14, 0))
	brake.IsWaiter = true
	mustBook(t, svc, brake)
	assertAllocationsValid(t, pool)

	day := londonDay(t, 0).String()
	cases := []struct {
		name, want string
		fixture    string
		args       []any
	}{
		{"technician level", "technician level", `UPDATE appointments SET min_skill_level = ` + code(domain.LevelMaster) + ` WHERE id = $1`, []any{oil.ID}},
		{"technician skill", "lacks a skill",
			`DELETE FROM technician_skills WHERE technician_id = (SELECT technician_id FROM appointments WHERE id = $1) AND skill = ` + code(domain.SkillLubrication), []any{oil.ID}},
		{"bay dealership", "bay of another dealership", `UPDATE appointments SET service_bay_id = $2 WHERE id = $1`, []any{oil.ID, seed.BayN4}},
		{"technician dealership", "technician of another dealership", `UPDATE appointments SET technician_id = $2 WHERE id = $1`, []any{oil.ID, seed.TechAlex}},
		{"bay type", "bay type", `UPDATE appointments SET required_bay_type = ` + code(domain.BayDiagnostic) + ` WHERE id = $1`, []any{oil.ID}},
		{"waiter upgrade too long", "bay type", `UPDATE appointments SET is_waiter = true, required_bay_type = ` + code(domain.BayGeneral) + `, booked_duration_minutes = 120 WHERE id = $1`, []any{oil.ID}},
		{"closure", "closed day", `INSERT INTO dealership_closures (dealership_id, day) VALUES ($1, $2::date)`, []any{seed.DealerRiver, day}},
		{"closed weekday", "closed day",
			`INSERT INTO dealership_hours (dealership_id, weekday, closed) VALUES ($1, extract(isodow FROM $2::date)::int, true)
			 ON CONFLICT (dealership_id, weekday) DO UPDATE SET closed = true, open_time = NULL, close_time = NULL, lunch_start = NULL, lunch_end = NULL`,
			[]any{seed.DealerRiver, day}},
		{"opening hours", "outside opening hours", `UPDATE dealerships SET open_time = '10:30', lunch_start = NULL, lunch_end = NULL WHERE id = $1`, []any{seed.DealerRiver}},
		{"lunch", "across lunch", `UPDATE dealerships SET lunch_start = '10:00', lunch_end = '10:30' WHERE id = $1`, []any{seed.DealerRiver}},
		{"absence", "off duty",
			`INSERT INTO technician_absences (id, technician_id, starts_at, ends_at)
			 SELECT gen_random_uuid(), technician_id, start_at, end_at FROM appointments WHERE id = $1`, []any{oil.ID}},
		{"shift elsewhere", "off duty",
			`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time)
			 SELECT technician_id, extract(isodow FROM $2::date)::int % 7 + 1, '08:00', '18:00' FROM appointments WHERE id = $1`, []any{oil.ID, day}},
		{"shift break", "off duty",
			`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time, break_start, break_end)
			 SELECT technician_id, extract(isodow FROM $2::date)::int, '08:00', '18:00', '10:00', '10:30' FROM appointments WHERE id = $1`, []any{oil.ID, day}},
	}
	// The schema refuses these outright (001_schema.sql): the case drops the constraint in its
	// rolled-back TX, so the check behind it is still exercised (defence in depth).
	guarded := map[string]string{
		"bay dealership":        "appointments_bay_dealership_fkey",
		"technician dealership": "appointments_technician_dealership_fkey",
	}
	for _, c := range cases {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if constraint, ok := guarded[c.name]; ok {
			if _, err := tx.Exec(ctx, `ALTER TABLE appointments DROP CONSTRAINT `+constraint); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("%s: drop %s: %v", c.name, constraint, err)
			}
		}
		if _, err := tx.Exec(ctx, c.fixture, c.args...); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: fixture: %v", c.name, err)
		}
		found := allocationViolations(t, tx)
		_ = tx.Rollback(ctx)
		if len(found) == 0 || !strings.Contains(strings.Join(found, "\n"), c.want) {
			t.Errorf("%s: violations %q, want one mentioning %q", c.name, found, c.want)
		}
	}
}
