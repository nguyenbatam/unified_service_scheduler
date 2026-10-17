package integration_test

import (
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A restart re-runs the seed: it must not rewrite configuration an admin edited since (that would
// bypass the stranded-booking checks of invariant 16) nor re-add a removed skill.
func TestSeedRerunKeepsAdminEdits(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	edits := []string{
		`UPDATE technicians SET skill_level = ` + code(domain.LevelMaster) + ` WHERE id = '` + seed.TechDrew.String() + `'`,
		`DELETE FROM technician_skills WHERE technician_id = '` + seed.TechAlex.String() + `' AND skill = ` + code(domain.SkillGeneral),
		`UPDATE dealerships SET lunch_start = NULL, lunch_end = NULL, staging_capacity = 5 WHERE id = '` + seed.DealerNorth.String() + `'`,
		`UPDATE service_types SET min_skill_level = ` + code(domain.LevelTechnician) + ` WHERE id = '` + seed.ServiceOil.String() + `'`,
	}
	for _, sql := range edits {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if err := seed.Run(ctx, st, nil, seed.WithClock(startOfLondonDay)); err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name, sql string
		want      string
	}{
		{"skill level", `SELECT skill_level::text FROM technicians WHERE id = $1`, "4"},
		{"removed skill", `SELECT count(*)::text FROM technician_skills WHERE technician_id = $1 AND skill = ` + code(domain.SkillGeneral), "0"},
		{"lunch + staging", `SELECT COALESCE(lunch_start::text, 'none') || '/' || staging_capacity FROM dealerships WHERE id = $1`, "none/5"},
		{"service level", `SELECT min_skill_level::text FROM service_types WHERE id = $1`, "2"},
	}
	ids := []any{seed.TechDrew, seed.TechAlex, seed.DealerNorth, seed.ServiceOil}
	for i, c := range checks {
		if got := scalar[string](t, pool, c.sql, ids[i]); got != c.want {
			t.Errorf("%s after re-seed = %s, want %s", c.name, got, c.want)
		}
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM technician_skills`); n != 10 {
		t.Errorf("technician skills = %d, want the 11 seeded minus the removed one", n)
	}
}

// Started mid-day, the seed books its demo jobs on the next day instead of this morning (where the
// no-show sweep would flip them within seconds).
func TestSeedDemoBookingsAreInTheFuture(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	noon := londonAt(t, 12, 0)
	if err := seed.Run(ctx, st, nil, seed.WithClock(func() time.Time { return noon })); err != nil {
		t.Fatal(err)
	}
	tomorrow := londonDay(t, 1).String()
	n := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments
		WHERE idempotency_key LIKE 'seed:%' AND idempotency_key LIKE '%' || $1 AND start_at > $2 AND status = `+code(domain.StatusConfirmed),
		tomorrow, noon)
	if n != 3 {
		t.Fatalf("demo bookings for %s after a noon start = %d, want 3", tomorrow, n)
	}
	// Same day again: replayed (idempotency key per day), not duplicated.
	if err := seed.Run(ctx, st, nil, seed.WithClock(func() time.Time { return noon.Add(time.Hour) })); err != nil {
		t.Fatal(err)
	}
	if got := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE idempotency_key LIKE 'seed:%'`); got != 6 {
		t.Fatalf("seed bookings = %d, want 6 (today's fixtures + tomorrow's, no duplicates)", got)
	}
}
