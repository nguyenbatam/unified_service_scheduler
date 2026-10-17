package integration_test

import (
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Reassign checks the new technician over the part of the window still ahead of now, like
// Reinstate and Extend: a late brake job (09:30–11:00, checked in) may move at 10:15 to Zoe, whose
// shift began at 10:00, but not at 09:45, when part of what is ahead is still before her shift;
// past its booked end only duty right now counts.
func TestReassignChecksOnlyTheWindowAhead(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, ids := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral}, []policyTech{
		{name: "Aaron", level: 2, skills: brakes},
		{name: "Zoe", level: 2, skills: brakes, shift: &domain.TechnicianShift{StartTime: "10:00", EndTime: "18:00"}},
		{name: "Yves", level: 2, skills: brakes, shift: &domain.TechnicianShift{StartTime: "11:00", EndTime: "18:00"}},
	})
	job, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, today, 9, 30), false)
	wantTech(t, "09:30 brake job", job, err, ids, "Aaron")
	if job, err = serviceOn(t, st, today, 9, 40).CheckIn(ctx, staffAdmin, job.ID, 0); err != nil {
		t.Fatal(err)
	}

	_, err = serviceOn(t, st, today, 9, 45).Reassign(ctx, staffAdmin, job.ID, booking.ReassignRequest{TechnicianID: ids["Zoe"]})
	expectCode(t, err, domain.CodeUnavailable)

	moved, err := serviceOn(t, st, today, 10, 15).Reassign(ctx, staffAdmin, job.ID, booking.ReassignRequest{TechnicianID: ids["Zoe"]})
	wantTech(t, "reassign at 10:15", moved, err, ids, "Zoe")
	// Still checked in after its booked end (11:00): nothing of the window is ahead, so the new
	// technician only has to be on duty now — Yves, whose shift starts at 11:00, after the whole
	// booked window.
	moved, err = serviceOn(t, st, today, 11, 15).Reassign(ctx, staffAdmin, job.ID, booking.ReassignRequest{TechnicianID: ids["Yves"]})
	wantTech(t, "reassign after the booked end", moved, err, ids, "Yves")
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
