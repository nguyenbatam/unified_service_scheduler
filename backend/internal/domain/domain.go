package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const (
	CodeInvalid      = "invalid"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeUnavailable  = "unavailable"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodePrecondition = "precondition_failed"
	// CodePreconditionRequired: a write that is not safe to retry blind (extend adds a delta) was
	// sent without If-Match (428).
	CodePreconditionRequired = "precondition_required"
	CodeRateLimited          = "rate_limited"
	CodeBusy                 = "busy" // lock or statement timeout: retry shortly (503)
	CodeInternal             = "internal"

	// SMALLINT codes — identical in Postgres and Go.
	StatusConfirmed  AppointmentStatus = 1
	StatusCheckedIn  AppointmentStatus = 2
	StatusInProgress AppointmentStatus = 3
	StatusCompleted  AppointmentStatus = 4
	StatusCancelled  AppointmentStatus = 5
	StatusNoShow     AppointmentStatus = 6

	RoleAdmin      Role = 1
	RoleDispatcher Role = 2
	RoleCustomer   Role = 3

	BayExpress    BayType = 1
	BayGeneral    BayType = 2
	BayDiagnostic BayType = 3
	BayAlignment  BayType = 4

	SkillLubrication TechnicianSkill = 1
	SkillBrakes      TechnicianSkill = 2
	SkillGeneral     TechnicianSkill = 3
)

type AppointmentStatus int16
type BayType int16
type TechnicianSkill int16
type Role int16

func (r Role) Valid() bool {
	return r == RoleAdmin || r == RoleDispatcher || r == RoleCustomer
}

func (r Role) Name() string {
	switch r {
	case RoleAdmin:
		return "admin"
	case RoleDispatcher:
		return "dispatcher"
	case RoleCustomer:
		return "customer"
	default:
		return "unknown"
	}
}

func (s AppointmentStatus) Name() string {
	switch s {
	case StatusConfirmed:
		return "confirmed"
	case StatusCheckedIn:
		return "checked_in"
	case StatusInProgress:
		return "in_progress"
	case StatusCompleted:
		return "completed"
	case StatusCancelled:
		return "cancelled"
	case StatusNoShow:
		return "no_show"
	default:
		return "unknown"
	}
}

func (b BayType) Valid() bool { return b >= BayExpress && b <= BayAlignment }

func (sk TechnicianSkill) Valid() bool { return sk >= SkillLubrication && sk <= SkillGeneral }

func (b BayType) Name() string {
	switch b {
	case BayExpress:
		return "express"
	case BayGeneral:
		return "general"
	case BayDiagnostic:
		return "diagnostic"
	case BayAlignment:
		return "alignment"
	default:
		return "unknown"
	}
}

func (sk TechnicianSkill) Name() string {
	switch sk {
	case SkillLubrication:
		return "lubrication"
	case SkillBrakes:
		return "brakes"
	case SkillGeneral:
		return "general"
	default:
		return "unknown"
	}
}

func (r Role) IsStaff() bool { return r == RoleAdmin || r == RoleDispatcher }

type User struct {
	ID           uuid.UUID `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	Role         Role      `json:"role"`                  // 1|2|3
	CustomerID   uuid.UUID `json:"customerId,omitzero"`   // set only for RoleCustomer
	DealershipID uuid.UUID `json:"dealershipId,omitzero"` // staff home dealership; zero = group admin
	PasswordHash string    `json:"-"`
	TokenVersion int       `json:"-"` // bumped on logout; tokens with an older version are rejected
}

// Actor is who performs a request. Authorization is decided from it in the service layer.
// UserID == uuid.Nil means the system itself (seed, background sweepers).
type Actor struct {
	UserID     uuid.UUID
	Role       Role
	CustomerID uuid.UUID
	// DealershipID scopes staff to one dealership; uuid.Nil is a group-wide admin (or a customer).
	DealershipID uuid.UUID
}

// SystemActor has staff rights and no user (created_by stays NULL).
var SystemActor = Actor{Role: RoleAdmin}

func (a Actor) IsStaff() bool { return a.Role.IsStaff() }

// CanAccessCustomer reports whether the actor may see/act on data owned by customerID.
func (a Actor) CanAccessCustomer(customerID uuid.UUID) bool {
	return a.IsStaff() || (a.CustomerID != uuid.Nil && a.CustomerID == customerID)
}

// CanAccessDealership reports whether a staff actor may work on dealershipID. Customers may book
// at any dealership, so for them it is always true (ownership is checked per vehicle/appointment).
func (a Actor) CanAccessDealership(dealershipID uuid.UUID) bool {
	return !a.IsStaff() || a.DealershipID == uuid.Nil || a.DealershipID == dealershipID
}

// CanAccessAppointment combines dealership scope (staff) and ownership (customers).
func (a Actor) CanAccessAppointment(appt Appointment) bool {
	return a.CanAccessDealership(appt.DealershipID) && a.CanAccessCustomer(appt.CustomerID)
}

// CreatedBy is the user to record on rows the actor creates (nil for the system).
func (a Actor) CreatedBy() *uuid.UUID {
	if a.UserID == uuid.Nil {
		return nil
	}
	id := a.UserID
	return &id
}

type TechnicianAdmin struct {
	ID           uuid.UUID         `json:"id"`
	DealershipID uuid.UUID         `json:"dealershipId"`
	Name         string            `json:"name"`
	SkillLevel   SkillLevel        `json:"skillLevel"`
	Active       bool              `json:"active"`
	Skills       []TechnicianSkill `json:"skills"`
	Version      int               `json:"version"`
}

// SkillLevel is a technician's grade, and the grade a service needs (technicians.skill_level,
// service_types.min_skill_level, appointments.min_skill_level): ordered, so a technician qualifies
// for a job at or above its level, and allocation prefers the lowest level that qualifies.
type SkillLevel int16

const (
	LevelApprentice SkillLevel = 1
	LevelTechnician SkillLevel = 2
	LevelSenior     SkillLevel = 3
	LevelMaster     SkillLevel = 4

	MinSkillLevel = LevelApprentice
	MaxSkillLevel = LevelMaster
)

func (l SkillLevel) Valid() bool { return l >= MinSkillLevel && l <= MaxSkillLevel }

func (l SkillLevel) Name() string {
	switch l {
	case LevelApprentice:
		return "apprentice"
	case LevelTechnician:
		return "technician"
	case LevelSenior:
		return "senior"
	case LevelMaster:
		return "master"
	default:
		return "unknown"
	}
}

type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

func NewError(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

func AsError(err error) (*Error, bool) {
	return errors.AsType[*Error](err)
}

// CheckIfMatch enforces an If-Match precondition: ifMatch 0 means the client sent none; any other
// value must equal the current version, else a 412 naming what changed (e.g. "appointment").
func CheckIfMatch(ifMatch, current int, what string) error {
	if ifMatch != 0 && ifMatch != current {
		return NewError(CodePrecondition,
			fmt.Sprintf("%s was changed by someone else (now version %d); reload and retry", what, current))
	}
	return nil
}

type Dealership struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Timezone   string    `json:"timezone"`
	OpenTime   string    `json:"openTime"`  // HH:MM from Postgres time
	CloseTime  string    `json:"closeTime"` // HH:MM from Postgres time
	LunchStart string    `json:"lunchStart,omitempty"`
	LunchEnd   string    `json:"lunchEnd,omitempty"`
	// StagingCapacity caps arrivals per StagingWindowMinutes (15/30/60) window; 0 = unlimited.
	StagingCapacity      int16  `json:"stagingCapacity"`
	StagingWindowMinutes int16  `json:"stagingWindowMinutes"`
	Policy               Policy `json:"policy"`
	Today                string `json:"today"` // YYYY-MM-DD civil date in dealership TZ
	Version              int    `json:"version"`
}

// Policy is a dealership's self-service and workshop rules (all minutes unless named otherwise).
type Policy struct {
	CustomerLeadMinutes         int16 `json:"customerLeadMinutes"`         // online bookings need this much notice
	CustomerChangeCutoffMinutes int16 `json:"customerChangeCutoffMinutes"` // online cancel/reschedule closes this long before start
	BookingHorizonDays          int16 `json:"bookingHorizonDays"`          // customers book at most this many days ahead
	NoShowGraceMinutes          int16 `json:"noShowGraceMinutes"`          // confirmed and not checked in this long after start → no-show
	ExpressMaxMinutes           int16 `json:"expressMaxMinutes"`           // waiters soft-upgrade to express only for jobs this short; 0 = never
}

type ServiceType struct {
	ID              uuid.UUID       `json:"id"`
	Code            string          `json:"code"`
	Name            string          `json:"name"`
	DurationMinutes int             `json:"durationMinutes"`
	RequiredSkill   TechnicianSkill `json:"requiredSkill"`
	RequiredBayType BayType         `json:"requiredBayType"`
	MinSkillLevel   SkillLevel      `json:"minSkillLevel"`
	Version         int             `json:"version"`
}

// Customer is a vehicle owner: self-registered (linked to a customer user) or a walk-in recorded by
// staff (no account). Phone and email are where notifications go ("" = none).
type Customer struct {
	ID    uuid.UUID `json:"id"`
	Name  string    `json:"name"`
	Phone string    `json:"phone"`
	Email string    `json:"email"`
}

type Vehicle struct {
	ID           uuid.UUID `json:"id"`
	CustomerID   uuid.UUID `json:"customerId"`
	CustomerName string    `json:"customerName"`
	VIN          string    `json:"vin"`
	Make         string    `json:"make"`
	Model        string    `json:"model"`
	Year         int       `json:"year"`
}

type Bay struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	BayType BayType   `json:"bayType"`
}

type Technician struct {
	ID         uuid.UUID         `json:"id"`
	Name       string            `json:"name"`
	SkillLevel SkillLevel        `json:"skillLevel,omitempty"`
	Skills     []TechnicianSkill `json:"skills,omitempty"`
	Load       int               `json:"-"` // booked minutes that day (holding statuses)
}

// HoldsResources reports whether the status still occupies bay + technician + vehicle.
func (s AppointmentStatus) HoldsResources() bool {
	switch s {
	case StatusConfirmed, StatusCheckedIn, StatusInProgress:
		return true
	default:
		return false
	}
}

type Appointment struct {
	ID              uuid.UUID         `json:"id"`
	DealershipID    uuid.UUID         `json:"dealershipId"`
	DealershipName  string            `json:"dealershipName"`
	CustomerID      uuid.UUID         `json:"customerId"`
	CustomerName    string            `json:"customerName"`
	VehicleID       uuid.UUID         `json:"vehicleId"`
	VehicleLabel    string            `json:"vehicleLabel"`
	VIN             string            `json:"vin"`
	ServiceTypeID   uuid.UUID         `json:"serviceTypeId"`   // primary (first) job line
	ServiceName     string            `json:"serviceName"`     // every line, joined with " + "
	DurationMinutes int               `json:"durationMinutes"` // booked labour time (sum of lines), not the catalog's current value
	Lines           []AppointmentLine `json:"lines"`
	RequiredBayType BayType           `json:"requiredBayType"`
	RequiredSkills  []TechnicianSkill `json:"requiredSkills"`
	MinSkillLevel   SkillLevel        `json:"minSkillLevel"`
	ServiceBayID    uuid.UUID         `json:"serviceBayId"`
	ServiceBayName  string            `json:"serviceBayName"`
	ServiceBayType  BayType           `json:"serviceBayType"`
	TechnicianID    uuid.UUID         `json:"technicianId"`
	TechnicianName  string            `json:"technicianName"`
	StartAt         time.Time         `json:"startAt"`
	EndAt           time.Time         `json:"endAt"`
	Status          AppointmentStatus `json:"status"`
	IsWaiter        bool              `json:"isWaiter"`
	Version         int               `json:"version"`
	CheckedInAt     *time.Time        `json:"checkedInAt,omitempty"`
	StartedAt       *time.Time        `json:"startedAt,omitempty"`
	CompletedAt     *time.Time        `json:"completedAt,omitempty"`
	OverrunAt       *time.Time        `json:"overrunAt,omitempty"` // ran past end_at and could not be auto-extended
	IdempotencyKey  string            `json:"-"`
	// IdempotencyFingerprint identifies the original booking request, so a replay matches it even
	// after the appointment was moved.
	IdempotencyFingerprint string    `json:"-"`
	CreatedAt              time.Time `json:"createdAt,omitzero"`
	UpdatedAt              time.Time `json:"updatedAt,omitzero"`
}

// AppointmentLine is one booked operation, snapshotted at booking time.
type AppointmentLine struct {
	LineNo          int16     `json:"lineNo"`
	ServiceTypeID   uuid.UUID `json:"serviceTypeId"`
	Code            string    `json:"code"`
	Name            string    `json:"name"`
	DurationMinutes int       `json:"durationMinutes"`
}

// AppointmentChange is one audit-trail entry (every change bumps the appointment version).
type AppointmentChange struct {
	Version      int               `json:"version"`
	EventType    EventType         `json:"eventType"`
	ActorID      *uuid.UUID        `json:"actorId,omitempty"` // nil = system (sweepers, seed)
	At           time.Time         `json:"at"`
	Status       AppointmentStatus `json:"status"`
	StartAt      time.Time         `json:"startAt"`
	EndAt        time.Time         `json:"endAt"`
	ServiceBayID uuid.UUID         `json:"serviceBayId"`
	TechnicianID uuid.UUID         `json:"technicianId"`
}

type Slot struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	Available       bool      `json:"available"`
	FreeBays        int       `json:"freeBays"`
	FreeTechnicians int       `json:"freeTechnicians"`
	Reason          string    `json:"reason,omitempty"`
	// Recommended: booking this start leaves no new idle gap (booking/fit.go), a hint to steer by.
	Recommended bool `json:"recommended"`
}

type Availability struct {
	Date            string `json:"date"`
	Timezone        string `json:"timezone"`
	DurationMinutes int    `json:"durationMinutes"`
	Closed          bool   `json:"closed,omitempty"` // dealership closed that day (weekday or closure); Slots is empty
	ClosedReason    string `json:"closedReason,omitempty"`
	// Reason says why an open day offers no start at all: "day_too_short" (that day's hours are
	// shorter than the job). Per-slot reasons are on Slots.
	Reason string `json:"reason,omitempty"`
	Slots  []Slot `json:"slots"`
}

type Schedule struct {
	Date         string        `json:"date"`
	Timezone     string        `json:"timezone"`
	OpenTime     string        `json:"openTime"`             // that day's hours (defaults when closed)
	CloseTime    string        `json:"closeTime"`            // that day's hours (defaults when closed)
	LunchStart   string        `json:"lunchStart,omitempty"` // that day's lunch (none when closed or no lunch)
	LunchEnd     string        `json:"lunchEnd,omitempty"`
	Closed       bool          `json:"closed,omitempty"`
	ClosedReason string        `json:"closedReason,omitempty"`
	Bays         []Bay         `json:"bays"`
	Technicians  []Technician  `json:"technicians"` // active technicians (reassign targets)
	Appointments []Appointment `json:"appointments"`
}

// NextSlot is one bookable start found by the multi-day "first available" search.
type NextSlot struct {
	Date            string    `json:"date"`
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	FreeBays        int       `json:"freeBays"`
	FreeTechnicians int       `json:"freeTechnicians"`
	Recommended     bool      `json:"recommended"` // see Slot.Recommended
}

type NextAvailability struct {
	Timezone        string     `json:"timezone"`
	DurationMinutes int        `json:"durationMinutes"`
	SearchedFrom    string     `json:"searchedFrom"`
	SearchedTo      string     `json:"searchedTo"`
	Slots           []NextSlot `json:"slots"`
}

// TechnicianShift is a weekly working pattern on one ISO weekday (local dealership time).
type TechnicianShift struct {
	Weekday    int16  `json:"weekday"`
	StartTime  string `json:"startTime"`
	EndTime    string `json:"endTime"`
	BreakStart string `json:"breakStart,omitempty"`
	BreakEnd   string `json:"breakEnd,omitempty"`
}

// TechnicianAbsence blocks a technician (leave, training) over [StartsAt, EndsAt).
type TechnicianAbsence struct {
	ID       uuid.UUID `json:"id"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
	Reason   string    `json:"reason,omitempty"`
}

// TechnicianRoster is the admin view of one technician's shifts and upcoming absences. Version is
// the technician row version: roster changes bump it.
type TechnicianRoster struct {
	TechnicianID uuid.UUID           `json:"technicianId"`
	Version      int                 `json:"version"`
	Shifts       []TechnicianShift   `json:"shifts"` // empty = works the dealership's opening hours
	Absences     []TechnicianAbsence `json:"absences"`
}

// WeekdayHours overrides a dealership's default hours on one ISO weekday (1 = Monday … 7 = Sunday).
// Closed days carry no times; open days need open/close and may have a lunch pair.
type WeekdayHours struct {
	Weekday    int16  `json:"weekday"`
	Closed     bool   `json:"closed"`
	OpenTime   string `json:"openTime,omitempty"`
	CloseTime  string `json:"closeTime,omitempty"`
	LunchStart string `json:"lunchStart,omitempty"`
	LunchEnd   string `json:"lunchEnd,omitempty"`
}

// Closure is a dated whole-day closure (public holiday, stock take) in dealership local time.
type Closure struct {
	Date   string `json:"date"` // YYYY-MM-DD
	Reason string `json:"reason,omitempty"`
}

// DealershipCalendar is the admin view of weekly overrides and upcoming closures. Version is the
// dealership row version: calendar changes bump it, so If-Match covers hours and closures too.
type DealershipCalendar struct {
	DealershipID uuid.UUID      `json:"dealershipId"`
	Version      int            `json:"version"`
	Weekly       []WeekdayHours `json:"weekly"`
	Closures     []Closure      `json:"closures"`
}
