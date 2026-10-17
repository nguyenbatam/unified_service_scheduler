package main

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// One general bay holds X (08:00–09:00), another holds Y (08:30–09:30): nothing is free for a
// 08:30–09:00 job. A third bay free for X's window lets X move there (repair); a checked-in X, a
// phantom, an X changed during the phase, or a third bay busy too do not (Y is checked in: it
// never moves).
func TestOracleRepairRule(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	day := time.Date(2030, 1, 7, 0, 0, 0, 0, london)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	general := jobRequirement{minutes: 30, bayType: domain.BayGeneral, skills: []domain.TechnicianSkill{domain.SkillGeneral}, minLevel: 1}
	b1, b2, b3 := uuid.New(), uuid.New(), uuid.New()
	t1, t2 := uuid.New(), uuid.New()
	skills := map[domain.TechnicianSkill]bool{domain.SkillGeneral: true}
	x := oHold{id: uuid.New(), bay: b1, tech: t1, status: 1, window: timeWindow{at(8, 0), at(9, 0)}, req: general}
	y := oHold{id: uuid.New(), bay: b2, tech: t2, status: 2, window: timeWindow{at(8, 30), at(9, 30)}, req: general}
	shop := func(holds ...oHold) *oracleShop {
		return &oracleShop{
			loc: london, now: day, pinned: map[uuid.UUID]bool{},
			bays:  []oBay{{b1, domain.BayGeneral}, {b2, domain.BayGeneral}, {b3, domain.BayGeneral}},
			techs: []oTech{{id: t1, level: 1, skills: skills}, {id: t2, level: 1, skills: skills}},
			holds: holds,
		}
	}
	w := timeWindow{at(8, 30), at(9, 0)}
	if s := shop(x, y); !s.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("X can move to the free third bay")
	}
	busy3 := oHold{id: uuid.New(), bay: b3, tech: uuid.New(), status: 1, window: timeWindow{at(8, 45), at(9, 45)}}
	if s := shop(x, y, busy3); s.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("every bay is busy over the window and no hold has a free bay to go to")
	}
	checkedIn := x
	checkedIn.status = int16(domain.StatusCheckedIn)
	if s := shop(checkedIn, y); s.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("a checked-in job never moves")
	}
	ghost := x
	ghost.phantom = true
	if s := shop(ghost, y); s.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("a phantom is a past position, never moved")
	}
	s := shop(x, y)
	s.pinned[x.id] = true
	if s.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("a job changed during the phase is not counted as movable")
	}
	started := shop(x, y)
	started.now = at(8, 0)
	if started.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("a job that has started never moves")
	}
	// Technicians: both busy over w; X's technician t1 holds only X, and t2 is busy for X's window,
	// so no move; with a third technician free, X can move to them.
	if s := shop(x, y); s.techRepairable(general, w, uuid.Nil) {
		t.Fatal("no technician is free for X's window")
	}
	s = shop(x, y)
	s.techs = append(s.techs, oTech{id: uuid.New(), level: 1, skills: skills})
	if !s.techRepairable(general, w, uuid.Nil) || !s.fits(general, false, w, uuid.Nil, 0, 0) {
		t.Fatal("X can move to the free third technician, and then the job fits")
	}
}

// A phantom of a hold's own position (historyHolds returns every unchanged appointment as it was
// before the phase) is one appointment with it: it neither fills a staging window twice nor makes
// the bay look doubly held, and an identical copy is dropped outright.
func TestOraclePhantomOfALiveHold(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	day := time.Date(2030, 1, 7, 0, 0, 0, 0, london)
	at := func(h, m int) time.Time { return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }
	general := jobRequirement{minutes: 30, bayType: domain.BayGeneral, skills: []domain.TechnicianSkill{domain.SkillGeneral}, minLevel: 1}
	b1, b2, b3 := uuid.New(), uuid.New(), uuid.New()
	t1, t2 := uuid.New(), uuid.New()
	skills := map[domain.TechnicianSkill]bool{domain.SkillGeneral: true}
	x := oHold{id: uuid.New(), bay: b1, tech: t1, status: 1, window: timeWindow{at(8, 0), at(9, 0)}, req: general}
	y := oHold{id: uuid.New(), bay: b2, tech: t2, status: 2, window: timeWindow{at(8, 30), at(9, 30)}, req: general}
	ghost := x
	ghost.phantom = true
	s := &oracleShop{
		loc: london, now: day, pinned: map[uuid.UUID]bool{}, stagingCap: 3, stagingWindow: 60,
		bays:  []oBay{{b1, domain.BayGeneral}, {b2, domain.BayGeneral}, {b3, domain.BayGeneral}},
		techs: []oTech{{id: t1, level: 1, skills: skills}, {id: t2, level: 1, skills: skills}},
		holds: []oHold{x, ghost, y},
	}
	w := timeWindow{at(8, 30), at(9, 0)}
	if !s.bayRepairable(general, false, w, uuid.Nil) {
		t.Fatal("X and its own phantom are one holder of bay 1: X can still move to the free third bay")
	}
	// 08:00 window: X (and its phantom) is one arrival, Y (08:30) another → 2 < 3.
	if !s.stagingRoom(at(8, 0), uuid.Nil) {
		t.Fatal("two distinct arrivals in a window of three: room left (the phantom is not a third)")
	}
	s.holds = append(s.holds, oHold{id: uuid.New(), bay: uuid.New(), tech: uuid.New(), status: 1, window: timeWindow{at(8, 0), at(8, 30)}})
	if s.stagingRoom(at(8, 0), uuid.Nil) {
		t.Fatal("three distinct arrivals fill a window of three")
	}
	moved := x
	moved.bay = b3
	got := withoutLiveDuplicates([]oHold{x, y}, []oHold{x, y, moved, moved})
	if len(got) != 1 || got[0].bay != b3 {
		t.Fatalf("phantoms kept = %+v, want only X's other position, once", got)
	}
}
