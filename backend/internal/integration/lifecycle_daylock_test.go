package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Check-in (1 → 2) and an on-time start (2 → 3) rewrite the hold's GiST entries (status is in the
// exclusion predicates), so they queue on the dealership-day lock like every other writer of
// holds: without it, a booking that inserted over the hold from stale candidates and the status
// change wait on each other's entries until deadlock_timeout breaks one.
func TestHoldStatusChangesQueueOnTheDayLock(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0))
	if err != nil {
		t.Fatal(err)
	}
	clock := serviceOn(t, st, today, 9, 0)

	waitsForDayLock(t, pool, londonOn(t, today, 9, 0), "check-in", func() error {
		_, err := clock.CheckIn(ctx, staffDispatcher, job.ID, 0)
		return err
	})
	waitsForDayLock(t, pool, londonOn(t, today, 9, 0), "on-time start", func() error {
		_, err := clock.StartWork(ctx, staffDispatcher, job.ID, 0)
		return err
	})
}

// waitsForDayLock holds Riverside's dealership-day lock for the day of at, runs change, and fails
// unless change blocks on that lock and then succeeds once it is released.
func waitsForDayLock(t *testing.T, pool *pgxpool.Pool, at time.Time, label string, change func() error) {
	t.Helper()
	holder, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.WithoutCancel(t.Context())) }()
	if _, err := holder.Exec(t.Context(), dayLockSQL, seed.DealerRiver, at); err != nil {
		t.Fatal(err)
	}
	var finished atomic.Bool
	done := make(chan error, 1)
	go func() {
		err := change()
		finished.Store(true)
		done <- err
	}()
	awaitLockWaiter(t, pool, "pg_advisory_xact_lock")
	if finished.Load() {
		t.Fatalf("%s finished without waiting for the day lock", label)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("%s after the day lock was released: %v", label, err)
	}
}
