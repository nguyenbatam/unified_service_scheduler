package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The calendar and roster are single objects (not paged lists): their dated entries are bounded
// instead. Past closures and ended absences do not count.
func TestCalendarAndRosterListsAreBounded(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	conflict := func(err error, what string) {
		t.Helper()
		if e, ok := domain.AsError(err); !ok || e.Code != domain.CodeConflict || !strings.Contains(e.Message, "at most") {
			t.Fatalf("%s over the bound: %v, want 409", what, err)
		}
	}

	exec(`INSERT INTO dealership_closures (dealership_id, day, reason)
		SELECT $1, date '2020-01-01' + n, 'past' FROM generate_series(0, 9) AS n`, seed.DealerRiver)
	exec(`INSERT INTO dealership_closures (dealership_id, day, reason)
		SELECT $1, date '2040-01-01' + n, 'future' FROM generate_series(0, $2::int - 1) AS n`, seed.DealerRiver, store.MaxUpcomingClosures)
	_, err := svc.AddClosure(ctx, staffAdmin, seed.DealerRiver, domain.Closure{Date: "2041-06-01", Reason: "one more"}, 0)
	conflict(err, "closures")
	exec(`DELETE FROM dealership_closures WHERE dealership_id = $1 AND day = date '2040-01-01'`, seed.DealerRiver)
	cal, err := svc.AddClosure(ctx, staffAdmin, seed.DealerRiver, domain.Closure{Date: "2041-06-01", Reason: "one more"}, 0)
	if err != nil || len(cal.Closures) != store.MaxUpcomingClosures {
		t.Fatalf("closure under the bound: %v, %d upcoming", err, len(cal.Closures))
	}

	base := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	exec(`INSERT INTO technician_absences (id, technician_id, starts_at, ends_at, reason)
		SELECT gen_random_uuid(), $1, $2::timestamptz + n * interval '2 hours', $2::timestamptz + n * interval '2 hours' + interval '1 hour', 'r'
		FROM generate_series(0, $3::int - 1) AS n`, seed.TechFinn, base, store.MaxUpcomingAbsences)
	exec(`INSERT INTO technician_absences (id, technician_id, starts_at, ends_at, reason)
		VALUES (gen_random_uuid(), $1, '2020-01-01T09:00:00Z', '2020-01-01T10:00:00Z', 'ended')`, seed.TechFinn)
	absence := domain.TechnicianAbsence{StartsAt: base.AddDate(1, 0, 0), EndsAt: base.AddDate(1, 0, 1), Reason: "one more"}
	_, err = svc.AddAbsence(ctx, staffAdmin, seed.TechFinn, absence, 0)
	conflict(err, "absences")
	exec(`DELETE FROM technician_absences WHERE technician_id = $1 AND starts_at = $2`, seed.TechFinn, base)
	roster, err := svc.AddAbsence(ctx, staffAdmin, seed.TechFinn, absence, 0)
	if err != nil || len(roster.Absences) != store.MaxUpcomingAbsences {
		t.Fatalf("absence under the bound: %v, %d upcoming", err, len(roster.Absences))
	}
}
