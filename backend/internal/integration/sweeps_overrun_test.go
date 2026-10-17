package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Flagged jobs already past the auto-extension cap can never be extended again; they used to be
// re-locked on every sweep and, sorted by end_at ahead of newer jobs, filled the 50-row batch so a
// flagged job whose blocker had moved never resumed. They are now left out of the lock, while an
// unflagged job at the cap still comes through once to be flagged.
func TestOverrunSweepSkipsCappedFlaggedJobs(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	// Copies of the job on earlier days, in progress and already extended to the cap (booked
	// 30 min + MaxExtend): 60 flagged, plus one never flagged.
	copies := `
		INSERT INTO appointments
		SELECT (jsonb_populate_record(a, jsonb_build_object(
			'id', gen_random_uuid(),
			'status', 3,
			'start_at', a.start_at - make_interval(days => g),
			'end_at', a.start_at - make_interval(days => g) + make_interval(mins => a.booked_duration_minutes)
			          + make_interval(secs => $2::float8),
			'idempotency_key', NULL,
			'idempotency_fingerprint', NULL,
			'overrun_at', CASE WHEN $3 THEN now() END))).*
		FROM appointments a, generate_series($4::int, $5::int) g
		WHERE a.id = $1`
	maxExtend := booking.MaxExtend.Seconds()
	if _, err := pool.Exec(ctx, copies, job.ID, maxExtend, true, 1, 60); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, copies, job.ID, maxExtend, false, 61, 61); err != nil {
		t.Fatal(err)
	}
	// The job itself overran, was flagged (blocked), and its blocker has since moved.
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`, overrun_at = now() WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}

	clock := serviceOn(t, st, today, 9, 40)
	extended, flagged, err := clock.HandleOverruns(ctx, 15*time.Minute)
	if err != nil || extended != 1 || flagged != 1 {
		t.Fatalf("sweep = extended %d flagged %d err %v, want 1/1 (capped flagged jobs must not starve the batch)", extended, flagged, err)
	}
	got, err := st.GetAppointment(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.OverrunAt == nil || !got.EndAt.Equal(londonOn(t, today, 9, 45)) {
		t.Fatalf("job = overrunAt %v end %s, want still flagged / 09:45", got.OverrunAt, got.EndAt)
	}

	// Nothing left that the sweep could act on, so nothing is picked.
	due, err := st.OverrunCandidates(ctx, londonOn(t, today, 9, 44), booking.MaxExtend, 50)
	if err != nil || len(due) != 0 {
		t.Fatalf("picked %d capped/flagged overruns (err %v), want 0", len(due), err)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE status = `+code(domain.StatusInProgress)+` AND overrun_at IS NOT NULL`); n != 62 {
		t.Fatalf("flagged in progress = %d, want 62 (all stay flagged, the resumed job included)", n)
	}
}

// An extension that waits on a booking still in flight over the same vehicle (GiST waits for the
// other TX before it can tell whether they conflict) gives up after the sweep's short lock_timeout
// and is retried on the next sweep: not extended, not flagged (the other TX may roll back), and
// the batch is not held for the pool's 3 s lock_timeout. Once the other TX is gone, GiST blocks the
// extension and the job is flagged as usual.
func TestOverrunSweepLockTimeoutRetriesNextTick(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	vehicle := newVehicle(t, svc)
	job, err := bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	next, err := bookAt(t, svc, seed.DealerRiver, vehicle, seed.ServiceOil, londonOn(t, today, 9, 30)) // same car, right after
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+` WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	// A dispatcher's change to the next booking is in flight (row updated, not committed).
	other, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Rollback(context.Background()) }()
	if _, err := other.Exec(ctx, `UPDATE appointments SET version = version + 1 WHERE id = $1`, next.ID); err != nil {
		t.Fatal(err)
	}

	sweeper := serviceOn(t, st, today, 9, 40)
	began := time.Now()
	extended, flagged, err := sweeper.HandleOverruns(ctx, 15*time.Minute)
	took := time.Since(began)
	if err != nil || extended != 0 || flagged != 0 {
		t.Fatalf("sweep behind an in-flight booking = extended %d flagged %d err %v, want 0/0/nil (retry next tick)", extended, flagged, err)
	}
	// The sweep gives up at its own short lock_timeout; waiting for the pool's (3 s) would mean the
	// per-job timeout is not applied. Three quarters of the pool's leaves room for a slow CI box.
	if limit := store.DefaultPoolOptions().LockTimeout * 3 / 4; took > limit {
		t.Fatalf("sweep waited %s behind one in-flight booking, want about the sweep lock_timeout (well under %s)", took, limit)
	}
	if got := scalar[*time.Time](t, pool, `SELECT overrun_at FROM appointments WHERE id = $1`, job.ID); got != nil {
		t.Fatalf("flagged while the blocker was undecided: overrun_at %v", got)
	}

	if err := other.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	extended, flagged, err = sweeper.HandleOverruns(ctx, 15*time.Minute)
	if err != nil || extended != 0 || flagged != 1 {
		t.Fatalf("next sweep = extended %d flagged %d err %v, want 0/1 (GiST blocks: flag once)", extended, flagged, err)
	}
}

// Flagging a job already at the cap (nothing grows) still queues on the dealership-day lock:
// Reassign, Reinstate, and the repair read the overrun holds after taking that lock, so a flag
// committed past it could put a car onto a bay that is still occupied. While another writer holds
// the day the sweep gives up at its short lock_timeout and flags on the next tick.
func TestOverrunFlagAtCapQueuesOnTheDayLock(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0)) // 09:00–09:30
	if err != nil {
		t.Fatal(err)
	}
	// In progress and already extended to the cap, never flagged.
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusInProgress)+`,
		end_at = start_at + make_interval(mins => booked_duration_minutes) + make_interval(secs => $2::float8)
		WHERE id = $1`, job.ID, booking.MaxExtend.Seconds()); err != nil {
		t.Fatal(err)
	}

	writer, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if err := writer.LockDealershipDay(ctx, seed.DealerRiver, uuid.Nil, job.StartAt, job.StartAt.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}

	sweeper := serviceOn(t, st, today, 13, 40) // the cap is 09:00 + 30 min + MaxExtend (4 h) = 13:30
	extended, flagged, err := sweeper.HandleOverruns(ctx, 15*time.Minute)
	if err != nil || extended != 0 || flagged != 0 {
		t.Fatalf("sweep while the day is held = extended %d flagged %d err %v, want 0/0/nil (retry next tick)", extended, flagged, err)
	}
	if got := scalar[*time.Time](t, pool, `SELECT overrun_at FROM appointments WHERE id = $1`, job.ID); got != nil {
		t.Fatalf("flagged without the day lock: overrun_at %v", got)
	}
	if err := writer.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	extended, flagged, err = sweeper.HandleOverruns(ctx, 15*time.Minute)
	if err != nil || extended != 0 || flagged != 1 {
		t.Fatalf("next sweep = extended %d flagged %d err %v, want 0/1", extended, flagged, err)
	}
}
