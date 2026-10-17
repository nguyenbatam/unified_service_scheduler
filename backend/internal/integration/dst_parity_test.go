package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// nextLondonTransitions returns the next London fall-back day (25 hours, clocks go back) and
// spring-forward day (23 hours, clocks go forward) after the suite's date.
func nextLondonTransitions(t *testing.T) (fallBack, springForward booking.Date) {
	t.Helper()
	_, loc := suiteToday()
	springForward, fallBack = nextTransitions(t, loc)
	return fallBack, springForward
}

// Bookings, shifts, breaks, and closures on the two clock-change days follow local wall time: an
// 08:00 opening is 08:00 on the clock (07:00 or 08:00 UTC depending on the offset), never "midnight
// plus eight hours", and both days still offer exactly the 20 half-hour oil starts of 08:00–18:00.
func TestDaylightSavingDaysFollowLocalTime(t *testing.T) {
	t.Parallel()
	fall, spring := nextLondonTransitions(t)
	_, loc := suiteToday()
	cases := []struct {
		name      string
		day       booking.Date
		offset    int // UTC offset (seconds) during opening hours on the day
		dayBefore int // ... and on the day before
	}{
		{"clocks go back " + fall.String(), fall, 0, 3600},
		{"clocks go forward " + spring.String(), spring, 3600, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, st, _, ctx := openTestDB(t)
			before := booking.DateFrom(tc.day.Start(loc).Add(-12*time.Hour), loc)
			// The service clock reads midnight the day before, so nothing on the day is "past".
			svc := booking.New(st, nil, booking.WithClock(func() time.Time { return before.Start(loc) }))
			oil := func(day booking.Date) domain.Availability {
				t.Helper()
				avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
					DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: day,
				})
				if err != nil {
					t.Fatal(err)
				}
				return avail
			}
			for _, d := range []struct {
				day    booking.Date
				offset int
			}{{before, tc.dayBefore}, {tc.day, tc.offset}} {
				avail := oil(d.day)
				if len(avail.Slots) != 20 {
					t.Fatalf("%s: %d oil slots, want 20 (08:00–17:30)", d.day, len(avail.Slots))
				}
				first, last := avail.Slots[0].Start, avail.Slots[len(avail.Slots)-1].Start
				if _, off := first.In(loc).Zone(); off != d.offset {
					t.Fatalf("%s: first slot offset %d, want %d", d.day, off, d.offset)
				}
				if got := first.In(loc).Format("15:04"); got != "08:00" {
					t.Fatalf("%s: first slot %s local, want 08:00", d.day, got)
				}
				if got := last.In(loc).Format("15:04"); got != "17:30" {
					t.Fatalf("%s: last slot %s local, want 17:30", d.day, got)
				}
				if wantUTC := 8 - d.offset/3600; first.UTC().Hour() != wantUTC {
					t.Fatalf("%s: first slot %s UTC, want %02d:00", d.day, first.UTC().Format("15:04"), wantUTC)
				}
			}

			// A booking at 14:00 local lands at 14:00 on the wall clock, 30 minutes long.
			item, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, tc.day, 14, 0))
			if err != nil {
				t.Fatal(err)
			}
			if _, off := item.StartAt.In(loc).Zone(); off != tc.offset || item.StartAt.In(loc).Hour() != 14 ||
				item.EndAt.Sub(item.StartAt) != 30*time.Minute {
				t.Fatalf("booked %s–%s (offset %d), want 14:00–14:30 local at offset %d", item.StartAt.In(loc), item.EndAt.In(loc), off, tc.offset)
			}

			// Eve works 09:00–15:00 with a 12:00–12:30 break on the day's weekday (both are Sundays).
			if _, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
				Weekday: tc.day.ISOWeekday(), StartTime: "09:00", EndTime: "15:00", BreakStart: "12:00", BreakEnd: "12:30",
			}}, 0); err != nil {
				t.Fatalf("shift covering the 14:00 booking: %v", err)
			}
			avail := oil(tc.day)
			var free []string
			for _, s := range avail.Slots {
				if s.Available {
					free = append(free, s.Start.In(loc).Format("15:04"))
				}
			}
			// 09:00–14:30 is 12 starts, minus the break (12:00) and the booked 14:00.
			want := []string{"09:00", "09:30", "10:00", "10:30", "11:00", "11:30", "12:30", "13:00", "13:30", "14:30"}
			if len(free) != len(want) {
				t.Fatalf("free starts %v, want %v", free, want)
			}
			for i := range want {
				if free[i] != want[i] {
					t.Fatalf("free starts %v, want %v", free, want)
				}
			}
			for _, hm := range [][2]int{{8, 30}, {12, 0}, {15, 0}} {
				_, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleB, seed.ServiceOil, londonOn(t, tc.day, hm[0], hm[1]))
				expectCode(t, err, domain.CodeUnavailable)
			}
			if _, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleB, seed.ServiceOil, londonOn(t, tc.day, 12, 30)); err != nil {
				t.Fatalf("12:30, right after the break: %v", err)
			}

			// A closure on the day closes it in local terms too.
			if _, err := svc.AddClosure(ctx, staffAdmin, seed.DealerNorth, domain.Closure{Date: tc.day.String()}, 0); err != nil {
				t.Fatal(err)
			}
			closed, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
				DealershipID: seed.DealerNorth, VehicleID: seed.VehicleC, ServiceTypeID: seed.ServiceOil, Day: tc.day,
			})
			if err != nil || !closed.Closed || len(closed.Slots) != 0 {
				t.Fatalf("closed day availability = %+v err=%v", closed, err)
			}
			_, err = bookAt(t, svc, seed.DealerNorth, seed.VehicleC, seed.ServiceOil, londonOn(t, tc.day, 10, 0))
			expectCode(t, err, domain.CodeUnavailable)
			// The day after is untouched by the closure.
			after := booking.DateFrom(tc.day.Next(loc), loc)
			if _, err := bookAt(t, svc, seed.DealerNorth, seed.VehicleC, seed.ServiceOil, londonOn(t, after, 10, 0)); err != nil {
				t.Fatalf("day after the closure: %v", err)
			}
		})
	}
}

// For every slot of a busy day, availability says "available" exactly when booking that slot
// succeeds. The day mixes every reason a slot can be refused: a held bay/technician, a full staging
// window, a technician absence, lunch, and the vehicle's own booking elsewhere.
func TestAvailabilityMatchesBookingForEverySlot(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	at := func(h, m int) time.Time { return londonOn(t, day, h, m) }
	mustBook := func(dealer, vehicle, service uuid.UUID, start time.Time) {
		t.Helper()
		if _, err := bookAt(t, svc, dealer, vehicle, service, start); err != nil {
			t.Fatal(err)
		}
	}
	mustBook(seed.DealerNorth, seed.VehicleB, seed.ServiceMajor, at(8, 0))      // Blair, 08:00–11:00
	mustBook(seed.DealerNorth, seed.VehicleA, seed.ServiceBrake, at(10, 0))     // one brake tech + general bay
	mustBook(seed.DealerNorth, newVehicle(t, svc), seed.ServiceOil, at(14, 0))  // 14:00 window: 1 of 2
	mustBook(seed.DealerNorth, newVehicle(t, svc), seed.ServiceOil, at(14, 30)) // 14:00 window full
	mustBook(seed.DealerRiver, seed.VehicleC, seed.ServiceOil, at(16, 0))       // the car is elsewhere
	if _, err := svc.AddAbsence(ctx, staffAdmin, seed.TechCasey, domain.TechnicianAbsence{
		StartsAt: at(15, 30), EndsAt: at(17, 0), Reason: "parity",
	}, 0); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		service uuid.UUID
		waiter  bool
	}{
		{"brake", seed.ServiceBrake, false},
		{"brake waiter", seed.ServiceBrake, true},
		{"oil", seed.ServiceOil, false},
		{"major", seed.ServiceMajor, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
				DealershipID: seed.DealerNorth, VehicleID: seed.VehicleC, ServiceTypeID: tc.service, Day: day, IsWaiter: tc.waiter,
			})
			if err != nil {
				t.Fatal(err)
			}
			free, taken := 0, 0
			for _, slot := range avail.Slots {
				item, created, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
					DealershipID: seed.DealerNorth, VehicleID: seed.VehicleC, ServiceTypeID: tc.service,
					StartAt: slot.Start, IsWaiter: tc.waiter, IdempotencyKey: uuid.NewString(),
				})
				if err == nil && !created {
					t.Fatalf("%s: fresh key replayed", slot.Start)
				}
				if slot.Available != (err == nil) {
					t.Fatalf("%s: availability says available=%v (reason %q), booking err=%v",
						slot.Start.Format("15:04"), slot.Available, slot.Reason, err)
				}
				if err != nil {
					if !isCleanLoss(err) {
						t.Fatalf("%s: refused with %v", slot.Start.Format("15:04"), err)
					}
					taken++
					continue
				}
				free++
				// Give the slot back so the next one sees the same day.
				if _, err := svc.Cancel(ctx, staffAdmin, item.ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%d slots: %d bookable, %d refused", len(avail.Slots), free, taken)
			if free == 0 || taken == 0 {
				t.Fatalf("free=%d taken=%d: the fixture must mix both", free, taken)
			}
		})
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertStagingWithinCapacity(t, pool)
}

// nextTransitions returns the next spring-forward (23-hour) and fall-back (25-hour) days in loc
// after the suite's date.
func nextTransitions(t *testing.T, loc *time.Location) (springForward, fallBack booking.Date) {
	t.Helper()
	today, _ := suiteToday()
	var gotSpring, gotFall bool
	for day := today.AddDays(1); (!gotSpring || !gotFall) && day.Before(today.AddDays(400)); day = day.AddDays(1) {
		switch day.Next(loc).Sub(day.Start(loc)) {
		case 23 * time.Hour:
			if !gotSpring {
				springForward, gotSpring = day, true
			}
		case 25 * time.Hour:
			if !gotFall {
				fallBack, gotFall = day, true
			}
		}
	}
	if !gotSpring || !gotFall {
		t.Fatalf("no clock changes in %s within 400 days of %s", loc, today)
	}
	return springForward, fallBack
}

// A shift boundary in the hour a clock change skips or repeats: availability (Go,
// booking.roster.onDuty) and booking (SQL, store.technicianOnDutySQL via allocation and
// LockBookableTechnician) must agree on every slot. Both compare the local wall clock; before,
// Go built the boundary instant with time.Date, which moves a skipped 02:30 to 01:30 EST and a
// repeated 01:30 to its first occurrence, so availability offered starts booking then refused.
// The dealership is open (almost) round the clock in America/New_York, where clocks change at 02:00.
func TestShiftBoundaryInClockChangeHourMatchesBooking(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	spring, fall := nextTransitions(t, loc)
	for _, tc := range []struct {
		name       string
		day        booking.Date
		start, end string
	}{
		{"spring forward, shift starts in the skipped hour", spring, "02:30", "08:00"},
		{"spring forward, shift ends in the skipped hour", spring, "00:00", "02:30"},
		{"fall back, shift starts in the repeated hour", fall, "01:30", "08:00"},
		{"fall back, shift ends in the repeated hour", fall, "00:00", "01:30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, st, _, ctx := openTestDB(t)
			dealer, bay, tech := uuid.New(), uuid.New(), uuid.New()
			mustExec(t, pool, `INSERT INTO dealerships (id, name, timezone, open_time, close_time, staging_capacity)
				VALUES ($1, 'Night Owl Service', 'America/New_York', TIME '00:00', TIME '23:30', 0)`, dealer)
			mustExec(t, pool, `INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES ($1, $2, 'Lane 1', 1)`, bay, dealer)
			mustExec(t, pool, `INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES ($1, $2, 'Nina Night', 1)`, tech, dealer)
			mustExec(t, pool, `INSERT INTO technician_skills (technician_id, skill) VALUES ($1, 1)`, tech)
			before := tc.day.AddDays(-1)
			svc := booking.New(st, nil, booking.WithClock(func() time.Time { return before.Start(loc) }))
			if _, err := svc.ReplaceShifts(ctx, staffAdmin, tech, []domain.TechnicianShift{{
				Weekday: tc.day.ISOWeekday(), StartTime: tc.start, EndTime: tc.end,
			}}, 0); err != nil {
				t.Fatal(err)
			}
			vehicle := newVehicle(t, svc)
			avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
				DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: seed.ServiceOil, Day: tc.day,
			})
			if err != nil {
				t.Fatal(err)
			}
			free := 0
			for _, slot := range avail.Slots {
				item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
					DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: seed.ServiceOil,
					StartAt: slot.Start, IdempotencyKey: uuid.NewString(),
				})
				label := slot.Start.In(loc).Format("15:04 MST")
				if slot.Available != (err == nil) {
					t.Fatalf("%s: availability says available=%v (reason %q), booking err=%v", label, slot.Available, slot.Reason, err)
				}
				if err != nil {
					if !isCleanLoss(err) {
						t.Fatalf("%s: refused with %v", label, err)
					}
					continue
				}
				free++
				if _, err := svc.Cancel(ctx, staffAdmin, item.ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			if free == 0 || free == len(avail.Slots) {
				t.Fatalf("%d of %d slots bookable: the shift must split the day", free, len(avail.Slots))
			}
		})
	}
}

// Availability and booking agree slot by slot on a technician's duty: a weekly shift with a break
// plus an absence (availability applies booking.roster.onDuty in Go, allocation store.technicianOnDutySQL).
// At Riverside only Eve does oil changes and brakes, so her roster alone decides.
func TestOnDutyParityAvailabilityVsBooking(t *testing.T) {
	t.Parallel()
	_, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 3)
	roster, err := svc.ReplaceShifts(ctx, staffAdmin, seed.TechEve, []domain.TechnicianShift{{
		Weekday: day.ISOWeekday(), StartTime: "08:00", EndTime: "16:00", BreakStart: "12:00", BreakEnd: "13:00",
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
		StartsAt: londonOn(t, day, 15, 0), EndsAt: londonOn(t, day, 15, 30), Reason: "Training",
	}, roster.Version); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		service uuid.UUID
		want    map[string]bool // spot checks of availability, HH:MM → available
	}{
		{"oil", seed.ServiceOil, map[string]bool{
			"08:00": true, "11:30": true, "12:00": false, "12:30": false, "13:00": true,
			"14:30": true, "15:00": false, "15:30": true, "16:00": false,
		}},
		{"brake", seed.ServiceBrake, map[string]bool{
			"10:30": true, "11:00": false, "11:30": false, "12:30": false, "13:00": true,
			"13:30": true, "14:00": false, "15:30": false,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{
				DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: tc.service, Day: day,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, loc := suiteToday()
			seen := 0
			for _, slot := range avail.Slots {
				hhmm := slot.Start.In(loc).Format("15:04")
				if want, ok := tc.want[hhmm]; ok {
					seen++
					if slot.Available != want {
						t.Errorf("availability at %s: %v (%s), want %v", hhmm, slot.Available, slot.Reason, want)
					}
				}
				item, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, tc.service, slot.Start)
				if booked := err == nil; booked != slot.Available {
					t.Errorf("%s: availability says %v (%s) but booking %v (%v)", hhmm, slot.Available, slot.Reason, booked, err)
				}
				if err != nil {
					if !isCleanLoss(err) {
						t.Fatalf("%s: %v", hhmm, err)
					}
					continue
				}
				if _, err := svc.Cancel(ctx, staffAdmin, item.ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			if seen != len(tc.want) {
				t.Fatalf("only %d of %d spot-checked starts are on the grid", seen, len(tc.want))
			}
		})
	}
}
