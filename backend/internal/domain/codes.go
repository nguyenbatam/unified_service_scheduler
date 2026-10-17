package domain

// Enums of the event and notification pipeline. Like the codes in domain.go they are SMALLINT
// values identical in Postgres (001_schema.sql), Go, and web/src/types.ts, starting at 1 so the
// zero value is never a valid code. JSON carries the numbers (REST, Kafka, WebSocket), like every
// other enum of the API; Name is for logs and for the notification webhook, whose receiver is a
// third-party gateway without these tables.

// EventType is an event's type: 1–13 are outbox events (outbox_events.event_type;
// appointment_history.event_type holds the appointment events 1–11), 14–15 are WebSocket message
// types that are never stored.
type EventType int16

const (
	EventAppointmentCreated     EventType = 1
	EventAppointmentRescheduled EventType = 2
	EventAppointmentCancelled   EventType = 3
	EventAppointmentCheckedIn   EventType = 4
	EventAppointmentStarted     EventType = 5
	EventAppointmentCompleted   EventType = 6
	EventAppointmentNoShow      EventType = 7
	EventAppointmentReinstated  EventType = 8
	EventAppointmentReassigned  EventType = 9
	EventAppointmentExtended    EventType = 10
	EventAppointmentOverrunning EventType = 11
	// EventSessionRevoked tells every instance to close the user's open sockets (logout everywhere).
	EventSessionRevoked EventType = 12
	// EventConfigUpdated: a dealership, service type, or technician changed.
	EventConfigUpdated EventType = 13

	// EventAvailabilityChanged is what customers receive for other people's bookings and for
	// technician changes: enough to refresh slot availability, nothing about who booked or which
	// technician changed. WebSocket only.
	EventAvailabilityChanged EventType = 14
	// EventConnected is the hub's first frame on a socket: the session is live. WebSocket only.
	EventConnected EventType = 15
)

var eventTypeNames = []string{
	EventAppointmentCreated:     "APPOINTMENT_CREATED",
	EventAppointmentRescheduled: "APPOINTMENT_RESCHEDULED",
	EventAppointmentCancelled:   "APPOINTMENT_CANCELLED",
	EventAppointmentCheckedIn:   "APPOINTMENT_CHECKED_IN",
	EventAppointmentStarted:     "APPOINTMENT_STARTED",
	EventAppointmentCompleted:   "APPOINTMENT_COMPLETED",
	EventAppointmentNoShow:      "APPOINTMENT_NO_SHOW",
	EventAppointmentReinstated:  "APPOINTMENT_REINSTATED",
	EventAppointmentReassigned:  "APPOINTMENT_REASSIGNED",
	EventAppointmentExtended:    "APPOINTMENT_EXTENDED",
	EventAppointmentOverrunning: "APPOINTMENT_OVERRUNNING",
	EventSessionRevoked:         "SESSION_REVOKED",
	EventConfigUpdated:          "CONFIG_UPDATED",
	EventAvailabilityChanged:    "AVAILABILITY_CHANGED",
	EventConnected:              "CONNECTED",
}

func (t EventType) Valid() bool { return validCode(t, eventTypeNames) }

// IsAppointment reports whether t is an appointment event (the ones appointment_history records).
func (t EventType) IsAppointment() bool {
	return t >= EventAppointmentCreated && t <= EventAppointmentOverrunning
}

// IsStored reports whether t can be an outbox event (the others are WebSocket message types).
func (t EventType) IsStored() bool { return t >= EventAppointmentCreated && t <= EventConfigUpdated }

func (t EventType) Name() string   { return codeName(t, eventTypeNames) }
func (t EventType) String() string { return t.Name() }

// AggregateType is what an outbox event is about (outbox_events.aggregate_type), and the entity a
// WebSocket config notice names.
type AggregateType int16

const (
	AggregateAppointment AggregateType = 1
	AggregateDealership  AggregateType = 2
	AggregateServiceType AggregateType = 3
	AggregateTechnician  AggregateType = 4
	AggregateUser        AggregateType = 5
)

var aggregateTypeNames = []string{
	AggregateAppointment: "appointment",
	AggregateDealership:  "dealership",
	AggregateServiceType: "service_type",
	AggregateTechnician:  "technician",
	AggregateUser:        "user",
}

func (t AggregateType) Valid() bool    { return validCode(t, aggregateTypeNames) }
func (t AggregateType) Name() string   { return codeName(t, aggregateTypeNames) }
func (t AggregateType) String() string { return t.Name() }

// NotificationKind is what a customer notification says (notifications.kind).
type NotificationKind int16

const (
	KindConfirmation NotificationKind = 1
	KindRescheduled  NotificationKind = 2
	KindCancelled    NotificationKind = 3
	KindNoShow       NotificationKind = 4
	KindReminder24h  NotificationKind = 5
	KindReminder2h   NotificationKind = 6
)

var notificationKindNames = []string{
	KindConfirmation: "confirmation",
	KindRescheduled:  "rescheduled",
	KindCancelled:    "cancelled",
	KindNoShow:       "no_show",
	KindReminder24h:  "reminder_24h",
	KindReminder2h:   "reminder_2h",
}

func (k NotificationKind) Valid() bool { return validCode(k, notificationKindNames) }

// IsReminder reports whether k is a reminder (deduplicated and skipped by slot, never by version).
func (k NotificationKind) IsReminder() bool { return k == KindReminder24h || k == KindReminder2h }

func (k NotificationKind) Name() string   { return codeName(k, notificationKindNames) }
func (k NotificationKind) String() string { return k.Name() }

// NotificationChannel is how a notification reaches the customer (notifications.channel).
type NotificationChannel int16

const (
	ChannelEmail NotificationChannel = 1
	ChannelSMS   NotificationChannel = 2
)

var notificationChannelNames = []string{
	ChannelEmail: "email",
	ChannelSMS:   "sms",
}

func (c NotificationChannel) Valid() bool    { return validCode(c, notificationChannelNames) }
func (c NotificationChannel) Name() string   { return codeName(c, notificationChannelNames) }
func (c NotificationChannel) String() string { return c.Name() }

// OutboxStatus is an outbox row's delivery state (outbox_events.status).
type OutboxStatus int16

const (
	OutboxPending    OutboxStatus = 1
	OutboxProcessing OutboxStatus = 2
	OutboxPublished  OutboxStatus = 3
	// OutboxFailed: poisoned after repeated content rejections; an operator requeues it.
	OutboxFailed OutboxStatus = 4
)

var outboxStatusNames = []string{
	OutboxPending:    "pending",
	OutboxProcessing: "processing",
	OutboxPublished:  "published",
	OutboxFailed:     "failed",
}

func (s OutboxStatus) Name() string   { return codeName(s, outboxStatusNames) }
func (s OutboxStatus) String() string { return s.Name() }

// NotificationStatus is a notification's delivery state (notifications.status).
type NotificationStatus int16

const (
	NotificationPending NotificationStatus = 1
	NotificationSending NotificationStatus = 2
	NotificationSent    NotificationStatus = 3
	NotificationFailed  NotificationStatus = 4
	// NotificationSkipped: stale before it went out (the appointment moved, a newer notice).
	NotificationSkipped NotificationStatus = 5
)

var notificationStatusNames = []string{
	NotificationPending: "pending",
	NotificationSending: "sending",
	NotificationSent:    "sent",
	NotificationFailed:  "failed",
	NotificationSkipped: "skipped",
}

func (s NotificationStatus) Name() string   { return codeName(s, notificationStatusNames) }
func (s NotificationStatus) String() string { return s.Name() }

// code is any of the enums above; names is indexed by code ("" at index 0, never a valid code).
type code interface{ ~int16 }

func validCode[T code](v T, names []string) bool {
	return v > 0 && int(v) < len(names) && names[v] != ""
}

func codeName[T code](v T, names []string) string {
	if validCode(v, names) {
		return names[v]
	}
	return "unknown"
}
