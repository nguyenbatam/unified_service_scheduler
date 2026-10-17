package main

import (
	"fmt"
	"log/slog"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// TestOracleAgreesWithAvailability away from London, where a calendar bug keyed on the UTC date
// or on wall clocks rebuilt as instants shows: the truth/fill workshop in Sydney in January
// (summer time, UTC+11: every start before 11:00 local lies on the previous UTC date), and a shop
// open 00:30–06:00 in New York on both 2030 clock-change Sundays (02:00 → 03:00 on 10 March, 02:00
// → 01:00 on 3 November), with 30-minute staging windows, so the skipped and the repeated hour
// fall inside its opening hours. Each day is filled by the engine and judged after every booking,
// like the London test.
func TestOracleAgreesWithAvailabilityAcrossZones(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	ctx := t.Context()
	st := store.New(pool)
	if err := seed.Run(ctx, st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	j := &oracleJudge{t: t, pool: pool, svc: booking.New(st, nil), mixes: oracleTestMixes(t, pool)}
	vehicles := oracleTestVehicles(t, pool, 300)
	next := 0
	car := func() uuid.UUID {
		next++
		return vehicles[next-1]
	}
	monday := booking.Date{Year: 2030, Month: time.January, Day: 7}
	sydney, _ := insertTestWorkshopIn(t, pool, fillWorkshop, "Oracle Sydney", "Australia/Sydney", 2, 60, []booking.Date{monday})
	if open := time.Date(2030, time.January, 7, 8, 0, 0, 0, sydney.loc).UTC(); open.Day() != 6 {
		t.Fatalf("Sydney's 08:00 on %s is %s UTC: the day no longer straddles a UTC date", monday, open)
	}
	j.fillAndJudge(sydney, monday, car, 1)

	night := workshopDef{name: "Oracle Night", open: "00:30", close: "06:00", bays: fillWorkshop.bays, techs: fillWorkshop.techs}
	ny, _ := insertTestWorkshopIn(t, pool, night, "Oracle Night NY", "America/New_York", 2, 30, nil)
	for k, day := range []booking.Date{{Year: 2030, Month: time.March, Day: 10}, {Year: 2030, Month: time.November, Day: 3}} {
		open, closeAt := time.Date(day.Year, day.Month, day.Day, 0, 30, 0, 0, ny.loc), time.Date(day.Year, day.Month, day.Day, 6, 0, 0, 0, ny.loc)
		if elapsed := closeAt.Sub(open); elapsed == 330*time.Minute {
			t.Fatalf("%s: 00:30–06:00 is %s, no clock change inside the opening hours", day, elapsed)
		}
		j.fillAndJudge(ny, day, car, uint64(10+k))
	}
	t.Logf("%d slot verdicts agreed (%d offered, %d of them only through a one-move repair; %d refused)", j.agreed, j.offered, j.repairOnly, j.refused)
}

// fillAndJudge books random job mixes (some waiting, some checked in afterwards) on day at w
// through the engine and judges every start against the oracle after each booking.
func (j *oracleJudge) fillAndJudge(w workshop, day booking.Date, car func() uuid.UUID, seedN uint64) {
	t := j.t
	t.Helper()
	rng := rand.New(rand.NewPCG(seedN, uint64(day.Day))) //nolint:gosec // reproducible choices, not security
	starts, err := dayStarts(w, day, slotMinutes)
	if err != nil {
		t.Fatal(err)
	}
	probe := car()
	j.judge(w, day, probe, "empty")
	booked := 0
	for attempt := range 60 {
		mix := j.mixes[rng.IntN(len(j.mixes))]
		start := starts[rng.IntN(len(starts))].start
		appt, ok := j.book(w, mix, start, car(), rng.IntN(3) == 0, fmt.Sprintf("oracle-zones:%s:%s:%d", w.dealer.Name, day, attempt))
		if !ok {
			continue
		}
		booked++
		if rng.IntN(5) == 0 {
			if _, err := j.pool.Exec(t.Context(), `UPDATE appointments SET status = `+sqlCode(domain.StatusCheckedIn)+` WHERE id = $1`, appt.ID); err != nil {
				t.Fatal(err)
			}
		}
		j.judge(w, day, probe, fmt.Sprintf("%d bookings", booked))
	}
	if booked < 8 {
		t.Fatalf("%s %s: only %d bookings, the day was barely exercised", w.dealer.Name, day, booked)
	}
}
