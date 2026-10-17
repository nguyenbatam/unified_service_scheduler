package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// bench provisions the bench shop (and, for the journey, signed-in customers with their own
// cars) and runs the capacity ramp.
func (ss *session) bench(ctx context.Context, stop func()) (err error) {
	if ss.conn == nil {
		return errors.New("bench needs STRESS_DATABASE_URL (Postgres commits, hot-round counts, integrity, cleanup)")
	}
	s := ss.s
	w, err := ensureWorkshop(ctx, ss.api, benchWorkshop())
	if err != nil {
		return err
	}
	oil, err := newJobMix(ss.catalog, "OIL", "OIL")
	if err != nil {
		return err
	}
	fleet, err := ensureVehicles(ctx, ss.api, ss.owner, vinBench, 5*benchBays, 64)
	if err != nil {
		return err
	}
	today, err := booking.ParseDate(w.dealer.Today)
	if err != nil {
		return err
	}
	first := today.AddDays(1)
	tgt := &benchTarget{shop: w, oil: oil.ids, vehicles: fleet, firstDay: first, days: s.benchDays, runID: ss.runID}
	if slices.Contains(s.bench.workloads, "journey") {
		if tgt.customers, err = provisionCustomers(ctx, ss.conn, ss.api, s.base, benchBays, 32); err != nil {
			return fmt.Errorf("bench journey customers: %w", err)
		}
	}
	horizon := first.AddDays(tgt.lastDay())
	if err := ss.resetDay(ctx, w, first.Start(w.loc), horizon.Start(w.loc)); err != nil {
		return err
	}
	defer func() {
		stop()
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		fmt.Println("bench: cancelling the bench bookings")
		if cerr := ss.resetDay(cctx, w, first.Start(w.loc), horizon.Start(w.loc)); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	if err := benchRoom(s, tgt, int(w.dealer.Policy.BookingHorizonDays)); err != nil {
		return err
	}
	return runBench(ctx, s, ss.api, ss.conn, tgt, ss.m)
}

// benchRoom refuses a ramp that would run out of fresh starts: "book" and "journey" past their
// days would repeat starts (and get refused, failing an uncontended step for the wrong reason),
// and "hot" past its days would wrap onto full starts, where every request is a 409 and a step
// could pass with no booking at all. The journey books as customers, who may book at most
// horizonDays ahead: its last day (today + 2·days + hotDays) must lie within that, or its late
// requests would be refused by policy, not capacity.
func benchRoom(s settings, tgt *benchTarget, horizonDays int) error {
	if slices.Contains(s.bench.workloads, "journey") {
		if last := tgt.lastDay(); last > horizonDays {
			return fmt.Errorf("bench: the journey books up to %d days ahead (2 × -bench-days %d + %d hot days) but customers may book at most %d days ahead (the bench shop's bookingHorizonDays); lower -bench-days",
				last, tgt.days, hotDays, horizonDays)
		}
	}
	sent := func(wl string) int {
		n := 0
		for _, r := range s.bench.rates[wl] {
			n += int(r * s.step().Seconds())
		}
		return n
	}
	for _, wl := range []string{"book", "journey"} {
		if !slices.Contains(s.bench.workloads, wl) {
			continue
		}
		if need := sent(wl); need > tgt.capacityBookings() {
			return fmt.Errorf("bench: the %s ramp sends up to %d bookings but %d days hold %d; raise -bench-days", wl, need, s.benchDays, tgt.capacityBookings())
		}
	}
	if slices.Contains(s.bench.workloads, "hot") {
		perRound := 5 * benchBays
		if rounds := (sent("hot") + perRound - 1) / perRound; rounds > tgt.hotRoundsCapacity() {
			return fmt.Errorf("bench: the hot ramp sends %d rounds but its %d days hold %d fresh starts; lower -bench-hot-rates or -bench-step",
				rounds, hotDays, tgt.hotRoundsCapacity())
		}
	}
	return nil
}
