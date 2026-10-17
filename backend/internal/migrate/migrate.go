package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// lockKey serialises migrations across instances starting at the same time.
const lockKey = 7_262_001

func Up(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// The pool's lock/statement timeouts are for requests; a replica waiting for another one's
	// migrations, or DDL waiting for old-version queries to finish, must not fail startup.
	if _, err := conn.Exec(ctx, `SET lock_timeout = 0; SET statement_timeout = 0; SET idle_in_transaction_session_timeout = 0`); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `RESET lock_timeout; RESET statement_timeout; RESET idle_in_transaction_session_timeout`)
	}()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	// Everything below runs on conn: the session that holds the advisory lock (so the lock covers
	// the applied-check and the apply) and had its request timeouts cleared above. Other pool
	// connections have neither.
	_, err = conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	unknown, err := unknownApplied(ctx, conn.Conn(), names)
	if err != nil {
		return err
	}

	for _, name := range names {
		var applied bool
		err := conn.QueryRow(ctx, `SELECT true FROM schema_migrations WHERE version = $1`, name).Scan(&applied)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if applied {
			continue
		}
		if len(unknown) > 0 {
			return fmt.Errorf("database records migrations this build does not ship (%s) and lacks %s: it was created by another migration layout; recreate it (locally: ./scripts/up.sh)",
				strings.Join(unknown, ", "), name)
		}
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := apply(ctx, conn.Conn(), name, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// unknownApplied lists the migrations the database records but this binary does not ship. That is
// fine while every shipped file is applied too (an older replica restarting after a newer one
// migrated, in a rolling deploy), but Up refuses to apply a file on top of them: the database was
// built by another migration layout (e.g. the history before it was squashed into 001_schema.sql),
// and applying ours would fail halfway or leave a schema the code does not expect.
func unknownApplied(ctx context.Context, conn *pgx.Conn, names []string) ([]string, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations WHERE NOT (version = ANY($1)) ORDER BY version`, names)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// apply runs one migration and records it in a TX on the lock-holding connection.
func apply(ctx context.Context, conn *pgx.Conn, name, body string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = 0; SET LOCAL statement_timeout = 0; SET LOCAL idle_in_transaction_session_timeout = 0`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, body); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
