package booking

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// benchAvailability builds a busy dealership in memory: 12 bays, 20 technicians (most on a weekly
// shift with a break, some with absences), and a hold every 30 minutes on most bays for days days,
// the shape of a 31-day NextAvailable scan.
func benchAvailability(b *testing.B, days int) (availInputs, store.AvailabilityRange, Date, time.Time) {
	b.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		b.Fatal(err)
	}
	first := Date{Year: 2026, Month: time.March, Day: 2}
	dealer := domain.Dealership{
		ID: uuid.New(), Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "18:00",
		LunchStart: "12:00", LunchEnd: "13:00", StagingCapacity: 4, StagingWindowMinutes: 60,
	}
	in := availInputs{
		actor: domain.Actor{Role: domain.RoleAdmin}, dealer: dealer, loc: loc,
		req:    Requirement{DurationMinutes: 60, BayType: domain.BayGeneral, Skills: []domain.TechnicianSkill{domain.SkillGeneral}, MinSkillLevel: 1},
		weekly: map[int16]domain.WeekdayHours{},
	}
	data := store.AvailabilityRange{
		Rosters: store.Rosters{Shifts: map[uuid.UUID][]domain.TechnicianShift{}, Absences: map[uuid.UUID][]store.Busy{}},
	}
	for range 12 {
		data.Bays = append(data.Bays, domain.Bay{ID: uuid.New(), BayType: domain.BayGeneral})
	}
	for i := range 20 {
		id := uuid.New()
		data.Techs = append(data.Techs, domain.Technician{ID: id, SkillLevel: 2, Skills: []domain.TechnicianSkill{domain.SkillGeneral}})
		if i%4 != 0 {
			for wd := int16(1); wd <= 6; wd++ {
				data.Rosters.Shifts[id] = append(data.Rosters.Shifts[id], domain.TechnicianShift{
					Weekday: wd, StartTime: fmt.Sprintf("%02d:00", 7+i%3), EndTime: fmt.Sprintf("%02d:00", 15+i%3),
					BreakStart: "11:30", BreakEnd: "12:00",
				})
			}
		}
		if i%5 == 0 {
			at := Clock{Hour: 9}.On(first.AddDays(i%days), loc)
			data.Rosters.Absences[id] = append(data.Rosters.Absences[id], store.Busy{Start: at, End: at.Add(3 * time.Hour)})
		}
	}
	for d := range days {
		day := first.AddDays(d)
		for slot := range 20 {
			start := Clock{Hour: 8}.On(day, loc).Add(time.Duration(slot) * SlotStep)
			for k := range 9 {
				data.Busy = append(data.Busy, store.Hold{
					BayID: data.Bays[(slot+k)%len(data.Bays)].ID, TechnicianID: data.Techs[(slot*3+k)%len(data.Techs)].ID,
					Start: start, End: start.Add(SlotStep), ID: uuid.New(), Status: domain.StatusConfirmed, RequiredBayType: domain.BayGeneral, DurationMinutes: 30,
					RequiredSkills: []domain.TechnicianSkill{domain.SkillGeneral}, MinSkillLevel: 1})
			}
			if slot%4 == 0 {
				data.Arrivals = append(data.Arrivals, start)
			}
		}
		at := Clock{Hour: 15}.On(day, loc)
		data.VehicleHolds = append(data.VehicleHolds, store.Busy{Start: at, End: at.Add(time.Hour)})
	}
	return in, data, first, Clock{}.On(first.AddDays(-1), loc)
}

// BenchmarkDayAvailabilityScan is the pure-Go part of a 31-day NextAvailable once the range is
// read: every day evaluated against the whole range's preloaded holds and rosters.
func BenchmarkDayAvailabilityScan(b *testing.B) {
	const days = 31
	in, data, first, now := benchAvailability(b, days)
	rng, err := newAvailRange(in, data)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		for d := range days {
			if _, err := dayAvailability(in, rng, first.AddDays(d), now); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkNextAvailableInMemory is a whole 31-day NextAvailable without the database: indexing
// the range read (newAvailRange) plus the scan.
func BenchmarkNextAvailableInMemory(b *testing.B) {
	const days = 31
	in, data, first, now := benchAvailability(b, days)
	b.ReportAllocs()
	for b.Loop() {
		rng, err := newAvailRange(in, data)
		if err != nil {
			b.Fatal(err)
		}
		for d := range days {
			if _, err := dayAvailability(in, rng, first.AddDays(d), now); err != nil {
				b.Fatal(err)
			}
		}
	}
}
