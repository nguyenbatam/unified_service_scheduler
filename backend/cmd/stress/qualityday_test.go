package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// qualityTestMixes are the script's services as the seeded catalog defines them (seed.go): OIL 30
// min express lubrication L1, BRAKE 90 min general brakes L2, MAJOR 180 min general general L3.
func qualityTestMixes() map[string]jobMix {
	mk := func(name string, minutes int, bay domain.BayType, skill domain.TechnicianSkill, level int16) jobMix {
		return jobMix{name: name, ids: []uuid.UUID{uuid.New()}, req: jobRequirement{minutes: minutes, bayType: bay, skills: []domain.TechnicianSkill{skill}, minLevel: level}}
	}
	return map[string]jobMix{
		"OIL":   mk("OIL", 30, domain.BayExpress, domain.SkillLubrication, 1),
		"BRAKE": mk("BRAKE", 90, domain.BayGeneral, domain.SkillBrakes, 2),
		"MAJOR": mk("MAJOR", 180, domain.BayGeneral, domain.SkillGeneral, 3),
	}
}

// shopFromDef is the oracle's model of def as provisioned (bays, technicians, weekly shifts; no
// holds), in definition order, with the resources' names by id.
func shopFromDef(t *testing.T, def workshopDef, loc *time.Location) (*oracleShop, map[uuid.UUID]string) {
	t.Helper()
	shop := &oracleShop{loc: loc, expressMax: workshopExpressMax, pinned: map[uuid.UUID]bool{}}
	names := map[uuid.UUID]string{}
	for _, b := range def.bays {
		id := uuid.New()
		names[id] = b.name
		shop.bays = append(shop.bays, oBay{id: id, bayType: b.bayType})
	}
	for _, td := range def.techs {
		tech := oTech{id: uuid.New(), level: td.level, skills: map[domain.TechnicianSkill]bool{}, roster: techRoster{shifts: map[int]shiftDef{}}}
		names[tech.id] = td.name
		for _, sk := range td.skills {
			tech.skills[sk] = true
		}
		for _, sh := range def.shifts[td.name] {
			parsed, err := parseShift(sh.StartTime, sh.EndTime, sh.BreakStart, sh.BreakEnd)
			if err != nil {
				t.Fatal(err)
			}
			tech.roster.shifts[int(sh.Weekday)] = parsed
		}
		shop.techs = append(shop.techs, tech)
	}
	return shop, names
}

// qualityTestDay is the quality day on def's hours, London, on a Wednesday.
func qualityTestDay(t *testing.T) (workshop, booking.Date, []qualityRequest) {
	t.Helper()
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	w := workshop{loc: london, def: qualityWorkshop, dealer: domain.Dealership{
		OpenTime: qualityWorkshop.open, CloseTime: qualityWorkshop.close, Policy: domain.Policy{ExpressMaxMinutes: workshopExpressMax},
	}}
	day := booking.Date{Year: 2030, Month: time.January, Day: 9}
	vehicles := make([]uuid.UUID, len(qualityScript))
	for i := range vehicles {
		vehicles[i] = uuid.New()
	}
	demand, err := qualityDemand(w, day, qualityTestMixes(), vehicles)
	if err != nil {
		t.Fatal(err)
	}
	return w, day, demand
}

// The script as requests: arrival order kept, exact local starts, one car each, inside the hours.
func TestQualityDemand(t *testing.T) {
	w, day, demand := qualityTestDay(t)
	if len(demand) != len(qualityScript) {
		t.Fatalf("%d requests, want %d", len(demand), len(qualityScript))
	}
	cars := map[uuid.UUID]bool{}
	asked := 0
	for i, r := range demand {
		st := qualityScript[i]
		if r.label != st.label || r.mix.name != st.service || r.waiter != st.waiter {
			t.Fatalf("request %d = %s %s waiter=%v, want the script's %+v", i, r.label, r.mix.name, r.waiter, st)
		}
		if got := r.start.In(w.loc).Format("15:04"); got != st.at || r.start.In(w.loc).Day() != day.Day {
			t.Fatalf("%s starts %s, want %s on %s", r.label, r.start, st.at, day)
		}
		cars[r.vehicle] = true
		asked += r.mix.req.minutes
	}
	if len(cars) != len(demand) || asked != 960 {
		t.Fatalf("%d cars for %d requests, %d minutes asked (want one car each, 960)", len(cars), len(demand), asked)
	}
	bad := w
	bad.dealer.CloseTime = "17:00" // the 15:30 major would run to 18:30
	if _, err := qualityDemand(bad, day, qualityTestMixes(), make([]uuid.UUID, len(qualityScript))); err == nil || !strings.Contains(err.Error(), "outside the opening hours") {
		t.Fatalf("a script running past closing: %v", err)
	}
	if _, err := qualityDemand(w, day, qualityTestMixes(), nil); err == nil {
		t.Fatal("too few cars accepted")
	}
}

// First fit loses on the script in every fixed order of the bays and technicians, naive or
// own-type-first, while the offline optimum fits the whole demand: the numbers the phase's margin
// (QUALITY_MARGIN_PCT) is chosen from.
func TestQualityFirstFitEveryOrder(t *testing.T) {
	w, _, demand := qualityTestDay(t)
	shop, names := shopFromDef(t, qualityWorkshop, w.loc)
	spread, err := firstFitEveryOrder(shop, demand)
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 * 6 * 120; spread.runs != want {
		t.Fatalf("%d replays, want %d (3! bay orders × 5! technician orders × 2 variants)", spread.runs, want)
	}
	if spread.best.minutes != 780 || spread.worst.minutes != 600 {
		t.Fatalf("first fit books %d … %d labour minutes over every order, want 600 … 780", spread.worst.minutes, spread.best.minutes)
	}
	label := func(run firstFitRun) []string {
		var refused []string
		for i, ok := range run.booked {
			if !ok {
				refused = append(refused, demand[i].label)
			}
		}
		return refused
	}
	// The best order lists the express bay first (naive: it put the waiter in the quick lane from
	// the start, so block B books all) and the lube + brakes technician first (block C loses the
	// brake job, not the major); block A loses its last brake job whatever the order.
	best := label(spread.best)
	if !slices.Equal(best, []string{"A4", "C2"}) || spread.best.ownTypeFirst || names[spread.best.bays[0]] != "Express 1" {
		t.Fatalf("best first fit refused %v (own type first %v, bays %v)", best, spread.best.ownTypeFirst, spread.best.bays)
	}
	if worst := label(spread.worst); !slices.Equal(worst, []string{"A4", "B3", "C4"}) {
		t.Fatalf("worst first fit refused %v, want A4 B3 C4", worst)
	}
	if opt := fixedOptimum(shop, demand); opt != 960 {
		t.Fatalf("offline optimum %d, want 960 (everything fits)", opt)
	}
	if err := qualityShortfall(960, spread.best.minutes, defaultQualityMarginPct); err != nil {
		t.Fatalf("everything booked must clear the margin: %v", err)
	}
	if err := qualityShortfall(960-90, spread.best.minutes, defaultQualityMarginPct); err == nil {
		t.Fatal("a server losing one brake job (one mechanism broken) must miss the margin")
	}
}

// The optimum search is exact on a small case where greedy loses: one bay and one technician, a
// major asked first that blocks three brake jobs worth more.
func TestFixedOptimumSkipsWhatBlocksMore(t *testing.T) {
	w, day, _ := qualityTestDay(t)
	shop, _ := shopFromDef(t, workshopDef{
		bays:  []bayDef{{"General 1", domain.BayGeneral}},
		techs: []techDef{{"Master", 3, []domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral}}},
	}, w.loc)
	mixes := qualityTestMixes()
	at := func(hh, mm int) time.Time { return booking.Clock{Hour: hh, Minute: mm}.On(day, w.loc) }
	demand := []qualityRequest{
		{label: "major", mix: mixes["MAJOR"], start: at(8, 0)},   // 08:00–11:00
		{label: "brake 1", mix: mixes["BRAKE"], start: at(7, 0)}, // 07:00–08:30
		{label: "brake 2", mix: mixes["BRAKE"], start: at(8, 30)},
		{label: "brake 3", mix: mixes["BRAKE"], start: at(10, 0)}, // 10:00–11:30
	}
	if got := fixedOptimum(shop, demand); got != 3*90 {
		t.Fatalf("optimum %d, want 270 (the three brake jobs instead of the major)", got)
	}
	if got, booked := firstFitFixed(shop, demand, false); got != 180 || !slices.Equal(booked, []bool{true, false, false, false}) {
		t.Fatalf("first fit %d %v: the major, then nothing", got, booked)
	}
}

func TestPermutations(t *testing.T) {
	got := permutations([]int{1, 2, 3})
	if len(got) != 6 {
		t.Fatalf("%d orders of 3, want 6", len(got))
	}
	seen := map[[3]int]bool{}
	for _, p := range got {
		seen[[3]int{p[0], p[1], p[2]}] = true
	}
	if len(seen) != 6 {
		t.Fatalf("duplicate orders: %v", got)
	}
	if len(permutations([]int{})) != 1 || factorial(5) != 120 {
		t.Fatal("empty order or 5!")
	}
}

func TestQualityShortfall(t *testing.T) {
	if err := qualityShortfall(115, 100, 15); err != nil {
		t.Fatalf("+15%% with a 15%% margin: %v", err)
	}
	if err := qualityShortfall(114, 100, 15); err == nil || !strings.Contains(err.Error(), "want at least +15%") {
		t.Fatalf("+14%%: %v", err)
	}
	if err := qualityShortfall(10, 0, 15); err == nil {
		t.Fatal("an empty baseline proves nothing")
	}
}
