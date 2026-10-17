package store

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Allocation queries run inside the booking TX. They only rank and pre-filter candidates; the GiST
// exclusion constraints remain the double-booking guarantee.

// technicianOnDutySQL is true when technician alias t works the whole [start, end): no absence
// overlaps it, and either t has no shift rows (works the opening hours) or that local weekday's
// shift covers it without touching the break. tz is the dealership timezone expression. The shift
// test compares local wall-clock times of day and dates, never instants, so a shift boundary in a
// skipped or repeated hour still compares as written. The Go twin is booking.roster.onDuty (same
// wall-clock comparison); keep both in step.
func technicianOnDutySQL(t, tz, start, end string) string {
	return fmt.Sprintf(`(
		NOT EXISTS (
			SELECT 1 FROM technician_absences ab
			WHERE ab.technician_id = %[1]s.id
			  AND tstzrange(ab.starts_at, ab.ends_at, '[)') && tstzrange(%[3]s, %[4]s, '[)'))
		AND (
			NOT EXISTS (SELECT 1 FROM technician_shifts sh WHERE sh.technician_id = %[1]s.id)
			OR EXISTS (
				SELECT 1 FROM technician_shifts sh
				WHERE sh.technician_id = %[1]s.id
				  AND sh.weekday = EXTRACT(ISODOW FROM (%[3]s AT TIME ZONE %[2]s))::smallint
				  AND (%[4]s AT TIME ZONE %[2]s)::date = (%[3]s AT TIME ZONE %[2]s)::date
				  AND sh.start_time <= (%[3]s AT TIME ZONE %[2]s)::time
				  AND sh.end_time >= (%[4]s AT TIME ZONE %[2]s)::time
				  AND (sh.break_start IS NULL
				       OR (%[4]s AT TIME ZONE %[2]s)::time <= sh.break_start
				       OR (%[3]s AT TIME ZONE %[2]s)::time >= sh.break_end))))`, t, tz, start, end)
}

// hasSkillsSQL is true when technician alias t holds every skill in the smallint[] placeholder.
func hasSkillsSQL(t, skillsPH string) string {
	return fmt.Sprintf(`NOT EXISTS (
		SELECT unnest(%[2]s::smallint[])
		EXCEPT
		SELECT sk.skill FROM technician_skills sk WHERE sk.technician_id = %[1]s.id)`, t, skillsPH)
}

func skillCodes(skills []domain.TechnicianSkill) []int16 {
	out := make([]int16, len(skills))
	for i, sk := range skills {
		out[i] = int16(sk)
	}
	return out
}

// Slot is the window being allocated plus the local day around it (for ranking).
type Slot struct {
	Start, End       time.Time
	DayStart, DayEnd time.Time // the local day: a technician's load counts the holds starting in it
	// Lo and Hi bound the bookable stretch around the slot — opening or the end of lunch before
	// it, the start of lunch or closing after it — so idle time is measured where work could
	// really go (an empty morning is a 4-hour gap, not one back to midnight; lunch is an edge).
	Lo, Hi time.Time
	// Exclude is the appointment being moved (reschedule): its own current hold is ignored.
	Exclude uuid.UUID
}

type BayQuery struct {
	DealershipID uuid.UUID
	Required     domain.BayType
	SoftUpgrade  bool // general → express allowed (short waiter job); express → general always (bayFitsSQL)
	Slot
}

// freeBaysSQL lists bays of the required type free for the slot (and not held by flagged
// overrunning work, overrunHeldSQL), then the fallback bays bayFitsSQL allows — free express bays
// for a soft-upgraded waiter, free general bays for an express job — each group best fit first
// (fitRankSQL: the bay whose neighbouring bookings leave the least idle time around the slot
// inside its bookable stretch, Slot.Lo–Hi), so long empty stretches stay available for long jobs
// (less fragmentation); then by name (byte order, as booking.availRange.pickBay compares) and id.
// A fallback is taken only when no bay of the job's own type is free: spending the quick lane on a
// general job while a general bay sits idle would turn a later oil change away for nothing, and an
// oil change in a general bay while the quick lane is idle would do the same to a later brake job.
func freeBaysSQL(q BayQuery) (string, []any) {
	before, after := sideGapsSQL("a.service_bay_id = b.id", "a.id <> $8", slotStart, slotEnd, "$6::timestamptz", "$7::timestamptz")
	return `
		SELECT id, name, bay_type FROM (
			SELECT b.id, b.name, b.bay_type, ` + before + ` AS gb, ` + after + ` AS ga
			FROM service_bays b
			WHERE b.dealership_id = $1
			  AND b.active
			  AND ` + bayFitsSQL("b.bay_type", "$2", "$5") + `
			  AND NOT EXISTS (
				SELECT 1 FROM appointments a
				WHERE a.service_bay_id = b.id
				  AND a.` + holdingStatusesSQL + `
				  AND a.id <> $8
				  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange($3::timestamptz, $4::timestamptz, '[)')
			  )
			  AND NOT ` + overrunHeldSQL("a.service_bay_id = b.id", "$8", "$4::timestamptz") + `
		) c
		ORDER BY CASE WHEN bay_type = $2 THEN 0 ELSE 1 END, ` + fitRankSQL([2]string{"gb", "ga"}) + `, name COLLATE "C", id`,
		[]any{q.DealershipID, int16(q.Required), q.Start, q.End, q.SoftUpgrade, q.Lo, q.Hi, q.Exclude}
}

// sideGapsSQL is the idle time, in seconds, a resource would keep before and after the window
// [start, end): from the later of lo and the end of its last hold before the window, and up to the
// earlier of hi and the start of its next hold after it. owner matches the resource's holds
// (alias a); skip leaves holds out (the appointment being moved; in a repair also the one leaving
// the resource). The resource is free for the window, so every hold lies wholly before or after
// it; the lookups are written as range overlaps so the GiST (resource, range) indexes serve them.
// booking.sideGaps is the Go twin (availability's recommended hint); keep both in step.
func sideGapsSQL(owner, skip, start, end, lo, hi string) (before, after string) {
	before = `EXTRACT(EPOCH FROM (` + start + ` - GREATEST(` + lo + `, (
			SELECT max(a.end_at) FROM appointments a
			WHERE ` + owner + ` AND a.` + holdingStatusesSQL + ` AND ` + skip + `
			  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange(` + lo + `, ` + start + `, '[)')))))`
	after = `EXTRACT(EPOCH FROM (LEAST(` + hi + `, (
			SELECT min(a.start_at) FROM appointments a
			WHERE ` + owner + ` AND a.` + holdingStatusesSQL + ` AND ` + skip + `
			  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange(` + end + `, ` + hi + `, '[)'))) - ` + end + `))`
	return before, after
}

// fitRankSQL is the best-fit ORDER BY key of one or more resources' side gaps (sideGapsSQL, as
// before/after pairs; a repair ranks the freed resource and the move's target together): the total
// idle time left around the windows, smallest first — the window then fills a hole instead of
// splitting a long free stretch in two. booking.gaps.fit is the Go twin.
//
// Measured, not adopted: ranking first by "dead" minutes — a side gap shorter than the shortest job
// the resource can take (a technician's shortest qualified service; a general bay 30, the express
// spill) — then by the total. Every harness came out identical (docs/system-design.md, Measured):
// bays never have dead gaps on the 30-minute grid, and two technicians tie on level and spare
// skills with different dead minutes too rarely to change an outcome.
func fitRankSQL(pairs ...[2]string) string {
	terms := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		terms = append(terms, p[0], p[1])
	}
	return "(" + strings.Join(terms, " + ") + ")"
}

// slotStart and slotEnd are the slot's placeholders in the allocation queries.
const slotStart, slotEnd = "$3::timestamptz", "$4::timestamptz"

type TechQuery struct {
	DealershipID uuid.UUID
	Skills       []domain.TechnicianSkill
	MinLevel     domain.SkillLevel
	Slot
}

// freeTechniciansSQL lists technicians who hold every required skill at the minimum level, are on
// duty (shift, break, absence) and free for the slot (overrunHeldSQL included), best candidate
// first. Three measures rank them:
//   - spare: skills held beyond the job's (an oil change on the one technician who also does
//     brakes can turn a later brake job away);
//   - gap: best fit on the technician's own day, as for bays (fitRankSQL), with the shift's start
//     and end and its break as further edges (techStretchSQL) — technicians are as often the binding resource as
//     bays, and spreading jobs across them (least loaded first) splits every timeline into
//     stretches too short for a long job;
//   - load: booked minutes that day (a 3h major weighs six oil changes).
//
// Waiter or not: the lowest skill level that still qualifies, then the fewest spare skills (senior
// and versatile technicians stay free for the work only they can do), then best fit, then load,
// then name (byte order) and id — booking.availRange.pickTech restates this order exactly.
// Waiters used to get the least-loaded, most senior technician (the customer is in the lounge):
// over 400 random streams with half the requests waiters that booked 164970 labour minutes with 33
// refusals an offline re-assignment would have fitted, best fit 165630 with 31 — spreading waiter
// jobs fragments the timelines and spends the senior technicians the next major needs.
func freeTechniciansSQL(q TechQuery) (string, []any) {
	lo, hi := techStretchSQL("sh", slotStart, slotEnd, "$9::timestamptz", "$10::timestamptz")
	before, after := sideGapsSQL("a.technician_id = t.id", "a.id <> $8", slotStart, slotEnd, lo, hi)
	return `
		SELECT id, name, skill_level, load FROM (
			SELECT t.id, t.name, t.skill_level, (
				SELECT COALESCE(sum(EXTRACT(EPOCH FROM (a.end_at - a.start_at)) / 60), 0)::int
				FROM appointments a
				WHERE a.technician_id = t.id
				  AND a.` + holdingStatusesSQL + `
				  AND a.id <> $8
				  AND a.start_at >= $6::timestamptz
				  AND a.start_at < $7::timestamptz
			) AS load, (
				SELECT count(*) FROM technician_skills sk
				WHERE sk.technician_id = t.id AND sk.skill <> ALL ($2::smallint[])
			) AS spare, ` + before + ` AS gb, ` + after + ` AS ga
			FROM technicians t
			JOIN dealerships d ON d.id = t.dealership_id
			LEFT JOIN technician_shifts sh
			  ON sh.technician_id = t.id AND sh.weekday = EXTRACT(ISODOW FROM ($3::timestamptz AT TIME ZONE d.timezone))::smallint
			WHERE t.dealership_id = $1
			  AND t.active
			  AND t.skill_level >= $5
			  AND ` + hasSkillsSQL("t", "$2") + `
			  AND ` + technicianOnDutySQL("t", "d.timezone", "$3::timestamptz", "$4::timestamptz") + `
			  AND NOT EXISTS (
				SELECT 1 FROM appointments a
				WHERE a.technician_id = t.id
				  AND a.` + holdingStatusesSQL + `
				  AND a.id <> $8
				  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange($3::timestamptz, $4::timestamptz, '[)')
			  )
			  AND NOT ` + overrunHeldSQL("a.technician_id = t.id", "$8", "$4::timestamptz") + `
		) c
		ORDER BY skill_level, spare, ` + fitRankSQL([2]string{"gb", "ga"}) + `, load, name COLLATE "C", id`,
		[]any{q.DealershipID, skillCodes(q.Skills), q.Start, q.End, q.MinLevel, q.DayStart, q.DayEnd, q.Exclude, q.Lo, q.Hi}
}

// techStretchSQL bounds the idle time around a technician's window [start, end): the stretch
// [lo, hi] (the slot's bookable stretch) narrowed to the technician's shift on start's local
// weekday (alias sh, LEFT JOINed: NULL when they work the opening hours) and to its break on the
// window's side, as instants on start's local date in the dealership's timezone (alias d). Clamped
// to the window: a shift clock inside a skipped hour maps to an instant an hour later. The Go twin
// is booking.roster.stretch; keep both in step.
func techStretchSQL(sh, start, end, lo, hi string) (string, string) {
	localDate := `(` + start + ` AT TIME ZONE d.timezone)::date`
	edge := func(clock string) string {
		return `((` + localDate + ` + ` + sh + `.` + clock + `) AT TIME ZONE d.timezone)`
	}
	l := `LEAST(` + start + `, GREATEST(` + lo + `, ` + edge("start_time") + `,
			CASE WHEN ` + sh + `.break_end <= (` + start + ` AT TIME ZONE d.timezone)::time THEN ` + edge("break_end") + ` END))`
	h := `GREATEST(` + end + `, LEAST(` + hi + `, ` + edge("end_time") + `,
			CASE WHEN ` + sh + `.break_start >= (` + end + ` AT TIME ZONE d.timezone)::time THEN ` + edge("break_start") + ` END))`
	return l, h
}

func scanFreeTechnician(row pgx.Row) (domain.Technician, error) {
	var item domain.Technician
	err := row.Scan(&item.ID, &item.Name, &item.SkillLevel, &item.Load)
	return item, err
}

// TechCheck is what LockBookableTechnician re-verifies for one technician and window.
type TechCheck struct {
	TechnicianID uuid.UUID
	DealershipID uuid.UUID
	Skills       []domain.TechnicianSkill
	MinLevel     domain.SkillLevel
	Start, End   time.Time
	// NoWait takes the share lock SKIP LOCKED: a technician row a roster writer holds (FOR UPDATE)
	// fails the check with ErrTechnicianLocked at once instead of waiting for the writer. For a
	// booking holding the dealership-day lock in a savepoint it can give back (booking.placement):
	// waiting there would stall every booking of that day behind one roster edit.
	NoWait bool
}

// ErrTechnicianLocked is a NoWait TechCheck finding the technician row locked by a concurrent
// writer (a roster edit, deactivation, skill change). Nothing is known about the technician; the
// caller decides whether to wait for it some other way.
var ErrTechnicianLocked = errors.New("technician row is locked by a concurrent writer")

// LockBookableTechnician re-checks, under a share lock on the technician row, that the technician
// still belongs to the dealership, is active, holds the skills at the level, and is on duty for
// the window. It closes the race with a concurrent admin change (deactivation, downgrade, roster
// or absence edit — all of which lock the row FOR UPDATE): either the admin waits for this booking
// (and its stranded check then sees it), or this booking sees the new state.
//
// Two statements on purpose: when the lock had to wait for an admin, READ COMMITTED re-checks
// (EvalPlanQual) only the locked row itself, while subqueries on technician_shifts/absences would
// still read the statement's older snapshot. The second statement takes a fresh snapshot after
// the lock is held, so it sees any roster change committed before it. Both are pipelined in one
// round trip (see batch.go): the server still runs the second only after the first, lock wait
// included, has finished, so its snapshot is just as fresh as in a separate round trip. When the
// first finds no row (other dealership, inactive, under-levelled) the second's answer is ignored.
// With c.NoWait the first never waits (see TechCheck.NoWait and ErrTechnicianLocked); the second
// still runs in its own snapshot, now taken while the share lock is held.
func (t *Tx) LockBookableTechnician(ctx context.Context, c TechCheck) (bool, error) {
	var v techVerdict
	b := &pgx.Batch{}
	queueTechCheck(b, c, &v)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return false, err
	}
	return v.result()
}

// ShareLockTechnician waits for the technician's share lock, held until the TX ends, checking
// nothing: a booking that gave its day lock back on meeting the technician mid-edit
// (ErrTechnicianLocked) waits for the roster writer here, outside the day's queue, so its next
// LockBookableTechnician on it — done again, in full — cannot wait.
func (t *Tx) ShareLockTechnician(ctx context.Context, id uuid.UUID) error {
	_, err := t.tx.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR SHARE`, id)
	return err
}

// candidateSavepoint names the savepoint LockBookableTechnicianSavepoint opens. One is open at a
// time: a candidate's savepoint is rolled back (and released) before the next one opens.
const candidateSavepoint = "alloc_candidate"

// LockBookableTechnicianSavepoint is LockBookableTechnician followed, in the same round trip, by the
// savepoint the allocation candidate then writes in (returned; its Rollback also releases it, so
// statements after it run on the TX itself). The share lock is taken before the savepoint, so
// rolling the savepoint back keeps it. The savepoint is opened whatever the answer: when the
// technician is not bookable the caller rolls it back. For a booking that already holds the day
// lock, this saves the separate SAVEPOINT round trip of every first try of a technician. With
// c.NoWait a locked technician returns ErrTechnicianLocked and no savepoint handle: the one opened
// is discarded with the savepoint the caller gives its day lock back with.
func (t *Tx) LockBookableTechnicianSavepoint(ctx context.Context, c TechCheck) (bool, *Tx, error) {
	var v techVerdict
	b := &pgx.Batch{}
	queueTechCheck(b, c, &v)
	b.Queue(`SAVEPOINT ` + candidateSavepoint).Exec(ignoreTag)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return false, nil, err
	}
	ok, err := v.result()
	if err != nil {
		return false, nil, err
	}
	return ok, &Tx{tx: t.tx, st: t.st, events: t.events, nested: true, savepoint: candidateSavepoint}, nil
}

// techVerdict is what queueTechCheck's two statements found.
type techVerdict struct {
	skipped bool // NoWait: the row was locked by someone else, nothing was checked
	locked  bool // share lock held and the row qualifies (dealership, active, level)
	onDuty  bool // skills and roster, read in a snapshot taken after the lock
}

func (v techVerdict) result() (bool, error) {
	if v.skipped {
		return false, ErrTechnicianLocked
	}
	return v.locked && v.onDuty, nil
}

// queueTechCheck queues LockBookableTechnician's two statements. The NoWait lock returns the row
// whatever it holds, so "locked by someone else" (no row: technicians are never deleted) is told
// apart from "not bookable"; the qualifying test then reads the locked row's latest version.
func queueTechCheck(b *pgx.Batch, c TechCheck, v *techVerdict) {
	if c.NoWait {
		b.Queue(`
			SELECT dealership_id = $2 AND active AND skill_level >= $3 FROM technicians
			WHERE id = $1
			FOR SHARE SKIP LOCKED`, c.TechnicianID, c.DealershipID, c.MinLevel).QueryRow(func(row pgx.Row) error {
			err := row.Scan(&v.locked)
			if errors.Is(err, pgx.ErrNoRows) {
				v.skipped = true
				return nil
			}
			return err
		})
	} else {
		b.Queue(`
			SELECT true FROM technicians
			WHERE id = $1 AND dealership_id = $2 AND active AND skill_level >= $3
			FOR SHARE`, c.TechnicianID, c.DealershipID, c.MinLevel).QueryRow(func(row pgx.Row) error {
			return scanOptionalBool(row, &v.locked)
		})
	}
	b.Queue(`
		SELECT `+hasSkillsSQL("t", "$2")+`
		   AND `+technicianOnDutySQL("t", "d.timezone", "$3::timestamptz", "$4::timestamptz")+`
		FROM technicians t
		JOIN dealerships d ON d.id = t.dealership_id
		WHERE t.id = $1`, c.TechnicianID, skillCodes(c.Skills), c.Start, c.End).QueryRow(func(row pgx.Row) error {
		return scanOptionalBool(row, &v.onDuty)
	})
}

// scanOptionalBool scans a single boolean column into dst; no row leaves dst false.
func scanOptionalBool(row pgx.Row, dst *bool) error {
	if err := row.Scan(dst); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return nil
}

// ReadBay reads one bay (a reassign target) with its dealership and active flag, in the TX. A plain
// read: no code path updates or deletes service_bays rows (bays are only added; type, dealership,
// and active never change after insert), so there is no writer a share lock would have to wait
// for. A bay deactivation or retype, if one is ever added, must lock the bay FOR UPDATE and this
// read must go back to FOR SHARE.
func (t *Tx) ReadBay(ctx context.Context, id uuid.UUID) (domain.Bay, uuid.UUID, bool, error) {
	var bay domain.Bay
	var bayType int16
	var dealershipID uuid.UUID
	var active bool
	err := t.tx.QueryRow(ctx, `
		SELECT id, name, bay_type, dealership_id, active FROM service_bays WHERE id = $1`, id).
		Scan(&bay.ID, &bay.Name, &bayType, &dealershipID, &active)
	if err != nil {
		return domain.Bay{}, uuid.Nil, false, missing(err, "service bay not found")
	}
	bay.BayType = domain.BayType(bayType)
	return bay, dealershipID, active, nil
}

// ArrivalLock names what LockArrival serialises: one vehicle (skipped when VehicleID is nil), and
// one dealership's local day — the day containing At in the dealership's timezone — or several.
// Count asks for the holding arrivals of the staging window [From, To), ignoring Exclude (a moving
// booking). Savepoint also opens the first allocation candidate's savepoint in the same round trip
// (returned by LockArrival), so a booking that wins on its first candidate spends one round trip
// less while it holds the day lock.
type ArrivalLock struct {
	VehicleID    uuid.UUID
	DealershipID uuid.UUID
	At           time.Time
	// Until, when set, is the end of a hold that may run past At's local midnight (an extended or
	// overrunning job): the day of its last instant is locked too.
	Until time.Time
	// Leaving, when set, is the start of the hold a reschedule moves away from, on another day than
	// At: that day is locked too, since the move rewrites its GiST entry there, so every write of a
	// day's holds queues on that day's lock (without it, e.g. an extension into the vacated window
	// waited on the move's uncommitted entry instead, outside the queue).
	Leaving   time.Time
	From, To  time.Time
	Exclude   uuid.UUID
	Count     bool
	Savepoint bool
	// Try takes the day lock only if no one holds it, never waiting for it (pg_try_advisory_xact_lock;
	// the vehicle lock is still waited for, before it). Only for a hold of one local day
	// (SingleDay): trying several days could take a later day and then wait for an earlier one, out
	// of order. LockArrivalAndCandidatesRevocably reports whether it got the lock; when it did not,
	// the count it read is stale.
	Try bool
}

// SingleDay reports whether the hold locks one local day only (no Until on a later day, no
// Leaving), so the day lock may be tried (Try).
func (l ArrivalLock) SingleDay() bool {
	return len(l.days()) == 1
}

// days lists the instants whose local days LockArrival locks, earliest first. A later instant never
// lies on an earlier local day, so this is day order: every writer takes its days in ascending
// order and two of them can never wait on each other's days (re-taking a day is a no-op).
func (l ArrivalLock) days() []time.Time {
	days := []time.Time{l.At}
	if !l.Until.IsZero() && l.Until.After(l.At) {
		days = append(days, l.Until.Add(-time.Microsecond))
	}
	if !l.Leaving.IsZero() {
		days = append(days, l.Leaving)
	}
	slices.SortFunc(days, time.Time.Compare)
	return days
}

// firstCandidateSavepoint names the savepoint LockArrival opens (pgx names its own sp_N).
const firstCandidateSavepoint = "alloc_first"

// dealershipDayLockSQL takes the advisory lock of dealership $1's local day containing $2. The key
// is derived in SQL from the dealership's own timezone, so every caller (booking, reschedule,
// reinstate, reassign, extend, early start) agrees on it without reading the dealership first.
const dealershipDayLockSQL = `
	SELECT pg_advisory_xact_lock(hashtextextended('dealer-day|' || d.id::text || '|' || ($2::timestamptz AT TIME ZONE d.timezone)::date::text, 0))
	FROM dealerships d WHERE d.id = $1`

// dealershipDayTryLockSQL is dealershipDayLockSQL without waiting: it takes the same lock only if
// it is free and reports whether it did (ArrivalLock.Try).
const dealershipDayTryLockSQL = `
	SELECT pg_try_advisory_xact_lock(hashtextextended('dealer-day|' || d.id::text || '|' || ($2::timestamptz AT TIME ZONE d.timezone)::date::text, 0))
	FROM dealerships d WHERE d.id = $1`

// LockArrival takes, in this order, the advisory locks of the vehicle and of the dealership's
// local day, then (when Count) counts the staging window's holding arrivals — one round trip.
//
// The day lock queues every writer of that dealership-day before its GiST writes: bays and
// technicians belong to one dealership, and two holds can only collide within one day (opening
// hours never cross midnight), so it orders exactly the writers that can conflict, and nothing
// else (other days and dealerships run in parallel). Without it, two bookings whose windows overlap
// can each insert first and then wait on the other's uncommitted entry on another constraint (bay
// vs technician): a deadlock Postgres only breaks after deadlock_timeout, while every writer queued
// behind them runs into lock_timeout. A lock per staging window was not enough — overlapping jobs
// arriving in different windows (a 3-hour service at 09:00, a brake job at 10:30) still deadlocked.
// It also makes the staging count + insert atomic (the window lies inside the day). The vehicle lock
// does the same for one car's bookings across dealerships. GiST stays the double-book guarantee:
// these locks only order the writers.
//
// The count is its own statement queued after the locks: under READ COMMITTED it takes its snapshot
// once they are granted, so it sees every booking that committed into the window meanwhile (see
// batch.go).
//
// With l.Savepoint the returned Tx is that savepoint (nil otherwise): its Rollback undoes and
// releases it, so statements after it run on the TX itself again (the technician re-checks must not
// run inside a savepoint that may be rolled back, see booking.allocate).
func (t *Tx) LockArrival(ctx context.Context, l ArrivalLock) (int, *Tx, error) {
	var n int
	b := &pgx.Batch{}
	l.Try = false
	queueArrivalLock(b, l, &n, nil)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return 0, nil, err
	}
	if !l.Savepoint {
		return n, nil, nil
	}
	return n, &Tx{tx: t.tx, st: t.st, events: t.events, nested: true, savepoint: firstCandidateSavepoint}, nil
}

// queueArrivalLock queues LockArrival's statements (the count into *n when l.Count). With l.Try the
// day lock is only tried (one day at most, see ArrivalLock.Try) and *held reports whether it was
// taken; without it the locks are waited for and *held is left alone.
func queueArrivalLock(b *pgx.Batch, l ArrivalLock, n *int, held *bool) {
	if l.VehicleID != uuid.Nil {
		b.Queue(advisoryLockSQL, advisoryKey("vehicle|"+l.VehicleID.String())).Exec(ignoreTag)
	}
	if l.Try {
		b.Queue(dealershipDayTryLockSQL, l.DealershipID, l.At).QueryRow(func(row pgx.Row) error {
			return row.Scan(held)
		})
	} else {
		for _, day := range l.days() {
			b.Queue(dealershipDayLockSQL, l.DealershipID, day).Exec(ignoreTag)
		}
	}
	if l.Count {
		b.Queue(`
			SELECT count(*)::int FROM appointments
			WHERE dealership_id = $1
			  AND `+holdingStatusesSQL+`
			  AND id <> $4
			  AND start_at >= $2::timestamptz
			  AND start_at < $3::timestamptz`, l.DealershipID, l.From, l.To, l.Exclude).QueryRow(func(row pgx.Row) error {
			return row.Scan(n)
		})
	}
	if l.Savepoint {
		b.Queue(`SAVEPOINT ` + firstCandidateSavepoint).Exec(ignoreTag)
	}
}

// LockDealershipDay takes the vehicle (when not nil) and dealership-day locks of LockArrival
// without counting, for the hold [at, until): writers that change an existing hold in place
// (reassign, extend, early start, the overrun sweep) queue with the bookings of its day(s) instead
// of deadlocking with them.
func (t *Tx) LockDealershipDay(ctx context.Context, dealershipID, vehicleID uuid.UUID, at, until time.Time) error {
	_, _, err := t.LockArrival(ctx, ArrivalLock{VehicleID: vehicleID, DealershipID: dealershipID, At: at, Until: until})
	return err
}

const advisoryLockSQL = `SELECT pg_advisory_xact_lock($1)`

// advisoryKey hashes a lock name to the bigint key of pg_advisory_xact_lock.
func advisoryKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) //nolint:gosec // G115: hash bits reinterpreted, overflow intended
}

// unlockTimeout bounds releasing a session advisory lock taken by WithTryLock.
const unlockTimeout = 2 * time.Second

// WithTryLock runs fn while this session holds the session-level advisory lock name, on a pool
// connection kept for the duration, and reports whether fn ran: when another session holds the
// lock, fn is skipped (that holder is doing the work). Background jobs that every instance would
// otherwise repeat (sweeps, purges) use it so one instance runs them per round. If the unlock
// fails the connection is closed, which ends the session and drops the lock with it.
func (s *Store) WithTryLock(ctx context.Context, name string, fn func()) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	key := advisoryKey("session|" + name)
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
			_ = conn.Conn().Close(uctx)
		}
	}()
	fn()
	return true, nil
}
