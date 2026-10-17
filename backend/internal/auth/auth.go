package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

// Token audiences: an access token cannot open a socket via URL and a ticket cannot call the API.
const (
	AudienceAPI = "api"
	AudienceWS  = "ws"
)

const (
	// DefaultAccessTTL is the access token lifetime unless WithAccessTTL says otherwise
	// (ACCESS_TOKEN_TTL). Short: the refresh cookie (POST /auth/refresh) renews it.
	DefaultAccessTTL = 15 * time.Minute
	ticketTTL        = 60 * time.Second
	// MinPasswordLength follows NIST SP 800-63B for user-chosen secrets.
	MinPasswordLength = 8
)

type ctxKey struct{}

type Claims struct {
	UserID     uuid.UUID   `json:"uid"`
	Email      string      `json:"email"`
	Name       string      `json:"name"`
	Role       domain.Role `json:"role"`
	CustomerID uuid.UUID   `json:"cid,omitzero"`
	// TokenVersion must equal users.token_version; logout bumps it to revoke every session.
	TokenVersion int `json:"tv"`
	// SessionID is the refresh session family (auth_sessions.family_id) an access token was minted
	// for; zero on tokens minted without one (tools, tests).
	SessionID uuid.UUID `json:"sid,omitzero"`
	// SessionExp is the session family's absolute expiry (unix seconds) on an access token, copied
	// onto its WS tickets, so a socket lives until the login session ends, not the access token.
	SessionExp int64 `json:"sexp,omitempty"`
	jwt.RegisteredClaims
}

func (c Claims) Actor() domain.Actor {
	return domain.Actor{UserID: c.UserID, Role: c.Role, CustomerID: c.CustomerID}
}

type Service struct {
	secret []byte
	// pseudonymKey keys Pseudonym; derived from secret so it is never the JWT signing key itself.
	pseudonymKey []byte
	// successorKey keys SuccessorRefreshToken; derived from secret like pseudonymKey.
	successorKey []byte
	ttl          time.Duration
	// now stamps issued tokens (time.Now unless WithClock).
	now func() time.Time
}

// Option configures a Service.
type Option func(*Service)

// WithAccessTTL sets the access token lifetime (config bounds ACCESS_TOKEN_TTL to 1m–1h); a
// non-positive ttl keeps DefaultAccessTTL.
func WithAccessTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

// WithClock replaces the clock that stamps issued tokens (iat, exp). For tests: it simulates an
// instance whose clock is off; refresh sessions never read it (they run on the database clock).
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// New returns a token service signing with secret. An empty secret is a programming error (config
// refuses a missing or short JWT_SECRET), so New panics rather than fall back to a guessable key.
func New(secret string, opts ...Option) *Service {
	if secret == "" {
		panic("auth.New: empty JWT secret")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("unified-service-scheduler/pseudonym/v1"))
	successor := hmac.New(sha256.New, []byte(secret))
	successor.Write([]byte("unified-service-scheduler/refresh-successor/v1"))
	s := &Service{secret: []byte(secret), pseudonymKey: mac.Sum(nil), successorKey: successor.Sum(nil), ttl: DefaultAccessTTL, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// AccessTTL is the access token lifetime.
func (s *Service) AccessTTL() time.Duration { return s.ttl }

// Pseudonym is a stable, non-reversible stand-in for value (an email in a Redis rate-limit key):
// the first 128 bits of HMAC-SHA256 under a key derived from the JWT secret, hex. Keyed, because
// a plain hash of an email is reversed by hashing candidate addresses; every instance shares
// the secret, so they all agree on the pseudonym. Rotating the secret only restarts the counters.
func (s *Service) Pseudonym(value string) string {
	mac := hmac.New(sha256.New, s.pseudonymKey)
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// dummyHash makes failed logins for unknown emails cost one bcrypt compare too, so response time
// does not reveal which emails are registered.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("timing-equaliser"), bcrypt.DefaultCost)

func CheckPassword(hash, password string) bool {
	if hash == "" {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// Issue mints an API access token outside any refresh session (tools and tests); the API itself
// mints through IssueSession.
func (s *Service) Issue(user domain.User) (string, time.Time, error) {
	return s.IssueSession(user, uuid.Nil, time.Time{})
}

// IssueSession mints an API access token for refresh session family sessionID, which ends at
// sessionExp: the token carries both (sid, sexp) and never outlives the family. A zero sessionExp
// means no family (plain AccessTTL).
func (s *Service) IssueSession(user domain.User, sessionID uuid.UUID, sessionExp time.Time) (string, time.Time, error) {
	now := s.now().UTC()
	exp := now.Add(s.ttl)
	var sexp int64
	if !sessionExp.IsZero() {
		sessionExp = sessionExp.UTC().Truncate(time.Second)
		if sessionExp.Before(exp) {
			exp = sessionExp
		}
		sexp = sessionExp.Unix()
	}
	raw, err := s.sign(Claims{
		UserID:       user.ID,
		Email:        user.Email,
		Name:         user.Name,
		Role:         user.Role,
		CustomerID:   user.CustomerID,
		TokenVersion: user.TokenVersion,
		SessionID:    sessionID,
		SessionExp:   sexp,
		RegisteredClaims: jwt.RegisteredClaims{ //nolint:modernize // embedlit: named field requires type
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   user.ID.String(),
			Audience:  jwt.ClaimStrings{AudienceAPI},
		},
	})
	return raw, exp, err
}

// IssueTicket is IssueSessionTicket without a refresh session (tools, tests).
func (s *Service) IssueTicket(actor domain.Actor, tokenVersion int, sessionExp time.Time) (string, time.Time, error) {
	return s.IssueSessionTicket(actor, tokenVersion, uuid.Nil, sessionExp)
}

// IssueSessionTicket mints a short-lived WebSocket ticket for an already-authenticated actor.
// sessionID and sessionExp are the access token's refresh family (sid) and its absolute end
// (Claims.SessionExpiry): the socket closes then, and earlier once the family is revoked (the
// hub's sweep). tokenVersion is copied from the access token so a logout also invalidates unused
// tickets.
func (s *Service) IssueSessionTicket(actor domain.Actor, tokenVersion int, sessionID uuid.UUID, sessionExp time.Time) (string, time.Time, error) {
	now := s.now().UTC()
	exp := now.Add(ticketTTL)
	raw, err := s.sign(Claims{
		UserID:       actor.UserID,
		Role:         actor.Role,
		CustomerID:   actor.CustomerID,
		TokenVersion: tokenVersion,
		SessionID:    sessionID,
		SessionExp:   sessionExp.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{ //nolint:modernize // embedlit: named field requires type
			ExpiresAt: jwt.NewNumericDate(exp),
			IssuedAt:  jwt.NewNumericDate(now),
			Subject:   actor.UserID.String(),
			Audience:  jwt.ClaimStrings{AudienceWS},
			ID:        uuid.NewString(),
		},
	})
	return raw, exp, err
}

func (s *Service) sign(c Claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(s.secret)
}

// Parse validates signature, expiry, and that the token was minted for audience.
func (s *Service) Parse(raw, audience string) (Claims, error) {
	token, err := jwt.ParseWithClaims(raw, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return s.secret, nil
	}, jwt.WithAudience(audience), jwt.WithExpirationRequired())
	if err != nil {
		return Claims{}, err
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid || !slices.Contains(claims.Audience, audience) {
		return Claims{}, errors.New("invalid token")
	}
	return *claims, nil
}

// SessionExpiry is when the login session behind these claims ends: the refresh family's absolute
// expiry (sexp), else, for a token minted without a session, its own expiry.
func (c Claims) SessionExpiry() time.Time {
	if c.SessionExp > 0 {
		return time.Unix(c.SessionExp, 0)
	}
	if c.ExpiresAt != nil {
		return c.ExpiresAt.Time
	}
	return time.Time{}
}

type session struct {
	claims Claims
	actor  domain.Actor
}

// WithSession stores the verified claims and the DB-fresh actor for the request.
func WithSession(ctx context.Context, c Claims, actor domain.Actor) context.Context {
	return context.WithValue(ctx, ctxKey{}, session{claims: c, actor: actor})
}

func FromContext(ctx context.Context) (Claims, bool) {
	s, ok := ctx.Value(ctxKey{}).(session)
	return s.claims, ok
}

// ActorFrom returns the actor of an authenticated request.
func ActorFrom(ctx context.Context) (domain.Actor, bool) {
	s, ok := ctx.Value(ctxKey{}).(session)
	return s.actor, ok
}

func Bearer(header string) string {
	header = strings.TrimSpace(header)
	if len(header) < 8 || !strings.EqualFold(header[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}
