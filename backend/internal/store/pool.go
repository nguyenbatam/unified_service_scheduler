package store

import (
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolOptions bound how long a request can wait on Postgres. Without lock_timeout, bookings that
// share-lock a dealership queue behind an admin's FOR UPDATE and can exhaust the pool; with it
// they fail fast (55P03 → 503 busy, retry) instead.
type PoolOptions struct {
	MaxConns         int32
	LockTimeout      time.Duration
	StatementTimeout time.Duration
	IdleInTxTimeout  time.Duration
}

func DefaultPoolOptions() PoolOptions {
	return PoolOptions{MaxConns: 20, LockTimeout: 3 * time.Second, StatementTimeout: 15 * time.Second, IdleInTxTimeout: 30 * time.Second}
}

// ConfigurePool applies o as session defaults on every pooled connection (no extra round trip
// per transaction). Migrations override them (migrate.Up).
func ConfigurePool(cfg *pgxpool.Config, o PoolOptions) {
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	if cfg.MinConns == 0 {
		cfg.MinConns = 2
	}
	params := cfg.ConnConfig.RuntimeParams
	set := func(name string, d time.Duration) {
		if d > 0 {
			params[name] = strconv.FormatInt(d.Milliseconds(), 10)
		}
	}
	set("lock_timeout", o.LockTimeout)
	set("statement_timeout", o.StatementTimeout)
	set("idle_in_transaction_session_timeout", o.IdleInTxTimeout)
}
