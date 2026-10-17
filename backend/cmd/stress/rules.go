package main

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The booking rules the oracle and the SQL checks judge the server by, restated here from the
// documented rules (docs/system-design.md, Assumptions 1, 4 and 11; backend/AGENTS.md invariants
// 1, 10, 11; the express spill, bayFits) instead of imported from internal/booking or internal/store. A rule shared with the
// server agrees with the server even when both are wrong; written twice, a slip in either shows up
// as an oracle mismatch. Only the data types (internal/domain) are shared: they are the API
// contract, not logic.

// jobRequirement is what one appointment of a job mix needs: one bay of bayType (or express for a
// waiter's soft upgrade) and one technician holding every skill at minLevel or above, both for
// minutes back to back.
type jobRequirement struct {
	minutes  int
	bayType  domain.BayType
	skills   []domain.TechnicianSkill // distinct, ascending
	minLevel int16
}

// bayRank orders the bay kinds by what they can host: an express job fits a general bay's work
// list, and a specialty bay (diagnostic or alignment) is the most specific; one appointment takes
// the most specific kind any of its lines needs.
func bayRank(t domain.BayType) (int, error) {
	switch t {
	case domain.BayExpress:
		return 0, nil
	case domain.BayGeneral:
		return 1, nil
	case domain.BayDiagnostic, domain.BayAlignment:
		return 2, nil
	}
	return 0, fmt.Errorf("unknown bay type %d", t)
}

// combineLines is the documented job-line rule (Assumption 1, invariant 1): 1–5 lines done back to
// back by one technician in one bay; the window is the summed labour time; the bay is the most
// specific kind any line needs (express < general < one specialty; a diagnostic and an alignment
// line cannot share a bay); the technician needs every line's skill at the highest line level
// (never below level 1).
func combineLines(services []domain.ServiceType) (jobRequirement, error) {
	if len(services) == 0 || len(services) > 5 {
		return jobRequirement{}, fmt.Errorf("a job has 1–5 lines, got %d", len(services))
	}
	r := jobRequirement{bayType: domain.BayExpress, minLevel: 1}
	best := 0
	var specialty domain.BayType
	for _, s := range services {
		if s.DurationMinutes <= 0 {
			return jobRequirement{}, fmt.Errorf("service %s has no labour time", s.Code)
		}
		r.minutes += s.DurationMinutes
		r.minLevel = max(r.minLevel, int16(s.MinSkillLevel))
		if !slices.Contains(r.skills, s.RequiredSkill) {
			r.skills = append(r.skills, s.RequiredSkill)
		}
		rank, err := bayRank(s.RequiredBayType)
		if err != nil {
			return jobRequirement{}, fmt.Errorf("service %s: %w", s.Code, err)
		}
		if rank == 2 {
			if specialty != 0 && specialty != s.RequiredBayType {
				return jobRequirement{}, errors.New("diagnostic and alignment lines cannot share one bay")
			}
			specialty = s.RequiredBayType
		}
		if rank > best {
			best, r.bayType = rank, s.RequiredBayType
		}
	}
	if specialty != 0 {
		r.bayType = specialty
	}
	slices.Sort(r.skills)
	return r, nil
}

// softUpgrade is Assumption 4: a waiter's general-bay job may take an express bay (only as the
// fallback, which the allocation-quality checks judge) when it is no longer than the dealership's
// express limit; 0 disables the quick lane for general work.
func softUpgrade(r jobRequirement, waiter bool, expressMax int) bool {
	return waiter && r.bayType == domain.BayGeneral && expressMax > 0 && r.minutes <= expressMax
}

// expressSpill is the mirror rule of the soft upgrade: a quick-lane (express) job may take a
// general bay, for any customer, but only as the fallback when no express bay is free (which the
// allocation-quality checks judge): an oil change is not refused while a general bay sits idle.
func expressSpill(r jobRequirement, t domain.BayType) bool {
	return r.bayType == domain.BayExpress && t == domain.BayGeneral
}

// bayFits reports whether a bay of type t may host r (soft: the soft upgrade applies; the express
// spill always does).
func bayFits(r jobRequirement, soft bool, t domain.BayType) bool {
	return t == r.bayType || (soft && r.bayType == domain.BayGeneral && t == domain.BayExpress) || expressSpill(r, t)
}

// qualified reports whether a technician of level with skills may do r.
func qualified(r jobRequirement, level int16, skills map[domain.TechnicianSkill]bool) bool {
	if level < r.minLevel {
		return false
	}
	for _, sk := range r.skills {
		if !skills[sk] {
			return false
		}
	}
	return true
}

// spareSkills counts a technician's skills beyond what r needs.
func spareSkills(r jobRequirement, skills map[domain.TechnicianSkill]bool) int {
	n := len(skills)
	for _, sk := range r.skills {
		if skills[sk] {
			n--
		}
	}
	return n
}

// clockMinutes is a local wall-clock time of day in minutes since midnight.
type clockMinutes int

// parseClockMinutes parses "HH:MM" (Postgres time rendered by to_char, or the API's form).
func parseClockMinutes(s string) (clockMinutes, error) {
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, fmt.Errorf("clock %q", s)
	}
	return clockMinutes(h*60 + m), nil
}

// slotStep is the documented slot grid: a start every 30 minutes from opening, stepped as elapsed
// time (backend rule 8; restated here, never the engine's booking.SlotStep).
const slotStep = 30 * time.Minute

// slotMinutes is slotStep in minutes.
const slotMinutes = int(slotStep / time.Minute)

// on is the instant of wall clock c on day in loc, as time.Date builds it. For a clock inside a
// skipped or repeated hour time.Date only promises "correct in one of the two zones involved",
// and which one it picks differs by zone and direction (New York 2026-03-08 02:30 becomes 01:30
// EST; Sydney 2026-04-05 02:30 is the second occurrence): unspecified. The oracle and the engine
// both place opening, closing and lunch with time.Date the same way, so they agree on such a day;
// the tests keep their own wall clocks out of the skipped and repeated hours.
func (c clockMinutes) on(day booking.Date, loc *time.Location) time.Time {
	return time.Date(day.Year, day.Month, day.Day, int(c)/60, int(c)%60, 0, 0, loc)
}

// localDay is day in loc from local midnight to the next one, built here with time.Date (the
// stress shops never open at midnight, so a zone that skips midnight changes nothing they hold).
func localDay(day booking.Date, loc *time.Location) (from, to time.Time) {
	return time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, loc), time.Date(day.Year, day.Month, day.Day+1, 0, 0, 0, 0, loc)
}

// dayBounds are generous bounds around day in loc for loading rows (two hours of slack on each
// side of the local midnights: whatever overlaps the day is inside, whatever the zone does).
func dayBounds(day booking.Date, loc *time.Location) (from, to time.Time) {
	from = time.Date(day.Year, day.Month, day.Day, 0, 0, 0, 0, loc).Add(-2 * time.Hour)
	to = time.Date(day.Year, day.Month, day.Day+1, 0, 0, 0, 0, loc).Add(2 * time.Hour)
	return from, to
}

// arrivalWindowStart is the start of the staging window holding start: windows of windowMinutes
// (15, 30 or 60; <= 0 means 60) aligned to the local hour, cut from the instant itself (so the two
// occurrences of a repeated hour are two windows). Offsets that change on a whole hour (every
// stress zone) keep that alignment.
func arrivalWindowStart(start time.Time, loc *time.Location, windowMinutes int) time.Time {
	w := windowMinutes
	if w <= 0 {
		w = 60
	}
	l := start.In(loc)
	return start.Add(-(time.Duration(l.Minute()%w)*time.Minute + time.Duration(l.Second())*time.Second + time.Duration(l.Nanosecond())))
}

// wallClock is t's local time of day in minutes (seconds dropped: the grid is 30 minutes).
func wallClock(t time.Time) clockMinutes { return clockMinutes(t.Hour()*60 + t.Minute()) }

// isoWeekday is 1 = Monday … 7 = Sunday.
func isoWeekday(t time.Time) int {
	if wd := int(t.Weekday()); wd != 0 {
		return wd
	}
	return 7
}

// shiftDef is one weekday of a technician's pattern (local wall clock).
type shiftDef struct {
	start, end           clockMinutes
	breakStart, breakEnd clockMinutes
	hasBreak             bool
}

// timeWindow is a half-open instant range [start, end).
type timeWindow struct{ start, end time.Time }

func (a timeWindow) overlaps(b timeWindow) bool {
	return a.start.Before(b.end) && b.start.Before(a.end)
}

// techRoster is a technician's duty data: the weekly pattern (empty = works the opening hours)
// and dated absences.
type techRoster struct {
	shifts   map[int]shiftDef // ISO weekday → shift
	absences []timeWindow
}

// onDuty is Assumption 11 for the window [start, end): no absence touches it; and either the
// technician has no shift pattern (works whatever the opening hours allow, which the slot grid
// already enforces), or the window lies on one local date whose weekday has a shift that covers it
// from start to end and the window stays clear of that shift's break. Times of day are compared
// as local wall-clock values, never as instants built from them.
func (r techRoster) onDuty(w timeWindow, loc *time.Location) bool {
	for _, a := range r.absences {
		if a.overlaps(w) {
			return false
		}
	}
	if len(r.shifts) == 0 {
		return true
	}
	ls, le := w.start.In(loc), w.end.In(loc)
	y1, m1, d1 := ls.Date()
	y2, m2, d2 := le.Date()
	if y1 != y2 || m1 != m2 || d1 != d2 {
		return false
	}
	sh, ok := r.shifts[isoWeekday(ls)]
	if !ok {
		return false
	}
	from, to := wallClock(ls), wallClock(le)
	if from < sh.start || to > sh.end {
		return false
	}
	return !sh.hasBreak || to <= sh.breakStart || from >= sh.breakEnd
}

// onDutySQL is the same rule as a SQL predicate for technician alias t, dealership timezone
// expression tz, and window expressions start and end (timestamptz). It is written from the rule,
// not from the server's query: plain comparisons on local wall-clock values instead of ranges.
func onDutySQL(t, tz, start, end string) string {
	return fmt.Sprintf(`(
		NOT EXISTS (
			SELECT 1 FROM technician_absences ab
			WHERE ab.technician_id = %[1]s.id AND ab.starts_at < %[4]s AND %[3]s < ab.ends_at)
		AND (
			NOT EXISTS (SELECT 1 FROM technician_shifts p WHERE p.technician_id = %[1]s.id)
			OR EXISTS (
				SELECT 1
				FROM technician_shifts p
				CROSS JOIN LATERAL (SELECT %[3]s AT TIME ZONE %[2]s AS ls, %[4]s AT TIME ZONE %[2]s AS le) w
				WHERE p.technician_id = %[1]s.id
				  AND w.ls::date = w.le::date
				  AND p.weekday = extract(isodow FROM w.ls)::int
				  AND w.ls::time >= p.start_time AND w.le::time <= p.end_time
				  AND NOT (p.break_start IS NOT NULL AND w.ls::time < p.break_end AND p.break_start < w.le::time))))`,
		t, tz, start, end)
}
