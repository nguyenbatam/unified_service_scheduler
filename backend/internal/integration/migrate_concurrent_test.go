package integration_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/migrate"
)

// Two instances starting together on an empty database: the advisory lock serialises them, so
// both succeed and every migration is applied exactly once. The applied-check and each apply run
// on the session that holds the lock.
func TestConcurrentMigrateUpAppliesEachOnce(t *testing.T) {
	t.Parallel()
	cfg := openEmptyDB(t)
	// Single-connection pools: Up must not need a second connection while it holds the lock one
	// (it used to run the checks and applies on other pool connections, which would wait forever).
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg.MaxConns = 1
	pools := make([]*pgxpool.Pool, 2)
	for i := range pools {
		pool, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		pools[i] = pool
	}
	errs := make([]error, len(pools))
	var wg sync.WaitGroup
	for i, pool := range pools {
		wg.Go(func() { errs[i] = migrate.Up(ctx, pool) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("instance %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir("../migrate/migrations")
	if err != nil {
		t.Fatal(err)
	}
	files := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files++
		}
	}
	if n := scalar[int](t, pools[0], `SELECT count(*)::int FROM schema_migrations`); n != files {
		t.Fatalf("schema_migrations has %d rows, want %d", n, files)
	}
	// A third start is a no-op.
	if err := migrate.Up(ctx, pools[1]); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	for _, idx := range []string{"vehicles_customer_idx", "technicians_dealership_name_idx"} {
		if !scalar[bool](t, pools[0], `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, idx) {
			t.Fatalf("index %s missing", idx)
		}
	}
}
