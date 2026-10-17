package store

import (
	"context"
	"time"
)

// notifyTimeout bounds one autocommit NOTIFY sent by RunOutboxNotifier.
const notifyTimeout = 2 * time.Second

// outboxSignals carries "a commit wrote outbox rows" from Tx.Commit to the workers. Both channels
// hold one pending signal: a burst of commits collapses into one wake-up, and a commit never
// blocks on a busy or absent receiver.
type outboxSignals struct {
	local  chan struct{} // this process's outbox worker (OutboxWakeups)
	remote chan struct{} // RunOutboxNotifier, which NOTIFYs every instance's LISTEN loop
}

func newOutboxSignals() outboxSignals {
	return outboxSignals{local: make(chan struct{}, 1), remote: make(chan struct{}, 1)}
}

func (s *Store) signalOutbox() {
	for _, ch := range []chan struct{}{s.signals.local, s.signals.remote} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// OutboxWakeups fires after a commit of this process wrote outbox rows (coalesced). The local
// worker reacts to it without a round trip through Postgres.
func (s *Store) OutboxWakeups() <-chan struct{} { return s.signals.local }

// RunOutboxNotifier sends NOTIFY on OutboxChannel, in its own autocommit statement, after commits
// of this process wrote outbox rows, so the other instances' workers wake too (ListenOutbox). A
// burst of commits collapses into one NOTIFY per round trip. Failures go to onError and are
// otherwise ignored: every worker's poll still picks the rows up. It returns when ctx ends.
func (s *Store) RunOutboxNotifier(ctx context.Context, onError func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.signals.remote:
		}
		nctx, cancel := context.WithTimeout(ctx, notifyTimeout)
		_, err := s.pool.Exec(nctx, `SELECT pg_notify($1, '')`, OutboxChannel)
		cancel()
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
}

// FlushOutboxNotify sends the NOTIFY a commit asked for but RunOutboxNotifier, already stopped,
// did not send: at shutdown, bookings committed while HTTP requests drained would otherwise wait
// for the other instances' poll. Best effort.
func (s *Store) FlushOutboxNotify(ctx context.Context) error {
	select {
	case <-s.signals.remote:
	default:
		return nil
	}
	nctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	_, err := s.pool.Exec(nctx, `SELECT pg_notify($1, '')`, OutboxChannel)
	return err
}
