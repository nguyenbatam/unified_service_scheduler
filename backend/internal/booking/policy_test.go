package booking

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

var testPolicy = domain.Policy{
	CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 30, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60,
}

func TestTooEarly(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	staff := domain.Actor{Role: domain.RoleDispatcher}
	customer := domain.Actor{Role: domain.RoleCustomer, CustomerID: uuid.New()}
	tests := []struct {
		name  string
		actor domain.Actor
		start time.Time
		want  string
	}{
		{"staff past", staff, now.Add(-30 * time.Minute), ReasonPast},
		{"staff now", staff, now, ""},
		{"staff soon", staff, now.Add(30 * time.Minute), ""},
		{"customer past", customer, now.Add(-time.Minute), ReasonPast},
		{"customer inside lead time", customer, now.Add(30 * time.Minute), ReasonLeadTime},
		{"customer at lead time", customer, now.Add(time.Hour), ""},
		{"customer at horizon", customer, now.AddDate(0, 0, 30), ""},
		{"customer beyond horizon", customer, now.AddDate(0, 0, 30).Add(time.Minute), ReasonBeyondHorizon},
		{"staff beyond horizon", staff, now.AddDate(0, 0, 200), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tooEarly(tt.actor, tt.start, now, testPolicy); got != tt.want {
				t.Fatalf("tooEarly = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCustomerMayChange(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	customer := domain.Actor{Role: domain.RoleCustomer}
	soon := domain.Appointment{StartAt: now.Add(30 * time.Minute)}
	later := domain.Appointment{StartAt: now.Add(3 * time.Hour)}
	if err := customerMayChange(customer, soon, now, testPolicy); err == nil {
		t.Fatal("customer change inside cutoff must be refused")
	}
	if err := customerMayChange(customer, later, now, testPolicy); err != nil {
		t.Fatalf("customer change outside cutoff: %v", err)
	}
	if err := customerMayChange(domain.Actor{Role: domain.RoleAdmin}, soon, now, testPolicy); err != nil {
		t.Fatalf("staff is not bound by the cutoff: %v", err)
	}
	strict := testPolicy
	strict.CustomerChangeCutoffMinutes = 240 // 4h: the dealership's own rule
	if err := customerMayChange(customer, later, now, strict); err == nil {
		t.Fatal("the dealership's longer cutoff must apply")
	}
}

func TestValidVIN(t *testing.T) {
	tests := map[string]bool{
		"WBA00000000000001":  true,
		"1HGCM82633A004352":  true,
		"WBA0000000000000":   false, // 16
		"WBA000000000000012": false, // 18
		"WBI00000000000001":  false, // I not allowed
		"WBO00000000000001":  false, // O not allowed
		"WBQ00000000000001":  false, // Q not allowed
		"wba00000000000001":  false, // caller upper-cases first
	}
	for vin, want := range tests {
		if got := ValidVIN(vin); got != want {
			t.Errorf("ValidVIN(%q) = %v, want %v", vin, got, want)
		}
	}
}

func TestDateBefore(t *testing.T) {
	d := Date{Year: 2026, Month: time.October, Day: 17}
	if !d.Before(Date{Year: 2026, Month: time.October, Day: 18}) ||
		!d.Before(Date{Year: 2026, Month: time.November, Day: 1}) ||
		!d.Before(Date{Year: 2027, Month: time.January, Day: 1}) {
		t.Fatal("expected earlier")
	}
	if d.Before(d) || d.Before(Date{Year: 2026, Month: time.October, Day: 16}) {
		t.Fatal("expected not earlier")
	}
}

func TestFingerprintDistinguishesPayload(t *testing.T) {
	oil, brake := uuid.New(), uuid.New()
	ids := []uuid.UUID{oil}
	base := BookRequest{DealershipID: uuid.New(), VehicleID: uuid.New(), ServiceTypeID: oil,
		StartAt: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	sameMinute := base
	sameMinute.StartAt = base.StartAt.Add(20 * time.Second)
	if fingerprint(base, ids) != fingerprint(sameMinute, ids) {
		t.Fatal("seconds must not change the fingerprint")
	}
	waiter := base
	waiter.IsWaiter = true
	if fingerprint(base, ids) == fingerprint(waiter, ids) {
		t.Fatal("waiter flag must change the fingerprint")
	}
	if fingerprint(base, []uuid.UUID{oil, brake}) == fingerprint(base, []uuid.UUID{brake, oil}) {
		t.Fatal("job line order must change the fingerprint (line 1 is the primary operation)")
	}
}

func TestLoadLocationCaches(t *testing.T) {
	a, err := loadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := loadLocation("Europe/London")
	if a != b {
		t.Fatal("expected cached *time.Location")
	}
	if _, err := loadLocation("Not/AZone"); err == nil {
		t.Fatal("expected error for unknown zone")
	}
}
