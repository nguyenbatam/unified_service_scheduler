package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/migrate"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/testdb"
)

// integrityDB is a migrated database of the test's own: migrating the shared entry database would
// race the integration package (run in parallel by go test ./...), which resets its schema in
// fallback mode.
func integrityDB(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dsn, own := testdb.FreshDSN(t)
	if !own {
		// Required runs (CI, check-all) give the test role CREATEDB; skipping there would hide
		// that the SQL integrity pass never ran.
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatal("needs CREATEDB for a database of its own (required when REQUIRE_INTEGRATION=1)")
		}
		t.Skip("needs CREATEDB for a database of its own (the entry database is shared with the integration package)")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Up(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return dsn, pool
}

// The SQL integrity pass against the real schema: every query parses and type-checks, and a run
// with no rows of its own passes (the staging check only looks at windows the run booked into, so
// whatever else is in the database cannot fail it).
func TestCheckSQLRunsAgainstSchema(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	dsn, _ := integrityDB(t)
	ctx := t.Context()
	runID := "stress:test-" + uuid.NewString()[:8]
	if err := checkSQL(ctx, dsn, runID, map[uuid.UUID]bool{}, 0, nil); err != nil {
		t.Fatalf("checkSQL on an empty run: %v", err)
	}
	ids, err := runHoldingIDs(ctx, dsn, runID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("runHoldingIDs = %v, %v; want none", ids, err)
	}
	// A client-reported booking that is not in Postgres must fail the pass.
	if err := checkSQL(ctx, dsn, runID, map[uuid.UUID]bool{uuid.New(): true}, 0, nil); err == nil {
		t.Fatal("checkSQL accepted a created id missing from Postgres")
	}
}

func TestReconcile(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	cases := []struct {
		name       string
		created    []uuid.UUID
		inDB       []uuid.UUID
		unresolved int64
		wantErr    string
	}{
		{"exact match", []uuid.UUID{a, b}, []uuid.UUID{a, b}, 0, ""},
		{"created but missing", []uuid.UUID{a, b}, []uuid.UUID{a}, 5, "not in Postgres"},
		{"one extra covered by an unresolved request", []uuid.UUID{a}, []uuid.UUID{a, b}, 1, ""},
		{"two extra, one unresolved", []uuid.UUID{a}, []uuid.UUID{a, b, c}, 1, "2 appointments in Postgres that no client was told about"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := map[uuid.UUID]bool{}
			for _, id := range tc.created {
				created[id] = true
			}
			err := reconcile(created, tc.inDB, tc.unresolved)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("reconcile = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// fixture is a minimal catalog (committed once) that violating appointments are built on, each
// test case inside a transaction rolled back afterwards, so every case sees only its own rows.
type fixture struct {
	dealer, customer             uuid.UUID
	vehicles                     [3]uuid.UUID
	oil, brake                   uuid.UUID
	express1, express2, general1 uuid.UUID
	senior, senior2, junior      uuid.UUID // levels 3, 3, 1; the seniors hold lubrication + brakes, the junior lubrication only
	runID                        string
	n                            int
}

func newFixture(t *testing.T, pool *pgxpool.Pool) *fixture {
	t.Helper()
	f := &fixture{
		dealer: uuid.New(), customer: uuid.New(), oil: uuid.New(), brake: uuid.New(),
		express1: uuid.New(), express2: uuid.New(), general1: uuid.New(),
		senior: uuid.New(), senior2: uuid.New(), junior: uuid.New(),
		runID: "stress:fx-" + uuid.NewString()[:8],
	}
	for i := range f.vehicles {
		f.vehicles[i] = uuid.New()
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO dealerships (id, name, timezone, open_time, close_time, staging_capacity, express_max_minutes)
			VALUES ($1, 'Fixture Motors', 'Europe/London', TIME '08:00', TIME '18:00', 0, 60)`, []any{f.dealer}},
		{`INSERT INTO customers (id, name, phone, email) VALUES ($1, 'Fixture Customer', '0', 'fx@example.com')`, []any{f.customer}},
		{`INSERT INTO vehicles (id, customer_id, vin, make, model, year) VALUES
			($1, $4, 'FX000000000000001', 'Fx', 'One', 2024),
			($2, $4, 'FX000000000000002', 'Fx', 'Two', 2024),
			($3, $4, 'FX000000000000003', 'Fx', 'Three', 2024)`, []any{f.vehicles[0], f.vehicles[1], f.vehicles[2], f.customer}},
		{`INSERT INTO service_types (id, code, name, duration_minutes, required_skill, required_bay_type, min_skill_level) VALUES
			($1, 'FXOIL', 'Oil', 30, 1, 1, 1), ($2, 'FXBRAKE', 'Brake', 90, 2, 2, 2)`, []any{f.oil, f.brake}},
		{`INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES
			($1, $4, 'E1', 1), ($2, $4, 'E2', 1), ($3, $4, 'G1', 2)`, []any{f.express1, f.express2, f.general1, f.dealer}},
		{`INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES
			($1, $4, 'Senior', 3), ($2, $4, 'Senior Two', 3), ($3, $4, 'Junior', 1)`, []any{f.senior, f.senior2, f.junior, f.dealer}},
		{`INSERT INTO technician_skills (technician_id, skill) VALUES ($1, 1), ($1, 2), ($2, 1), ($2, 2), ($3, 1)`,
			[]any{f.senior, f.senior2, f.junior}},
	}
	for _, s := range stmts {
		if _, err := pool.Exec(t.Context(), s.sql, s.args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	return f
}

// mixes are the job mixes the fixture "run" booked: oil alone, and oil + brake.
func (f *fixture) mixes() []string {
	return []string{mixKey([]uuid.UUID{f.oil}), mixKey([]uuid.UUID{f.oil, f.brake})}
}

// at is 2030-01-07 (a Monday; London is on UTC in January) at hh:mm.
func at(hh, mm int) time.Time { return time.Date(2030, 1, 7, hh, mm, 0, 0, time.UTC) }

// appt is one appointment row with its lines and events. Zero snapshot fields are derived from the
// lines the way the job-line rule does, so a spec only names what it breaks.
type appt struct {
	vehicle, bay, tech uuid.UUID
	start              time.Time
	windowMinutes      int // 0 = the booked duration
	status             int16
	lines              []uuid.UUID
	booked             int
	bayType            int16
	skills             []int16
	level              int16
	events             int // APPOINTMENT_CREATED outbox + history rows (-1 = none)
}

type service struct {
	minutes      int
	skill, level int16
	bay          int16
}

func (f *fixture) catalog() map[uuid.UUID]service {
	return map[uuid.UUID]service{f.oil: {30, 1, 1, 1}, f.brake: {90, 2, 2, 2}}
}

func (f *fixture) insert(ctx context.Context, tx pgx.Tx, a appt) (uuid.UUID, error) {
	cat := f.catalog()
	booked, bay, level := 0, int16(1), int16(1)
	var skills []int16
	for _, l := range a.lines {
		s := cat[l]
		booked += s.minutes
		bay = max(bay, s.bay)
		level = max(level, s.level)
		if !slices.Contains(skills, s.skill) {
			skills = append(skills, s.skill)
		}
	}
	slices.Sort(skills)
	if a.booked == 0 {
		a.booked = booked
	}
	if a.bayType == 0 {
		a.bayType = bay
	}
	if a.skills == nil {
		a.skills = skills
	}
	if a.level == 0 {
		a.level = level
	}
	if a.windowMinutes == 0 {
		a.windowMinutes = a.booked
	}
	if a.status == 0 {
		a.status = int16(domain.StatusConfirmed)
	}
	id := uuid.New()
	f.n++
	end := a.start.Add(time.Duration(a.windowMinutes) * time.Minute)
	if _, err := tx.Exec(ctx, `
		INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id, service_bay_id, technician_id,
			start_at, end_at, status, idempotency_key, booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		id, f.dealer, f.customer, a.vehicle, a.lines[0], a.bay, a.tech, a.start, end, a.status,
		fmt.Sprintf("%s:%d", f.runID, f.n), a.booked, a.bayType, a.skills, a.level); err != nil {
		return id, fmt.Errorf("insert appointment: %w", err)
	}
	for i, l := range a.lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO appointment_lines (appointment_id, line_no, service_type_id, service_code, service_name, duration_minutes)
			VALUES ($1, $2, $3, 'X', 'X', $4)`, id, i+1, l, cat[l].minutes); err != nil {
			return id, fmt.Errorf("insert line: %w", err)
		}
	}
	events := a.events
	if events == 0 {
		events = 1
	}
	for range events { // -1: none
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (id, aggregate_type, aggregate_id, event_type, payload)
			VALUES ($1, `+sqlCode(domain.AggregateAppointment)+`, $2, `+sqlCode(domain.EventAppointmentCreated)+`, '{}')`, uuid.New(), id); err != nil {
			return id, fmt.Errorf("insert outbox: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO appointment_history (appointment_id, version, event_type, status, start_at, end_at, service_bay_id, technician_id)
			VALUES ($1, 1, `+sqlCode(domain.EventAppointmentCreated)+`, $2, $3, $4, $5, $6)`, id, a.status, a.start, end, a.bay, a.tech); err != nil {
			return id, fmt.Errorf("insert history: %w", err)
		}
	}
	return id, nil
}

// Every integrity check reports the violation it exists for, and only that one: each case inserts
// a consistent booking broken in exactly one way (the overlap cases drop the GiST constraint that
// would refuse them first), inside a transaction rolled back afterwards.
func TestIntegrityChecksCatchViolations(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	f := newFixture(t, pool)
	oil := []uuid.UUID{f.oil}
	oilBrake := []uuid.UUID{f.oil, f.brake}
	v0, v1, v2 := f.vehicles[0], f.vehicles[1], f.vehicles[2]
	cases := []struct {
		name  string
		setup []string // SQL run first in the transaction
		appts []appt
		want  string // check name; "" = every check passes
	}{
		{name: "consistent bookings pass", appts: []appt{
			{vehicle: v0, bay: f.express1, tech: f.junior, start: at(9, 0), lines: oil},
			{vehicle: v1, bay: f.general1, tech: f.senior, start: at(9, 0), lines: oilBrake},
			// Same bay, technician and vehicle back to back: half-open windows do not overlap.
			{vehicle: v0, bay: f.express1, tech: f.junior, start: at(9, 30), lines: oil},
			// A cancelled booking overlapping a holding one is no double-book.
			{vehicle: v0, bay: f.express1, tech: f.junior, start: at(9, 0), lines: oil, status: 5},
		}},
		{name: "bay double-booked", want: checkBayOverlap,
			setup: []string{`ALTER TABLE appointments DROP CONSTRAINT appointments_bay_no_overlap`},
			appts: []appt{
				{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil},
				{vehicle: v1, bay: f.express1, tech: f.senior2, start: at(10, 0), lines: oil},
			}},
		{name: "technician double-booked", want: checkTechOverlap,
			setup: []string{`ALTER TABLE appointments DROP CONSTRAINT appointments_technician_no_overlap`},
			appts: []appt{
				{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil},
				{vehicle: v1, bay: f.express2, tech: f.senior, start: at(10, 0), lines: oil},
			}},
		{name: "vehicle double-booked", want: checkVehicleOverlap,
			setup: []string{`ALTER TABLE appointments DROP CONSTRAINT appointments_vehicle_no_overlap`},
			appts: []appt{
				{vehicle: v2, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil},
				{vehicle: v2, bay: f.express2, tech: f.senior2, start: at(10, 0), lines: oil},
			}},
		{name: "staging window over capacity", want: checkStaging,
			setup: []string{`UPDATE dealerships SET staging_capacity = 1, staging_window_minutes = 60`},
			appts: []appt{
				{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil},
				{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 30), lines: oil},
			}},
		{name: "technician below the level and missing a skill", want: checkUnqualified,
			appts: []appt{{vehicle: v0, bay: f.general1, tech: f.junior, start: at(10, 0), lines: oilBrake}}},
		{name: "technician absent", want: checkOffDuty,
			setup: []string{fmt.Sprintf(`INSERT INTO technician_absences (id, technician_id, starts_at, ends_at, reason)
				VALUES ('%s', '%s', '2030-01-07 10:15+00', '2030-01-07 11:00+00', 'fixture')`, uuid.New(), f.senior)},
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil}}},
		{name: "technician's shift ends before the job", want: checkOffDuty,
			setup: []string{fmt.Sprintf(`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time)
				VALUES ('%s', 1, '08:00', '10:15')`, f.senior)},
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil}}},
		{name: "technician's break overlaps the job", want: checkOffDuty,
			setup: []string{fmt.Sprintf(`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time, break_start, break_end)
				VALUES ('%s', 1, '08:00', '17:00', '10:15', '10:45')`, f.senior)},
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil}}},
		{name: "on duty: the break ends as the job starts and the shift covers it (consistent)",
			setup: []string{fmt.Sprintf(`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time, break_start, break_end)
				VALUES ('%s', 1, '08:00', '10:30', '09:30', '10:00')`, f.senior)},
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil}}},
		{name: "technician has no shift that weekday", want: checkOffDuty,
			setup: []string{fmt.Sprintf(`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time)
				VALUES ('%s', 2, '08:00', '17:00')`, f.senior)},
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil}}},
		{name: "express job spilled into a general bay (consistent)",
			appts: []appt{{vehicle: v0, bay: f.general1, tech: f.senior, start: at(10, 0), lines: oil}}},
		{name: "general job in an express bay (not a waiter)", want: checkBay,
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oilBrake}}},
		{name: "no CREATED outbox/history row", want: checkEvents,
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil, events: -1}}},
		{name: "two CREATED outbox/history rows", want: checkEvents,
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil, events: 2}}},
		{name: "six job lines", want: checkLines,
			// The schema refuses a sixth line (001_schema.sql); dropped in this rolled-back TX so
			// the check behind it is still exercised (defence in depth).
			setup: []string{`ALTER TABLE appointment_lines DROP CONSTRAINT appointment_lines_line_no_check`},
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: slices.Repeat(oil, 6)}}},
		{name: "lines in an order no job mix has", want: checkLines,
			appts: []appt{{vehicle: v0, bay: f.general1, tech: f.senior, start: at(10, 0), lines: []uuid.UUID{f.brake, f.oil}}}},
		{name: "window longer than the booked duration", want: checkDuration,
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil, windowMinutes: 60}}},
		{name: "booked duration is not the lines' sum", want: checkDuration,
			appts: []appt{{vehicle: v0, bay: f.express1, tech: f.senior, start: at(10, 0), lines: oil, booked: 60}}},
		{name: "express job snapshotted as general", want: checkRequirement,
			appts: []appt{{vehicle: v0, bay: f.general1, tech: f.senior, start: at(10, 0), lines: oil, bayType: 2}}},
		{name: "level below the brake line's", want: checkRequirement,
			appts: []appt{{vehicle: v0, bay: f.general1, tech: f.senior, start: at(10, 0), lines: oilBrake, level: 1}}},
		{name: "skill missing from the snapshot", want: checkRequirement,
			appts: []appt{{vehicle: v0, bay: f.general1, tech: f.senior, start: at(10, 0), lines: oilBrake, skills: []int16{2}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
			for _, sql := range tc.setup {
				if _, err := tx.Exec(ctx, sql); err != nil {
					t.Fatalf("setup: %v", err)
				}
			}
			for _, a := range tc.appts {
				if _, err := f.insert(ctx, tx, a); err != nil {
					t.Fatal(err)
				}
			}
			failures, err := runIntegrityChecks(ctx, tx, f.runID, f.mixes())
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, fl := range failures {
				names = append(names, fl.name)
			}
			switch {
			case tc.want == "" && len(failures) > 0:
				t.Fatalf("consistent rows failed: %v", failures)
			case tc.want != "" && !slices.Equal(names, []string{tc.want}):
				t.Fatalf("failures %v, want exactly %q", failures, tc.want)
			}
		})
	}
}

// Without job mixes the lines check still bounds the count but accepts any order.
func TestLinesCheckWithoutMixes(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	f := newFixture(t, pool)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := f.insert(ctx, tx, appt{vehicle: f.vehicles[0], bay: f.general1, tech: f.senior, start: at(10, 0),
		lines: []uuid.UUID{f.brake, f.oil}}); err != nil {
		t.Fatal(err)
	}
	if failures, err := runIntegrityChecks(ctx, tx, f.runID, nil); err != nil || len(failures) > 0 {
		t.Fatalf("failures %v, err %v; want none without mixes", failures, err)
	}
}

// A run with no rows of its own fails the integrity record instead of passing every per-run check
// over nothing.
func TestRecordIntegrityFailsOverZeroRows(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	dsn, _ := integrityDB(t)
	ctx := t.Context()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	m := newMatrix()
	n, err := recordIntegrity(ctx, m, conn, "stress:empty-"+uuid.NewString()[:8], nil, "sql")
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rowVerdict(m.rows["R3"]) != verdictFail || len(m.rows["R2a"]) != 0 {
		t.Fatalf("rows=%d R3=%v R2a=%v: want one R3 failure and no per-check pass", n, m.rows["R3"], m.rows["R2a"])
	}
}
