package integration_test

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/notify"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A car that arrives after its booked window has ended cannot be checked in on it: the bay and the
// technician may already be sold to the next job (like Reinstate, the visit is rescheduled).
func TestCheckInRefusedOnceTheWindowEnded(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
	job, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	_, err = serviceOn(t, st, today, 9, 30).CheckIn(ctx, staffDispatcher, job.ID, 0)
	expectCode(t, err, domain.CodeUnavailable)
	if _, err := serviceOn(t, st, today, 9, 20).CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatalf("check-in inside the window: %v", err)
	}
}

// Checked in on time but started after the window ended: the hold moves to [now, now+length)
// under GiST, so it never sits on top of the next job's bay and technician.
func TestStartAfterTheWindowEndedMovesIt(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
	late, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	next, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 30)) // 09:30–10:00
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceOn(t, st, today, 9, 20).CheckIn(ctx, staffDispatcher, late.ID, 0); err != nil {
		t.Fatal(err)
	}
	clock := serviceOn(t, st, today, 9, 45)
	_, err = clock.StartWork(ctx, staffDispatcher, late.ID, 0)
	expectCode(t, err, domain.CodeConflict) // [09:45, 10:15) would sit on the 09:30 job's bay
	if got := mustGet(t, svc, late.ID); got.Status != domain.StatusCheckedIn || !got.StartAt.Equal(late.StartAt) {
		t.Fatalf("refused start changed the job: %s %s", got.Status.Name(), got.StartAt)
	}

	if _, err := svc.Cancel(ctx, staffDispatcher, next.ID, 0); err != nil {
		t.Fatal(err)
	}
	started, err := clock.StartWork(ctx, staffDispatcher, late.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !started.StartAt.Equal(londonOn(t, today, 9, 45)) || !started.EndAt.Equal(londonOn(t, today, 10, 15)) {
		t.Fatalf("late start window = %s–%s, want 09:45–10:15", started.StartAt, started.EndAt)
	}
	assertNoOverlaps(t, pool)
}

// A late start racing a new booking for the only bay and technician over the moved window: exactly
// one wins, the other gets a clean refusal (never a 500 or a deadlock), and nothing overlaps.
func TestLateStartRacingABookingOneWinner(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for offset := range 4 {
		day := londonDay(t, offset)
		dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
		late, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, day, 9, 0)) // 09:00–09:30
		if err != nil {
			t.Fatal(err)
		}
		if _, err := serviceOn(t, st, day, 9, 20).CheckIn(ctx, staffDispatcher, late.ID, 0); err != nil {
			t.Fatal(err)
		}
		clock := serviceOn(t, st, day, 9, 45) // the start moves to 09:45–10:15
		walkIn := newVehicle(t, svc)
		var startErr, bookErr error
		race(func() {
			_, startErr = clock.StartWork(ctx, staffDispatcher, late.ID, 0)
		}, func() {
			_, bookErr = bookAt(t, clock, dealer, walkIn, seed.ServiceOil, londonOn(t, day, 10, 0))
		})
		oneWinner(t, day.String(), loses(startErr, domain.CodeConflict), loses(bookErr, refusals...))
	}
	assertNoOverlaps(t, pool)
}

// A manual extension that still ends in the past leaves the car on the lift: the overrun flag,
// and the hold it keeps on the bay and technician, stay until the new end is ahead of now.
func TestManualExtendEndingInThePastKeepsTheOverrunFlag(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
	job, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serviceOn(t, st, today, 8, 50).CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := serviceOn(t, st, today, 9, 0).StartWork(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET overrun_at = now() WHERE id = $1`, job.ID)
	flagged := func() bool {
		return scalar[bool](t, pool, `SELECT overrun_at IS NOT NULL FROM appointments WHERE id = $1`, job.ID)
	}

	clock := serviceOn(t, st, today, 11, 0)
	if _, err := clock.Extend(ctx, staffDispatcher, job.ID, 15, 0); err != nil { // to 09:45, still past
		t.Fatal(err)
	}
	if !flagged() {
		t.Fatal("an extension ending at 09:45 cleared the flag at 11:00")
	}
	if _, err := clock.Extend(ctx, staffDispatcher, job.ID, 120, 0); err != nil { // to 11:45
		t.Fatal(err)
	}
	if flagged() {
		t.Fatal("an extension past now kept the flag")
	}
}

// Changing only the waiter flag keeps the window: it is a reassign (no "your appointment has
// moved" notice), and the reminders keep their schedule.
func TestWaiterOnlyChangeIsAReassign(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress, domain.BayGeneral}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
	start := londonOn(t, londonDay(t, 1), 10, 0)
	job, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, start)
	if err != nil {
		t.Fatal(err)
	}
	scheduledAt := func() time.Time {
		return scalar[time.Time](t, pool, `SELECT scheduled_at FROM appointments WHERE id = $1`, job.ID)
	}
	before := scheduledAt()
	waiter := true
	moved, err := svc.Reschedule(ctx, staffAdmin, job.ID, booking.RescheduleRequest{StartAt: start, IsWaiter: &waiter})
	if err != nil {
		t.Fatal(err)
	}
	if !moved.IsWaiter || !moved.StartAt.Equal(start) || moved.Version != job.Version+1 {
		t.Fatalf("after the change: waiter %v start %s v%d", moved.IsWaiter, moved.StartAt, moved.Version)
	}
	wantEvents(t, "reassigned", pool, job.ID, domain.EventAppointmentReassigned, 1)
	wantEvents(t, "rescheduled", pool, job.ID, domain.EventAppointmentRescheduled, 0)
	if got := scheduledAt(); !got.Equal(before) {
		t.Fatalf("scheduled_at moved from %s to %s", before, got)
	}
}

// A flagged overrun is a carry-over: the car is still on the lift the next morning, so its bay is
// not sold until the job is completed.
func TestFlaggedOverrunCarriesOverToTheNextDay(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today, tomorrow := londonDay(t, 0), londonDay(t, 1)
	// The only bay is the quick lane; a second technician keeps the technician side free.
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress},
		[]policyTech{{name: "Aaron", level: 1, skills: lube}, {name: "Bea", level: 1, skills: lube}})
	job, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 16, 0))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`, overrun_at = now() WHERE id = $1`, job.ID)

	clock := serviceOn(t, st, tomorrow, 7, 0)
	avail, err := clock.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
		DealershipID: dealer, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: tomorrow,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range avail.Slots {
		if s.Available {
			t.Fatalf("slot %s offered while the quick lane still holds yesterday's overrun", s.Start)
		}
	}
	_, err = bookAt(t, clock, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, tomorrow, 9, 0))
	expectCode(t, err, domain.CodeUnavailable)
	// Today's board shows it, so the dispatcher sees why the bay cannot be booked.
	board, err := clock.Schedule(ctx, staffAdmin, dealer, tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(board.Appointments, func(a domain.Appointment) bool { return a.ID == job.ID }) {
		t.Fatal("the carried-over job is missing from the next day's board")
	}
	// Left open for half a day it is forgotten work: the workshop queue metric counts it.
	mustExec(t, pool, `UPDATE appointments SET start_at = now() - interval '13 hours', end_at = now() - interval '12 hours 30 minutes' WHERE id = $1`, job.ID)
	depths, err := st.QueueDepths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(depths, store.QueueDepth{Queue: "workshop", Status: "stale_overrun", Count: 1}) {
		t.Fatalf("queue depths %+v, want one stale overrun", depths)
	}

	if _, err := clock.Complete(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := bookAt(t, clock, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, tomorrow, 9, 0)); err != nil {
		t.Fatalf("after completion: %v", err)
	}
	assertNoOverlaps(t, pool)
}

// A "we missed you" notice still queued when staff reinstate the late customer is never sent.
func TestNoShowNoticeSkippedAfterReinstate(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	today := londonDay(t, 0)
	dealer, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress}, []policyTech{{name: "Aaron", level: 1, skills: lube}})
	job, err := bookAt(t, svc, dealer, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	clock := serviceOn(t, st, today, 9, 15)
	if _, err := clock.NoShow(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	n := notify.New(st, sender, slog.Default(), notify.DefaultConfig())
	for _, ev := range drainOutboxEvents(t, st) {
		if ev.Type == domain.EventAppointmentNoShow {
			if err := n.HandleEvent(ctx, ev); err != nil {
				t.Fatal(err)
			}
		}
	}
	if queued := scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE appointment_id = $1 AND kind = `+code(domain.KindNoShow), job.ID); queued == 0 {
		t.Fatal("precondition: the no-show notice was not queued")
	}
	if _, err := clock.Reinstate(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	for _, sent := range sender.sent {
		if sent.Kind == domain.KindNoShow {
			t.Fatalf("no-show notice sent after the reinstate: %+v", sent)
		}
	}
	if pending := scalar[int](t, pool, `SELECT count(*)::int FROM notifications WHERE appointment_id = $1 AND kind = `+
		code(domain.KindNoShow)+` AND status <> `+code(domain.NotificationSkipped), job.ID); pending != 0 {
		t.Fatalf("%d no-show notices not skipped", pending)
	}
}

// The schema itself ties an appointment's bay and technician to its dealership and holds the
// documented formats, so a script cannot write what the booking rules would refuse.
func TestSchemaHoldsTheBookingIntegrity(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, londonDay(t, 1), 10, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, sql, constraint string
		args                  []any
	}{
		{"bay of another dealership", `UPDATE appointments SET service_bay_id = $2 WHERE id = $1`, "appointments_bay_dealership_fkey", []any{job.ID, seed.BayN1}},
		{"technician of another dealership", `UPDATE appointments SET technician_id = $2 WHERE id = $1`, "appointments_technician_dealership_fkey", []any{job.ID, seed.TechAlex}},
		{"sixth job line", `UPDATE appointment_lines SET line_no = 6 WHERE appointment_id = $1`, "appointment_lines_line_no_check", []any{job.ID}},
		{"duration off the grid", `UPDATE service_types SET duration_minutes = 45 WHERE id = $1`, "service_types_duration_minutes_check", []any{seed.ServiceOil}},
		{"VIN with an O", `UPDATE vehicles SET vin = 'WBAOOOOOOOOOOOOO1' WHERE id = $1`, "vehicles_vin_format", []any{job.VehicleID}},
	} {
		_, err := pool.Exec(ctx, tc.sql, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.constraint) {
			t.Errorf("%s: want a %s violation, got %v", tc.name, tc.constraint, err)
		}
	}
}
