package integration_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

// The purge loops over 1000-row batches until a batch comes back short, so a backlog larger than
// one batch is cleared in one run; finished notifications past retention are purged too.
func TestPurgeLoopsAndCoversNotifications(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE outbox_events SET status = ` + code(domain.OutboxPublished) + `, published_at = now()`) // seed events: recent
	exec(`
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status, published_at)
		SELECT gen_random_uuid(), ` + code(domain.AggregateTechnician) + `, gen_random_uuid(), ` + code(domain.EventConfigUpdated) + `, '{}', ` + code(domain.OutboxPublished) + `, now() - interval '8 days'
		FROM generate_series(1, 2500)`)
	exec(`
		INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload, status, created_at)
		SELECT gen_random_uuid(), ` + code(domain.AggregateTechnician) + `, gen_random_uuid(), ` + code(domain.EventConfigUpdated) + `, '{}', ` + code(domain.OutboxFailed) + `, now() - interval '8 days'
		FROM generate_series(1, 3)`) // poisoned rows are kept for the operator
	keepOutbox := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE published_at IS NULL OR published_at > now() - interval '1 day'`)

	appointment := scalar[string](t, pool, `SELECT id::text FROM appointments LIMIT 1`)
	notification := `
		INSERT INTO notifications (id, appointment_id, kind, channel, recipient, subject, body, dedupe_key, status, start_at, created_at)
		SELECT gen_random_uuid(), $1::uuid, ` + code(domain.KindConfirmation) + `, ` + code(domain.ChannelEmail) + `, 'a@example.com', 's', 'b',
		       $2 || g, $3, now() - $4::interval, now() - $5::interval
		FROM generate_series(1, $6::int) g`
	exec(notification, appointment, "oldsent:", int16(domain.NotificationSent), "40 days", "40 days", 1500)
	exec(notification, appointment, "oldskipped:", int16(domain.NotificationSkipped), "40 days", "40 days", 600)
	exec(notification, appointment, "oldpending:", int16(domain.NotificationPending), "40 days", "40 days", 1) // not finished: kept
	exec(notification, appointment, "recent:", int16(domain.NotificationSent), "40 days", "1 day", 1)          // created recently: kept
	exec(notification, appointment, "future:", int16(domain.NotificationSent), "-10 days", "40 days", 1)       // slot ahead: kept (reminder dedupe)

	worker.NewMaintenance(st, nil, nil, slog.Default(), worker.DefaultMaintenanceConfig()).Purge(ctx)

	if got := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events`); got != keepOutbox {
		t.Fatalf("outbox rows after purge = %d, want %d", got, keepOutbox)
	}
	got := scalar[string](t, pool, `SELECT string_agg(split_part(dedupe_key, ':', 1), ',' ORDER BY dedupe_key) FROM notifications`)
	if got != "future,oldpending,recent" {
		t.Fatalf("notifications after purge = %s", got)
	}
}

// Queue depths report every status (zeros included) and every count is served by a partial index.
func TestQueueDepthsUseIndexes(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	depths, err := st.QueueDepths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, d := range depths {
		seen[d.Queue+"/"+d.Status] = true
	}
	for _, k := range []string{"outbox/pending", "outbox/processing", "outbox/failed", "notifications/pending", "notifications/sending", "notifications/failed", "workshop/stale_overrun"} {
		if !seen[k] {
			t.Fatalf("queue depth %s missing: %+v", k, depths)
		}
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `RESET enable_seqscan`) }()
	for table, statuses := range map[string][]string{
		"outbox_events": {code(domain.OutboxPending), code(domain.OutboxFailed)},
		"notifications": {code(domain.NotificationPending), code(domain.NotificationFailed)},
	} {
		for _, status := range statuses {
			rows, err := conn.Query(ctx, `EXPLAIN SELECT count(*) FROM `+table+` WHERE status = `+status)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			if p := strings.Join(plan, "\n"); strings.Contains(p, "Seq Scan") {
				t.Fatalf("%s %s count scans the table:\n%s", table, status, p)
			}
		}
	}
}
