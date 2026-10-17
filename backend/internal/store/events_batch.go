package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// RecordAppointmentEvents is RecordAppointmentEvent for many appointments in one statement (the
// sweeps): one appointment_history row and one outbox_events row per item (the workers are woken after COMMIT). Each
// item must be a different appointment whose row this TX has locked, so each outbox row's seq is
// still drawn after its aggregate's row lock (per-aggregate commit order, see ClaimOutbox); rows
// are inserted in the order of items.
func (t *Tx) RecordAppointmentEvents(ctx context.Context, eventType domain.EventType, items []domain.Appointment, by *uuid.UUID) error {
	if len(items) == 0 {
		return nil
	}
	event, err := appointmentEventCode(eventType)
	if err != nil {
		return err
	}
	n := len(items)
	ids := make([]uuid.UUID, n)
	versions := make([]int, n)
	statuses := make([]int16, n)
	starts := make([]time.Time, n)
	ends := make([]time.Time, n)
	bays := make([]uuid.UUID, n)
	techs := make([]uuid.UUID, n)
	eventIDs := make([]uuid.UUID, n)
	payloads := make([]string, n)
	for i, item := range items {
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		ids[i], versions[i], statuses[i] = item.ID, item.Version, int16(item.Status)
		starts[i], ends[i], bays[i], techs[i] = item.StartAt, item.EndAt, item.ServiceBayID, item.TechnicianID
		eventIDs[i], payloads[i] = uuid.New(), string(raw)
	}
	// Data-modifying CTEs always run to completion, whatever the outer SELECT reads from them.
	_, err = t.tx.Exec(ctx, `
		WITH e AS (
			SELECT * FROM unnest($1::uuid[], $2::int[], $3::smallint[], $4::timestamptz[], $5::timestamptz[],
			                     $6::uuid[], $7::uuid[], $8::uuid[], $9::text[])
			     WITH ORDINALITY AS e(appointment_id, version, status, start_at, end_at,
			                          service_bay_id, technician_id, event_id, payload, ord)
		), h AS (
			INSERT INTO appointment_history
				(appointment_id, version, event_type, actor_id, status, start_at, end_at, service_bay_id, technician_id)
			SELECT appointment_id, version, $10, $11::uuid, status, start_at, end_at, service_bay_id, technician_id
			FROM e ORDER BY ord
		)
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status)
		SELECT event_id, `+aggregateAppointmentSQL+`, appointment_id, $10, payload::jsonb, `+outboxPending+`
		FROM e ORDER BY ord`,
		ids, versions, statuses, starts, ends, bays, techs, eventIDs, payloads,
		event, by)
	if err == nil {
		t.markEvents()
	}
	return err
}
