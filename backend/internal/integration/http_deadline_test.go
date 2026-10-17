package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/migrate"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// metricLine returns the /metrics line of api starting with prefix, or "".
func metricLine(t *testing.T, api http.Handler, prefix string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(rec.Body.String()) {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// riverBooking is a booking request for Riverside at 11:30 today (the suite's clock is 00:00).
func riverBooking(t *testing.T) map[string]string {
	return map[string]string{
		"dealershipId":  seed.DealerRiver.String(),
		"vehicleId":     seed.VehicleA.String(),
		"serviceTypeId": seed.ServiceOil.String(),
		"startAt":       londonAt(t, 11, 30).Format(time.RFC3339),
	}
}

// REQUEST_TIMEOUT bounds a request queued behind a lock: 503 busy + Retry-After well before the
// pool's lock_timeout, and the retry with the same Idempotency-Key books once the lock is gone.
func TestHTTPRequestDeadlineIsBusy(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	api := newAPI(st, svc, httpapi.WithRequestTimeout(300*time.Millisecond))
	// Signed in on an instance without the short deadline: login's bcrypt (slow under -race) plus
	// its session insert would not fit in 300 ms.
	admin := login(t, api.peer("login"), seed.DemoAdminEmail, seed.DemoAdminPassword).on(api)
	key := map[string]string{"Idempotency-Key": "review:deadline:" + uuid.NewString()}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM dealerships WHERE id = $1 FOR UPDATE`, seed.DealerRiver); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rec := admin.do(http.MethodPost, "/api/v1/appointments", riverBooking(t), key)
	elapsed := time.Since(started)
	_ = tx.Rollback(ctx)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Fatalf("blocked booking = %d Retry-After %q %s, want 503 busy", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("blocked booking took %s: the request deadline did not cut the lock wait", elapsed)
	}
	if got := metricLine(t, api, `scheduler_bookings_total{instance_id="test",result="busy"}`); !strings.HasSuffix(got, " 1") {
		t.Fatalf("busy booking metric = %q, want 1", got)
	}

	var created domain.Appointment
	admin.expect(admin.do(http.MethodPost, "/api/v1/appointments", riverBooking(t), key), http.StatusCreated, &created)
	if created.ID == uuid.Nil {
		t.Fatal("retry did not book")
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE idempotency_key = $1`, key["Idempotency-Key"]); n != 1 {
		t.Fatalf("appointments for the key = %d, want 1", n)
	}
}

// A client that disconnects while its booking waits on a lock is a 499 and a "canceled" booking,
// never a 500 (no BookingServerErrors page for impatient clients).
func TestHTTPClientDisconnectIsNotA500(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	api := newAPI(st, svc)
	admin := login(t, api, seed.DemoAdminEmail, seed.DemoAdminPassword)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM dealerships WHERE id = $1 FOR UPDATE`, seed.DealerRiver); err != nil {
		t.Fatal(err)
	}
	reqCtx, disconnect := context.WithCancel(t.Context())
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequestWithContext(reqCtx, http.MethodPost, "/api/v1/appointments",
			strings.NewReader(`{"dealershipId":"`+seed.DealerRiver.String()+`","vehicleId":"`+seed.VehicleA.String()+
				`","serviceTypeId":"`+seed.ServiceOil.String()+`","startAt":"`+londonAt(t, 11, 30).Format(time.RFC3339)+`"}`))
		req.Header.Set("Authorization", "Bearer "+admin.token)
		req.Header.Set("Idempotency-Key", "review:gone:"+uuid.NewString())
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		done <- rec
	}()
	awaitLockWaiter(t, pool, "FROM dealerships WHERE id = $1 FOR SHARE")
	disconnect()
	rec := <-done
	if rec.Code != 499 {
		t.Fatalf("disconnected booking = %d %s, want 499", rec.Code, rec.Body.String())
	}
	if got := metricLine(t, api, `scheduler_bookings_total{instance_id="test",result="canceled"}`); !strings.HasSuffix(got, " 1") {
		t.Fatalf("canceled booking metric = %q, want 1", got)
	}
	if got := metricLine(t, api, `scheduler_bookings_total{instance_id="test",result="internal"}`); got != "" {
		t.Fatalf("internal booking metric = %q, want none", got)
	}
	if got := metricLine(t, api, `scheduler_http_requests_total{instance_id="test",method="POST",route="/api/v1/appointments",status="499"}`); !strings.HasSuffix(got, " 1") {
		t.Fatalf("499 request metric = %q, want 1", got)
	}
}

// Every CHECK constraint with a readable message exists in the schema (catches typos and renames),
// and a real violation surfaces as that message, without the constraint name.
func TestCheckConstraintMessages(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	var names []string
	rows, err := pool.Query(ctx, `SELECT conname FROM pg_constraint WHERE contype = 'c' AND connamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, name := range store.CheckMessageConstraints() {
		if !slices.Contains(names, name) {
			t.Errorf("check message for %q, but the schema has no such CHECK constraint", name)
		}
	}

	_, err = st.CreateVehicle(ctx, domain.Vehicle{ID: uuid.New(), CustomerID: seed.CustomerA, VIN: "REVXEW00000000001", Make: "Ford", Model: "T", Year: 1908})
	domainErr, ok := domain.AsError(err)
	if !ok || domainErr.Code != domain.CodeInvalid || domainErr.Message != "year must be 1980–2100" {
		t.Fatalf("year 1908 = %v, want invalid with a readable message", err)
	}
	if !strings.Contains(err.Error(), "vehicles_year_check") {
		t.Fatalf("error %q lost the constraint name for the server log", err)
	}
}

// The seed skips rows that collide with data registered before it ran (a VIN, an email, or a
// user id) instead of failing startup.
func TestSeedSkipsConflictingRows(t *testing.T) {
	t.Parallel()
	pool := newPool(t, openEmptyDB(t), store.DefaultPoolOptions())
	ctx := t.Context()
	if err := migrate.Up(ctx, pool); err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	hash, err := auth.HashPassword("someone-elses-password")
	if err != nil {
		t.Fatal(err)
	}
	other := uuid.New()
	mustExec(t, pool, `INSERT INTO customers (id, name, phone, email) VALUES ($1, 'Early Bird', '', 'early@example.com')`, other)
	mustExec(t, pool, `INSERT INTO vehicles (id, customer_id, vin, make, model, year) VALUES ($1, $2, 'WBA00000000000001', 'BMW', 'X1', 2020)`, uuid.New(), other)
	// The demo admin's email, under another id; and the demo dispatcher's id, under another email.
	mustExec(t, pool, `INSERT INTO users (id, email, password_hash, name, role) VALUES ($1, $2, $3, 'Early Admin', 1)`, uuid.New(), seed.DemoAdminEmail, hash)
	mustExec(t, pool, `INSERT INTO users (id, email, password_hash, name, role) VALUES ($1, 'ops@example.com', $2, 'Ops', 1)`, seed.DemoDispatcherID, hash)

	if err := seed.Run(ctx, st, nil, seed.WithClock(startOfLondonDay)); err != nil {
		t.Fatalf("seed over pre-existing rows: %v", err)
	}
	if got := scalar[string](t, pool, `SELECT name FROM users WHERE email = $1`, seed.DemoAdminEmail); got != "Early Admin" {
		t.Fatalf("admin email owner = %q, want the pre-existing account untouched", got)
	}
	if got := scalar[string](t, pool, `SELECT email FROM users WHERE id = $1`, seed.DemoDispatcherID); got != "ops@example.com" {
		t.Fatalf("dispatcher id email = %q, want the pre-existing account untouched", got)
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM vehicles WHERE id = $1`, seed.VehicleA); n != 0 {
		t.Fatal("seed vehicle inserted over a taken VIN")
	}
	if n := scalar[int](t, pool, `SELECT count(*)::int FROM dealerships`); n != 2 {
		t.Fatalf("dealerships = %d, want the 2 seeded", n)
	}
	// A second run is still a no-op.
	if err := seed.Run(ctx, st, nil, seed.WithClock(startOfLondonDay)); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
}
