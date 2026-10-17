package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func poolScrape(instance string, waitSeconds, acquired, maxConns float64) []promSample {
	l := map[string]string{"instance_id": instance}
	return []promSample{
		{"scheduler_db_pool_empty_acquire_wait_seconds_total", l, waitSeconds},
		{"scheduler_db_pool_acquired_conns", l, acquired},
		{"scheduler_db_pool_max_conns", l, maxConns},
	}
}

func TestJudgePool(t *testing.T) {
	before := append(append(poolScrape("a", 1.0, 2, 20), poolScrape("b", 3.0, 2, 20)...),
		promSample{"process_start_time_seconds", map[string]string{"instance_id": "a"}, 1000})
	sampled := func(a, b float64) poolSamples {
		return poolSamples{maxAcquired: map[string]float64{"a": a, "b": b}, maxConns: map[string]float64{"a": 20, "b": 20}, samples: 10}
	}
	cases := []struct {
		name    string
		after   []promSample
		samples poolSamples
		ok      bool
		unknown bool
		want    string
	}{
		{"gate holds: little wait, headroom", append(poolScrape("a", 1.2, 3, 20), poolScrape("b", 3.3, 3, 20)...), sampled(6, 8), true, false, "peak acquired 6/20"},
		{"wait over budget", append(poolScrape("a", 3.5, 3, 20), poolScrape("b", 3.0, 3, 20)...), sampled(6, 8), false, false, "budget 2000 ms"},
		{"a backend hit its cap", append(poolScrape("a", 1.0, 3, 20), poolScrape("b", 3.0, 3, 20)...), sampled(20, 8), false, false, "reached its pool cap"},
		{"counters went backwards (restart)", append(poolScrape("a", 0.1, 3, 20), poolScrape("b", 3.0, 3, 20)...), sampled(6, 8), true, true, ""},
		{"restarted, the new counter past the old (process start changed)", append(append(poolScrape("a", 30.0, 3, 20), poolScrape("b", 3.0, 3, 20)...),
			promSample{"process_start_time_seconds", map[string]string{"instance_id": "a"}, 2000}), sampled(6, 8), true, true, ""},
		{"nothing sampled", append(poolScrape("a", 1.0, 3, 20), poolScrape("b", 3.0, 3, 20)...), poolSamples{}, false, false, "no sample"},
		{"most scrapes failed", append(poolScrape("a", 1.2, 3, 20), poolScrape("b", 3.3, 3, 20)...),
			poolSamples{maxAcquired: map[string]float64{"a": 6}, maxConns: map[string]float64{"a": 20}, samples: 3, attempts: 20, scrapeErrs: 17,
				backendTries: map[string]int{"http://a": 10, "http://b": 10}, backendErrs: map[string]int{"http://a": 7, "http://b": 10}},
			false, false, "7 of 10 scrapes of http://a failed"},
		{"one backend never read (exactly half the scrapes)", append(poolScrape("a", 1.2, 3, 20), poolScrape("b", 3.3, 3, 20)...),
			poolSamples{maxAcquired: map[string]float64{"a": 6}, maxConns: map[string]float64{"a": 20}, samples: 10, attempts: 20, scrapeErrs: 10,
				backendTries: map[string]int{"http://a": 10, "http://b": 10}, backendErrs: map[string]int{"http://b": 10}},
			false, false, "10 of 10 scrapes of http://b failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := judgePool(before, tc.after, tc.samples, 2000, true)
			if v.ok != tc.ok || v.unknown != tc.unknown || !strings.Contains(v.detail, tc.want) {
				t.Fatalf("judgePool = %+v, want ok=%v unknown=%v detail containing %q", v, tc.ok, tc.unknown, tc.want)
			}
		})
	}

	// -pool-cap-fails=false: the cap is reported, the wait budget still fails the phase.
	atCap := append(poolScrape("a", 1.0, 3, 20), poolScrape("b", 3.0, 3, 20)...)
	if v := judgePool(before, atCap, sampled(20, 8), 2000, false); !v.ok || !strings.Contains(v.detail, "reached its pool cap") {
		t.Fatalf("cap not judged: judgePool = %+v, want ok with the cap noted", v)
	}
	overBudget := append(poolScrape("a", 3.5, 3, 20), poolScrape("b", 3.0, 3, 20)...)
	if v := judgePool(before, overBudget, sampled(20, 8), 2000, false); v.ok || !strings.Contains(v.detail, "budget 2000 ms") {
		t.Fatalf("cap not judged, wait over budget: judgePool = %+v, want a failure", v)
	}
}

func TestEnforceRequired(t *testing.T) {
	m := newMatrix()
	m.pass("R1", "x", "booked")
	m.skip("Q6", "metrics", "no DSN")
	m.fail("Q1", "x", "broken")
	unmet := m.enforceRequired([]string{"R1", "Q6", "Q7", "Q1"})
	if strings.Join(unmet, ",") != "Q6,Q7" {
		t.Fatalf("unmet = %v, want Q6 (skip only) and Q7 (nothing ran); Q1 already failed, R1 passed", unmet)
	}
	if got := strings.Join(m.failed(), ","); got != "Q1,Q6,Q7" {
		t.Fatalf("failed = %s, want Q1,Q6,Q7", got)
	}
	if !validRequirement("R2a") || validRequirement("R9") {
		t.Fatal("validRequirement")
	}
}

func TestSettingsRejectUnknownRequirement(t *testing.T) {
	s := settings{phases: []string{"river"}, mode: "stress", require: []string{"R1", "Q8"}}
	if err := s.normalize(); err == nil || !strings.Contains(err.Error(), `"Q8"`) {
		t.Fatalf("normalize = %v, want the unknown id named", err)
	}
	s = settings{phases: []string{"river"}, mode: "stress", require: []string{"Q4/"}}
	if err := s.normalize(); err == nil {
		t.Fatal("normalize accepted a named check without a name")
	}
	s = settings{phases: []string{"river"}, mode: "stress", chaosRestart: true, mixedOps: 3}
	if err := s.normalize(); err == nil || !strings.Contains(err.Error(), "-mixed-ops 0") {
		t.Fatalf("normalize = %v, want -chaos-restart to refuse mixed ops", err)
	}
}

// "all" is every row plus the essential named checks; a named check must have passed on its own,
// whatever else its row proved.
func TestRequireAllAndNamedEvidence(t *testing.T) {
	all, err := parseRequire([]string{"all", "R1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"R1", "R2a", "Q7", "Q4/ws order", "Q4/ws"} {
		if !slices.Contains(all, want) {
			t.Fatalf("all = %v, missing %s", all, want)
		}
	}
	if len(all) != len(requirements)+len(essentialEvidence) {
		t.Fatalf("all = %v: duplicates kept", all)
	}
	m := newMatrix()
	m.pass("Q4", "side", "reads ok")
	m.pass("Q4", "ws", "fan-out ok")
	m.skip("Q4", "ws order", "nothing changed")
	m.info("Q5", "fill", "a fact")
	if v := rowVerdict(m.rows["Q4"]); v != verdictPass {
		t.Fatalf("Q4 = %s: one pass is enough for the row", v)
	}
	if v := rowVerdict(m.rows["Q5"]); v != verdictSkip {
		t.Fatalf("an INFO-only row = %s, want SKIP (a fact proves nothing)", v)
	}
	unmet := m.enforceRequired([]string{"Q4", "Q4/ws", "Q4/ws order", "Q5"})
	if strings.Join(unmet, ",") != "Q4/ws order,Q5" {
		t.Fatalf("unmet = %v, want the skipped named check and the INFO-only row", unmet)
	}
	if got := strings.Join(m.failed(), ","); got != "Q4,Q5" {
		t.Fatalf("failed = %s", got)
	}
}

// The compact summary names every skipped check of a row, and the Markdown copy appends.
func TestSummaryNamesSkippedChecks(t *testing.T) {
	m := newMatrix()
	m.pass("R2b", "truth", "ok")
	m.skip("R2b", "customers", "nobody turned away")
	m.headline("R2b", "%d slot verdicts", 42)
	var r2b rowSummary
	for _, s := range m.summaries() {
		if s.id == "R2b" {
			r2b = s
		}
	}
	if r2b.verdict != verdictPass || r2b.headline != "42 slot verdicts" || !strings.Contains(r2b.counts(), "skipped: customers") {
		t.Fatalf("summary %+v / %q", r2b, r2b.counts())
	}
	path := filepath.Join(t.TempDir(), "summary.md")
	for range 2 {
		if err := m.appendMarkdown(path, "run", nil); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(raw), "| PASS | R2b | 42 slot verdicts |"); n != 2 {
		t.Fatalf("summary.md has %d R2b rows, want 2 (appended):\n%s", n, raw)
	}
}

// A malformed or negative environment default fails the settings instead of silently running
// with the built-in default; unset or empty keeps it.
func TestEnvDefaultsReportBadValues(t *testing.T) {
	t.Setenv("STRESS_T_INT", "12x")
	t.Setenv("STRESS_T_NEG", "-3")
	t.Setenv("STRESS_T_DUR", "12")
	t.Setenv("STRESS_T_FLOAT", "NaN")
	t.Setenv("STRESS_T_BOOL", "on")
	t.Setenv("STRESS_T_EMPTY", " ")
	t.Setenv("STRESS_T_OK", "7")
	var e envDefaults
	if e.int("STRESS_T_INT", 1) != 1 || e.int("STRESS_T_NEG", 2) != 2 || e.duration("STRESS_T_DUR", time.Second) != time.Second ||
		e.float("STRESS_T_FLOAT", 3) != 3 || e.bool("STRESS_T_BOOL") {
		t.Fatal("a bad value must leave the default in place")
	}
	if len(e.errs) != 5 {
		t.Fatalf("errors %v, want one per bad variable", e.errs)
	}
	if e.int("STRESS_T_EMPTY", 4) != 4 || e.int("STRESS_T_UNSET", 5) != 5 || e.int("STRESS_T_OK", 0) != 7 || len(e.errs) != 5 {
		t.Fatalf("empty/unset/valid values: errors %v", e.errs)
	}
	if !strings.Contains(e.errs[0].Error(), "STRESS_T_INT") {
		t.Fatalf("error %v does not name the variable", e.errs[0])
	}
}

func TestMixVacuity(t *testing.T) {
	m := newMixStats()
	m.add(opReassign, "busy", true, "")
	m.add(opCancel, "conflict", true, "")
	if err := m.vacuity(); err == nil || !strings.Contains(err.Error(), "reassign never applied") || !strings.Contains(err.Error(), "no stale") {
		t.Fatalf("vacuity = %v, want every missing success and the missing 412 named", err)
	}
	for _, op := range []mixOp{opReassign, opReschedule, opCancel} {
		m.add(op, "ok", true, "")
	}
	m.add(opReschedule, "stale_412", true, "")
	if err := m.vacuity(); err == nil || !strings.Contains(err.Error(), "burst") || !strings.Contains(err.Error(), "exactly one winner") {
		t.Fatalf("vacuity = %v, want the missing burst and race named", err)
	}
	m.addN(opBurst, "versions", 3)
	m.add(opRace, "one_winner", true, "")
	if err := m.vacuity(); err != nil {
		t.Fatalf("every op applied, one stale 412, a burst and a won race: %v", err)
	}
}

func TestRaceVerdict(t *testing.T) {
	t0 := time.Date(2030, 1, 7, 14, 0, 0, 0, time.UTC)
	targets := []time.Time{t0, t0.Add(30 * time.Minute), t0.Add(time.Hour)}
	cur := domain.Appointment{Version: 3, StartAt: t0.Add(-time.Hour)}
	moved := func(v int, at time.Time) domain.Appointment { return domain.Appointment{Version: v, StartAt: at} }
	cases := []struct {
		name  string
		codes []int
		after domain.Appointment
		label string
		ok    bool
	}{
		{"one winner, the rest stale", []int{412, 200, 412}, moved(4, targets[1]), "one_winner", true},
		{"a taken start, then a winner", []int{409, 200, 412}, moved(4, targets[1]), "one_winner", true},
		{"two winners: lost update", []int{200, 200, 412}, moved(5, targets[1]), "lost_update", false},
		{"winner not applied", []int{412, 200, 412}, moved(4, targets[0]), "winner_not_applied", false},
		{"changed without a winner", []int{409, 409, 422}, moved(4, targets[2]), "changed_without_winner", false},
		{"nobody won", []int{409, 422, 503}, cur, "inconclusive", true},
		{"a 500 is a bug", []int{500, 200, 412}, moved(4, targets[1]), "unexpected_500", false},
	}
	for _, tc := range cases {
		label, ok, _ := raceVerdict(tc.codes, targets, cur, tc.after)
		if label != tc.label || ok != tc.ok {
			t.Fatalf("%s: raceVerdict = %s/%v, want %s/%v", tc.name, label, ok, tc.label, tc.ok)
		}
	}
}

func TestBenchRoomGuardsHotRounds(t *testing.T) {
	tgt := &benchTarget{days: 150}
	s := settings{benchDays: 150}
	s.bench = benchConfig{workloads: []string{"hot"}, step: 10 * time.Second, rates: map[string][]float64{"hot": {100, 400, 800, 1600}}}
	if err := benchRoom(s, tgt, 365); err != nil {
		t.Fatalf("the default hot ramp (29000 requests = 290 rounds of 800): %v", err)
	}
	s.bench.rates["hot"] = []float64{100, 400, 800, 1600, 6400}
	if err := benchRoom(s, tgt, 365); err == nil || !strings.Contains(err.Error(), "fresh starts") {
		t.Fatalf("a hot ramp past its days must be refused: %v", err)
	}
	s.bench.workloads = []string{"journey"}
	s.bench.rates["journey"] = []float64{1}
	if err := benchRoom(s, &benchTarget{days: 150}, 365); err != nil {
		t.Fatalf("150 days: the journey's last day is 340, inside a 365-day horizon: %v", err)
	}
	if err := benchRoom(s, &benchTarget{days: 170}, 365); err == nil || !strings.Contains(err.Error(), "bookingHorizonDays") {
		t.Fatalf("170 days put the journey's last day (380) past the horizon: %v", err)
	}
	s.bench.workloads = []string{"book"}
	s.bench.rates["book"] = []float64{1000, 2000}
	if err := benchRoom(s, &benchTarget{days: 10}, 365); err == nil || !strings.Contains(err.Error(), "raise -bench-days") {
		t.Fatalf("a book ramp past its days must be refused: %v", err)
	}
}

// Journey requests spread over customers first (no one customer's write budget runs out), and a
// customer's car never gets the same start twice before days × 20 of its requests.
func TestSpreadAcrossCustomers(t *testing.T) {
	day, _ := booking.ParseDate("2030-01-07")
	tgt := &benchTarget{firstDay: day, days: 3, shop: workshop{loc: time.UTC}}
	seen := map[string]bool{}
	for n := range 3 * 20 * 4 {
		start, who := tgt.spreadAcross(n, 0, 4)
		if who != n%4 {
			t.Fatalf("request %d went to customer %d, want %d", n, who, n%4)
		}
		key := fmt.Sprintf("%s|%d", start, who)
		if seen[key] {
			t.Fatalf("customer %d got %s twice", who, start)
		}
		seen[key] = true
	}
}
