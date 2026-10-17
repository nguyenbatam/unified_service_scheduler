package main

import (
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The holds at a moment, rebuilt from history rows: a booking created after the window is left out,
// one created before it counts, a move inside the window holds both places (phantoms), a cancel
// before the window releases, one inside it does not.
func TestTimelineBetween(t *testing.T) {
	t0 := time.Date(2030, 1, 7, 12, 0, 0, 0, time.UTC)
	slot := timeWindow{t0.Add(24 * time.Hour), t0.Add(25 * time.Hour)}
	moved := timeWindow{slot.start.Add(time.Hour), slot.end.Add(time.Hour)}
	pos := func(id uuid.UUID, status int16, w timeWindow, at time.Time) histPos {
		return histPos{hold: oHold{id: id, bay: id, tech: id, status: status, window: w}, at: at}
	}
	early, late, mover, gone, going := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tl := holdTimeline{
		early: {pos(early, 1, slot, t0.Add(-time.Minute))},
		late:  {pos(late, 1, slot, t0.Add(time.Minute))},
		mover: {pos(mover, 1, slot, t0.Add(-time.Hour)), pos(mover, 1, moved, t0.Add(5*time.Second))},
		gone:  {pos(gone, 1, slot, t0.Add(-time.Hour)), pos(gone, 5, slot, t0.Add(-time.Minute))},
		going: {pos(going, 1, slot, t0.Add(-time.Hour)), pos(going, 5, slot, t0.Add(5*time.Second))},
	}
	got := map[uuid.UUID][]oHold{}
	for _, h := range tl.between(t0, t0.Add(10*time.Second)) {
		got[h.id] = append(got[h.id], h)
	}
	if len(got[early]) != 1 || got[early][0].phantom {
		t.Fatalf("a booking made before the refusal holds its place (movable): %+v", got[early])
	}
	if len(got[late]) != 0 {
		t.Fatalf("a booking made after the refusal's answer was not in its way: %+v", got[late])
	}
	if len(got[mover]) != 2 || !got[mover][0].phantom || !got[mover][1].phantom {
		t.Fatalf("a hold moved while the request ran holds both places, never moved again: %+v", got[mover])
	}
	if len(got[gone]) != 0 {
		t.Fatalf("a booking cancelled before the request no longer holds: %+v", got[gone])
	}
	if len(got[going]) != 1 {
		t.Fatalf("a booking cancelled while the request ran still counts as held: %+v", got[going])
	}
	r := refusal{sent: t0, at: t0.Add(time.Second)}
	from, to := pgClock{offset: 2 * time.Second, uncertainty: 10 * time.Millisecond}.decisionWindow(r)
	if !from.Equal(t0.Add(2*time.Second-110*time.Millisecond-txSlack)) || !to.Equal(t0.Add(3*time.Second+110*time.Millisecond)) {
		t.Fatalf("decision window %s – %s", from, to)
	}
}

// judgeRefusals against the engine: a slot filled to refusal, then judged. The same refusal is
// false when timed before the bookings that filled the slot (the shop was empty then), justified
// when timed after them, and justified when untimed (against the end state, which is full) — the
// end state alone cannot tell a refusal made too early.
func TestTimedRefusalsJudgedAgainstTheirMoment(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	ctx := t.Context()
	st := store.New(pool)
	if err := seed.Run(ctx, st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := booking.New(st, nil)
	day := booking.Date{Year: 2030, Month: time.January, Day: 7}
	w, _ := insertTestWorkshop(t, pool, fillWorkshop, "Refusal Moment", 0, 60, []booking.Date{day})
	oil := oracleTestMixes(t, pool)[3]
	cars := oracleTestVehicles(t, pool, 40)
	start := time.Date(2030, time.January, 7, 9, 0, 0, 0, w.loc)
	before := time.Now().Add(-2 * time.Second)
	booked := 0
	for i, car := range cars[:len(cars)-1] {
		_, _, err := svc.Book(ctx, oracleTestAdmin, booking.BookRequest{
			DealershipID: w.dealer.ID, VehicleID: car, ServiceTypeIDs: oil.ids, StartAt: start, IdempotencyKey: fmt.Sprintf("refusal-moment:%d", i),
		})
		if _, refused := errors.AsType[*domain.Error](err); refused {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		booked++
	}
	if booked == 0 || booked == len(cars)-1 {
		t.Fatalf("%d bookings: the slot never filled", booked)
	}
	after := time.Now().Add(time.Second)
	probe := cars[len(cars)-1]
	refused := func(sent, at time.Time) refusal {
		return refusal{mix: oil, start: start, vehicle: probe, sent: sent, at: at}
	}
	for _, tc := range []struct {
		name      string
		r         refusal
		wantFalse int
		wantTimed int
	}{
		{"timed before the slot filled", refused(before.Add(-10*time.Millisecond), before), 1, 1},
		{"timed after the slot filled", refused(after, after.Add(10*time.Millisecond)), 0, 1},
		{"untimed: the end state", refused(time.Time{}, time.Time{}), 0, 0},
	} {
		v, err := judgeRefusals(ctx, pool, w.dealer.ID, []refusal{tc.r}, nil, before.Add(-time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if v.judged != 1 || v.timed != tc.wantTimed || v.falseRefusals != tc.wantFalse {
			t.Fatalf("%s: %+v, want %d false (%d timed)", tc.name, v, tc.wantFalse, tc.wantTimed)
		}
	}
}

// A timed refusal's state keeps a live flagged overrun's widened hold (history only has its raw
// positions), live holds history does not cover, and the phantoms; a covered ordinary hold comes
// from the timeline alone.
func TestAlwaysHeldKeepsOverrunsAndUncovered(t *testing.T) {
	covered, overrun, uncovered := uuid.New(), uuid.New(), uuid.New()
	tl := holdTimeline{covered: {{hold: oHold{id: covered}}}, overrun: {{hold: oHold{id: overrun}}}}
	live := []oHold{{id: covered}, {id: overrun, overrun: true}, {id: uncovered}}
	phantom := oHold{id: uuid.New(), phantom: true}
	got := map[uuid.UUID]bool{}
	for _, h := range alwaysHeld(tl, live, []oHold{phantom}) {
		got[h.id] = true
	}
	if got[covered] || !got[overrun] || !got[uncovered] || !got[phantom.id] || len(got) != 3 {
		t.Fatalf("always held: %v", got)
	}
}
