package integration_test

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// GET /appointments?when=upcoming lists, soonest first, the confirmed appointments not yet ended and
// every checked-in or in-progress one whatever its end_at (the car is on site: a checked-in car
// left past its booked end, or an overrun flagged without being extended, still holds its bay and
// technician); ?when=past lists every other one newest first, so upcoming + past = all. Following
// X-Next-Cursor visits each row of a listing exactly once, in its order, under every scope (group
// admin, dealership dispatcher, customer). A cursor only continues the listing it came from.
func TestHTTPAppointmentsUpcomingAndPast(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	// Each seeded row (n of them) becomes the template of 9 clones, one per day, the n-th in the
	// n-th hour of that day: every window is distinct, so holding clones never collide under
	// GiST. Statuses mix holding (1, 2, 3) and released (4, 5, 6) rows on both sides of now, which
	// falls inside the first row's window on day 4 (started, not ended: still upcoming). Before
	// now: confirmed and ended (day 0: past), checked in and past its end (day 2: upcoming), in
	// progress and flagged overrunning without being extended (day 3: upcoming). The second row's
	// clones move to Riverside, outside the dispatcher's scope. The seeded rows themselves are
	// cancelled (past).
	base := time.Date(2031, 3, 3, 0, 0, 0, 0, time.UTC)
	now := base.Add(4*24*time.Hour + 15*time.Minute)
	var seeded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM appointments`).Scan(&seeded); err != nil {
		t.Fatal(err)
	}
	if seeded < 2 || seeded > 24 {
		t.Fatalf("%d seeded appointments; the clone grid needs 2–24", seeded)
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusCancelled)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO appointments
		SELECT (jsonb_populate_record(NULL::appointments, to_jsonb(a) || jsonb_build_object(
			'id', gen_random_uuid(), 'idempotency_key', gen_random_uuid()::text,
			'status', (ARRAY[1, 5, 2, 3, 1, 6, 2, 1, 4])[k + 1],
			'start_at', $1::timestamptz + k * interval '1 day' + (a.rn - 1) * interval '1 hour',
			'end_at', $1::timestamptz + k * interval '1 day' + (a.rn - 1) * interval '1 hour' + interval '30 minutes')
			|| CASE WHEN a.rn = 2 THEN jsonb_build_object('dealership_id', $2::uuid, 'service_bay_id', $3::uuid, 'technician_id', $4::uuid)
			        ELSE '{}'::jsonb END)).*
		FROM (SELECT *, row_number() OVER (ORDER BY id) AS rn FROM appointments) a
		CROSS JOIN generate_series(0, 8) AS k`, base, seed.DealerRiver, seed.BayR2, seed.TechEve); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET overrun_at = $1 WHERE status = `+code(domain.StatusInProgress)+` AND end_at <= $1`, now); err != nil {
		t.Fatal(err)
	}

	svc := booking.New(st, nil, booking.WithClock(func() time.Time { return now }))
	h := newAPI(st, svc)
	scopes := []struct {
		name  string
		user  apiClient
		where string // the scope as SQL over appointments a
	}{
		{"group admin", login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword), "TRUE"},
		{"dealership dispatcher", login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword), fmt.Sprintf("a.dealership_id = '%s'", seed.DealerNorth)},
		{"customer", login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword), fmt.Sprintf("a.customer_id = '%s'", seed.CustomerA)},
	}
	var groupWide int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM appointments`).Scan(&groupWide); err != nil {
		t.Fatal(err)
	}
	listings := []struct {
		when, oracle string
	}{
		{"upcoming", `(a.status IN (` + onSiteCodes + `) OR (a.status = ` + code(domain.StatusConfirmed) + ` AND a.end_at > $1)) ORDER BY a.start_at, a.id`},
		{"past", `NOT (a.status IN (` + onSiteCodes + `) OR (a.status = ` + code(domain.StatusConfirmed) + ` AND a.end_at > $1)) ORDER BY a.start_at DESC, a.id DESC`},
	}
	for _, scope := range scopes {
		total := 0
		for _, listing := range listings {
			rows, err := pool.Query(ctx, `SELECT a.id FROM appointments a WHERE `+scope.where+` AND `+listing.oracle, now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
			if err != nil {
				t.Fatal(err)
			}
			if len(want) < 3 {
				t.Fatalf("%s %s: only %d rows, too few to page through", scope.name, listing.when, len(want))
			}
			total += len(want)
			got, pages, lastCursor := pageAll(t, scope.user, "/api/v1/appointments?limit=2&when="+listing.when)
			if !slices.Equal(got, want) {
				t.Fatalf("%s %s: paged %d ids over %d pages, want %d in order\n got %v\nwant %v", scope.name, listing.when, len(got), pages, len(want), got, want)
			}
			if wantPages := (len(want) + 1) / 2; pages != wantPages {
				t.Fatalf("%s %s: pages = %d, want %d", scope.name, listing.when, pages, wantPages)
			}
			// The cursor continues only its own listing.
			for _, other := range []string{"", "upcoming", "past"} {
				if other != listing.when {
					rec := scope.user.do(http.MethodGet, "/api/v1/appointments?limit=2&when="+other+"&cursor="+url.QueryEscape(lastCursor), nil, nil)
					scope.user.expect(rec, http.StatusBadRequest, nil)
				}
			}
		}
		var all int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM appointments a WHERE `+scope.where).Scan(&all); err != nil {
			t.Fatal(err)
		}
		if total != all {
			t.Fatalf("%s: upcoming + past = %d rows, want all %d", scope.name, total, all)
		}
		if scope.name != "group admin" && all == groupWide {
			t.Fatalf("%s sees all %d appointments: the scope filters nothing", scope.name, all)
		}
	}
	admin := scopes[0].user
	admin.expect(admin.do(http.MethodGet, "/api/v1/appointments?when=soon", nil, nil), http.StatusBadRequest, nil)

	// The rule itself, independent of the oracle above: work on site is upcoming however long ago
	// it was due to end; a confirmed appointment that ended, and released work, are past.
	upcoming, _, _ := pageAll(t, admin, "/api/v1/appointments?limit=2&when=upcoming")
	past, _, _ := pageAll(t, admin, "/api/v1/appointments?limit=2&when=past")
	for _, c := range []struct {
		name, where string
		in          []uuid.UUID
	}{
		{"checked in, past its end", `status = ` + code(domain.StatusCheckedIn) + ` AND end_at <= $1`, upcoming},
		{"in progress, flagged overrunning, past its end", `status = ` + code(domain.StatusInProgress) + ` AND overrun_at IS NOT NULL AND end_at <= $1`, upcoming},
		{"confirmed, ended", `status = ` + code(domain.StatusConfirmed) + ` AND end_at <= $1`, past},
		{"completed, still ahead", `status = ` + code(domain.StatusCompleted) + ` AND start_at > $1`, past},
	} {
		ids := queryIDs(t, ctx, pool, `SELECT id FROM appointments WHERE `+c.where, now)
		if len(ids) == 0 {
			t.Fatalf("%s: no such row to check", c.name)
		}
		for _, id := range ids {
			if !slices.Contains(c.in, id) {
				t.Fatalf("%s: %s missing from its listing", c.name, id)
			}
		}
	}

	// Rows leaving "upcoming" between pages — the cursor row, the one before it, the one right
	// after it (cancelled), and the last one (completed) — neither break the paging nor shift it:
	// the rest comes once, in order.
	want := queryIDs(t, ctx, pool, `SELECT a.id FROM appointments a WHERE `+listings[0].oracle, now)
	rec := admin.do(http.MethodGet, "/api/v1/appointments?when=upcoming&limit=2", nil, nil)
	var page []domain.Appointment
	admin.expect(rec, http.StatusOK, &page)
	if len(page) != 2 || page[0].ID != want[0] || page[1].ID != want[1] {
		t.Fatalf("first upcoming page %v, want %v", page, want[:2])
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusCancelled)+` WHERE id = ANY($1)`, want[:3]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusCompleted)+` WHERE id = $1`, want[len(want)-1]); err != nil {
		t.Fatal(err)
	}
	rest, _, _ := pageAllIDs(t, admin, "/api/v1/appointments?when=upcoming&limit=2", rec.Header().Get("X-Next-Cursor"))
	if wantRest := want[3 : len(want)-1]; !slices.Equal(rest, wantRest) {
		t.Fatalf("upcoming after rows left it: got %v, want %v", rest, wantRest)
	}
}

// pageAll follows X-Next-Cursor from path to the last page and returns the ids in order, the
// number of pages, and the last cursor it was given.
func pageAll(t *testing.T, user apiClient, path string) (ids []uuid.UUID, pages int, lastCursor string) {
	t.Helper()
	cursor := ""
	for {
		p := path
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := user.do(http.MethodGet, p, nil, nil)
		var page []domain.Appointment
		user.expect(rec, http.StatusOK, &page)
		pages++
		for _, a := range page {
			ids = append(ids, a.ID)
		}
		next := rec.Header().Get("X-Next-Cursor")
		if next == "" {
			return ids, pages, lastCursor
		}
		if len(page) != 2 || pages > 1000 {
			t.Fatalf("%s page %d: %d items with a next cursor", path, pages, len(page))
		}
		cursor, lastCursor = next, next
	}
}
