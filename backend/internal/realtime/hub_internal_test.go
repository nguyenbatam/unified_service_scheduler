package realtime

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// register is the authority on the per-user cap: two handshakes of one user can both pass the
// pre-upgrade check in ServeWS, and the second must still be refused here (closed with 1013).
func TestRegisterEnforcesCapAfterPrecheck(t *testing.T) {
	h := NewHub(nil)
	h.SetMaxConnsPerUser(1)
	user := uuid.New()
	first := &client{send: make(chan []byte, 1), session: Session{UserID: user}}
	second := &client{send: make(chan []byte, 1), session: Session{UserID: user}}
	if h.full(user) {
		t.Fatal("no socket yet")
	}
	if got := h.register(first); got != registered {
		t.Fatalf("first register = %d", got)
	}
	if got := h.register(second); got != registerFull {
		t.Fatalf("second register = %d, want registerFull", got)
	}
	if !h.full(user) {
		t.Fatal("pre-upgrade check must see the user at the cap")
	}
	h.unregister(first)
	h.writers.Done() // no writePump was started for first
	if h.full(user) {
		t.Fatal("unregister must free the slot")
	}
}

// fakeClock is a settable hub clock.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time { return f.t }

func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestClient(user uuid.UUID, version int, accepted time.Time) *client {
	return &client{send: make(chan []byte, 1), session: Session{UserID: user, TokenVersion: version}, accepted: accepted}
}

// The handshake reads the user (token version 3), then the logout's SESSION_REVOKED (version 4)
// reaches this hub and closeUser runs before the handshake registers. The socket must be refused
// at register, not left open until the next sweep; a session opened with the new token is not.
func TestRegisterRefusesHandshakeRevokedMidway(t *testing.T) {
	h := NewHub(nil)
	clock := &fakeClock{t: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	h.now = clock.now
	user := uuid.New()
	stale := newTestClient(user, 3, clock.now()) // user read before the logout committed

	clock.advance(time.Millisecond)
	h.closeUser(user, 4, "signed out") // nothing registered yet: closes nothing, remembers the floor

	if got := h.register(stale); got != registerRevoked {
		t.Fatalf("stale handshake register = %d, want registerRevoked", got)
	}
	if h.ClientCount() != 0 {
		t.Fatal("a refused socket must not be counted")
	}
	fresh := newTestClient(user, 4, clock.now())
	if got := h.register(fresh); got != registered {
		t.Fatalf("new session register = %d, want registered", got)
	}
	// A replayed older revoke does not lower the floor, nor close the new session.
	h.closeUser(user, 3, "signed out")
	if got := h.register(newTestClient(user, 3, clock.now())); got != registerRevoked {
		t.Fatalf("floor lowered by a replayed revoke: register = %d", got)
	}
	if h.ClientCount() != 1 {
		t.Fatalf("clients = %d, want the new session only", h.ClientCount())
	}
	h.unregister(fresh)
	h.writers.Done()
}

// A revoke without a version (0 = all) cannot tell old sessions from new ones by version, so it
// refuses handshakes accepted before it and lets later ones through.
func TestRegisterRefusesHandshakeAcceptedBeforeVersionlessRevoke(t *testing.T) {
	h := NewHub(nil)
	clock := &fakeClock{t: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	h.now = clock.now
	user := uuid.New()
	before := newTestClient(user, 7, clock.now())
	clock.advance(time.Millisecond)
	h.closeUser(user, 0, "signed out")
	clock.advance(time.Millisecond)
	if got := h.register(before); got != registerRevoked {
		t.Fatalf("handshake accepted before the revoke: register = %d, want registerRevoked", got)
	}
	after := newTestClient(user, 7, clock.now())
	if got := h.register(after); got != registered {
		t.Fatalf("handshake accepted after the revoke: register = %d, want registered", got)
	}
	h.unregister(after)
	h.writers.Done()
}

// Revocations are forgotten after revokeMemory and the map never grows past maxRevocations.
func TestRevocationMemoryIsBounded(t *testing.T) {
	h := NewHub(nil)
	clock := &fakeClock{t: time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)}
	h.now = clock.now
	user := uuid.New()
	h.closeUser(user, 5, "signed out")
	clock.advance(revokeMemory)
	if got := h.register(newTestClient(user, 4, clock.now())); got != registered {
		t.Fatalf("expired revocation still refuses: register = %d", got)
	}
	for range maxRevocations + 5 {
		h.closeUser(uuid.New(), 1, "signed out")
	}
	if n := len(h.revoked); n > maxRevocations {
		t.Fatalf("revocations = %d, want at most %d", n, maxRevocations)
	}
	clock.advance(revokeMemory)
	h.closeUser(uuid.New(), 1, "signed out") // prunes every expired entry
	if n := len(h.revoked); n != 1 {
		t.Fatalf("revocations after expiry = %d, want 1", n)
	}
}

// closeUser touches only the revoked user's sockets (per-user index), and the index stays in step
// with the client set as sockets come and go.
func TestPerUserIndexTracksClients(t *testing.T) {
	h := NewHub(nil)
	a, b := uuid.New(), uuid.New()
	a1, a2, b1 := newTestClient(a, 1, time.Now()), newTestClient(a, 2, time.Now()), newTestClient(b, 1, time.Now())
	for _, c := range []*client{a1, a2, b1} {
		if got := h.register(c); got != registered {
			t.Fatalf("register = %d", got)
		}
	}
	h.closeUser(a, 2, "signed out") // closes a1 (version 1) only
	if _, open := h.clients[a1]; open {
		t.Fatal("a1 must be closed")
	}
	if len(h.perUser[a]) != 1 || len(h.perUser[b]) != 1 || h.ClientCount() != 2 {
		t.Fatalf("index = %d/%d, clients = %d", len(h.perUser[a]), len(h.perUser[b]), h.ClientCount())
	}
	h.unregister(a2)
	h.unregister(b1)
	if len(h.perUser) != 0 {
		t.Fatalf("index keeps %d users with no socket", len(h.perUser))
	}
	for range 3 {
		h.writers.Done()
	}
}
