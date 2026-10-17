package store

import (
	"fmt"
	"os"
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

// The hot read paths — a booking's allocation candidates (and its one-move repair) and a day's
// availability — must reach
// appointments through an index (the GiST exclusion indexes, or (dealership_id, start_at) /
// (vehicle_id, start_at)), never by scanning the table: it holds every booking ever made. Each
// query is planned exactly as the code builds it (the batches' queued SQL and arguments) with
// sequential scans priced out, so a plan that still scans appointments has no index to use. Every
// keyset list (appointments, catalog, customers, vehicles, technicians, history, failed outbox)
// is checked the same way, first page and after a cursor: none may scan its table.
func TestHotQueriesUseIndexes(t *testing.T) {
	dsn, own := testdb.FreshDSN(t)
	if !own {
		// The entry database is shared with the integration package, which resets its schema.
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatal("needs CREATEDB for a database of its own (required when REQUIRE_INTEGRATION=1)")
		}
		t.Skip("needs CREATEDB for a database of its own")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}

	dealer, vehicle, exclude := uuid.New(), uuid.New(), uuid.New()
	start := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	slot := Slot{Start: start, End: start.Add(90 * time.Minute), DayStart: start.Add(-9 * time.Hour), DayEnd: start.Add(15 * time.Hour),
		Lo: start.Add(-time.Hour), Hi: start.Add(8 * time.Hour), Exclude: exclude}
	skills := []domain.TechnicianSkill{domain.SkillBrakes}

	type query struct {
		name string
		sql  string
		args []any
	}
	var queries []query
	add := func(name, sql string, args []any) { queries = append(queries, query{name, sql, args}) }
	// must names an index (prefix) a query's plan has to use.
	must := map[string]string{}
	sql, args := freeBaysSQL(BayQuery{DealershipID: dealer, Required: domain.BayGeneral, SoftUpgrade: true, Slot: slot})
	add("free bays", sql, args)
	sql, args = freeTechniciansSQL(TechQuery{DealershipID: dealer, Skills: skills, MinLevel: 2, Slot: slot})
	add("free technicians", sql, args)
	sql, args = vehicleBusySQL(vehicle, slot.Start, slot.End, exclude)
	add("vehicle busy", sql, args)
	repair := RepairQuery{DealershipID: dealer, Slot: slot, Now: start.Add(-24 * time.Hour), BayType: domain.BayGeneral, SoftUpgrade: true, Skills: skills, MinLevel: 2}
	for _, ranked := range []bool{true, false} {
		sql, args = bayRepairSQL(repair, ranked)
		add(fmt.Sprintf("bay repair (ranked %v)", ranked), sql+lockMovedSQL, args)
		sql, args = techRepairSQL(repair, ranked)
		add(fmt.Sprintf("technician repair (ranked %v)", ranked), sql+lockMovedSQL, args)
	}
	b := &pgx.Batch{}
	var holds []Hold
	var busy []Busy
	queueConfirmedBusy(b, &holds, dealer, slot.DayStart, slot.DayEnd, exclude)
	queueVehicleHolds(b, &busy, vehicle, slot.DayStart, slot.DayEnd, exclude)
	for i, q := range b.QueuedQueries {
		add([]string{"day holds", "vehicle holds"}[i], q.SQL, q.Arguments)
	}

	// Appointment lists (GET /appointments): every listing, scope, and page position.
	after := &AppointmentCursor{StartAt: start, ID: exclude}
	for _, when := range []AppointmentWhen{WhenAll, WhenUpcoming, WhenPast} {
		for _, f := range []AppointmentFilter{{}, {CustomerID: uuid.New()}, {DealershipID: dealer}, {VehicleID: vehicle}, {DealershipID: dealer, CustomerID: uuid.New()}} {
			f.When, f.Now, f.Limit = when, start, 20
			for _, cursor := range []*AppointmentCursor{nil, after} {
				sql, args := appointmentsPageSQL(f, cursor)
				name := fmt.Sprintf("appointments page %+v after %v", f, cursor)
				add(name, sql, args)
				if when == WhenUpcoming {
					// The on-site branch (2|3) has no start_at bound: only its partial indexes
					// (026) keep it off the scope's whole history.
					must[name] = "appointments_on_site_"
				}
			}
		}
	}

	// The other keyset lists, first page and after a cursor. Their tables are small or the scan is
	// bounded differently (a search's GIN bitmap), so only the order's own index is asserted:
	// without enable_seqscan a plan that still scans has no index for the order.
	name := &NameCursor{Name: "M", ID: exclude}
	lists := []query{
		{"dealerships", dealershipsFirstSQL, []any{21}},
		{"dealerships after", dealershipsAfterSQL, []any{"M", exclude, 21}},
		{"service types", serviceTypesFirstSQL, []any{21}},
		{"service types after", serviceTypesAfterSQL, []any{60, "M", exclude, 21}},
		{"history", historyPageSQL, []any{exclude, int64(0), 21}},
	}
	for _, dealer := range []uuid.UUID{uuid.Nil, dealer} {
		for _, after := range []*NameCursor{nil, name} {
			sql, args := technicianAdminPageSQL(dealer, 20, after)
			lists = append(lists, query{fmt.Sprintf("technicians of %v after %v", dealer, after), sql, args})
		}
	}
	for _, search := range []string{"", "ann"} {
		for _, after := range []*NameCursor{nil, name} {
			sql, args := customersPageSQL(search, 20, after)
			lists = append(lists, query{fmt.Sprintf("customers %q after %v", search, after), sql, args})
		}
		for _, owner := range []uuid.UUID{uuid.Nil, uuid.New()} {
			for _, after := range []*VehicleCursor{nil, {Owner: "M", Make: "A", Model: "B", ID: exclude}} {
				sql, args := vehiclesPageSQL(VehicleFilter{CustomerID: owner, Search: search, Limit: 20, After: after})
				lists = append(lists, query{fmt.Sprintf("vehicles of %v %q after %v", owner, search, after), sql, args})
			}
		}
	}
	for _, after := range []*OutboxCursor{nil, {CreatedAt: start, ID: exclude}} {
		sql, args := failedOutboxPageSQL(20, after)
		lists = append(lists, query{fmt.Sprintf("failed outbox after %v", after), sql, args})
	}
	hot := len(queries)
	queries = append(queries, lists...)

	for i, q := range queries {
		rows, err := conn.Query(ctx, "EXPLAIN "+q.sql, q.args...)
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		plan, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("%s: %v", q.name, err)
		}
		// The hot paths must not scan appointments; a list must not scan any table.
		forbidden := "Seq Scan on "
		if i < hot {
			forbidden += "appointments"
		}
		p := strings.Join(plan, "\n")
		if strings.Contains(p, forbidden) {
			t.Fatalf("%s scans:\n%s", q.name, p)
		}
		if idx := must[q.name]; idx != "" && !strings.Contains(p, idx) {
			t.Fatalf("%s does not use %s*:\n%s", q.name, idx, p)
		}
	}
}
