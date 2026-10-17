package booking

import (
	"slices"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func TestOnDuty(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	day := Date{Year: 2026, Month: time.October, Day: 19} // Monday
	at := func(h, m int) time.Time { return Clock{Hour: h, Minute: m}.On(day, loc) }
	slot := func(h, m, mins int) Interval {
		return Interval{Start: at(h, m), End: at(h, m).Add(time.Duration(mins) * time.Minute)}
	}
	shift := []domain.TechnicianShift{{Weekday: 1, StartTime: "09:00", EndTime: "15:00", BreakStart: "11:30", BreakEnd: "12:00"}}
	tuesday := []domain.TechnicianShift{{Weekday: 2, StartTime: "09:00", EndTime: "15:00"}}
	absence := []store.Busy{{Start: at(13, 0), End: at(14, 0)}}

	tests := []struct {
		name     string
		shifts   []domain.TechnicianShift
		absences []store.Busy
		slot     Interval
		free     bool
	}{
		{"no pattern: always on", nil, nil, slot(8, 0, 60), true},
		{"no pattern: absence blocks", nil, absence, slot(13, 30, 30), false},
		{"pattern without that weekday: off all day", tuesday, nil, slot(10, 0, 30), false},
		{"before shift", shift, nil, slot(8, 30, 60), false},
		{"inside shift", shift, nil, slot(9, 0, 120), true},
		{"ends at break start", shift, nil, slot(11, 0, 30), true},
		{"crosses break", shift, nil, slot(11, 0, 60), false},
		{"starts at break end", shift, nil, slot(12, 0, 60), true},
		{"ends at shift end", shift, nil, slot(14, 0, 60), true},
		{"past shift end", shift, nil, slot(14, 30, 60), false},
		{"absence inside shift", shift, absence, slot(13, 30, 30), false},
		{"ends at the next local midnight", []domain.TechnicianShift{{Weekday: 1, StartTime: "00:00", EndTime: "23:59"}}, nil,
			Interval{Start: at(23, 30), End: day.Next(loc)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := newRoster(tt.shifts, tt.absences)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.onDuty(tt.slot, localOf(tt.slot, loc)); got != tt.free {
				t.Fatalf("on duty = %v, want %v", got, tt.free)
			}
		})
	}
}

// sqlOnDuty is store.technicianOnDutySQL's shift rule evaluated by hand, the way Postgres does:
// on the local wall-clock time of day (instant AT TIME ZONE tz)::time and local date of each end.
func sqlOnDuty(sh domain.TechnicianShift, slot Interval, loc *time.Location) bool {
	s, e := slot.Start.In(loc), slot.End.In(loc)
	clock := func(t time.Time) string { return t.Format("15:04:05") }
	pad := func(hhmm string) string { return hhmm + ":00" }
	if s.Format("2006-01-02") != e.Format("2006-01-02") || isoWeekdays[s.Weekday()] != sh.Weekday {
		return false
	}
	if pad(sh.StartTime) > clock(s) || pad(sh.EndTime) < clock(e) {
		return false
	}
	return sh.BreakStart == "" || clock(e) <= pad(sh.BreakStart) || clock(s) >= pad(sh.BreakEnd)
}

// On a clock-change day a shift boundary can fall in the skipped or the repeated hour, where
// Clock.On (time.Date) normalises the wall clock to some instant. Availability must still agree
// with the SQL on-duty rule, which compares wall clocks: otherwise it offers a slot booking
// (LockBookableTechnician) refuses, or hides one it accepts. America/New_York springs forward at
// 02:00 → 03:00 on 2026-03-08 and falls back 02:00 → 01:00 on 2026-11-01 (both Sundays);
// America/Santiago skips midnight on 2026-09-06 (00:00 → 01:00, a Sunday).
func TestOnDutyMatchesSQLRuleAcrossClockChanges(t *testing.T) {
	for _, tt := range []struct {
		zone   string
		day    Date
		shifts []domain.TechnicianShift
	}{
		{"America/New_York", Date{Year: 2026, Month: time.March, Day: 8}, []domain.TechnicianShift{
			{Weekday: 7, StartTime: "02:30", EndTime: "08:00"},
			{Weekday: 7, StartTime: "00:00", EndTime: "02:30"},
			{Weekday: 7, StartTime: "00:00", EndTime: "06:00", BreakStart: "02:00", BreakEnd: "03:00"},
			{Weekday: 7, StartTime: "00:00", EndTime: "06:00", BreakStart: "02:15", BreakEnd: "02:45"},
		}},
		{"America/New_York", Date{Year: 2026, Month: time.November, Day: 1}, []domain.TechnicianShift{
			{Weekday: 7, StartTime: "01:30", EndTime: "08:00"},
			{Weekday: 7, StartTime: "00:00", EndTime: "01:30"},
			{Weekday: 7, StartTime: "00:00", EndTime: "06:00", BreakStart: "01:00", BreakEnd: "01:30"},
		}},
		{"America/Santiago", Date{Year: 2026, Month: time.September, Day: 6}, []domain.TechnicianShift{
			{Weekday: 7, StartTime: "00:30", EndTime: "06:00"},
			{Weekday: 7, StartTime: "00:00", EndTime: "01:30"},
		}},
	} {
		loc, err := time.LoadLocation(tt.zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, sh := range tt.shifts {
			r, err := newRoster([]domain.TechnicianShift{sh}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, minutes := range []int{30, 60, 90} {
				for start := tt.day.Start(loc); start.Before(tt.day.Next(loc)); start = start.Add(SlotStep) {
					slot := Interval{Start: start, End: start.Add(time.Duration(minutes) * time.Minute)}
					if got, want := r.onDuty(slot, localOf(slot, loc)), sqlOnDuty(sh, slot, loc); got != want {
						t.Errorf("%s %s shift %s–%s break %q–%q, slot %s +%dm: Go on duty = %v, SQL = %v", tt.zone, tt.day,
							sh.StartTime, sh.EndTime, sh.BreakStart, sh.BreakEnd, start.In(loc).Format("15:04 MST"), minutes, got, want)
					}
				}
			}
		}
	}
}

func TestValidateShifts(t *testing.T) {
	ok := []domain.TechnicianShift{{Weekday: 1, StartTime: "08:00", EndTime: "16:00", BreakStart: "12:00", BreakEnd: "12:30"}}
	if err := validateShifts(ok); err != nil {
		t.Fatal(err)
	}
	bad := map[string][]domain.TechnicianShift{
		"weekday":        {{Weekday: 8, StartTime: "08:00", EndTime: "16:00"}},
		"twice":          {{Weekday: 1, StartTime: "08:00", EndTime: "16:00"}, {Weekday: 1, StartTime: "09:00", EndTime: "10:00"}},
		"end before":     {{Weekday: 1, StartTime: "16:00", EndTime: "08:00"}},
		"half break":     {{Weekday: 1, StartTime: "08:00", EndTime: "16:00", BreakStart: "12:00"}},
		"break outside":  {{Weekday: 1, StartTime: "08:00", EndTime: "16:00", BreakStart: "07:00", BreakEnd: "07:30"}},
		"bad clock":      {{Weekday: 1, StartTime: "8:00", EndTime: "16:00"}},
		"inverted break": {{Weekday: 1, StartTime: "08:00", EndTime: "16:00", BreakStart: "12:30", BreakEnd: "12:00"}},
	}
	for name, shifts := range bad {
		if err := validateShifts(shifts); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestArrivalWindow(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	day := Date{Year: 2026, Month: time.October, Day: 19}
	start := Clock{Hour: 9, Minute: 40}.On(day, loc)
	for _, tt := range []struct {
		window   int16
		from, to Clock
	}{
		{60, Clock{Hour: 9}, Clock{Hour: 10}},
		{30, Clock{Hour: 9, Minute: 30}, Clock{Hour: 10}},
		{15, Clock{Hour: 9, Minute: 30}, Clock{Hour: 9, Minute: 45}},
		{0, Clock{Hour: 9}, Clock{Hour: 10}}, // 0 = hourly
	} {
		from, to := ArrivalWindow(start, loc, tt.window)
		if !from.Equal(tt.from.On(day, loc)) || !to.Equal(tt.to.On(day, loc)) {
			t.Errorf("window %d: got %s–%s", tt.window, from.In(loc).Format("15:04"), to.In(loc).Format("15:04"))
		}
	}
}

// Every start on a day with an offset change falls in exactly one window, and windows never
// overlap — including Lord Howe's 30-minute shift, where an hourly window would straddle it.
func TestArrivalWindowsPartitionOffsetChangeDays(t *testing.T) {
	for _, tt := range []struct {
		zone string
		day  time.Time // local midnight-ish start of the scan, UTC instant
	}{
		{"Europe/London", time.Date(2026, time.October, 24, 22, 0, 0, 0, time.UTC)},      // clocks back
		{"Europe/London", time.Date(2026, time.March, 28, 23, 0, 0, 0, time.UTC)},        // clocks forward
		{"Australia/Lord_Howe", time.Date(2026, time.October, 3, 13, 0, 0, 0, time.UTC)}, // +10:30 -> +11
		{"Australia/Lord_Howe", time.Date(2026, time.April, 4, 13, 0, 0, 0, time.UTC)},   // +11 -> +10:30
	} {
		loc, err := time.LoadLocation(tt.zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range []int16{15, 30, 60} {
			windows := map[int64]time.Time{} // from -> to
			for at := tt.day; at.Before(tt.day.Add(26 * time.Hour)); at = at.Add(5 * time.Minute) {
				from, to := ArrivalWindow(at, loc, w)
				if at.Before(from) || !at.Before(to) {
					t.Fatalf("%s w=%d: %s outside its window %s–%s", tt.zone, w, at, from, to)
				}
				if prev, ok := windows[from.Unix()]; ok && !prev.Equal(to) {
					t.Fatalf("%s w=%d: window from %s ends at %s and %s", tt.zone, w, from, prev, to)
				}
				windows[from.Unix()] = to
			}
			type span struct{ from, to time.Time }
			spans := make([]span, 0, len(windows))
			for f, to := range windows {
				spans = append(spans, span{time.Unix(f, 0), to})
			}
			slices.SortFunc(spans, func(a, b span) int { return a.from.Compare(b.from) })
			for i := 1; i < len(spans); i++ {
				if spans[i].from.Before(spans[i-1].to) {
					t.Fatalf("%s w=%d: windows overlap: %s–%s and %s–%s", tt.zone, w,
						spans[i-1].from, spans[i-1].to, spans[i].from, spans[i].to)
				}
			}
		}
	}
}

// 2026-10-25 01:00–02:00 London happens twice (BST, then GMT). A start in the second 01:40 must
// stay in the second hour's window, not jump to the first.
func TestArrivalWindowRepeatedHour(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/London")
	first := time.Date(2026, time.October, 25, 0, 40, 0, 0, time.UTC)  // 01:40 BST
	second := time.Date(2026, time.October, 25, 1, 40, 0, 0, time.UTC) // 01:40 GMT
	for _, tt := range []struct {
		name   string
		start  time.Time
		window int16
		from   time.Time
	}{
		{"first 01:40, hourly", first, 60, time.Date(2026, time.October, 25, 0, 0, 0, 0, time.UTC)},
		{"second 01:40, hourly", second, 60, time.Date(2026, time.October, 25, 1, 0, 0, 0, time.UTC)},
		{"second 01:40, 15 min", second, 15, time.Date(2026, time.October, 25, 1, 30, 0, 0, time.UTC)},
	} {
		from, to := ArrivalWindow(tt.start, loc, tt.window)
		if !from.Equal(tt.from) || to.Sub(from) != time.Duration(tt.window)*time.Minute {
			t.Errorf("%s: got %s–%s, want from %s", tt.name, from.UTC(), to.UTC(), tt.from.UTC())
		}
		if tt.start.Before(from) || !tt.start.Before(to) {
			t.Errorf("%s: start %s outside its window %s–%s", tt.name, tt.start.UTC(), from.UTC(), to.UTC())
		}
	}
}
