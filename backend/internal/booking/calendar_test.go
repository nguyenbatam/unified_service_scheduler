package booking

import (
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func TestISOWeekday(t *testing.T) {
	for _, tc := range []struct {
		day  Date
		want int16
	}{
		{Date{2026, time.October, 12}, 1}, // Monday
		{Date{2026, time.October, 17}, 6}, // Saturday
		{Date{2026, time.October, 18}, 7}, // Sunday
	} {
		if got := tc.day.ISOWeekday(); got != tc.want {
			t.Errorf("%s: weekday %d, want %d", tc.day, got, tc.want)
		}
	}
}

func TestResolveDay(t *testing.T) {
	dealer := domain.Dealership{OpenTime: "08:00", CloseTime: "17:00", LunchStart: "12:00", LunchEnd: "13:00"}
	sunday := Date{2026, time.October, 18}
	for _, tc := range []struct {
		name  string
		rules store.DayRules
		want  DayHours
	}{
		{"defaults", store.DayRules{},
			DayHours{OpenTime: "08:00", CloseTime: "17:00", LunchStart: "12:00", LunchEnd: "13:00"}},
		{"override drops lunch", store.DayRules{Override: &domain.WeekdayHours{OpenTime: "09:00", CloseTime: "12:00"}},
			DayHours{OpenTime: "09:00", CloseTime: "12:00"}},
		{"closed weekday", store.DayRules{Override: &domain.WeekdayHours{Closed: true}},
			DayHours{Closed: true, Reason: "Sunday", OpenTime: "08:00", CloseTime: "17:00", LunchStart: "12:00", LunchEnd: "13:00"}},
		{"closure wins over override", store.DayRules{
			Override: &domain.WeekdayHours{OpenTime: "09:00", CloseTime: "12:00"},
			Closure:  &domain.Closure{Reason: "Bank holiday"},
		}, DayHours{Closed: true, Reason: "Bank holiday", OpenTime: "09:00", CloseTime: "12:00"}},
		{"closure without reason", store.DayRules{Closure: &domain.Closure{}},
			DayHours{Closed: true, Reason: "closure", OpenTime: "08:00", CloseTime: "17:00", LunchStart: "12:00", LunchEnd: "13:00"}},
	} {
		if got := ResolveDay(dealer, sunday, tc.rules); got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestLongestWindow(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hours DayHours
		want  time.Duration
	}{
		{"no lunch", DayHours{OpenTime: "08:00", CloseTime: "18:00"}, 10 * time.Hour},
		{"longer afternoon", DayHours{OpenTime: "08:00", CloseTime: "18:00", LunchStart: "12:00", LunchEnd: "13:00"}, 5 * time.Hour},
		{"longer morning", DayHours{OpenTime: "07:30", CloseTime: "15:00", LunchStart: "12:30", LunchEnd: "13:00"}, 5 * time.Hour},
		{"closed", DayHours{Closed: true, OpenTime: "08:00", CloseTime: "18:00"}, 0},
	} {
		got, err := tc.hours.LongestWindow()
		if err != nil || got != tc.want {
			t.Errorf("%s: got %s %v, want %s", tc.name, got, err, tc.want)
		}
	}
}

func TestValidateWeekly(t *testing.T) {
	ok := []domain.WeekdayHours{
		{Weekday: 6, OpenTime: "08:00", CloseTime: "12:00"},
		{Weekday: 7, Closed: true},
		{Weekday: 1, OpenTime: "08:00", CloseTime: "17:00", LunchStart: "12:00", LunchEnd: "13:00"},
	}
	if err := validateWeekly(ok); err != nil {
		t.Fatalf("valid week rejected: %v", err)
	}
	for name, bad := range map[string][]domain.WeekdayHours{
		"weekday 0":         {{Weekday: 0, Closed: true}},
		"duplicate":         {{Weekday: 2, Closed: true}, {Weekday: 2, Closed: true}},
		"closed with lunch": {{Weekday: 3, Closed: true, LunchStart: "12:00", LunchEnd: "13:00"}},
		"bad clock":         {{Weekday: 4, OpenTime: "8:00", CloseTime: "17:00"}},
		"half lunch":        {{Weekday: 5, OpenTime: "08:00", CloseTime: "17:00", LunchEnd: "13:00"}},
	} {
		if err := validateWeekly(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOverrunEnd(t *testing.T) {
	end := time.Date(2026, time.October, 17, 11, 0, 0, 0, time.UTC)
	step := 15 * time.Minute
	for _, tc := range []struct {
		now  time.Time
		want time.Time
	}{
		{end, end.Add(15 * time.Minute)},                       // exactly at the end: one step
		{end.Add(5 * time.Minute), end.Add(15 * time.Minute)},  // inside the first step
		{end.Add(15 * time.Minute), end.Add(30 * time.Minute)}, // strictly after now
		{end.Add(50 * time.Minute), end.Add(60 * time.Minute)}, // sweeper was down: catch up in whole steps
	} {
		if got := overrunEnd(end, tc.now, step); !got.Equal(tc.want) {
			t.Errorf("now %s: end %s, want %s", tc.now.Format("15:04"), got.Format("15:04"), tc.want.Format("15:04"))
		}
	}
}
