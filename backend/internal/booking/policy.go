package booking

import (
	"fmt"
	"sync"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Self-service vs staff rules come from each dealership's domain.Policy (lead time, change cutoff,
// booking horizon, no-show grace, express-lane limit). Only the extend bounds are global.
const (
	// ExtendStep / MaxExtend bound how a dispatcher may stretch an overrunning job.
	ExtendStep = 15 * time.Minute
	MaxExtend  = 4 * time.Hour
)

// Availability reasons for slots blocked by time rules.
const (
	ReasonPast          = "past"
	ReasonLeadTime      = "lead_time"
	ReasonBeyondHorizon = "beyond_horizon"
	ReasonVehicleBusy   = "vehicle_busy"
)

var locations sync.Map // tz name → *time.Location

// loadLocation caches parsed tzdata; time.LoadLocation re-reads and parses the zone every call.
func loadLocation(name string) (*time.Location, error) {
	if v, ok := locations.Load(name); ok {
		if loc, ok := v.(*time.Location); ok {
			return loc, nil
		}
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	locations.Store(name, loc)
	return loc, nil
}

func minutes(n int16) time.Duration { return time.Duration(n) * time.Minute }

// earliestStart is the first instant actor may book: now for staff, now+lead time for customers.
func earliestStart(actor domain.Actor, now time.Time, p domain.Policy) time.Time {
	if actor.IsStaff() {
		return now
	}
	return now.Add(minutes(p.CustomerLeadMinutes))
}

// latestStart is how far ahead actor may book: customers up to the horizon, staff without limit.
func latestStart(actor domain.Actor, now time.Time, p domain.Policy) (time.Time, bool) {
	if actor.IsStaff() {
		return time.Time{}, false
	}
	return now.AddDate(0, 0, int(p.BookingHorizonDays)), true
}

// tooEarly explains why start is not bookable for actor at this time, or returns "" when it is.
func tooEarly(actor domain.Actor, start, now time.Time, p domain.Policy) string {
	switch {
	case start.Before(now):
		return ReasonPast
	case start.Before(earliestStart(actor, now, p)):
		return ReasonLeadTime
	}
	if latest, ok := latestStart(actor, now, p); ok && start.After(latest) {
		return ReasonBeyondHorizon
	}
	return ""
}

func tooEarlyError(reason string, p domain.Policy) error {
	switch reason {
	case ReasonPast:
		return domain.NewError(domain.CodeUnavailable, "slot has already started or is in the past")
	case ReasonBeyondHorizon:
		return domain.NewError(domain.CodeUnavailable,
			fmt.Sprintf("online bookings open %d days ahead; call the dealership for later dates", p.BookingHorizonDays))
	}
	return domain.NewError(domain.CodeUnavailable,
		fmt.Sprintf("online bookings need at least %d minutes notice; call the dealership for sooner", p.CustomerLeadMinutes))
}

// customerMayChange enforces the online change cutoff for customers; staff are not limited.
func customerMayChange(actor domain.Actor, appt domain.Appointment, now time.Time, p domain.Policy) error {
	if actor.IsStaff() {
		return nil
	}
	if now.After(appt.StartAt.Add(-minutes(p.CustomerChangeCutoffMinutes))) {
		return domain.NewError(domain.CodeUnavailable,
			fmt.Sprintf("online changes close %d minutes before the appointment; please call the dealership", p.CustomerChangeCutoffMinutes))
	}
	return nil
}

func notFoundAppointment() error {
	return domain.NewError(domain.CodeNotFound, "appointment not found")
}

func notFoundVehicle() error {
	return domain.NewError(domain.CodeNotFound, "vehicle not found")
}

func staffOnly() error {
	return domain.NewError(domain.CodeForbidden, "dealership staff role required")
}

func otherDealership() error {
	return domain.NewError(domain.CodeForbidden, "your account is limited to another dealership")
}
