package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// One-move repair. When no bay (or no technician) is free for a slot, a booking may still fit by
// moving ONE other appointment off a bay (technician) that fits the new job, to another bay
// (technician) that fits the moved one, in the same window: the moved customer sees nothing change
// (same time, same car), only the dispatcher's board. Greedy allocation without it refuses work an
// offline re-assignment would have fitted (the regret TestAllocationRegret… measures).
//
// What may move (booking.movable is the Go twin; keep both in step): a confirmed appointment (not
// checked in, so no car is in the bay yet) that has not started (start_at > now), and that is the
// only hold of the freed resource overlapping the slot. Where it may go: an active bay of the same
// dealership that fits its booked requirement (bayFitsSQL, with its own waiter soft upgrade), or an
// active technician holding its booked skills at its level, on duty for its window — the rules of
// Reassign — and no scarcer for it than the technician it leaves (techRepairSQL), free for its
// window, not held by flagged overrunning work. Holds never cross midnight
// while confirmed, so the moved appointment lies in the slot's local day and the booking's
// dealership-day lock covers it.

// RepairQuery is the slot a booking found nothing free for, and its requirement.
type RepairQuery struct {
	DealershipID uuid.UUID
	Slot         // Start, End, Lo, Hi, Exclude (the appointment being rescheduled)
	Now          time.Time
	BayType      domain.BayType
	SoftUpgrade  bool
	Skills       []domain.TechnicianSkill
	MinLevel     domain.SkillLevel
}

// Repair is one move: appointment AppointmentID leaves From (the bay or technician the booking then
// takes) for To. Start, End, Skills and MinLevel are the moved appointment's window and booked
// technician requirement, so the caller can re-check To under lock (LockBookableTechnician).
type Repair struct {
	AppointmentID uuid.UUID
	From, To      uuid.UUID
	Start, End    time.Time
	Skills        []domain.TechnicianSkill
	MinLevel      domain.SkillLevel
}

// movableSQL matches appointment alias x overlapping the slot [$3, $4) that a booking may move
// (see above); $6 is the appointment being rescheduled, $7 now.
var movableSQL = `x.status = ` + statusConfirmedSQL + ` AND x.id <> $6 AND x.start_at > $7::timestamptz
	  AND tstzrange(x.start_at, x.end_at, '[)') && tstzrange($3::timestamptz, $4::timestamptz, '[)')`

// soleHoldSQL is true when x is the only hold of the resource (owner, on alias o) overlapping the
// slot and no flagged overrun holds it for the slot (overrunHeldSQL).
func soleHoldSQL(ownerO, ownerA string) string {
	return `NOT EXISTS (
			SELECT 1 FROM appointments o
			WHERE ` + ownerO + ` AND o.` + holdingStatusesSQL + ` AND o.id <> $6 AND o.id <> x.id
			  AND tstzrange(o.start_at, o.end_at, '[)') && tstzrange($3::timestamptz, $4::timestamptz, '[)'))
		  AND NOT ` + overrunHeldSQL(ownerA, "$6", "$4::timestamptz")
}

// freeForMovedSQL is true when the target resource (owner, on aliases o and a) is free for x's
// window: no other hold overlaps it and no flagged overrun holds it before x ends. The appointment
// being rescheduled ($6) still counts here: x moves before it does, so its current hold is still
// in GiST when x's UPDATE runs (booking.availInputs.excludedHoldsBay and excludedHoldsTechnician
// are the Go twins).
func freeForMovedSQL(ownerO, ownerA string) string {
	return `NOT EXISTS (
			SELECT 1 FROM appointments o
			WHERE ` + ownerO + ` AND o.` + holdingStatusesSQL + `
			  AND tstzrange(o.start_at, o.end_at, '[)') && tstzrange(x.start_at, x.end_at, '[)'))
		  AND NOT ` + overrunHeldSQL(ownerA, "$6", "x.end_at")
}

// movedUpgradeSQL is x's own waiter soft upgrade (booking.Requirement.SoftUpgrade on its snapshot).
var movedUpgradeSQL = `(x.is_waiter AND x.required_bay_type = ` + bayGeneralSQL + ` AND d.express_max_minutes > 0
	  AND x.booked_duration_minutes <= d.express_max_minutes)`

// Repairs are ranked by fit like allocation (fitRankSQL), on both resources together: the new
// job's idle time on the freed resource once x has left it (the slot's stretch, $8–$9) plus x's on
// its target (inside the same stretch: x overlaps the slot and, like any confirmed hold, never
// crosses lunch — clamped all the same, in case the hours changed since it was booked). So the
// move frees the resource the new job fills best and lands x where it fills a hole, instead of
// choosing by name. Technicians' stretches also end at their shift and break (techStretchSQL), as
// in allocation.

// freedGaps is the new job's side gaps on the freed resource (owner on alias a) once x has left.
func freedGaps(owner, lo, hi string) (string, string) {
	return sideGapsSQL(owner, "a.id <> $6 AND a.id <> x.id", "$3::timestamptz", "$4::timestamptz", lo, hi)
}

// targetGaps is x's side gaps on its target (owner on alias a).
func targetGaps(owner, lo, hi string) (string, string) {
	return sideGapsSQL(owner, "a.id <> $6", "x.start_at", "x.end_at", lo, hi)
}

// xLo and xHi are x's bookable stretch: the slot's, widened to x's window.
const xLo, xHi = "LEAST($8::timestamptz, x.start_at)", "GREATEST($9::timestamptz, x.end_at)"

// repairFitSQL is the LATERAL row of a repair candidate's side gaps: fb/fa for the freed resource,
// tb/ta for the target.
func repairFitSQL(freedBefore, freedAfter, targetBefore, targetAfter string) string {
	return `CROSS JOIN LATERAL (SELECT ` + freedBefore + ` AS fb, ` + freedAfter + ` AS fa, ` +
		targetBefore + ` AS tb, ` + targetAfter + ` AS ta) g`
}

// repairArgs is a repair query's arguments: the stretch ($8, $9) only ranks, so an unranked query
// does not take it.
func repairArgs(ranked bool, args ...any) []any {
	if !ranked {
		return args[:7]
	}
	return args
}

// optional is sql when on, else nothing.
func optional(on bool, sql string) string {
	if on {
		return sql
	}
	return ""
}

// repairRankSQL is the fit key of a repair candidate (repairFitSQL's columns).
func repairRankSQL() string {
	return fitRankSQL([2]string{"g.fb", "g.fa"}, [2]string{"g.tb", "g.ta"})
}

// bayRepairSQL finds a bay b that fits the new job and holds only x in the slot, and a bay nb that
// fits x and is free for x's window. Ranked: a freed bay of the job's own type first, then a target
// of x's own type (no fallback spent on it), then fit (repairRankSQL), then the names (byte order)
// and x's id. Unranked (RepairPossible's probe) it leaves the ranking out.
func bayRepairSQL(q RepairQuery, ranked bool) (string, []any) {
	fb, fa := freedGaps("a.service_bay_id = b.id", "$8::timestamptz", "$9::timestamptz")
	tb, ta := targetGaps("a.service_bay_id = nb.id", xLo, xHi)
	return `
		SELECT x.id, b.id, nb.id, x.start_at, x.end_at, x.required_skills, x.min_skill_level
		FROM service_bays b
		JOIN dealerships d ON d.id = b.dealership_id
		JOIN appointments x ON x.service_bay_id = b.id
		JOIN service_bays nb ON nb.dealership_id = b.dealership_id AND nb.active AND nb.id <> b.id
		` + optional(ranked, repairFitSQL(fb, fa, tb, ta)) + `
		WHERE b.dealership_id = $1 AND b.active
		  AND ` + bayFitsSQL("b.bay_type", "$2", "$5") + `
		  AND ` + movableSQL + `
		  AND ` + soleHoldSQL("o.service_bay_id = b.id", "a.service_bay_id = b.id") + `
		  AND ` + bayFitsSQL("nb.bay_type", "x.required_bay_type", movedUpgradeSQL) + `
		  AND ` + freeForMovedSQL("o.service_bay_id = nb.id", "a.service_bay_id = nb.id") + `
		` + optional(ranked, `ORDER BY b.bay_type = $2 DESC, nb.bay_type = x.required_bay_type DESC, `+repairRankSQL()+`,
		         b.name COLLATE "C", nb.name COLLATE "C", x.id`) + `
		LIMIT 1`,
		repairArgs(ranked, q.DealershipID, int16(q.BayType), q.Start, q.End, q.SoftUpgrade, q.Exclude, q.Now, q.Lo, q.Hi)
}

// spareSkillsSQL counts technician alias t's skills outside the smallint[] expression need.
func spareSkillsSQL(t, need string) string {
	return `(SELECT count(*) FROM technician_skills sk WHERE sk.technician_id = ` + t + `.id AND sk.skill <> ALL (` + need + `))`
}

// techRepairSQL finds a technician t qualified and on duty for the new job who holds only x in the
// slot, and a technician nt who holds x's booked skills at its level, is on duty for x's window and
// free then, and is no scarcer than t for x: no higher level and no more skills beyond x's (moving
// x onto a more senior or versatile technician would spend exactly the capacity allocation keeps
// free for work only they can do — on random streams it cost more later refusals than it saved).
// Ranked like allocation on both sides (freeTechniciansSQL): the freed technician by level and
// spare skills for the new job, the target by level and spare skills for x (junior and least
// versatile first: senior technicians stay free for the work only they can do), then fit
// (repairRankSQL, each technician's stretch bounded by their shift and break: sh for t on the
// slot's weekday, nsh for nt on x's), then the names (byte order) and x's id. Unranked
// (RepairPossible's probe) it leaves the ranking out.
func techRepairSQL(q RepairQuery, ranked bool) (string, []any) {
	lo, hi := techStretchSQL("sh", "$3::timestamptz", "$4::timestamptz", "$8::timestamptz", "$9::timestamptz")
	fb, fa := freedGaps("a.technician_id = t.id", lo, hi)
	nlo, nhi := techStretchSQL("nsh", "x.start_at", "x.end_at", xLo, xHi)
	tb, ta := targetGaps("a.technician_id = nt.id", nlo, nhi)
	return `
		SELECT x.id, t.id, nt.id, x.start_at, x.end_at, x.required_skills, x.min_skill_level
		FROM technicians t
		JOIN dealerships d ON d.id = t.dealership_id
		JOIN appointments x ON x.technician_id = t.id
		JOIN technicians nt ON nt.dealership_id = t.dealership_id AND nt.active AND nt.id <> t.id
		LEFT JOIN technician_shifts sh
		  ON sh.technician_id = t.id AND sh.weekday = EXTRACT(ISODOW FROM ($3::timestamptz AT TIME ZONE d.timezone))::smallint
		LEFT JOIN technician_shifts nsh
		  ON nsh.technician_id = nt.id AND nsh.weekday = EXTRACT(ISODOW FROM (x.start_at AT TIME ZONE d.timezone))::smallint
		` + optional(ranked, repairFitSQL(fb, fa, tb, ta)) + `
		WHERE t.dealership_id = $1 AND t.active AND t.skill_level >= $5
		  AND ` + hasSkillsSQL("t", "$2") + `
		  AND ` + technicianOnDutySQL("t", "d.timezone", "$3::timestamptz", "$4::timestamptz") + `
		  AND ` + movableSQL + `
		  AND ` + soleHoldSQL("o.technician_id = t.id", "a.technician_id = t.id") + `
		  AND nt.skill_level >= x.min_skill_level
		  AND ` + hasSkillsSQL("nt", "x.required_skills") + `
		  AND nt.skill_level <= t.skill_level
		  AND ` + spareSkillsSQL("nt", "x.required_skills") + ` <= ` + spareSkillsSQL("t", "x.required_skills") + `
		  AND ` + technicianOnDutySQL("nt", "d.timezone", "x.start_at", "x.end_at") + `
		  AND ` + freeForMovedSQL("o.technician_id = nt.id", "a.technician_id = nt.id") + `
		` + optional(ranked, `ORDER BY t.skill_level, `+spareSkillsSQL("t", "$2::smallint[]")+`, nt.skill_level, `+spareSkillsSQL("nt", "x.required_skills")+`,
		         `+repairRankSQL()+`, t.name COLLATE "C", nt.name COLLATE "C", x.id`) + `
		LIMIT 1`,
		repairArgs(ranked, q.DealershipID, skillCodes(q.Skills), q.Start, q.End, q.MinLevel, q.Exclude, q.Now, q.Lo, q.Hi)
}

// lockMovedSQL locks the chosen appointment without waiting: the caller holds the dealership-day
// lock, and a writer that holds this row may be queued for that lock (Reassign, Extend, check-in:
// row first, then day), so waiting here would deadlock; a locked row is skipped instead (that
// writer is changing it anyway).
const lockMovedSQL = ` FOR NO KEY UPDATE OF x SKIP LOCKED`

func scanRepair(row pgx.Row) (Repair, error) {
	var r Repair
	var skills []int16
	err := row.Scan(&r.AppointmentID, &r.From, &r.To, &r.Start, &r.End, &skills, &r.MinLevel)
	r.Skills = make([]domain.TechnicianSkill, len(skills))
	for i, sk := range skills {
		r.Skills[i] = domain.TechnicianSkill(sk)
	}
	return r, err
}

// RepairPossible probes, without locks, whether a bay move (when bays) and a technician move (when
// techs) exist for q: a booking refused with nothing free asks this before taking the
// dealership-day lock, so a refusal no move could help costs no lock.
func (t *Tx) RepairPossible(ctx context.Context, q RepairQuery, bays, techs bool) (bool, error) {
	bayOK, techOK := !bays, !techs
	b := &pgx.Batch{}
	if bays {
		sql, args := bayRepairSQL(q, false)
		b.Queue(`SELECT EXISTS (`+sql+`)`, args...).QueryRow(func(row pgx.Row) error { return row.Scan(&bayOK) })
	}
	if techs {
		sql, args := techRepairSQL(q, false)
		b.Queue(`SELECT EXISTS (`+sql+`)`, args...).QueryRow(func(row pgx.Row) error { return row.Scan(&techOK) })
	}
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return false, err
	}
	return bayOK && techOK, nil
}

// LockRepairs finds a bay move for q (bayRepairSQL, when bays) and a technician move
// (techRepairSQL, when techs) in one round trip — the two searches are independent — and
// row-locks the appointment each moves (lockMovedSQL; both may pick the same one, which this TX
// then simply holds). A nil move is none found. The caller holds the dealership-day lock.
func (t *Tx) LockRepairs(ctx context.Context, q RepairQuery, bays, techs bool) (bayMove, techMove *Repair, err error) {
	b := &pgx.Batch{}
	if bays {
		sql, args := bayRepairSQL(q, true)
		queueRepair(b, &bayMove, sql, args)
	}
	if techs {
		sql, args := techRepairSQL(q, true)
		queueRepair(b, &techMove, sql, args)
	}
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return nil, nil, err
	}
	return bayMove, techMove, nil
}

// queueRepair queues a repair search with its row lock; *dst stays nil when there is no move (an
// answer, not an error).
func queueRepair(b *pgx.Batch, dst **Repair, sql string, args []any) {
	b.Queue(sql+lockMovedSQL, args...).QueryRow(func(row pgx.Row) error {
		r, err := scanRepair(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		*dst = &r
		return nil
	})
}
