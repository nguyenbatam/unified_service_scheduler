// Package testdb gives integration tests databases of their own on the TEST_DATABASE_URL server:
// an empty one, or a copy of a template built once per test binary (CREATE DATABASE … TEMPLATE),
// each dropped when its test ends. It is imported only from _test.go files.
//
// TEST_DATABASE_URL names the entry database: it is used to create and drop databases (prefixed
// "itest_", so they never collide with the application's or another suite's) and is never written
// to. A role without CREATEDB cannot do that; callers then fall back to resetting the entry
// database's schema and run one test at a time (Server.CanCreate reports which).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Prefix starts the name of every database this package creates.
const Prefix = "itest_"

// staleAfter is how old an itest_ database must be before Sweep drops it: far longer than any test
// binary runs (CI's go test timeout is 15 minutes), so only leftovers of killed runs qualify.
const staleAfter = 2 * time.Hour

// DSN returns TEST_DATABASE_URL. Without it the test is skipped, or failed under
// REQUIRE_INTEGRATION=1.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatal("TEST_DATABASE_URL is required when REQUIRE_INTEGRATION=1")
		}
		t.Skip("TEST_DATABASE_URL not set")
	}
	return dsn
}

// RedisURL returns TEST_REDIS_URL. Without it the test is skipped, or failed under
// REQUIRE_INTEGRATION=1; there is no fallback to a default Redis, which may be the dev stack's.
func RedisURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_REDIS_URL")
	if u == "" {
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			t.Fatal("TEST_REDIS_URL is required when REQUIRE_INTEGRATION=1")
		}
		t.Skip("TEST_REDIS_URL not set")
	}
	return u
}

// Server creates and drops databases through a connection to the entry database.
type Server struct {
	dsn       string
	admin     *pgxpool.Pool
	canCreate bool
}

// Open connects to the entry database dsn and checks whether its role may create databases.
func Open(ctx context.Context, dsn string) (*Server, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 4
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	var canCreate bool
	if err := admin.QueryRow(ctx,
		`SELECT rolcreatedb OR rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&canCreate); err != nil {
		admin.Close()
		return nil, fmt.Errorf("check CREATEDB: %w", err)
	}
	return &Server{dsn: dsn, admin: admin, canCreate: canCreate}, nil
}

// Close closes the entry connection.
func (s *Server) Close() { s.admin.Close() }

// CanCreate reports whether the entry role may create databases.
func (s *Server) CanCreate() bool { return s.canCreate }

// EntryDSN is the entry database's DSN (TEST_DATABASE_URL).
func (s *Server) EntryDSN() string { return s.dsn }

// NewName returns a fresh database name: Prefix, kind, creation time (read by Sweep), random suffix.
func NewName(kind string) string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return fmt.Sprintf("%s%s_%d_%s", Prefix, kind, time.Now().Unix(), hex.EncodeToString(b[:]))
}

// Create creates database name, empty (template "") or as a copy of template. A template must have
// no open connections while it is copied.
func (s *Server) Create(ctx context.Context, name, template string) error {
	sql := `CREATE DATABASE ` + pgx.Identifier{name}.Sanitize()
	if template != "" {
		sql += ` TEMPLATE ` + pgx.Identifier{template}.Sanitize()
	}
	if _, err := s.admin.Exec(ctx, sql); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// Drop drops database name, closing any session still connected to it.
func (s *Server) Drop(ctx context.Context, name string) error {
	if _, err := s.admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// Sweep drops itest_ databases left behind by killed runs (older than staleAfter, by the time in
// their name). Databases of runs still in progress are younger and stay.
func (s *Server) Sweep(ctx context.Context) error {
	rows, err := s.admin.Query(ctx, `SELECT datname FROM pg_database WHERE starts_with(datname, $1)`, Prefix)
	if err != nil {
		return fmt.Errorf("list test databases: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("list test databases: %w", err)
	}
	var errs []error
	for _, name := range names {
		if created, ok := createdAt(name); ok && time.Since(created) > staleAfter {
			errs = append(errs, s.Drop(ctx, name))
		}
	}
	return errors.Join(errs...)
}

// createdAt reads the creation time NewName put in name.
func createdAt(name string) (time.Time, bool) {
	parts := strings.Split(strings.TrimPrefix(name, Prefix), "_")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(sec, 0), true
}

// DSNFor is the entry DSN pointed at database name instead.
func (s *Server) DSNFor(name string) (string, error) {
	if strings.HasPrefix(s.dsn, "postgres://") || strings.HasPrefix(s.dsn, "postgresql://") {
		u, err := url.Parse(s.dsn)
		if err != nil {
			return "", fmt.Errorf("parse TEST_DATABASE_URL: %w", err)
		}
		u.Path = "/" + name
		u.RawPath = ""
		return u.String(), nil
	}
	// keyword=value form: a later keyword overrides an earlier one.
	return s.dsn + " dbname=" + name, nil
}

// Fresh creates an empty database (template "") or a copy of template for t, dropped in
// t.Cleanup, and returns its DSN. It fails t when the role cannot create databases.
func (s *Server) Fresh(t testing.TB, template string) string {
	t.Helper()
	if !s.canCreate {
		t.Fatal("testdb: the TEST_DATABASE_URL role lacks CREATEDB")
	}
	name := NewName("db")
	if err := s.Create(t.Context(), name, template); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// t.Context() is already cancelled when cleanups run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()
		if err := s.Drop(ctx, name); err != nil {
			t.Errorf("testdb: %v", err)
		}
	})
	dsn, err := s.DSNFor(name)
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// FreshDSN opens the server behind TEST_DATABASE_URL for one test and returns the DSN of an empty
// database of its own, dropped when the test ends. It skips like DSN, and falls back to the entry
// database itself (logged) when the role cannot create databases; the caller then shares it.
func FreshDSN(t testing.TB) (dsn string, own bool) {
	t.Helper()
	entry := DSN(t)
	srv, err := Open(t.Context(), entry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	if !srv.CanCreate() {
		t.Log("testdb: role lacks CREATEDB; using the entry database")
		return entry, false
	}
	return srv.Fresh(t, ""), true
}
