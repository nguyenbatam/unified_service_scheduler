package store

import (
	"fmt"
	"strconv"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Enum codes in query text. Every enum-like column is SMALLINT from 1 (001_schema.sql), and every
// code is defined once, in internal/domain; the store scans and binds them as int16.
//
// SQL concatenates the codes as literals: `WHERE status = `+outboxFailed+``. A misspelt name does
// not compile, and the text Postgres receives keeps literal predicates, which partial indexes need
// (a bound parameter never matches an index predicate).

// Domain codes used in query text, derived from internal/domain so it stays their one definition.
var (
	// outbox_events.status.
	outboxPending    = sqlCode(domain.OutboxPending)
	outboxProcessing = sqlCode(domain.OutboxProcessing)
	outboxPublished  = sqlCode(domain.OutboxPublished)
	outboxFailed     = sqlCode(domain.OutboxFailed)

	// notifications.status.
	notificationPending = sqlCode(domain.NotificationPending)
	notificationSending = sqlCode(domain.NotificationSending)
	notificationSent    = sqlCode(domain.NotificationSent)
	notificationFailed  = sqlCode(domain.NotificationFailed)
	notificationSkipped = sqlCode(domain.NotificationSkipped)

	// appointments.status.
	statusConfirmedSQL  = sqlCode(domain.StatusConfirmed)
	statusCheckedInSQL  = sqlCode(domain.StatusCheckedIn)
	statusInProgressSQL = sqlCode(domain.StatusInProgress)
	statusCompletedSQL  = sqlCode(domain.StatusCompleted)
	statusCancelledSQL  = sqlCode(domain.StatusCancelled)
	statusNoShowSQL     = sqlCode(domain.StatusNoShow)
	// holdingSQL lists the statuses that hold a bay, a technician and a vehicle (the GiST
	// predicates in 001_schema.sql); onSiteSQL the ones with the car on site.
	holdingSQL = statusConfirmedSQL + ", " + statusCheckedInSQL + ", " + statusInProgressSQL
	onSiteSQL  = statusCheckedInSQL + ", " + statusInProgressSQL
	// confirmedOrCheckedInSQL: not yet started (what config and roster changes must not strand).
	confirmedOrCheckedInSQL = statusConfirmedSQL + ", " + statusCheckedInSQL

	// service_bays.bay_type / required_bay_type.
	bayExpressSQL = sqlCode(domain.BayExpress)
	bayGeneralSQL = sqlCode(domain.BayGeneral)

	aggregateAppointmentSQL = sqlCode(domain.AggregateAppointment)
	kindRescheduledSQL      = sqlCode(domain.KindRescheduled)
	kindNoShowSQL           = sqlCode(domain.KindNoShow)
	kindRemindersSQL        = sqlCode(domain.KindReminder24h) + ", " + sqlCode(domain.KindReminder2h)
)

// sqlCode is v as a SQL integer literal.
func sqlCode[T ~int16](v T) string { return strconv.Itoa(int(v)) }

// appointmentEventCode is t as written to appointment_history and the outbox by the appointment
// writers; only appointment events have history rows.
func appointmentEventCode(t domain.EventType) (int16, error) {
	if !t.IsAppointment() {
		return 0, fmt.Errorf("store: %v (%d) is not an appointment event", t, int16(t))
	}
	return int16(t), nil
}
