package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// querier is what the integrity queries need: a *pgx.Conn in a run, a pgx.Tx in tests (which roll
// their violating fixtures back).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// integrityCheck is one SQL query returning a text description per offending row (at most 5).
// Queries take the first args parameters of ($1 run key LIKE pattern, $2 job mixes text[]).
type integrityCheck struct {
	name string
	sql  string
	args int
}

// Check names, as reported (tests match on them).
const (
	checkBayOverlap     = "bay overlap"
	checkTechOverlap    = "technician overlap"
	checkVehicleOverlap = "vehicle overlap"
	checkStaging        = "staging over capacity"
	checkUnqualified    = "unqualified technician"
	checkOffDuty        = "technician off duty"
	checkBay            = "incompatible bay"
	checkEvents         = "CREATED outbox/history count"
	checkLines          = "job lines"
	checkDuration       = "booked duration"
	checkRequirement    = "requirement snapshot"
	checkRecord         = "appointment record"
)

// overlapSQL finds two holding appointments (status 1–3) overlapping on column col, anywhere in
// the database (the GiST constraints are global, so is this check).
func overlapSQL(col string) string {
	return fmt.Sprintf(`
		SELECT a.id::text || ' vs ' || b.id::text
		FROM appointments a
		JOIN appointments b ON a.id < b.id AND a.%[1]s = b.%[1]s
			AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange(b.start_at, b.end_at, '[)')
		WHERE a.status IN (`+holdingSQL+`) AND b.status IN (`+holdingSQL+`)
		LIMIT 5`, col)
}

// integrityChecks re-verify the run straight in Postgres, independent of what the API said:
//   - every appointment of the run is a complete record: its customer owns the car, and its bay and
//     technician belong to its dealership (the foreign keys guarantee they exist);
//   - no two holding appointments (status 1–3) overlap on a bay, technician, or vehicle;
//   - no staging window the run booked into holds more arrivals than its dealership's capacity
//     (confirmed + checked-in rows: an in-progress job started early was moved to [now, …) on
//     purpose without a staging re-check, so it may legitimately crowd a window);
//   - every appointment of the run went to a technician of its dealership holding each required
//     skill at >= min_skill_level and on duty for the window (onDutySQL: the documented roster rule,
//     written independently of the server's query), and to a bay of its dealership of the required
//     type, or express for a waiter's general job no longer than express_max_minutes (Assumption 4),
//     or general for an express job (the express spill);
//   - each of them has exactly one APPOINTMENT_CREATED outbox row and one history row;
//   - its booked snapshot is consistent (invariant 1): 1–5 appointment_lines numbered 1..n, the
//     first one the row's primary service, in the order of one of the run's job mixes; the window
//     (unless in progress or completed, which overruns may extend) is booked_duration_minutes long,
//     which is the sum of the lines; required_bay_type / required_skills / min_skill_level are what
//     the documented job-line rule derives from the lines' services (express < general < one
//     specialty, distinct sorted skills, highest level). The catalog is read as it is now: a run never edits it, so a
//     mismatch is a booking bug, not a catalog change.
func integrityChecks() []integrityCheck {
	return []integrityCheck{
		{checkRecord, `
			SELECT a.id::text || ': customer ' || a.customer_id::text || ' vehicle owner ' || v.customer_id::text
				|| ' bay dealership ' || b.dealership_id::text || ' technician dealership ' || t.dealership_id::text
				|| ' appointment dealership ' || a.dealership_id::text
			FROM appointments a
			JOIN vehicles v ON v.id = a.vehicle_id
			JOIN service_bays b ON b.id = a.service_bay_id
			JOIN technicians t ON t.id = a.technician_id
			JOIN customers c ON c.id = a.customer_id
			WHERE a.idempotency_key LIKE $1
			  AND (a.customer_id <> v.customer_id OR b.dealership_id <> a.dealership_id OR t.dealership_id <> a.dealership_id)
			LIMIT 5`, 1},
		{checkBayOverlap, overlapSQL("service_bay_id"), 0},
		{checkTechOverlap, overlapSQL("technician_id"), 0},
		{checkVehicleOverlap, overlapSQL("vehicle_id"), 0},
		{checkStaging, `
			WITH arrivals AS (
				SELECT d.id, d.staging_capacity AS cap, d.staging_window_minutes AS w,
					a.start_at AT TIME ZONE d.timezone AS local, a.idempotency_key LIKE $1 AS ours
				FROM appointments a JOIN dealerships d ON d.id = a.dealership_id
				WHERE a.status IN (` + notStartedSQL + `) AND d.staging_capacity > 0
			), windows AS (
				SELECT id, cap, ours, date_trunc('hour', local) + floor(extract(minute FROM local) / w) * w * interval '1 minute' AS win
				FROM arrivals
			)
			SELECT id::text || ' ' || win::text || ': ' || count(*) || ' > ' || cap
			FROM windows GROUP BY id, cap, win HAVING count(*) > cap AND bool_or(ours) LIMIT 5`, 1},
		{checkUnqualified, `
			SELECT a.id::text || ': technician ' || t.id::text || ' level ' || t.skill_level || ' (min ' || a.min_skill_level
				|| ') skills ' || coalesce((SELECT array_agg(sk.skill ORDER BY sk.skill) FROM technician_skills sk WHERE sk.technician_id = t.id)::text, '{}')
				|| ' (need ' || a.required_skills::text || ')'
				|| CASE WHEN t.dealership_id <> a.dealership_id THEN ' other dealership' ELSE '' END
			FROM appointments a
			JOIN technicians t ON t.id = a.technician_id
			WHERE a.idempotency_key LIKE $1
			  AND (t.skill_level < a.min_skill_level
			       OR t.dealership_id <> a.dealership_id
			       OR EXISTS (SELECT unnest(a.required_skills)
			                  EXCEPT SELECT sk.skill FROM technician_skills sk WHERE sk.technician_id = t.id))
			LIMIT 5`, 1},
		{checkOffDuty, `
			SELECT a.id::text || ': technician ' || t.id::text || ' off duty for ' || a.start_at::text || ' – ' || a.end_at::text
			FROM appointments a
			JOIN technicians t ON t.id = a.technician_id
			JOIN dealerships d ON d.id = a.dealership_id
			WHERE a.idempotency_key LIKE $1 AND a.status IN (` + notStartedSQL + `)
			  AND NOT ` + onDutySQL("t", "d.timezone", "a.start_at", "a.end_at") + `
			LIMIT 5`, 1},
		{checkBay, `
			SELECT a.id::text || ': bay ' || b.id::text || ' type ' || b.bay_type || ' for required ' || a.required_bay_type
				|| CASE WHEN a.is_waiter THEN ' (waiter, ' || a.booked_duration_minutes || ' min, express max ' || d.express_max_minutes || ')' ELSE '' END
				|| CASE WHEN b.dealership_id <> a.dealership_id THEN ' other dealership' ELSE '' END
			FROM appointments a
			JOIN service_bays b ON b.id = a.service_bay_id
			JOIN dealerships d ON d.id = a.dealership_id
			WHERE a.idempotency_key LIKE $1
			  AND (b.dealership_id <> a.dealership_id
			       OR NOT (b.bay_type = a.required_bay_type
			               OR (a.is_waiter AND a.required_bay_type = ` + bayGeneralSQL + ` AND b.bay_type = ` + bayExpressSQL + `
			                   AND d.express_max_minutes > 0 AND a.booked_duration_minutes <= d.express_max_minutes)
			               OR (a.required_bay_type = ` + bayExpressSQL + ` AND b.bay_type = ` + bayGeneralSQL + `)))
			LIMIT 5`, 1},
		{checkEvents, `
			SELECT id::text || ' outbox=' || outbox || ' history=' || history
			FROM (
				SELECT a.id,
					(SELECT count(*) FROM outbox_events o
					 WHERE o.aggregate_id = a.id AND o.event_type = ` + sqlCode(domain.EventAppointmentCreated) + `) AS outbox,
					(SELECT count(*) FROM appointment_history h
					 WHERE h.appointment_id = a.id AND h.event_type = ` + sqlCode(domain.EventAppointmentCreated) + `) AS history
				FROM appointments a
				WHERE a.idempotency_key LIKE $1) c
			WHERE outbox <> 1 OR history <> 1
			LIMIT 5`, 1},
		{checkLines, `
			SELECT a.id::text || ': ' || l.n || ' line(s) [' || coalesce(l.ids, '') || '] numbered '
				|| coalesce(l.lo::text, '-') || '..' || coalesce(l.hi::text, '-') || ', primary service ' || a.service_type_id::text
			FROM appointments a
			CROSS JOIN LATERAL (
				SELECT count(*) AS n, min(line_no) AS lo, max(line_no) AS hi,
					string_agg(service_type_id::text, ',' ORDER BY line_no) AS ids,
					(array_agg(service_type_id ORDER BY line_no))[1] AS head_id
				FROM appointment_lines WHERE appointment_id = a.id) l
			WHERE a.idempotency_key LIKE $1
			  AND (l.n NOT BETWEEN 1 AND 5
			       OR l.lo IS DISTINCT FROM 1 OR l.hi IS DISTINCT FROM l.n
			       OR l.head_id IS DISTINCT FROM a.service_type_id
			       OR (cardinality($2::text[]) > 0 AND NOT coalesce(l.ids = ANY($2::text[]), false)))
			LIMIT 5`, 2},
		{checkDuration, `
			SELECT a.id::text || ': window ' || (extract(epoch FROM a.end_at - a.start_at) / 60)::int || ' min, booked '
				|| a.booked_duration_minutes || ' min, lines ' || coalesce(l.total::text, 'none')
			FROM appointments a
			CROSS JOIN LATERAL (SELECT sum(duration_minutes) AS total FROM appointment_lines WHERE appointment_id = a.id) l
			WHERE a.idempotency_key LIKE $1 AND a.status NOT IN (` + statusInProgressSQL + `, ` + statusCompletedSQL + `)
			  AND (a.end_at - a.start_at <> make_interval(mins => a.booked_duration_minutes)
			       OR l.total IS DISTINCT FROM a.booked_duration_minutes)
			LIMIT 5`, 1},
		{checkRequirement, `
			SELECT a.id::text || ': booked bay ' || a.required_bay_type || ' skills ' || a.required_skills::text
				|| ' level ' || a.min_skill_level || ', lines need bay ' || coalesce(r.bay::text, 'none')
				|| ' skills ' || coalesce(r.skills::text, '{}') || ' level ' || coalesce(r.lvl::text, 'none')
			FROM appointments a
			CROSS JOIN LATERAL (
				SELECT
					CASE WHEN count(DISTINCT st.required_bay_type) FILTER (WHERE st.required_bay_type IN (` + bayDiagnosticSQL + `, ` + bayAlignmentSQL + `)) > 1 THEN NULL
					     WHEN bool_or(st.required_bay_type IN (` + bayDiagnosticSQL + `, ` + bayAlignmentSQL + `)) THEN max(st.required_bay_type) FILTER (WHERE st.required_bay_type IN (` + bayDiagnosticSQL + `, ` + bayAlignmentSQL + `))
					     WHEN bool_or(st.required_bay_type = ` + bayGeneralSQL + `) THEN ` + bayGeneralSQL + `
					     WHEN count(*) > 0 THEN 1
					END AS bay,
					array_agg(DISTINCT st.required_skill ORDER BY st.required_skill) AS skills,
					greatest(1, max(st.min_skill_level)) AS lvl
				FROM appointment_lines l JOIN service_types st ON st.id = l.service_type_id
				WHERE l.appointment_id = a.id) r
			WHERE a.idempotency_key LIKE $1
			  AND (r.bay IS DISTINCT FROM a.required_bay_type
			       OR r.skills IS DISTINCT FROM (SELECT array_agg(s ORDER BY s) FROM unnest(a.required_skills) s)
			       OR r.lvl IS DISTINCT FROM a.min_skill_level)
			LIMIT 5`, 1},
	}
}

// checkFailure is one integrity check that found offending rows.
type checkFailure struct {
	name string
	rows []string
}

func (f checkFailure) String() string { return fmt.Sprintf("%s %v", f.name, f.rows) }

// runIntegrityChecks runs every check (one failing does not hide the others) and returns those
// that found rows. mixes are the run's job mixes, each its service type ids joined by "," in line
// order; empty skips the mix match.
func runIntegrityChecks(ctx context.Context, q querier, runID string, mixes []string) ([]checkFailure, error) {
	if mixes == nil {
		mixes = []string{}
	}
	args := []any{runID + ":%", mixes}
	var failures []checkFailure
	for _, c := range integrityChecks() {
		rows, err := textRows(ctx, q, c.sql, args[:c.args]...)
		if err != nil {
			return nil, fmt.Errorf("sql integrity: %s: %w", c.name, err)
		}
		if len(rows) > 0 {
			failures = append(failures, checkFailure{name: c.name, rows: rows})
		}
	}
	return failures, nil
}

// mixKey is a job mix as the lines check expects it: service type ids joined by "," in line order.
func mixKey(job []uuid.UUID) string {
	parts := make([]string, len(job))
	for i, id := range job {
		parts[i] = id.String()
	}
	return strings.Join(parts, ",")
}

// checkSQL runs the integrity checks, then reconciles the run's rows (found by Idempotency-Key
// prefix) with what clients were told: every created id is there, and at most one extra per
// request left unresolved.
func checkSQL(ctx context.Context, dsn, runID string, created map[uuid.UUID]bool, unresolved int64, mixes []string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("sql integrity: connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	failures, err := runIntegrityChecks(ctx, conn, runID, mixes)
	if err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("sql integrity: %v", failures)
	}
	rows, err := conn.Query(ctx, `SELECT id FROM appointments WHERE idempotency_key LIKE $1`, runID+":%")
	if err != nil {
		return fmt.Errorf("sql integrity: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("sql integrity: %w", err)
	}
	return reconcile(created, ids, unresolved)
}

// reconcile compares the ids clients were told about with the run's rows in Postgres.
func reconcile(created map[uuid.UUID]bool, inDB []uuid.UUID, unresolved int64) error {
	have := make(map[uuid.UUID]bool, len(inDB))
	for _, id := range inDB {
		have[id] = true
	}
	for id := range created {
		if !have[id] {
			return fmt.Errorf("sql integrity: appointment %s was answered 201/200 but is not in Postgres under this run's keys", id)
		}
	}
	if extra := int64(len(have) - len(created)); extra > unresolved {
		return fmt.Errorf("sql integrity: %d appointments in Postgres that no client was told about (only %d requests unresolved)", extra, unresolved)
	}
	return nil
}

// runHoldingIDs returns the run's appointments (by Idempotency-Key prefix) that still hold
// resources and can be cancelled (confirmed or checked in).
func runHoldingIDs(ctx context.Context, dsn, runID string) ([]uuid.UUID, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	rows, err := conn.Query(ctx,
		`SELECT id FROM appointments WHERE idempotency_key LIKE $1 || ':%' AND status IN (`+notStartedSQL+`)`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
}

// textRows runs a query returning one text column.
func textRows(ctx context.Context, q querier, sql string, args ...any) ([]string, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
