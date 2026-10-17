package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

const customerSelect = `SELECT id, name, phone, email FROM customers`

// CreateCustomer inserts a walk-in customer (no user account).
func (s *Store) CreateCustomer(ctx context.Context, c domain.Customer) (domain.Customer, error) {
	_, err := s.pool.Exec(ctx, `INSERT INTO customers (id, name, phone, email) VALUES ($1, $2, $3, $4)`,
		c.ID, c.Name, c.Phone, c.Email)
	if err != nil {
		return domain.Customer{}, translate(err)
	}
	return c, nil
}

// customersPageSQL builds ListCustomers' query. The unfiltered page walks customers_name_idx
// (name, id) in order, seeking to the cursor, and stops at the LIMIT; a search is one table, so
// its OR is a BitmapOr over the pg_trgm GIN indexes (019, 024), then sorted.
func customersPageSQL(search string, limit int, after *NameCursor) (string, []any) {
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	var conds []string
	if search != "" {
		conds = append(conds, matchAnySQL(arg(containsPattern(search)), "name", "phone", "email"))
	}
	if after != nil {
		name, id := arg(after.Name), arg(after.ID)
		conds = append(conds, fmt.Sprintf(`name >= %s AND (name, id) > (%s, %s)`, name, name, id))
	}
	sql := customerSelect
	if len(conds) > 0 {
		sql += ` WHERE ` + strings.Join(conds, ` AND `)
	}
	return sql + ` ORDER BY name, id LIMIT ` + arg(pageLimit(limit)+1), args
}

// ListCustomers returns one page of customers in (name, id) order; search ("" = none) is matched as
// a literal, case-insensitive substring of the name, phone, or email.
func (s *Store) ListCustomers(ctx context.Context, search string, limit int, after *NameCursor) ([]domain.Customer, *NameCursor, error) {
	sql, args := customersPageSQL(search, limit, after)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	items, err := collect(rows, func(row pgx.Row) (domain.Customer, error) {
		var c domain.Customer
		err := row.Scan(&c.ID, &c.Name, &c.Phone, &c.Email)
		return c, err
	})
	if err != nil {
		return nil, nil, err
	}
	items, next := cutPage(items, pageLimit(limit), func(c domain.Customer) NameCursor { return NameCursor{Name: c.Name, ID: c.ID} })
	return items, next, nil
}
