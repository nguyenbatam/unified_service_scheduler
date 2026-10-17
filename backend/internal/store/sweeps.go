package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// minNoShowGrace is the smallest dealerships.no_show_grace_minutes (CHECK): a sargable bound so the
// sweep scans appointments_no_show_sweep_idx only up to now - 5 min.
const minNoShowGrace = 5 * time.Minute

// MarkNoShows flips confirmed appointments whose dealership's no-show grace has passed since start
// to no_show and records their events in the same TX. SKIP LOCKED keeps concurrent sweepers on
// several instances disjoint. Three statements whatever the batch size: lock + update + joined
// read in one, then every event (outbox + history) set-based, while the row locks are held.
func (s *Store) MarkNoShows(ctx context.Context, now time.Time, limit int) (int, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.tx.Query(ctx, `
		WITH due AS (
			SELECT a.id FROM appointments a
			JOIN dealerships d ON d.id = a.dealership_id
			WHERE a.status = `+statusConfirmedSQL+`
			  AND a.start_at < $3::timestamptz
			  AND a.start_at < $1::timestamptz - make_interval(mins => d.no_show_grace_minutes)
			ORDER BY a.start_at
			LIMIT $2
			FOR NO KEY UPDATE OF a SKIP LOCKED
		), upd AS (
			UPDATE appointments a
			SET status = `+statusNoShowSQL+`, version = a.version + 1, updated_at = now()
			FROM due
			WHERE a.id = due.id
			RETURNING a.*
		)`+appointmentColumns+` FROM upd a`+appointmentJoins+` ORDER BY a.start_at, a.id`,
		now, limit, now.Add(-minNoShowGrace))
	if err != nil {
		return 0, err
	}
	items, err := collect(rows, scanAppointment)
	rows.Close()
	if err != nil {
		return 0, err
	}
	if err := tx.RecordAppointmentEvents(ctx, domain.EventAppointmentNoShow, items, nil); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(items), nil
}

// LockWorkStart serialises "start work" for one technician and one bay (advisory locks, always
// technician first, then bay) and reports whether either already has another job in progress.
// Windows never overlap (GiST), but an overrun that nobody extended can still be on the lift.
// One round trip: the statements run in queue order, so the locks keep their order and the check,
// queued after them, reads a snapshot taken once both are held (see batch.go).
func (t *Tx) LockWorkStart(ctx context.Context, technicianID, bayID, exclude uuid.UUID) (techBusy, bayBusy bool, err error) {
	b := &pgx.Batch{}
	b.Queue(advisoryLockSQL, advisoryKey("start|tech|"+technicianID.String())).Exec(ignoreTag)
	b.Queue(advisoryLockSQL, advisoryKey("start|bay|"+bayID.String())).Exec(ignoreTag)
	b.Queue(`
		SELECT
			EXISTS (SELECT 1 FROM appointments WHERE technician_id = $1 AND status = `+statusInProgressSQL+` AND id <> $3),
			EXISTS (SELECT 1 FROM appointments WHERE service_bay_id = $2 AND status = `+statusInProgressSQL+` AND id <> $3)`,
		technicianID, bayID, exclude).QueryRow(func(row pgx.Row) error {
		return row.Scan(&techBusy, &bayBusy)
	})
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return false, false, err
	}
	return techBusy, bayBusy, nil
}

// OverrunHolds reports whether bay and technician (uuid.Nil: not asked) are held by flagged
// overrunning work (overrunHeldSQL) that started before end: GiST knows only
// end_at, so writers that put a hold on a chosen bay or technician without the allocation ranking
// (reassign, reinstate) check it themselves. exclude is the appointment being written. Read after
// the dealership-day lock, so a sweep flagging work that day cannot change the answer before
// commit; a flag set on an earlier day is not queued with this day's writers, so for them it is
// a pre-check, and GiST stays the guarantee.
func (t *Tx) OverrunHolds(ctx context.Context, bayID, technicianID, exclude uuid.UUID, end time.Time) (bayHeld, techHeld bool, err error) {
	err = t.tx.QueryRow(ctx, `SELECT `+overrunHeldSQL("a.service_bay_id = $1", "$3", "$4::timestamptz")+`,
		`+overrunHeldSQL("a.technician_id = $2", "$3", "$4::timestamptz"),
		bayID, technicianID, exclude, end).Scan(&bayHeld, &techHeld)
	return bayHeld, techHeld, err
}

// Overrun is an in-progress job whose booked window has ended.
type Overrun struct {
	ID              uuid.UUID
	DealershipID    uuid.UUID
	VehicleID       uuid.UUID
	StartAt         time.Time
	EndAt           time.Time
	DurationMinutes int        // the booked labour time (snapshot), not the catalog's current value
	OverrunAt       *time.Time // already flagged: keep trying to extend, but do not alert again
}

// overrunDueSQL matches an in-progress job (alias a) the overrun sweep can act on: end_at <= now
// ($1), except one already flagged and already extended to the auto-extension cap (booked labour
// time + maxExtend, $2 seconds): the sweep could do nothing more for it, and picking it every tick
// would crowd extendable jobs out of the batch and lock rows dispatchers need. A flagged job whose
// next whole step would pass the cap still comes through, once, to be extended up to it (the sweep
// extends as far as it goes); unflagged jobs at the cap still come through to be flagged.
var overrunDueSQL = `a.status = ` + statusInProgressSQL + ` AND a.end_at <= $1::timestamptz
		  AND NOT (a.overrun_at IS NOT NULL
		       AND a.end_at >= a.start_at + make_interval(mins => a.booked_duration_minutes, secs => $2::float8))`

// OverrunCandidates lists up to limit jobs the overrun sweep can act on (overrunDueSQL), unflagged
// first, without locking them: the sweep then locks and handles each in its own short TX
// (LockOverrun), so a dispatcher's Complete never waits behind a whole batch.
func (s *Store) OverrunCandidates(ctx context.Context, now time.Time, maxExtend time.Duration, limit int) ([]uuid.UUID, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT a.id FROM appointments a
		WHERE `+overrunDueSQL+`
		ORDER BY a.overrun_at IS NOT NULL, a.end_at
		LIMIT $3`, now, maxExtend.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// LockOverrun row-locks one candidate if it is still one (re-checked under the lock) and not
// locked by anyone else (SKIP LOCKED: another sweeper or a dispatcher has it; never wait). nil
// means there is nothing to do for it now.
func (t *Tx) LockOverrun(ctx context.Context, id uuid.UUID, now time.Time, maxExtend time.Duration) (*Overrun, error) {
	var o Overrun
	err := t.tx.QueryRow(ctx, `
		SELECT a.id, a.dealership_id, a.vehicle_id, a.start_at, a.end_at, a.booked_duration_minutes, a.overrun_at
		FROM appointments a
		WHERE a.id = $3 AND `+overrunDueSQL+`
		FOR NO KEY UPDATE SKIP LOCKED`, now, maxExtend.Seconds(), id).
		Scan(&o.ID, &o.DealershipID, &o.VehicleID, &o.StartAt, &o.EndAt, &o.DurationMinutes, &o.OverrunAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// FirstHoldAfter is the earliest start in [from, to) of another holding appointment sharing the
// bay, technician, or vehicle of appointment id: how far an overrunning job can still be extended
// when a whole sweep step is blocked. ok is false when none starts in that range.
func (t *Tx) FirstHoldAfter(ctx context.Context, id uuid.UUID, from, to time.Time) (time.Time, bool, error) {
	var first *time.Time
	err := t.tx.QueryRow(ctx, `
		SELECT min(o.start_at) FROM appointments a
		JOIN appointments o ON o.id <> a.id AND o.`+holdingStatusesSQL+`
		  AND (o.service_bay_id = a.service_bay_id OR o.technician_id = a.technician_id OR o.vehicle_id = a.vehicle_id)
		  AND o.start_at >= $2::timestamptz AND o.start_at < $3::timestamptz
		WHERE a.id = $1`, id, from, to).Scan(&first)
	if err != nil || first == nil {
		return time.Time{}, false, err
	}
	return *first, true, nil
}

// ExtendEndFlagged moves an overrunning job's end without clearing its overrun flag: a partial
// extension up to the next booking, which still blocks it (the dispatcher was, or is about to be,
// alerted once).
func (t *Tx) ExtendEndFlagged(ctx context.Context, id uuid.UUID, end time.Time) (domain.Appointment, error) {
	return t.updateAppointment(ctx, `end_at = $2::timestamptz, version = version + 1, updated_at = now()`, id, end)
}

// MarkOverrun flags an overrunning job (alert once) and bumps version.
func (t *Tx) MarkOverrun(ctx context.Context, id uuid.UUID) (domain.Appointment, error) {
	return t.updateAppointment(ctx, `overrun_at = now(), version = version + 1, updated_at = now()`, id)
}
