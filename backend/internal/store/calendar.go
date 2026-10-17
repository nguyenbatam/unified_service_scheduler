package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// DayRules is what the calendar says about one civil day: a closure and/or a weekday override.
// Both nil means the dealership's default hours apply.
type DayRules struct {
	Closure  *domain.Closure
	Override *domain.WeekdayHours
}

// DayRules reads the closure and weekday override for day (YYYY-MM-DD, ISO weekday 1–7) inside the
// booking TX: the caller has already share-locked the dealership row (Tx.GetDealership), and
// calendar writers lock it FOR UPDATE, so the rules cannot change between this read and the insert.
func (t *Tx) DayRules(ctx context.Context, dealershipID uuid.UUID, day string, weekday int16) (DayRules, error) {
	rules, err := scanDayRules(t.tx.QueryRow(ctx, dayRulesSQL, dealershipID, day, weekday), day, weekday)
	return rules, translate(err)
}

// queueDayRules queues the same read as Tx.DayRules into *dst.
func queueDayRules(b *pgx.Batch, dst *DayRules, dealershipID uuid.UUID, day string, weekday int16) {
	b.Queue(dayRulesSQL, dealershipID, day, weekday).QueryRow(func(row pgx.Row) error {
		rules, err := scanDayRules(row, day, weekday)
		*dst = rules
		return translate(err)
	})
}

const dayRulesSQL = `
	SELECT c.day IS NOT NULL, COALESCE(c.reason, ''),
	       h.weekday IS NOT NULL, COALESCE(h.closed, false),
	       COALESCE(to_char(h.open_time, 'HH24:MI'), ''), COALESCE(to_char(h.close_time, 'HH24:MI'), ''),
	       COALESCE(to_char(h.lunch_start, 'HH24:MI'), ''), COALESCE(to_char(h.lunch_end, 'HH24:MI'), '')
	FROM (SELECT 1) one
	LEFT JOIN dealership_closures c ON c.dealership_id = $1 AND c.day = $2::date
	LEFT JOIN dealership_hours h ON h.dealership_id = $1 AND h.weekday = $3`

func scanDayRules(row pgx.Row, day string, weekday int16) (DayRules, error) {
	var (
		closed, hasOverride bool
		reason              string
		h                   domain.WeekdayHours
		isClosure           bool
	)
	if err := row.Scan(&isClosure, &reason, &hasOverride, &closed, &h.OpenTime, &h.CloseTime, &h.LunchStart, &h.LunchEnd); err != nil {
		return DayRules{}, err
	}
	return buildDayRules(day, weekday, isClosure, reason, hasOverride, closed, h), nil
}

// DayRulesAt is the DayRules of the local day containing an instant, in the dealership's timezone
// as Postgres resolves it, with that day (YYYY-MM-DD; "" when the dealership does not exist).
// BookingInputs and MoveInputs read it in their round trip, queued after the dealership's FOR
// SHARE, so a booking's calendar costs no round trip of its own. Callers compare Day with the day
// they computed in Go and fall back to Tx.DayRules when they differ (a tz database mismatch).
type DayRulesAt struct {
	Day   string
	Rules DayRules
}

const dayRulesAtSQL = `
	SELECT to_char(d.day, 'YYYY-MM-DD'), d.weekday,
	       c.day IS NOT NULL, COALESCE(c.reason, ''),
	       h.weekday IS NOT NULL, COALESCE(h.closed, false),
	       COALESCE(to_char(h.open_time, 'HH24:MI'), ''), COALESCE(to_char(h.close_time, 'HH24:MI'), ''),
	       COALESCE(to_char(h.lunch_start, 'HH24:MI'), ''), COALESCE(to_char(h.lunch_end, 'HH24:MI'), '')
	FROM (
		SELECT ($2::timestamptz AT TIME ZONE dl.timezone)::date AS day,
		       EXTRACT(ISODOW FROM ($2::timestamptz AT TIME ZONE dl.timezone))::smallint AS weekday
		FROM dealerships dl WHERE dl.id = $1
	) d
	LEFT JOIN dealership_closures c ON c.dealership_id = $1 AND c.day = d.day
	LEFT JOIN dealership_hours h ON h.dealership_id = $1 AND h.weekday = d.weekday`

// queueDayRulesAt queues the calendar read of the local day containing at into *dst.
func queueDayRulesAt(b *pgx.Batch, dst *DayRulesAt, dealershipID uuid.UUID, at time.Time) {
	b.Queue(dayRulesAtSQL, dealershipID, at).QueryRow(func(row pgx.Row) error {
		var (
			day                            string
			weekday                        int16
			isClosure, hasOverride, closed bool
			reason                         string
			h                              domain.WeekdayHours
		)
		err := row.Scan(&day, &weekday, &isClosure, &reason, &hasOverride, &closed, &h.OpenTime, &h.CloseTime, &h.LunchStart, &h.LunchEnd)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return translate(err)
		}
		*dst = DayRulesAt{Day: day, Rules: buildDayRules(day, weekday, isClosure, reason, hasOverride, closed, h)}
		return nil
	})
}

func buildDayRules(day string, weekday int16, isClosure bool, reason string, hasOverride, closed bool, h domain.WeekdayHours) DayRules {
	var rules DayRules
	if isClosure {
		rules.Closure = &domain.Closure{Date: day, Reason: reason}
	}
	if hasOverride {
		h.Weekday = weekday
		h.Closed = closed
		rules.Override = &h
	}
	return rules
}

const weekdayHoursSelect = `
	SELECT weekday, closed,
	       COALESCE(to_char(open_time, 'HH24:MI'), ''), COALESCE(to_char(close_time, 'HH24:MI'), ''),
	       COALESCE(to_char(lunch_start, 'HH24:MI'), ''), COALESCE(to_char(lunch_end, 'HH24:MI'), '')
	FROM dealership_hours`

func scanWeekdayHours(row pgx.Row) (domain.WeekdayHours, error) {
	var h domain.WeekdayHours
	err := row.Scan(&h.Weekday, &h.Closed, &h.OpenTime, &h.CloseTime, &h.LunchStart, &h.LunchEnd)
	return h, err
}

// queueWeeklyHours queues the dealership's weekday overrides (availability reads them once for a
// whole multi-day search; days without one use the default hours).
func queueWeeklyHours(b *pgx.Batch, dst *[]domain.WeekdayHours, dealershipID uuid.UUID) {
	queueRows(b, dst, scanWeekdayHours, weekdayHoursSelect+` WHERE dealership_id = $1 ORDER BY weekday`, dealershipID)
}

// WeeklyHours reads the dealership's weekday overrides in tx (a booking refused as outside the
// hours checks whether any weekday could hold the job at all).
func (t *Tx) WeeklyHours(ctx context.Context, dealershipID uuid.UUID) ([]domain.WeekdayHours, error) {
	var out []domain.WeekdayHours
	b := &pgx.Batch{}
	queueWeeklyHours(b, &out, dealershipID)
	return out, sendBatch(ctx, t.tx, b)
}

// queueClosures queues the dealership's dated closures on days in [fromDay, toDay] (YYYY-MM-DD).
func queueClosures(b *pgx.Batch, dst *[]domain.Closure, dealershipID uuid.UUID, fromDay, toDay string) {
	queueRows(b, dst, scanClosure, `
		SELECT to_char(day, 'YYYY-MM-DD'), reason FROM dealership_closures
		WHERE dealership_id = $1 AND day BETWEEN $2::date AND $3::date
		ORDER BY day`, dealershipID, fromDay, toDay)
}

func scanClosure(row pgx.Row) (domain.Closure, error) {
	var c domain.Closure
	err := row.Scan(&c.Date, &c.Reason)
	return c, err
}

// WeeklyHoursByDealership returns every weekday override, grouped by dealership.
func (s *Store) WeeklyHoursByDealership(ctx context.Context) (map[uuid.UUID][]domain.WeekdayHours, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT dealership_id, weekday, closed,
		       COALESCE(to_char(open_time, 'HH24:MI'), ''), COALESCE(to_char(close_time, 'HH24:MI'), ''),
		       COALESCE(to_char(lunch_start, 'HH24:MI'), ''), COALESCE(to_char(lunch_end, 'HH24:MI'), '')
		FROM dealership_hours`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]domain.WeekdayHours{}
	for rows.Next() {
		var id uuid.UUID
		var h domain.WeekdayHours
		if err := rows.Scan(&id, &h.Weekday, &h.Closed, &h.OpenTime, &h.CloseTime, &h.LunchStart, &h.LunchEnd); err != nil {
			return nil, err
		}
		out[id] = append(out[id], h)
	}
	return out, rows.Err()
}

// GetCalendar returns weekly overrides and closures from the dealership's local today (at now, the
// service clock) onward.
func (s *Store) GetCalendar(ctx context.Context, now time.Time, dealershipID uuid.UUID) (domain.DealershipCalendar, error) {
	return getCalendar(ctx, s.pool, dealershipID, now)
}

func getCalendar(ctx context.Context, q queryer, dealershipID uuid.UUID, now time.Time) (domain.DealershipCalendar, error) {
	cal := domain.DealershipCalendar{DealershipID: dealershipID}
	if err := q.QueryRow(ctx, `SELECT version FROM dealerships WHERE id = $1`, dealershipID).Scan(&cal.Version); err != nil {
		return domain.DealershipCalendar{}, missing(err, "dealership not found")
	}
	rows, err := q.Query(ctx, weekdayHoursSelect+` WHERE dealership_id = $1 ORDER BY weekday`, dealershipID)
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	weekly, err := collect(rows, scanWeekdayHours)
	rows.Close()
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	rows, err = q.Query(ctx, `
		SELECT to_char(c.day, 'YYYY-MM-DD'), c.reason
		FROM dealership_closures c
		JOIN dealerships d ON d.id = c.dealership_id
		WHERE c.dealership_id = $1 AND c.day >= ($2::timestamptz AT TIME ZONE d.timezone)::date
		ORDER BY c.day`, dealershipID, now)
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	closures, err := collect(rows, scanClosure)
	rows.Close()
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	cal.Weekly, cal.Closures = weekly, closures
	return cal, nil
}

// ReplaceWeeklyHours swaps the whole set of weekday overrides (weekdays left out fall back to the
// default hours). It fails with 409 while a booking upcoming at now would fall outside the new hours.
func (s *Store) ReplaceWeeklyHours(ctx context.Context, now time.Time, dealershipID uuid.UUID, weekly []domain.WeekdayHours, ifMatch int) (domain.DealershipCalendar, error) {
	return s.changeCalendar(ctx, now, dealershipID, ifMatch, func(tx *Tx) error {
		if _, err := tx.tx.Exec(ctx, `DELETE FROM dealership_hours WHERE dealership_id = $1`, dealershipID); err != nil {
			return err
		}
		for _, h := range weekly {
			if _, err := tx.tx.Exec(ctx, `
				INSERT INTO dealership_hours (dealership_id, weekday, closed, open_time, close_time, lunch_start, lunch_end)
				VALUES ($1, $2, $3, NULLIF($4, '')::time, NULLIF($5, '')::time, NULLIF($6, '')::time, NULLIF($7, '')::time)`,
				dealershipID, h.Weekday, h.Closed, h.OpenTime, h.CloseTime, h.LunchStart, h.LunchEnd); err != nil {
				if IsUniqueViolation(err) {
					return domain.NewError(domain.CodeInvalid, fmt.Sprintf("weekday %d is listed twice", h.Weekday))
				}
				return translate(err)
			}
		}
		return nil
	})
}

// AddClosure closes the dealership for a whole local day. 409 if already closed that day or if
// upcoming bookings sit on it (move them first).
// MaxUpcomingClosures bounds a dealership's closures from its local today on (a year of days), so
// the calendar (GET /admin/dealerships/:id/calendar and every edit's answer) stays one small
// object instead of a list to page.
const MaxUpcomingClosures = 366

// AddClosure closes the dealership on c.Date; 409 once MaxUpcomingClosures are upcoming (counted
// under the dealership row lock, so concurrent adds cannot pass it together).
func (s *Store) AddClosure(ctx context.Context, now time.Time, dealershipID uuid.UUID, c domain.Closure, ifMatch int) (domain.DealershipCalendar, error) {
	return s.changeCalendar(ctx, now, dealershipID, ifMatch, func(tx *Tx) error {
		var upcoming int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM dealership_closures c
			JOIN dealerships d ON d.id = c.dealership_id
			WHERE c.dealership_id = $1 AND c.day >= ($2::timestamptz AT TIME ZONE d.timezone)::date`,
			dealershipID, now).Scan(&upcoming); err != nil {
			return err
		}
		if upcoming >= MaxUpcomingClosures {
			return domain.NewError(domain.CodeConflict, fmt.Sprintf("a dealership has at most %d upcoming closures; remove one first", MaxUpcomingClosures))
		}
		_, err := tx.tx.Exec(ctx, `
			INSERT INTO dealership_closures (dealership_id, day, reason) VALUES ($1, $2::date, $3)`,
			dealershipID, c.Date, c.Reason)
		if IsUniqueViolation(err) {
			return domain.NewError(domain.CodeConflict, "the dealership is already closed on "+c.Date)
		}
		return translate(err)
	})
}

// DeleteClosure reopens a closed day.
func (s *Store) DeleteClosure(ctx context.Context, now time.Time, dealershipID uuid.UUID, day string, ifMatch int) (domain.DealershipCalendar, error) {
	return s.changeCalendar(ctx, now, dealershipID, ifMatch, func(tx *Tx) error {
		tag, err := tx.tx.Exec(ctx, `DELETE FROM dealership_closures WHERE dealership_id = $1 AND day = $2::date`, dealershipID, day)
		if err != nil {
			return translate(err)
		}
		if tag.RowsAffected() == 0 {
			return domain.NewError(domain.CodeNotFound, "no closure on "+day)
		}
		return nil
	})
}

// changeCalendar runs one calendar edit under the dealership row lock (FOR UPDATE, so in-flight
// bookings that share-locked it finish first and are then seen by the stranded check), bumps the
// dealership version, refuses to strand bookings upcoming at now (the service clock), and emits
// CONFIG_UPDATED in the TX.
func (s *Store) changeCalendar(ctx context.Context, now time.Time, dealershipID uuid.UUID, ifMatch int, edit func(tx *Tx) error) (domain.DealershipCalendar, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var version int
	if err := tx.tx.QueryRow(ctx, `SELECT version FROM dealerships WHERE id = $1 FOR UPDATE`, dealershipID).Scan(&version); err != nil {
		return domain.DealershipCalendar{}, missing(err, "dealership not found")
	}
	if err := domain.CheckIfMatch(ifMatch, version, "record"); err != nil {
		return domain.DealershipCalendar{}, err
	}
	if err := edit(tx); err != nil {
		return domain.DealershipCalendar{}, err
	}
	updated, err := scanDealership(tx.tx.QueryRow(ctx, `
		UPDATE dealerships SET version = version + 1 WHERE id = $1
		RETURNING `+dealershipColumns, dealershipID))
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	if err := tx.refuseStranded(ctx, dealershipID, now); err != nil {
		return domain.DealershipCalendar{}, err
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateDealership, dealershipID, domain.EventConfigUpdated, updated); err != nil {
		return domain.DealershipCalendar{}, err
	}
	cal, err := getCalendar(ctx, tx.tx, dealershipID, now)
	if err != nil {
		return domain.DealershipCalendar{}, err
	}
	return cal, tx.Commit(ctx)
}

// refuseStranded returns 409 while any confirmed or checked-in appointment that has not ended falls,
// over the part of its window still ahead of now, outside the hours that apply to its local day
// after the change (closure, closed weekday, weekday override, or defaults), crosses midnight, or
// overlaps that day's lunch — the rule Tx.refuseStrandedTechnician applies to rosters: a job that
// started at 10:00 and runs to 13:00 is stranded by a 12:00 closing made at 10:30, and Extend or
// Reinstate of it would then fail; an edit that only touches a window's past is allowed.
// In-progress work is left out (an overrun on the lift never blocks a calendar edit, and its
// window says when it was booked, not when it ends). now is the service clock, passed as a
// parameter rather than Postgres now(), so a pinned clock gives the same answer whatever the wall
// time. It reads the TX's own uncommitted config, so callers
// run it after their UPDATE/INSERT; table CHECKs fail first (400) on malformed input.
func (t *Tx) refuseStranded(ctx context.Context, dealershipID uuid.UUID, now time.Time) error {
	var stranded int
	err := t.tx.QueryRow(ctx, `
		SELECT count(*)::int
		FROM appointments a
		JOIN dealerships d ON d.id = a.dealership_id
		CROSS JOIN LATERAL (
			SELECT greatest(a.start_at, $2::timestamptz) AT TIME ZONE d.timezone AS ls, a.end_at AT TIME ZONE d.timezone AS le
		) l
		LEFT JOIN dealership_hours h
		       ON h.dealership_id = d.id AND h.weekday = EXTRACT(ISODOW FROM l.ls)::smallint
		LEFT JOIN dealership_closures c
		       ON c.dealership_id = d.id AND c.day = l.ls::date
		CROSS JOIN LATERAL (
			SELECT CASE WHEN h.weekday IS NULL THEN d.open_time ELSE h.open_time END AS open_t,
			       CASE WHEN h.weekday IS NULL THEN d.close_time ELSE h.close_time END AS close_t,
			       CASE WHEN h.weekday IS NULL THEN d.lunch_start ELSE h.lunch_start END AS lunch_s,
			       CASE WHEN h.weekday IS NULL THEN d.lunch_end ELSE h.lunch_end END AS lunch_e
		) e
		WHERE a.dealership_id = $1 AND a.status IN (`+confirmedOrCheckedInSQL+`) AND a.end_at > $2::timestamptz
		  AND a.start_at > $3::timestamptz -- no hold spans more than MaxAppointmentSpan: index range
		  AND (
		    c.day IS NOT NULL
		    OR COALESCE(h.closed, false)
		    OR l.ls::time < e.open_t
		    OR l.le::time > e.close_t
		    OR l.le::date > l.ls::date
		    OR (e.lunch_s IS NOT NULL AND l.ls::time < e.lunch_e AND l.le::time > e.lunch_s)
		  )`, dealershipID, now, now.Add(-MaxAppointmentSpan)).Scan(&stranded)
	if err != nil {
		return translate(err)
	}
	if stranded > 0 {
		return domain.NewError(domain.CodeConflict,
			fmt.Sprintf("%d upcoming appointment(s) fall outside the new hours, lunch, or closures; move them first", stranded))
	}
	return nil
}
