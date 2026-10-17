package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// DayHours are the opening rules that apply to one civil day at a dealership: its default hours,
// replaced by a weekday override, and closed outright by a weekday closing day or a dated closure.
type DayHours struct {
	Closed     bool
	Reason     string // why it is closed: the closure reason, or the weekday name
	OpenTime   string // HH:MM
	CloseTime  string // HH:MM
	LunchStart string // HH:MM, "" = no lunch
	LunchEnd   string
}

// ISOWeekday is 1 = Monday … 7 = Sunday (the dealership_hours.weekday convention).
func (d Date) ISOWeekday() int16 {
	return isoWeekdays[d.weekday()]
}

// isoWeekdays maps time.Weekday (Sunday = 0) to ISO 8601 (Monday = 1 … Sunday = 7).
var isoWeekdays = [7]int16{7, 1, 2, 3, 4, 5, 6}

func (d Date) weekday() time.Weekday {
	return time.Date(d.Year, d.Month, d.Day, 12, 0, 0, 0, time.UTC).Weekday()
}

// ResolveDay applies the calendar rules for day on top of the dealership defaults.
func ResolveDay(dealer domain.Dealership, day Date, rules store.DayRules) DayHours {
	h := DayHours{
		OpenTime: dealer.OpenTime, CloseTime: dealer.CloseTime,
		LunchStart: dealer.LunchStart, LunchEnd: dealer.LunchEnd,
	}
	if o := rules.Override; o != nil {
		if o.Closed {
			h.Closed, h.Reason = true, day.weekday().String()
		} else {
			h.OpenTime, h.CloseTime, h.LunchStart, h.LunchEnd = o.OpenTime, o.CloseTime, o.LunchStart, o.LunchEnd
		}
	}
	if c := rules.Closure; c != nil {
		h.Closed, h.Reason = true, c.Reason
		if h.Reason == "" {
			h.Reason = "closure"
		}
	}
	return h
}

// dayHoursFor reads day's calendar rules in the booking TX (after the dealership FOR SHARE).
func dayHoursFor(ctx context.Context, tx *store.Tx, dealer domain.Dealership, day Date) (DayHours, error) {
	rules, err := tx.DayRules(ctx, dealer.ID, day.String(), day.ISOWeekday())
	if err != nil {
		return DayHours{}, err
	}
	return ResolveDay(dealer, day, rules), nil
}

func closedError(day Date, h DayHours) error {
	return domain.NewError(domain.CodeUnavailable,
		fmt.Sprintf("the dealership is closed on %s (%s)", day.String(), h.Reason))
}

// dayClocks are an open day's opening and lunch clocks, parsed once, as wall-clock times of day.
type dayClocks struct {
	openClock, closeClock Clock // the slot grid runs from openClock (GenerateSlots)
	open, close           time.Duration
	lunchStart, lunchEnd  time.Duration
	hasLunch              bool
}

func (h DayHours) clocks() (dayClocks, error) {
	var c dayClocks
	var err error
	if c.openClock, err = ParseClock(h.OpenTime); err != nil {
		return dayClocks{}, err
	}
	if c.closeClock, err = ParseClock(h.CloseTime); err != nil {
		return dayClocks{}, err
	}
	c.open, c.close = c.openClock.sinceMidnight(), c.closeClock.sinceMidnight()
	if h.LunchStart != "" && h.LunchEnd != "" {
		c.hasLunch = true
		if c.lunchStart, err = parseTimeOfDay(h.LunchStart); err != nil {
			return dayClocks{}, err
		}
		if c.lunchEnd, err = parseTimeOfDay(h.LunchEnd); err != nil {
			return dayClocks{}, err
		}
	}
	return c, nil
}

// withinHours and crossesLunch are the Go twins of Tx.refuseStranded's hour and lunch rules: they
// compare the window's local wall clocks (see localWindow), so a window Go books is never one the
// stranded check calls outside the hours, and a booked window Go re-checks (reinstate, extend)
// gets the same answer as that check.
func (c dayClocks) withinHours(w localWindow) bool {
	return w.sameDay && w.start >= c.open && w.end <= c.close
}

func (c dayClocks) crossesLunch(w localWindow) bool {
	return c.hasLunch && w.start < c.lunchEnd && w.end > c.lunchStart
}

// stretch bounds the bookable stretch of day around slot (a window planSlot accepted, so inside
// the hours and clear of lunch): from opening, or the end of lunch when the slot is after it, to
// closing, or the start of lunch when the slot is before it. Allocation measures idle gaps inside
// it (store.Slot.Lo/Hi). Clamped to the slot itself, so a wall clock time.Date normalises across
// a DST change can never put an edge inside it.
func (c dayClocks) stretch(slot Interval, day Date, loc *time.Location) (lo, hi time.Time) {
	lo, hi = c.openClock.On(day, loc), c.closeClock.On(day, loc)
	if c.hasLunch {
		w := localOf(slot, loc)
		if w.start >= c.lunchEnd {
			lo = clockOf(c.lunchEnd).On(day, loc)
		} else if w.end <= c.lunchStart {
			hi = clockOf(c.lunchStart).On(day, loc)
		}
	}
	if lo.After(slot.Start) {
		lo = slot.Start
	}
	if hi.Before(slot.End) {
		hi = slot.End
	}
	return lo, hi
}

// LongestWindow is the longest uninterrupted bookable stretch of an open day: open→close without
// lunch, otherwise the longer of open→lunch and lunch→close. A job longer than this can never be
// booked that day, because slots may not overlap lunch.
func (h DayHours) LongestWindow() (time.Duration, error) {
	if h.Closed {
		return 0, nil
	}
	clocks := []string{h.OpenTime, h.CloseTime}
	if h.LunchStart != "" && h.LunchEnd != "" {
		clocks = []string{h.OpenTime, h.LunchStart, h.LunchEnd, h.CloseTime}
	}
	minutes := make([]int, len(clocks))
	for i, v := range clocks {
		c, err := ParseClock(v)
		if err != nil {
			return 0, err
		}
		minutes[i] = c.Hour*60 + c.Minute
	}
	longest := 0
	for i := 0; i+1 < len(minutes); i += 2 {
		longest = max(longest, minutes[i+1]-minutes[i])
	}
	return time.Duration(longest) * time.Minute, nil
}

// Calendar is the admin view of a dealership's weekday overrides and upcoming closures.
func (s *Service) Calendar(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID) (domain.DealershipCalendar, error) {
	if err := adminFor(actor, dealershipID); err != nil {
		return domain.DealershipCalendar{}, err
	}
	return s.store.GetCalendar(ctx, s.now(), dealershipID)
}

// ReplaceWeeklyHours sets the weekday overrides (weekdays left out use the default hours).
func (s *Service) ReplaceWeeklyHours(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID, weekly []domain.WeekdayHours, ifMatch int) (domain.DealershipCalendar, error) {
	if err := adminFor(actor, dealershipID); err != nil {
		return domain.DealershipCalendar{}, err
	}
	if err := validateWeekly(weekly); err != nil {
		return domain.DealershipCalendar{}, err
	}
	return s.store.ReplaceWeeklyHours(ctx, s.now(), dealershipID, weekly, ifMatch)
}

func (s *Service) AddClosure(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID, c domain.Closure, ifMatch int) (domain.DealershipCalendar, error) {
	if err := adminFor(actor, dealershipID); err != nil {
		return domain.DealershipCalendar{}, err
	}
	if _, err := ParseDate(c.Date); err != nil {
		return domain.DealershipCalendar{}, domain.NewError(domain.CodeInvalid, "date must be YYYY-MM-DD")
	}
	if len(c.Reason) > 120 {
		return domain.DealershipCalendar{}, domain.NewError(domain.CodeInvalid, "reason is at most 120 characters")
	}
	return s.store.AddClosure(ctx, s.now(), dealershipID, c, ifMatch)
}

func (s *Service) DeleteClosure(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID, day Date, ifMatch int) (domain.DealershipCalendar, error) {
	if err := adminFor(actor, dealershipID); err != nil {
		return domain.DealershipCalendar{}, err
	}
	return s.store.DeleteClosure(ctx, s.now(), dealershipID, day.String(), ifMatch)
}

func validateWeekly(weekly []domain.WeekdayHours) error {
	seen := map[int16]bool{}
	for _, h := range weekly {
		if h.Weekday < 1 || h.Weekday > 7 {
			return domain.NewError(domain.CodeInvalid, "weekday must be 1 (Monday) – 7 (Sunday)")
		}
		if seen[h.Weekday] {
			return domain.NewError(domain.CodeInvalid, fmt.Sprintf("weekday %d is listed twice", h.Weekday))
		}
		seen[h.Weekday] = true
		if h.Closed {
			if h.OpenTime != "" || h.CloseTime != "" || h.LunchStart != "" || h.LunchEnd != "" {
				return domain.NewError(domain.CodeInvalid, "a closed weekday has no opening or lunch times")
			}
			continue
		}
		for _, v := range []string{h.OpenTime, h.CloseTime} {
			if !validHHMM(v) {
				return domain.NewError(domain.CodeInvalid, "openTime and closeTime must be HH:MM on an open weekday")
			}
		}
		if (h.LunchStart == "") != (h.LunchEnd == "") {
			return domain.NewError(domain.CodeInvalid, "lunchStart and lunchEnd must be set together")
		}
		if h.LunchStart != "" && (!validHHMM(h.LunchStart) || !validHHMM(h.LunchEnd)) {
			return domain.NewError(domain.CodeInvalid, "lunchStart/lunchEnd must be HH:MM")
		}
	}
	return nil
}

func validHHMM(v string) bool {
	_, err := ParseClock(v)
	return err == nil && len(v) == 5
}

// checkDurationFits rejects a service duration that no open day at any dealership can hold
// without crossing lunch (it would show up as unbookable everywhere). Service types are shared
// across dealerships, so fitting at one is enough. A guardrail, not an invariant: later hour
// changes are not re-checked against service durations.
func (s *Service) checkDurationFits(ctx context.Context, minutes int) error {
	dealers, err := s.store.ListDealerships(ctx)
	if err != nil {
		return err
	}
	weekly, err := s.store.WeeklyHoursByDealership(ctx)
	if err != nil {
		return err
	}
	if len(dealers) == 0 {
		return nil
	}
	var longest time.Duration
	for _, dealer := range dealers {
		overrides := map[int16]domain.WeekdayHours{}
		for _, h := range weekly[dealer.ID] {
			overrides[h.Weekday] = h
		}
		w, err := longestWeeklyWindow(dealer, overrides)
		if err != nil {
			return err
		}
		longest = max(longest, w)
	}
	if time.Duration(minutes)*time.Minute > longest {
		return domain.NewError(domain.CodeInvalid, fmt.Sprintf(
			"durationMinutes %d exceeds the longest uninterrupted opening window (%d min) at any dealership",
			minutes, int(longest.Minutes())))
	}
	return nil
}

// fitsSomeWeekday is 400 when a job of durationMinutes is longer than any opening window of the
// dealership's week (availability, and a booking refused as outside the hours: both say why the
// same way).
func fitsSomeWeekday(dealer domain.Dealership, overrides map[int16]domain.WeekdayHours, durationMinutes int) error {
	longest, err := longestWeeklyWindow(dealer, overrides)
	if err != nil {
		return err
	}
	if time.Duration(durationMinutes)*time.Minute > longest {
		return domain.NewError(domain.CodeInvalid, fmt.Sprintf(
			"the job (%d min) is longer than any opening window at this dealership (%d min)",
			durationMinutes, int(longest.Minutes())))
	}
	return nil
}

// longestWeeklyWindow is the longest uninterrupted bookable stretch over the dealership's week
// (defaults replaced by weekday overrides; dated closures only remove days, so they cannot make
// it longer). A job longer than this can never be booked there.
func longestWeeklyWindow(dealer domain.Dealership, overrides map[int16]domain.WeekdayHours) (time.Duration, error) {
	var longest time.Duration
	for wd := int16(1); wd <= 7; wd++ {
		var rules store.DayRules
		if h, ok := overrides[wd]; ok {
			rules.Override = &h
		}
		w, err := ResolveDay(dealer, Date{}, rules).LongestWindow()
		if err != nil {
			return 0, err
		}
		longest = max(longest, w)
	}
	return longest, nil
}
