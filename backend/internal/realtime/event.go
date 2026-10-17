package realtime

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// EventKind routes an Event: what it is about, for the hubs and the notifier. Like every enum on
// Kafka it travels as its number.
type EventKind int16

const (
	KindAppointment EventKind = 1
	KindConfig      EventKind = 2
	// KindSession is a control event (never forwarded): close the sockets of UserID whose session
	// predates TokenVersion.
	KindSession EventKind = 3
)

func (k EventKind) String() string {
	switch k {
	case KindAppointment:
		return "appointment"
	case KindConfig:
		return "config"
	case KindSession:
		return "session"
	default:
		return "unknown"
	}
}

// Event is the cross-instance envelope carried on Kafka. Routing fields decide who may see it.
// Browsers only ever receive a Notice; Payload (full aggregate, with PII) stays on the back end.
type Event struct {
	ID           uuid.UUID        `json:"id,omitzero"` // outbox row id: consumers dedupe on it
	Type         domain.EventType `json:"type"`
	InstanceID   string           `json:"instanceId,omitempty"`
	Kind         EventKind        `json:"kind"`
	DealershipID uuid.UUID        `json:"dealershipId,omitzero"`
	CustomerID   uuid.UUID        `json:"customerId,omitzero"`
	UserID       uuid.UUID        `json:"userId,omitzero"` // KindSession target
	// TokenVersion (KindSession) is the user's token_version after the revocation: sockets opened
	// with an older version are closed, newer ones survive a late or replayed event. 0 = unknown:
	// close every socket of the user.
	TokenVersion int                `json:"tokenVersion,omitempty"`
	Appointment  *AppointmentNotice `json:"appointment,omitempty"`
	Config       *ConfigNotice      `json:"config,omitempty"`
	Payload      json.RawMessage    `json:"payload,omitempty"`
}

// AppointmentNotice is the thin, PII-free view sent to staff and to the owning customer.
// Clients compare Version to ignore stale or duplicate events (delivery is at-least-once).
type AppointmentNotice struct {
	ID           uuid.UUID                `json:"id"`
	DealershipID uuid.UUID                `json:"dealershipId"`
	Status       domain.AppointmentStatus `json:"status"`
	Version      int                      `json:"version"`
	StartAt      time.Time                `json:"startAt"`
	EndAt        time.Time                `json:"endAt"`
}

// ConfigNotice tells clients which catalog entity changed so they can reload. A technician
// (skills, shifts, absences) notice is staff-only, routed by the technician's dealership; customers
// get an anonymous, window-less AVAILABILITY_CHANGED instead. Dealerships and service types are the
// public catalog every client lists.
type ConfigNotice struct {
	Entity  domain.AggregateType `json:"entity"`
	ID      uuid.UUID            `json:"id"`
	Version int                  `json:"version"`
}

// AvailabilityNotice is the anonymous ping for customers who do not own the appointment. The
// window is the appointment's; a technician change (roster, skills) has none (omitted): it may
// change any day of that dealership.
type AvailabilityNotice struct {
	DealershipID uuid.UUID `json:"dealershipId"`
	StartAt      time.Time `json:"startAt,omitzero"`
	EndAt        time.Time `json:"endAt,omitzero"`
}

// Message is the browser wire format; Type is a domain.EventType code (the appointment and config
// events, 14 AVAILABILITY_CHANGED, 15 CONNECTED).
type Message struct {
	Type       domain.EventType `json:"type"`
	InstanceID string           `json:"instanceId,omitempty"`
	Payload    any              `json:"payload,omitempty"`
}

// Session is the authenticated identity bound to one WebSocket connection.
type Session struct {
	UserID       uuid.UUID
	Role         domain.Role
	CustomerID   uuid.UUID
	TokenVersion int // re-validated periodically; a logout elsewhere bumps it
	// SessionID is the refresh family (sid) the socket was opened for; the periodic re-check
	// closes it once that family is revoked (a new login replacing it, logout). Zero = none.
	SessionID    uuid.UUID
	ExpiresAt    time.Time // socket is closed when the login session ends; zero = never
	DealershipID uuid.UUID // initial subscription; zero = every dealership (staff)
	// Scope is the staff member's home dealership (users.dealership_id): appointment events of
	// other dealerships are never delivered, whatever the subscription. Zero = group-wide.
	Scope uuid.UUID
}

type delivery int

const (
	deliverNone delivery = iota
	deliverNotice
	deliverAvailability
)

// capacityEvents change what is bookable, so other customers need an availability refresh.
var capacityEvents = map[domain.EventType]bool{
	domain.EventAppointmentCreated:     true,
	domain.EventAppointmentCancelled:   true,
	domain.EventAppointmentRescheduled: true,
	domain.EventAppointmentCompleted:   true,
	domain.EventAppointmentNoShow:      true,
	domain.EventAppointmentExtended:    true,
	domain.EventAppointmentReinstated:  true, // a no-show holds its bay/technician again
	domain.EventAppointmentReassigned:  true, // frees one technician/bay and takes another
	domain.EventAppointmentStarted:     true, // an early start moves the hold to an earlier window
	domain.EventAppointmentOverrunning: true, // flagged overrun: its bay and technician stay held until it is completed
}

// route decides what one subscriber may see of ev.
//
// Staff: the scope (home dealership) always applies, then the subscription filter. Customers: the
// owner gets every notice of their own appointment whatever dealership they are subscribed to
// (the booking form subscribes to the dealership being browsed, which need not be the one where
// they already hold a booking); the subscription only filters the anonymous AVAILABILITY_CHANGED
// sent for other people's bookings.
func route(ev Event, s Session, subscribed uuid.UUID) delivery {
	switch ev.Kind {
	case KindConfig:
		return routeConfig(ev, s, subscribed)
	case KindAppointment:
	default:
		return deliverNone
	}
	otherDealership := subscribed != uuid.Nil && ev.DealershipID != uuid.Nil && subscribed != ev.DealershipID
	switch {
	case s.Role.IsStaff():
		if (s.Scope != uuid.Nil && ev.DealershipID != s.Scope) || otherDealership {
			return deliverNone
		}
		return deliverNotice
	case s.CustomerID != uuid.Nil && s.CustomerID == ev.CustomerID:
		return deliverNotice
	case otherDealership || !capacityEvents[ev.Type]:
		return deliverNone
	}
	return deliverAvailability
}

// routeConfig decides who sees a CONFIG_UPDATED. Dealership-scoped staff never see another
// dealership's config. Dealerships and service types are the public catalog (GET /dealerships,
// /service-types), which clients load and list whole, so everyone else gets them whatever their
// subscription; a dealership's hours and policy are what customers need to stay current.
// Technician changes (skills, roster) are filtered by subscription like appointment events: staff
// get the notice, customers only an anonymous availability ping (a roster or skill change moves
// what is bookable, but which technician changed is staff business); one whose dealership is
// unknown (zero) reaches group-wide staff only.
func routeConfig(ev Event, s Session, subscribed uuid.UUID) delivery {
	technician := ev.Config != nil && ev.Config.Entity == domain.AggregateTechnician
	switch {
	case s.Role.IsStaff() && s.Scope != uuid.Nil && ev.DealershipID != s.Scope && (technician || ev.DealershipID != uuid.Nil):
		return deliverNone
	case !technician:
		return deliverNotice
	case subscribed != uuid.Nil && ev.DealershipID != uuid.Nil && subscribed != ev.DealershipID:
		return deliverNone
	case s.Role.IsStaff():
		return deliverNotice
	case ev.DealershipID == uuid.Nil:
		return deliverNone
	}
	return deliverAvailability
}

func noticeMessage(ev Event) Message {
	msg := Message{Type: ev.Type, InstanceID: ev.InstanceID}
	switch {
	case ev.Appointment != nil:
		msg.Payload = ev.Appointment
	case ev.Config != nil:
		msg.Payload = ev.Config
	}
	return msg
}

func availabilityMessage(ev Event) Message {
	msg := Message{Type: domain.EventAvailabilityChanged, InstanceID: ev.InstanceID}
	switch {
	case ev.Appointment != nil:
		msg.Payload = AvailabilityNotice{
			DealershipID: ev.Appointment.DealershipID,
			StartAt:      ev.Appointment.StartAt,
			EndAt:        ev.Appointment.EndAt,
		}
	case ev.Config != nil:
		// A technician change: the dealership only (no window, no technician).
		msg.Payload = AvailabilityNotice{DealershipID: ev.DealershipID}
	}
	return msg
}
