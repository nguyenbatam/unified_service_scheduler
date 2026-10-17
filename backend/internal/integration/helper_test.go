package integration_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// londonDay is the suite's London date + offset days (see suiteToday).
func londonDay(t testing.TB, offset int) booking.Date {
	t.Helper()
	day, loc := suiteToday()
	return booking.DateFrom(day.Start(loc).AddDate(0, 0, offset), loc)
}

// londonOn is hh:mm London on day.
func londonOn(t testing.TB, day booking.Date, hour, minute int) time.Time {
	t.Helper()
	_, loc := suiteToday()
	return booking.Clock{Hour: hour, Minute: minute}.On(day, loc)
}

// londonAt is hh:mm London on the suite's date: londonOn(t, londonDay(t, 0), …).
func londonAt(t *testing.T, hour, minute int) time.Time {
	t.Helper()
	return londonOn(t, londonDay(t, 0), hour, minute)
}

// serviceOn returns a service whose clock reads hh:mm London on day.
func serviceOn(t *testing.T, st *store.Store, day booking.Date, hour, minute int) *booking.Service {
	t.Helper()
	at := londonOn(t, day, hour, minute)
	return booking.New(st, nil, booking.WithClock(func() time.Time { return at }))
}

func expectCode(t *testing.T, err error, code string) {
	t.Helper()
	domainErr, ok := domain.AsError(err)
	if !ok || domainErr.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func scalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

var (
	staffAdmin      = domain.Actor{UserID: seed.DemoAdminID, Role: domain.RoleAdmin}
	staffDispatcher = domain.Actor{UserID: seed.DemoDispatcherID, Role: domain.RoleDispatcher}
	customerAmelia  = domain.Actor{UserID: seed.DemoCustomerID, Role: domain.RoleCustomer, CustomerID: seed.CustomerA}
)

// atStartOfLondonDay pins the service clock to 00:00 London today so fixed-hour test bookings
// "today" are never rejected as past, whatever time the suite runs.
func atStartOfLondonDay(st *store.Store) *booking.Service {
	return booking.New(st, nil, booking.WithClock(startOfLondonDay))
}

// startOfLondonDay is 00:00 London today.
func startOfLondonDay() time.Time {
	day, loc := suiteToday()
	return day.Start(loc)
}

// suiteToday is the London date when the suite started, read from the wall clock once: a test
// that runs across midnight must not book "today 14:00" on one date and check it on the next.
var suiteToday = sync.OnceValues(func() (booking.Date, *time.Location) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		panic(err) // time/tzdata is embedded, so this cannot happen
	}
	return booking.Today(loc, time.Now()), loc
})

// race runs every fn in its own goroutine and releases them together: each goroutine is parked on
// a closed-channel barrier before any of them starts, so the calls really overlap.
func race(fns ...func()) {
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for _, fn := range fns {
		ready.Add(1)
		done.Go(func() {
			ready.Done()
			<-start
			fn()
		})
	}
	ready.Wait()
	close(start)
	done.Wait()
}

// raceN runs fn(0) … fn(n-1) concurrently behind one start barrier (see race).
func raceN(n int, fn func(i int)) {
	fns := make([]func(), n)
	for i := range n {
		fns[i] = func() { fn(i) }
	}
	race(fns...)
}

// isCleanLoss reports whether err is a refusal a racing loser may get: 409 conflict, 412 stale
// If-Match, or 422 unavailable. Anything else (500, 503 busy, a raw pg error) is a bug in a race test.
func isCleanLoss(err error) bool {
	e, ok := domain.AsError(err)
	return ok && (e.Code == domain.CodeConflict || e.Code == domain.CodeUnavailable || e.Code == domain.CodePrecondition)
}

// contender is one side of a race: its result and the codes it may lose with.
type contender struct {
	err   error
	codes []string // empty: any clean loss (isCleanLoss)
}

// loses is a contender whose err must be nil (it won) or one of codes (any clean loss if none).
func loses(err error, codes ...string) contender { return contender{err: err, codes: codes} }

// allLose is loses for every error of a raceN.
func allLose(errs []error, codes ...string) []contender {
	cs := make([]contender, len(errs))
	for i, err := range errs {
		cs[i] = loses(err, codes...)
	}
	return cs
}

func (c contender) lostCleanly() bool {
	if len(c.codes) == 0 {
		return isCleanLoss(c.err)
	}
	e, ok := domain.AsError(c.err)
	return ok && slices.Contains(c.codes, e.Code)
}

// refusals are the codes of a loser that did not use If-Match: no 412 is possible.
var refusals = []string{domain.CodeConflict, domain.CodeUnavailable}

// expectWinners fails t unless exactly want contenders succeeded and every other lost with one of
// its codes; it returns the winners' indexes. It is the one winner check of this package: callers
// still recount the outcome in Postgres (assertNoOverlaps, assertEventsFor, …).
func expectWinners(t *testing.T, label string, want int, cs ...contender) []int {
	t.Helper()
	var won []int
	errs := make([]error, len(cs))
	for i, c := range cs {
		errs[i] = c.err
		if c.err == nil {
			won = append(won, i)
			continue
		}
		if !c.lostCleanly() {
			t.Fatalf("%s: contender %d lost with %v, want a clean loss %v", label, i, c.err, c.codes)
		}
	}
	if len(won) != want {
		t.Fatalf("%s: %d winners %v, want %d (errors %v)", label, len(won), won, want, errs)
	}
	return won
}

// oneWinner is expectWinners for exactly one winner; it returns the winner's index.
func oneWinner(t *testing.T, label string, cs ...contender) int {
	t.Helper()
	return expectWinners(t, label, 1, cs...)[0]
}

// assertNoOverlaps recounts holds in Postgres (not the callers' return codes): no two holding
// appointments (status 1–3) may overlap on the same bay, technician, or vehicle.
func assertNoOverlaps(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT a.id::text || ' vs ' || b.id::text || ' on ' ||
			concat_ws(',',
				CASE WHEN a.service_bay_id = b.service_bay_id THEN 'bay' END,
				CASE WHEN a.technician_id = b.technician_id THEN 'technician' END,
				CASE WHEN a.vehicle_id = b.vehicle_id THEN 'vehicle' END)
		FROM appointments a
		JOIN appointments b ON a.id < b.id
			AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange(b.start_at, b.end_at, '[)')
			AND (a.service_bay_id = b.service_bay_id OR a.technician_id = b.technician_id OR a.vehicle_id = b.vehicle_id)
		WHERE a.status IN (`+holdingCodes+`) AND b.status IN (`+holdingCodes+`)
		LIMIT 10`)
	if err != nil {
		t.Fatalf("overlap query: %v", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		found = append(found, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("double-booked holds:\n%s", strings.Join(found, "\n"))
	}
}

// allocationViolationsSQL finds holding appointments (status 1–3) that break the allocation rules
// (invariants 1, 10–12), re-stated here in SQL rather than read from the engine:
//   - the technician and bay belong to the appointment's dealership;
//   - the technician holds every required skill at >= min_skill_level;
//   - the bay fits: the required type, or express for a waiter's general job no longer than the
//     dealership's express_max_minutes (soft upgrade), or general for an express job (spill);
//   - the window lies inside the technician's duty (no absence; no shift pattern = opening hours,
//     else that weekday's shift, outside its break) and that local day's calendar (no closure,
//     the weekday override's hours and lunch, else the dealership's defaults) — compared as local
//     wall-clock dates and times, never instants.
//
// The duty and calendar part only covers windows a booking or reschedule placed: rows not yet
// started (status 1|2) whose history has no extend or reinstate (both re-check only the part of
// the window still ahead of now), and for duty no reassign either (same rule for its technician).
// Started work is out: an early start pulls the window to now and the overrun sweep extends it,
// neither re-checking duty or hours, on purpose.
var allocationViolationsSQL = `
	WITH holds AS (
		SELECT a.*, d.timezone, d.express_max_minutes,
			t.dealership_id AS tech_dealer, t.skill_level, b.dealership_id AS bay_dealer, b.bay_type,
			(a.start_at AT TIME ZONE d.timezone) AS ls, (a.end_at AT TIME ZONE d.timezone) AS le,
			a.status IN (` + notStartedCodes + `) AND NOT EXISTS (
				SELECT 1 FROM appointment_history h WHERE h.appointment_id = a.id
				  AND h.event_type IN (` + code(domain.EventAppointmentExtended) + `, ` + code(domain.EventAppointmentReinstated) + `)) AS placed,
			EXISTS (SELECT 1 FROM appointment_history h WHERE h.appointment_id = a.id
				  AND h.event_type = ` + code(domain.EventAppointmentReassigned) + `) AS reassigned
		FROM appointments a
		JOIN dealerships d ON d.id = a.dealership_id
		JOIN technicians t ON t.id = a.technician_id
		JOIN service_bays b ON b.id = a.service_bay_id
		WHERE a.status IN (` + holdingCodes + `)
	), days AS (
		SELECT h.*,
			EXISTS (SELECT 1 FROM dealership_closures c WHERE c.dealership_id = h.dealership_id AND c.day = h.ls::date) AS closure,
			COALESCE(o.closed, false) AS weekday_closed,
			CASE WHEN o.weekday IS NULL THEN dd.open_time ELSE o.open_time END AS open_t,
			CASE WHEN o.weekday IS NULL THEN dd.close_time ELSE o.close_time END AS close_t,
			CASE WHEN o.weekday IS NULL THEN dd.lunch_start ELSE o.lunch_start END AS lunch_s,
			CASE WHEN o.weekday IS NULL THEN dd.lunch_end ELSE o.lunch_end END AS lunch_e
		FROM holds h
		JOIN dealerships dd ON dd.id = h.dealership_id
		LEFT JOIN dealership_hours o ON o.dealership_id = h.dealership_id AND o.weekday = extract(isodow FROM h.ls)::int
	)
	SELECT id::text || ': ' || problems FROM (
	SELECT id, concat_ws(', ',
		CASE WHEN tech_dealer <> dealership_id THEN 'technician of another dealership' END,
		CASE WHEN bay_dealer <> dealership_id THEN 'bay of another dealership' END,
		CASE WHEN skill_level < min_skill_level THEN 'technician level ' || skill_level || ' < ' || min_skill_level END,
		CASE WHEN EXISTS (SELECT unnest(required_skills) EXCEPT SELECT sk.skill FROM technician_skills sk WHERE sk.technician_id = days.technician_id)
			THEN 'technician lacks a skill of ' || required_skills::text END,
		CASE WHEN NOT (bay_type = required_bay_type
				OR (bay_type = ` + code(domain.BayExpress) + ` AND required_bay_type = ` + code(domain.BayGeneral) + ` AND is_waiter AND booked_duration_minutes <= express_max_minutes)
				OR (bay_type = ` + code(domain.BayGeneral) + ` AND required_bay_type = ` + code(domain.BayExpress) + `))
			THEN 'bay type ' || bay_type || ' for required ' || required_bay_type || CASE WHEN is_waiter THEN ' (waiter)' ELSE '' END END,
		CASE WHEN placed AND (closure OR weekday_closed) THEN 'booked on a closed day ' || ls::date END,
		CASE WHEN placed AND NOT closure AND NOT weekday_closed
				AND (ls::date <> le::date OR ls::time < open_t OR le::time > close_t)
			THEN 'outside opening hours ' || open_t || '–' || close_t || ': ' || ls || ' – ' || le END,
		CASE WHEN placed AND lunch_s IS NOT NULL AND ls::time < lunch_e AND lunch_s < le::time
			THEN 'across lunch ' || lunch_s || '–' || lunch_e END,
		CASE WHEN placed AND NOT reassigned AND (
				EXISTS (SELECT 1 FROM technician_absences ab
					WHERE ab.technician_id = days.technician_id AND ab.starts_at < days.end_at AND days.start_at < ab.ends_at)
				OR (EXISTS (SELECT 1 FROM technician_shifts p WHERE p.technician_id = days.technician_id)
					AND NOT EXISTS (SELECT 1 FROM technician_shifts p
						WHERE p.technician_id = days.technician_id AND p.weekday = extract(isodow FROM ls)::int
						  AND ls::date = le::date AND ls::time >= p.start_time AND le::time <= p.end_time
						  AND NOT (p.break_start IS NOT NULL AND ls::time < p.break_end AND p.break_start < le::time))))
			THEN 'technician off duty' END) AS problems
	FROM days) v
	WHERE problems <> ''
	LIMIT 10`

// assertAllocationsValid recounts in Postgres that every holding appointment went to a qualified,
// on-duty technician and a fitting bay of its dealership, inside its day's calendar
// (allocationViolationsSQL). Where assertNoOverlaps proves no resource is held twice, this proves
// each winner of a race got resources it was allowed to have.
func assertAllocationsValid(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if found := allocationViolations(t, pool); len(found) > 0 {
		t.Fatalf("invalid allocations:\n%s", strings.Join(found, "\n"))
	}
}

// allocationViolations runs allocationViolationsSQL on q (a pool, or a TX holding fixtures).
func allocationViolations(t *testing.T, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), allocationViolationsSQL)
	if err != nil {
		t.Fatalf("allocation validity query: %v", err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("allocation validity query: %v", err)
	}
	return found
}

// assertStagingWithinCapacity recounts arrivals per staging window (local time, holding statuses)
// for every dealership with a staging cap: none may exceed staging_capacity.
func assertStagingWithinCapacity(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		WITH arrivals AS (
			SELECT d.id, d.staging_capacity, d.staging_window_minutes AS w,
				a.start_at AT TIME ZONE d.timezone AS local
			FROM appointments a JOIN dealerships d ON d.id = a.dealership_id
			WHERE a.status IN (`+holdingCodes+`) AND d.staging_capacity > 0
		)
		SELECT id::text || ' window ' ||
			(date_trunc('hour', local) + floor(extract(minute FROM local) / w) * w * interval '1 minute')::text ||
			': ' || count(*) || ' arrivals > capacity ' || staging_capacity
		FROM arrivals
		GROUP BY id, staging_capacity, w, date_trunc('hour', local) + floor(extract(minute FROM local) / w) * w * interval '1 minute'
		HAVING count(*) > staging_capacity
		LIMIT 10`)
	if err != nil {
		t.Fatalf("staging query: %v", err)
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		found = append(found, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Fatalf("staging over capacity:\n%s", strings.Join(found, "\n"))
	}
}

// assertEventsFor checks the outbox/audit side of a race: every created appointment in ids has
// exactly one APPOINTMENT_CREATED outbox row and one matching history row, and no outbox row
// points at an appointment that does not exist (a loser rolled back must leave nothing behind).
func assertEventsFor(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	for _, id := range ids {
		outbox := scalar[int](t, pool, `
			SELECT count(*)::int FROM outbox_events
			WHERE aggregate_type = $1 AND aggregate_id = $2 AND event_type = `+code(domain.EventAppointmentCreated),
			domain.AggregateAppointment, id)
		history := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointment_history WHERE appointment_id = $1 AND event_type = `+code(domain.EventAppointmentCreated), id)
		if outbox != 1 || history != 1 {
			t.Fatalf("appointment %s: %d APPOINTMENT_CREATED outbox rows and %d history rows, want 1 and 1", id, outbox, history)
		}
	}
	assertNoOrphanEvents(t, pool)
}

// assertNoOrphanEvents: every appointment outbox row belongs to a committed appointment, and every
// appointment has at most one APPOINTMENT_CREATED event.
func assertNoOrphanEvents(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	orphans := scalar[int](t, pool, `
		SELECT count(*)::int FROM outbox_events o
		WHERE o.aggregate_type = $1 AND NOT EXISTS (SELECT 1 FROM appointments a WHERE a.id = o.aggregate_id)`,
		domain.AggregateAppointment)
	if orphans != 0 {
		t.Fatalf("%d outbox rows for appointments that do not exist", orphans)
	}
	dupes := scalar[int](t, pool, `
		SELECT count(*)::int FROM (
			SELECT aggregate_id FROM outbox_events
			WHERE aggregate_type = $1 AND event_type = `+code(domain.EventAppointmentCreated)+`
			GROUP BY aggregate_id HAVING count(*) > 1
		) d`, domain.AggregateAppointment)
	if dupes != 0 {
		t.Fatalf("%d appointments with more than one APPOINTMENT_CREATED event", dupes)
	}
}

// eventCount is the number of outbox rows and history rows of eventType for one appointment.
func eventCount(t *testing.T, pool *pgxpool.Pool, id uuid.UUID, eventType domain.EventType) (outbox, history int) {
	t.Helper()
	outbox = scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, id, int16(eventType))
	history = scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE appointment_id = $1 AND event_type = $2`, id, int16(eventType))
	return outbox, history
}

// wantEvents fails unless appointment id has exactly want outbox rows and want history rows of
// eventType (a losing writer must leave neither).
func wantEvents(t *testing.T, label string, pool *pgxpool.Pool, id uuid.UUID, eventType domain.EventType, want int) {
	t.Helper()
	outbox, history := eventCount(t, pool, id, eventType)
	if outbox != want || history != want {
		t.Fatalf("%s: %v outbox=%d history=%d, want %d", label, eventType, outbox, history, want)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func clearOutbox(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `UPDATE outbox_events SET status = `+code(domain.OutboxPublished)); err != nil { // ignore seed events
		t.Fatal(err)
	}
}

// mustExec runs a fixture statement and fails t on error.
func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// mustBook books req as staffAdmin and fails t on error.
func mustBook(t *testing.T, svc *booking.Service, req booking.BookRequest) domain.Appointment {
	t.Helper()
	item, _, err := svc.Book(t.Context(), staffAdmin, req)
	if err != nil {
		t.Fatalf("book %s at %s: %v", req.VehicleID, req.StartAt, err)
	}
	return item
}

// mustGet reads appointment id as staffAdmin and fails t on error.
func mustGet(t *testing.T, svc *booking.Service, id uuid.UUID) domain.Appointment {
	t.Helper()
	item, err := svc.GetAppointment(t.Context(), staffAdmin, id)
	if err != nil {
		t.Fatalf("get appointment %s: %v", id, err)
	}
	return item
}

// awaitLockWaiter polls pg_stat_activity until some other session of this database is blocked on
// a lock whose query text contains marker (the racing request has reached the contended row), so a
// test can release the lock at exactly that point instead of sleeping and hoping.
func awaitLockWaiter(t *testing.T, pool *pgxpool.Pool, marker string) {
	t.Helper()
	awaitLockWaiters(t, pool, marker, 1)
}

// awaitLockWaiters is awaitLockWaiter for at least n blocked sessions.
func awaitLockWaiters(t *testing.T, pool *pgxpool.Pool, marker string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if lockWaiters(t, pool, marker) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("fewer than %d session(s) blocked on a lock running %q within 10s; sessions: %s", n, marker,
				scalar[string](t, pool, `
					SELECT COALESCE(string_agg(state || ' ' || COALESCE(wait_event_type || '/' || wait_event, '-') || ': ' || left(regexp_replace(query, '\s+', ' ', 'g'), 120), E'\n'), '')
					FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid()`))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lockWaiters counts the other sessions of this database blocked on a lock while running a
// statement whose text contains marker.
func lockWaiters(t *testing.T, pool *pgxpool.Pool, marker string) int {
	t.Helper()
	return scalar[int](t, pool, `
		SELECT count(*)::int FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND wait_event_type = 'Lock' AND query ILIKE '%' || $1 || '%'`, marker)
}

// provisionShop creates an empty dealership open 08:00–18:00 every day (no lunch, no staging cap,
// express lane up to 60 min): a fixture whose bays and technicians the test names and orders.
func provisionShop(t *testing.T, svc *booking.Service, name string) domain.Dealership {
	t.Helper()
	d, err := svc.CreateDealership(t.Context(), staffAdmin, store.NewDealership{
		Name: name + " " + uuid.NewString()[:8], Timezone: "Europe/London", OpenTime: "08:00", CloseTime: "18:00",
		StagingWindowMinutes: 60,
		Policy: domain.Policy{
			CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 90, NoShowGraceMinutes: 30, ExpressMaxMinutes: 60,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// addBay creates an active bay in dealership d and returns its id.
func addBay(t *testing.T, svc *booking.Service, d uuid.UUID, name string, bayType domain.BayType) uuid.UUID {
	t.Helper()
	bay, err := svc.CreateBay(t.Context(), staffAdmin, d, name, bayType)
	if err != nil {
		t.Fatal(err)
	}
	return bay.ID
}

// addTechnician creates an active technician (no shift pattern: works the opening hours) in
// dealership d and returns its id.
func addTechnician(t *testing.T, svc *booking.Service, d uuid.UUID, name string, level int16, skills ...domain.TechnicianSkill) uuid.UUID {
	t.Helper()
	tech, err := svc.CreateTechnician(t.Context(), staffAdmin, store.NewTechnician{
		DealershipID: d, Name: name, SkillLevel: domain.SkillLevel(level), Skills: skills,
	})
	if err != nil {
		t.Fatal(err)
	}
	return tech.ID
}

// code is an enum code as a SQL literal, for the tests' raw SQL (codes are defined once, in
// internal/domain).
func code[T ~int16](v T) string { return strconv.Itoa(int(v)) }

// outboxStatusText is a SQL expression turning the SMALLINT status column col into its name, so a test
// can compare statuses by name: outboxStatusText("status") = 'pending'.
func outboxStatusText(col string) string {
	return caseNames(col, domain.OutboxPending, domain.OutboxProcessing, domain.OutboxPublished, domain.OutboxFailed)
}

func notificationStatusText(col string) string {
	return caseNames(col, domain.NotificationPending, domain.NotificationSending, domain.NotificationSent,
		domain.NotificationFailed, domain.NotificationSkipped)
}

func caseNames[T interface {
	~int16
	Name() string
}](col string, values ...T) string {
	var b strings.Builder
	b.WriteString("(CASE " + col)
	for _, v := range values {
		b.WriteString(" WHEN " + code(v) + " THEN '" + v.Name() + "'")
	}
	b.WriteString(" END)")
	return b.String()
}

// notificationChannelText is notifications.channel by name ('email', 'sms').
func notificationChannelText(col string) string {
	return caseNames(col, domain.ChannelEmail, domain.ChannelSMS)
}

// Appointment status sets as SQL literals, for the tests' raw SQL.
var (
	holdingCodes    = code(domain.StatusConfirmed) + ", " + code(domain.StatusCheckedIn) + ", " + code(domain.StatusInProgress)
	onSiteCodes     = code(domain.StatusCheckedIn) + ", " + code(domain.StatusInProgress)
	notStartedCodes = code(domain.StatusConfirmed) + ", " + code(domain.StatusCheckedIn)
)
