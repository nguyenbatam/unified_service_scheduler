package store

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// EnsureUser creates a fixed (seed) user unless it conflicts with an existing row on any unique
// key (id, email, or customer link): an existing account is left as is, so a restart never resets
// a changed password, role, scope or deactivation, and a seed user whose email someone registered
// first is skipped instead of failing startup. Customer users must carry CustomerID.
func (s *Store) EnsureUser(ctx context.Context, user domain.User) error {
	if !user.Role.Valid() {
		return domain.NewError(domain.CodeInvalid, "invalid role")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, name, role, customer_id, dealership_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING`,
		user.ID, strings.ToLower(user.Email), user.PasswordHash, user.Name, int16(user.Role), nullUUID(user.CustomerID),
		nullUUID(user.DealershipID))
	return translate(err)
}

// CreateCustomerUser registers a self-service customer: a customers row plus a linked user, atomically.
func (s *Store) CreateCustomerUser(ctx context.Context, user domain.User) (domain.User, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	email := strings.ToLower(user.Email)
	var taken bool
	if err := tx.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)`, email).Scan(&taken); err != nil {
		return domain.User{}, err
	}
	if taken {
		return domain.User{}, domain.NewError(domain.CodeConflict, "email already registered")
	}
	customerID := uuid.New()
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO customers (id, name, phone, email) VALUES ($1, $2, '', $3)`,
		customerID, user.Name, email); err != nil {
		return domain.User{}, err
	}
	if _, err := tx.tx.Exec(ctx, `
		INSERT INTO users (id, email, password_hash, name, role, customer_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		user.ID, email, user.PasswordHash, user.Name, int16(domain.RoleCustomer), customerID); err != nil {
		if IsUniqueViolation(err) {
			return domain.User{}, domain.NewError(domain.CodeConflict, "email already registered")
		}
		return domain.User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.User{}, err
	}
	user.Email = email
	user.Role = domain.RoleCustomer
	user.CustomerID = customerID
	return user, nil
}

const userSelect = `SELECT id, email, password_hash, name, role, customer_id, dealership_id, token_version FROM users`

func (s *Store) FindUserByEmail(ctx context.Context, email string) (domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, userSelect+` WHERE lower(email) = lower($1) AND active`, email))
}

func (s *Store) FindUserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return scanUser(s.pool.QueryRow(ctx, userSelect+` WHERE id = $1 AND active`, id))
}

// FindUsersByIDs loads the active users among ids in one query, keyed by id; inactive and unknown
// ids are simply absent (the WS hub's batched session sweep).
func (s *Store) FindUsersByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]domain.User, error) {
	users := make(map[uuid.UUID]domain.User, len(ids))
	if len(ids) == 0 {
		return users, nil
	}
	rows, err := s.pool.Query(ctx, userSelect+` WHERE id = ANY($1) AND active`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users[user.ID] = user
	}
	return users, rows.Err()
}

func scanUser(row pgx.Row) (domain.User, error) {
	return scanUserWith(row)
}

// scanUserWith scans userSelect's columns followed by extra destinations.
func scanUserWith(row pgx.Row, extra ...any) (domain.User, error) {
	var item domain.User
	var customer, dealership *uuid.UUID
	var role int16
	dst := append([]any{&item.ID, &item.Email, &item.PasswordHash, &item.Name, &role, &customer, &dealership, &item.TokenVersion}, extra...)
	err := row.Scan(dst...)
	if err != nil {
		return domain.User{}, missing(err, "user not found")
	}
	if customer != nil {
		item.CustomerID = *customer
	}
	if dealership != nil {
		item.DealershipID = *dealership
	}
	item.Role = domain.Role(role)
	if !item.Role.Valid() {
		return domain.User{}, domain.NewError(domain.CodeInternal, "unknown role code")
	}
	return item, nil
}
