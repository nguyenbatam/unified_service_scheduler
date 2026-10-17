package httpapi

import "testing"

// A budget below the dearest single availability read (a 31-day next-available search) would
// refuse that read forever, so WithCustomerLimits raises it; write budgets are taken as given.
func TestWithCustomerLimitsKeepsTheDearestReadPossible(t *testing.T) {
	s := &Server{}
	WithCustomerLimits(1, 2, 1, 2)(s)
	if s.availPerUser != MinAvailabilityLimit || s.availPerIP != MinAvailabilityLimit {
		t.Fatalf("availability budgets %d/%d, want at least %d", s.availPerUser, s.availPerIP, MinAvailabilityLimit)
	}
	if s.writePerUser != 1 || s.writePerIP != 2 {
		t.Fatalf("write budgets %d/%d, want 1/2", s.writePerUser, s.writePerIP)
	}
	if MinAvailabilityLimit != 5 {
		t.Fatalf("MinAvailabilityLimit = %d, want 5 (config.Load's floor for AVAILABILITY_LIMIT_*)", MinAvailabilityLimit)
	}
}
