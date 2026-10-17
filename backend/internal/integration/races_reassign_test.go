package integration_test

import (
	"sync"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Writers queued on one appointment's row lock while another moves it to a different bay must
// still find it. The lock used to be taken inside the read that joins the bay and technician:
// after the wait Postgres re-checked that join against the bay row it had already read, the moved
// row no longer matched, and the waiter answered 404 "appointment not found" (found by the stress
// run's concurrent reassign burst).
func TestConcurrentReassignsNeverLoseTheAppointment(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral, domain.BayGeneral},
		[]policyTech{{name: "T1", level: 2, skills: lubeBrakes}})
	appt, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, day, 10, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	bays := shopBays(t, svc, dealer, day)
	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := range writers {
		target := bays["Bay A"]
		if i%2 == 1 {
			target = bays["Bay B"]
		}
		wg.Go(func() {
			<-gate
			_, errs[i] = svc.Reassign(ctx, staffAdmin, appt.ID, booking.ReassignRequest{ServiceBayID: target})
		})
	}
	close(gate)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("reassign %d: %v (every one must apply or replay: the appointment never disappears)", i, err)
		}
	}
	got := mustGet(t, svc, appt.ID)
	if got.Version <= appt.Version {
		t.Fatalf("version %d after %d reassigns, want above %d", got.Version, writers, appt.Version)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
