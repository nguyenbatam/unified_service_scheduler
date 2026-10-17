package integration_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Northside stages 2 arrivals per hour. Rescheduling into another, full arrival hour is refused on
// staging alone (the bay and technician are free there) and leaves the appointment where it was.
func TestRescheduleIntoFullStagingWindow(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	at := func(h, m int) time.Time { return londonOn(t, day, h, m) }
	moving := mustBook(t, svc, oilAt(seed.DealerNorth, newVehicle(t, svc), at(14, 0)))
	// Two brake jobs fill the 15:00 hour (general bays and brake technicians, not Bay 1 or Alex).
	mustBook(t, svc, brakeAt(seed.DealerNorth, newVehicle(t, svc), at(15, 0)))
	mustBook(t, svc, brakeAt(seed.DealerNorth, newVehicle(t, svc), at(15, 0)))

	_, err := svc.Reschedule(ctx, staffDispatcher, moving.ID, booking.RescheduleRequest{StartAt: at(15, 30)})
	expectCode(t, err, domain.CodeUnavailable)
	if !strings.Contains(err.Error(), "staging") {
		t.Fatalf("refused with %v, want the staging cap", err)
	}
	if got := mustGet(t, svc, moving.ID); !got.StartAt.Equal(moving.StartAt) || got.Version != moving.Version {
		t.Fatalf("refused move changed the appointment: %s v%d", got.StartAt, got.Version)
	}
	wantEvents(t, "refused move", pool, moving.ID, domain.EventAppointmentRescheduled, 0)
	// Within its own (not full) hour the move is fine.
	if _, err := svc.Reschedule(ctx, staffDispatcher, moving.ID, booking.RescheduleRequest{StartAt: at(14, 30)}); err != nil {
		t.Fatalf("move within its own arrival hour: %v", err)
	}
	assertStagingWithinCapacity(t, pool)
}

// A reschedule and a new booking race for the last staging place of an arrival hour (both have a
// free bay and technician there): exactly one wins, the hour never exceeds its cap, and the loser
// leaves nothing behind (the moved appointment stays put, the booking has no row or event).
func TestRescheduleRacesBookForLastStagingPlace(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for round := 1; round <= 3; round++ {
		day := londonDay(t, round)
		label := fmt.Sprintf("round %d", round)
		moving := mustBook(t, svc, oilAt(seed.DealerNorth, newVehicle(t, svc), londonOn(t, day, 14, 0)))
		mustBook(t, svc, brakeAt(seed.DealerNorth, newVehicle(t, svc), londonOn(t, day, 15, 0))) // 1 of 2
		walkIn := newVehicle(t, svc)
		var moveErr, bookErr error
		var booked domain.Appointment
		race(
			func() {
				_, moveErr = svc.Reschedule(ctx, staffDispatcher, moving.ID, booking.RescheduleRequest{StartAt: londonOn(t, day, 15, 30)})
			},
			func() {
				booked, _, bookErr = svc.Book(ctx, staffAdmin, oilAt(seed.DealerNorth, walkIn, londonOn(t, day, 15, 0)))
			},
		)
		switch oneWinner(t, label, loses(moveErr, refusals...), loses(bookErr, refusals...)) {
		case 0:
			wantEvents(t, label, pool, moving.ID, domain.EventAppointmentRescheduled, 1)
		case 1:
			assertEventsFor(t, pool, booked.ID)
			if got := mustGet(t, svc, moving.ID); !got.StartAt.Equal(moving.StartAt) {
				t.Fatalf("%s: the losing move changed the appointment to %s", label, got.StartAt)
			}
			wantEvents(t, label, pool, moving.ID, domain.EventAppointmentRescheduled, 0)
		}
	}
	assertStagingWithinCapacity(t, pool)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}
