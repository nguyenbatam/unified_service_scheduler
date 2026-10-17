package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

type Store struct {
	pool    *pgxpool.Pool
	signals outboxSignals
	// probe is readiness's own connection (probe.go: Ping), outside pool.
	probe *probeConn
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, signals: newOutboxSignals(), probe: &probeConn{}}
}

type Tx struct {
	tx pgx.Tx
	st *Store
	// events is shared by a TX and its savepoints: set once any of them wrote an outbox row, so
	// the top-level Commit wakes the outbox workers (a rolled-back savepoint only costs a spurious
	// wake-up).
	events *bool
	nested bool
	// savepoint names a savepoint opened by hand on tx (LockArrival) rather than by pgx's Begin:
	// Commit releases it and Rollback rolls back to it and releases it.
	savepoint string
}

func (s *Store) Begin(ctx context.Context) (*Tx, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, st: s, events: new(bool)}, nil
}

func (t *Tx) Begin(ctx context.Context) (*Tx, error) {
	tx, err := t.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, st: t.st, events: t.events, nested: true}, nil
}

// markEvents records that this TX wrote outbox rows.
func (t *Tx) markEvents() {
	if t.events != nil {
		*t.events = true
	}
}

// commitTimeout bounds a COMMIT (or savepoint release) once it is sent.
const commitTimeout = 5 * time.Second

// Commit commits detached from ctx's cancellation (bounded by commitTimeout instead): a request
// deadline or client disconnect that fires mid-COMMIT would report a failure for a write the
// server may already have made durable. Cancellation still cuts every wait before the COMMIT.
//
// A top-level commit that wrote outbox rows then wakes the outbox workers (Store.signalOutbox):
// after the COMMIT, never through a NOTIFY inside the TX, whose commit takes a cluster-wide lock
// and would serialise every booking's COMMIT (and its staging-lock hold) behind the others.
func (t *Tx) Commit(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if t.savepoint != "" {
		_, err := t.tx.Exec(ctx, `RELEASE SAVEPOINT `+t.savepoint)
		return err
	}
	if err := t.tx.Commit(ctx); err != nil {
		return err
	}
	if !t.nested && t.events != nil && *t.events && t.st != nil {
		t.st.signalOutbox()
	}
	return nil
}

// rollbackTimeout bounds a ROLLBACK (or rollback to savepoint) once it is sent.
const rollbackTimeout = 2 * time.Second

// Rollback rolls back detached from ctx's cancellation (bounded by rollbackTimeout instead): pgx
// destroys the connection when a ROLLBACK fails, and an already cancelled or expired request ctx
// fails it at once, so every client disconnect or request timeout would otherwise cost a pooled
// connection (and a reconnect) exactly when the server is overloaded. After a Commit it is a
// no-op (pgx returns ErrTxClosed without a round trip).
func (t *Tx) Rollback(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if t.savepoint != "" {
		// One round trip (simple protocol: no arguments). Released, so later statements run on the
		// TX itself and a later Rollback of this handle fails instead of undoing them.
		_, err := t.tx.Exec(ctx, `ROLLBACK TO SAVEPOINT `+t.savepoint+`; RELEASE SAVEPOINT `+t.savepoint)
		return err
	}
	return t.tx.Rollback(ctx)
}

// GiST exclusion constraint names (001_schema.sql).
const (
	ConstraintBayOverlap     = "appointments_bay_no_overlap"
	ConstraintTechOverlap    = "appointments_technician_no_overlap"
	ConstraintVehicleOverlap = "appointments_vehicle_no_overlap"
)

func IsExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

// ExclusionConstraint returns the violated exclusion constraint name, or "" when err is not one.
func ExclusionConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23P01" {
		return pgErr.ConstraintName
	}
	return ""
}

// IsDeadlock reports a Postgres deadlock (40P01); the savepoint can be rolled back and retried.
func IsDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
}

// IsCheckViolation reports a CHECK constraint failure (23514), e.g. appointments_max_span.
func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// IsLockTimeout reports lock_timeout (55P03) or statement_timeout (57014): the database is busy
// (e.g. an admin change holds the dealership row); the client should retry shortly.
func IsLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "55P03" || pgErr.Code == "57014")
}

func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ConstraintIdempotencyScope is the unique index on (created_by, idempotency_key) (001_schema.sql).
const ConstraintIdempotencyScope = "appointments_idempotency_scope_uniq"

// IsIdempotencyViolation reports a duplicate (created_by, idempotency_key). Behind the per-key
// advisory lock taken first in BookingInputs it is unreachable; it stays a defined answer should the
// lock key and the index scope ever drift apart.
func IsIdempotencyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == ConstraintIdempotencyScope
}

// checkMessages are the client-facing messages of the CHECK constraints a request can reach (names
// as in pg_constraint; unnamed column checks are <table>_<column>_check). Any other check is "value
// is out of range": constraint names are schema detail and never reach clients.
var checkMessages = map[string]string{
	"dealerships_hours_order":                          "closeTime must be after openTime",
	"dealerships_lunch_check":                          "lunch needs both lunchStart and lunchEnd, with the end after the start",
	"dealerships_lunch_within_hours":                   "lunch must fall within opening hours",
	"dealerships_staging_capacity_check":               "stagingCapacity must be 0 or more",
	"dealerships_staging_window_minutes_check":         "stagingWindowMinutes must be 15, 30, or 60",
	"dealerships_customer_lead_minutes_check":          "customerLeadMinutes must be 0–10080",
	"dealerships_customer_change_cutoff_minutes_check": "customerChangeCutoffMinutes must be 0–10080",
	"dealerships_booking_horizon_days_check":           "bookingHorizonDays must be 1–365",
	"dealerships_no_show_grace_minutes_check":          "noShowGraceMinutes must be 5–240",
	"dealerships_express_max_minutes_check":            "expressMaxMinutes must be 0–600",
	"dealership_hours_shape":                           "a weekday is either closed or open with closeTime after openTime",
	"dealership_hours_lunch":                           "a weekday's lunch needs a start and an end within its opening hours",
	"dealership_hours_weekday_check":                   "weekday must be 1–7",
	"dealership_closures_reason_check":                 "reason must be at most 120 characters",
	"technician_shifts_window":                         "a shift must end after it starts",
	"technician_shifts_break":                          "a break needs a start and an end within its shift",
	"technician_shifts_weekday_check":                  "weekday must be 1–7",
	"technician_absences_window":                       "an absence must end after it starts",
	"technician_absences_reason_check":                 "reason must be at most 120 characters",
	"technicians_skill_level_check":                    "skillLevel must be 1–4",
	"technician_skills_skill_check":                    "unknown skill",
	"service_types_duration_minutes_check":             "durationMinutes must be a positive multiple of 30, at most 600",
	"vehicles_vin_format":                              "vin must be 17 characters: digits and capital letters except I, O and Q",
	"appointment_lines_line_no_check":                  "an appointment has 1–5 job lines",
	"service_types_min_skill_level_check":              "minSkillLevel must be 1–4",
	"vehicles_year_check":                              "year must be 1980–2100",
	"appointments_max_span":                            "an appointment cannot span more than 24 hours",
	"customers_name_not_blank":                         "name must not be blank",
	"users_name_not_blank":                             "name must not be blank",
}

// checkViolation is the 400 for a CHECK constraint failure. The constraint name stays in the
// wrapping error (logged by httpapi.fail), never in the message.
func checkViolation(constraint string) error {
	message, ok := checkMessages[constraint]
	if !ok {
		message = "value is out of range"
	}
	return fmt.Errorf("check constraint %s: %w", constraint, domain.NewError(domain.CodeInvalid, message))
}

// translate maps input-shaped Postgres errors to domain errors so they surface as 4xx, not 500.
func translate(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "23514": // check_violation
		return checkViolation(pgErr.ConstraintName)
	case "22007", "22008": // invalid datetime format / out of range
		return domain.NewError(domain.CodeInvalid, "invalid time value")
	case "23503": // foreign_key_violation
		return domain.NewError(domain.CodeNotFound, "referenced record not found")
	}
	return err
}

// holdingStatusesSQL matches GiST exclusion: confirmed=1, checked_in=2, in_progress=3.
var holdingStatusesSQL = `status IN (` + holdingSQL + `)`

// dealershipColumns is shared by SELECTs and UPDATE … RETURNING (see scanDealership).
const dealershipColumns = `
	id, name, timezone,
	to_char(open_time, 'HH24:MI'),
	to_char(close_time, 'HH24:MI'),
	COALESCE(to_char(lunch_start, 'HH24:MI'), ''),
	COALESCE(to_char(lunch_end, 'HH24:MI'), ''),
	staging_capacity, staging_window_minutes,
	customer_lead_minutes, customer_change_cutoff_minutes, booking_horizon_days,
	no_show_grace_minutes, express_max_minutes,
	version`

const dealershipSelect = `SELECT ` + dealershipColumns + ` FROM dealerships`

const serviceTypeSelect = `
	SELECT id, code, name, duration_minutes, required_skill, required_bay_type, min_skill_level, version
	FROM service_types`

const vehicleSelect = `
	SELECT v.id, v.customer_id, c.name, v.vin, v.make, v.model, v.year
	FROM vehicles v
	JOIN customers c ON c.id = v.customer_id`

// bayFitsSQL is true when a bay of type bayExpr can host work requiring requiredExpr: the exact
// type; express when general is required and upgradeExpr allows the soft upgrade (a waiter on a job
// short enough for the express lane, see booking.Requirement.SoftUpgrade); or general when express
// is required (the express spill: a general bay holds anything the quick lane does). Callers rank
// the exact type first, so either fallback is used only when no bay of the job's own type is free.
// The Go twin is booking.BayFits; keep both in step.
func bayFitsSQL(bayExpr, requiredExpr, upgradeExpr string) string {
	return fmt.Sprintf(`(%[1]s = %[2]s OR (%[3]s AND %[2]s = %[4]s AND %[1]s = %[5]s) OR (%[2]s = %[5]s AND %[1]s = %[4]s))`,
		bayExpr, requiredExpr, upgradeExpr, bayGeneralSQL, bayExpressSQL)
}

// ListDealerships returns every dealership in name order, for internal checks across the group
// (the API pages them: ListDealershipsPage).
func (s *Store) ListDealerships(ctx context.Context) ([]domain.Dealership, error) {
	rows, err := s.pool.Query(ctx, dealershipSelect+` ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows, scanDealership)
}

func (s *Store) CreateVehicle(ctx context.Context, v domain.Vehicle) (domain.Vehicle, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO vehicles (id, customer_id, vin, make, model, year)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		v.ID, v.CustomerID, v.VIN, v.Make, v.Model, v.Year)
	if err != nil {
		if IsUniqueViolation(err) {
			return domain.Vehicle{}, domain.NewError(domain.CodeConflict, "a vehicle with this VIN already exists")
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return domain.Vehicle{}, domain.NewError(domain.CodeNotFound, "customer not found")
		}
		return domain.Vehicle{}, translate(err)
	}
	return s.GetVehicle(ctx, v.ID)
}

func (s *Store) GetDealership(ctx context.Context, id uuid.UUID) (domain.Dealership, error) {
	return getDealership(ctx, s.pool, id)
}

// GetDealership inside a TX share-locks the row so an admin config change cannot commit
// between reading the rules and inserting a booking that relies on them.
func (t *Tx) GetDealership(ctx context.Context, id uuid.UUID) (domain.Dealership, error) {
	item, err := scanDealership(t.tx.QueryRow(ctx, dealershipSelect+` WHERE id = $1 FOR SHARE`, id))
	if err != nil {
		return domain.Dealership{}, missing(err, "dealership not found")
	}
	return item, nil
}

// ReadDealership is a plain read on the TX's own connection (no lock): for checks that must not
// queue behind an admin's FOR UPDATE, and must not take a second pool connection while this TX
// holds one (with every connection held by such a TX, the pool would deadlock).
func (t *Tx) ReadDealership(ctx context.Context, id uuid.UUID) (domain.Dealership, error) {
	return getDealership(ctx, t.tx, id)
}

func getDealership(ctx context.Context, q queryer, id uuid.UUID) (domain.Dealership, error) {
	row := q.QueryRow(ctx, dealershipSelect+` WHERE id = $1`, id)
	item, err := scanDealership(row)
	if err != nil {
		return domain.Dealership{}, missing(err, "dealership not found")
	}
	return item, nil
}

func (s *Store) GetVehicle(ctx context.Context, id uuid.UUID) (domain.Vehicle, error) {
	return getVehicle(ctx, s.pool, id)
}

func getVehicle(ctx context.Context, q queryer, id uuid.UUID) (domain.Vehicle, error) {
	row := q.QueryRow(ctx, vehicleSelect+` WHERE v.id = $1`, id)
	item, err := scanVehicle(row)
	if err != nil {
		return domain.Vehicle{}, missing(err, "vehicle not found")
	}
	return item, nil
}

const serviceTypesByIDSQL = serviceTypeSelect + ` WHERE id = ANY($1) ORDER BY id`

// queueServiceTypes queues one read of the service types ids (distinct; booking.serviceIDs
// guarantees it): *dst is them in the order of ids (job lines), or nil when any id is missing. A
// plain read, never locked: a booking snapshots its lines and requirement, and catalog edits apply
// to new bookings only, so no invariant needs the rows held until commit (a share lock would make
// every booking in the group write a row lock on the same few catalog rows, and stall all of them
// behind one group admin's edit).
func queueServiceTypes(b *pgx.Batch, dst *[]domain.ServiceType, ids []uuid.UUID) {
	b.Queue(serviceTypesByIDSQL, ids).Query(func(rows pgx.Rows) error {
		found, err := collect(rows, scanServiceType)
		if err != nil {
			return err
		}
		if out, ok := inIDOrder(found, ids); ok {
			*dst = out
		}
		return nil
	})
}

// inIDOrder returns found in the order of ids; ok is false when any id is missing.
func inIDOrder(found []domain.ServiceType, ids []uuid.UUID) ([]domain.ServiceType, bool) {
	byID := make(map[uuid.UUID]domain.ServiceType, len(found))
	for _, item := range found {
		byID[item.ID] = item
	}
	out := make([]domain.ServiceType, 0, len(ids))
	for _, id := range ids {
		item, ok := byID[id]
		if !ok {
			return nil, false
		}
		out = append(out, item)
	}
	return out, true
}

// baysSQL lists the dealership's active bays (of bayType, with the soft upgrade, unless 0),
// express first.
func baysSQL(dealershipID uuid.UUID, bayType domain.BayType, softUpgrade bool) (string, []any) {
	sql := `
		SELECT id, name, bay_type
		FROM service_bays
		WHERE dealership_id = $1 AND active`
	args := []any{dealershipID}
	if bayType != 0 {
		sql += ` AND ` + bayFitsSQL("bay_type", "$2", "$3")
		args = append(args, int16(bayType), softUpgrade)
	}
	return sql + ` ORDER BY CASE WHEN bay_type = ` + bayExpressSQL + ` THEN 0 ELSE 1 END, name`, args
}

type Busy struct {
	BayID        uuid.UUID
	TechnicianID uuid.UUID
	Start        time.Time
	End          time.Time
}

// Hold is a holding appointment as availability reads it: its resources and window (End is
// extended to the end of the read range while it is a flagged overrun, which holds until
// completed, see overrunningSQL), plus
// what decides whether a booking may move it to another bay or technician to make room
// (booking.movable: confirmed and not started; its booked bay type, waiter flag, labour time,
// skills and level decide where it may go).
type Hold struct {
	Busy
	ID              uuid.UUID
	Status          domain.AppointmentStatus
	RequiredBayType domain.BayType
	IsWaiter        bool
	DurationMinutes int
	RequiredSkills  []domain.TechnicianSkill
	MinSkillLevel   domain.SkillLevel
}

// queueConfirmedBusy queues the holding appointments overlapping [start, end) at a dealership,
// except exclude (the appointment being rescheduled; uuid.Nil = none). The lower bound on start_at
// (no appointment spans more than MaxAppointmentSpan) keeps the (dealership_id, start_at) index
// scan to the window instead of the dealership's whole history; flagged overruns (overrunningSQL)
// hold until completed, so the ones that started earlier are read by a second branch over the
// on-site index, and every flagged hold ends at the range's end.
func queueConfirmedBusy(b *pgx.Batch, dst *[]Hold, dealershipID uuid.UUID, start, end time.Time, exclude uuid.UUID) {
	queueRows(b, dst, func(row pgx.Row) (Hold, error) {
		var item Hold
		var status, bayType int16
		var skills []int16
		err := row.Scan(&item.BayID, &item.TechnicianID, &item.Start, &item.End,
			&item.ID, &status, &bayType, &item.IsWaiter, &item.DurationMinutes, &skills, &item.MinSkillLevel)
		item.Status, item.RequiredBayType = domain.AppointmentStatus(status), domain.BayType(bayType)
		item.RequiredSkills = make([]domain.TechnicianSkill, len(skills))
		for i, sk := range skills {
			item.RequiredSkills[i] = domain.TechnicianSkill(sk)
		}
		return item, err
	}, `
		SELECT a.service_bay_id, a.technician_id, a.start_at,
		       CASE WHEN `+overrunningSQL("a")+` THEN GREATEST(a.end_at, $3::timestamptz) ELSE a.end_at END,
		       a.id, a.status, a.required_bay_type, a.is_waiter, a.booked_duration_minutes, a.required_skills, a.min_skill_level
		FROM appointments a
		WHERE a.dealership_id = $1
		  AND a.`+holdingStatusesSQL+`
		  AND a.id <> $5
		  AND a.start_at < $3::timestamptz
		  AND a.start_at > $4::timestamptz
		  AND (a.end_at > $2::timestamptz OR `+overrunningSQL("a")+`)
		UNION ALL
		SELECT a.service_bay_id, a.technician_id, a.start_at, GREATEST(a.end_at, $3::timestamptz),
		       a.id, a.status, a.required_bay_type, a.is_waiter, a.booked_duration_minutes, a.required_skills, a.min_skill_level
		FROM appointments a
		WHERE a.dealership_id = $1
		  AND a.status = `+statusInProgressSQL+`
		  AND a.overrun_at IS NOT NULL
		  AND a.id <> $5
		  AND a.start_at <= $4::timestamptz
		ORDER BY 3`,
		dealershipID, start, end, start.Add(-MaxAppointmentSpan), exclude)
}

// overrunningSQL is true for alias a when it is in-progress work flagged as overrunning: the
// sweep could not extend it (the next booking holds its technician or vehicle, or the cap is
// reached), so its end_at no longer says when the car leaves the lift. Allocation and
// availability treat its bay and technician as held from its start until it is completed (or
// extended again past now) — a carry-over job, however many days that takes — so no slot is sold
// over a car that is still on the lift; StartWork refuses the next job there anyway. GiST only
// knows end_at, so this is a pre-check, like the rest of the ranking.
// (overrunHeldSQL and queueConfirmedBusy are the two readers; keep them in step.)
func overrunningSQL(a string) string {
	return a + `.status = ` + statusInProgressSQL + ` AND ` + a + `.overrun_at IS NOT NULL`
}

// overrunHeldSQL is true when a resource (owner matches alias a's column) has flagged overrunning
// work that started before the slot ends (end): see overrunningSQL.
func overrunHeldSQL(owner, exclude, end string) string {
	return `EXISTS (
				SELECT 1 FROM appointments a
				WHERE ` + owner + ` AND ` + overrunningSQL("a") + ` AND a.id <> ` + exclude + `
				  AND a.start_at < ` + end + `)`
}

// AppointmentDealership is the dealership of appointment id and its timezone (found false when
// there is none): a plain read outside any TX, so a reschedule can pass its dealership-day gate
// before taking a pool connection for its TX (which re-reads the row under lock and authorises the
// caller).
func (s *Store) AppointmentDealership(ctx context.Context, id uuid.UUID) (uuid.UUID, string, bool, error) {
	var dealershipID uuid.UUID
	var tz string
	err := s.pool.QueryRow(ctx, `
		SELECT a.dealership_id, d.timezone FROM appointments a JOIN dealerships d ON d.id = a.dealership_id
		WHERE a.id = $1`, id).Scan(&dealershipID, &tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", false, nil
	}
	if err != nil {
		return uuid.Nil, "", false, translate(err)
	}
	return dealershipID, tz, true, nil
}

// DealershipTimezone is a dealership's timezone (found false when there is none): a plain read
// outside any TX, for the booking gate's key (timezones never change once a dealership is opened).
func (s *Store) DealershipTimezone(ctx context.Context, id uuid.UUID) (string, bool, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT timezone FROM dealerships WHERE id = $1`, id).Scan(&tz)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, translate(err)
	}
	return tz, true, nil
}

// queueVehicleHolds queues the vehicle's holding appointment windows overlapping [from, to), at any
// dealership, except exclude, so availability can flag slots the car cannot attend. Bounded below
// like queueConfirmedBusy so (vehicle_id, start_at) skips the vehicle's history.
func queueVehicleHolds(b *pgx.Batch, dst *[]Busy, vehicleID uuid.UUID, from, to time.Time, exclude uuid.UUID) {
	queueRows(b, dst, func(row pgx.Row) (Busy, error) {
		var item Busy
		err := row.Scan(&item.Start, &item.End)
		return item, err
	}, `
		SELECT start_at, end_at FROM appointments
		WHERE vehicle_id = $1
		  AND `+holdingStatusesSQL+`
		  AND id <> $5
		  AND start_at < $3::timestamptz
		  AND start_at > $4::timestamptz
		  AND end_at > $2::timestamptz
		ORDER BY start_at`, vehicleID, from, to, from.Add(-MaxAppointmentSpan), exclude)
}

// queueStagingArrivals queues the start times of holding appointments starting in [from, to),
// except exclude, so availability can bucket arrivals per staging window with one query.
func queueStagingArrivals(b *pgx.Batch, dst *[]time.Time, dealershipID uuid.UUID, from, to time.Time, exclude uuid.UUID) {
	queueRows(b, dst, func(row pgx.Row) (time.Time, error) {
		var at time.Time
		err := row.Scan(&at)
		return at, err
	}, `
		SELECT start_at FROM appointments
		WHERE dealership_id = $1
		  AND `+holdingStatusesSQL+`
		  AND id <> $4
		  AND start_at >= $2::timestamptz
		  AND start_at < $3::timestamptz`, dealershipID, from, to, exclude)
}

func (s *Store) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := s.pool.Exec(ctx, sql, args...)
	return err
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func scanDealership(row pgx.Row) (domain.Dealership, error) {
	var item domain.Dealership
	err := row.Scan(
		&item.ID, &item.Name, &item.Timezone, &item.OpenTime, &item.CloseTime,
		&item.LunchStart, &item.LunchEnd, &item.StagingCapacity, &item.StagingWindowMinutes,
		&item.Policy.CustomerLeadMinutes, &item.Policy.CustomerChangeCutoffMinutes, &item.Policy.BookingHorizonDays,
		&item.Policy.NoShowGraceMinutes, &item.Policy.ExpressMaxMinutes,
		&item.Version,
	)
	if err != nil {
		return domain.Dealership{}, err
	}
	return item, nil
}

func scanServiceType(row pgx.Row) (domain.ServiceType, error) {
	var item domain.ServiceType
	var skill, bayType int16
	err := row.Scan(&item.ID, &item.Code, &item.Name, &item.DurationMinutes, &skill, &bayType, &item.MinSkillLevel, &item.Version)
	if err != nil {
		return domain.ServiceType{}, err
	}
	item.RequiredSkill = domain.TechnicianSkill(skill)
	item.RequiredBayType = domain.BayType(bayType)
	return item, nil
}

func scanVehicle(row pgx.Row) (domain.Vehicle, error) {
	var item domain.Vehicle
	err := row.Scan(&item.ID, &item.CustomerID, &item.CustomerName, &item.VIN, &item.Make, &item.Model, &item.Year)
	if err != nil {
		return domain.Vehicle{}, err
	}
	return item, nil
}

func scanBay(row pgx.Row) (domain.Bay, error) {
	var item domain.Bay
	var bayType int16
	if err := row.Scan(&item.ID, &item.Name, &bayType); err != nil {
		return domain.Bay{}, err
	}
	item.BayType = domain.BayType(bayType)
	return item, nil
}

func missing(err error, message string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, message)
	}
	return err
}

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	items := []T{}
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CheckMessageConstraints lists the CHECK constraint names that have a readable message (for the
// integration test that proves each one exists in the schema).
func CheckMessageConstraints() []string {
	names := make([]string, 0, len(checkMessages))
	for name := range checkMessages {
		names = append(names, name)
	}
	return names
}

// PoolStat is a snapshot of the connection pool (for the pool metrics).
func (s *Store) PoolStat() *pgxpool.Stat { return s.pool.Stat() }
