package main

import (
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// Each truth-phase failure lands on the row of the requirement it breaks, not all under R2b.
func TestTruthFailuresFileUnderTheirRequirement(t *testing.T) {
	st := &truthStats{booked: 1, fullProbes: 1, partialProbes: 1, full: true}
	st.failf("R2b", "false offer")
	st.failf("Q5", "bad move")
	st.failf("R2a", "overbooked probe")
	m := newMatrix()
	st.record(m, booking.Date{Year: 2026, Month: 3, Day: 2})
	want := map[string]string{"false offer": "R2b", "bad move": "Q5", "overbooked probe": "R2a"}
	for req, rows := range m.rows {
		for _, e := range rows {
			if wantReq, ok := want[e.Detail]; ok && (req != wantReq || e.Verdict != verdictFail) {
				t.Fatalf("%q filed as %s under %s, want FAIL under %s", e.Detail, e.Verdict, req, wantReq)
			}
			delete(want, e.Detail)
		}
	}
	if len(want) > 0 {
		t.Fatalf("failures not recorded: %v", want)
	}
}
