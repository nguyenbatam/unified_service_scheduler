package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestAccessTokenCarriesCustomerAndAudience(t *testing.T) {
	svc := auth.New("secret")
	user := domain.User{ID: uuid.New(), Email: "c@x", Name: "C", Role: domain.RoleCustomer, CustomerID: uuid.New()}
	raw, exp, err := svc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.Parse(raw, auth.AudienceAPI)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Actor() != (domain.Actor{UserID: user.ID, Role: user.Role, CustomerID: user.CustomerID}) {
		t.Fatalf("actor = %+v", claims.Actor())
	}
	if !claims.SessionExpiry().Equal(exp.Truncate(time.Second)) {
		t.Fatalf("session expiry %v want %v", claims.SessionExpiry(), exp)
	}
	if _, err := svc.Parse(raw, auth.AudienceWS); err == nil {
		t.Fatal("access token must not be accepted as a WS ticket")
	}
}

func TestTicketIsShortLivedAndBoundToSession(t *testing.T) {
	svc := auth.New("secret")
	actor := domain.Actor{UserID: uuid.New(), Role: domain.RoleDispatcher}
	sessionExp := time.Now().Add(time.Hour).Truncate(time.Second)
	raw, exp, err := svc.IssueTicket(actor, 0, sessionExp)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) > 2*time.Minute {
		t.Fatalf("ticket lives too long: %v", time.Until(exp))
	}
	claims, err := svc.Parse(raw, auth.AudienceWS)
	if err != nil {
		t.Fatal(err)
	}
	if !claims.SessionExpiry().Equal(sessionExp) {
		t.Fatalf("socket must close with the session: %v want %v", claims.SessionExpiry(), sessionExp)
	}
	if _, err := svc.Parse(raw, auth.AudienceAPI); err == nil {
		t.Fatal("ticket must not be accepted as an API token")
	}
}

func TestParseRejectsForeignSignature(t *testing.T) {
	raw, _, err := auth.New("one").Issue(domain.User{ID: uuid.New(), Role: domain.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.New("two").Parse(raw, auth.AudienceAPI); err == nil {
		t.Fatal("token signed with another secret must fail")
	}
}

func TestCheckPassword(t *testing.T) {
	hash, err := auth.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !auth.CheckPassword(hash, "correct horse") || auth.CheckPassword(hash, "wrong") {
		t.Fatal("password check mismatch")
	}
	if auth.CheckPassword("", "anything") {
		t.Fatal("empty hash (unknown user) must never match")
	}
}

func TestNewRefusesEmptySecret(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("auth.New(\"\") did not panic")
		}
	}()
	auth.New("")
}

// Pseudonym is stable for one secret (every instance agrees), differs across secrets and values,
// and never contains the value.
func TestPseudonym(t *testing.T) {
	a, b := auth.New("secret"), auth.New("other-secret")
	p := a.Pseudonym("someone@example.test")
	if p != auth.New("secret").Pseudonym("someone@example.test") {
		t.Fatal("pseudonym differs between services with the same secret")
	}
	if p == b.Pseudonym("someone@example.test") || p == a.Pseudonym("other@example.test") {
		t.Fatal("pseudonym collides across secrets or values")
	}
	if len(p) != 32 || strings.Contains(p, "someone") {
		t.Fatalf("pseudonym %q: want 32 hex characters without the value", p)
	}
}
