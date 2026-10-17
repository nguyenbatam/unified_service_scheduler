package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Refresh sessions (auth_sessions): one row per refresh token, grouped in families
// (one per login). Only the token's SHA-256 is stored.
//
// Clock: every session time — issue, idle and absolute expiry, rotation, revocation, the grace
// and expiry verdicts, the purge — is the database's (clock_timestamp(), now() where an index
// needs a stable value), never an API instance's, so replicas whose clocks drift apart still agree
// on every verdict. The times a caller needs (cookie Max-Age) come back with the result
// (Session.IssuedAt, RefreshResult.Now). Only the JWT's own exp is on the instance clock, as every
// instance verifies it with its own clock anyway.
//
// Locking: every path that changes a user's existing sessions or token_version takes that user's
// row lock first — RevokeTokens through its UPDATE, RefreshSession and StartSession's replacement
// with FOR NO KEY UPDATE — then the session rows, each in a statement started after the lock (READ
// COMMITTED: a fresh snapshot). So a refresh committing its successor and a logout (or a login
// replacing that family) are serialised: the revoke starts after the refresh committed and sees
// the successor, and a refresh queued behind a revoke reads the revoked row. Inserting a new
// family's first row takes only the foreign key's KEY SHARE on its user, which conflicts with none
// of these locks: a login racing a logout may survive it, the order "logout, then login" anyway.

// MaxUserAgentLength bounds auth_sessions.user_agent (characters).
const MaxUserAgentLength = 200

// Session is one refresh token's row.
type Session struct {
	ID       uuid.UUID
	FamilyID uuid.UUID
	UserID   uuid.UUID
	// ExpiresAt is the token's idle expiry (never after FamilyExpiresAt).
	ExpiresAt time.Time
	// FamilyExpiresAt is the login session's absolute end.
	FamilyExpiresAt time.Time
	// IssuedAt is created_at (database clock); for StartSession the moment the family started.
	IssuedAt time.Time
	// Replaced is the family StartSession revoked for NewSession.Replace (zero if none).
	Replaced uuid.UUID
}

// NewSession starts a refresh family (login, register).
type NewSession struct {
	UserID    uuid.UUID
	TokenHash []byte
	UserAgent string
	IdleTTL   time.Duration
	MaxTTL    time.Duration
	// Replace is the digest of the refresh cookie the request carried, if any: that family is
	// revoked in the same TX (a new login replaces the browser's previous session).
	Replace []byte
}

// StartSession revokes the family of n.Replace (when set) and inserts the first token of a new
// family, in one TX; its times are the database clock's (returned in the Session).
func (s *Store) StartSession(ctx context.Context, n NewSession) (Session, error) {
	session := Session{ID: uuid.New(), FamilyID: uuid.New(), UserID: n.UserID}
	tx, err := s.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &pgx.Batch{}
	if len(n.Replace) > 0 {
		// The replaced family's user lock first (it may be another account: same browser), so a
		// refresh of that family committing a successor right now is serialised before the revoke.
		b.Queue(`
			SELECT 1 FROM users WHERE id = (SELECT user_id FROM auth_sessions WHERE token_hash = $1)
			FOR NO KEY UPDATE`, n.Replace)
		b.Queue(`
			SELECT family_id FROM auth_sessions WHERE token_hash = $1`, n.Replace).QueryRow(func(r pgx.Row) error {
			if err := r.Scan(&session.Replaced); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			return nil
		})
		b.Queue(`
			UPDATE auth_sessions SET revoked_at = clock_timestamp()
			WHERE family_id = (SELECT family_id FROM auth_sessions WHERE token_hash = $1)
			  AND revoked_at IS NULL`, n.Replace)
	}
	b.Queue(`
		WITH t AS (SELECT clock_timestamp() AS now)
		INSERT INTO auth_sessions (id, family_id, user_id, token_hash, created_at, expires_at, family_expires_at, user_agent)
		SELECT $1, $2, $3, $4, t.now,
		       least(t.now + make_interval(secs => $5::float8), t.now + make_interval(secs => $6::float8)),
		       t.now + make_interval(secs => $6::float8), $7
		FROM t
		RETURNING created_at, expires_at, family_expires_at`,
		session.ID, session.FamilyID, session.UserID, n.TokenHash, n.IdleTTL.Seconds(), n.MaxTTL.Seconds(),
		userAgent(n.UserAgent)).QueryRow(func(r pgx.Row) error {
		return r.Scan(&session.IssuedAt, &session.ExpiresAt, &session.FamilyExpiresAt)
	})
	if err := tx.tx.SendBatch(ctx, b).Close(); err != nil {
		return Session{}, translate(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	return session, nil
}

// RefreshOutcome is what RefreshSession did with a presented refresh token.
type RefreshOutcome int

const (
	// RefreshUnknown: no such token (never issued, or purged).
	RefreshUnknown RefreshOutcome = iota
	// RefreshRotated: the token was marked rotated and a successor issued (RefreshResult.Session).
	RefreshRotated
	// RefreshGrace: the token was rotated less than the grace ago (a racing tab, a lost response);
	// nothing changed. RefreshResult.Session is its successor (req.NewHash) when that is still the
	// family's live token (RefreshResult.Resend: send its cookie again), else the presented row;
	// either way a fresh access token of the family is answered.
	RefreshGrace
	// RefreshExpired: the token's idle expiry or its family's absolute expiry has passed (checked
	// before rotation and revocation: an expired token is never reuse).
	RefreshExpired
	// RefreshReused: a token rotated longer than the grace ago in a live family. Every family of
	// the user was revoked and token_version bumped (SESSION_REVOKED emitted).
	RefreshReused
	// RefreshInactive: the user is deactivated; every family of the user was revoked.
	RefreshInactive
	// RefreshRevoked: the token's family was revoked (logout, a new login on the browser, a
	// detected reuse). Nothing of it is live, so nothing else changes: revoking the user's newer
	// sessions for it would let a stale cookie (another tab after a logout) or anyone replaying
	// one sign the user out again and again.
	RefreshRevoked
)

// RefreshRequest presents TokenHash; on rotation the successor gets NewHash, which must be a
// function of the presented token (auth.SuccessorRefreshToken), so the grace can resend it.
type RefreshRequest struct {
	TokenHash []byte
	NewHash   []byte
	UserAgent string
	IdleTTL   time.Duration
	Grace     time.Duration
}

// RefreshResult reports a refresh. User is the session's user as read under its row lock (set for
// RefreshRotated and RefreshGrace; also for RefreshReused and RefreshInactive, for logging).
type RefreshResult struct {
	Outcome RefreshOutcome
	Session Session
	User    domain.User
	// Resend: RefreshGrace found the successor still live; the caller sets its cookie again.
	Resend bool
	// Now is the database time every verdict of this refresh was made at (cookie Max-Age).
	Now time.Time
}

// RefreshSession rotates a refresh token in one TX: the user's row is locked (FOR NO KEY UPDATE,
// serialising with logout and with the user's other refreshes), then the token's row (FOR UPDATE),
// so two refreshes of one token run one after the other and the second, reading the committed
// rotation, gets RefreshGrace (within req.Grace) or RefreshReused (later) — never a second
// rotation; a revoked family is RefreshRevoked whatever its state. Reuse detection revokes every
// family of the user and bumps token_version with SESSION_REVOKED in the same TX, then commits: the
// caller answers 401. Every verdict compares the rows with one database time, read after the locks.
func (s *Store) RefreshSession(ctx context.Context, req RefreshRequest) (RefreshResult, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return RefreshResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		user    domain.User
		active  bool
		found   bool
		row     sessionRow
		haveRow bool
		now     time.Time
	)
	b := &pgx.Batch{}
	// Two statements on purpose, pipelined: the second runs after the first's lock wait and takes
	// its own snapshot, so it reads the token row as the previous holder of the user lock left it.
	b.Queue(userSelectFor+` WHERE id = (SELECT user_id FROM auth_sessions WHERE token_hash = $1) FOR NO KEY UPDATE`,
		req.TokenHash).QueryRow(func(r pgx.Row) error {
		var err error
		user, active, err = scanUserActive(r)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	b.Queue(`
		SELECT id, family_id, user_id, expires_at, family_expires_at, rotated_at, revoked_at
		FROM auth_sessions WHERE token_hash = $1 FOR UPDATE`, req.TokenHash).QueryRow(func(r pgx.Row) error {
		err := r.Scan(&row.ID, &row.FamilyID, &row.UserID, &row.ExpiresAt, &row.FamilyExpiresAt, &row.rotatedAt, &row.revokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		haveRow = err == nil
		return err
	})
	// The verdicts' time: read after both locks (a statement of its own, so after any lock wait),
	// from the database clock every instance shares.
	b.Queue(`SELECT clock_timestamp()`).QueryRow(func(r pgx.Row) error { return r.Scan(&now) })
	if err := tx.tx.SendBatch(ctx, b).Close(); err != nil {
		return RefreshResult{}, err
	}
	if !found || !haveRow || row.UserID != user.ID {
		return RefreshResult{Outcome: RefreshUnknown, Now: now}, nil
	}
	result := RefreshResult{Session: row.Session, User: user, Now: now}
	switch {
	case !active:
		result.Outcome = RefreshInactive
		if err := tx.revokeSessions(ctx, user.ID); err != nil {
			return RefreshResult{}, err
		}
	case !now.Before(row.FamilyExpiresAt), !now.Before(row.ExpiresAt):
		// Expiry first, whatever the row's state: the family has ended, or this token's idle time
		// ran out — then it can no longer be used, so presenting it (even rotated: an old cookie
		// from a long-closed tab) proves nothing about theft. A plain 401, nothing revoked.
		return RefreshResult{Outcome: RefreshExpired, Now: now}, nil
	case row.revokedAt != nil:
		return RefreshResult{Outcome: RefreshRevoked, Session: row.Session, User: user, Now: now}, nil
	case row.rotatedAt != nil:
		if now.Sub(*row.rotatedAt) < req.Grace {
			return tx.grace(ctx, row.Session, user, req, now)
		}
		result.Outcome = RefreshReused
	default:
		next, err := tx.rotate(ctx, row.Session, req, now)
		if err != nil {
			return RefreshResult{}, err
		}
		result.Outcome, result.Session = RefreshRotated, next
	}
	if result.Outcome == RefreshReused {
		if _, err := tx.revokeUser(ctx, user.ID); err != nil {
			return RefreshResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshResult{}, err
	}
	return result, nil
}

// grace answers a token rotated within the grace: its successor (req.NewHash) when that is still
// the family's live token — not rotated or revoked itself, not expired — so the caller resends
// that cookie; otherwise only an access token of the family (the client holds a newer token).
func (t *Tx) grace(ctx context.Context, presented Session, user domain.User, req RefreshRequest, now time.Time) (RefreshResult, error) {
	result := RefreshResult{Outcome: RefreshGrace, Session: presented, User: user, Now: now}
	var next Session
	err := t.tx.QueryRow(ctx, `
		SELECT id, family_id, user_id, expires_at, family_expires_at, created_at FROM auth_sessions
		WHERE token_hash = $1 AND family_id = $2 AND rotated_at IS NULL AND revoked_at IS NULL AND expires_at > $3`,
		req.NewHash, presented.FamilyID, now).Scan(&next.ID, &next.FamilyID, &next.UserID, &next.ExpiresAt, &next.FamilyExpiresAt, &next.IssuedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return result, nil
	case err != nil:
		return RefreshResult{}, err
	}
	result.Session, result.Resend = next, true
	return result, nil
}

// rotate marks prev rotated at now (the refresh's database time) and inserts its successor in the
// same family, expiring an idle TTL from now but never after the family.
func (t *Tx) rotate(ctx context.Context, prev Session, req RefreshRequest, now time.Time) (Session, error) {
	next := Session{ID: uuid.New(), FamilyID: prev.FamilyID, UserID: prev.UserID, FamilyExpiresAt: prev.FamilyExpiresAt}
	b := &pgx.Batch{}
	b.Queue(`UPDATE auth_sessions SET rotated_at = $2 WHERE id = $1`, prev.ID, now)
	b.Queue(`
		INSERT INTO auth_sessions (id, family_id, user_id, token_hash, created_at, expires_at, family_expires_at, user_agent)
		VALUES ($1, $2, $3, $4, $5::timestamptz, least($5::timestamptz + make_interval(secs => $6::float8), $7::timestamptz), $7::timestamptz, $8)
		RETURNING created_at, expires_at`,
		next.ID, next.FamilyID, next.UserID, req.NewHash, now, req.IdleTTL.Seconds(), next.FamilyExpiresAt,
		userAgent(req.UserAgent)).QueryRow(func(r pgx.Row) error {
		return r.Scan(&next.IssuedAt, &next.ExpiresAt)
	})
	if err := t.tx.SendBatch(ctx, b).Close(); err != nil {
		return Session{}, translate(err)
	}
	return next, nil
}

// SessionUser returns the user of a live refresh token (not revoked, neither its idle nor its
// family expiry passed on the database clock): POST /auth/logout with the cookie only. A rotated
// token still counts: logging out revokes every family anyway, as reuse detection would.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT user_id FROM auth_sessions
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > clock_timestamp() AND family_expires_at > clock_timestamp()`,
		tokenHash).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return id, err == nil, err
}

// liveFamilySQL is true while refresh family $f can still be used: not revoked and not past its
// absolute end. Every row of a family shares family_expires_at and is revoked together, so one
// row tells (auth_sessions_family_idx).
const liveFamilySQL = `EXISTS (
	SELECT 1 FROM auth_sessions f
	WHERE f.family_id = %s AND f.revoked_at IS NULL AND f.family_expires_at > clock_timestamp())`

// FindUserSession loads an active user (as FindUserByID) and whether refresh family sid is live,
// in one query: requireAuth and the WebSocket handshake for an access token minted for a session,
// so a token whose session was revoked (a new login on that browser, logout, reuse detection)
// stops working at once (within USER_CACHE_TTL through the cache).
func (s *Store) FindUserSession(ctx context.Context, id, sid uuid.UUID) (domain.User, bool, error) {
	var live bool
	user, err := scanUserWith(s.pool.QueryRow(ctx, `
		SELECT id, email, password_hash, name, role, customer_id, dealership_id, token_version, `+
		fmt.Sprintf(liveFamilySQL, "$2")+`
		FROM users WHERE id = $1 AND active`, id, sid), &live)
	return user, live, err
}

// FindUsersAndSessions is FindUsersByIDs plus the live refresh families among sids, in one round
// trip (the WebSocket hub's batched session sweep).
func (s *Store) FindUsersAndSessions(ctx context.Context, ids, sids []uuid.UUID) (map[uuid.UUID]domain.User, map[uuid.UUID]bool, error) {
	users := make(map[uuid.UUID]domain.User, len(ids))
	live := make(map[uuid.UUID]bool, len(sids))
	if len(ids) == 0 {
		return users, live, nil
	}
	b := &pgx.Batch{}
	b.Queue(userSelect+` WHERE id = ANY($1) AND active`, ids).Query(func(rows pgx.Rows) error {
		for rows.Next() {
			user, err := scanUser(rows)
			if err != nil {
				return err
			}
			users[user.ID] = user
		}
		return rows.Err()
	})
	if len(sids) > 0 {
		b.Queue(`
			SELECT DISTINCT family_id FROM auth_sessions
			WHERE family_id = ANY($1) AND revoked_at IS NULL AND family_expires_at > clock_timestamp()`, sids).Query(func(rows pgx.Rows) error {
			for rows.Next() {
				var sid uuid.UUID
				if err := rows.Scan(&sid); err != nil {
					return err
				}
				live[sid] = true
			}
			return rows.Err()
		})
	}
	if err := s.pool.SendBatch(ctx, b).Close(); err != nil {
		return nil, nil, err
	}
	return users, live, nil
}

// RevokeTokens bumps token_version so every token issued to the user so far stops working, revokes
// every refresh family of the user, and emits SESSION_REVOKED in the same TX so every instance
// closes the user's open sockets.
func (s *Store) RevokeTokens(ctx context.Context, id uuid.UUID) error {
	tx, err := s.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.revokeUser(ctx, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// revokeUser bumps the user's token_version (taking the user's row lock first, see the locking
// note above), revokes every live refresh family of the user, and records SESSION_REVOKED with the
// new version. It returns that version.
func (t *Tx) revokeUser(ctx context.Context, id uuid.UUID) (int, error) {
	var version int
	if err := t.tx.QueryRow(ctx, `
		UPDATE users SET token_version = token_version + 1 WHERE id = $1 RETURNING token_version`, id).Scan(&version); err != nil {
		return 0, missing(err, "user not found")
	}
	if err := t.revokeSessions(ctx, id); err != nil {
		return 0, err
	}
	payload := map[string]any{"id": id, "tokenVersion": version}
	if err := t.InsertOutbox(ctx, domain.AggregateUser, id, domain.EventSessionRevoked, payload); err != nil {
		return 0, err
	}
	return version, nil
}

// revokeSessions revokes every live refresh family of the user.
func (t *Tx) revokeSessions(ctx context.Context, id uuid.UUID) error {
	_, err := t.tx.Exec(ctx, `UPDATE auth_sessions SET revoked_at = clock_timestamp() WHERE user_id = $1 AND revoked_at IS NULL`, id)
	return err
}

// PurgeAuthSessions deletes refresh tokens whose family has ended, or whose idle expiry passed
// more than a day ago (kept that long so a stolen, rotated token still trips reuse detection), at
// most limit per call; rows another instance is purging are skipped, not waited on. now() (the
// statement's start, database clock) rather than clock_timestamp(): a stable value, so the
// expires_at index serves the scan.
func (s *Store) PurgeAuthSessions(ctx context.Context, limit int) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM auth_sessions
		WHERE id IN (
			SELECT id FROM auth_sessions
			WHERE expires_at < now()
			  AND (family_expires_at < now() OR expires_at < now() - interval '1 day')
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)`, limit)
	return tag.RowsAffected(), err
}

type sessionRow struct {
	Session
	rotatedAt *time.Time
	revokedAt *time.Time
}

// userSelectFor is userSelect plus the active flag (a refresh must see a deactivated user).
const userSelectFor = `SELECT id, email, password_hash, name, role, customer_id, dealership_id, token_version, active FROM users`

func scanUserActive(row pgx.Row) (domain.User, bool, error) {
	var item domain.User
	var customer, dealership *uuid.UUID
	var role int16
	var active bool
	if err := row.Scan(&item.ID, &item.Email, &item.PasswordHash, &item.Name, &role, &customer, &dealership,
		&item.TokenVersion, &active); err != nil {
		return domain.User{}, false, err
	}
	if customer != nil {
		item.CustomerID = *customer
	}
	if dealership != nil {
		item.DealershipID = *dealership
	}
	item.Role = domain.Role(role)
	if !item.Role.Valid() {
		return domain.User{}, false, domain.NewError(domain.CodeInternal, "unknown role code")
	}
	return item, active, nil
}

// userAgent is the stored form of a User-Agent header: valid UTF-8 without NUL (Postgres text),
// at most MaxUserAgentLength characters; empty is NULL.
func userAgent(raw string) any {
	ua := strings.ReplaceAll(strings.ToValidUTF8(raw, ""), "\x00", "")
	if utf8.RuneCountInString(ua) > MaxUserAgentLength {
		ua = string([]rune(ua)[:MaxUserAgentLength])
	}
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return nil
	}
	return ua
}
