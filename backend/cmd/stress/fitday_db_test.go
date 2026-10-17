package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// fitHarness replays demands through the engine (booking.Service, the system under test) one
// customer at a time, exactly as the stress replay does through the API, on fresh days of one
// workshop, and replays them offline with naive first fit.
type fitHarness struct {
	t      *testing.T
	pool   *pgxpool.Pool
	svc    *booking.Service
	w      workshop
	mixes  []jobMix
	weight []int
	cars   []uuid.UUID
	names  map[uuid.UUID]string // bay and technician names (first fit's listed order)
}

func newFitHarness(t *testing.T, days []booking.Date) *fitHarness {
	t.Helper()
	_, pool := integrityDB(t)
	st := store.New(pool)
	if err := seed.Run(t.Context(), st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w, ids := insertTestWorkshop(t, pool, fillWorkshop, "Fit Test", 0, 60, days)
	names := map[uuid.UUID]string{}
	for name, id := range ids {
		names[id] = name
	}
	byName := map[string]jobMix{}
	for _, mx := range oracleTestMixes(t, pool) {
		byName[mx.name] = mx
	}
	// The fill day's mixes and weights (session.dayMixes).
	h := &fitHarness{t: t, pool: pool, svc: booking.New(st, nil), w: w, cars: oracleTestVehicles(t, pool, maxFillRequestsTest), names: names}
	for _, sp := range []struct {
		name   string
		weight int
	}{{"[MAJOR]", 15}, {"[OIL BRAKE]", 20}, {"[BRAKE]", 30}, {"[OIL]", 35}} {
		mx, ok := byName[sp.name]
		if !ok {
			t.Fatalf("no job mix %s", sp.name)
		}
		h.mixes, h.weight = append(h.mixes, mx), append(h.weight, sp.weight)
	}
	return h
}

// maxFillRequestsTest is the most customers one replayed day has (the fill day's 150).
const maxFillRequestsTest = 150

// shop is the oracle's model of day (bays, technicians, rosters).
func (h *fitHarness) shop(day booking.Date) *oracleShop {
	from, to := dayBounds(day, h.w.loc)
	s, err := loadShop(h.t.Context(), h.pool, h.w.dealer.ID, from, to)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// capBound is the day's capacity bound for every job mix offered.
func (h *fitHarness) capBound(day booking.Date) int {
	b, _, _, err := capacityBound(h.shop(day), h.w, day, fullDayClasses(h.mixes))
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

// fitResult is one replayed demand: labour minutes and customers booked by the engine and by
// naive first fit.
type fitResult struct {
	asked, customers         int
	server, serverBooked     int
	firstFit, firstFitBooked int // first fit in the listed order
	spread                   orderSpread
	bound                    int
}

func (r fitResult) gain() float64 { return 100 * float64(r.server-r.firstFit) / float64(r.firstFit) }

// replay books demand on day through the engine (each customer: the offered start nearest its
// wish, up to fillTries reads) and offline with first fit.
func (h *fitHarness) replay(day booking.Date, demand []fillRequest, key string) fitResult {
	t := h.t
	t.Helper()
	res := fitResult{customers: len(demand), bound: h.capBound(day)}
	for i, r := range demand {
		res.asked += r.mix.req.minutes
		for try := 1; try <= fillTries; try++ {
			avail, err := h.svc.Availability(t.Context(), oracleTestAdmin, booking.AvailabilityQuery{
				DealershipID: h.w.dealer.ID, VehicleID: r.vehicle, ServiceTypeIDs: r.mix.ids, Day: day, IsWaiter: r.waiter, WaiterGiven: true,
			})
			if err != nil {
				t.Fatalf("availability: %v", err)
			}
			slot, ok := nearestOffered(avail.Slots, r.desired)
			if !ok {
				break
			}
			_, _, err = h.svc.Book(t.Context(), oracleTestAdmin, booking.BookRequest{
				DealershipID: h.w.dealer.ID, VehicleID: r.vehicle, ServiceTypeIDs: r.mix.ids, StartAt: slot.Start,
				IsWaiter: r.waiter, IdempotencyKey: fmt.Sprintf("%s:%d:%d", key, i, try),
			})
			if err == nil {
				res.server += r.mix.req.minutes
				res.serverBooked++
				break
			}
			if _, refused := errors.AsType[*domain.Error](err); !refused {
				t.Fatalf("book: %v", err)
			}
		}
	}
	var err error
	if res.spread, err = firstFitAcross(h.shop(day), h.w, day, demand, h.names); err != nil {
		t.Fatal(err)
	}
	res.firstFit, res.firstFitBooked = res.spread.listed, res.spread.listedBooked
	return res
}

// TestFitDayCalibration prints server vs first fit over seeds and loads on a weekday and a Saturday
// (FIT_CALIBRATE=1; the weekdays Monday–Friday share one roster, Saturday and Sunday another).
func TestFitDayCalibration(t *testing.T) {
	if os.Getenv("FIT_CALIBRATE") != "1" {
		t.Skip("calibration only (FIT_CALIBRATE=1)")
	}
	const seeds = 20
	monday := booking.Date{Year: 2030, Month: time.January, Day: 7}
	var days []booking.Date
	for i := range seeds * (len(fitLoads) + 1) {
		days = append(days, monday.AddDays(7*i), monday.AddDays(7*i+5))
	}
	h := newFitHarness(t, days)
	n := 0
	sum := map[string]float64{}
	worst := map[string]float64{}
	for s := 1; s <= seeds; s++ {
		for li := -1; li < len(fitLoads); li++ {
			for range 2 {
				day := days[n]
				n++
				var demand []fillRequest
				label := "fill"
				if li < 0 {
					demand = fillDemand(uint64(s), 150, h.w, day, h.mixes, h.weight, h.cars)
				} else {
					label = fmt.Sprintf("x%.2f", fitLoads[li])
					demand = fitDemand(uint64(s), fitLoads[li], h.capBound(day), h.w, day, h.mixes, h.weight, h.cars)
				}
				r := h.replay(day, demand, fmt.Sprintf("cal:%d:%s:%s", s, label, day))
				wd := time.Date(day.Year, day.Month, day.Day, 12, 0, 0, 0, time.UTC).Weekday().String()[:3]
				k := label + " " + wd
				sum[k] += r.gain()
				if s == 1 || r.gain() < worst[k] {
					worst[k] = r.gain()
				}
				t.Logf("seed %2d %-5s %s %s: asked %5d bound %4d | server %4d (%3d) first fit %4d (%3d) gain %+5.1f%% | random orders %d…%d…%d",
					s, label, day, wd, r.asked, r.bound, r.server, r.serverBooked, r.firstFit, r.firstFitBooked, r.gain(), r.spread.min, r.spread.median, r.spread.max)
			}
		}
	}
	for k, v := range sum {
		t.Logf("%-10s mean gain %+5.1f%%, worst %+5.1f%%", k, v/seeds, worst[k])
	}
}

// The fill day's FIRSTFIT_MARGIN_PCT 0 ("never fewer than first fit") is not a matter of the run
// date: the stress fill day is today+3, so any weekday, and the shop's roster differs between
// weekdays (General L3 works Monday–Friday). The default demand (seed 1) on every weekday, and
// seeds 2 and 3 on a weekday and a Saturday, replayed through the engine one customer at a time,
// book no fewer labour minutes than first fit in the listed resource order.
func TestFillDayNoWorseThanFirstFitEveryWeekday(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	monday := booking.Date{Year: 2030, Month: time.January, Day: 7}
	type fillCase struct {
		seed uint64
		day  booking.Date
	}
	var cases []fillCase
	for d := range 7 {
		cases = append(cases, fillCase{1, monday.AddDays(d)})
	}
	for i, s := range []uint64{2, 3} {
		cases = append(cases, fillCase{s, monday.AddDays(7 * (i + 1))}, fillCase{s, monday.AddDays(7*(i+1) + 5)})
	}
	days := make([]booking.Date, len(cases))
	for i, c := range cases {
		days[i] = c.day
	}
	h := newFitHarness(t, days)
	for _, c := range cases {
		r := h.replay(c.day, fillDemand(c.seed, 150, h.w, c.day, h.mixes, h.weight, h.cars), fmt.Sprintf("fillwd:%d:%s", c.seed, c.day))
		if err := firstFitShortfall(r.server, r.firstFit, 0); err != nil {
			t.Errorf("seed %d on %s: %v", c.seed, c.day, err)
		}
		t.Logf("seed %d %s: server %d, first fit %d (listed order), random orders %d … %d … %d", c.seed, c.day, r.server, r.firstFit, r.spread.min, r.spread.median, r.spread.max)
	}
}

// The fit days against the engine: demand 1.0 / 1.15 / 1.3 × the capacity bound (seeds 1–6, on a
// weekday and on a Saturday, the two rosters of the shop), booked one customer at a time. Pinned:
// every day books no fewer labour minutes than first fit in the listed order (gain ≥ 0, the stress
// gate); every seed's three days together book no fewer than first fit in the median random order;
// and pooled, the engine books at least fitPinnedGainPct more than that median (seeds 1–6: +4.3 %;
// TestFitDayCalibration over 20 seeds: +5.6 % on weekdays, +3.6 % on Saturdays, worst seed +1.6 % /
// +0.0 %, worst single day −5.6 %; vs the listed order, which already puts the quick lane and the
// junior technicians first, never below and +0.0 … +0.3 % on average). Random demand is where
// greedy allocators land close; the scripted quality day (TestQualityDayBeatsEveryFirstFit) is
// where best fit provably wins.
func TestFitDaysGainOverFirstFit(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	const seeds = 6
	const fitPinnedGainPct = 2.0
	monday := booking.Date{Year: 2030, Month: time.January, Day: 7}
	var days []booking.Date
	for i := range seeds * len(fitLoads) {
		days = append(days, monday.AddDays(7*i), monday.AddDays(7*i+5))
	}
	h := newFitHarness(t, days)
	n := 0
	var server, median int
	for s := uint64(1); s <= seeds; s++ {
		perRoster := [2][]fitOutcome{}
		for _, load := range fitLoads {
			for k := range 2 {
				day := days[n]
				n++
				demand := fitDemand(s, load, h.capBound(day), h.w, day, h.mixes, h.weight, h.cars)
				r := h.replay(day, demand, fmt.Sprintf("fit:%d:%s", s, day))
				if err := firstFitShortfall(r.server, r.firstFit, 0); err != nil {
					t.Errorf("seed %d load %.2f on %s: %v", s, load, day, err)
				}
				perRoster[k] = append(perRoster[k], fitOutcome{day: day, load: load, bound: r.bound, asked: r.asked, customers: r.customers,
					server: r.server, serverBooked: r.serverBooked, firstFit: r.spread})
				server += r.server
				median += r.spread.median
			}
		}
		for k, out := range perRoster {
			m := newMatrix()
			recordFitDays(m, settings{}, out)
			if v := rowVerdict(m.rows["Q5"]); v != verdictPass {
				t.Errorf("seed %d roster %d: the stress gate fails: %v", s, k, m.rows["Q5"])
			}
		}
	}
	gain := 100 * float64(server-median) / float64(median)
	t.Logf("pooled over %d seeds × %d loads × 2 rosters: engine %d labour minutes vs first fit in the median random order %d: %+.1f%%", seeds, len(fitLoads), server, median, gain)
	if gain < fitPinnedGainPct {
		t.Fatalf("pooled gain %+.1f%% over the median first-fit order, want ≥ %+.1f%%", gain, fitPinnedGainPct)
	}
}
