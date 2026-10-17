package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"
)

// Refresh session defaults (SESSION_IDLE_TTL, SESSION_MAX_TTL).
const (
	// DefaultSessionIdleTTL is how long one refresh token stays usable after it was issued.
	DefaultSessionIdleTTL = 24 * time.Hour
	// DefaultSessionMaxTTL is the absolute end of a login session (refresh family): refreshing
	// never extends it.
	DefaultSessionMaxTTL = 7 * 24 * time.Hour
	// RefreshReuseGrace is how long a rotated refresh token is still answered (a fresh access
	// token for its family and its successor cookie again, no second rotation) — two tabs racing
	// the same cookie, or a refresh whose response (Set-Cookie) was lost on a slow or closed
	// request; RFC 9700 §4.14.2 allows such a short leeway. Presented any later it counts as reuse:
	// every session of the user is revoked.
	RefreshReuseGrace = 60 * time.Second
)

// refreshTokenBytes is the refresh token's entropy (256 bits, OWASP: at least 64).
const refreshTokenBytes = 32

// refreshTokenLength is the unpadded base64url length of a refresh token.
var refreshTokenLength = base64.RawURLEncoding.EncodedLen(refreshTokenBytes)

// NewRefreshToken returns a new opaque refresh token (32 random bytes, unpadded base64url) and the
// SHA-256 digest that is the only thing stored server-side.
func NewRefreshToken() (string, []byte, error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("refresh token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, HashRefreshToken(token), nil
}

// SuccessorRefreshToken is the token a rotation of presented issues, and its digest: HMAC-SHA256
// of presented under a key derived from the JWT secret, so every instance derives the same one.
// Deterministic on purpose: a request that presents a token rotated moments ago (two tabs racing,
// or a refresh whose response was lost) is sent that same successor again instead of nothing or a
// second rotation, so the browser always ends up holding the family's live token. Unpredictable
// without the secret, like a random token.
func (s *Service) SuccessorRefreshToken(presented string) (string, []byte) {
	mac := hmac.New(sha256.New, s.successorKey)
	mac.Write([]byte(presented))
	token := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return token, HashRefreshToken(token)
}

// HashRefreshToken is the stored digest of token. A plain (unkeyed) SHA-256 is enough: the token
// carries 256 random bits, so the digest cannot be inverted by guessing.
func HashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// WellFormedRefreshToken reports whether token has the shape NewRefreshToken produces, so
// garbage cookies are refused without a database round trip.
func WellFormedRefreshToken(token string) bool {
	if len(token) != refreshTokenLength {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil
}
