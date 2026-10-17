package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsIdempotencyViolation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"idempotency index", &pgconn.PgError{Code: "23505", ConstraintName: ConstraintIdempotencyScope}, true},
		{"wrapped", fmt.Errorf("insert: %w", &pgconn.PgError{Code: "23505", ConstraintName: ConstraintIdempotencyScope}), true},
		{"other unique index", &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"}, false},
		{"exclusion", &pgconn.PgError{Code: "23P01", ConstraintName: ConstraintIdempotencyScope}, false},
		{"plain", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsIdempotencyViolation(tc.err); got != tc.want {
				t.Fatalf("IsIdempotencyViolation(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
