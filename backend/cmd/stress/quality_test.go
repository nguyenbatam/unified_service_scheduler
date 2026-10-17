package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestTransportMaxFlow(t *testing.T) {
	cases := []struct {
		name     string
		supply   []int
		capacity []int
		edge     [][]bool
		want     int
	}{
		{"demand below capacity", []int{60}, []int{100}, [][]bool{{true}}, 60},
		{"capacity below demand", []int{300}, []int{100, 50}, [][]bool{{true, true}}, 150},
		{"no edge, no flow", []int{100}, []int{100}, [][]bool{{false}}, 0},
		// Class 0 may only use resource 0; class 1 either. A greedy class-1-first fill of resource
		// 0 would strand class 0: the max flow routes class 1 to resource 1.
		{"routing matters", []int{100, 100}, []int{100, 100}, [][]bool{{true, false}, {true, true}}, 200},
		{"shared bottleneck", []int{100, 100}, []int{120, 0}, [][]bool{{true, true}, {true, true}}, 120},
	}
	for _, tc := range cases {
		if got := transportMaxFlow(tc.supply, tc.capacity, tc.edge); got != tc.want {
			t.Fatalf("%s: flow = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// The bound on a small shop: one general and one express bay, open 08:00–12:00 (8 cells), one
// general technician on duty 08:00–10:00 only and one lube technician all day.
func TestCapacityBound(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	day, err := booking.ParseDate("2030-01-07") // a Monday
	if err != nil {
		t.Fatal(err)
	}
	w := workshop{dealer: domain.Dealership{OpenTime: "08:00", CloseTime: "12:00"}, loc: london}
	sh, _ := parseShift("08:00", "10:00", "", "")
	shop := &oracleShop{
		loc: london, expressMax: 120,
		bays: []oBay{{uuid.New(), domain.BayGeneral}, {uuid.New(), domain.BayExpress}},
		techs: []oTech{
			{id: uuid.New(), level: 3, skills: map[domain.TechnicianSkill]bool{domain.SkillGeneral: true}, roster: techRoster{shifts: map[int]shiftDef{1: sh}}},
			{id: uuid.New(), level: 1, skills: map[domain.TechnicianSkill]bool{domain.SkillLubrication: true}, roster: techRoster{shifts: map[int]shiftDef{}}},
		},
	}
	general := jobMix{name: "GEN", req: jobRequirement{minutes: 60, bayType: domain.BayGeneral, skills: []domain.TechnicianSkill{domain.SkillGeneral}, minLevel: 2}}
	oil := jobMix{name: "OIL", req: jobRequirement{minutes: 30, bayType: domain.BayExpress, skills: []domain.TechnicianSkill{domain.SkillLubrication}, minLevel: 1}}
	bound, bays, techs, err := capacityBound(shop, w, day, []demandClass{{mix: general, minutes: 600}, {mix: oil, minutes: 600}})
	if err != nil {
		t.Fatal(err)
	}
	// Bays: general 240 (its bay) + oil 240 (the express bay) = 480. Technicians: general only
	// 120 (on duty 08:00–10:00), oil 240 = 360.
	if bays != 480 || techs != 360 || bound != 360 {
		t.Fatalf("bound = %d (bays %d, techs %d), want 360 (480, 360)", bound, bays, techs)
	}
	// A waiting general job may also use the express bay (soft upgrade), but the technicians still
	// cap it.
	bound, bays, _, err = capacityBound(shop, w, day, []demandClass{{mix: general, waiter: true, minutes: 600}})
	if err != nil || bays != 480 || bound != 120 {
		t.Fatalf("waiter bound = %d (bays %d), %v; want 120 (480)", bound, bays, err)
	}
}

func TestReplayShortfall(t *testing.T) {
	if err := replayShortfall(900, 1000, 10); err != nil {
		t.Fatalf("10%% below with a 10%% margin: %v", err)
	}
	if err := replayShortfall(1100, 1000, 10); err != nil {
		t.Fatalf("the concurrent day packed better: %v", err)
	}
	if err := replayShortfall(899, 1000, 10); err == nil || !strings.Contains(err.Error(), "below the sequential") {
		t.Fatalf("10.1%% below passed: %v", err)
	}
	if err := replayShortfall(0, 0, 10); err == nil {
		t.Fatal("an empty replay proves nothing")
	}
}

// The refusal oracle: a refused start is false while a bay, a technician, staging room and the
// car are all free — the requester's own later booking left out.
func TestRefusalFeasible(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	start := time.Date(2030, 1, 7, 9, 0, 0, 0, london)
	bay, tech, car, other := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	oil := jobMix{name: "OIL", req: jobRequirement{minutes: 30, bayType: domain.BayExpress, skills: []domain.TechnicianSkill{1}, minLevel: 1}}
	shop := func(holds ...oHold) *oracleShop {
		s := &oracleShop{
			loc: london, bays: []oBay{{bay, domain.BayExpress}},
			techs:        []oTech{{id: tech, level: 1, skills: map[domain.TechnicianSkill]bool{1: true}, roster: techRoster{shifts: map[int]shiftDef{}}}},
			holds:        holds,
			vehicleHolds: map[uuid.UUID][]oHold{},
		}
		for _, h := range holds {
			s.vehicleHolds[h.vehicle] = append(s.vehicleHolds[h.vehicle], h)
		}
		return s
	}
	own := oHold{id: uuid.New(), bay: bay, tech: tech, vehicle: car, status: 1, window: timeWindow{start, start.Add(30 * time.Minute)}}
	r := refusal{mix: oil, start: start, vehicle: car}
	if !shop().refusalFeasible(r) {
		t.Fatal("an empty shop refused: false refusal expected")
	}
	if shop(own).refusalFeasible(r) {
		t.Fatal("the slot is taken now: the refusal cannot be judged false")
	}
	r.exclude = own.id
	if !shop(own).refusalFeasible(r) {
		t.Fatal("only the requester's own later booking holds the slot: the refusal was false")
	}
	busyCar := oHold{id: uuid.New(), bay: uuid.New(), tech: uuid.New(), vehicle: car, status: 1, window: timeWindow{start.Add(-time.Hour), start.Add(15 * time.Minute)}}
	if shop(busyCar).refusalFeasible(refusal{mix: oil, start: start, vehicle: car}) {
		t.Fatal("the car is elsewhere: not judgeable as false")
	}
	staged := shop(oHold{id: uuid.New(), bay: uuid.New(), tech: uuid.New(), vehicle: other, status: 1, window: timeWindow{start.Add(15 * time.Minute), start.Add(time.Hour)}})
	staged.stagingCap, staged.stagingWindow = 1, 30
	if staged.refusalFeasible(refusal{mix: oil, start: start, vehicle: car}) {
		t.Fatal("the 09:00–09:30 arrival window is full: not a false refusal")
	}
	staged.stagingWindow = 15
	if !staged.refusalFeasible(refusal{mix: oil, start: start, vehicle: car}) {
		t.Fatal("with 15-minute windows 09:15 is another window: the refusal was false")
	}
}

func TestRefusalLogDedupesAndOrders(t *testing.T) {
	l := newRefusalLog()
	oil := jobMix{name: "OIL", req: jobRequirement{minutes: 30}}
	s := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	car := uuid.New()
	l.add(refusal{mix: oil, start: s, vehicle: car}, refusal{mix: oil, start: s, vehicle: car}, refusal{mix: oil, start: s.Add(time.Hour), vehicle: car})
	if l.len() != 2 || len(l.list()) != 2 {
		t.Fatalf("log holds %d, want 2 distinct", l.len())
	}
	if got := (refusalVerdict{}).String(); !strings.Contains(got, "vacuous") {
		t.Fatalf("an empty verdict must say it is vacuous: %q", got)
	}
}
