package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Customer notifications are a durable queue: rows are enqueued idempotently (dedupe_key) from
// Kafka appointment events and from the reminder sweep, then delivered by a lease-based sender.

type NewNotification struct {
	ID            uuid.UUID
	AppointmentID uuid.UUID
	Kind          domain.NotificationKind
	Channel       domain.NotificationChannel
	Recipient     string
	Subject       string
	Body          string
	DedupeKey     string
	StartAt       time.Time // the slot it is about (reminders are skipped if the appointment moved)
	// AppointmentVersion is the appointment version a change notice describes: a newer notice for
	// the same appointment and channel supersedes it. 0 for reminders (never superseded by version).
	AppointmentVersion int
}

type Notification struct {
	NewNotification
	Attempts  int
	ClaimedAt time.Time
}

// Contact is what a notification needs about the appointment's customer and dealership.
type Contact struct {
	Email, Phone   string
	DealershipName string
	Timezone       string
}

func (s *Store) Contact(ctx context.Context, customerID, dealershipID uuid.UUID) (Contact, error) {
	var c Contact
	err := s.pool.QueryRow(ctx, `
		SELECT c.email, c.phone, d.name, d.timezone
		FROM customers c, dealerships d
		WHERE c.id = $1 AND d.id = $2`, customerID, dealershipID).Scan(&c.Email, &c.Phone, &c.DealershipName, &c.Timezone)
	return c, missing(err, "customer or dealership not found")
}

// EnqueueNotifications inserts rows in one statement, skipping any whose dedupe_key already exists
// (at-least-once producers). It returns how many were new.
func (s *Store) EnqueueNotifications(ctx context.Context, items []NewNotification) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	ids := make([]uuid.UUID, len(items))
	appointments := make([]uuid.UUID, len(items))
	kinds, channels := make([]int16, len(items)), make([]int16, len(items))
	recipients := make([]string, len(items))
	subjects, bodies, keys := make([]string, len(items)), make([]string, len(items)), make([]string, len(items))
	starts := make([]time.Time, len(items))
	versions := make([]int, len(items))
	for i, it := range items {
		if !it.Kind.Valid() || !it.Channel.Valid() {
			return 0, fmt.Errorf("store: notification %s has kind %d, channel %d", it.DedupeKey, it.Kind, it.Channel)
		}
		ids[i], appointments[i], kinds[i], channels[i] = it.ID, it.AppointmentID, int16(it.Kind), int16(it.Channel)
		recipients[i], subjects[i], bodies[i], keys[i] = it.Recipient, it.Subject, it.Body, it.DedupeKey
		starts[i], versions[i] = it.StartAt, it.AppointmentVersion
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO notifications
			(id, appointment_id, kind, channel, recipient, subject, body, dedupe_key, start_at, appointment_version)
		SELECT id, appointment_id, kind, channel, recipient, subject, body, dedupe_key, start_at, NULLIF(version, 0)
		FROM unnest($1::uuid[], $2::uuid[], $3::smallint[], $4::smallint[], $5::text[], $6::text[], $7::text[], $8::text[],
		            $9::timestamptz[], $10::int[])
		     AS u(id, appointment_id, kind, channel, recipient, subject, body, dedupe_key, start_at, version)
		-- One fixed order for every writer: every instance sweeps reminders, and a key that collides
		-- with another TX's uncommitted row waits for it, so two batches inserting the same keys in
		-- different orders would deadlock.
		ORDER BY appointment_id, kind, start_at, channel, dedupe_key
		ON CONFLICT DO NOTHING -- dedupe_key, or a second live reminder for one slot
		`,
		ids, appointments, kinds, channels, recipients, subjects, bodies, keys, starts, versions)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ReminderCandidate is a confirmed appointment due a reminder of some kind.
type ReminderCandidate struct {
	AppointmentID  uuid.UUID
	CustomerID     uuid.UUID
	DealershipID   uuid.UUID
	StartAt        time.Time
	ScheduledAt    time.Time
	VehicleLabel   string
	ServiceName    string
	CustomerName   string
	DealershipName string
	Timezone       string
	Email, Phone   string
}

// spaceChars is every rune unicode.IsSpace accepts (the highest is U+3000), so SQL
// btrim(x, spaceChars) is empty exactly when strings.TrimSpace(x) is.
var spaceChars = func() string {
	var b strings.Builder
	for r := rune(1); r <= 0x3000; r++ {
		if unicode.IsSpace(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}()

// ReminderCandidates lists confirmed appointments starting in (now, now+within] that were booked
// (or last moved) more than minLead before their start and have no kind reminder for that start
// yet. Comparing the slot (not timestamps) stays correct when a reschedule commits while the
// sweep runs. Customers without any contact are left out, so they are not re-selected forever;
// "without" uses the same whitespace rule as the producer (strings.TrimSpace, via spaceChars), or a
// contact of only tabs would pass here, be dropped there, and fill every sweep's batch.
func (s *Store) ReminderCandidates(ctx context.Context, kind domain.NotificationKind, now time.Time, within, minLead time.Duration, limit int) ([]ReminderCandidate, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.customer_id, a.dealership_id, a.start_at, a.scheduled_at,
		       v.year::text || ' ' || v.make || ' ' || v.model,
		       COALESCE((SELECT string_agg(al.service_name, ' + ' ORDER BY al.line_no)
		                 FROM appointment_lines al WHERE al.appointment_id = a.id), ''),
		       c.name, d.name, d.timezone, c.email, c.phone
		FROM appointments a
		JOIN customers c ON c.id = a.customer_id
		JOIN vehicles v ON v.id = a.vehicle_id
		JOIN dealerships d ON d.id = a.dealership_id
		WHERE a.status = `+statusConfirmedSQL+`
		  AND (btrim(c.email, $6) <> '' OR btrim(c.phone, $6) <> '')
		  AND a.start_at > $2::timestamptz
		  AND a.start_at <= $3::timestamptz
		  AND a.start_at - a.scheduled_at > make_interval(secs => $4::float8)
		  AND NOT EXISTS (
			SELECT 1 FROM notifications n
			WHERE n.appointment_id = a.id AND n.kind = $1 AND n.start_at = a.start_at
			  AND n.status <> `+notificationSkipped+`) -- skipped when the job left this slot; it may have come back
		ORDER BY a.start_at
		LIMIT $5`, int16(kind), now, now.Add(within), minLead.Seconds(), limit, spaceChars)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ReminderCandidate, error) {
		var c ReminderCandidate
		err := row.Scan(&c.AppointmentID, &c.CustomerID, &c.DealershipID, &c.StartAt, &c.ScheduledAt,
			&c.VehicleLabel, &c.ServiceName, &c.CustomerName, &c.DealershipName, &c.Timezone, &c.Email, &c.Phone)
		return c, err
	})
}

// staleNotificationSQL holds for a row n (joined to its appointment a) that must not go out:
//   - a reminder whose appointment is no longer confirmed at the slot it is about;
//   - a "rescheduled" notice once a newer notice exists for the same appointment and channel (its
//     time is out of date, and the newer one tells the customer where things stand);
//   - any change notice that a newer one has already overtaken (sending or sent): it arrived late
//     (redelivery, replay), and sending it now would put the customer's messages out of order;
//   - a "we missed you" notice once the appointment is no longer a no-show (staff reinstated the
//     customer who turned up late: the car is in the workshop).
//
// So a confirmation followed by a cancellation sends both, in order, while a burst of moves sends
// only the last. The newest change notice is never stale, so it is always delivered.
var staleNotificationSQL = `(
	(n.kind IN (` + kindRemindersSQL + `) AND (a.status <> ` + statusConfirmedSQL + ` OR a.start_at <> n.start_at))
	OR (n.kind = ` + kindNoShowSQL + ` AND a.status <> ` + statusNoShowSQL + `)
	OR (n.appointment_version IS NOT NULL AND EXISTS (
		SELECT 1 FROM notifications m
		WHERE m.appointment_id = n.appointment_id AND m.channel = n.channel
		  AND m.appointment_version > n.appointment_version
		  AND (n.kind = ` + kindRescheduledSQL + ` OR m.status IN (` + notificationSending + `, ` + notificationSent + `)))))`

// staleSkipWindow bounds the claim's bookkeeping skip to this many times the claim's limit of the
// oldest due pending rows.
const staleSkipWindow = 4

// staleReasonSQL is the last_error recorded for a row skipped by staleNotificationSQL.
var staleReasonSQL = `CASE WHEN n.kind IN (` + kindRemindersSQL + `) THEN 'appointment changed'
	WHEN n.kind = ` + kindNoShowSQL + ` AND a.status <> ` + statusNoShowSQL + ` THEN 'no longer a no-show'
	ELSE 'superseded by a newer notice' END`

// ClaimNotifications leases up to limit ready rows to this sender. Stale rows (staleNotificationSQL)
// are never claimed: the predicate is part of the claim itself, so a reschedule or a newer notice
// that committed before the claim always wins; the separate UPDATE only records them as skipped.
// A change notice also waits while an older one for the same appointment and channel is being
// sent, or is pending and still due to go out (anything but a superseded "rescheduled"), so notices
// go out in version order. The older one is never blocked in turn, so this cannot stall.
//
// The skip is bookkeeping, bounded so a large backlog cannot push the claim past its timeout: it
// looks only at the staleSkipWindow*limit oldest due pending rows (notifications_ready_idx, no
// locks), marks at most limit stale ones among them, and passes over rows another sender holds
// (SKIP LOCKED) instead of waiting for them. A stale row further back is left pending — never
// claimed, since the claim checks staleness itself — until the rows ahead of it are sent or
// skipped and the window reaches it; a stale row backing off is skipped once it is due.
//
// The newer notices queued behind a head are blocked by the claim's ordering check whatever their
// next_attempt_at says, so for them next_attempt_at is only a cost hint that keeps them out of the
// scan (notifications_ready_idx) — the outbox's trick (see shiftedSQL there). During a sender
// outage a head backs off for up to 10 minutes, and without it every poll of every instance would
// walk all the blocked rows (and their correlated probes) before reaching claimable ones. Three
// rules keep the hint from delaying a notice past its predecessor (notifyShiftedSQL):
//   - the claim holds back, for NotificationInflightHold, the never-claimed newer notices of each
//     head it claims (they wait while it is sending);
//   - a head that backs off pushes them up to its own next_attempt_at (never down): it blocks them
//     until it resolves, and it cannot resolve before it is claimed again at that time;
//   - a head that resolves (sent, failed, skipped before sending) releases them to now.
//
// Only never-claimed rows (attempts = 0) are moved, so no row's own backoff is ever changed. A
// notice enqueued while its head backs off stays in the scan until the head's next claim holds it.
func (s *Store) ClaimNotifications(ctx context.Context, limit int) ([]Notification, error) {
	if _, err := s.pool.Exec(ctx, `
		WITH window_rows AS (
			SELECT id FROM notifications
			WHERE status = `+notificationPending+` AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $2
		), stale AS (
			SELECT n.id, `+staleReasonSQL+` AS reason
			FROM window_rows w
			JOIN notifications n ON n.id = w.id
			JOIN appointments a ON a.id = n.appointment_id
			WHERE n.status = `+notificationPending+` AND n.next_attempt_at <= now() AND `+staleNotificationSQL+`
			ORDER BY n.next_attempt_at
			FOR UPDATE OF n SKIP LOCKED
			LIMIT $1
		)
		UPDATE notifications n SET status = `+notificationSkipped+`, last_error = stale.reason
		FROM stale WHERE n.id = stale.id`, limit, staleSkipWindow*limit); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		WITH cte AS (
			SELECT n.id FROM notifications n
			JOIN appointments a ON a.id = n.appointment_id
			WHERE n.status = `+notificationPending+` AND n.next_attempt_at <= now()
			  AND NOT `+staleNotificationSQL+`
			  AND NOT EXISTS (
				SELECT 1 FROM notifications m
				WHERE m.appointment_id = n.appointment_id AND m.channel = n.channel
				  AND m.appointment_version < n.appointment_version
				  AND (m.status = `+notificationSending+` OR (m.status = `+notificationPending+` AND m.kind <> `+kindRescheduledSQL+`)))
			ORDER BY n.next_attempt_at
			FOR UPDATE OF n SKIP LOCKED
			LIMIT $1
		), claimed AS (
			UPDATE notifications n
			SET status = `+notificationSending+`, attempts = n.attempts + 1, claimed_at = now()
			FROM cte WHERE n.id = cte.id
			RETURNING n.id, n.appointment_id, n.kind, n.channel, n.recipient, n.subject, n.body, n.dedupe_key,
			          n.start_at, n.appointment_version, n.attempts, n.claimed_at
		), held AS (
			-- The newer change notices of a claimed head wait for it (it is sending now): keep them
			-- out of the claim's scan while it is in flight (see NotificationInflightHold).
			SELECT l.id
			FROM claimed c
			JOIN notifications l ON l.appointment_id = c.appointment_id AND l.channel = c.channel
			                    AND l.appointment_version > c.appointment_version
			WHERE l.status = `+notificationPending+` AND l.attempts = 0 AND l.next_attempt_at <= now()
			LIMIT $3
			FOR UPDATE OF l SKIP LOCKED
		), hold AS (
			UPDATE notifications o SET next_attempt_at = now() + make_interval(secs => $2::float8)
			FROM held WHERE o.id = held.id
		)
		SELECT id, appointment_id, kind, channel, recipient, subject, body, dedupe_key,
		       start_at, COALESCE(appointment_version, 0), attempts, claimed_at
		FROM claimed`, limit, NotificationInflightHold.Seconds(), notificationHoldLimit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) {
		var n Notification
		var kind, channel int16
		if err := row.Scan(&n.ID, &n.AppointmentID, &kind, &channel, &n.Recipient, &n.Subject, &n.Body,
			&n.DedupeKey, &n.StartAt, &n.AppointmentVersion, &n.Attempts, &n.ClaimedAt); err != nil {
			return n, err
		}
		n.Kind, n.Channel = domain.NotificationKind(kind), domain.NotificationChannel(channel)
		return n, nil
	})
}

// NotificationInflightHold is how long a claim holds back the newer notices queued behind each head
// it claims (about the maintenance lease: a crashed sender's head is reclaimed around then).
const NotificationInflightHold = 30 * time.Second

// notificationHoldLimit caps the rows one claim holds back (an appointment has a few notices).
const notificationHoldLimit = 2000

// notifyReleaseSQL lists, for every row of CTE "done" (appointment_id, channel, appointment_version)
// that resolved, the newer never-claimed notices still pushed into the future, with until = now().
var notifyReleaseSQL = `
		SELECT l.id, now() AS until
		FROM done d
		JOIN notifications l ON l.appointment_id = d.appointment_id AND l.channel = d.channel
		                    AND l.appointment_version > d.appointment_version
		WHERE l.status = ` + notificationPending + ` AND l.attempts = 0 AND l.next_attempt_at > now()`

// notifyShiftedSQL continues a WITH whose CTE "shift" lists (id, until): each such never-claimed
// pending row gets next_attempt_at = the largest until listed for it. Rows are locked in id order,
// so two marks never deadlock (each holds only its own sending rows before this, and nothing that
// holds a pending row waits for a sending one).
var notifyShiftedSQL = `, locked AS (
			SELECT o.id, s.until
			FROM notifications o
			JOIN (SELECT id, max(until) AS until FROM shift GROUP BY id) s ON s.id = o.id
			WHERE o.status = ` + notificationPending + ` AND o.attempts = 0
			ORDER BY o.id
			FOR UPDATE OF o
		), shifted AS (
			UPDATE notifications o SET next_attempt_at = k.until
			FROM locked k
			WHERE o.id = k.id AND o.status = ` + notificationPending + ` AND o.attempts = 0
		)`

// SkipStaleNotifications re-checks claimed rows right before they are sent and marks the ones that
// went stale since the claim (appointment moved or cancelled, newer notice enqueued) as skipped.
// It returns the skipped ids. Fenced by the lease like every other mark.
func (s *Store) SkipStaleNotifications(ctx context.Context, ids []uuid.UUID, claimedAt time.Time) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH done AS (
			UPDATE notifications n SET status = `+notificationSkipped+`, claimed_at = NULL, last_error = `+staleReasonSQL+`
			FROM appointments a
			WHERE n.id = ANY($1) AND n.status = `+notificationSending+` AND n.claimed_at = $2
			  AND a.id = n.appointment_id AND `+staleNotificationSQL+`
			RETURNING n.id, n.appointment_id, n.channel, n.appointment_version
		), shift AS (`+notifyReleaseSQL+`
		)`+notifyShiftedSQL+`
		SELECT id FROM done`, ids, claimedAt)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// MarkNotificationsSent applies only while this sender's lease (claimedAt) is current, and releases
// the newer notices held back behind the sent ones (see ClaimNotifications).
func (s *Store) MarkNotificationsSent(ctx context.Context, ids []uuid.UUID, claimedAt time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		WITH done AS (
			UPDATE notifications SET status = `+notificationSent+`, sent_at = now(), claimed_at = NULL, last_error = NULL
			WHERE id = ANY($1) AND status = `+notificationSending+` AND claimed_at = $2
			RETURNING appointment_id, channel, appointment_version
		), shift AS (`+notifyReleaseSQL+`
		)`+notifyShiftedSQL+`
		SELECT 1`, ids, claimedAt)
	return err
}

// SendFailure is one row whose delivery failed, with the reason recorded on it.
type SendFailure struct {
	ID     uuid.UUID
	Reason string
}

// MarkNotificationsRetry backs off (2^attempts s, max 10 min; the exponent is capped so power()
// cannot overflow) or fails the rows that reached maxAttempts. Fenced by the lease (claimedAt). A
// backed-off change notice pushes the newer ones it blocks to its retry time; a failed one releases
// them (see ClaimNotifications).
func (s *Store) MarkNotificationsRetry(ctx context.Context, failures []SendFailure, claimedAt time.Time, maxAttempts int) error {
	if len(failures) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(failures))
	reasons := make([]string, len(failures))
	for i, f := range failures {
		ids[i], reasons[i] = f.ID, f.Reason
	}
	_, err := s.pool.Exec(ctx, `
		WITH done AS (
			UPDATE notifications n
			SET status = CASE WHEN n.attempts >= $4 THEN `+notificationFailed+` ELSE `+notificationPending+` END,
			    next_attempt_at = now() + make_interval(secs => LEAST(600, power(2, LEAST(n.attempts, 10)))::float8),
			    claimed_at = NULL,
			    last_error = left(f.reason, 500)
			FROM unnest($1::uuid[], $2::text[]) AS f(id, reason)
			WHERE n.id = f.id AND n.status = `+notificationSending+` AND n.claimed_at = $3
			RETURNING n.status, n.kind, n.appointment_id, n.channel, n.appointment_version, n.next_attempt_at
		), shift AS (
			-- A backed-off head blocks its newer notices until it is claimed again (a superseded
			-- "rescheduled" one blocks nothing): push them to that time, never earlier than now set.
			SELECT l.id, d.next_attempt_at AS until
			FROM done d
			JOIN notifications l ON l.appointment_id = d.appointment_id AND l.channel = d.channel
			                    AND l.appointment_version > d.appointment_version
			WHERE d.status = `+notificationPending+` AND d.kind <> `+kindRescheduledSQL+`
			  AND l.status = `+notificationPending+` AND l.attempts = 0 AND l.next_attempt_at < d.next_attempt_at
			UNION ALL`+notifyReleaseSQL+`
			  AND d.status = `+notificationFailed+`
		)`+notifyShiftedSQL+`
		SELECT 1`, ids, reasons, claimedAt, maxAttempts)
	return err
}

// PurgeNotifications deletes finished rows (sent, skipped, failed) created more than retention ago
// and about a slot more than retention in the past, at most limit per call. Retention must exceed
// the Kafka topic retention (dedupe_key is what makes an event replay safe) and the reminder
// horizon; failed rows age out too, so the poison alert only covers the retention window.
func (s *Store) PurgeNotifications(ctx context.Context, retention time.Duration, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM notifications
		WHERE id IN (
			SELECT id FROM notifications
			WHERE status IN (`+notificationSent+`, `+notificationSkipped+`, `+notificationFailed+`)
			  AND created_at < now() - make_interval(secs => $1::float8)
			  AND start_at < now() - make_interval(secs => $1::float8)
			LIMIT $2
			FOR UPDATE SKIP LOCKED -- another instance purging the same rows: skip, do not wait
		)`, retention.Seconds(), limit)
	return tag.RowsAffected(), err
}

// ReclaimStaleNotifications returns rows whose lease expired (sender crashed) to pending.
func (s *Store) ReclaimStaleNotifications(ctx context.Context, lease time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE notifications SET status = `+notificationPending+`, claimed_at = NULL
		WHERE status = `+notificationSending+` AND claimed_at < now() - make_interval(secs => $1::float8)`, lease.Seconds())
	return tag.RowsAffected(), err
}

// QueueDepth counts rows per queue and status (metrics: backlog and poison alerts).
type QueueDepth struct {
	Queue  string // outbox | notifications | workshop
	Status string
	Count  int
}

// QueueDepthCap caps each QueueDepths count: the metric saturates there (far above every alert
// threshold) instead of costing a scrape O(backlog) index entries during a long broker outage.
const QueueDepthCap = 100000

// StaleOverrunAge is how long after its start a flagged overrun counts as forgotten: it holds its
// bay and technician until someone completes it (overrunningSQL), so one left open blocks them on
// every later day; the workshop/stale_overrun depth and its alert surface it.
const StaleOverrunAge = 12 * time.Hour

// QueueDepths counts each status separately so every count is served by its own partial index
// (no sequential scan of the finished history), each at most QueueDepthCap; statuses with no rows
// report 0. The workshop queue counts flagged overruns older than StaleOverrunAge (the on-site
// index), work only a person can close.
func (s *Store) QueueDepths(ctx context.Context) ([]QueueDepth, error) {
	// The status code is a literal in the text, so each count matches its partial index; the name
	// is what the metric label carries.
	count := func(queue, table, code, status string) string {
		return `SELECT '` + queue + `', '` + status + `', count(*)::int FROM (SELECT 1 FROM ` + table +
			` WHERE status = ` + code + ` LIMIT $1) x`
	}
	rows, err := s.pool.Query(ctx, strings.Join([]string{
		count("outbox", "outbox_events", outboxPending, "pending"),
		count("outbox", "outbox_events", outboxProcessing, "processing"),
		count("outbox", "outbox_events", outboxFailed, "failed"),
		count("notifications", "notifications", notificationPending, "pending"),
		count("notifications", "notifications", notificationSending, "sending"),
		count("notifications", "notifications", notificationFailed, "failed"),
		`SELECT 'workshop', 'stale_overrun', count(*)::int FROM (SELECT 1 FROM appointments a
			WHERE ` + overrunningSQL("a") + ` AND a.start_at < now() - make_interval(secs => $2::float8) LIMIT $1) x`,
	}, "\n\t\tUNION ALL "), QueueDepthCap, StaleOverrunAge.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (QueueDepth, error) {
		var q QueueDepth
		err := row.Scan(&q.Queue, &q.Status, &q.Count)
		return q, err
	})
}
