package integration_test

import (
	"strings"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A flagged overrun holds its bay and technician until it is completed (rule 11). Allocation and
// availability already honour it; reassign and reinstate pick their bay and technician themselves,
// so they check it too (GiST only knows the overrun's end_at): 409, never a job put on the car still
// on the lift. Completing the overrun gives the bay back.
func TestReassignRefusesAnOverrunHeldTarget(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral},
		[]policyTech{{name: "Aaron", level: 2, skills: brakes}, {name: "Bea", level: 2, skills: brakes}})
	early := serviceOn(t, st, today, 7, 0)
	overrun, err := policyBook(ctx, t, early, dealer, seed.ServiceBrake, londonOn(t, today, 8, 0), false) // 08:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`, overrun_at = now() WHERE id = $1`, overrun.ID)

	clock := serviceOn(t, st, today, 9, 40)
	later, err := policyBook(ctx, t, clock, dealer, seed.ServiceBrake, londonOn(t, today, 11, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	if later.ServiceBayID == overrun.ServiceBayID || later.TechnicianID == overrun.TechnicianID {
		t.Fatalf("11:00 booked on the overrun's %s / %s", later.ServiceBayName, later.TechnicianName)
	}
	_, err = clock.Reassign(ctx, staffAdmin, later.ID, booking.ReassignRequest{ServiceBayID: overrun.ServiceBayID})
	expectCode(t, err, domain.CodeConflict)
	if !strings.Contains(err.Error(), "overrunning") {
		t.Fatalf("bay move refused with %q, want the overrun reason", err)
	}
	_, err = clock.Reassign(ctx, staffAdmin, later.ID, booking.ReassignRequest{TechnicianID: overrun.TechnicianID})
	expectCode(t, err, domain.CodeConflict)
	if !strings.Contains(err.Error(), "overrunning") {
		t.Fatalf("technician move refused with %q, want the overrun reason", err)
	}

	if _, err := clock.Complete(ctx, staffAdmin, overrun.ID, 0); err != nil {
		t.Fatal(err)
	}
	moved, err := clock.Reassign(ctx, staffAdmin, later.ID, booking.ReassignRequest{ServiceBayID: overrun.ServiceBayID, TechnicianID: overrun.TechnicianID})
	if err != nil {
		t.Fatalf("after completion: %v", err)
	}
	if moved.ServiceBayID != overrun.ServiceBayID || moved.TechnicianID != overrun.TechnicianID {
		t.Fatalf("moved to %s / %s, want the freed bay and technician", moved.ServiceBayName, moved.TechnicianName)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// Reinstating a no-show puts it back on its booked bay and technician: refused while an overrun
// flagged since the no-show holds either for the day.
func TestReinstateRefusesAnOverrunHeldBay(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, techs := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral},
		[]policyTech{{name: "Aaron", level: 2, skills: brakes}, {name: "Bea", level: 2, skills: brakes}})
	early := serviceOn(t, st, today, 7, 0)
	job, err := policyBook(ctx, t, early, dealer, seed.ServiceBrake, londonOn(t, today, 9, 0), false) // 09:00–10:30
	if err != nil {
		t.Fatal(err)
	}
	other := techs["Bea"]
	if job.TechnicianID == other {
		other = techs["Aaron"]
	}
	// Work that started at 07:30 in the same bay with the other technician, flagged as overrunning
	// after the no-show: it holds the bay until it is completed.
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertRawHold(ctx, hold, dealer, newVehicle(t, svc), job.ServiceBayID, other, londonOn(t, today, 7, 30), londonOn(t, today, 9, 0)); err != nil {
		t.Fatal(err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	clock := serviceOn(t, st, today, 9, 40)
	if job, err = clock.NoShow(ctx, staffAdmin, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`, overrun_at = now()
		WHERE service_bay_id = $1 AND id <> $2`, job.ServiceBayID, job.ID)

	_, err = clock.Reinstate(ctx, staffAdmin, job.ID, 0)
	expectCode(t, err, domain.CodeConflict)
	if !strings.Contains(err.Error(), "overrunning") {
		t.Fatalf("reinstate refused with %q, want the overrun reason", err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusCompleted)+` WHERE service_bay_id = $1 AND id <> $2`, job.ServiceBayID, job.ID)
	if _, err := clock.Reinstate(ctx, staffAdmin, job.ID, 0); err != nil {
		t.Fatalf("after the overrun completed: %v", err)
	}
	assertNoOverlaps(t, pool)
}
