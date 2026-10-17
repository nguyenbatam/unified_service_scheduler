package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Every appointment change goes through RecordAppointmentEvent in the same TX: one outbox event
// (Kafka → WebSocket hubs, notifications) and one audit-trail row.

// RecordAppointmentEvent writes the outbox event and the appointment_history row for item's
// current state. by is the acting user (nil = system).
func (t *Tx) RecordAppointmentEvent(ctx context.Context, eventType domain.EventType, item domain.Appointment, by *uuid.UUID) error {
	event, err := appointmentEventCode(eventType)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return err
	}
	// One round trip inside the booking TX (which may hold the dealership-day lock): audit row and outbox
	// row as a single statement (a data-modifying CTE always runs to completion). The workers are
	// woken after COMMIT (Tx.Commit), never by a NOTIFY inside the TX.
	_, err = t.tx.Exec(ctx, `
		WITH h AS (
			INSERT INTO appointment_history
				(appointment_id, version, event_type, actor_id, status, start_at, end_at, service_bay_id, technician_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		)
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status)
		VALUES ($10, `+aggregateAppointmentSQL+`, $1, $3, $11::jsonb, `+outboxPending+`)`,
		item.ID, item.Version, event, by, int16(item.Status), item.StartAt, item.EndAt,
		item.ServiceBayID, item.TechnicianID, uuid.New(), raw)
	if err == nil {
		t.markEvents()
	}
	return err
}

// NewAppointment is a row to insert. Lines, DurationMinutes, RequiredBayType, RequiredSkills, and
// MinSkillLevel on the embedded Appointment are the booked snapshot.
type NewAppointment struct {
	domain.Appointment
	IdempotencyKey *string
	Fingerprint    string
	CreatedBy      *uuid.UUID
}

// insertAppointmentSQL writes the row and the booked snapshot of its lines in one statement (a
// data-modifying CTE; the lines' foreign key is checked at the end of the statement, after the row
// exists). Lines are never rewritten afterwards, so there is nothing to delete first.
func insertAppointmentSQL(item NewAppointment) (string, []any) {
	var fingerprint *string
	if item.IdempotencyKey != nil {
		fingerprint = &item.Fingerprint
	}
	n := len(item.Lines)
	lineNos := make([]int16, n)
	serviceIDs := make([]uuid.UUID, n)
	codes := make([]string, n)
	names := make([]string, n)
	durations := make([]int, n)
	for i, l := range item.Lines {
		lineNos[i], serviceIDs[i], codes[i], names[i], durations[i] = l.LineNo, l.ServiceTypeID, l.Code, l.Name, l.DurationMinutes
	}
	return `
		WITH a AS (
			INSERT INTO appointments (
				id, dealership_id, customer_id, vehicle_id, service_type_id,
				service_bay_id, technician_id, start_at, end_at, status, idempotency_key, is_waiter,
				created_by, booked_duration_minutes, required_bay_type, required_skills, min_skill_level,
				idempotency_fingerprint
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::timestamptz,$9::timestamptz,$10,$11,$12,$13,$14,$15,$16,$17,$18)
			RETURNING id
		)
		INSERT INTO appointment_lines (appointment_id, line_no, service_type_id, service_code, service_name, duration_minutes)
		SELECT a.id, l.line_no, l.service_type_id, l.service_code, l.service_name, l.duration_minutes
		FROM a, unnest($19::smallint[], $20::uuid[], $21::text[], $22::text[], $23::int[])
		     AS l(line_no, service_type_id, service_code, service_name, duration_minutes)`,
		[]any{
			item.ID, item.DealershipID, item.CustomerID, item.VehicleID, item.ServiceTypeID,
			item.ServiceBayID, item.TechnicianID, item.StartAt, item.EndAt, int16(item.Status),
			item.IdempotencyKey, item.IsWaiter, item.CreatedBy, item.DurationMinutes, int16(item.RequiredBayType),
			skillCodes(item.RequiredSkills), item.MinSkillLevel, fingerprint,
			lineNos, serviceIDs, codes, names, durations,
		}
}

// InsertAppointmentWithEvent inserts the row, records its event (history + outbox), and
// reads back the fully joined appointment, all in one round trip: the three statements are
// pipelined in one pgx batch and run in order in the caller's TX, so the event statement sees the
// inserted row and draws the outbox seq after it exists (invariant 7). The event payload is built
// in SQL from the same joined row (appointmentPayloadSQL) rather than marshalled in Go from the
// read, which would need a second round trip while the booking holds the dealership-day lock.
func (t *Tx) InsertAppointmentWithEvent(ctx context.Context, item NewAppointment, eventType domain.EventType) (domain.Appointment, error) {
	event, err := appointmentEventCode(eventType)
	if err != nil {
		return domain.Appointment{}, err
	}
	var full *domain.Appointment
	b := &pgx.Batch{}
	sql, args := insertAppointmentSQL(item)
	b.Queue(sql, args...).Exec(ignoreTag)
	queueOptional(b, &full, scanAppointment, appointmentSelect+` WHERE a.id = $1`, item.ID)
	b.Queue(insertedEventSQL, item.ID, event, item.CreatedBy, uuid.New()).Exec(ignoreTag)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return domain.Appointment{}, err
	}
	t.markEvents()
	if full == nil {
		return domain.Appointment{}, domain.NewError(domain.CodeNotFound, "appointment not found")
	}
	return *full, nil
}

// insertedEventSQL is RecordAppointmentEvent for a row inserted earlier in the same TX, reading the
// row itself instead of taking its state as parameters: $1 appointment id, $2 event type code
// (appointmentEventCode), $3 actor, $4 outbox id.
var insertedEventSQL = `
	WITH h AS (
		INSERT INTO appointment_history
			(appointment_id, version, event_type, actor_id, status, start_at, end_at, service_bay_id, technician_id)
		SELECT a.id, a.version, $2, $3::uuid, a.status, a.start_at, a.end_at, a.service_bay_id, a.technician_id
		FROM appointments a WHERE a.id = $1
	)
	INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status)
	SELECT $4, ` + aggregateAppointmentSQL + `, a.id, $2, ` + appointmentPayloadSQL + `, ` + outboxPending + `
	FROM appointments a` + appointmentJoins + `
	WHERE a.id = $1`

// appointmentPayloadSQL encodes the joined appointment row a (appointmentJoins) as the outbox
// payload: the JSON shape json.Marshal gives domain.Appointment after scanAppointment — the same
// keys in field order, "-" fields left out, the omitempty timestamps left out while NULL
// (json_strip_nulls; no other key can be null), enums as numbers, vehicleLabel as scanAppointment
// formats it, timestamps as Go's RFC 3339 encoding of the instant in UTC (jsonTimeSQL). Consumers
// (worker.BuildEvent, notify) decode the payload into domain.Appointment. Keep in step with
// domain.Appointment's json tags: integration TestCreatedEventPayloadMatchesAppointment compares
// the two.
var appointmentPayloadSQL = `json_strip_nulls(json_build_object(
		'id', a.id,
		'dealershipId', a.dealership_id,
		'dealershipName', d.name,
		'customerId', a.customer_id,
		'customerName', c.name,
		'vehicleId', a.vehicle_id,
		'vehicleLabel', format('%s %s %s', v.year, v.make, v.model),
		'vin', v.vin,
		'serviceTypeId', a.service_type_id,
		'serviceName', COALESCE(l.names, st.name),
		'durationMinutes', a.booked_duration_minutes,
		'lines', COALESCE(l.items, '[]'::json),
		'requiredBayType', a.required_bay_type,
		'requiredSkills', a.required_skills,
		'minSkillLevel', a.min_skill_level,
		'serviceBayId', a.service_bay_id,
		'serviceBayName', b.name,
		'serviceBayType', b.bay_type,
		'technicianId', a.technician_id,
		'technicianName', t.name,
		'startAt', ` + jsonTimeSQL("a.start_at") + `,
		'endAt', ` + jsonTimeSQL("a.end_at") + `,
		'status', a.status,
		'isWaiter', a.is_waiter,
		'version', a.version,
		'checkedInAt', ` + jsonTimeSQL("a.checked_in_at") + `,
		'startedAt', ` + jsonTimeSQL("a.started_at") + `,
		'completedAt', ` + jsonTimeSQL("a.completed_at") + `,
		'overrunAt', ` + jsonTimeSQL("a.overrun_at") + `,
		'createdAt', ` + jsonTimeSQL("a.created_at") + `,
		'updatedAt', ` + jsonTimeSQL("a.updated_at") + `))::jsonb`

// jsonTimeSQL encodes the timestamptz expression col as json.Marshal encodes a time.Time in UTC
// (RFC 3339 with "Z" and trailing zeros of the fraction dropped, as time.RFC3339Nano), so the
// SQL-built payload reads like every Go-built one. NULL stays NULL (json_strip_nulls drops it).
func jsonTimeSQL(col string) string {
	utc := col + ` AT TIME ZONE 'UTC'`
	return `(to_char(` + utc + `, 'YYYY-MM-DD"T"HH24:MI:SS') || rtrim(rtrim(to_char(` + utc + `, '.US'), '0'), '.') || 'Z')`
}

// Move is an in-place reschedule: new window, bay, technician, and waiter flag. The booked
// snapshot (lines, labour time, requirements) is never re-derived, so it is not rewritten.
type Move struct {
	ID           uuid.UUID
	ServiceBayID uuid.UUID
	TechnicianID uuid.UUID
	StartAt      time.Time
	EndAt        time.Time
	IsWaiter     bool
}

// moveSetSQL is the SET list of a move: $1 id, $2 bay, $3 technician, $4 start, $5 end, $6 waiter.
// scheduled_at restarts only when the start changes (the right-hand start_at is the old one), so a
// waiter-flag change in the same window keeps its reminders.
const moveSetSQL = `
		service_bay_id = $2, technician_id = $3, start_at = $4::timestamptz, end_at = $5::timestamptz,
		is_waiter = $6, scheduled_at = CASE WHEN start_at = $4::timestamptz THEN scheduled_at ELSE now() END,
		overrun_at = NULL, version = version + 1, updated_at = now()`

// MoveAppointmentWithEvent applies m, bumps version, and stamps scheduled_at when the start moves (reminders restart),
// then records its event (RecordAppointmentEvent's rows, the payload built in SQL from the moved
// row). The GiST constraints check the new window against every other row; the row's own old
// window is replaced. One round trip: both statements are pipelined in one
// pgx batch in the caller's TX (a reschedule holds the dealership-day lock while it runs).
func (t *Tx) MoveAppointmentWithEvent(ctx context.Context, m Move, eventType domain.EventType, by *uuid.UUID) (domain.Appointment, error) {
	event, err := appointmentEventCode(eventType)
	if err != nil {
		return domain.Appointment{}, err
	}
	var moved *domain.Appointment
	b := &pgx.Batch{}
	queueOptional(b, &moved, scanAppointment, `WITH upd AS (UPDATE appointments SET `+moveSetSQL+` WHERE id = $1 RETURNING *)`+
		appointmentColumns+` FROM upd a`+appointmentJoins,
		m.ID, m.ServiceBayID, m.TechnicianID, m.StartAt, m.EndAt, m.IsWaiter)
	b.Queue(insertedEventSQL, m.ID, event, by, uuid.New()).Exec(ignoreTag)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return domain.Appointment{}, err
	}
	if moved == nil {
		return domain.Appointment{}, domain.NewError(domain.CodeNotFound, "appointment not found")
	}
	t.markEvents()
	return *moved, nil
}

// Reassign moves the appointment to another bay and/or technician in the same window and bumps
// version. GiST rejects it if the target is already held then.
func (t *Tx) Reassign(ctx context.Context, id, bayID, technicianID uuid.UUID) (domain.Appointment, error) {
	return t.updateAppointment(ctx, `service_bay_id = $2, technician_id = $3, version = version + 1, updated_at = now()`,
		id, bayID, technicianID)
}

// RepairMove is one move of a booking's one-move repair: the appointment changes bay and/or
// technician in place (uuid.Nil keeps the current one).
type RepairMove struct {
	ID, ServiceBayID, TechnicianID uuid.UUID
}

// ApplyRepairMoves reassigns each move's appointment, bumping its version, and records eventType
// for it (RecordAppointmentEvent's rows, the payload built in SQL from the moved row) — every
// UPDATE with its event, in one round trip (one pgx batch; the booking holds the dealership-day
// lock meanwhile). GiST rejects a move whose target is held then; a missing row is not found.
func (t *Tx) ApplyRepairMoves(ctx context.Context, moves []RepairMove, eventType domain.EventType, by *uuid.UUID) error {
	event, err := appointmentEventCode(eventType)
	if err != nil {
		return err
	}
	b := &pgx.Batch{}
	for _, m := range moves {
		var bay, tech *uuid.UUID
		if m.ServiceBayID != uuid.Nil {
			bay = &m.ServiceBayID
		}
		if m.TechnicianID != uuid.Nil {
			tech = &m.TechnicianID
		}
		b.Queue(`UPDATE appointments SET service_bay_id = COALESCE($2::uuid, service_bay_id),
			technician_id = COALESCE($3::uuid, technician_id), version = version + 1, updated_at = now()
			WHERE id = $1`, m.ID, bay, tech).Exec(func(tag pgconn.CommandTag) error {
			if tag.RowsAffected() == 0 {
				return domain.NewError(domain.CodeNotFound, "appointment not found")
			}
			return nil
		})
		b.Queue(insertedEventSQL, m.ID, event, by, uuid.New()).Exec(ignoreTag)
	}
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return err
	}
	t.markEvents()
	return nil
}

// SetWindow moves start/end without bumping version: the caller's SetStatus in the same TX does
// (start work early pulls the window forward to when the technician actually begins).
func (t *Tx) SetWindow(ctx context.Context, id uuid.UUID, start, end time.Time) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE appointments SET start_at = $2::timestamptz, end_at = $3::timestamptz WHERE id = $1`, id, start, end)
	return err
}

// SetStatus moves the appointment to status, stamps the matching lifecycle time, and bumps version.
func (t *Tx) SetStatus(ctx context.Context, id uuid.UUID, to domain.AppointmentStatus) (domain.Appointment, error) {
	return t.updateAppointment(ctx, `
		status = $2::smallint,
		version = version + 1,
		updated_at = now(),
		checked_in_at = CASE WHEN $2::smallint = `+statusCheckedInSQL+` THEN now() ELSE checked_in_at END,
		started_at = CASE WHEN $2::smallint = `+statusInProgressSQL+` THEN now() ELSE started_at END,
		completed_at = CASE WHEN $2::smallint = `+statusCompletedSQL+` THEN now() ELSE completed_at END`, id, int16(to))
}

// ExtendEnd moves end_at (overrun) and clears the overrun flag when the new end is still ahead of
// now (the service clock): an extension that ends in the past leaves the car on the lift, so the
// flag, and the hold it keeps on the bay and technician, stay. GiST exclusion rejects it if the
// bay/tech/vehicle is taken next.
func (t *Tx) ExtendEnd(ctx context.Context, id uuid.UUID, end, now time.Time) (domain.Appointment, error) {
	return t.updateAppointment(ctx, `end_at = $2::timestamptz,
		overrun_at = CASE WHEN $2::timestamptz > $3::timestamptz THEN NULL ELSE overrun_at END,
		version = version + 1, updated_at = now()`, id, end, now)
}

// updateAppointment applies set (an UPDATE SET list; $1 is the appointment id) and returns the
// updated row with its display names in the same statement: the joins read the RETURNING row, so
// a changed bay or technician comes back with its new name, without a second round trip.
func (t *Tx) updateAppointment(ctx context.Context, set string, args ...any) (domain.Appointment, error) {
	row := t.tx.QueryRow(ctx, `WITH upd AS (UPDATE appointments SET `+set+` WHERE id = $1 RETURNING *)`+
		appointmentColumns+` FROM upd a`+appointmentJoins, args...)
	item, err := scanAppointment(row)
	if err != nil {
		return domain.Appointment{}, missing(err, "appointment not found")
	}
	return item, nil
}
