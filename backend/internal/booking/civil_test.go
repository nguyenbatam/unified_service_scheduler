package booking

import (
	"testing"
	"time"
	_ "time/tzdata"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDateAddDays(t *testing.T) {
	for _, tc := range []struct {
		from Date
		n    int
		want Date
	}{
		{Date{2026, time.October, 17}, 1, Date{2026, time.October, 18}},
		{Date{2026, time.October, 31}, 1, Date{2026, time.November, 1}},
		{Date{2026, time.December, 31}, 1, Date{2027, time.January, 1}},
		{Date{2028, time.February, 28}, 1, Date{2028, time.February, 29}},
		{Date{2027, time.February, 28}, 1, Date{2027, time.March, 1}},
		{Date{2026, time.January, 1}, -1, Date{2025, time.December, 31}},
		{Date{2026, time.October, 17}, 0, Date{2026, time.October, 17}},
		{Date{2026, time.October, 17}, 31, Date{2026, time.November, 17}},
	} {
		if got := tc.from.AddDays(tc.n); got != tc.want {
			t.Errorf("%s + %d = %s, want %s", tc.from, tc.n, got, tc.want)
		}
	}
}

// Start is the first real instant of the day and Next the first of the following one, also where
// the clocks skip or repeat midnight, so every day is a non-empty [Start, Next) on that date.
func TestDateStartAndNextAcrossTransitions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		zone      string
		day       Date
		wantStart string // local wall clock of Start
		wantLen   time.Duration
	}{
		{"normal day", "Europe/London", Date{2026, time.October, 17}, "00:00", 24 * time.Hour},
		{"London spring forward at 01:00", "Europe/London", Date{2026, time.March, 29}, "00:00", 23 * time.Hour},
		{"London fall back at 02:00", "Europe/London", Date{2026, time.October, 25}, "00:00", 25 * time.Hour},
		{"Santiago skips midnight", "America/Santiago", Date{2024, time.September, 8}, "01:00", 23 * time.Hour},
		{"Santiago day before the skip", "America/Santiago", Date{2024, time.September, 7}, "00:00", 24 * time.Hour},
		{"Santiago repeats the last hour", "America/Santiago", Date{2024, time.April, 6}, "00:00", 25 * time.Hour},
		{"Amman repeats midnight", "Asia/Amman", Date{2021, time.October, 29}, "00:00", 25 * time.Hour},
		{"Amman day before the repeat", "Asia/Amman", Date{2021, time.October, 28}, "00:00", 24 * time.Hour},
		{"Asuncion skips midnight", "America/Asuncion", Date{2024, time.October, 6}, "01:00", 23 * time.Hour},
		{"Havana skips midnight", "America/Havana", Date{2024, time.March, 10}, "01:00", 23 * time.Hour},
		{"UTC", "UTC", Date{2024, time.March, 10}, "00:00", 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loc := mustLoad(t, tc.zone)
			start, next := tc.day.Start(loc), tc.day.Next(loc)
			if got := DateFrom(start, loc); got != tc.day {
				t.Fatalf("Start %s is on %s, want %s", start.In(loc), got, tc.day)
			}
			if got := start.In(loc).Format("15:04"); got != tc.wantStart {
				t.Errorf("Start wall clock %s, want %s", got, tc.wantStart)
			}
			if got := next.Sub(start); got != tc.wantLen {
				t.Errorf("day length %s, want %s", got, tc.wantLen)
			}
			if got := DateFrom(next, loc); got != tc.day.AddDays(1) {
				t.Errorf("Next %s is on %s, want %s", next.In(loc), got, tc.day.AddDays(1))
			}
			if before := DateFrom(start.Add(-time.Nanosecond), loc); before != tc.day.AddDays(-1) {
				t.Errorf("the instant before Start is on %s, want %s", before, tc.day.AddDays(-1))
			}
		})
	}
}

// A day-by-day scan (NextAvailable, the seed) must visit consecutive distinct days across a
// transition. Before the fix, Next on a day after a skipped midnight returned an instant on the
// same day, so DateFrom(day.Next(loc)) looped on it forever.
func TestDayScanVisitsConsecutiveDays(t *testing.T) {
	for _, tc := range []struct {
		zone  string
		first Date
	}{
		{"America/Santiago", Date{2024, time.September, 5}},
		{"America/Asuncion", Date{2024, time.October, 3}},
		{"America/Havana", Date{2024, time.March, 7}},
		{"Europe/London", Date{2026, time.March, 26}},
		{"Europe/London", Date{2026, time.October, 22}},
	} {
		t.Run(tc.zone+" "+tc.first.String(), func(t *testing.T) {
			loc := mustLoad(t, tc.zone)
			const days = 7
			for name, step := range map[string]func(Date) Date{
				"AddDays":      func(d Date) Date { return d.AddDays(1) },
				"DateFromNext": func(d Date) Date { return DateFrom(d.Next(loc), loc) },
			} {
				day := tc.first
				prevEnd := day.Start(loc)
				for i := range days {
					if want := tc.first.AddDays(i); day != want {
						t.Fatalf("%s: step %d visited %s, want %s", name, i, day, want)
					}
					start, end := day.Start(loc), day.Next(loc)
					if !start.Equal(prevEnd) || !end.After(start) {
						t.Fatalf("%s: %s = [%s, %s) does not follow the previous day ending %s",
							name, day, start.In(loc), end.In(loc), prevEnd.In(loc))
					}
					prevEnd = end
					day = step(day)
				}
			}
		})
	}
}
