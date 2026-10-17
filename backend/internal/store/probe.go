package store

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// probeCloseTimeout bounds closing a dead probe connection (its Terminate message).
const probeCloseTimeout = time.Second

// probeConn is the readiness probe's own connection, outside the request pool: a pool.Ping waits
// for a free pooled connection, so under saturation (every connection busy, requests queued) it
// timed out and reported a healthy Postgres as down. One connection per instance, opened on the
// first probe and reused; a broken one is replaced within the same probe.
type probeConn struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

// Ping reports whether Postgres answers, over the probe connection (never the request pool, so
// it measures reachability, not pool load: see PoolSaturated).
func (s *Store) Ping(ctx context.Context) error {
	return s.probe.ping(ctx, func(ctx context.Context) (*pgx.Conn, error) {
		return pgx.ConnectConfig(ctx, s.pool.Config().ConnConfig)
	})
}

// PoolSaturated reports whether every request-pool connection is in use right now, so new
// requests queue for one (reachability is Ping's business).
func (s *Store) PoolSaturated() bool {
	st := s.pool.Stat()
	return st.MaxConns() > 0 && st.AcquiredConns() >= st.MaxConns()
}

// Close closes the probe connection (the pool belongs to the caller). Safe to call twice.
func (s *Store) Close() {
	s.probe.close(context.Background())
}

func (p *probeConn) ping(ctx context.Context, dial func(context.Context) (*pgx.Conn, error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil {
		if p.conn.Ping(ctx) == nil {
			return nil
		}
		// Broken (Postgres restarted, network blip) or slow: replace it once within this probe,
		// so a restarted Postgres reads as up on the first probe after it is back.
		p.closeLocked(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	if err := conn.Ping(ctx); err != nil {
		closeConn(ctx, conn)
		return err
	}
	p.conn = conn
	return nil
}

func (p *probeConn) close(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked(ctx)
}

func (p *probeConn) closeLocked(ctx context.Context) {
	if p.conn != nil {
		closeConn(ctx, p.conn)
		p.conn = nil
	}
}

func closeConn(ctx context.Context, conn *pgx.Conn) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeCloseTimeout)
	defer cancel()
	_ = conn.Close(cctx)
}
