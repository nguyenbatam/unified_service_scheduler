package auth_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestRefreshTokenShape(t *testing.T) {
	a, hashA, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || len(a) != 43 || strings.ContainsAny(a, "+/=") {
		t.Fatalf("tokens %q %q: want distinct 43-char unpadded base64url", a, b)
	}
	if len(hashA) != 32 || !bytes.Equal(hashA, auth.HashRefreshToken(a)) {
		t.Fatalf("hash %x is not the SHA-256 of the token", hashA)
	}
	for token, want := range map[string]bool{a: true, b: true, "": false, a[:42]: false, a + "A": false, strings.Repeat("*", 43): false} {
		if got := auth.WellFormedRefreshToken(token); got != want {
			t.Fatalf("WellFormedRefreshToken(%q) = %v, want %v", token, got, want)
		}
	}
}

// The successor of a token is a function of the token and the secret: the same on every instance,
// different for another token or secret, and shaped like any refresh token.
func TestSuccessorRefreshToken(t *testing.T) {
	a, b := auth.New("secret-one"), auth.New("secret-one")
	token, _, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	next, hash := a.SuccessorRefreshToken(token)
	if again, _ := b.SuccessorRefreshToken(token); again != next {
		t.Fatal("two instances sharing the secret derive different successors")
	}
	if other, _ := auth.New("secret-two").SuccessorRefreshToken(token); other == next {
		t.Fatal("the successor does not depend on the secret")
	}
	if chained, _ := a.SuccessorRefreshToken(next); chained == next || chained == token {
		t.Fatal("successor chain repeats")
	}
	if !auth.WellFormedRefreshToken(next) || !bytes.Equal(hash, auth.HashRefreshToken(next)) || next == token {
		t.Fatalf("successor %q is not a well-formed token with its hash", next)
	}
}

// An access token minted for a session carries its family (sid) and absolute end (sexp), lives
// AccessTTL, and never outlives the family.
func TestIssueSessionClaims(t *testing.T) {
	svc := auth.New("secret", auth.WithAccessTTL(10*time.Minute))
	if svc.AccessTTL() != 10*time.Minute || auth.New("secret").AccessTTL() != auth.DefaultAccessTTL {
		t.Fatalf("access TTL %s / default %s", svc.AccessTTL(), auth.New("secret").AccessTTL())
	}
	user := domain.User{ID: uuid.New(), Role: domain.RoleCustomer, CustomerID: uuid.New(), TokenVersion: 3}
	sid := uuid.New()
	family := time.Now().Add(7 * 24 * time.Hour)
	raw, exp, err := svc.IssueSession(user, sid, family)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.Parse(raw, auth.AudienceAPI)
	if err != nil {
		t.Fatal(err)
	}
	if claims.SessionID != sid || claims.SessionExp != family.Unix() || claims.TokenVersion != 3 {
		t.Fatalf("claims sid %s sexp %d tv %d, want %s %d 3", claims.SessionID, claims.SessionExp, claims.TokenVersion, sid, family.Unix())
	}
	if d := time.Until(exp); d > 10*time.Minute || d < 9*time.Minute {
		t.Fatalf("access expiry in %s, want the 10m access TTL", d)
	}
	if !claims.SessionExpiry().Equal(family.Truncate(time.Second)) {
		t.Fatalf("SessionExpiry %s, want the family's end %s", claims.SessionExpiry(), family)
	}

	ending := time.Now().Add(2 * time.Minute)
	raw, exp, err = svc.IssueSession(user, sid, ending)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(ending.UTC().Truncate(time.Second)) {
		t.Fatalf("access expiry %s, want capped at the family's end %s", exp, ending)
	}
	if claims, err = svc.Parse(raw, auth.AudienceAPI); err != nil || !claims.ExpiresAt.Equal(exp) {
		t.Fatalf("parse capped token: %v, exp %v", err, claims.ExpiresAt)
	}

	// Minted without a session (tools): no sid/sexp, the session ends with the token.
	raw, exp, err = svc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	claims, err = svc.Parse(raw, auth.AudienceAPI)
	if err != nil || claims.SessionID != uuid.Nil || claims.SessionExp != 0 || !claims.SessionExpiry().Equal(exp.Truncate(time.Second)) {
		t.Fatalf("plain token claims %+v err %v", claims, err)
	}
	if strings.Contains(string(mustSegment(t, raw)), `"sid"`) {
		t.Fatal("a token without a session must not carry an empty sid")
	}
}

// mustSegment decodes the JWT payload.
func mustSegment(t *testing.T, raw string) []byte {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %s", raw)
	}
	out, err := base64URL(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func base64URL(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
