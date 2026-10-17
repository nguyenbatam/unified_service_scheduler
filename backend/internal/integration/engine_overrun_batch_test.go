package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// One job whose extension fails for a reason other than a lock timeout (here a trigger refusing its
// UPDATE) must not stop the overrun sweep: the next job of the batch is still extended, the counts
// cover it, and the failure comes back naming the job that failed.
func TestEngineOverrunSweepContinuesPastAFailingJob(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	failing, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // ends 09:30: first in the batch
	if err != nil {
		t.Fatal(err)
	}
	healthy, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 30)) // ends 10:00
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+` WHERE id = ANY ($1)`, []uuid.UUID{failing.ID, healthy.ID})
	mustExec(t, pool, `CREATE FUNCTION engine_refuse_update() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected failure'; END $$`)
	mustExec(t, pool, `CREATE TRIGGER engine_refuse_update BEFORE UPDATE ON appointments
		FOR EACH ROW WHEN (OLD.id = '`+failing.ID.String()+`') EXECUTE FUNCTION engine_refuse_update()`)

	extended, flagged, err := serviceOn(t, st, today, 10, 10).HandleOverruns(ctx, 15*time.Minute)
	if err == nil || !strings.Contains(err.Error(), failing.ID.String()) || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("sweep error = %v, want the injected failure of %s", err, failing.ID)
	}
	if extended != 1 || flagged != 0 {
		t.Fatalf("sweep = extended %d flagged %d, want 1/0 (the job after the failing one still extended)", extended, flagged)
	}
	if got := mustGet(t, svc, healthy.ID); !got.EndAt.Equal(londonOn(t, today, 10, 15)) {
		t.Fatalf("healthy job ends %s, want 10:15", got.EndAt)
	}
	if got := mustGet(t, svc, failing.ID); !got.EndAt.Equal(failing.EndAt) || got.OverrunAt != nil {
		t.Fatalf("failing job changed: end %s overrun %v", got.EndAt, got.OverrunAt)
	}
}
