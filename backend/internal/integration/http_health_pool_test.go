package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A saturated request pool is load, not an outage: readiness probes Postgres over its own
// connection (store.Ping), so with every pooled connection held it still answers 200 with
// postgres "ok", reporting the pool as "saturated" (status degraded), instead of timing out on
// the pool and reporting a healthy Postgres down.
func TestHealthReportsSaturatedPoolNotDown(t *testing.T) {
	t.Parallel()
	pool, _, _, ctx := openTestDB(t)
	opts := store.DefaultPoolOptions()
	opts.MaxConns = 2
	tight := newPoolLike(t, pool, opts)
	st := store.New(tight)
	t.Cleanup(st.Close)
	api := newAPI(st, booking.New(st, nil))

	var held []*pgxpool.Conn
	for range opts.MaxConns {
		conn, err := tight.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	code, body := readiness(t, api)
	if code != http.StatusOK || body.Status != "degraded" || body.Checks["postgres"] != "ok" || body.Checks["pool"] != "saturated" {
		t.Fatalf("ready with the pool full = %d %+v, want 200 degraded (postgres ok, pool saturated)", code, body)
	}
	for _, conn := range held {
		conn.Release()
	}
	if st.PoolSaturated() {
		t.Fatal("pool still saturated after every connection was released")
	}
}

// The probe connection is reused, and a broken one (Postgres restarted, backend killed) is
// replaced within the same probe, so readiness never reports down while Postgres is back.
func TestHealthProbeReplacesABrokenConnection(t *testing.T) {
	t.Parallel()
	pool, _, _, ctx := openTestDB(t)
	// The probe dials with its store's pool config: a pool of its own under an application_name
	// of its own, never queried and without MinConns, makes every backend of that name the probe's
	// (counting the whole database also counts the test pool filling its MinConns in the background).
	cfg := pool.Config()
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = "health-probe-test"
	probePool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probePool.Close)
	st := store.New(probePool)
	t.Cleanup(st.Close)
	probeBackends := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity
			WHERE datname = current_database() AND application_name = 'health-probe-test'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for i := range 2 {
		if err := st.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		if n := probeBackends(); n != 1 {
			t.Fatalf("after probe %d: %d probe connections, want the one reused", i+1, n)
		}
	}
	// Kill every other backend of this database: the probe's connection among them (the pool
	// replaces its own).
	mustExec(t, pool, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`)
	if err := st.Ping(ctx); err != nil {
		t.Fatalf("probe after its connection was killed: %v", err)
	}
}

type readyBody struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func readiness(t *testing.T, api *testAPI) (int, readyBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health/ready", nil))
	var body readyBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v (%s)", err, rec.Body.String())
	}
	return rec.Code, body
}
