package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// queueTechnicians queues the dealership's active technicians with skills (day board).
func queueTechnicians(b *pgx.Batch, dst *[]domain.Technician, dealershipID uuid.UUID) {
	queueRows(b, dst, func(row pgx.Row) (domain.Technician, error) {
		var item domain.Technician
		var skills []int16
		if err := row.Scan(&item.ID, &item.Name, &item.SkillLevel, &skills); err != nil {
			return domain.Technician{}, err
		}
		item.Skills = make([]domain.TechnicianSkill, len(skills))
		for i, sk := range skills {
			item.Skills[i] = domain.TechnicianSkill(sk)
		}
		return item, nil
	}, `
		SELECT t.id, t.name, t.skill_level,
		       coalesce(array_agg(sk.skill ORDER BY sk.skill) FILTER (WHERE sk.skill IS NOT NULL), '{}')
		FROM technicians t
		LEFT JOIN technician_skills sk ON sk.technician_id = t.id
		WHERE t.dealership_id = $1 AND t.active
		GROUP BY t.id
		ORDER BY t.name`, dealershipID)
}

// Rosters is what availability needs about technicians' duty over a range of days: every weekly
// shift (day-independent) and every absence overlapping the range, per technician.
type Rosters struct {
	Shifts   map[uuid.UUID][]domain.TechnicianShift
	Absences map[uuid.UUID][]Busy
}

// queueRosters queues the shifts and absences (overlapping [from, to)) of the technicians matched
// by techFilter, a condition on technician_id whose only parameter is $1 = techArg. dst must have
// its maps made.
func queueRosters(b *pgx.Batch, dst *Rosters, techFilter string, techArg any, from, to time.Time) {
	b.Queue(`
		SELECT technician_id, weekday, to_char(start_time, 'HH24:MI'), to_char(end_time, 'HH24:MI'),
		       COALESCE(to_char(break_start, 'HH24:MI'), ''), COALESCE(to_char(break_end, 'HH24:MI'), '')
		FROM technician_shifts WHERE `+techFilter, techArg).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var id uuid.UUID
			var sh domain.TechnicianShift
			if err := rows.Scan(&id, &sh.Weekday, &sh.StartTime, &sh.EndTime, &sh.BreakStart, &sh.BreakEnd); err != nil {
				return err
			}
			dst.Shifts[id] = append(dst.Shifts[id], sh)
		}
		return rows.Err()
	})
	b.Queue(`
		SELECT technician_id, starts_at, ends_at FROM technician_absences
		WHERE `+techFilter+`
		  AND tstzrange(starts_at, ends_at, '[)') && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		ORDER BY starts_at`, techArg, from, to).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			var id uuid.UUID
			var a Busy
			if err := rows.Scan(&id, &a.Start, &a.End); err != nil {
				return err
			}
			dst.Absences[id] = append(dst.Absences[id], a)
		}
		return rows.Err()
	})
}

// GetRoster returns a technician's weekly shifts and the absences not ended at now (service clock).
func (s *Store) GetRoster(ctx context.Context, now time.Time, technicianID uuid.UUID) (domain.TechnicianRoster, error) {
	return getRoster(ctx, s.pool, technicianID, now)
}

func getRoster(ctx context.Context, q queryer, technicianID uuid.UUID, now time.Time) (domain.TechnicianRoster, error) {
	r := domain.TechnicianRoster{TechnicianID: technicianID}
	if err := q.QueryRow(ctx, `SELECT version FROM technicians WHERE id = $1`, technicianID).Scan(&r.Version); err != nil {
		return domain.TechnicianRoster{}, missing(err, "technician not found")
	}
	rows, err := q.Query(ctx, `
		SELECT weekday, to_char(start_time, 'HH24:MI'), to_char(end_time, 'HH24:MI'),
		       COALESCE(to_char(break_start, 'HH24:MI'), ''), COALESCE(to_char(break_end, 'HH24:MI'), '')
		FROM technician_shifts WHERE technician_id = $1 ORDER BY weekday`, technicianID)
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	shifts, err := collect(rows, func(row pgx.Row) (domain.TechnicianShift, error) {
		var sh domain.TechnicianShift
		err := row.Scan(&sh.Weekday, &sh.StartTime, &sh.EndTime, &sh.BreakStart, &sh.BreakEnd)
		return sh, err
	})
	rows.Close()
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	rows, err = q.Query(ctx, `
		SELECT id, starts_at, ends_at, reason FROM technician_absences
		WHERE technician_id = $1 AND ends_at > $2::timestamptz ORDER BY starts_at`, technicianID, now)
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	absences, err := collect(rows, func(row pgx.Row) (domain.TechnicianAbsence, error) {
		var a domain.TechnicianAbsence
		err := row.Scan(&a.ID, &a.StartsAt, &a.EndsAt, &a.Reason)
		return a, err
	})
	rows.Close()
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	r.Shifts, r.Absences = shifts, absences
	return r, nil
}

// ReplaceShifts swaps a technician's weekly pattern ([] = works the dealership's opening hours).
func (s *Store) ReplaceShifts(ctx context.Context, now time.Time, technicianID uuid.UUID, shifts []domain.TechnicianShift, ifMatch int) (domain.TechnicianRoster, error) {
	return s.changeRoster(ctx, now, technicianID, ifMatch, func(tx *Tx) error {
		if _, err := tx.tx.Exec(ctx, `DELETE FROM technician_shifts WHERE technician_id = $1`, technicianID); err != nil {
			return err
		}
		for _, sh := range shifts {
			if _, err := tx.tx.Exec(ctx, `
				INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time, break_start, break_end)
				VALUES ($1, $2, $3::time, $4::time, NULLIF($5, '')::time, NULLIF($6, '')::time)`,
				technicianID, sh.Weekday, sh.StartTime, sh.EndTime, sh.BreakStart, sh.BreakEnd); err != nil {
				if IsUniqueViolation(err) {
					return domain.NewError(domain.CodeInvalid, fmt.Sprintf("weekday %d is listed twice", sh.Weekday))
				}
				return translate(err)
			}
		}
		return nil
	})
}

// AddAbsence blocks the technician over [StartsAt, EndsAt). 409 if it overlaps another absence or
// an upcoming booking still assigned to them (reassign it first).
// MaxUpcomingAbsences bounds a technician's absences not yet ended, so the roster
// (GET /admin/technicians/:id/roster and every edit's answer) stays one small object instead of a
// list to page.
const MaxUpcomingAbsences = 100

// AddAbsence records an absence; 409 once MaxUpcomingAbsences have not ended (counted under the
// technician row lock, so concurrent adds cannot pass it together).
func (s *Store) AddAbsence(ctx context.Context, now time.Time, technicianID uuid.UUID, a domain.TechnicianAbsence, ifMatch int) (domain.TechnicianRoster, error) {
	return s.changeRoster(ctx, now, technicianID, ifMatch, func(tx *Tx) error {
		var upcoming int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*) FROM technician_absences WHERE technician_id = $1 AND ends_at > $2::timestamptz`,
			technicianID, now).Scan(&upcoming); err != nil {
			return err
		}
		if upcoming >= MaxUpcomingAbsences {
			return domain.NewError(domain.CodeConflict, fmt.Sprintf("a technician has at most %d upcoming absences; remove one first", MaxUpcomingAbsences))
		}
		_, err := tx.tx.Exec(ctx, `
			INSERT INTO technician_absences (id, technician_id, starts_at, ends_at, reason)
			VALUES ($1, $2, $3::timestamptz, $4::timestamptz, $5)`, a.ID, technicianID, a.StartsAt, a.EndsAt, a.Reason)
		if IsExclusionViolation(err) {
			return domain.NewError(domain.CodeConflict, "the technician already has an absence overlapping that time")
		}
		return translate(err)
	})
}

func (s *Store) DeleteAbsence(ctx context.Context, now time.Time, technicianID, absenceID uuid.UUID, ifMatch int) (domain.TechnicianRoster, error) {
	return s.changeRoster(ctx, now, technicianID, ifMatch, func(tx *Tx) error {
		tag, err := tx.tx.Exec(ctx, `DELETE FROM technician_absences WHERE id = $1 AND technician_id = $2`, absenceID, technicianID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.NewError(domain.CodeNotFound, "absence not found")
		}
		return nil
	})
}

// changeRoster runs one roster edit under the technician row lock (FOR UPDATE, so in-flight
// bookings that share-locked it in LockBookableTechnician finish first and are then seen by the
// stranded check, at now: the service clock), bumps the technician version, and emits
// CONFIG_UPDATED in the TX.
func (s *Store) changeRoster(ctx context.Context, now time.Time, technicianID uuid.UUID, ifMatch int, edit func(tx *Tx) error) (domain.TechnicianRoster, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var version int
	if err := tx.tx.QueryRow(ctx, `SELECT version FROM technicians WHERE id = $1 FOR UPDATE`, technicianID).Scan(&version); err != nil {
		return domain.TechnicianRoster{}, missing(err, "technician not found")
	}
	if err := domain.CheckIfMatch(ifMatch, version, "record"); err != nil {
		return domain.TechnicianRoster{}, err
	}
	if err := edit(tx); err != nil {
		return domain.TechnicianRoster{}, err
	}
	if _, err := tx.tx.Exec(ctx, `UPDATE technicians SET version = version + 1 WHERE id = $1`, technicianID); err != nil {
		return domain.TechnicianRoster{}, err
	}
	if err := tx.refuseStrandedTechnician(ctx, technicianID, now); err != nil {
		return domain.TechnicianRoster{}, err
	}
	updated, err := getTechnicianAdmin(ctx, tx.tx, technicianID)
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateTechnician, technicianID, domain.EventConfigUpdated, updated); err != nil {
		return domain.TechnicianRoster{}, err
	}
	roster, err := getRoster(ctx, tx.tx, technicianID, now)
	if err != nil {
		return domain.TechnicianRoster{}, err
	}
	return roster, tx.Commit(ctx)
}

// refuseStrandedTechnician returns 409 while a confirmed or checked-in appointment of the
// technician that has not ended at now falls outside their (new, uncommitted) roster: off shift, in
// the break, or during an absence — for the part of it still ahead of now. In-progress work is left
// out: the technician is on it now, and an overrun past the shift must not block every edit. now is
// the service clock (a parameter, not Postgres now()), so a pinned clock is deterministic.
func (t *Tx) refuseStrandedTechnician(ctx context.Context, technicianID uuid.UUID, now time.Time) error {
	var stranded int
	err := t.tx.QueryRow(ctx, `
		SELECT count(*)::int
		FROM appointments a
		JOIN technicians t ON t.id = a.technician_id
		JOIN dealerships d ON d.id = t.dealership_id
		CROSS JOIN LATERAL (SELECT greatest(a.start_at, $2::timestamptz) AS from_at) r
		WHERE a.technician_id = $1 AND a.status IN (`+confirmedOrCheckedInSQL+`) AND a.end_at > $2::timestamptz
		  AND NOT `+technicianOnDutySQL("t", "d.timezone", "r.from_at", "a.end_at"), technicianID, now).Scan(&stranded)
	if err != nil {
		return translate(err)
	}
	if stranded > 0 {
		return domain.NewError(domain.CodeConflict,
			fmt.Sprintf("%d upcoming appointment(s) of this technician fall outside the new roster; reassign them first", stranded))
	}
	return nil
}
