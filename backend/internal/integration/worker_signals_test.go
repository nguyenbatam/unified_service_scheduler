package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A commit that wrote outbox rows wakes the local outbox worker at once (no NOTIFY round trip
// through Postgres); a rolled-back write or a commit without events does not.
func TestCommitWithEventsWakesOutboxWorker(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	drain := func() {
		select {
		case <-st.OutboxWakeups():
		default:
		}
	}
	woke := func() bool {
		select {
		case <-st.OutboxWakeups():
			return true
		case <-time.After(50 * time.Millisecond):
			return false
		}
	}
	drain()
	// Outbox rows written, then rolled back: no wake-up.
	rolled, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := rolled.InsertOutbox(ctx, domain.AggregateDealership, seed.DealerRiver, domain.EventConfigUpdated, map[string]int{"version": 1}); err != nil {
		t.Fatal(err)
	}
	if err := rolled.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if woke() {
		t.Fatal("a rolled-back write of outbox rows woke the outbox worker")
	}
	// A plain commit without events: no wake-up.
	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if woke() {
		t.Fatal("a commit without outbox rows woke the outbox worker")
	}
	// A booking commits its CREATED event: woken.
	if _, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, londonDay(t, 1), 9, 0)); err != nil {
		t.Fatal(err)
	}
	if !woke() {
		t.Fatal("a committed booking did not wake the outbox worker")
	}
}

// Only one caller at a time runs under a WithTryLock name; the others skip instead of waiting.
func TestWithTryLockRunsOneHolderAtATime(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	inner := make(chan bool, 1)
	ran, err := st.WithTryLock(ctx, "review2", func() {
		ranInner, err := st.WithTryLock(ctx, "review2", func() { t.Error("second holder ran while the first held the lock") })
		if err != nil {
			t.Error(err)
		}
		inner <- ranInner
	})
	if err != nil || !ran {
		t.Fatalf("first holder: ran=%v err=%v", ran, err)
	}
	if <-inner {
		t.Fatal("second caller reported running")
	}
	// Released afterwards. Asking again proves nothing (the pool hands back the same session, and
	// session advisory locks are re-entrant): no advisory lock may be left held in this database.
	if n := scalar[int](t, pool, `
		SELECT count(*)::int FROM pg_locks
		WHERE locktype = 'advisory' AND granted
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`); n != 0 {
		t.Fatalf("%d advisory lock(s) still held after WithTryLock returned", n)
	}
}

// At shutdown the server sends the NOTIFY a commit asked for after the notifier stopped
// (FlushOutboxNotify): the other instances' listeners wake; with nothing pending it sends none.
//
// No sleeps: readiness of the store's listener is read from pg_stat_activity (its LISTEN ran and
// it is idle, waiting), so no probe NOTIFY can arrive late; the "sent none" side is proved by a
// raw listener of the test's own and a fence: notifications reach a session in commit order, so
// a NOTIFY from the flush would arrive before the test's fence (payload "fence"; the flush sends ”).
func TestFlushOutboxNotifyAtShutdown(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Active once Exec returns (autocommit). Quoted, so its text differs from the store listener's
	// LISTEN that the readiness check below looks for.
	if _, err := conn.Exec(ctx, `LISTEN "`+store.OutboxChannel+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `UNLISTEN *`) }()
	// nextPayload returns the payload of the next notification on the test's own listener.
	nextPayload := func() string {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := conn.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("waiting for a notification: %v", err)
		}
		return n.Payload
	}

	listenCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	woke := make(chan struct{}, 8)
	done := make(chan error, 1)
	go func() {
		done <- st.ListenOutbox(listenCtx, func() {
			select { // never block the listener
			case woke <- struct{}{}:
			default:
			}
		})
	}()
	// The store's listener is ready once its LISTEN statement has run and it sits idle waiting (the
	// exact text: the test's own listener above quoted the channel).
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if n := scalar[int](t, pool, `
			SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND state = 'idle' AND query = 'LISTEN ' || $1`, store.OutboxChannel); n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("LISTEN never became ready")
		}
	}

	if err := st.FlushOutboxNotify(ctx); err != nil { // nothing committed since: no NOTIFY
		t.Fatal(err)
	}
	if err := st.Exec(ctx, `SELECT pg_notify($1, 'fence')`, store.OutboxChannel); err != nil {
		t.Fatal(err)
	}
	if p := nextPayload(); p != "fence" {
		t.Fatalf("FlushOutboxNotify sent a NOTIFY (payload %q) with no commit pending", p)
	}
	// The store's listener got the fence (and nothing before it): one wake-up, then none queued.
	select {
	case <-woke:
	case <-time.After(5 * time.Second):
		t.Fatal("the store's listener did not wake for the fence")
	}

	if _, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonOn(t, londonDay(t, 1), 9, 0)); err != nil {
		t.Fatal(err)
	}
	if err := st.FlushOutboxNotify(ctx); err != nil {
		t.Fatal(err)
	}
	if p := nextPayload(); p != "" {
		t.Fatalf("after a commit with events FlushOutboxNotify sent payload %q, want the empty wake-up", p)
	}
	select {
	case <-woke:
	case <-time.After(5 * time.Second):
		t.Fatal("the pending wake-up did not reach the store's listener")
	}
	cancel()
	<-done
}
