package integration_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Schema: customer and user names must contain a non-space character (the notifier greets
// customers by their first word).
func TestBlankNamesAreRefused(t *testing.T) {
	t.Parallel()
	pool, _, _, ctx := openTestDB(t)
	for _, name := range []string{"", "   ", "\t\n"} {
		_, err := pool.Exec(ctx, `INSERT INTO customers (id, name, phone, email) VALUES ($1, $2, '', '')`, uuid.New(), name)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.ConstraintName != "customers_name_not_blank" {
			t.Errorf("customer name %q: err = %v, want customers_name_not_blank", name, err)
		}
		_, err = pool.Exec(ctx, `UPDATE users SET name = $1 WHERE id = (SELECT id FROM users LIMIT 1)`, name)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.ConstraintName != "users_name_not_blank" {
			t.Errorf("user name %q: err = %v, want users_name_not_blank", name, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO customers (id, name, phone, email) VALUES ($1, ' Amelia ', '', '')`, uuid.New()); err != nil {
		t.Fatalf("a padded real name must pass: %v", err)
	}
}
