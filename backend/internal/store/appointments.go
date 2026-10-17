package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// appointmentSelect joins the display names and aggregates the job lines in a LATERAL subquery.
// Never row-lock through it (see LockAppointment): a waiter would re-check the joins against rows
// read before the wait and lose an appointment that moved bay or technician meanwhile.
const appointmentSelect = appointmentColumns + ` FROM appointments a` + appointmentJoins

// appointmentColumns and appointmentJoins read an appointment row aliased a (a table or the
// RETURNING of an UPDATE, see Tx.updateAppointment) the way scanAppointment expects.
const appointmentColumns = `
	SELECT
		a.id,
		a.dealership_id,
		d.name,
		a.customer_id,
		c.name,
		a.vehicle_id,
		v.year,
		v.make,
		v.model,
		v.vin,
		a.service_type_id,
		COALESCE(l.names, st.name),
		a.booked_duration_minutes,
		COALESCE(l.items, '[]'::json),
		a.required_bay_type,
		a.required_skills,
		a.min_skill_level,
		a.service_bay_id,
		b.name,
		b.bay_type,
		a.technician_id,
		t.name,
		a.start_at,
		a.end_at,
		a.status,
		a.is_waiter,
		COALESCE(a.idempotency_key, ''),
		COALESCE(a.idempotency_fingerprint, ''),
		a.created_at,
		a.version,
		a.updated_at,
		a.checked_in_at,
		a.started_at,
		a.completed_at,
		a.overrun_at`

const appointmentJoins = `
	JOIN dealerships d ON d.id = a.dealership_id
	JOIN customers c ON c.id = a.customer_id
	JOIN vehicles v ON v.id = a.vehicle_id
	JOIN service_types st ON st.id = a.service_type_id
	JOIN service_bays b ON b.id = a.service_bay_id
	JOIN technicians t ON t.id = a.technician_id
	LEFT JOIN LATERAL (
		SELECT json_agg(json_build_object(
		           'lineNo', al.line_no, 'serviceTypeId', al.service_type_id, 'code', al.service_code,
		           'name', al.service_name, 'durationMinutes', al.duration_minutes) ORDER BY al.line_no) AS items,
		       string_agg(al.service_name, ' + ' ORDER BY al.line_no) AS names
		FROM appointment_lines al
		WHERE al.appointment_id = a.id
	) l ON true
`

func getAppointment(ctx context.Context, q queryer, where string, args ...any) (domain.Appointment, error) {
	row := q.QueryRow(ctx, appointmentSelect+` WHERE `+where, args...)
	item, err := scanAppointment(row)
	if err != nil {
		return domain.Appointment{}, missing(err, "appointment not found")
	}
	return item, nil
}

func (s *Store) GetAppointment(ctx context.Context, id uuid.UUID) (domain.Appointment, error) {
	return getAppointment(ctx, s.pool, `a.id = $1`, id)
}

// LockAppointment reads the appointment and row-locks it until the TX ends. FOR NO KEY UPDATE is
// enough (an appointment's id never changes) and, unlike FOR UPDATE, does not block the KEY SHARE
// locks foreign-key inserts take: the notifier's enqueue for this appointment (and so its Kafka
// partition) never waits behind a reschedule or a transition.
//
// The lock is its own statement on appointments alone, and the joined read follows it in the same
// round trip (READ COMMITTED: a fresh snapshot, taken after the lock is granted). Locking inside
// the joined read would lose the row: after waiting behind a writer that moved it to another bay or
// technician (Reassign, a repair move), Postgres re-checks the join against the bay and technician
// rows it had already read, the join no longer matches, and the locker saw "not found" (404).
func (t *Tx) LockAppointment(ctx context.Context, id uuid.UUID) (domain.Appointment, error) {
	b := &pgx.Batch{}
	b.Queue(`SELECT 1 FROM appointments WHERE id = $1 FOR NO KEY UPDATE`, id)
	var item *domain.Appointment
	queueOptional(b, &item, scanAppointment, appointmentSelect+` WHERE a.id = $1`, id)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return domain.Appointment{}, err
	}
	if item == nil {
		return domain.Appointment{}, domain.NewError(domain.CodeNotFound, "appointment not found")
	}
	return *item, nil
}

// idempotencyWhere uses = / IS NULL (not IS NOT DISTINCT FROM) so the planner can use
// appointments_idempotency_scope_uniq instead of scanning appointments inside the booking TX.
func idempotencyWhere(createdBy *uuid.UUID, key string) (string, []any) {
	if createdBy == nil {
		return `a.created_by IS NULL AND a.idempotency_key = $1`, []any{key}
	}
	return `a.created_by = $1 AND a.idempotency_key = $2`, []any{*createdBy, key}
}

// MaxAppointmentSpan bounds end_at - start_at (CHECK appointments_max_span). Overlap queries use
// it as a lower bound on start_at so the (dealership_id, start_at) index skips older history.
const MaxAppointmentSpan = 24 * time.Hour

// Board is what the dealership day board shows for one local day.
type Board struct {
	Rules        DayRules
	Bays         []domain.Bay // active bays, express first
	Technicians  []domain.Technician
	Appointments []domain.Appointment // every non-cancelled appointment overlapping the day
}

// ScheduleBoard reads the day board of local day (YYYY-MM-DD, ISO weekday, instants [from, to)) in
// one round trip: the calendar rules, the active bays and technicians, and the day's appointments
// (plain reads on one pool connection, no locks). A flagged overrun that started on an earlier day
// is on the board too: it still holds its bay and technician (overrunningSQL), so the dispatcher
// sees why they cannot be booked.
func (s *Store) ScheduleBoard(ctx context.Context, dealershipID uuid.UUID, day string, weekday int16, from, to time.Time) (Board, error) {
	var out Board
	b := &pgx.Batch{}
	queueDayRules(b, &out.Rules, dealershipID, day, weekday)
	sql, args := baysSQL(dealershipID, 0, false)
	queueRows(b, &out.Bays, scanBay, sql, args...)
	queueTechnicians(b, &out.Technicians, dealershipID)
	queueRows(b, &out.Appointments, scanAppointment, appointmentSelect+`
		WHERE a.id IN (
			SELECT o.id FROM appointments o
			WHERE o.dealership_id = $1
			  AND o.status <> `+statusCancelledSQL+`
			  AND o.start_at < $3::timestamptz
			  AND o.start_at > $4::timestamptz
			  AND (o.end_at > $2::timestamptz OR `+overrunningSQL("o")+`)
			UNION ALL
			SELECT o.id FROM appointments o
			WHERE o.dealership_id = $1
			  AND `+overrunningSQL("o")+`
			  AND o.start_at <= $4::timestamptz)
		ORDER BY a.start_at, b.name`, dealershipID, from, to, from.Add(-MaxAppointmentSpan))
	return out, sendBatch(ctx, s.pool, b)
}

// AppointmentFilter narrows ListAppointmentsPage; zero ids mean any.
type AppointmentFilter struct {
	DealershipID uuid.UUID
	CustomerID   uuid.UUID
	VehicleID    uuid.UUID
	Limit        int // 1–200, else 50
	// When picks upcoming, past, or every appointment (the default); Now splits upcoming from past.
	When AppointmentWhen
	Now  time.Time
}

func vehicleBusySQL(vehicleID uuid.UUID, start, end time.Time, exclude uuid.UUID) (string, []any) {
	return `
		SELECT EXISTS (
			SELECT 1 FROM appointments
			WHERE vehicle_id = $1
			  AND id <> $4
			  AND ` + holdingStatusesSQL + `
			  AND tstzrange(start_at, end_at, '[)') && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		)`, []any{vehicleID, start, end, exclude}
}

func scanAppointment(row pgx.Row) (domain.Appointment, error) {
	var item domain.Appointment
	var year int
	var vehicleMake, model string
	var status, bayType, requiredBay int16
	var skills []int16
	err := row.Scan(
		&item.ID, &item.DealershipID, &item.DealershipName, &item.CustomerID, &item.CustomerName,
		&item.VehicleID, &year, &vehicleMake, &model, &item.VIN,
		&item.ServiceTypeID, &item.ServiceName, &item.DurationMinutes, &item.Lines,
		&requiredBay, &skills, &item.MinSkillLevel,
		&item.ServiceBayID, &item.ServiceBayName, &bayType, &item.TechnicianID, &item.TechnicianName,
		&item.StartAt, &item.EndAt, &status, &item.IsWaiter, &item.IdempotencyKey, &item.IdempotencyFingerprint,
		&item.CreatedAt, &item.Version, &item.UpdatedAt, &item.CheckedInAt, &item.StartedAt, &item.CompletedAt,
		&item.OverrunAt,
	)
	if err != nil {
		return domain.Appointment{}, err
	}
	item.Status = domain.AppointmentStatus(status)
	item.ServiceBayType = domain.BayType(bayType)
	item.RequiredBayType = domain.BayType(requiredBay)
	item.RequiredSkills = make([]domain.TechnicianSkill, len(skills))
	for i, sk := range skills {
		item.RequiredSkills[i] = domain.TechnicianSkill(sk)
	}
	item.VehicleLabel = fmt.Sprintf("%d %s %s", year, vehicleMake, model)
	return item, nil
}
