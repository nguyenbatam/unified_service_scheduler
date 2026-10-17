package integration_test

import (
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A job that has started still depends on the rest of its day: at 10:30 a checked-in brake job
// (10:00–11:30) is stranded by closing at 11:00 or by a closure of today, so both are refused
// like the roster guard refuses a shift edit; opening later at 10:15 only touches the window's
// past and is allowed.
func TestCalendarEditsNeverStrandStartedJobs(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayGeneral}, []policyTech{{name: "Aaron", level: 2, skills: brakes}})
	job, err := policyBook(ctx, t, svc, dealer, seed.ServiceBrake, londonOn(t, today, 10, 0), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceOn(t, st, today, 10, 5).CheckIn(ctx, staffAdmin, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	at1030 := serviceOn(t, st, today, 10, 30)
	hours := func(open, close string) []domain.WeekdayHours {
		return []domain.WeekdayHours{{Weekday: today.ISOWeekday(), OpenTime: open, CloseTime: close}}
	}

	if _, err := at1030.ReplaceWeeklyHours(ctx, staffAdmin, dealer, hours("10:15", "18:00"), 0); err != nil {
		t.Fatalf("opening later than a started job's start (its past only): %v", err)
	}
	_, err = at1030.ReplaceWeeklyHours(ctx, staffAdmin, dealer, hours("08:00", "11:00"), 0)
	expectCode(t, err, domain.CodeConflict)
	_, err = at1030.AddClosure(ctx, staffAdmin, dealer, domain.Closure{Date: today.String(), Reason: "Flood"}, 0)
	expectCode(t, err, domain.CodeConflict)

	// In-progress work is left out, as in the roster guard: the car is on the lift and the
	// overrun sweep, not the calendar, decides when it is done.
	if _, err := at1030.StartWork(ctx, staffAdmin, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := at1030.ReplaceWeeklyHours(ctx, staffAdmin, dealer, hours("08:00", "11:00"), 0); err != nil {
		t.Fatalf("closing at 11:00 while the job is in progress: %v", err)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
