package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The naive allocator really is naive: on a one-hour day with a general bay and a senior
// technician listed first, an oil change takes both (the express spill lets it use the general
// bay), and the brake job after it finds no start at all. A best-fit allocator would put the oil
// change in the express bay with the junior and book both.
func TestFirstFitDayIsNaive(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	day, err := booking.ParseDate("2030-01-07")
	if err != nil {
		t.Fatal(err)
	}
	w := workshop{dealer: domain.Dealership{OpenTime: "08:00", CloseTime: "09:00"}, loc: london}
	lube, general := domain.SkillLubrication, domain.SkillGeneral
	shop := &oracleShop{
		loc: london, expressMax: 120,
		bays: []oBay{{uuid.New(), domain.BayGeneral}, {uuid.New(), domain.BayExpress}},
		techs: []oTech{
			{id: uuid.New(), level: 3, skills: map[domain.TechnicianSkill]bool{lube: true, general: true}},
			{id: uuid.New(), level: 1, skills: map[domain.TechnicianSkill]bool{lube: true}},
		},
		holds: []oHold{{id: uuid.New(), window: timeWindow{day.Start(london), day.Next(london)}}}, // ignored
	}
	oil := jobMix{name: "OIL", req: jobRequirement{minutes: 30, bayType: domain.BayExpress, skills: []domain.TechnicianSkill{lube}, minLevel: 1}}
	gen := jobMix{name: "GEN", req: jobRequirement{minutes: 60, bayType: domain.BayGeneral, skills: []domain.TechnicianSkill{general}, minLevel: 2}}
	eight := day.Start(london).Add(8 * time.Hour)
	demand := []fillRequest{{mix: oil, desired: eight, vehicle: uuid.New()}, {mix: gen, desired: eight, vehicle: uuid.New()}}
	minutes, booked, err := firstFitDay(shop, w, day, demand)
	if err != nil {
		t.Fatal(err)
	}
	if minutes != 30 || booked != 1 {
		t.Fatalf("first fit booked %d min (%d customers), want 30 (1): the oil change blocks the brake job", minutes, booked)
	}
	if len(shop.holds) != 1 {
		t.Fatal("firstFitDay must not change the shop it was given")
	}
}

func TestFirstFitShortfall(t *testing.T) {
	if err := firstFitShortfall(98, 100, 2); err != nil {
		t.Fatalf("2%% below with a 2%% margin: %v", err)
	}
	if err := firstFitShortfall(97, 100, 2); err == nil || !strings.Contains(err.Error(), "below naive first fit") {
		t.Fatalf("3%% below: %v", err)
	}
	if err := firstFitShortfall(10, 0, 2); err == nil {
		t.Fatal("an empty baseline proves nothing")
	}
}
