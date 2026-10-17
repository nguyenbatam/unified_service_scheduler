package booking

import (
	"slices"
	"testing"
	"time"
)

func TestGenerateSlotsRespectsClosingTime(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	day := Date{Year: 2026, Month: time.June, Day: 16}
	open := Clock{Hour: 8}
	close := Clock{Hour: 17}

	halfHour := GenerateSlots(day, loc, open, close, 30*time.Minute, SlotStep)
	if len(halfHour) != 18 {
		t.Fatalf("30-minute service slots = %d, want 18", len(halfHour))
	}
	if got := halfHour[0].Start.Format("15:04"); got != "08:00" {
		t.Fatalf("first slot = %s", got)
	}
	if got := halfHour[len(halfHour)-1].Start.Format("15:04"); got != "16:30" {
		t.Fatalf("last 30-minute slot = %s", got)
	}

	major := GenerateSlots(day, loc, open, close, 180*time.Minute, SlotStep)
	if len(major) != 13 {
		t.Fatalf("180-minute service slots = %d, want 13", len(major))
	}
	if got := major[len(major)-1].Start.Format("15:04"); got != "14:00" {
		t.Fatalf("last 180-minute slot = %s", got)
	}
}

func TestOverlapsIsHalfOpen(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	day := Date{Year: 2026, Month: time.June, Day: 16}
	first := Interval{Start: Clock{8, 0}.On(day, loc), End: Clock{8, 30}.On(day, loc)}
	next := Interval{Start: Clock{8, 30}.On(day, loc), End: Clock{9, 0}.On(day, loc)}
	if Overlaps(first, next) {
		t.Fatal("back-to-back intervals must not overlap")
	}
	overlap := Interval{Start: Clock{8, 15}.On(day, loc), End: Clock{8, 45}.On(day, loc)}
	if !Overlaps(first, overlap) {
		t.Fatal("partial overlap must be detected")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		bay  bool
		tech bool
		ok   bool
		why  string
	}{
		{true, true, true, ""},
		{false, false, false, "no_resources"},
		{false, true, false, "no_bay"},
		{true, false, false, "no_technician"},
	}
	for _, tc := range cases {
		ok, why := Classify(tc.bay, tc.tech)
		if ok != tc.ok || why != tc.why {
			t.Fatalf("Classify(%t,%t) = %v %q, want %v %q", tc.bay, tc.tech, ok, why, tc.ok, tc.why)
		}
	}
}

// holdList.overlapping narrows sorted holds by binary search: for every slot in the window, the run
// it returns must say busy exactly when the full list does — including long holds that started
// the day before, holds that end exactly at from, and holds that start exactly at to.
func TestHoldListOverlappingKeepsEveryOverlap(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	day := Date{Year: 2026, Month: time.June, Day: 16}
	at := func(h, m int) time.Time { return Clock{Hour: h, Minute: m}.On(day, loc) }
	holds := []Interval{
		{Start: at(12, 0), End: at(13, 0)},                                         // unsorted on purpose: newHoldList sorts
		{Start: at(0, 0).Add(-20 * time.Hour), End: at(0, 0).Add(-19 * time.Hour)}, // day before, ended
		{Start: at(0, 0).Add(-10 * time.Hour), End: at(9, 0)},                      // overnight into the day
		{Start: at(7, 0), End: at(8, 0)},                                           // ends exactly at from
		{Start: at(8, 0), End: at(9, 0)},
		{Start: at(8, 0), End: at(8, 30)},
		{Start: at(16, 30), End: at(17, 0)},
		{Start: at(17, 0), End: at(18, 0)}, // starts exactly at to
		{Start: at(19, 0), End: at(20, 0)},
	}
	all := slices.Clone(holds)
	l := newHoldList(holds, nil)
	for _, w := range []Interval{
		{Start: at(8, 0), End: at(17, 0)},
		{Start: at(12, 0), End: at(12, 30)},
		{Start: at(20, 0), End: at(21, 0)},
		{Start: at(0, 0).Add(-48 * time.Hour), End: at(0, 0).Add(-47 * time.Hour)},
	} {
		run := l.overlapping(w.Start, w.End)
		for start := w.Start; start.Before(w.End); start = start.Add(15 * time.Minute) {
			for _, length := range []time.Duration{15 * time.Minute, time.Hour} {
				end := start.Add(length)
				if end.After(w.End) {
					end = w.End
				}
				slot := Interval{Start: start, End: end}
				if got, want := busyAt(run, slot), busyAt(all, slot); got != want {
					t.Fatalf("window %s–%s, slot %s–%s: busy = %v, want %v (run %v)",
						w.Start.Format("15:04"), w.End.Format("15:04"), slot.Start.Format("15:04"), slot.End.Format("15:04"), got, want, run)
				}
			}
		}
	}
	if got := len(l.overlapping(at(8, 0), at(17, 0))); got != 6 {
		t.Fatalf("run for the day has %d holds, want the 6 starting within the longest hold's span of it", got)
	}
}

func TestMatchingSlotIgnoresSeconds(t *testing.T) {
	loc := mustLoc(t, "Europe/London")
	day := Date{Year: 2026, Month: time.June, Day: 16}
	slots := GenerateSlots(day, loc, Clock{Hour: 8}, Clock{Hour: 17}, 30*time.Minute, SlotStep)
	start := Clock{Hour: 9}.On(day, loc).Add(15 * time.Second)
	got, ok := MatchingSlot(slots, start)
	if !ok || got.Start.Hour() != 9 {
		t.Fatalf("matching=%v ok=%v", got, ok)
	}
}

func TestParseDateDoesNotShiftCalendarDay(t *testing.T) {
	day, err := ParseDate("2026-10-16")
	if err != nil {
		t.Fatal(err)
	}
	loc := mustLoc(t, "America/Los_Angeles")
	start := Clock{Hour: 8}.On(day, loc)
	if got := DateFrom(start, loc); got != day {
		t.Fatalf("calendar day shifted to %s", got)
	}
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestOnSlotGridIsAnchoredAtOpeningTime(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	day := Date{Year: 2026, Month: time.November, Day: 2}
	hours := DayHours{OpenTime: "08:15", CloseTime: "17:00"}
	at := func(h, m, s int) time.Time { return time.Date(2026, time.November, 2, h, m, s, 0, loc) }
	tests := []struct {
		name  string
		start time.Time
		want  bool
	}{
		{"opening time", at(8, 15, 0), true},
		{"after closing, on grid", at(18, 45, 0), true},
		{"before opening, on grid", at(6, 45, 0), true},
		{"seconds ignored", at(9, 15, 30), true},
		{"off grid", at(9, 0, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := onSlotGrid(hours, day, loc, tt.start); got != tt.want {
				t.Fatalf("onSlotGrid(%s) = %v, want %v", tt.start.Format(time.Kitchen), got, tt.want)
			}
		})
	}
}
