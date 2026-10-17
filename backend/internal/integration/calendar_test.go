package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func bookAt(t *testing.T, svc *booking.Service, dealer, vehicle, service uuid.UUID, start time.Time) (domain.Appointment, error) {
	t.Helper()
	item, _, err := svc.Book(t.Context(), staffAdmin, booking.BookRequest{
		DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: service, StartAt: start,
		IdempotencyKey: uuid.NewString(),
	})
	return item, err
}

func TestClosureBlocksBookingAvailabilityAndBoard(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	tomorrow := londonDay(t, 1)

	_, err := svc.Calendar(ctx, customerAmelia, seed.DealerNorth)
	expectCode(t, err, domain.CodeForbidden)
	_, err = svc.AddClosure(ctx, staffAdmin, seed.DealerNorth, domain.Closure{Date: tomorrow.String()}, 99)
	expectCode(t, err, domain.CodePrecondition)

	before, err := svc.Calendar(ctx, staffAdmin, seed.DealerNorth)
	if err != nil {
		t.Fatal(err)
	}
	cal, err := svc.AddClosure(ctx, staffAdmin, seed.DealerNorth,
		domain.Closure{Date: tomorrow.String(), Reason: "Bank holiday"}, before.Version)
	if err != nil {
		t.Fatal(err)
	}
	if cal.Version != before.Version+1 || len(cal.Closures) != 1 || cal.Closures[0].Date != tomorrow.String() {
		t.Fatalf("calendar after closure = %+v", cal)
	}
	_, err = svc.AddClosure(ctx, staffAdmin, seed.DealerNorth, domain.Closure{Date: tomorrow.String()}, 0)
	expectCode(t, err, domain.CodeConflict)

	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: tomorrow, IsWaiter: false})
	if err != nil {
		t.Fatal(err)
	}
	if !avail.Closed || avail.ClosedReason != "Bank holiday" || len(avail.Slots) != 0 {
		t.Fatalf("availability on a closure = %+v", avail)
	}
	_, err = bookAt(t, svc, seed.DealerNorth, seed.VehicleA, seed.ServiceOil, londonOn(t, tomorrow, 10, 0))
	expectCode(t, err, domain.CodeUnavailable)
	board, err := svc.Schedule(ctx, staffAdmin, seed.DealerNorth, tomorrow)
	if err != nil {
		t.Fatal(err)
	}
	if !board.Closed || board.OpenTime != "08:00" {
		t.Fatalf("board on a closure = closed %v open %s", board.Closed, board.OpenTime)
	}

	if _, err := svc.DeleteClosure(ctx, staffAdmin, seed.DealerNorth, tomorrow, 0); err != nil {
		t.Fatal(err)
	}
	_, err = svc.DeleteClosure(ctx, staffAdmin, seed.DealerNorth, tomorrow, 0)
	expectCode(t, err, domain.CodeNotFound)
	if _, err := bookAt(t, svc, seed.DealerNorth, seed.VehicleA, seed.ServiceOil, londonOn(t, tomorrow, 10, 0)); err != nil {
		t.Fatalf("reopened day: %v", err)
	}
	// Bookings now sit on that day: closing it again would strand them.
	_, err = svc.AddClosure(ctx, staffAdmin, seed.DealerNorth, domain.Closure{Date: tomorrow.String()}, 0)
	expectCode(t, err, domain.CodeConflict)
}

func TestWeekdayOverrideReplacesDefaultHours(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	tomorrow := londonDay(t, 1)
	wd := tomorrow.ISOWeekday()

	// Short day without lunch: 09:00–12:00.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
		{Weekday: wd, OpenTime: "09:00", CloseTime: "12:00"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	avail, err := svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: tomorrow, IsWaiter: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(avail.Slots) != 6 || !avail.Slots[0].Start.Equal(londonOn(t, tomorrow, 9, 0)) ||
		!avail.Slots[5].Start.Equal(londonOn(t, tomorrow, 11, 30)) {
		t.Fatalf("slots on a 09:00–12:00 day = %d (%v)", len(avail.Slots), avail.Slots)
	}
	_, err = bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, tomorrow, 8, 0))
	expectCode(t, err, domain.CodeUnavailable) // on the grid, outside that day's hours
	board, err := svc.Schedule(ctx, staffAdmin, seed.DealerRiver, tomorrow)
	if err != nil || board.OpenTime != "09:00" || board.CloseTime != "12:00" {
		t.Fatalf("board hours = %s–%s %v", board.OpenTime, board.CloseTime, err)
	}

	// Closed weekday.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
		{Weekday: wd, Closed: true},
	}, 0); err != nil {
		t.Fatal(err)
	}
	avail, err = svc.Availability(ctx, staffAdmin, booking.AvailabilityQuery{DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil, Day: tomorrow, IsWaiter: false})
	if err != nil || !avail.Closed || avail.ClosedReason == "" {
		t.Fatalf("closed weekday availability = %+v %v", avail, err)
	}

	// Northside has a default 12:00–13:00 lunch; an override without lunch drops it that day.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, []domain.WeekdayHours{
		{Weekday: wd, OpenTime: "08:00", CloseTime: "17:00"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := bookAt(t, svc, seed.DealerNorth, seed.VehicleB, seed.ServiceOil, londonOn(t, tomorrow, 12, 0)); err != nil {
		t.Fatalf("12:00 with no lunch that day: %v", err)
	}

	// [] clears every override: Riverside is back on its default 08:00–18:00.
	cal, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{}, 0)
	if err != nil || len(cal.Weekly) != 0 {
		t.Fatalf("clear overrides = %+v %v", cal, err)
	}
	if _, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, tomorrow, 8, 0)); err != nil {
		t.Fatalf("default hours restored: %v", err)
	}
}

func TestWeekdayOverrideValidation(t *testing.T) {
	t.Parallel()
	_, svc, ctx := openTestStore(t)
	for name, weekly := range map[string][]domain.WeekdayHours{
		"weekday out of range": {{Weekday: 8, OpenTime: "08:00", CloseTime: "17:00"}},
		"weekday twice":        {{Weekday: 1, Closed: true}, {Weekday: 1, Closed: true}},
		"closed with times":    {{Weekday: 2, Closed: true, OpenTime: "08:00"}},
		"open without times":   {{Weekday: 3}},
		"lunch half set":       {{Weekday: 4, OpenTime: "08:00", CloseTime: "17:00", LunchStart: "12:00"}},
		"close before open":    {{Weekday: 5, OpenTime: "17:00", CloseTime: "08:00"}},
		"lunch outside hours":  {{Weekday: 6, OpenTime: "08:00", CloseTime: "12:00", LunchStart: "12:00", LunchEnd: "13:00"}},
	} {
		_, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, weekly, 0)
		if domainErr, ok := domain.AsError(err); !ok || domainErr.Code != domain.CodeInvalid {
			t.Errorf("%s: want invalid, got %v", name, err)
		}
	}
}

// The stranded check reads the hours that apply to each booking's own day: an override for that
// weekday protects it from default-hour changes, and conflicting overrides are refused.
func TestCalendarChangesCannotStrandBookings(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	tomorrow := londonDay(t, 1)
	wd := tomorrow.ISOWeekday()
	if _, err := bookAt(t, svc, seed.DealerNorth, seed.VehicleA, seed.ServiceBrake, londonOn(t, tomorrow, 15, 0)); err != nil {
		t.Fatal(err) // 15:00–16:30
	}
	for name, weekly := range map[string][]domain.WeekdayHours{
		"closes before the job ends": {{Weekday: wd, OpenTime: "08:00", CloseTime: "16:00"}},
		"closed that weekday":        {{Weekday: wd, Closed: true}},
		"lunch over the job":         {{Weekday: wd, OpenTime: "08:00", CloseTime: "17:00", LunchStart: "15:30", LunchEnd: "16:00"}},
	} {
		_, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, weekly, 0)
		if domainErr, ok := domain.AsError(err); !ok || domainErr.Code != domain.CodeConflict {
			t.Errorf("%s: want conflict, got %v", name, err)
		}
	}
	// Another weekday is unaffected.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, []domain.WeekdayHours{
		{Weekday: wd%7 + 1, Closed: true},
	}, 0); err != nil {
		t.Fatalf("closing a different weekday: %v", err)
	}
	// With an 08:00–17:00 override on the booking's weekday, shortening the defaults is fine.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, []domain.WeekdayHours{
		{Weekday: wd, OpenTime: "08:00", CloseTime: "17:00"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{CloseTime: new("16:00")}); err != nil {
		t.Fatalf("default hours do not apply on an overridden weekday: %v", err)
	}
	// Dropping the override now would put the job under the 16:00 default close.
	_, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerNorth, []domain.WeekdayHours{}, 0)
	expectCode(t, err, domain.CodeConflict)
}

// A closure and a booking for the same day race: the dealership row lock orders them, so either
// the booking lands and the closure is refused (409), or the closure lands and the booking is
// refused (422). Never both.
func TestClosureAndBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	for i := 1; i <= 8; i++ {
		day := londonDay(t, i)
		var closeErr, bookErr error
		race(func() {
			_, closeErr = svc.AddClosure(ctx, staffAdmin, seed.DealerRiver, domain.Closure{Date: day.String()}, 0)
		}, func() {
			_, bookErr = bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, day, 10, 0))
		})
		oneWinner(t, day.String(), loses(closeErr, domain.CodeConflict), loses(bookErr, domain.CodeUnavailable))
	}
	// Recount: no holding appointment on a closed Riverside day.
	stranded := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a
		JOIN dealership_closures c ON c.dealership_id = a.dealership_id
			AND c.day = (a.start_at AT TIME ZONE 'Europe/London')::date
		WHERE a.dealership_id = $1 AND a.status IN (`+holdingCodes+`)`, seed.DealerRiver)
	if stranded != 0 {
		t.Fatalf("%d holding appointments on a closed day", stranded)
	}
	assertNoOrphanEvents(t, pool)
}

// A service longer than every uninterrupted opening window could never be booked anywhere.
func TestServiceDurationMustFitAnOpenDay(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	// Riverside gets a lunch too: longest window is now 13:00–18:00 = 300 min (Northside: 240).
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerRiver, store.DealershipPatch{LunchStart: new("12:00"), LunchEnd: new("13:00")}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.UpdateServiceType(ctx, staffAdmin, seed.ServiceMajor, store.ServiceTypePatch{DurationMinutes: new(330)})
	expectCode(t, err, domain.CodeInvalid)
	if _, err := svc.UpdateServiceType(ctx, staffAdmin, seed.ServiceMajor, store.ServiceTypePatch{DurationMinutes: new(300)}); err != nil {
		t.Fatalf("300 fits Riverside 13:00–18:00: %v", err)
	}
	// A long weekday without lunch makes room for longer jobs.
	if _, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
		{Weekday: 6, OpenTime: "07:00", CloseTime: "17:30"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateServiceType(ctx, staffAdmin, seed.ServiceMajor, store.ServiceTypePatch{DurationMinutes: new(600)}); err != nil {
		t.Fatalf("600 fits a 07:00–17:30 Saturday: %v", err)
	}
}
