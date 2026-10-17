package worker_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

func TestBuildEventAppointmentRoutingAndNotice(t *testing.T) {
	appt := domain.Appointment{
		ID: uuid.New(), DealershipID: uuid.New(), CustomerID: uuid.New(), CustomerName: "Amelia Clarke",
		VIN: "WBA00000000000001", Status: domain.StatusConfirmed, Version: 3,
		StartAt: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC), EndAt: time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC),
	}
	payload, err := json.Marshal(appt)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := worker.BuildEvent(store.OutboxEvent{
		AggregateType: domain.AggregateAppointment, AggregateID: appt.ID, EventType: domain.EventAppointmentCreated, Payload: payload,
	}, "backend-1")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.KindAppointment || ev.DealershipID != appt.DealershipID || ev.CustomerID != appt.CustomerID {
		t.Fatalf("routing = %+v", ev)
	}
	if ev.Appointment == nil || ev.Appointment.ID != appt.ID || ev.Appointment.Version != 3 {
		t.Fatalf("notice = %+v", ev.Appointment)
	}
	notice, err := json.Marshal(ev.Appointment)
	if err != nil {
		t.Fatal(err)
	}
	for _, pii := range []string{"Amelia", "WBA0"} {
		if strings.Contains(string(notice), pii) {
			t.Fatalf("notice leaks %q: %s", pii, notice)
		}
	}
}

func TestBuildEventConfigAndUnknown(t *testing.T) {
	id := uuid.New()
	ev, err := worker.BuildEvent(store.OutboxEvent{
		AggregateType: domain.AggregateDealership, AggregateID: id, EventType: domain.EventConfigUpdated,
		Payload: []byte(`{"version":4}`),
	}, "i")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.KindConfig || ev.Config.ID != id || ev.Config.Version != 4 || ev.Config.Entity != domain.AggregateDealership {
		t.Fatalf("config event = %+v", ev.Config)
	}
	if _, err := worker.BuildEvent(store.OutboxEvent{AggregateType: domain.AggregateType(99), Payload: []byte(`{}`)}, "i"); err == nil {
		t.Fatal("unknown aggregate must be rejected (poison)")
	}
	if _, err := worker.BuildEvent(store.OutboxEvent{AggregateType: domain.AggregateAppointment, Payload: []byte(`{`)}, "i"); err == nil {
		t.Fatal("bad JSON must be rejected (poison)")
	}
}

func TestBuildEventSessionRevokedIsControlOnly(t *testing.T) {
	user := uuid.New()
	ev, err := worker.BuildEvent(store.OutboxEvent{
		AggregateType: domain.AggregateUser, AggregateID: user, EventType: domain.EventSessionRevoked,
		Payload: []byte(`{"id":"x","tokenVersion":3}`),
	}, "i")
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != realtime.KindSession || ev.UserID != user || ev.Payload != nil || ev.TokenVersion != 3 {
		t.Fatalf("session event = %+v", ev)
	}
	// A revoke is never dropped: an unreadable payload still closes every socket (version 0).
	ev, err = worker.BuildEvent(store.OutboxEvent{
		AggregateType: domain.AggregateUser, AggregateID: user, EventType: domain.EventSessionRevoked, Payload: []byte(`{`),
	}, "i")
	if err != nil || ev.Kind != realtime.KindSession || ev.TokenVersion != 0 {
		t.Fatalf("session event from a bad payload = %+v, %v", ev, err)
	}
}

// Config events carry the dealership they belong to, so the hub can keep technician changes to
// that dealership's staff (and away from customers): the aggregate itself for a dealership, the
// technician's dealership from its payload; service types are group-wide.
func TestBuildEventConfigDealershipRouting(t *testing.T) {
	tech := domain.TechnicianAdmin{ID: uuid.New(), DealershipID: uuid.New(), Name: "Sam", Version: 2}
	payload, err := json.Marshal(tech)
	if err != nil {
		t.Fatal(err)
	}
	dealer := uuid.New()
	for _, tt := range []struct {
		name      string
		aggregate domain.AggregateType
		id        uuid.UUID
		payload   []byte
		want      uuid.UUID
	}{
		{"dealership: its own id", domain.AggregateDealership, dealer, []byte(`{"id":"` + dealer.String() + `","version":1}`), dealer},
		{"technician: its dealership", domain.AggregateTechnician, tech.ID, payload, tech.DealershipID},
		{"service type: group-wide", domain.AggregateServiceType, uuid.New(), []byte(`{"version":1}`), uuid.Nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ev, err := worker.BuildEvent(store.OutboxEvent{
				AggregateType: tt.aggregate, AggregateID: tt.id, EventType: domain.EventConfigUpdated, Payload: tt.payload,
			}, "i")
			if err != nil {
				t.Fatal(err)
			}
			if ev.Kind != realtime.KindConfig || ev.DealershipID != tt.want || ev.Config.Entity != tt.aggregate {
				t.Fatalf("event = kind %v dealership %s entity %v, want dealership %s", ev.Kind, ev.DealershipID, ev.Config.Entity, tt.want)
			}
		})
	}
}
