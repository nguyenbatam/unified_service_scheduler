package booking

import "time"

const SlotStep = 30 * time.Minute

type Interval struct {
	Start time.Time
	End   time.Time
}

// Overlaps reports whether two half-open intervals [start, end) share any instant.
func Overlaps(a, b Interval) bool {
	return a.Start.Before(b.End) && b.Start.Before(a.End)
}

// GenerateSlots returns bookable windows whose end is still inside closing time.
func GenerateSlots(day Date, loc *time.Location, open, close Clock, duration, step time.Duration) []Interval {
	if loc == nil || duration <= 0 || step <= 0 {
		return nil
	}
	openAt := open.On(day, loc)
	closeAt := close.On(day, loc)
	if !closeAt.After(openAt) {
		return nil
	}
	slots := make([]Interval, 0, 24)
	for start := openAt; !start.Add(duration).After(closeAt); start = start.Add(step) {
		slots = append(slots, Interval{Start: start, End: start.Add(duration)})
	}
	return slots
}

func MatchingSlot(slots []Interval, start time.Time) (Interval, bool) {
	// Compare wall-clock minutes so RFC3339 clients with/without seconds still match.
	want := start.Unix() / 60
	for _, slot := range slots {
		if slot.Start.Unix()/60 == want {
			return slot, true
		}
	}
	return Interval{}, false
}

func busyAt(intervals []Interval, slot Interval) bool {
	for _, interval := range intervals {
		if Overlaps(interval, slot) {
			return true
		}
	}
	return false
}

// Classify is a slot's availability and, when it is not, the reason: whether a bay and a
// technician can be had (free, or freed by a one-move repair).
func Classify(bay, technician bool) (bool, string) {
	switch {
	case bay && technician:
		return true, ""
	case !bay && !technician:
		return false, "no_resources"
	case !bay:
		return false, "no_bay"
	default:
		return false, "no_technician"
	}
}

// ArrivalWindow bounds the staging window [from, from+window) in loc containing start. Windows are
// aligned to local midnight (15 → :00/:15/:30/:45, 60 → whole hours); window <= 0 means 60.
// The start is truncated as an instant, not rebuilt from the wall clock: in the hour repeated
// when clocks go back, time.Date picks one of the two occurrences (which one is unspecified and
// differs by zone), so rebuilding could move a window of the other occurrence by an hour. The window is also clipped to the UTC-offset period containing start, so
// windows never overlap across an offset change that is not on a window boundary (e.g. Lord Howe's
// 30-minute shift); a clipped window is just shorter.
func ArrivalWindow(start time.Time, loc *time.Location, windowMinutes int16) (time.Time, time.Time) {
	w := int(windowMinutes)
	if w <= 0 {
		w = 60
	}
	local := start.In(loc)
	into := time.Duration(local.Minute()%w)*time.Minute +
		time.Duration(local.Second())*time.Second + time.Duration(local.Nanosecond())
	from := start.Add(-into)
	to := from.Add(time.Duration(w) * time.Minute)
	zoneStart, zoneEnd := local.ZoneBounds()
	if !zoneStart.IsZero() && from.Before(zoneStart) {
		from = zoneStart
	}
	if !zoneEnd.IsZero() && to.After(zoneEnd) {
		to = zoneEnd
	}
	return from, to
}
