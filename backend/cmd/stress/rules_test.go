package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func svc(code string, minutes int, skill domain.TechnicianSkill, bay domain.BayType, level int16) domain.ServiceType {
	return domain.ServiceType{ID: uuid.New(), Code: code, DurationMinutes: minutes, RequiredSkill: skill, RequiredBayType: bay, MinSkillLevel: domain.SkillLevel(level)}
}

// The documented job-line rule, independent of booking.Combine.
func TestCombineLines(t *testing.T) {
	oil := svc("OIL", 30, domain.SkillLubrication, domain.BayExpress, 1)
	brake := svc("BRAKE", 90, domain.SkillBrakes, domain.BayGeneral, 2)
	major := svc("MAJOR", 180, domain.SkillGeneral, domain.BayGeneral, 3)
	diag := svc("DIAG", 60, domain.SkillGeneral, domain.BayDiagnostic, 2)
	align := svc("ALIGN", 60, domain.SkillGeneral, domain.BayAlignment, 1)
	zero := svc("ZERO", 30, domain.SkillLubrication, domain.BayExpress, 0)
	cases := []struct {
		name    string
		lines   []domain.ServiceType
		want    jobRequirement
		wantErr string
	}{
		{"one express line", []domain.ServiceType{oil}, jobRequirement{30, domain.BayExpress, []domain.TechnicianSkill{1}, 1}, ""},
		{"express + general takes general, sums time, highest level, sorted skills",
			[]domain.ServiceType{brake, oil}, jobRequirement{120, domain.BayGeneral, []domain.TechnicianSkill{1, 2}, 2}, ""},
		{"a specialty wins over general", []domain.ServiceType{major, diag, oil}, jobRequirement{270, domain.BayDiagnostic, []domain.TechnicianSkill{1, 3}, 3}, ""},
		{"a repeated skill counts once", []domain.ServiceType{oil, oil}, jobRequirement{60, domain.BayExpress, []domain.TechnicianSkill{1}, 1}, ""},
		{"level never below 1", []domain.ServiceType{zero}, jobRequirement{30, domain.BayExpress, []domain.TechnicianSkill{1}, 1}, ""},
		{"diagnostic and alignment cannot share", []domain.ServiceType{diag, align}, jobRequirement{}, "cannot share"},
		{"no lines", nil, jobRequirement{}, "1–5 lines"},
		{"six lines", []domain.ServiceType{oil, oil, oil, oil, oil, oil}, jobRequirement{}, "1–5 lines"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := combineLines(tc.lines)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.minutes != tc.want.minutes || got.bayType != tc.want.bayType || got.minLevel != tc.want.minLevel || !slices.Equal(got.skills, tc.want.skills) {
				t.Fatalf("combineLines = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSoftUpgradeAndBayFit(t *testing.T) {
	general90 := jobRequirement{minutes: 90, bayType: domain.BayGeneral}
	general180 := jobRequirement{minutes: 180, bayType: domain.BayGeneral}
	express := jobRequirement{minutes: 30, bayType: domain.BayExpress}
	cases := []struct {
		name       string
		r          jobRequirement
		waiter     bool
		expressMax int
		want       bool
	}{
		{"waiter general job within the limit", general90, true, 120, true},
		{"non-waiter never", general90, false, 120, false},
		{"longer than the limit", general180, true, 120, false},
		{"exactly the limit", general90, true, 90, true},
		{"limit 0 disables the quick lane", general90, true, 0, false},
		{"an express job is no upgrade", express, true, 120, false},
	}
	for _, tc := range cases {
		if got := softUpgrade(tc.r, tc.waiter, tc.expressMax); got != tc.want {
			t.Fatalf("%s: softUpgrade = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !bayFits(general90, true, domain.BayExpress) || bayFits(general90, false, domain.BayExpress) {
		t.Fatal("a general job fits an express bay only with the soft upgrade")
	}
	if !bayFits(express, false, domain.BayGeneral) || bayFits(express, false, domain.BayDiagnostic) {
		t.Fatal("an express job may spill into a general bay (any customer), never into a specialty bay")
	}
	if !qualified(jobRequirement{skills: []domain.TechnicianSkill{1, 2}, minLevel: 2}, 3, map[domain.TechnicianSkill]bool{1: true, 2: true, 3: true}) ||
		qualified(jobRequirement{skills: []domain.TechnicianSkill{1, 2}, minLevel: 2}, 3, map[domain.TechnicianSkill]bool{1: true}) ||
		qualified(jobRequirement{skills: []domain.TechnicianSkill{1}, minLevel: 2}, 1, map[domain.TechnicianSkill]bool{1: true}) {
		t.Fatal("qualified: every skill at the level")
	}
}

// The documented roster rule, independent of the server's SQL and Go twins.
func TestOnDuty(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Fatal(err)
	}
	monday := time.Date(2030, 1, 7, 0, 0, 0, 0, london)    // a Monday
	saturday := time.Date(2030, 1, 12, 0, 0, 0, 0, london) // a Saturday
	at := func(day time.Time, hh, mm int) time.Time {
		return day.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
	}
	win := func(day time.Time, hh, mm, minutes int) timeWindow {
		s := at(day, hh, mm)
		return timeWindow{s, s.Add(time.Duration(minutes) * time.Minute)}
	}
	sh, err := parseShift("08:00", "16:00", "10:00", "10:30")
	if err != nil {
		t.Fatal(err)
	}
	weekdays := techRoster{shifts: map[int]shiftDef{1: sh, 2: sh, 3: sh, 4: sh, 5: sh}}
	withAbsence := techRoster{shifts: weekdays.shifts, absences: []timeWindow{{at(monday, 13, 0), at(monday, 15, 0)}}}
	cases := []struct {
		name   string
		roster techRoster
		w      timeWindow
		want   bool
	}{
		{"no pattern: works whatever the hours allow", techRoster{}, win(saturday, 7, 0, 60), true},
		{"inside the shift before the break", weekdays, win(monday, 8, 0, 120), true},
		{"ends exactly when the break starts", weekdays, win(monday, 8, 30, 90), true},
		{"overlaps the break", weekdays, win(monday, 9, 0, 90), false},
		{"starts exactly when the break ends", weekdays, win(monday, 10, 30, 90), true},
		{"starts before the shift", weekdays, win(monday, 7, 30, 60), false},
		{"ends after the shift", weekdays, win(monday, 15, 0, 90), false},
		{"ends exactly at the shift's end", weekdays, win(monday, 14, 30, 90), true},
		{"a weekday without a shift", weekdays, win(saturday, 9, 0, 30), false},
		{"crosses midnight", techRoster{shifts: map[int]shiftDef{1: {start: 0, end: 24 * 60}, 2: {start: 0, end: 24 * 60}}}, win(monday, 23, 30, 60), false},
		{"an absence blocks an overlapping window", withAbsence, win(monday, 14, 30, 60), false},
		{"an absence ending at the start does not", withAbsence, win(monday, 15, 0, 60), true},
		{"an absence blocks a technician without a pattern", techRoster{absences: withAbsence.absences}, win(monday, 12, 30, 60), false},
	}
	for _, tc := range cases {
		if got := tc.roster.onDuty(tc.w, london); got != tc.want {
			t.Fatalf("%s: onDuty(%s–%s) = %v, want %v", tc.name, tc.w.start.Format("Mon 15:04"), tc.w.end.Format("15:04"), got, tc.want)
		}
	}
	// Wall clock, not instants: on the spring-forward Sunday a 09:00 local start is still 09:00.
	sunday := time.Date(2030, 3, 31, 0, 0, 0, 0, london)
	dst := techRoster{shifts: map[int]shiftDef{7: {start: 9 * 60, end: 10 * 60}}}
	if !dst.onDuty(timeWindow{time.Date(2030, 3, 31, 9, 0, 0, 0, london), time.Date(2030, 3, 31, 10, 0, 0, 0, london)}, london) {
		t.Fatalf("09:00–10:00 local on %s is inside a 09:00–10:00 shift", sunday.Format(time.DateOnly))
	}
}

func TestParseShiftAndClock(t *testing.T) {
	if _, err := parseClockMinutes("24:30"); err == nil {
		t.Fatal("24:30 is not a clock")
	}
	if c, err := parseClockMinutes("24:00"); err != nil || c != 24*60 {
		t.Fatalf("24:00 = %v, %v", c, err)
	}
	sh, err := parseShift("09:00", "17:00", "", "")
	if err != nil || sh.hasBreak || sh.start != 9*60 || sh.end != 17*60 {
		t.Fatalf("parseShift without a break = %+v, %v", sh, err)
	}
}

func TestOnDutySQLMentionsEveryRule(t *testing.T) {
	sql := onDutySQL("t", "d.timezone", "a.start_at", "a.end_at")
	for _, want := range []string{"technician_absences", "technician_shifts", "isodow", "break_start", "::date"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("onDutySQL lacks %q:\n%s", want, sql)
		}
	}
}

// A flagged in-progress overrun holds its bay and technician until it is completed, across days;
// anything else keeps its own window.
func TestOverrunHold(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	start := time.Date(2030, 3, 30, 15, 0, 0, 0, london) // the Saturday before the clocks go forward
	w := timeWindow{start, start.Add(time.Hour)}
	midnight := time.Date(2030, 3, 31, 0, 0, 0, 0, london)
	if got := overrunHold(w, int16(domain.StatusInProgress), true); !got.end.Equal(overrunOpenEnd) || !got.start.Equal(start) {
		t.Fatalf("flagged overrun holds %s–%s, want until completed", got.start, got.end)
	}
	if got := overrunHold(w, int16(domain.StatusInProgress), false); got != w {
		t.Fatalf("an unflagged job keeps its window, got %v", got)
	}
	if got := overrunHold(w, int16(domain.StatusConfirmed), true); got != w {
		t.Fatalf("only in-progress work overruns, got %v", got)
	}
	// The hold blocks a later start of the same day for the oracle.
	shop := &oracleShop{loc: london, bays: []oBay{{uuid.New(), domain.BayExpress}},
		techs: []oTech{{id: uuid.New(), level: 1, skills: map[domain.TechnicianSkill]bool{1: true}, roster: techRoster{shifts: map[int]shiftDef{}}}}}
	shop.holds = []oHold{{id: uuid.New(), bay: shop.bays[0].id, tech: shop.techs[0].id, status: 3,
		window: overrunHold(w, int16(domain.StatusInProgress), true)}}
	oil := jobRequirement{minutes: 30, bayType: domain.BayExpress, skills: []domain.TechnicianSkill{1}, minLevel: 1}
	later := start.Add(3 * time.Hour)
	if bays, techs := shop.counts(oil, false, timeWindow{later, later.Add(30 * time.Minute)}, uuid.Nil); bays != 0 || techs != 0 {
		t.Fatalf("18:00 next to a flagged overrun: %d bays, %d techs free, want none", bays, techs)
	}
	// A carry-over: the car is still on the lift the next morning.
	nextMorning := midnight.Add(9 * time.Hour)
	if bays, techs := shop.counts(oil, false, timeWindow{nextMorning, nextMorning.Add(30 * time.Minute)}, uuid.Nil); bays != 0 || techs != 0 {
		t.Fatalf("next morning after a flagged overrun: %d bays, %d techs free, want none", bays, techs)
	}
}
