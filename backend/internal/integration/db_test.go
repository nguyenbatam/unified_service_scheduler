package integration_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/migrate"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/testdb"
)

// Every test gets a database of its own, copied (CREATE DATABASE … TEMPLATE) from a template this
// binary migrates and seeds once, so tests can run in parallel and none pays for migrations and
// bcrypt seeding. When the TEST_DATABASE_URL role lacks CREATEDB, openTestDB falls back to
// resetting the entry database's schema per test, one test at a time.

// testPoolConns is wider than the largest race in this package (20 goroutines) plus the
// connections a test holds open itself.
const testPoolConns = 32

// defaultMaxDBs caps how many test databases are open at once (ITEST_MAX_DBS overrides it). Pools
// open connections lazily, but a race test can use all testPoolConns, and some open further pools
// on their database (newPoolLike: up to 20 more each): 5 × (32 + 20) = 260 stays under the server's
// max_connections (300) with room for the template build and other clients (7 would be ~364).
// Parallel tests beyond the cap wait for a slot, whatever -parallel says.
const defaultMaxDBs = 5

var suite struct {
	once  sync.Once
	err   error
	srv   *testdb.Server
	tpl   string        // template database ("" in fallback mode)
	slots chan struct{} // one token per open test database
}

func TestMain(m *testing.M) {
	code := m.Run()
	if suite.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		if suite.tpl != "" {
			if err := suite.srv.Drop(ctx, suite.tpl); err != nil {
				log.Printf("integration: %v", err)
			}
		}
		cancel()
		suite.srv.Close()
	}
	os.Exit(code)
}

// setupSuite connects to TEST_DATABASE_URL's server and builds the template, once per binary.
func setupSuite(t testing.TB) {
	t.Helper()
	dsn := testdb.DSN(t)
	suite.once.Do(func() { suite.err = buildSuite(dsn) })
	if suite.err != nil {
		t.Fatalf("test database setup: %v", suite.err)
	}
}

func buildSuite(dsn string) error {
	// Not t.Context(): the template outlives the first test.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv, err := testdb.Open(ctx, dsn)
	if err != nil {
		return err
	}
	suite.srv = srv
	maxDBs := defaultMaxDBs
	if v := os.Getenv("ITEST_MAX_DBS"); v != "" {
		if maxDBs, err = strconv.Atoi(v); err != nil || maxDBs < 1 {
			return fmt.Errorf("ITEST_MAX_DBS=%q: want a positive integer", v)
		}
	}
	if !srv.CanCreate() {
		log.Printf("integration: the TEST_DATABASE_URL role lacks CREATEDB: resetting the entry database per test, one test at a time")
		suite.slots = make(chan struct{}, 1)
		return nil
	}
	suite.slots = make(chan struct{}, maxDBs)
	if err := srv.Sweep(ctx); err != nil {
		log.Printf("integration: dropping stale test databases: %v", err)
	}
	tpl := testdb.NewName("tpl")
	if err := srv.Create(ctx, tpl, ""); err != nil {
		return err
	}
	suite.tpl = tpl
	tplDSN, err := srv.DSNFor(tpl)
	if err != nil {
		return err
	}
	cfg, err := pgxpool.ParseConfig(tplDSN)
	if err != nil {
		return err
	}
	store.ConfigurePool(cfg, store.DefaultPoolOptions())
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return err
	}
	// Copies need the template free of sessions: close the pool before the first CREATE … TEMPLATE.
	defer pool.Close()
	return migrateAndSeed(ctx, pool)
}

// migrateAndSeed applies every migration and seeds the demo data with the suite's frozen date:
// demo bookings land on today's board (Northside 09:00 / 10:00), as the tests expect.
func migrateAndSeed(ctx context.Context, pool *pgxpool.Pool) error {
	if err := migrate.Up(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := seed.Run(ctx, store.New(pool), nil, seed.WithClock(startOfLondonDay)); err != nil {
		return fmt.Errorf("seed: %w", err)
	}
	return nil
}

// slotHolders names the tests holding a database slot.
var slotHolders = struct {
	sync.Mutex
	names map[string]bool
}{names: map[string]bool{}}

// acquireDBSlot waits for a free database slot and frees it when t ends (after the database is
// dropped: cleanups run last-registered first). One database per test tree: a test (or a subtest
// of one) that already holds a slot asking for another could wait forever once every slot is
// taken, so that fails at once instead.
func acquireDBSlot(t testing.TB) {
	t.Helper()
	name := t.Name()
	slotHolders.Lock()
	for held := range slotHolders.names {
		if held == name || strings.HasPrefix(name, held+"/") {
			slotHolders.Unlock()
			t.Fatalf("%s already holds a test database (held by %s): open one per test", name, held)
		}
	}
	slotHolders.Unlock()
	suite.slots <- struct{}{}
	slotHolders.Lock()
	slotHolders.names[name] = true
	slotHolders.Unlock()
	t.Cleanup(func() {
		slotHolders.Lock()
		delete(slotHolders.names, name)
		slotHolders.Unlock()
		<-suite.slots
	})
}

// testDBConfig is a pool config for a database of t's own: a copy of the template (seeded) or an
// empty one. In fallback mode it is the entry database, reset to the same state.
func testDBConfig(t testing.TB, seeded bool) *pgxpool.Config {
	t.Helper()
	setupSuite(t)
	acquireDBSlot(t)
	if suite.tpl == "" {
		return resetEntryDB(t, seeded)
	}
	tpl := ""
	if seeded {
		tpl = suite.tpl
	}
	cfg, err := pgxpool.ParseConfig(suite.srv.Fresh(t, tpl))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// resetEntryDB is the fallback without CREATEDB: drop and recreate the entry database's public
// schema, then migrate and seed it when seeded.
func resetEntryDB(t testing.TB, seeded bool) *pgxpool.Config {
	t.Helper()
	ctx := t.Context()
	cfg, err := pgxpool.ParseConfig(suite.srv.EntryDSN())
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;
		GRANT ALL ON SCHEMA public TO public;
	`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if seeded {
		if err := migrateAndSeed(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func openTestStore(t *testing.T) (*store.Store, *booking.Service, context.Context) {
	t.Helper()
	_, st, svc, ctx := openTestDB(t)
	return st, svc, ctx
}

// openTestDB gives t a migrated, seeded database of its own and returns its pool (for direct
// assertions), a store, and a service whose clock reads 00:00 London on the suite's date.
func openTestDB(t testing.TB) (*pgxpool.Pool, *store.Store, *booking.Service, context.Context) {
	t.Helper()
	cfg := testDBConfig(t, true)
	// Production session limits (lock/statement/idle-in-TX timeouts), so a test that queues on a
	// lock behaves as a request would; enough connections that a 20-goroutine race really runs in
	// parallel instead of queueing for the pool.
	opts := store.DefaultPoolOptions()
	opts.MaxConns = testPoolConns
	pool := newPool(t, cfg, opts)
	st := store.New(pool)
	return pool, st, atStartOfLondonDay(st), context.Background()
}

// openEmptyDB gives t an empty database of its own (no migrations, no seed) for migration tests.
func openEmptyDB(t *testing.T) *pgxpool.Config {
	t.Helper()
	return testDBConfig(t, false)
}

// newPoolLike opens another pool on the same database as like (t's own), with opts.
func newPoolLike(t *testing.T, like *pgxpool.Pool, opts store.PoolOptions) *pgxpool.Pool {
	t.Helper()
	return newPool(t, like.Config(), opts)
}

func newPool(t testing.TB, cfg *pgxpool.Config, opts store.PoolOptions) *pgxpool.Pool {
	t.Helper()
	store.ConfigurePool(cfg, opts)
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered after the database's drop, so it runs first: the pool closes, then the drop.
	t.Cleanup(pool.Close)
	return pool
}
