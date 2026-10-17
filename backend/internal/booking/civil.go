package booking

import (
	"fmt"
	"time"
)

// Date is a calendar day with no timezone. Instants are built only in a dealership location.
type Date struct {
	Year  int
	Month time.Month
	Day   int
}

func ParseDate(value string) (Date, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return Date{}, fmt.Errorf("date must be YYYY-MM-DD")
	}
	year, month, day := parsed.Date()
	return Date{Year: year, Month: month, Day: day}, nil
}

func DateFrom(instant time.Time, loc *time.Location) Date {
	year, month, day := instant.In(loc).Date()
	return Date{Year: year, Month: month, Day: day}
}

func Today(loc *time.Location, now time.Time) Date {
	return DateFrom(now, loc)
}

func (d Date) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

// Before reports whether d is an earlier calendar day than other.
func (d Date) Before(other Date) bool {
	if d.Year != other.Year {
		return d.Year < other.Year
	}
	if d.Month != other.Month {
		return d.Month < other.Month
	}
	return d.Day < other.Day
}

// AddDays is the calendar day n days after d (n may be negative). It is pure civil arithmetic, so
// stepping day by day never depends on how long a day is in any timezone.
func (d Date) AddDays(n int) Date {
	year, month, day := time.Date(d.Year, d.Month, d.Day+n, 12, 0, 0, 0, time.UTC).Date()
	return Date{Year: year, Month: month, Day: day}
}

// Start is the first instant of d in loc. That is local midnight, except where the clocks skip
// midnight (e.g. America/Santiago, America/Asuncion, America/Havana spring forward at 00:00):
// time.Date then normalises 00:00 to 23:00 of the day before, so Start is instead the end of that
// zone period, the first wall-clock instant that is on d (01:00 there). Where the clocks fall back
// across midnight (e.g. Asia/Amman 2021-10-29, 01:00 +03 → 00:00 +02), midnight happens twice and
// time.Date picks the later one, so Start steps back to the earlier.
func (d Date) Start(loc *time.Location) time.Time {
	t := time.Date(d.Year, d.Month, d.Day, 0, 0, 0, 0, loc)
	if DateFrom(t, loc) != d {
		if _, end := t.ZoneBounds(); !end.IsZero() {
			t = end
		}
		return t
	}
	if zoneStart, _ := t.ZoneBounds(); !zoneStart.IsZero() && zoneStart.After(t.Add(-24*time.Hour)) {
		if before := zoneStart.Add(-time.Nanosecond); DateFrom(before, loc) == d {
			_, prev := before.Zone()
			_, cur := t.Zone()
			t = t.Add(time.Duration(cur-prev) * time.Second)
		}
	}
	return t
}

// Next is the end of d in loc (exclusive): the Start of the following calendar day.
func (d Date) Next(loc *time.Location) time.Time {
	return d.AddDays(1).Start(loc)
}

type Clock struct {
	Hour   int
	Minute int
}

func ParseClock(value string) (Clock, error) {
	parsed, err := time.Parse("15:04", value)
	if err != nil {
		return Clock{}, fmt.Errorf("clock must be HH:MM")
	}
	return Clock{Hour: parsed.Hour(), Minute: parsed.Minute()}, nil
}

func (c Clock) On(day Date, loc *time.Location) time.Time {
	return time.Date(day.Year, day.Month, day.Day, c.Hour, c.Minute, 0, 0, loc)
}

// sinceMidnight is c as a wall-clock time of day.
func (c Clock) sinceMidnight() time.Duration {
	return time.Duration(c.Hour)*time.Hour + time.Duration(c.Minute)*time.Minute
}

// clockOf is the wall clock of a time of day (whole minutes).
func clockOf(d time.Duration) Clock {
	return Clock{Hour: int(d / time.Hour), Minute: int(d % time.Hour / time.Minute)}
}

// parseTimeOfDay parses HH:MM as a wall-clock time of day.
func parseTimeOfDay(value string) (time.Duration, error) {
	c, err := ParseClock(value)
	if err != nil {
		return 0, err
	}
	return c.sinceMidnight(), nil
}

// timeOfDay is t's wall-clock time of day in t's location, what Postgres reads as
// (instant AT TIME ZONE tz)::time.
func timeOfDay(t time.Time) time.Duration {
	h, m, s := t.Clock()
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second +
		time.Duration(t.Nanosecond())
}

// localWindow is a window as the SQL rules see it (store.technicianOnDutySQL, Tx.refuseStranded):
// the local wall-clock times of day of its two ends, the start's ISO weekday, and whether the end
// is on the start's local date. Rules written on it compare wall clocks, never instants built with
// Clock.On, so a clock that falls in a skipped or repeated hour (time.Date normalises those)
// gives the same answer in Go and in Postgres.
type localWindow struct {
	start, end time.Duration
	weekday    int16
	sameDay    bool
}

func localOf(w Interval, loc *time.Location) localWindow {
	s, e := w.Start.In(loc), w.End.In(loc)
	sy, sm, sd := s.Date()
	ey, em, ed := e.Date()
	return localWindow{
		start: timeOfDay(s), end: timeOfDay(e),
		weekday: isoWeekdays[s.Weekday()],
		sameDay: sy == ey && sm == em && sd == ed,
	}
}
