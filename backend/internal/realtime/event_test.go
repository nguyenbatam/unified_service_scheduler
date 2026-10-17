package realtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestRoute(t *testing.T) {
	dealerA, dealerB := uuid.New(), uuid.New()
	owner, other := uuid.New(), uuid.New()
	staff := Session{Role: domain.RoleAdmin}
	ownerSes := Session{Role: domain.RoleCustomer, CustomerID: owner}
	otherSes := Session{Role: domain.RoleCustomer, CustomerID: other}
	created := Event{Type: domain.EventAppointmentCreated, Kind: KindAppointment, DealershipID: dealerA, CustomerID: owner}
	started := Event{Type: domain.EventAppointmentStarted, Kind: KindAppointment, DealershipID: dealerA, CustomerID: owner}
	checkedIn := Event{Type: domain.EventAppointmentCheckedIn, Kind: KindAppointment, DealershipID: dealerA, CustomerID: owner}
	overrunning := Event{Type: domain.EventAppointmentOverrunning, Kind: KindAppointment, DealershipID: dealerA, CustomerID: owner}
	scopedB := Session{Role: domain.RoleDispatcher, Scope: dealerB}
	config := Event{Type: domain.EventConfigUpdated, Kind: KindConfig, Config: &ConfigNotice{Entity: domain.AggregateServiceType}}
	dealerConfigA := Event{Type: domain.EventConfigUpdated, Kind: KindConfig, DealershipID: dealerA, Config: &ConfigNotice{Entity: domain.AggregateDealership}}
	techA := Event{Type: domain.EventConfigUpdated, Kind: KindConfig, DealershipID: dealerA, Config: &ConfigNotice{Entity: domain.AggregateTechnician}}
	techUnknown := Event{Type: domain.EventConfigUpdated, Kind: KindConfig, Config: &ConfigNotice{Entity: domain.AggregateTechnician}}

	tests := []struct {
		name       string
		ev         Event
		session    Session
		subscribed uuid.UUID
		want       delivery
	}{
		{"staff sees everything", created, staff, uuid.Nil, deliverNotice},
		{"staff filtered by subscription", created, staff, dealerB, deliverNone},
		{"staff matching subscription", created, staff, dealerA, deliverNotice},
		{"owner sees own booking", created, ownerSes, uuid.Nil, deliverNotice},
		{"owner sees own non-capacity event", started, ownerSes, uuid.Nil, deliverNotice},
		{"other customer gets anonymous ping", created, otherSes, uuid.Nil, deliverAvailability},
		{"other customer ignores non-capacity", checkedIn, otherSes, uuid.Nil, deliverNone},
		{"early start may move a hold: capacity ping", started, otherSes, uuid.Nil, deliverAvailability},
		{"flagged overrun holds its bay for the day: capacity ping", overrunning, otherSes, uuid.Nil, deliverAvailability},
		{"staff scoped to another dealership", created, scopedB, uuid.Nil, deliverNone},
		{"staff scoped to another dealership ignores subscription", created, scopedB, dealerA, deliverNone},
		{"staff scoped to its dealership", created, Session{Role: domain.RoleDispatcher, Scope: dealerA}, uuid.Nil, deliverNotice},
		{"config reaches scoped staff", config, scopedB, uuid.Nil, deliverNotice},
		{"other customer other dealership", created, otherSes, dealerB, deliverNone},
		{"owner subscribed elsewhere still sees own booking", created, ownerSes, dealerB, deliverNotice},
		{"owner subscribed elsewhere still sees own non-capacity event", checkedIn, ownerSes, dealerB, deliverNotice},
		{"owner matching subscription", started, ownerSes, dealerA, deliverNotice},
		{"other customer matching subscription gets ping", created, otherSes, dealerA, deliverAvailability},
		{"config reaches customers", config, otherSes, dealerB, deliverNotice},
		{"dealership config reaches customers whatever the subscription", dealerConfigA, otherSes, dealerB, deliverNotice},
		{"dealership config reaches staff whatever the subscription", dealerConfigA, staff, dealerB, deliverNotice},
		{"dealership config skips staff scoped elsewhere", dealerConfigA, scopedB, uuid.Nil, deliverNone},
		{"dealership config reaches its own scoped staff", dealerConfigA, Session{Role: domain.RoleDispatcher, Scope: dealerA}, uuid.Nil, deliverNotice},
		{"technician config reaches customers as an anonymous ping", techA, ownerSes, uuid.Nil, deliverAvailability},
		{"technician config pings customers subscribed to its dealership", techA, otherSes, dealerA, deliverAvailability},
		{"technician config skips customers subscribed elsewhere", techA, otherSes, dealerB, deliverNone},
		{"technician of unknown dealership never reaches customers", techUnknown, otherSes, uuid.Nil, deliverNone},
		{"technician config reaches staff", techA, staff, uuid.Nil, deliverNotice},
		{"technician config filtered by subscription", techA, staff, dealerB, deliverNone},
		{"technician config matching subscription", techA, staff, dealerA, deliverNotice},
		{"technician config skips staff scoped elsewhere", techA, scopedB, uuid.Nil, deliverNone},
		{"technician of unknown dealership: group-wide staff only", techUnknown, staff, uuid.Nil, deliverNotice},
		{"technician of unknown dealership skips scoped staff", techUnknown, Session{Role: domain.RoleDispatcher, Scope: dealerA}, uuid.Nil, deliverNone},
		{"customer without link never matches", created, Session{Role: domain.RoleCustomer}, uuid.Nil, deliverAvailability},
		{"unknown kind dropped", Event{Type: domain.EventType(99)}, staff, uuid.Nil, deliverNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := route(tt.ev, tt.session, tt.subscribed); got != tt.want {
				t.Fatalf("route = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestAvailabilityMessage(t *testing.T) {
	dealer, tech := uuid.New(), uuid.New()
	start := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		ev   Event
		want string
	}{
		{
			name: "appointment: dealership and window",
			ev: Event{Type: domain.EventAppointmentCreated, Kind: KindAppointment, DealershipID: dealer, CustomerID: uuid.New(),
				Appointment: &AppointmentNotice{ID: uuid.New(), DealershipID: dealer, Version: 3, StartAt: start, EndAt: start.Add(time.Hour)}},
			want: `{"type":14,"payload":{"dealershipId":"` + dealer.String() +
				`","startAt":"2026-10-19T09:00:00Z","endAt":"2026-10-19T10:00:00Z"}}`,
		},
		{
			name: "technician: dealership only, no window, no technician",
			ev: Event{Type: domain.EventConfigUpdated, Kind: KindConfig, DealershipID: dealer,
				Config: &ConfigNotice{Entity: domain.AggregateTechnician, ID: tech, Version: 7}, Payload: json.RawMessage(`{"name":"Ann"}`)},
			want: `{"type":14,"payload":{"dealershipId":"` + dealer.String() + `"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(availabilityMessage(tt.ev))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != tt.want {
				t.Fatalf("message = %s, want %s", raw, tt.want)
			}
			if strings.Contains(string(raw), tech.String()) {
				t.Fatalf("message leaks the technician id: %s", raw)
			}
		})
	}
}
