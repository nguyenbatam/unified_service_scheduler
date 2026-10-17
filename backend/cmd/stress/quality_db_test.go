package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The quality day (qualityday.go) against the engine itself (booking.Service, the system under
// test; the first-fit replays, the optimum and the integrity pass stay the stress client's own),
// on every PR: the margin the stress phase requires is pinned here, and so is how the engine gets
// it — every request booked, A3 placed best fit behind A2, B3 booked by moving the waiter's job B1
// into the quick lane, C2 by moving the oil change C1 to the other senior technician.
func TestQualityDayBeatsEveryFirstFit(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	ctx := t.Context()
	st := store.New(pool)
	if err := seed.Run(ctx, st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := booking.New(st, nil)
	day := booking.Date{Year: 2030, Month: time.January, Day: 9}
	w, ids := insertTestWorkshop(t, pool, qualityWorkshop, "Quality Test", 0, 60, nil)
	names := map[uuid.UUID]string{}
	for name, id := range ids {
		names[id] = name
	}
	jobs, err := qualityMixes(oracleTestCatalog(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	var mixKeys []string
	for _, code := range qualityServices {
		mixKeys = append(mixKeys, mixKey(jobs[code].ids))
	}
	demand, err := qualityDemand(w, day, jobs, oracleTestVehicles(t, pool, len(qualityScript)))
	if err != nil {
		t.Fatal(err)
	}
	keyPrefix := "stress:qtest:quality"
	outcomes := make([]qualityOutcome, len(demand))
	label := map[uuid.UUID]string{}
	for i, r := range demand {
		mark, err := historyMark(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		appt, _, err := svc.Book(ctx, oracleTestAdmin, booking.BookRequest{
			DealershipID: w.dealer.ID, VehicleID: r.vehicle, ServiceTypeIDs: r.mix.ids, StartAt: r.start,
			IsWaiter: r.waiter, IdempotencyKey: fmt.Sprintf("%s:%d", keyPrefix, i),
		})
		o := &outcomes[i]
		var derr *domain.Error
		switch {
		case errors.As(err, &derr):
			o.answer = derr.Code
			continue
		case err != nil:
			t.Fatalf("book %s: %v", r.label, err)
		}
		o.booked, o.id, o.bay, o.tech = true, appt.ID, names[appt.ServiceBayID], names[appt.TechnicianID]
		label[appt.ID] = r.label
		if o.moves, err = qualityMovesSince(ctx, pool, w.dealer.ID, mark, names); err != nil {
			t.Fatal(err)
		}
		for k := range o.moves {
			o.moves[k].label = label[o.moves[k].appointment]
		}
		t.Logf("%s %s %s → %s", r.label, r.mix.name, r.start.In(w.loc).Format("15:04"), describeOutcome(*o))
	}
	v, err := judgeQualityDay(ctx, pool, w, day, demand, outcomes, keyPrefix, mixKeys, names)
	if err != nil {
		t.Fatal(err)
	}
	if v.server != 960 || v.asked != 960 || v.optimum != 960 {
		t.Fatalf("the engine booked %d of %d labour minutes asked (optimum %d), want all 960", v.server, v.asked, v.optimum)
	}
	if v.spread.best.minutes != 780 || v.spread.worst.minutes != 600 {
		t.Fatalf("first fit on the oracle's model read from the tables: %d … %d, want 600 … 780", v.spread.worst.minutes, v.spread.best.minutes)
	}
	if len(v.integrity) > 0 {
		t.Fatalf("SQL integrity over the quality day: %v", v.integrity)
	}
	byLabel := map[string]qualityOutcome{}
	var moves []string
	for i, o := range outcomes {
		byLabel[demand[i].label] = o
		for _, mv := range o.moves {
			moves = append(moves, demand[i].label+" moved "+mv.String())
		}
	}
	if a3 := byLabel["A3"]; a3.bay != "General 2" {
		t.Fatalf("A3 went to %s: best fit puts it right behind A2 in General 2", a3.bay)
	}
	want := []string{"B3 moved B1 (bay General 2 → Express 1)", "C2 moved C1 (technician Lube Brakes → Lube General)"}
	if strings.Join(moves, "; ") != strings.Join(want, "; ") {
		t.Fatalf("repairs %v, want %v", moves, want)
	}
	m := newMatrix()
	v.record(m, defaultQualityMarginPct)
	if failed := m.failed(); len(failed) > 0 {
		t.Fatalf("the quality day's evidence failed: %v", m.rows["Q5"])
	}
	if unmet := m.enforceRequired([]string{"Q5/beats first fit", "Q5/repair"}); len(unmet) > 0 {
		t.Fatalf("named checks never passed: %v", unmet)
	}
	t.Logf("engine %d, first fit %d … %d (%d replays), %s vs the best; %v", v.server, v.spread.worst.minutes, v.spread.best.minutes, v.spread.runs, gainPct(v.server, v.spread.best.minutes), moves)
}
