package integration_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Allocation quality as an outcome, against a baseline: the same random request streams go through
// the real engine and through a simulated "first free by name" allocator, and each is judged by an
// offline oracle. A refusal is regret when re-assigning the bays and technicians of everything the
// allocator had already accepted (same times) would have made room for it: capacity lost to a
// poor choice, not to demand. Both allocators play by the same rules (bayFits: the exact type, the
// express spill, a short waiter's soft upgrade); only their choices differ. Over the fixed seeds the
// engine must book clearly more labour minutes than first fit and have less regret (pinned floors
// below, calibrated on the deterministic streams with margin); both are logged, so the margin is
// visible. The engine's edge comes from best fit (bays and technicians) and from the one-move
// repair (a refused request moves one confirmed booking to another bay or technician).
//
// The streams are sequential (one booking at a time, each knowing nothing of the ones to come — the
// engine's real situation); shops are small (2–3 bays, 3 technicians with random skills and levels)
// so the oracle can search every assignment.

type regretJob struct {
	service      uuid.UUID
	bayType      domain.BayType
	skill        domain.TechnicianSkill
	level        int16
	start, end   int // minutes after opening
	bay, tech    int // assignment (index into the shop's bays / techs)
	labourMinute int
	waiter       bool
}

type regretShop struct {
	bays       []domain.BayType
	techs      []policyTech
	expressMax int // the dealership's expressMaxMinutes (waiter soft upgrade)
}

// regretWaiterExpressMax is the waiter variant's expressMaxMinutes: a 90-minute brake job may take
// the quick lane, so the soft upgrade really competes with oil changes for it.
const regretWaiterExpressMax = 90

// fits is booking.BayFits restated for the simulation: the exact type, the express spill into a
// general bay, or a short waiter's general job in the quick lane.
func (s regretShop) fits(bay int, j regretJob) bool {
	bt := s.bays[bay]
	return bt == j.bayType ||
		(j.bayType == domain.BayExpress && bt == domain.BayGeneral) ||
		(j.waiter && j.bayType == domain.BayGeneral && j.labourMinute <= s.expressMax && bt == domain.BayExpress)
}

var regretServices = []struct {
	id      uuid.UUID
	minutes int
	bay     domain.BayType
	skill   domain.TechnicianSkill
	level   int16
}{
	{seed.ServiceOil, 30, domain.BayExpress, domain.SkillLubrication, 1},
	{seed.ServiceBrake, 90, domain.BayGeneral, domain.SkillBrakes, 2},
	{seed.ServiceMajor, 180, domain.BayGeneral, domain.SkillGeneral, 3},
}

func randomRegretShop(r *rand.Rand) regretShop {
	shop := regretShop{bays: []domain.BayType{domain.BayExpress, domain.BayGeneral}, expressMax: 60}
	if r.IntN(2) == 0 {
		shop.bays = append(shop.bays, domain.BayGeneral)
	}
	all := []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral}
	for i := range 3 {
		var skills []domain.TechnicianSkill
		for _, sk := range all {
			if r.IntN(2) == 0 {
				skills = append(skills, sk)
			}
		}
		if len(skills) == 0 {
			skills = []domain.TechnicianSkill{all[r.IntN(len(all))]}
		}
		shop.techs = append(shop.techs, policyTech{name: fmt.Sprintf("Tech %d", i), level: []int16{1, 2, 3}[r.IntN(3)], skills: skills})
	}
	return shop
}

// randomRegretStream draws n requests; with waiters, each is a waiter with probability 1/2 (drawn
// after the job, so the jobs are the same streams as without).
func randomRegretStream(r *rand.Rand, n int, waiters bool) []regretJob {
	jobs := make([]regretJob, n)
	for i := range jobs {
		s := regretServices[r.IntN(len(regretServices))]
		start := 30 * r.IntN(14) // 08:00 … 14:30, so even a major ends by 17:30
		jobs[i] = regretJob{service: s.id, bayType: s.bay, skill: s.skill, level: s.level,
			start: start, end: start + s.minutes, bay: -1, tech: -1, labourMinute: s.minutes}
		if waiters {
			jobs[i].waiter = r.IntN(2) == 0
		}
	}
	return jobs
}

func (s regretShop) qualified(tech int, j regretJob) bool {
	t := s.techs[tech]
	return t.level >= j.level && slices.Contains(t.skills, j.skill)
}

func overlaps(a, b regretJob) bool { return a.start < b.end && b.start < a.end }

// firstFit is the baseline: the first free bay of the job's type (else the first free fallback bay)
// and the first free qualified technician, each in name order (the shop's order).
func (s regretShop) firstFit(booked []regretJob, j regretJob) (regretJob, bool) {
	free := func(isBusy func(regretJob) bool) bool {
		for _, b := range booked {
			if overlaps(b, j) && isBusy(b) {
				return false
			}
		}
		return true
	}
	j.bay, j.tech = -1, -1
	for pass := range 2 { // the job's own bay type first, then a fallback (spill or soft upgrade)
		for i, bt := range s.bays {
			if (bt == j.bayType) == (pass == 0) && s.fits(i, j) && free(func(b regretJob) bool { return b.bay == i }) {
				j.bay = i
				break
			}
		}
		if j.bay >= 0 {
			break
		}
	}
	for i := range s.techs {
		if s.qualified(i, j) && free(func(b regretJob) bool { return b.tech == i }) {
			j.tech = i
			break
		}
	}
	return j, j.bay >= 0 && j.tech >= 0
}

// feasible reports whether some assignment of bays and technicians fits every job (times fixed):
// a depth-first search with the jobs in start order, pruned on the first conflict.
func (s regretShop) feasible(jobs []regretJob) bool {
	jobs = slices.Clone(jobs)
	slices.SortFunc(jobs, func(a, b regretJob) int { return a.start - b.start })
	var place func(i int) bool
	place = func(i int) bool {
		if i == len(jobs) {
			return true
		}
		j := jobs[i]
		for bay := range s.bays {
			if !s.fits(bay, j) {
				continue
			}
			for tech := range s.techs {
				if !s.qualified(tech, j) {
					continue
				}
				clash := false
				for _, prev := range jobs[:i] {
					if overlaps(prev, j) && (prev.bay == bay || prev.tech == tech) {
						clash = true
						break
					}
				}
				if clash {
					continue
				}
				jobs[i].bay, jobs[i].tech = bay, tech
				if place(i + 1) {
					return true
				}
			}
		}
		jobs[i].bay, jobs[i].tech = -1, -1
		return false
	}
	return place(0)
}

type regretScore struct {
	minutes, regret int
}

func (s regretShop) judge(accepted []regretJob, refused []regretJob, score *regretScore) {
	for _, j := range accepted {
		score.minutes += j.labourMinute
	}
	for _, j := range refused {
		if s.feasible(append(slices.Clone(accepted), j)) {
			score.regret++
		}
	}
}

func TestAllocationRegretNoWorseThanFirstFit(t *testing.T) {
	t.Parallel()
	engine, baseline := runRegret(t, false)
	// Calibrated on the fixed seeds (engine 9600 min / 2 regrets vs first fit 9030 / 6 when this
	// floor was set, +6.3 %): the engine must keep ≥ 5 % more labour and strictly less regret.
	if engine.minutes*100 < baseline.minutes*105 || engine.regret >= baseline.regret {
		t.Fatalf("engine allocation not clearly better than first fit: %+v vs %+v", engine, baseline)
	}
}

// The same streams with half the requests waiters, at a dealership whose quick lane takes jobs up
// to 90 minutes: the soft upgrade (a waiter's brake job in the express bay, only as the fallback)
// and the express spill compete for both bays.
func TestAllocationRegretWithWaiters(t *testing.T) {
	t.Parallel()
	engine, baseline := runRegret(t, true)
	// Calibrated like the plain streams (engine 10710 / 1 vs first fit 10350 / 5, +3.5 %; without
	// the one-move repair the engine booked 10440 with 3 regrets): ≥ 2 % more, strictly less regret.
	if engine.minutes*100 < baseline.minutes*102 || engine.regret >= baseline.regret {
		t.Fatalf("engine allocation not clearly better than first fit with waiters: %+v vs %+v", engine, baseline)
	}
}

// runRegret plays the fixed seeds through the engine and through first fit and returns both
// totals (logged). With waiters, each shop's quick lane takes jobs up to regretWaiterExpressMax.
func runRegret(t *testing.T, waiters bool) (engine, baseline regretScore) {
	t.Helper()
	pool, _, svc, ctx := openTestDB(t)
	const seeds, perStream = 24, 14
	for seedN := range seeds {
		r := rand.New(rand.NewPCG(uint64(seedN), 0x5eed)) //nolint:gosec // reproducible request streams
		shop := randomRegretShop(r)
		stream := randomRegretStream(r, perStream, waiters)
		day := londonDay(t, 1+seedN%7)
		open := londonOn(t, day, 8, 0)

		dealer, techIDs := policyShop(ctx, t, svc, shop.bays, shop.techs)
		if waiters {
			shop.expressMax = regretWaiterExpressMax
			limit := int16(regretWaiterExpressMax)
			if _, err := svc.UpdateDealership(ctx, staffAdmin, dealer, store.DealershipPatch{ExpressMaxMinutes: &limit}); err != nil {
				t.Fatal(err)
			}
		}
		var simAccepted, simRefused []regretJob
		for _, j := range stream {
			if placed, ok := shop.firstFit(simAccepted, j); ok {
				simAccepted = append(simAccepted, placed)
			} else {
				simRefused = append(simRefused, j)
			}
		}

		techIndex := map[uuid.UUID]int{}
		for i, pt := range shop.techs {
			techIndex[techIDs[pt.name]] = i
		}
		bayIndex := map[uuid.UUID]int{}
		var engAccepted, engRefused []regretJob
		for _, j := range stream {
			appt, err := policyBook(ctx, t, svc, dealer, j.service, open.Add(time.Duration(j.start)*time.Minute), j.waiter)
			if err != nil {
				if de, ok := domain.AsError(err); !ok || de.Code != domain.CodeUnavailable {
					t.Fatalf("seed %d: book %+v: %v", seedN, j, err)
				}
				engRefused = append(engRefused, j)
				continue
			}
			if _, ok := bayIndex[appt.ServiceBayID]; !ok {
				bayIndex[appt.ServiceBayID] = bayByName(appt.ServiceBayName)
			}
			j.bay, j.tech = bayIndex[appt.ServiceBayID], techIndex[appt.TechnicianID]
			engAccepted = append(engAccepted, j)
		}
		var e, b regretScore
		shop.judge(engAccepted, engRefused, &e)
		shop.judge(simAccepted, simRefused, &b)
		if e.regret > 0 || b.regret > 0 {
			t.Logf("seed %d: engine %d min, regret %d; first fit %d min, regret %d", seedN, e.minutes, e.regret, b.minutes, b.regret)
		}
		engine.minutes += e.minutes
		engine.regret += e.regret
		baseline.minutes += b.minutes
		baseline.regret += b.regret
	}
	t.Logf("waiters=%v over %d streams of %d requests: engine booked %d labour minutes with %d regrets; first fit %d with %d",
		waiters, seeds, perStream, engine.minutes, engine.regret, baseline.minutes, baseline.regret)
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	return engine, baseline
}

// bayByName maps policyShop's "Bay A", "Bay B", … back to the shop index.
func bayByName(name string) int {
	return int(name[len(name)-1] - 'A')
}
