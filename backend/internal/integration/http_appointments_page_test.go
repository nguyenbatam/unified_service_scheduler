package integration_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// GET /appointments pages by (start_at, id) newest first: following X-Next-Cursor visits every
// row exactly once, in order, even when most rows share one start time.
func TestHTTPAppointmentsKeysetPagination(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	// Release every hold, then clone the rows twice (new ids and idempotency keys) at one start
	// time: lots of ties, and cancelled rows are outside the GiST exclusions.
	if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusCancelled)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := pool.Exec(ctx, `
			INSERT INTO appointments
			SELECT (jsonb_populate_record(NULL::appointments, to_jsonb(a) || jsonb_build_object(
				'id', gen_random_uuid(), 'idempotency_key', gen_random_uuid()::text,
				'start_at', '2030-01-07T09:00:00Z', 'end_at', '2030-01-07T10:00:00Z'))).*
			FROM appointments a`); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT id FROM appointments ORDER BY start_at DESC, id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	if len(want) < 9 {
		t.Fatalf("only %d appointments to page through", len(want))
	}

	h := newAPI(st, svc)
	admin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)
	var got []uuid.UUID
	cursor, pages := "", 0
	for {
		path := "/api/v1/appointments?limit=2"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rec := admin.do(http.MethodGet, path, nil, nil)
		var page []domain.Appointment
		admin.expect(rec, http.StatusOK, &page)
		pages++
		for _, a := range page {
			got = append(got, a.ID)
		}
		cursor = rec.Header().Get("X-Next-Cursor")
		if cursor == "" {
			break
		}
		if len(page) != 2 || pages > len(want) {
			t.Fatalf("page %d: %d items with a next cursor", pages, len(page))
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("paged ids (%d over %d pages) differ from the full order (%d)\n got %v\nwant %v", len(got), pages, len(want), got, want)
	}
	if wantPages := (len(want) + 1) / 2; pages != wantPages {
		t.Fatalf("pages = %d, want %d (no empty trailing page)", pages, wantPages)
	}

	// Scoping still applies on later pages: a customer sees only their own rows.
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	var mine []domain.Appointment
	rec := customer.do(http.MethodGet, "/api/v1/appointments?limit=1", nil, nil)
	customer.expect(rec, http.StatusOK, &mine)
	for next := rec.Header().Get("X-Next-Cursor"); next != ""; next = rec.Header().Get("X-Next-Cursor") {
		rec = customer.do(http.MethodGet, "/api/v1/appointments?limit=1&cursor="+url.QueryEscape(next), nil, nil)
		var page []domain.Appointment
		customer.expect(rec, http.StatusOK, &page)
		mine = append(mine, page...)
	}
	for _, a := range mine {
		if a.CustomerID != seed.CustomerA {
			t.Fatalf("customer page holds %s's appointment", a.CustomerID)
		}
	}

	for _, bad := range []string{"not-base64!", "Zm9v", "MjAzMC0wMS0wN1QwOTowMDowMFp8bm90LWEtdXVpZA"} {
		admin.expect(admin.do(http.MethodGet, "/api/v1/appointments?cursor="+bad, nil, nil), http.StatusBadRequest, nil)
	}
}
