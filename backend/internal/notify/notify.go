// Package notify sends customers their booking confirmations, change notices, and reminders.
//
// Producers only enqueue rows (idempotently, by dedupe key) into the notifications table:
//   - HandleEvent consumes appointment events from Kafka in one consumer group shared by every
//     instance (each event is handled once; redelivery is deduped on the outbox event id);
//   - EnqueueReminders is run by the maintenance loop on every instance (deduped per slot).
//
// Run then claims rows with a lease (SKIP LOCKED) and hands them to a Sender. Rows that went stale
// are skipped, both at claim time and again right before sending: reminders whose appointment was
// cancelled or moved after they were enqueued; "rescheduled" notices superseded by a newer notice;
// and any notice a newer one already overtook (Kafka redelivery or a retry must never send "moved
// to 10:00" after "moved to 11:00"). Notices for one appointment go out in version order, so a
// confirmation and a quick cancellation both arrive, in that order. The newest always goes out.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Timing of one Flush (see Config.Validate). Every phase is bounded, so a batch is done with its
// rows well inside the maintenance lease, which a crashed sender relies on.
const (
	// claimTimeout bounds the claim statement; it runs inside SendTimeout (which starts before it).
	claimTimeout = 2 * time.Second
	// staleCheckTimeout bounds the pre-send stale re-check; also inside SendTimeout.
	staleCheckTimeout = 2 * time.Second
	// bookkeepingTimeout bounds recording send outcomes once the sends are done.
	bookkeepingTimeout = 5 * time.Second
	// leaseMargin is slack for clock drift between this process and Postgres (the lease runs from
	// the database's claimed_at, the send deadline from this process's clock before the claim).
	leaseMargin = 2 * time.Second
)

// eventKinds maps the appointment events customers are told about.
var eventKinds = map[domain.EventType]domain.NotificationKind{
	domain.EventAppointmentCreated:     domain.KindConfirmation,
	domain.EventAppointmentRescheduled: domain.KindRescheduled,
	domain.EventAppointmentCancelled:   domain.KindCancelled,
	domain.EventAppointmentNoShow:      domain.KindNoShow,
}

// reminder is one reminder rule: sent when the start is within Within, and only if the slot was
// booked more than MinLead before it (no "see you tomorrow" right after booking for tomorrow).
type reminder struct {
	Kind    domain.NotificationKind
	Within  time.Duration
	MinLead time.Duration
}

var reminders = []reminder{
	{Kind: domain.KindReminder24h, Within: 24 * time.Hour, MinLead: 26 * time.Hour},
	{Kind: domain.KindReminder2h, Within: 2 * time.Hour, MinLead: 4 * time.Hour},
}

type Config struct {
	BatchSize    int
	PollInterval time.Duration
	MaxAttempts  int // a row that failed this many sends becomes failed (alerted on)
	// SendTimeout is the deadline for one batch's claim, stale re-check and sends, measured from
	// just before the claim (so from no later than the lease's claimed_at). The default leaves the
	// sends about 10 s once the claim and the re-check (2 s each at most) have run.
	SendTimeout time.Duration
}

func DefaultConfig() Config {
	return Config{BatchSize: 50, PollInterval: 2 * time.Second, MaxAttempts: 8, SendTimeout: 13 * time.Second}
}

// Validate checks the timing against the maintenance lease (ReclaimStaleNotifications): a row must
// never be reclaimed — and so sent by another instance — while this one may still send it or
// record its outcome:
//   - SendTimeout > claimTimeout + staleCheckTimeout: the sends get some of the deadline;
//   - SendTimeout + bookkeepingTimeout + margin <= lease: outcomes land before the lease ends.
func (c Config) Validate(lease time.Duration) error {
	if c.BatchSize <= 0 || c.PollInterval <= 0 || c.MaxAttempts <= 0 {
		return errors.New("notify: batch size, poll interval and max attempts must be positive")
	}
	if c.SendTimeout <= claimTimeout+staleCheckTimeout {
		return fmt.Errorf("notify: send timeout %s leaves no time to send after the claim (%s) and stale check (%s)",
			c.SendTimeout, claimTimeout, staleCheckTimeout)
	}
	if c.SendTimeout+bookkeepingTimeout+leaseMargin > lease {
		return fmt.Errorf("notify: send timeout %s + bookkeeping %s + %s margin exceeds the lease %s",
			c.SendTimeout, bookkeepingTimeout, leaseMargin, lease)
	}
	return nil
}

// DrainTimeout is the longest one in-flight Flush can still run after shutdown cancels its context:
// the send deadline (measured from just before the claim) plus the bookkeeping after the sends.
// Shutdown should wait at least this long, or rows stay sending until the lease ends and a message
// the provider accepted is sent again.
func (c Config) DrainTimeout() time.Duration {
	return c.SendTimeout + bookkeepingTimeout
}

// Sender delivers one message (SMTP, SMS gateway, webhook…).
type Sender interface {
	Send(ctx context.Context, n store.Notification) error
}

type Service struct {
	store  *store.Store
	sender Sender
	log    *slog.Logger
	cfg    Config
	now    func() time.Time
	// locations caches time.LoadLocation by IANA name (string -> *time.Location): loading reads
	// and parses the zoneinfo file, once per event otherwise. Dealerships use a handful of zones,
	// and names that fail to load are cached as UTC, so the map stays small.
	locations sync.Map
}

// location returns the named zone, or UTC when it cannot be loaded (logged once per name).
func (s *Service) location(name string) *time.Location {
	if loc, ok := s.locations.Load(name); ok {
		return loc.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		s.log.Warn("notify: unknown dealership timezone; using UTC", "timezone", name, "err", err)
		loc = time.UTC
	}
	actual, _ := s.locations.LoadOrStore(name, loc)
	return actual.(*time.Location)
}

func New(st *store.Store, sender Sender, log *slog.Logger, cfg Config) *Service {
	return &Service{store: st, sender: sender, log: log, cfg: cfg, now: time.Now}
}

// WithClock overrides "now" (tests).
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// HandleEvent is the Kafka handler: enqueue the customer message for an appointment event.
func (s *Service) HandleEvent(ctx context.Context, ev realtime.Event) error {
	kind, ok := eventKinds[ev.Type]
	if ev.Kind != realtime.KindAppointment || !ok {
		return nil
	}
	var a domain.Appointment
	if err := json.Unmarshal(ev.Payload, &a); err != nil {
		s.log.Error("notify: bad appointment payload", "err", err, "event", ev.ID)
		return nil // poison: never block the partition
	}
	contact, err := s.store.Contact(ctx, a.CustomerID, a.DealershipID)
	if err != nil {
		if domainErr, ok := domain.AsError(err); ok && domainErr.Code == domain.CodeNotFound {
			return nil
		}
		return err
	}
	loc := s.location(contact.Timezone)
	msg := message{
		kind: kind, appointmentID: a.ID, version: a.Version, start: a.StartAt.In(loc), dealer: contact.DealershipName,
		vehicle: a.VehicleLabel, service: a.ServiceName, customer: a.CustomerName,
	}
	eventKey := ev.ID.String()
	if ev.ID == uuid.Nil {
		eventKey = fmt.Sprintf("%s|%s|v%d", ev.Type, a.ID, a.Version)
	}
	_, err = s.store.EnqueueNotifications(ctx, msg.rows(contact.Email, contact.Phone, eventKey))
	return err
}

// reminderDedupe keys a reminder by slot and by when the appointment was booked or last moved
// there (scheduled_at, which only booking and reschedule set — not reassign or other edits): a
// retry, or two instances sweeping at once, never double-sends, yet an appointment moved
// A -> B -> A gets a fresh reminder for A instead of colliding with the one skipped when it left A.
// notifications_reminder_slot_uniq backs this in the database (one live reminder per slot).
func reminderDedupe(kind domain.NotificationKind, appointmentID uuid.UUID, start, scheduledAt time.Time) string {
	return fmt.Sprintf("%s|%s|%d|%d", kind, appointmentID, start.Unix(), scheduledAt.UnixMicro())
}

// EnqueueReminders adds due 24h and 2h reminders for confirmed appointments, one insert per rule
// (duplicates are dropped by the insert itself, so a concurrent sweep or a retry is harmless).
func (s *Service) EnqueueReminders(ctx context.Context) (int, error) {
	now := s.now()
	total := 0
	for _, r := range reminders {
		due, err := s.store.ReminderCandidates(ctx, r.Kind, now, r.Within, r.MinLead, 200)
		if err != nil {
			return total, err
		}
		rows := make([]store.NewNotification, 0, len(due))
		for _, c := range due {
			msg := message{
				kind: r.Kind, appointmentID: c.AppointmentID, start: c.StartAt.In(s.location(c.Timezone)), dealer: c.DealershipName,
				vehicle: c.VehicleLabel, service: c.ServiceName, customer: c.CustomerName,
			}
			rows = append(rows, msg.rows(c.Email, c.Phone, reminderDedupe(r.Kind, c.AppointmentID, c.StartAt, c.ScheduledAt))...)
		}
		n, err := s.store.EnqueueNotifications(ctx, rows)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Run delivers queued notifications until ctx ends.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		for ctx.Err() == nil {
			n, err := s.Flush(ctx)
			if err != nil {
				s.log.Error("notify flush", "err", err)
				break
			}
			if n < s.cfg.BatchSize {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Flush claims one batch, drops rows that went stale since the claim, sends the rest concurrently,
// and records the outcomes in one UPDATE per outcome. It returns how many rows were claimed.
func (s *Service) Flush(ctx context.Context) (int, error) {
	// The lease runs from claimed_at (the database's now() at the claim); the send deadline is taken
	// before the claim is sent, so it never ends later than SendTimeout into the lease.
	start := time.Now()
	sendDeadline := start.Add(s.cfg.SendTimeout)
	claimCtx, cancelClaim := context.WithTimeout(ctx, claimTimeout)
	batch, err := s.store.ClaimNotifications(claimCtx, s.cfg.BatchSize)
	cancelClaim()
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	claimedAt := batch[0].ClaimedAt // one UPDATE claims the batch, so all rows share it
	// From here on shutdown no longer interrupts the batch: every phase has its own deadline.
	detached := context.WithoutCancel(ctx)

	ids := make([]uuid.UUID, len(batch))
	for i, n := range batch {
		ids[i] = n.ID
	}
	// Detached: rows already leased would otherwise wait out the lease on shutdown.
	staleCtx, cancelStale := context.WithTimeout(detached, staleCheckTimeout)
	skipped, err := s.store.SkipStaleNotifications(staleCtx, ids, claimedAt)
	cancelStale()
	if err != nil {
		// Leave the rows leased: reclaim returns them to pending once the lease expires.
		return len(batch), err
	}
	drop := make(map[uuid.UUID]bool, len(skipped))
	for _, id := range skipped {
		drop[id] = true
	}

	var (
		mu       sync.Mutex
		sent     []uuid.UUID
		failures []store.SendFailure
		wg       sync.WaitGroup
	)
	for _, n := range batch {
		if drop[n.ID] {
			continue
		}
		wg.Go(func() {
			// A send already under way finishes (bounded by the batch's send deadline) even on
			// shutdown: aborting it would count a failed attempt for a message the provider may
			// have accepted, and the retry would deliver it twice. Validate keeps it in the lease.
			sendCtx, cancelSend := context.WithDeadline(detached, sendDeadline)
			err := s.sender.Send(sendCtx, n)
			cancelSend()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				s.log.Warn("notify send failed", "err", err, "id", n.ID, "kind", n.Kind, "attempt", n.Attempts)
				failures = append(failures, store.SendFailure{ID: n.ID, Reason: err.Error()})
				return
			}
			sent = append(sent, n.ID)
		})
	}
	wg.Wait()
	// Bookkeeping must land even if ctx was cancelled mid-send, or rows wait for lease expiry and
	// are sent again. Its deadline starts only now: a slow send must not use it up.
	bk, cancel := context.WithTimeout(detached, bookkeepingTimeout)
	defer cancel()
	if err := s.store.MarkNotificationsSent(bk, sent, claimedAt); err != nil {
		s.log.Error("notify mark sent", "err", err, "rows", len(sent))
	}
	if err := s.store.MarkNotificationsRetry(bk, failures, claimedAt, s.cfg.MaxAttempts); err != nil {
		s.log.Error("notify mark retry", "err", err, "rows", len(failures))
	}
	return len(batch), nil
}

// message renders one notification for every channel it goes to.
type message struct {
	kind          domain.NotificationKind
	appointmentID uuid.UUID
	version       int       // appointment version a change notice describes; 0 for reminders
	start         time.Time // dealership local time
	dealer        string
	vehicle       string
	service       string
	customer      string
}

func (m message) text() (subject, body string) {
	when := m.start.Format("Mon 2 Jan 2006 at 15:04")
	what := fmt.Sprintf("%s for your %s at %s on %s", m.service, m.vehicle, m.dealer, when)
	greeting := "Hello"
	if first := strings.Fields(m.customer); len(first) > 0 { // blank names (only spaces) get "Hello"
		greeting = "Hello " + first[0]
	}
	switch m.kind {
	case domain.KindRescheduled:
		return "Your service appointment has moved", fmt.Sprintf("%s, your appointment is now: %s.", greeting, what)
	case domain.KindCancelled:
		return "Your service appointment is cancelled", fmt.Sprintf("%s, your appointment (%s) has been cancelled.", greeting, what)
	case domain.KindNoShow:
		return "We missed you today", fmt.Sprintf("%s, we did not see you for %s. Reply or call us to rebook.", greeting, what)
	case domain.KindReminder24h, domain.KindReminder2h:
		return "Reminder: your service appointment", fmt.Sprintf("%s, a reminder of your appointment: %s.", greeting, what)
	default:
		return "Your service appointment is confirmed", fmt.Sprintf("%s, you are booked in: %s.", greeting, what)
	}
}

// rows picks the channel: reminders prefer SMS (read within minutes), everything else email; each
// falls back to the other contact when the preferred one is missing.
func (m message) rows(email, phone, dedupe string) []store.NewNotification {
	channel, recipient := domain.ChannelEmail, email
	if m.kind.IsReminder() {
		channel, recipient = domain.ChannelSMS, phone
	}
	if strings.TrimSpace(recipient) == "" {
		if channel == domain.ChannelEmail {
			channel, recipient = domain.ChannelSMS, phone
		} else {
			channel, recipient = domain.ChannelEmail, email
		}
	}
	if strings.TrimSpace(recipient) == "" {
		return nil
	}
	subject, body := m.text()
	return []store.NewNotification{{
		ID: uuid.New(), AppointmentID: m.appointmentID, Kind: m.kind, Channel: channel,
		Recipient: recipient, Subject: subject, Body: body, DedupeKey: dedupe + "|" + channel.Name(), StartAt: m.start,
		AppointmentVersion: m.version,
	}}
}
