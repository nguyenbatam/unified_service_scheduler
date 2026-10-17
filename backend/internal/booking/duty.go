package booking

import (
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// shiftClocks is one weekday's shift with its clocks parsed once, as wall-clock times of day.
type shiftClocks struct {
	start, end           time.Duration
	breakStart, breakEnd time.Duration
	hasBreak             bool
}

// roster is a technician's weekly pattern parsed once for a whole availability range, plus the
// absences overlapping it.
type roster struct {
	hasShifts bool
	byWeekday [8]*shiftClocks // ISO weekday 1–7; nil = not working that weekday
	absences  []Interval
}

// newRoster parses a technician's shifts (clocks are parsed once, not per day) and keeps absences.
func newRoster(shifts []domain.TechnicianShift, absences []store.Busy) (roster, error) {
	r := roster{hasShifts: len(shifts) > 0, absences: make([]Interval, len(absences))}
	for i, a := range absences {
		r.absences[i] = Interval{Start: a.Start, End: a.End}
	}
	for _, sh := range shifts {
		if sh.Weekday < 1 || sh.Weekday > 7 {
			continue
		}
		c := &shiftClocks{}
		var err error
		if c.start, err = parseTimeOfDay(sh.StartTime); err != nil {
			return roster{}, err
		}
		if c.end, err = parseTimeOfDay(sh.EndTime); err != nil {
			return roster{}, err
		}
		if sh.BreakStart != "" && sh.BreakEnd != "" {
			c.hasBreak = true
			if c.breakStart, err = parseTimeOfDay(sh.BreakStart); err != nil {
				return roster{}, err
			}
			if c.breakEnd, err = parseTimeOfDay(sh.BreakEnd); err != nil {
				return roster{}, err
			}
		}
		r.byWeekday[sh.Weekday] = c
	}
	return r, nil
}

// onDuty is the Go twin of store.technicianOnDutySQL (used by allocation and
// LockBookableTechnician); keep both in step. The technician works the whole of slot iff no
// absence overlaps it (instants) and either there is no shift pattern (opening hours apply) or the
// shift of the start's local weekday covers it without touching the break. Like the SQL, the shift
// test compares the local wall-clock times of day of the slot's two ends (local is
// localOf(slot, loc)) and requires both on the same local date, so on a clock-change day a shift
// boundary that falls in the skipped or repeated hour gives the same answer in Go and in Postgres.
func (r *roster) onDuty(slot Interval, local localWindow) bool {
	if busyAt(r.absences, slot) {
		return false
	}
	if !r.hasShifts {
		return true
	}
	sh := r.byWeekday[local.weekday]
	if sh == nil || !local.sameDay || local.start < sh.start || local.end > sh.end {
		return false
	}
	return !sh.hasBreak || local.end <= sh.breakStart || local.start >= sh.breakEnd
}
