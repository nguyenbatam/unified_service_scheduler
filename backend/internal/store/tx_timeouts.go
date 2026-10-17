package store

import (
	"context"
	"fmt"
	"time"
)

// SetLockTimeout overrides the pool's lock_timeout for the rest of this transaction (SET LOCAL,
// so it ends with the transaction and survives savepoint rollbacks only if set outside them).
// Sweeps that hold many row locks use a shorter one, so one wait does not hold the whole batch.
func (t *Tx) SetLockTimeout(ctx context.Context, d time.Duration) error {
	_, err := t.tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, fmt.Sprintf("%dms", max(d.Milliseconds(), 1)))
	return err
}
