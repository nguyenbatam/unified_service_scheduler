package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The fill, truth, and bench phases run on dealerships of their own, provisioned through the admin
// API (POST /admin/dealerships, …/bays, /admin/technicians) on first use and reused afterwards
// (found by name): a mix of bay types and of technicians with overlapping skills and levels, so
// allocation choices matter, and nothing the demo data or other phases book can disturb them.

type bayDef struct {
	name    string
	bayType domain.BayType
}

type techDef struct {
	name   string
	level  int16
	skills []domain.TechnicianSkill
}

type workshopDef struct {
	name                 string
	open, close          string
	lunchStart, lunchEnd string
	bays                 []bayDef
	techs                []techDef
	// shifts gives some technicians (by name) a weekly pattern; the others work the opening hours.
	shifts map[string][]domain.TechnicianShift
	// absences block a technician (by name) on every phase day, at the same local times
	// (ensureDayRoster), so duty is exercised by dated absences as well as by the pattern.
	absences []absenceDef
}

// absenceDef is a dated absence repeated on each phase day: technician name, local from–to.
type absenceDef struct {
	tech     string
	from, to string // HH:MM, local
}

// fillWorkshop is the shop the truth, fill, and customers phases book: 2 express, 4 general, 1
// diagnostic bay, and six technicians whose skills overlap at different levels (a lubrication-only
// junior up to a master who can do everything). Two of them have a shift pattern and one is away
// for two hours on every phase day, so the oracle's duty rule is exercised non-trivially: Brakes L2
// works 08:00–16:00 every day with a break at 10:00–10:30 (a brake job starting at 09:00 or ending
// after 16:00 cannot use him), General L3 works weekdays only, 09:00–18:00 with a break at
// 15:00–15:30 (weekend phase days run without him), and Lube L2 is on training 13:00–15:00.
var fillWorkshop = workshopDef{
	name: "Stress Workshop", open: "08:00", close: "18:00", lunchStart: "12:00", lunchEnd: "13:00",
	bays: []bayDef{
		{"Express 1", domain.BayExpress}, {"Express 2", domain.BayExpress},
		{"General 1", domain.BayGeneral}, {"General 2", domain.BayGeneral},
		{"General 3", domain.BayGeneral}, {"General 4", domain.BayGeneral},
		{"Diagnostic 1", domain.BayDiagnostic},
	},
	techs: []techDef{
		{"Lube L1", 1, []domain.TechnicianSkill{domain.SkillLubrication}},
		{"Lube L2", 2, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillGeneral}},
		{"Brakes L2", 2, []domain.TechnicianSkill{domain.SkillBrakes}},
		{"Brakes L3", 3, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral}},
		{"General L3", 3, []domain.TechnicianSkill{domain.SkillGeneral}},
		{"Master L4", 4, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral}},
	},
	shifts: map[string][]domain.TechnicianShift{
		"Brakes L2":  weeklyShift([]int16{1, 2, 3, 4, 5, 6, 7}, "08:00", "16:00", "10:00", "10:30"),
		"General L3": weeklyShift([]int16{1, 2, 3, 4, 5}, "09:00", "18:00", "15:00", "15:30"),
	},
	absences: []absenceDef{{tech: "Lube L2", from: "13:00", to: "15:00"}},
}

// riverWorkshop is the shop the river hammer books: a dedicated dealership without a staging cap
// (enforced at every run, so the exact capacity check can never silently degrade to upper bounds)
// and without lunch, with four express bays and four technicians who do oil changes at different
// levels (reassign has somewhere to go).
var riverWorkshop = workshopDef{
	name: "Stress River", open: "08:00", close: "18:00",
	bays: []bayDef{
		{"Lane 1", domain.BayExpress}, {"Lane 2", domain.BayExpress}, {"Lane 3", domain.BayExpress}, {"Lane 4", domain.BayExpress},
		{"General 1", domain.BayGeneral},
	},
	techs: []techDef{
		{"River Lube A", 1, []domain.TechnicianSkill{domain.SkillLubrication}},
		{"River Lube B", 1, []domain.TechnicianSkill{domain.SkillLubrication}},
		{"River Lube C", 2, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillGeneral}},
		{"River Master", 3, []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral}},
	},
}

// weeklyShift is the same shift on each of weekdays.
func weeklyShift(weekdays []int16, start, end, breakStart, breakEnd string) []domain.TechnicianShift {
	out := make([]domain.TechnicianShift, len(weekdays))
	for i, wd := range weekdays {
		out[i] = domain.TechnicianShift{Weekday: wd, StartTime: start, EndTime: end, BreakStart: breakStart, BreakEnd: breakEnd}
	}
	return out
}

// workshopExpressMax is the stress shops' express-lane limit: long enough that the waiters' general
// jobs of the fill day (BRAKE 90 min, OIL+BRAKE 120 min) may take the quick lane when no general
// bay is free, so the soft upgrade — and the rule that it is only a fallback — is exercised.
const workshopExpressMax = 120

// dayFleet is how many cars the truth and fill days draw from (truth's probes and bookings take a
// fresh car each).
const dayFleet = 800

// benchBays is the size of the bench shop: that many express bays and lubrication technicians, so
// an oil change start holds benchBays bookings and uncontended writes never run out of room.
const benchBays = 20

// benchWorkshop is the shop the bench books: identical express bays and lubrication technicians,
// open 08:00–18:00 without lunch (20 starts a day, benchBays bookings each).
func benchWorkshop() workshopDef {
	w := workshopDef{name: "Stress Bench", open: "08:00", close: "18:00"}
	for i := range benchBays {
		w.bays = append(w.bays, bayDef{fmt.Sprintf("Lane %02d", i+1), domain.BayExpress})
		w.techs = append(w.techs, techDef{fmt.Sprintf("Lube %02d", i+1), 1, []domain.TechnicianSkill{domain.SkillLubrication}})
	}
	return w
}

// workshop is a provisioned shop: its dealership, bays, and technicians as the API reports them,
// and the definition it was provisioned from (its roster).
type workshop struct {
	dealer domain.Dealership
	loc    *time.Location
	bays   []domain.Bay
	techs  []domain.TechnicianAdmin
	def    workshopDef
}

// openMinutes is the bookable time of one day per resource (opening hours minus lunch).
func (w workshop) openMinutes() int {
	minutes := func(a, b string) int {
		ca, errA := parseClockMinutes(a)
		cb, errB := parseClockMinutes(b)
		if errA != nil || errB != nil {
			return 0
		}
		return int(cb - ca)
	}
	m := minutes(w.dealer.OpenTime, w.dealer.CloseTime)
	if w.dealer.LunchStart != "" {
		m -= minutes(w.dealer.LunchStart, w.dealer.LunchEnd)
	}
	return m
}

// ensureWorkshop finds def's dealership by name (creating it when missing) and adds whichever of
// its bays and technicians are missing, so repeated runs reuse one shop.
func ensureWorkshop(ctx context.Context, api *apiClient, def workshopDef) (workshop, error) {
	dealers, err := getAll[domain.Dealership](ctx, api, "/api/v1/dealerships", maxListLimit, map[string]string{"Cache-Control": "no-cache"})
	if err != nil {
		return workshop{}, err
	}
	w := workshop{def: def}
	if i := slices.IndexFunc(dealers, func(d domain.Dealership) bool { return d.Name == def.name }); i >= 0 {
		w.dealer = dealers[i]
	} else {
		body := map[string]any{
			"name": def.name, "timezone": "Europe/London", "openTime": def.open, "closeTime": def.close,
			"policy": map[string]any{"bookingHorizonDays": 365, "expressMaxMinutes": workshopExpressMax},
		}
		if def.lunchStart != "" {
			body["lunchStart"], body["lunchEnd"] = def.lunchStart, def.lunchEnd
		}
		if w.dealer, err = getJSON[domain.Dealership](ctx, api, "POST", "/api/v1/admin/dealerships", body); err != nil {
			return workshop{}, fmt.Errorf("create dealership %s: %w", def.name, err)
		}
	}
	// A shop made by an older run, or edited since: the express limit the fill day relies on, and
	// no staging cap (river's exact check and the oracle's day verdicts assume none).
	if w.dealer.Policy.ExpressMaxMinutes != workshopExpressMax || w.dealer.StagingCapacity != 0 {
		if w.dealer, err = getJSONWith[domain.Dealership](ctx, api, "PATCH", "/api/v1/admin/dealerships/"+w.dealer.ID.String(),
			map[string]any{"stagingCapacity": 0, "policy": map[string]any{"expressMaxMinutes": workshopExpressMax}},
			map[string]string{"If-Match": fmt.Sprintf("%q", fmt.Sprint(w.dealer.Version))}); err != nil {
			return workshop{}, fmt.Errorf("reset the express limit / staging cap of %s: %w", def.name, err)
		}
	}
	if w.dealer.StagingCapacity != 0 {
		return workshop{}, fmt.Errorf("%s still has a staging cap of %d after the reset", def.name, w.dealer.StagingCapacity)
	}
	if w.loc, err = time.LoadLocation(w.dealer.Timezone); err != nil {
		return workshop{}, err
	}
	board, err := getJSON[domain.Schedule](ctx, api, "GET", fmt.Sprintf("/api/v1/schedule?dealershipId=%s&date=%s", w.dealer.ID, w.dealer.Today), nil)
	if err != nil {
		return workshop{}, err
	}
	w.bays = board.Bays
	for _, b := range def.bays {
		if slices.ContainsFunc(w.bays, func(x domain.Bay) bool { return x.Name == b.name }) {
			continue
		}
		bay, err := getJSON[domain.Bay](ctx, api, "POST", "/api/v1/admin/dealerships/"+w.dealer.ID.String()+"/bays",
			map[string]any{"name": b.name, "bayType": b.bayType})
		if err != nil {
			return workshop{}, fmt.Errorf("create bay %s: %w", b.name, err)
		}
		w.bays = append(w.bays, bay)
	}
	if w.techs, err = getAll[domain.TechnicianAdmin](ctx, api, "/api/v1/admin/technicians?dealershipId="+w.dealer.ID.String(), maxListLimit, nil); err != nil {
		return workshop{}, err
	}
	for _, t := range def.techs {
		if slices.ContainsFunc(w.techs, func(x domain.TechnicianAdmin) bool { return x.Name == t.name }) {
			continue
		}
		tech, err := getJSON[domain.TechnicianAdmin](ctx, api, "POST", "/api/v1/admin/technicians", map[string]any{
			"dealershipId": w.dealer.ID, "name": t.name, "skillLevel": t.level, "skills": t.skills,
		})
		if err != nil {
			return workshop{}, fmt.Errorf("create technician %s: %w", t.name, err)
		}
		w.techs = append(w.techs, tech)
	}
	w.techs = slices.DeleteFunc(w.techs, func(t domain.TechnicianAdmin) bool { return !t.Active })
	return w, nil
}

// Stress vehicles belong to a customer of their own (not a demo customer), registered on first use.
const (
	stressOwnerEmail    = "stress-owner@scheduler.local"
	stressOwnerPassword = "stress-owner-pass-1"
)

// ensureStressOwner logs the stress owner in, registering the account first when it does not
// exist, and returns its customer id. onLost is the session's loss hook (nil: ignored).
func ensureStressOwner(ctx context.Context, base string, onLost func(error)) (uuid.UUID, error) {
	owner := newAPI(base)
	err := owner.login(ctx, stressOwnerEmail, stressOwnerPassword, onLost)
	if err != nil {
		resp, rerr := owner.do(ctx, http.MethodPost, "/api/v1/auth/register", map[string]string{
			"email": stressOwnerEmail, "password": stressOwnerPassword, "name": "Stress Owner",
		}, nil)
		if rerr != nil {
			return uuid.Nil, fmt.Errorf("register stress owner: %w", rerr)
		}
		if resp.code != http.StatusCreated && resp.code != http.StatusOK {
			return uuid.Nil, fmt.Errorf("register stress owner: %d %s (login: %w)", resp.code, truncate(string(resp.body), 160), err)
		}
		if err = owner.login(ctx, stressOwnerEmail, stressOwnerPassword, onLost); err != nil {
			return uuid.Nil, fmt.Errorf("stress owner login after register: %w", err)
		}
	}
	me, err := getJSON[domain.User](ctx, owner, "GET", "/api/v1/auth/me", nil)
	if err != nil {
		return uuid.Nil, err
	}
	if me.CustomerID == uuid.Nil {
		return uuid.Nil, errors.New("stress owner has no customer record")
	}
	return me.CustomerID, nil
}
