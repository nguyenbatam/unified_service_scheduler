package integration_test

import (
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// TestEngineRegretWide is the regret measurement of TestAllocationRegretNoWorseThanFirstFit and
// TestAllocationRegretWithWaiters over many more seeds, for ranking experiments: opt-in
// (ENGINE_REGRET_SEEDS=400), it only logs the totals and checks the day stays valid. The pinned
// tests keep 24 seeds so CI stays fast; docs/system-design.md quotes both.
func TestEngineRegretWide(t *testing.T) {
	seeds, _ := strconv.Atoi(os.Getenv("ENGINE_REGRET_SEEDS"))
	if seeds <= 0 {
		t.Skip("set ENGINE_REGRET_SEEDS (e.g. 400) to run the wide regret measurement")
	}
	for _, waiters := range []bool{false, true} {
		t.Run(strconv.FormatBool(waiters), func(t *testing.T) {
			t.Parallel()
			engine, baseline := runRegretSeeds(t, waiters, seeds)
			t.Logf("WIDE waiters=%v over %d streams: engine %d labour minutes, %d refused, %d regrets; first fit %d, %d refused, %d regrets",
				waiters, seeds, engine.minutes, engine.refused, engine.regret, baseline.minutes, baseline.refused, baseline.regret)
		})
	}
}

type wideScore struct {
	regretScore
	refused int
}

// runRegretSeeds is runRegret over seeds streams, also counting refusals.
func runRegretSeeds(t *testing.T, waiters bool, seeds int) (engine, baseline wideScore) {
	t.Helper()
	pool, _, svc, ctx := openTestDB(t)
	const perStream = 14
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
			j.bay, j.tech = bayByName(appt.ServiceBayName), techIndex[appt.TechnicianID]
			engAccepted = append(engAccepted, j)
		}
		var e, b regretScore
		shop.judge(engAccepted, engRefused, &e)
		shop.judge(simAccepted, simRefused, &b)
		engine.minutes += e.minutes
		engine.regret += e.regret
		engine.refused += len(engRefused)
		baseline.minutes += b.minutes
		baseline.regret += b.regret
		baseline.refused += len(simRefused)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	return engine, baseline
}
