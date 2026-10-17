package booking

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// dayAvailability without a database: the reason precedence (too early → lunch → vehicle → bay /
// technician → staging), the free counts, rosters, and the one-move repair, on a small shop — a
// quick lane (Bay A), a general bay (Bay B), Tom (level 2, lubrication + brakes) and Una (level 1,
// lubrication) — open 08:00–12:00 with lunch 10:00–10:30, for a 60-minute brake job.
func TestDayAvailabilityReasons(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	day := Date{Year: 2026, Month: time.March, Day: 2} // a Monday
	at := func(h, m int) time.Time { return Clock{Hour: h, Minute: m}.On(day, loc) }
	bayA, bayB := uuid.New(), uuid.New()
	tom, una, vic, wes := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	lube := []domain.TechnicianSkill{domain.SkillLubrication}
	lubeBrakes := []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes}
	brake := Requirement{DurationMinutes: 60, BayType: domain.BayGeneral, Skills: []domain.TechnicianSkill{domain.SkillBrakes}, MinSkillLevel: 2}
	// oil is a confirmed 30-minute oil change at h:m on bay and tech (status overridable).
	oil := func(bay, tech uuid.UUID, h, m int, status domain.AppointmentStatus) store.Hold {
		return store.Hold{
			BayID: bay, TechnicianID: tech, Start: at(h, m), End: at(h, m).Add(30 * time.Minute),
			ID: uuid.New(), Status: status, RequiredBayType: domain.BayExpress, DurationMinutes: 30,
			RequiredSkills: lube, MinSkillLevel: 1,
		}
	}
	staff := domain.Actor{Role: domain.RoleDispatcher}
	customer := domain.Actor{Role: domain.RoleCustomer, CustomerID: uuid.New()}

	type want struct {
		available        bool
		reason           string
		freeBays, techs  int
		countsUnreported bool // the slot was refused before the counts (too early, lunch, vehicle)
	}
	cases := []struct {
		name   string
		actor  domain.Actor
		now    time.Time
		req    Requirement
		techs  []domain.Technician // nil: Tom and Una
		setup  func(*store.AvailabilityRange, *domain.Dealership)
		slotAt time.Time
		want   want
	}{
		{name: "free", slotAt: at(9, 0), want: want{available: true, freeBays: 1, techs: 1}},
		{name: "lunch before vehicle", slotAt: at(9, 30),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.VehicleHolds = []store.Busy{{Start: at(9, 30), End: at(10, 30)}}
			}, want: want{reason: "lunch_break", countsUnreported: true}},
		{name: "lead time before lunch", actor: customer, now: at(9, 0), slotAt: at(9, 30),
			want: want{reason: ReasonLeadTime, countsUnreported: true}},
		{name: "past", now: at(9, 5), slotAt: at(9, 0), want: want{reason: ReasonPast, countsUnreported: true}},
		{name: "vehicle busy", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.VehicleHolds = []store.Busy{{Start: at(8, 30), End: at(9, 30)}}
			}, want: want{reason: ReasonVehicleBusy, countsUnreported: true}},
		{name: "no bay: checked-in work never moves", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayB, una, 9, 0, domain.StatusCheckedIn)}
			}, want: want{reason: "no_bay", freeBays: 0, techs: 1}},
		{name: "no technician: off shift", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Rosters.Shifts[tom] = []domain.TechnicianShift{{Weekday: 1, StartTime: "10:30", EndTime: "12:00"}}
			}, want: want{reason: "no_technician", freeBays: 1, techs: 0}},
		{name: "no technician: absent", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Rosters.Absences[tom] = []store.Busy{{Start: at(8, 0), End: at(9, 30)}}
			}, want: want{reason: "no_technician", freeBays: 1, techs: 0}},
		{name: "no resources", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayB, tom, 9, 0, domain.StatusInProgress)}
			}, want: want{reason: "no_resources", freeBays: 0, techs: 0}},
		{name: "staging full after the counts", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, dealer *domain.Dealership) {
				dealer.StagingCapacity = 1
				d.Arrivals = []time.Time{at(9, 15)}
			}, want: want{reason: "staging_full", freeBays: 1, techs: 1}},
		{name: "repair: an oil change in the general bay moves to the quick lane", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayB, una, 9, 0, domain.StatusConfirmed)}
			}, want: want{available: true, freeBays: 0, techs: 1}},
		{name: "repair: none when the quick lane is busy too", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayB, una, 9, 0, domain.StatusConfirmed), oil(bayA, tom, 9, 15, domain.StatusInProgress)}
			}, want: want{reason: "no_resources", freeBays: 0, techs: 0}},
		{name: "repair: not a booking that has started", now: at(9, 10), slotAt: at(9, 30),
			req: Requirement{DurationMinutes: 30, BayType: domain.BayGeneral, Skills: []domain.TechnicianSkill{domain.SkillBrakes}, MinSkillLevel: 2},
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				h := oil(bayB, una, 9, 0, domain.StatusConfirmed)
				h.End = at(10, 0) // 09:00–10:00, started at 09:10
				d.Busy = []store.Hold{h}
			}, want: want{reason: "no_bay", freeBays: 0, techs: 1}},
		{name: "repair: not when two bookings hold the bay", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayB, una, 9, 0, domain.StatusConfirmed), oil(bayB, una, 9, 30, domain.StatusConfirmed)}
			}, want: want{reason: "no_bay", freeBays: 0, techs: 1}},
		{name: "repair: Tom's oil change moves to Una", slotAt: at(9, 0),
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayA, tom, 9, 0, domain.StatusConfirmed)}
			}, want: want{available: true, freeBays: 1, techs: 0}},
		{name: "repair: not onto a more senior technician", slotAt: at(9, 0),
			techs: []domain.Technician{{ID: tom, SkillLevel: 2, Skills: lubeBrakes}, {ID: vic, SkillLevel: 4, Skills: lube}},
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				d.Busy = []store.Hold{oil(bayA, tom, 9, 0, domain.StatusConfirmed)}
			}, want: want{reason: "no_technician", freeBays: 1, techs: 0}},
		{name: "repair: onto a technician of equal standing", slotAt: at(9, 0),
			techs: []domain.Technician{{ID: tom, SkillLevel: 2, Skills: lubeBrakes}, {ID: wes, SkillLevel: 2, Skills: lubeBrakes}},
			setup: func(d *store.AvailabilityRange, _ *domain.Dealership) {
				// Wes is busy for the brake job's second half only: free for the oil change's window.
				d.Busy = []store.Hold{oil(bayA, tom, 9, 0, domain.StatusConfirmed), oil(bayA, wes, 9, 30, domain.StatusInProgress)}
			}, want: want{available: true, freeBays: 1, techs: 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dealer := domain.Dealership{
				ID: uuid.New(), Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "12:00",
				LunchStart: "10:00", LunchEnd: "10:30", StagingWindowMinutes: 60,
				Policy: domain.Policy{CustomerLeadMinutes: 60, BookingHorizonDays: 90, ExpressMaxMinutes: 60},
			}
			techs := tc.techs
			if techs == nil {
				techs = []domain.Technician{{ID: tom, SkillLevel: 2, Skills: lubeBrakes}, {ID: una, SkillLevel: 1, Skills: lube}}
			}
			data := store.AvailabilityRange{
				Bays:    []domain.Bay{{ID: bayA, BayType: domain.BayExpress}, {ID: bayB, BayType: domain.BayGeneral}},
				Techs:   techs,
				Rosters: store.Rosters{Shifts: map[uuid.UUID][]domain.TechnicianShift{}, Absences: map[uuid.UUID][]store.Busy{}},
			}
			if tc.setup != nil {
				tc.setup(&data, &dealer)
			}
			actor, now, req := tc.actor, tc.now, tc.req
			if actor.Role == 0 {
				actor = staff
			}
			if now.IsZero() {
				now = at(7, 0)
			}
			if req.DurationMinutes == 0 {
				req = brake
			}
			in := availInputs{actor: actor, dealer: dealer, req: req, loc: loc, weekly: map[int16]domain.WeekdayHours{}}
			rng, err := newAvailRange(in, data)
			if err != nil {
				t.Fatal(err)
			}
			avail, err := dayAvailability(in, rng, day, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range avail.Slots {
				if !s.Start.Equal(tc.slotAt) {
					continue
				}
				got := want{available: s.Available, reason: s.Reason, freeBays: s.FreeBays, techs: s.FreeTechnicians, countsUnreported: tc.want.countsUnreported}
				if got != tc.want {
					t.Fatalf("slot %s = %+v, want %+v", tc.slotAt.Format("15:04"), got, tc.want)
				}
				return
			}
			t.Fatalf("no slot at %s", tc.slotAt.Format("15:04"))
		})
	}
}
