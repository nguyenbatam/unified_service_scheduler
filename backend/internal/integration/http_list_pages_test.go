package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// headID is the n-th lowest id: copies of a row with these ids sort right before it (every
// listing breaks ties by id); highestID sorts after every copy.
func headID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("00000000-0000-4000-8000-%012d", n))
}

var highestID = uuid.MustParse("ffffffff-ffff-4fff-bfff-fffffffffffe")

// cloneRow inserts a copy of table's row id with set's columns overridden (plus a new id unless
// set names one) and returns the copy's id.
func cloneRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, id uuid.UUID, set map[string]any) uuid.UUID {
	t.Helper()
	if _, ok := set["id"]; !ok {
		set["id"] = uuid.New()
	}
	over, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var out uuid.UUID
	// table is a test constant, never input.
	if err := pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO %[1]s SELECT (jsonb_populate_record(NULL::%[1]s, to_jsonb(x) || $2::jsonb)).*
		FROM %[1]s x WHERE x.id = $1 RETURNING id`, table), id, over).Scan(&out); err != nil {
		t.Fatalf("clone %s %s: %v", table, id, err)
	}
	return out
}

func queryIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) []uuid.UUID {
	t.Helper()
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// listCase is one paged listing seen by one user: order is the SQL oracle (ids in the listing's
// order and scope); rows of table are cloned (with unique's columns made unique) to build ties
// and pages; drop removes or hides a row from the listing (deleted, requeued…).
type listCase struct {
	name, path string
	user       apiClient
	order      string
	table      string
	unique     func() map[string]any
	drop       func(id uuid.UUID)
}

// Every list endpoint pages by keyset: the token carries the last row's sort-key values, never a
// row to look up, so following X-Next-Cursor returns each remaining row exactly once in order even
// when, between two pages, the cursor row itself, the rows before it, and the row right after it
// are removed or hidden, a row sorting before the cursor is inserted (it belongs to a page already
// read, so it is not shown), and the last row goes too (the tail never yields an empty page with a
// cursor). OFFSET paging would repeat or skip rows; a cursor that looked its row up would fail.
func TestHTTPListsPageByKeyset(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	h := newAPI(st, svc)
	admin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	del := func(table string) func(uuid.UUID) {
		return func(id uuid.UUID) { exec(fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, table), id) }
	}
	newCode := func() map[string]any { return map[string]any{"code": "page-" + uuid.NewString()} }
	vin := func() map[string]any {
		return map[string]any{"vin": strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))[:17]}
	}
	failed := func() map[string]any { return map[string]any{"status": domain.OutboxFailed, "last_error": "test"} }

	// The failed outbox list starts empty: fail a few seeded events.
	exec(`UPDATE outbox_events SET status = ` + code(domain.OutboxFailed) + ` WHERE id IN (SELECT id FROM outbox_events ORDER BY seq LIMIT 3)`)

	cases := []listCase{
		{name: "dealerships", path: "/api/v1/dealerships?", user: customer, table: "dealerships",
			order: `SELECT id FROM dealerships ORDER BY name, id`, drop: del("dealerships")},
		{name: "service types", path: "/api/v1/service-types?", user: customer, table: "service_types", unique: newCode,
			order: `SELECT id FROM service_types ORDER BY duration_minutes, name, id`, drop: del("service_types")},
		{name: "customers", path: "/api/v1/customers?", user: dispatcher, table: "customers",
			order: `SELECT id FROM customers ORDER BY name, id`, drop: del("customers")},
		{name: "customer search", path: "/api/v1/customers?q=a", user: dispatcher, table: "customers",
			order: `SELECT id FROM customers WHERE name ILIKE '%a%' OR phone ILIKE '%a%' OR email ILIKE '%a%' ORDER BY name, id`,
			// Hidden by an edit: it no longer matches the search.
			drop: func(id uuid.UUID) {
				exec(`UPDATE customers SET name = 'Zed', phone = '0', email = 'z@z.z' WHERE id = $1`, id)
			}},
		{name: "vehicles (staff, group-wide)", path: "/api/v1/vehicles?", user: dispatcher, table: "vehicles", unique: vin,
			order: `SELECT v.id FROM vehicles v JOIN customers c ON c.id = v.customer_id ORDER BY c.name, v.make, v.model, v.id`, drop: del("vehicles")},
		{name: "vehicle search (staff)", path: "/api/v1/vehicles?q=o", user: dispatcher, table: "vehicles", unique: vin,
			order: `SELECT v.id FROM vehicles v JOIN customers c ON c.id = v.customer_id
				WHERE v.vin ILIKE '%o%' OR v.make ILIKE '%o%' OR v.model ILIKE '%o%' OR c.name ILIKE '%o%'
				ORDER BY c.name, v.make, v.model, v.id`, drop: del("vehicles")},
		{name: "vehicles (customer: own only)", path: "/api/v1/vehicles?", user: customer, table: "vehicles", unique: vin,
			order: fmt.Sprintf(`SELECT v.id FROM vehicles v JOIN customers c ON c.id = v.customer_id
				WHERE v.customer_id = '%s' ORDER BY c.name, v.make, v.model, v.id`, seed.CustomerA),
			// Hidden by an edit: handed to another customer.
			drop: func(id uuid.UUID) { exec(`UPDATE vehicles SET customer_id = $2 WHERE id = $1`, id, seed.CustomerB) }},
		{name: "technicians", path: "/api/v1/admin/technicians?", user: admin, table: "technicians",
			order: `SELECT id FROM technicians ORDER BY name, id`, drop: del("technicians")},
		{name: "technicians of one dealership", path: "/api/v1/admin/technicians?dealershipId=" + seed.DealerNorth.String(), user: admin, table: "technicians",
			order: fmt.Sprintf(`SELECT id FROM technicians WHERE dealership_id = '%s' ORDER BY name, id`, seed.DealerNorth),
			// Hidden by an edit: moved to the other dealership.
			drop: func(id uuid.UUID) {
				exec(`UPDATE technicians SET dealership_id = $2 WHERE id = $1`, id, seed.DealerRiver)
			}},
		{name: "failed outbox", path: "/api/v1/admin/outbox/failed?", user: admin, table: "outbox_events", unique: failed,
			order: `SELECT id FROM outbox_events WHERE status = ` + code(domain.OutboxFailed) + ` ORDER BY created_at, id`,
			// Requeued: it leaves the list without being deleted.
			drop: func(id uuid.UUID) {
				exec(`UPDATE outbox_events SET status = `+code(domain.OutboxPending)+` WHERE id = $1`, id)
			}},
	}
	for _, tc := range cases {
		clone := func(src, id uuid.UUID) uuid.UUID {
			set := map[string]any{}
			if tc.unique != nil {
				set = tc.unique()
			}
			if id != uuid.Nil {
				set["id"] = id
			}
			return cloneRow(t, ctx, pool, tc.table, src, set)
		}
		before := queryIDs(t, ctx, pool, tc.order)
		if len(before) == 0 {
			t.Fatalf("%s: nothing to list", tc.name)
		}
		// Ties on every key but the id, and enough rows for several pages.
		copies := 2
		if len(before) < 3 {
			copies = 5
		}
		for _, id := range before {
			for range copies {
				clone(id, uuid.Nil)
			}
		}
		grown := queryIDs(t, ctx, pool, tc.order)
		// Three rows that sort first (the first page and the row right after its cursor), and one
		// that sorts last.
		head := []uuid.UUID{clone(grown[0], headID(1)), clone(grown[0], headID(2)), clone(grown[0], headID(3))}
		lastID := clone(grown[len(grown)-1], highestID)
		want := queryIDs(t, ctx, pool, tc.order)
		if len(want) < 8 || !slices.Equal(want[:3], head) || want[len(want)-1] != lastID {
			t.Fatalf("%s: %d rows %v, want the head clones %v first and %v last", tc.name, len(want), want, head, lastID)
		}

		rec := tc.user.do(http.MethodGet, tc.path+"&limit=2", nil, nil)
		var page []struct {
			ID uuid.UUID `json:"id"`
		}
		tc.user.expect(rec, http.StatusOK, &page)
		next := rec.Header().Get("X-Next-Cursor")
		if len(page) != 2 || page[0].ID != head[0] || page[1].ID != head[1] || next == "" {
			t.Fatalf("%s: first page %v, next %q", tc.name, page, next)
		}
		// Between pages: the cursor row, the row before it, the row right after it, and the last
		// row go; a row sorting before the cursor appears.
		inserted := clone(grown[0], headID(0))
		for _, id := range append(head, lastID) {
			tc.drop(id)
		}
		got, pages, _ := pageAllIDs(t, tc.user, tc.path+"&limit=2", next)
		if wantRest := want[3 : len(want)-1]; !slices.Equal(got, wantRest) {
			t.Fatalf("%s: paged %d ids over %d pages, want %d in order\n got %v\nwant %v", tc.name, len(got), pages, len(wantRest), got, wantRest)
		}
		// Undo, so the next listing over the same table starts from the seeded rows.
		tc.drop(inserted)
		for _, id := range append(queryIDs(t, ctx, pool, tc.order), head...) {
			if !slices.Contains(before, id) {
				tc.drop(id)
			}
		}
		if after := queryIDs(t, ctx, pool, tc.order); !slices.Equal(after, before) {
			t.Fatalf("%s: cleanup left %v, want %v", tc.name, after, before)
		}
	}
}

// pageAllIDs follows path from cursor to the last page, returning the ids, pages read, and the
// last cursor given. A page that hands out a next cursor is always full (the server reads one row
// past the page), so a client never loops over empty pages.
func pageAllIDs(t *testing.T, user apiClient, path, cursor string) (ids []uuid.UUID, pages int, lastCursor string) {
	t.Helper()
	for cursor != "" {
		rec := user.do(http.MethodGet, path+"&cursor="+url.QueryEscape(cursor), nil, nil)
		var page []struct {
			ID uuid.UUID `json:"id"`
		}
		user.expect(rec, http.StatusOK, &page)
		pages++
		for _, item := range page {
			ids = append(ids, item.ID)
		}
		lastCursor, cursor = cursor, rec.Header().Get("X-Next-Cursor")
		if cursor != "" && len(page) != 2 {
			t.Fatalf("%s: page %d has %d rows and a next cursor", path, pages, len(page))
		}
		if pages > 1000 {
			t.Fatalf("%s: no last page", path)
		}
	}
	return ids, pages, lastCursor
}

// Cursors only continue their own listing and filters, limits are bounded, and the scope rules
// still apply on every page.
func TestHTTPListPagingRules(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	h := newAPI(st, svc)
	admin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)

	cursorOf := func(user apiClient, path string) string {
		t.Helper()
		rec := user.do(http.MethodGet, path, nil, nil)
		user.expect(rec, http.StatusOK, nil)
		next := rec.Header().Get("X-Next-Cursor")
		if next == "" {
			t.Fatalf("%s: no next page", path)
		}
		return url.QueryEscape(next)
	}
	techs := cursorOf(admin, "/api/v1/admin/technicians?limit=1")
	customers := cursorOf(dispatcher, "/api/v1/customers?limit=1")
	types := cursorOf(admin, "/api/v1/service-types?limit=1")
	for _, tc := range []struct {
		user   apiClient
		path   string
		status int
	}{
		// A token from another listing, or the same listing with other filters.
		{admin, "/api/v1/dealerships?cursor=" + techs, http.StatusBadRequest},
		{admin, "/api/v1/admin/technicians?dealershipId=" + seed.DealerNorth.String() + "&cursor=" + techs, http.StatusBadRequest},
		{dispatcher, "/api/v1/customers?q=amelia&cursor=" + customers, http.StatusBadRequest},
		{dispatcher, "/api/v1/vehicles?cursor=" + customers, http.StatusBadRequest},
		{admin, "/api/v1/appointments?cursor=" + types, http.StatusBadRequest},
		{admin, "/api/v1/admin/outbox/failed?cursor=" + types, http.StatusBadRequest},
		{admin, "/api/v1/service-types?cursor=garbage", http.StatusBadRequest},
		// The same token with the same filters continues.
		{admin, "/api/v1/admin/technicians?limit=1&cursor=" + techs, http.StatusOK},
		{admin, "/api/v1/service-types?limit=1&cursor=" + types, http.StatusOK},
		// Limits: 1 to each listing's maximum.
		{admin, "/api/v1/dealerships?limit=0", http.StatusBadRequest},
		{admin, "/api/v1/dealerships?limit=201", http.StatusBadRequest},
		{admin, "/api/v1/service-types?limit=x", http.StatusBadRequest},
		{admin, "/api/v1/admin/technicians?limit=201", http.StatusBadRequest},
		{dispatcher, "/api/v1/customers?limit=101", http.StatusBadRequest},
		{dispatcher, "/api/v1/vehicles?limit=201", http.StatusBadRequest},
		{admin, "/api/v1/admin/outbox/failed?limit=201", http.StatusBadRequest},
		{dispatcher, "/api/v1/appointments?limit=201", http.StatusBadRequest},
		{admin, "/api/v1/dealerships?limit=200", http.StatusOK},
		// Scope: staff-only and admin-only lists stay so on every page.
		{customer, "/api/v1/customers?limit=1", http.StatusForbidden},
		{dispatcher, "/api/v1/admin/technicians?limit=1", http.StatusForbidden},
		{dispatcher, "/api/v1/admin/technicians?limit=1&cursor=" + techs, http.StatusForbidden},
	} {
		tc.user.expect(tc.user.do(http.MethodGet, tc.path, nil, nil), tc.status, nil)
	}

	// A filter whose target goes away between pages: the next page is an ordinary empty last page.
	gone := cloneRow(t, ctx, pool, "dealerships", seed.DealerRiver, map[string]any{"name": "Closing down"})
	for range 3 {
		cloneRow(t, ctx, pool, "technicians", seed.TechEve, map[string]any{"dealership_id": gone})
	}
	path := "/api/v1/admin/technicians?limit=1&dealershipId=" + gone.String()
	next := cursorOf(admin, path)
	for _, sql := range []string{
		`DELETE FROM technicians WHERE dealership_id = $1`,
		`DELETE FROM dealerships WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, sql, gone); err != nil {
			t.Fatal(err)
		}
	}
	var rest []json.RawMessage
	rec := admin.do(http.MethodGet, path+"&cursor="+next, nil, nil)
	admin.expect(rec, http.StatusOK, &rest)
	if len(rest) != 0 || rec.Header().Get("X-Next-Cursor") != "" {
		t.Fatalf("after the dealership went: %d rows, next %q", len(rest), rec.Header().Get("X-Next-Cursor"))
	}
	// A dealership admin replaying a group admin's cursor of another dealership: refused as before.
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, name, role, dealership_id, active)
		SELECT gen_random_uuid(), 'river-admin@scheduler.local', password_hash, 'River Admin', role, $1, true
		FROM users WHERE id = $2`, seed.DealerRiver, seed.DemoAdminID); err != nil {
		t.Fatal(err)
	}
	riverAdmin := login(t, h, "river-admin@scheduler.local", seed.DemoAdminPassword)
	north := "/api/v1/admin/technicians?limit=1&dealershipId=" + seed.DealerNorth.String()
	riverAdmin.expect(riverAdmin.do(http.MethodGet, north+"&cursor="+cursorOf(admin, north), nil, nil), http.StatusForbidden, nil)
}

// An appointment's history pages oldest first: an event recorded between two pages shows up on a
// later page, and nothing repeats.
func TestHTTPHistoryPages(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	h := newAPI(st, svc)
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	var appt uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM appointments WHERE dealership_id = $1 ORDER BY start_at, id LIMIT 1`, seed.DealerNorth).Scan(&appt); err != nil {
		t.Fatal(err)
	}
	addEvents := func(n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO appointment_history (appointment_id, version, event_type, actor_id, at, status, start_at, end_at, service_bay_id, technician_id)
			SELECT appointment_id, (SELECT max(version) FROM appointment_history WHERE appointment_id = $1) + k,
			       `+code(domain.EventAppointmentReassigned)+`, actor_id, at, status, start_at, end_at, service_bay_id, technician_id
			FROM (SELECT * FROM appointment_history WHERE appointment_id = $1 ORDER BY id LIMIT 1) h
			CROSS JOIN generate_series(1, $2::int) AS k`, appt, n); err != nil {
			t.Fatal(err)
		}
	}
	addEvents(4)
	path := "/api/v1/appointments/" + appt.String() + "/history?limit=2"
	rec := dispatcher.do(http.MethodGet, path, nil, nil)
	var page []struct {
		Version int `json:"version"`
	}
	dispatcher.expect(rec, http.StatusOK, &page)
	versions := []int{page[0].Version, page[1].Version}
	cursor := rec.Header().Get("X-Next-Cursor")
	addEvents(1) // recorded while the operator reads
	for cursor != "" {
		rec = dispatcher.do(http.MethodGet, path+"&cursor="+url.QueryEscape(cursor), nil, nil)
		page = nil
		dispatcher.expect(rec, http.StatusOK, &page)
		for _, c := range page {
			versions = append(versions, c.Version)
		}
		cursor = rec.Header().Get("X-Next-Cursor")
	}
	rows, err := pool.Query(ctx, `SELECT version FROM appointment_history WHERE appointment_id = $1 ORDER BY id`, appt)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 6 || !slices.Equal(versions, want) {
		t.Fatalf("paged versions %v, want %v", versions, want)
	}
	// The token is bound to the appointment it was read for.
	var other uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM appointments WHERE dealership_id = $1 AND id <> $2 LIMIT 1`, seed.DealerNorth, appt).Scan(&other); err != nil {
		t.Fatal(err)
	}
	rec = dispatcher.do(http.MethodGet, path, nil, nil)
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/appointments/"+other.String()+"/history?limit=2&cursor="+url.QueryEscape(rec.Header().Get("X-Next-Cursor")), nil, nil), http.StatusBadRequest, nil)
}
