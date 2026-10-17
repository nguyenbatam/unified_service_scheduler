package integration_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A free-running race of two requests usually ends the same way every time (whichever reaches
// Postgres first), so its other outcome may never be exercised. The two-sided race tests therefore
// pin each order once — the first side holds its locks, uncommitted, while the second arrives and
// queues behind them — and then run free-running rounds too, logging who won.

// gate is a lock the test takes in a transaction of its own to pause one side of a race at a known
// point, after it took the locks the other side needs: sql (with args) runs in the gate TX, and
// marker is text of the paused side's blocked statement (see awaitLockWaiter).
type gate struct {
	sql    string
	args   []any
	marker string
}

// dayGate pauses a booking, reschedule, reinstate, reassign or early start of dealership's day
// containing at on its dealership-day lock (store.LockArrival), taken after its share locks on the
// dealership, the technician, and (for an existing appointment) the appointment row.
func dayGate(dealership uuid.UUID, at time.Time) gate {
	return gate{sql: dayLockSQL, args: []any{dealership, at}, marker: "dealer-day"}
}

// outboxGate pauses a config writer (calendar, roster, technician) on its CONFIG_UPDATED insert:
// by then it holds its row FOR UPDATE and has run its stranded-booking check. Bookings read
// outbox_events never, so only writers wait on the gate.
var outboxGate = gate{sql: `LOCK TABLE outbox_events IN SHARE MODE`, marker: "INSERT INTO outbox_events"}

// pinnedRace runs first until it blocks on g, then second until it blocks on a lock while running
// a statement containing queued (behind first's locks, or behind first in the queue of the same
// lock), then opens the gate: first commits (or fails) before second can go on.
func pinnedRace(t *testing.T, pool *pgxpool.Pool, g gate, first func(), queued string, second func()) {
	t.Helper()
	ctx := t.Context()
	var wg sync.WaitGroup
	defer wg.Wait() // after the deferred rollback below (LIFO), so a failed wait never strands the goroutines
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, g.sql, g.args...); err != nil {
		t.Fatal(err)
	}
	wg.Go(first)
	awaitLockWaiter(t, pool, g.marker)
	before := lockWaiters(t, pool, queued) // first itself, when both queue on the same lock
	wg.Go(second)
	awaitLockWaiters(t, pool, queued, before+1)
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
}

// raceOrder is how one round of a two-sided race is run.
type raceOrder int

const (
	aFirst  raceOrder = iota // side a pinned first
	bFirst                   // side b pinned first
	freeRun                  // both released together (race)
)

func (o raceOrder) String() string {
	return [...]string{"a first", "b first", "free-running"}[o]
}

// raceOrders: each order pinned once, then two free-running rounds.
var raceOrders = []raceOrder{aFirst, bFirst, freeRun, freeRun}

// raceSide is one side of a two-sided race: run is the request; gate pauses it when it goes
// first; queued is text of its blocked statement while it waits behind the other side.
type raceSide struct {
	run    func()
	gate   gate
	queued string
}

// runOrdered runs one round of a against b in order.
func runOrdered(t *testing.T, pool *pgxpool.Pool, order raceOrder, a, b raceSide) {
	t.Helper()
	switch order {
	case aFirst:
		pinnedRace(t, pool, a.gate, a.run, b.queued, b.run)
	case bFirst:
		pinnedRace(t, pool, b.gate, b.run, a.queued, a.run)
	default:
		race(a.run, b.run)
	}
}

// wantOrderWinner fails unless the pinned side won its round (winner is 0 for a, 1 for b); a
// free-running round may go either way and is logged, so a run shows which outcomes it covered.
func wantOrderWinner(t *testing.T, label string, order raceOrder, winner int, sides [2]string) {
	t.Helper()
	switch {
	case order == aFirst && winner != 0, order == bFirst && winner != 1:
		t.Fatalf("%s: %s pinned first, but %s won", label, sides[order], sides[winner])
	case order == freeRun:
		t.Logf("%s: free-running round won by %s", label, sides[winner])
	}
}
