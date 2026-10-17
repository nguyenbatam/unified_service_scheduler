package integration_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// The APPOINTMENT_CREATED payload is built in SQL (store.appointmentPayloadSQL) in the booking's
// single round trip; every other event marshals domain.Appointment in Go. Consumers decode both
// into domain.Appointment, so the created payload must carry exactly the keys json.Marshal gives
// the appointment and decode to the same values. A field added to domain.Appointment and not to
// the SQL fails here.
func TestCreatedEventPayloadMatchesAppointment(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	clearOutbox(t, pool)
	for _, req := range []booking.BookRequest{
		oilAt(seed.DealerRiver, seed.VehicleA, londonAt(t, 14, 0)),
		{ // two job lines, waiter
			DealershipID: seed.DealerNorth, VehicleID: newVehicle(t, svc), StartAt: londonAt(t, 14, 0), IsWaiter: true,
			ServiceTypeIDs: []uuid.UUID{seed.ServiceOil, seed.ServiceBrake}, IdempotencyKey: uuid.NewString(),
		},
	} {
		item := mustBook(t, svc, req)
		read := mustGet(t, svc, item.ID)
		var raw []byte
		if err := pool.QueryRow(ctx, `
			SELECT payload FROM outbox_events
			WHERE aggregate_type = $1 AND aggregate_id = $2 AND event_type = `+code(domain.EventAppointmentCreated),
			domain.AggregateAppointment, item.ID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		want, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		if got, wantKeys := jsonKeys(t, raw), jsonKeys(t, want); !slices.Equal(got, wantKeys) {
			t.Fatalf("payload keys %v, want %v", got, wantKeys)
		}
		var decoded domain.Appointment
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		// Timestamps are written exactly as Go writes them in UTC (RFC 3339, "Z", trimmed fraction).
		var gotFields, wantFields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &gotFields); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(marshalUTC(t, read), &wantFields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"startAt", "endAt", "createdAt", "updatedAt"} {
			if string(gotFields[key]) != string(wantFields[key]) {
				t.Fatalf("%s = %s, want Go's encoding %s", key, gotFields[key], wantFields[key])
			}
		}
		// Same values once both sides are marshalled from the decoded struct.
		var fromGo domain.Appointment
		if err := json.Unmarshal(want, &fromGo); err != nil {
			t.Fatal(err)
		}
		if a, b := marshalUTC(t, decoded), marshalUTC(t, fromGo); !bytes.Equal(a, b) {
			t.Fatalf("created payload decodes to\n%s\nwant\n%s", a, b)
		}
		if len(decoded.Lines) != len(read.Lines) || decoded.Version != 1 || decoded.Status != domain.StatusConfirmed {
			t.Fatalf("decoded %+v", decoded)
		}
		wantEvents(t, "created", pool, item.ID, domain.EventAppointmentCreated, 1)
	}
}

func jsonKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(m))
}

func marshalUTC(t *testing.T, a domain.Appointment) []byte {
	t.Helper()
	a.StartAt, a.EndAt, a.CreatedAt, a.UpdatedAt = a.StartAt.UTC(), a.EndAt.UTC(), a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	for _, p := range []**time.Time{&a.CheckedInAt, &a.StartedAt, &a.CompletedAt, &a.OverrunAt} {
		if *p != nil {
			v := (*p).UTC()
			*p = &v
		}
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
