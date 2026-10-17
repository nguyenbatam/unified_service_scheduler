package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestParsePromAndSum(t *testing.T) {
	text := `# HELP scheduler_bookings_total Booking attempts by result.
# TYPE scheduler_bookings_total counter
scheduler_bookings_total{instance_id="a",result="created"} 7
scheduler_bookings_total{instance_id="a",result="conflict"} 3
scheduler_bookings_total{instance_id="b",result="created"} 5
scheduler_db_pool_max_conns{instance_id="a"} 20
go_goroutines 42
garbage line
`
	s := parseProm(text)
	if got := promSum(s, "scheduler_bookings_total", map[string]string{"result": "created"}); got != 12 {
		t.Fatalf("created = %v, want 12", got)
	}
	if got := promSum(s, "scheduler_bookings_total", nil); got != 15 {
		t.Fatalf("all bookings = %v, want 15", got)
	}
	if got := promSum(s, "go_goroutines", nil); got != 42 {
		t.Fatalf("unlabelled series = %v, want 42", got)
	}
	if got := promSum(s, "scheduler_db_pool_max_conns", map[string]string{"instance_id": "b"}); got != 0 {
		t.Fatalf("absent label match = %v, want 0", got)
	}
}

func TestNearestOffered(t *testing.T) {
	base := time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	slots := []domain.Slot{
		{Start: at(0), Available: true}, {Start: at(30)}, {Start: at(60), Available: true}, {Start: at(120), Available: true},
	}
	cases := []struct {
		desired int
		want    int
	}{
		{30, 0},    // tie between 08:00 and 09:00: the earlier
		{90, 60},   // 09:00 and 10:00 tie at 30 min: the earlier
		{110, 120}, // closest
		{0, 0},
	}
	for _, tc := range cases {
		got, ok := nearestOffered(slots, at(tc.desired))
		if !ok || !got.Start.Equal(at(tc.want)) {
			t.Fatalf("desired +%d: got %v ok=%v, want +%d", tc.desired, got.Start, ok, tc.want)
		}
	}
	if _, ok := nearestOffered([]domain.Slot{{Start: at(0)}}, at(0)); ok {
		t.Fatal("nothing offered must report not found")
	}
}

// The fill day's demand is the same for a seed (reproducible runs) and differs between seeds.
func TestFillDemandIsReproducible(t *testing.T) {
	loc := time.UTC
	w := workshop{dealer: domain.Dealership{OpenTime: "08:00", CloseTime: "18:00", LunchStart: "12:00", LunchEnd: "13:00"}, loc: loc}
	day := booking.Date{Year: 2026, Month: time.March, Day: 2}
	mixes := []jobMix{{name: "OIL"}, {name: "BRAKE"}}
	vehicles := []uuid.UUID{uuid.New(), uuid.New()}
	a := fillDemand(1, 50, w, day, mixes, []int{3, 1}, vehicles)
	b := fillDemand(1, 50, w, day, mixes, []int{3, 1}, vehicles)
	c := fillDemand(2, 50, w, day, mixes, []int{3, 1}, vehicles)
	same, oil := true, 0
	for i := range a {
		if a[i].mix.name != b[i].mix.name || !a[i].desired.Equal(b[i].desired) || a[i].waiter != b[i].waiter {
			t.Fatalf("request %d differs for one seed", i)
		}
		same = same && a[i].mix.name == c[i].mix.name && a[i].desired.Equal(c[i].desired)
		if a[i].mix.name == "OIL" {
			oil++
		}
		if h := a[i].desired.In(loc).Hour(); h < 8 || h >= 18 {
			t.Fatalf("desired start %s outside the opening hours", a[i].desired)
		}
	}
	if same {
		t.Fatal("two seeds drew the same day")
	}
	if oil < 25 { // weight 3:1 over 50 draws
		t.Fatalf("OIL drawn %d of 50 with weight 3:1", oil)
	}
}

func TestMatrixVerdictsAndJSON(t *testing.T) {
	m := newMatrix()
	m.pass("R1", "x", "ok")
	m.pass("R2a", "x", "ok")
	m.fail("R2a", "y", "bad %d", 1)
	m.skip("R3", "z", "no dsn")
	if got := m.failed(); len(got) != 1 || got[0] != "R2a" {
		t.Fatalf("failed = %v, want [R2a]", got)
	}
	if v := rowVerdict(m.rows["R3"]); v != verdictSkip {
		t.Fatalf("skip-only row = %s", v)
	}
	if v := rowVerdict(nil); v != verdictSkip {
		t.Fatalf("empty row = %s", v)
	}
	path := filepath.Join(t.TempDir(), "report.json")
	if err := m.writeJSON(path, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rep jsonReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Passed || len(rep.Requirements) != len(requirements) {
		t.Fatalf("report passed=%v rows=%d, want false and %d", rep.Passed, len(rep.Requirements), len(requirements))
	}
}

func TestParseRates(t *testing.T) {
	got, err := parseRates(" 25, 50,100 ")
	if err != nil || len(got) != 3 || got[2] != 100 {
		t.Fatalf("parseRates = %v, %v", got, err)
	}
	for _, bad := range []string{"", "a", "10,-1", "0"} {
		if _, err := parseRates(bad); err == nil {
			t.Fatalf("parseRates(%q) accepted", bad)
		}
	}
}

// Bench requests never meet another workload's bookings, and "book" never puts more than
// benchBays bookings on one start.
func TestBenchSpreadKeepsWorkloadsApart(t *testing.T) {
	tgt := &benchTarget{shop: workshop{loc: time.UTC}, firstDay: booking.Date{Year: 2026, Month: time.March, Day: 2}, days: 5, vehicles: make([]uuid.UUID, 5*benchBays)}
	perStart := map[time.Time]int{}
	vehiclesAt := map[[2]int64]bool{}
	for n := range tgt.capacityBookings() {
		start, k := tgt.spread(n, 0)
		perStart[start]++
		key := [2]int64{start.Unix(), int64(k)}
		if vehiclesAt[key] {
			t.Fatalf("request %d: vehicle %d booked twice at %s", n, k, start)
		}
		vehiclesAt[key] = true
	}
	for s, c := range perStart {
		if c > benchBays {
			t.Fatalf("%s holds %d bookings, more than %d bays", s, c, benchBays)
		}
	}
	lastBook := booking.Clock{Hour: 8}.On(tgt.firstDay.AddDays(tgt.days), time.UTC)
	if hot := tgt.slot(0); hot.Before(lastBook) {
		t.Fatalf("hot round 0 at %s overlaps the book days", hot)
	}
	if j, _ := tgt.spread(0, tgt.journeyFirstDay()); j.Before(tgt.slot(20*hotDays - 1)) {
		t.Fatalf("journey starts at %s inside the hot days", j)
	}
}
