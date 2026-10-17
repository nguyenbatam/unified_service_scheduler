package worker

import (
	"context"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

type MaintenanceConfig struct {
	Interval        time.Duration // how often to run reclaim + sweeps
	Lease           time.Duration // processing/sending rows older than this (by claimed_at) are reclaimed
	NoShowSweep     bool          // mark confirmed bookings not checked in within the dealership's grace as no_show
	OverrunStep     time.Duration // in-progress jobs past end_at are auto-extended in these steps; 0 disables
	OutboxRetention time.Duration // published outbox rows are deleted after this
	// NotificationRetention: finished notifications (sent, skipped, failed) created and about a slot
	// more than this long ago are deleted. It must exceed the Kafka topic retention (7 days by
	// default): the dedupe_key rows are what make a replay of the notify group harmless.
	NotificationRetention time.Duration
	PurgeEvery            time.Duration
	// ReminderEvery spaces the reminder sweep: reminders are due 24 h / 2 h ahead, so a minute of
	// granularity is plenty, and each sweep reads every confirmed appointment of the next day. The
	// interval is per instance (the last run is remembered in memory): with N instances taking the
	// sweep lock in turn, the sweep runs at most N times per interval — never every tick on every
	// instance, as it did before the lock. The sweep is idempotent, so that bound is only cost.
	ReminderEvery time.Duration
}

// sweepLock is the session advisory lock that elects, per round, the one instance that runs the
// sweeps and purges (store.WithTryLock); the others skip that round.
const sweepLock = "maintenance-sweeps"

// Purges delete in batches of purgeBatch until a batch comes back short, at most purgeMaxBatches
// per table and run, so one instance never spends a whole tick deleting; the next run continues.
const (
	purgeBatch      = 1000
	purgeMaxBatches = 50
)

func DefaultMaintenanceConfig() MaintenanceConfig {
	return MaintenanceConfig{
		Interval:              10 * time.Second,
		Lease:                 30 * time.Second,
		NoShowSweep:           true,
		OverrunStep:           15 * time.Minute,
		OutboxRetention:       7 * 24 * time.Hour,
		NotificationRetention: 30 * 24 * time.Hour,
		PurgeEvery:            10 * time.Minute,
		ReminderEvery:         time.Minute,
	}
}

// Sweeper is the slice of booking.Service the maintenance loop drives: no-shows (confirmed and not
// checked in within the dealership's grace after start) and overruns (in-progress work past its
// booked end).
type Sweeper interface {
	MarkNoShows(ctx context.Context) (int, error)
	HandleOverruns(ctx context.Context, step time.Duration) (extended, flagged int, err error)
}

// Reminders is the slice of notify.Service the loop drives (customer appointment reminders).
type Reminders interface {
	EnqueueReminders(ctx context.Context) (int, error)
}

// Maintenance runs idempotent background jobs. Every instance runs it. The lease reclaims run on
// every instance each tick (cheap, index-backed); the sweeps and purges run on one instance per
// round (sweepLock), because each of them scans tables every instance would otherwise scan again.
// The SQL stays safe to run concurrently (row locks / SKIP LOCKED), so the lock is only about cost:
// an instance that dies mid-round drops it with its session and another takes over next tick.
type Maintenance struct {
	store        *store.Store
	sweeper      Sweeper
	reminders    Reminders
	log          *slog.Logger
	cfg          MaintenanceConfig
	now          func() time.Time
	lastReminder time.Time
}

func NewMaintenance(st *store.Store, sweeper Sweeper, reminders Reminders, log *slog.Logger, cfg MaintenanceConfig) *Maintenance {
	return &Maintenance{store: st, sweeper: sweeper, reminders: reminders, log: log, cfg: cfg, now: time.Now}
}

func (m *Maintenance) Run(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m.Tick(ctx)
		if m.now().Sub(lastPurge) >= m.cfg.PurgeEvery {
			lastPurge = m.now()
			m.asLeader(ctx, "purge", func() { m.Purge(ctx) })
		}
	}
}

// Purge deletes published outbox rows past OutboxRetention, finished notifications past
// NotificationRetention, and refresh sessions whose family ended or whose idle expiry passed more
// than a day ago (store.PurgeAuthSessions).
func (m *Maintenance) Purge(ctx context.Context) {
	purges := []struct {
		name string
		run  func(context.Context) (int64, error)
	}{
		{"outbox purge", func(ctx context.Context) (int64, error) {
			return m.store.PurgePublishedOutbox(ctx, m.cfg.OutboxRetention, purgeBatch)
		}},
		{"notification purge", func(ctx context.Context) (int64, error) {
			return m.store.PurgeNotifications(ctx, m.cfg.NotificationRetention, purgeBatch)
		}},
		{"auth session purge", func(ctx context.Context) (int64, error) {
			return m.store.PurgeAuthSessions(ctx, purgeBatch)
		}},
	}
	for _, p := range purges {
		m.guard(p.name, func() {
			n, err := purgeAll(ctx, p.run)
			if err != nil {
				m.log.Error(p.name, "err", err, "deleted", n)
			} else if n > 0 {
				m.log.Info(p.name, "deleted", n)
			}
		})
	}
}

// guard runs one maintenance task and turns a panic into an error log, so a bug in one task (or
// one poisoned row it reads) neither kills the process on every replica nor skips the tasks after
// it; the task simply runs again next tick.
func (m *Maintenance) guard(task string, run func()) {
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("maintenance task panicked", "task", task, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	run()
}

// purgeAll runs one batched delete until a batch deletes fewer than purgeBatch rows, ctx ends, or
// purgeMaxBatches is reached.
func purgeAll(ctx context.Context, batch func(context.Context) (int64, error)) (int64, error) {
	var total int64
	for range purgeMaxBatches {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := batch(ctx)
		total += n
		if err != nil || n < purgeBatch {
			return total, err
		}
	}
	return total, nil
}

// asLeader runs fn when this instance wins the round's sweepLock; without a store (unit tests)
// it just runs fn.
func (m *Maintenance) asLeader(ctx context.Context, round string, fn func()) {
	if m.store == nil {
		fn()
		return
	}
	if _, err := m.store.WithTryLock(ctx, sweepLock, fn); err != nil && ctx.Err() == nil {
		m.log.Error("maintenance lock", "round", round, "err", err)
	}
}

// Tick runs one reclaim round, then (on the instance holding sweepLock) the reminder (at most every
// ReminderEvery), no-show, and overrun sweeps. Each task runs under guard: a failing or panicking
// task never stops the ones after it.
func (m *Maintenance) Tick(ctx context.Context) {
	m.guard("outbox reclaim", func() {
		if n, err := m.store.ReclaimStaleOutbox(ctx, m.cfg.Lease); err != nil {
			m.log.Error("outbox reclaim", "err", err)
		} else if n > 0 {
			m.log.Warn("outbox reclaimed expired leases", "rows", n)
		}
	})
	m.guard("notification reclaim", func() {
		if n, err := m.store.ReclaimStaleNotifications(ctx, m.cfg.Lease); err != nil {
			m.log.Error("notification reclaim", "err", err)
		} else if n > 0 {
			m.log.Warn("notifications reclaimed expired leases", "rows", n)
		}
	})
	m.asLeader(ctx, "sweeps", func() { m.sweep(ctx) })
}

func (m *Maintenance) sweep(ctx context.Context) {
	if m.reminders != nil && m.now().Sub(m.lastReminder) >= m.cfg.ReminderEvery {
		m.lastReminder = m.now()
		m.guard("reminder sweep", func() {
			if n, err := m.reminders.EnqueueReminders(ctx); err != nil {
				m.log.Error("reminder sweep", "err", err)
			} else if n > 0 {
				m.log.Info("reminder sweep", "enqueued", n)
			}
		})
	}
	if m.sweeper == nil {
		return
	}
	if m.cfg.NoShowSweep {
		m.guard("no-show sweep", func() {
			if n, err := m.sweeper.MarkNoShows(ctx); err != nil {
				m.log.Error("no-show sweep", "err", err)
			} else if n > 0 {
				m.log.Info("no-show sweep", "marked", n)
			}
		})
	}
	if m.cfg.OverrunStep > 0 {
		m.guard("overrun sweep", func() {
			if extended, flagged, err := m.sweeper.HandleOverruns(ctx, m.cfg.OverrunStep); err != nil {
				m.log.Error("overrun sweep", "err", err)
			} else if extended > 0 || flagged > 0 {
				m.log.Info("overrun sweep", "extended", extended, "flagged", flagged)
			}
		})
	}
}
