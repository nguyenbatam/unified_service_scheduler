package booking

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// fitShop is a dealership open 08:00–18:00 with lunch 12:00–13:00, on a Monday, for the fit tests.
type fitShop struct {
	loc    *time.Location
	day    Date
	dealer domain.Dealership
}

func newFitShop(t *testing.T) fitShop {
	t.Helper()
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	return fitShop{loc: loc, day: Date{Year: 2026, Month: time.March, Day: 2}, dealer: domain.Dealership{
		ID: uuid.New(), Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "18:00",
		LunchStart: "12:00", LunchEnd: "13:00", StagingWindowMinutes: 60,
		Policy: domain.Policy{BookingHorizonDays: 90, ExpressMaxMinutes: 60},
	}}
}

func (f fitShop) at(h, m int) time.Time { return Clock{Hour: h, Minute: m}.On(f.day, f.loc) }

func (f fitShop) hold(bay, tech uuid.UUID, h, m, minutes int) store.Hold {
	start := f.at(h, m)
	return store.Hold{
		BayID: bay, TechnicianID: tech, Start: start, End: start.Add(time.Duration(minutes) * time.Minute),
		ID: uuid.New(), Status: domain.StatusConfirmed, RequiredBayType: domain.BayGeneral, DurationMinutes: minutes,
	}
}

func (f fitShop) inputs(req Requirement) availInputs {
	return availInputs{actor: domain.Actor{Role: domain.RoleDispatcher}, dealer: f.dealer, req: req, loc: f.loc, weekly: map[int16]domain.WeekdayHours{}}
}

// recommendedStarts evaluates the day for req over data (at 07:00, as staff) and lists the
// recommended starts as local clocks.
func (f fitShop) recommendedStarts(t *testing.T, req Requirement, data store.AvailabilityRange) []string {
	t.Helper()
	if data.Rosters.Shifts == nil {
		data.Rosters.Shifts = map[uuid.UUID][]domain.TechnicianShift{}
	}
	in := f.inputs(req)
	rng, err := newAvailRange(in, data)
	if err != nil {
		t.Fatal(err)
	}
	avail, err := dayAvailability(in, rng, f.day, f.at(7, 0))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range avail.Slots {
		if s.Recommended {
			out = append(out, s.Start.In(f.loc).Format("15:04"))
		}
	}
	return out
}

var (
	fitBrakes = []domain.TechnicianSkill{domain.SkillBrakes}
	fitLube   = []domain.TechnicianSkill{domain.SkillLubrication}
	brake60   = Requirement{DurationMinutes: 60, BayType: domain.BayGeneral, Skills: fitBrakes, MinSkillLevel: 2}
)

// The review's example: a 60-minute general job at 09:30 with lunch at 12:00, G1 holding 08:00–09:00
// and 11:00–12:00 (idle 30 + 30 around the job) and G2 holding 08:00–09:30 (0 + 90). The engine books
// G1 (the smallest total idle time) and leaves two 30-minute holes, so the start is not recommended
// although G2 would butt against its hold (the old hint said yes). At 11:00 the only free bay, G2,
// ends at lunch: recommended. The only brake technician works 09:30–12:00, so the technician side
// is flush at 09:30 and 11:00 and the bay pick decides.
func TestRecommendedFollowsTheEnginesBayPick(t *testing.T) {
	f := newFitShop(t)
	g1, g2, brakeTech, lube1, lube2 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	data := store.AvailabilityRange{
		Bays: []domain.Bay{{ID: g1, Name: "G1", BayType: domain.BayGeneral}, {ID: g2, Name: "G2", BayType: domain.BayGeneral}},
		Techs: []domain.Technician{
			{ID: brakeTech, Name: "Brakes", SkillLevel: 2, Skills: fitBrakes},
			{ID: lube1, Name: "Lube 1", SkillLevel: 1, Skills: fitLube}, {ID: lube2, Name: "Lube 2", SkillLevel: 1, Skills: fitLube},
		},
		Busy: []store.Hold{f.hold(g1, lube1, 8, 0, 60), f.hold(g2, lube2, 8, 0, 90), f.hold(g1, lube1, 11, 0, 60)},
		Rosters: store.Rosters{Shifts: map[uuid.UUID][]domain.TechnicianShift{
			brakeTech: {{Weekday: 1, StartTime: "09:30", EndTime: "12:00"}},
		}},
	}
	if got, want := f.recommendedStarts(t, brake60, data), []string{"11:00"}; !slices.Equal(got, want) {
		t.Fatalf("recommended %v, want %v", got, want)
	}
	in := f.inputs(brake60)
	rng, err := newAvailRange(in, data)
	if err != nil {
		t.Fatal(err)
	}
	slot := Interval{Start: f.at(9, 30), End: f.at(10, 30)}
	g, ok := rng.pickBay(in, slot, f.at(8, 0), f.at(12, 0))
	if !ok || g != (gaps{before: 30 * time.Minute, after: 30 * time.Minute}) {
		t.Fatalf("pickBay at 09:30 = %+v, %v; want G1's 30 + 30", g, ok)
	}
}

// pickTech restates freeTechniciansSQL's order: level, spare skills, fit, load, name, id.
func TestPickTechOrder(t *testing.T) {
	f := newFitShop(t)
	bay := uuid.New()
	slot := Interval{Start: f.at(10, 0), End: f.at(11, 0)}
	lo, hi := f.at(8, 0), f.at(12, 0)
	both := []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes}
	lowID, highID := uuid.MustParse("00000000-0000-4000-8000-000000000001"), uuid.MustParse("ff000000-0000-4000-8000-000000000001")
	type tech struct {
		tech  domain.Technician
		holds [][2]int // start hour, minutes
	}
	cases := []struct {
		name  string
		techs []tech
		want  string
	}{
		{"lowest level", []tech{{tech: domain.Technician{Name: "A", SkillLevel: 3, Skills: fitBrakes}}, {tech: domain.Technician{Name: "B", SkillLevel: 2, Skills: fitBrakes}}}, "B"},
		{"fewest spare skills", []tech{{tech: domain.Technician{Name: "A", SkillLevel: 2, Skills: both}}, {tech: domain.Technician{Name: "B", SkillLevel: 2, Skills: fitBrakes}}}, "B"},
		{"best fit", []tech{
			{tech: domain.Technician{Name: "A", SkillLevel: 2, Skills: fitBrakes}},
			{tech: domain.Technician{Name: "B", SkillLevel: 2, Skills: fitBrakes}, holds: [][2]int{{9, 60}}},
		}, "B"},
		{"least loaded", []tech{
			{tech: domain.Technician{Name: "A", SkillLevel: 2, Skills: fitBrakes}, holds: [][2]int{{14, 120}}},
			{tech: domain.Technician{Name: "B", SkillLevel: 2, Skills: fitBrakes}, holds: [][2]int{{15, 60}}},
		}, "B"},
		{"name in byte order", []tech{{tech: domain.Technician{Name: "b", SkillLevel: 2, Skills: fitBrakes}}, {tech: domain.Technician{Name: "B", SkillLevel: 2, Skills: fitBrakes}}}, "B"},
		{"id", []tech{
			{tech: domain.Technician{ID: highID, Name: "Same", SkillLevel: 2, Skills: fitBrakes}},
			{tech: domain.Technician{ID: lowID, Name: "Same", SkillLevel: 2, Skills: fitBrakes}},
		}, lowID.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := store.AvailabilityRange{Bays: []domain.Bay{{ID: bay, Name: "Bay", BayType: domain.BayGeneral}}}
			for _, tt := range tc.techs {
				if tt.tech.ID == uuid.Nil {
					tt.tech.ID = uuid.New()
				}
				data.Techs = append(data.Techs, tt.tech)
				for _, h := range tt.holds {
					data.Busy = append(data.Busy, f.hold(uuid.New(), tt.tech.ID, h[0], 0, h[1]))
				}
			}
			slices.SortFunc(data.Busy, func(a, b store.Hold) int { return a.Start.Compare(b.Start) })
			data.Rosters.Shifts = map[uuid.UUID][]domain.TechnicianShift{}
			in := f.inputs(brake60)
			rng, err := newAvailRange(in, data)
			if err != nil {
				t.Fatal(err)
			}
			loads := rng.techLoads(f.day.Start(f.loc), f.day.Next(f.loc))
			best, bestGaps := -1, gaps{}
			for i := range rng.techs {
				g := sideGaps(rng.techHolds[i], slot, lo, hi)
				if best < 0 || rng.compareTechs(in, loads, i, g, best, bestGaps) < 0 {
					best, bestGaps = i, g
				}
			}
			got := rng.techs[best].Name
			if tc.name == "id" {
				got = rng.techs[best].ID.String()
			}
			if got != tc.want {
				t.Fatalf("picked %s, want %s", got, tc.want)
			}
			if _, ok := rng.pickTech(in, loads, slot, localOf(slot, f.loc), lo, hi); !ok {
				t.Fatal("pickTech found no technician")
			}
		})
	}
}

// sideGaps is store.sideGapsSQL: bounded by lo and hi, holds before and after, an empty side at an
// edge, holds outside the stretch ignored.
func TestSideGaps(t *testing.T) {
	f := newFitShop(t)
	w := Interval{Start: f.at(10, 0), End: f.at(11, 0)}
	lo, hi := f.at(8, 0), f.at(12, 0)
	iv := func(h1, m1, h2, m2 int) Interval { return Interval{Start: f.at(h1, m1), End: f.at(h2, m2)} }
	for _, tc := range []struct {
		name          string
		holds         []Interval
		w             Interval
		before, after time.Duration
	}{
		{"empty stretch", nil, w, 2 * time.Hour, time.Hour},
		{"holds both sides", []Interval{iv(8, 0, 9, 0), iv(9, 0, 9, 30), iv(11, 30, 12, 0)}, w, 30 * time.Minute, 30 * time.Minute},
		{"butting", []Interval{iv(9, 0, 10, 0), iv(11, 0, 11, 30)}, w, 0, 0},
		{"outside the stretch", []Interval{iv(7, 0, 7, 30), iv(13, 0, 14, 0)}, w, 2 * time.Hour, time.Hour},
		{"window at the edges", []Interval{iv(9, 0, 9, 30)}, iv(8, 0, 12, 0), 0, 0},
		{"hold straddling lo", []Interval{iv(7, 30, 8, 30)}, w, 90 * time.Minute, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := sideGaps(newHoldList(tc.holds, nil), tc.w, lo, hi)
			if g.before != tc.before || g.after != tc.after {
				t.Fatalf("sideGaps = %v / %v, want %v / %v", g.before, g.after, tc.before, tc.after)
			}
		})
	}
}

// roster.stretch is store.techStretchSQL: the stretch narrowed to the shift and to its break on the
// slot's side, widened back to the slot; no pattern (or no shift that weekday) leaves it as it is.
func TestRosterStretch(t *testing.T) {
	f := newFitShop(t)
	r, err := newRoster([]domain.TechnicianShift{{Weekday: 1, StartTime: "09:00", EndTime: "17:00", BreakStart: "14:00", BreakEnd: "14:30"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lo, hi := f.at(8, 0), f.at(12, 0)
	afterLunchLo, afterLunchHi := f.at(13, 0), f.at(18, 0)
	for _, tc := range []struct {
		name           string
		r              roster
		slot           Interval
		lo, hi         time.Time
		wantLo, wantHi time.Time
	}{
		{"shift start", r, Interval{Start: f.at(10, 0), End: f.at(11, 0)}, lo, hi, f.at(9, 0), f.at(12, 0)},
		{"break ends the stretch", r, Interval{Start: f.at(13, 0), End: f.at(13, 30)}, afterLunchLo, afterLunchHi, f.at(13, 0), f.at(14, 0)},
		{"break starts it", r, Interval{Start: f.at(15, 0), End: f.at(16, 0)}, afterLunchLo, afterLunchHi, f.at(14, 30), f.at(17, 0)},
		{"no pattern", roster{}, Interval{Start: f.at(10, 0), End: f.at(11, 0)}, lo, hi, lo, hi},
		{"widened back to the slot", r, Interval{Start: f.at(8, 30), End: f.at(9, 30)}, lo, hi, f.at(8, 30), f.at(12, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotLo, gotHi := tc.r.stretch(tc.slot, localOf(tc.slot, f.loc), f.loc, tc.lo, tc.hi)
			if !gotLo.Equal(tc.wantLo) || !gotHi.Equal(tc.wantHi) {
				t.Fatalf("stretch = %s–%s, want %s–%s", gotLo.In(f.loc).Format("15:04"), gotHi.In(f.loc).Format("15:04"),
					tc.wantLo.In(f.loc).Format("15:04"), tc.wantHi.In(f.loc).Format("15:04"))
			}
		})
	}
	tuesday := Interval{Start: f.at(10, 0).AddDate(0, 0, 1), End: f.at(11, 0).AddDate(0, 0, 1)}
	tLo, tHi := lo.AddDate(0, 0, 1), hi.AddDate(0, 0, 1)
	if gotLo, gotHi := r.stretch(tuesday, localOf(tuesday, f.loc), f.loc, tLo, tHi); !gotLo.Equal(tLo) || !gotHi.Equal(tHi) {
		t.Fatal("no shift that weekday must leave the stretch as it is")
	}
}

// appendNextSlots takes the earliest starts, or with preferRecommended the recommended ones first,
// and lists them in time order; unavailable slots never appear, and take holds.
func TestAppendNextSlots(t *testing.T) {
	f := newFitShop(t)
	slot := func(h int, available, recommended bool) domain.Slot {
		return domain.Slot{Start: f.at(h, 0), End: f.at(h, 30), Available: available, Recommended: recommended}
	}
	slots := []domain.Slot{slot(8, true, false), slot(9, false, false), slot(10, true, true), slot(11, true, false), slot(12, true, true)}
	hours := func(out []domain.NextSlot) []int {
		var hs []int
		for _, s := range out {
			hs = append(hs, s.Start.In(f.loc).Hour())
		}
		return hs
	}
	for _, tc := range []struct {
		prefer bool
		take   int
		want   []int
	}{
		{false, 10, []int{8, 10, 11, 12}},
		{true, 10, []int{8, 10, 11, 12}},
		{true, 3, []int{8, 10, 12}},
		{true, 2, []int{10, 12}},
		{true, 1, []int{10}},
		{false, 2, []int{8, 10}},
		{false, 0, nil},
	} {
		out := appendNextSlots(nil, f.day, slots, tc.take, tc.prefer)
		if got := hours(out); !slices.Equal(got, tc.want) {
			t.Errorf("prefer=%v take=%d: %v, want %v", tc.prefer, tc.take, got, tc.want)
		}
	}
	// A later day's starts never come before an earlier day's.
	out := appendNextSlots(appendNextSlots(nil, f.day, slots[:1], 10, true), f.day.AddDays(1), slots[2:3], 10, true)
	if len(out) != 2 || out[0].Date != f.day.String() {
		t.Fatalf("days out of order: %+v", out)
	}
}

// A shift edge in the hour a clock change skips or repeats lands where Postgres puts it
// (techStretchSQL's (date + clock) AT TIME ZONE tz): a skipped 02:30 is 07:30Z (03:30 EDT), a
// repeated 01:30 its later occurrence, 06:30Z (01:30 EST); time.Date gave an hour earlier.
func TestRosterStretchOnClockChangeDays(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	spring, fall := Date{Year: 2026, Month: time.March, Day: 8}, Date{Year: 2026, Month: time.November, Day: 1}
	utc := func(d Date, h, m int) time.Time { return time.Date(d.Year, d.Month, d.Day, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		name         string
		day          Date
		shiftStart   string
		slot         Interval
		wantLo       time.Time
		wantEdgeAt   time.Time
		edgeMinuteOf time.Duration
	}{
		{name: "skipped hour", day: spring, shiftStart: "02:30", slot: Interval{Start: utc(spring, 7, 0), End: utc(spring, 7, 30)},
			wantLo: utc(spring, 7, 0), wantEdgeAt: utc(spring, 7, 30), edgeMinuteOf: 2*time.Hour + 30*time.Minute},
		{name: "repeated hour", day: fall, shiftStart: "01:30", slot: Interval{Start: utc(fall, 7, 0), End: utc(fall, 7, 30)},
			wantLo: utc(fall, 6, 30), wantEdgeAt: utc(fall, 6, 30), edgeMinuteOf: time.Hour + 30*time.Minute},
		{name: "ordinary clock", day: fall, shiftStart: "05:00", slot: Interval{Start: utc(fall, 11, 0), End: utc(fall, 11, 30)},
			wantLo: utc(fall, 10, 0), wantEdgeAt: utc(fall, 10, 0), edgeMinuteOf: 5 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pgLocalInstant(tc.day.Year, tc.day.Month, tc.day.Day, tc.edgeMinuteOf, loc); !got.Equal(tc.wantEdgeAt) {
				t.Fatalf("edge %s = %s, want %s", tc.shiftStart, got.UTC(), tc.wantEdgeAt)
			}
			r, err := newRoster([]domain.TechnicianShift{{Weekday: tc.day.ISOWeekday(), StartTime: tc.shiftStart, EndTime: "20:00"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			lo := tc.day.Start(loc)
			gotLo, _ := r.stretch(tc.slot, localOf(tc.slot, loc), loc, lo, tc.slot.End.Add(time.Hour))
			if !gotLo.Equal(tc.wantLo) {
				t.Fatalf("stretch lo = %s, want %s", gotLo.UTC(), tc.wantLo)
			}
		})
	}
}
