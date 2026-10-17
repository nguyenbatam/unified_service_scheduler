package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

func TestUserCacheTTLAndInvalidate(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	c := newUserCache(5*time.Second, 10)
	c.now = func() time.Time { return now }
	user := domain.User{ID: uuid.New(), Email: "a@b.test"}

	_, ticket, ok := c.get(user.ID)
	if ok {
		t.Fatal("hit on an empty cache")
	}
	c.put(user, ticket)
	if got, _, ok := c.get(user.ID); !ok || got.Email != user.Email {
		t.Fatalf("get after put = %v, %v", got, ok)
	}
	now = now.Add(5 * time.Second)
	if _, _, ok := c.get(user.ID); ok {
		t.Fatal("entry served at its TTL")
	}

	_, ticket, _ = c.get(user.ID)
	c.put(user, ticket)
	c.invalidate(user.ID)
	if _, _, ok := c.get(user.ID); ok {
		t.Fatal("entry served after invalidate")
	}
}

// A load that started before an invalidation of the same user may have read the old row: it
// must not be cached. Another user's invalidation (any logout) must not drop it.
func TestUserCacheDropsLoadsRacingAnInvalidation(t *testing.T) {
	c := newUserCache(time.Minute, 10)
	a, b := domain.User{ID: uuid.New()}, domain.User{ID: uuid.New()}
	_, ticketA, _ := c.get(a.ID)
	_, ticketB, _ := c.get(b.ID)
	c.invalidate(a.ID)
	c.put(a, ticketA)
	c.put(b, ticketB)
	if _, _, ok := c.get(a.ID); ok {
		t.Fatal("stale load cached after its user was invalidated")
	}
	if _, _, ok := c.get(b.ID); !ok {
		t.Fatal("invalidating A dropped B's in-flight load")
	}
	// A load that starts after the invalidation is cached again.
	_, ticketA, _ = c.get(a.ID)
	c.put(a, ticketA)
	if _, _, ok := c.get(a.ID); !ok {
		t.Fatal("load after the invalidation not cached")
	}
}

// Invalidations are forgotten after memory, and a ticket older than a forgotten one is refused.
func TestUserCacheForgetsOldInvalidationsSafely(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	c := newUserCache(5*time.Second, 10)
	c.now = func() time.Time { return now }
	slow, fresh := domain.User{ID: uuid.New()}, domain.User{ID: uuid.New()}
	_, slowTicket, _ := c.get(slow.ID)
	for range 100 {
		c.invalidate(uuid.New())
	}
	c.invalidate(slow.ID)
	now = now.Add(c.memory)
	c.invalidate(uuid.New()) // triggers the prune
	if n := len(c.invalidated); n != 1 {
		t.Fatalf("remembered invalidations = %d, want 1 (only the newest)", n)
	}
	c.put(slow, slowTicket)
	if _, _, ok := c.get(slow.ID); ok {
		t.Fatal("a load older than a forgotten invalidation was cached")
	}
	_, ticket, _ := c.get(fresh.ID)
	c.put(fresh, ticket)
	if _, _, ok := c.get(fresh.ID); !ok {
		t.Fatal("a load started after the prune was refused")
	}
}

func TestUserCacheIsBounded(t *testing.T) {
	c := newUserCache(time.Minute, 3)
	for range 10 {
		_, ticket, _ := c.get(uuid.Nil)
		c.put(domain.User{ID: uuid.New()}, ticket)
	}
	if n := c.len(); n > 3 {
		t.Fatalf("len = %d, want ≤ 3", n)
	}
}

// A full cache evicts a batch (expired first, then the oldest) so the next misses do not scan.
func TestUserCacheEvictsInBatches(t *testing.T) {
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	c := newUserCache(time.Minute, 100)
	c.now = func() time.Time { return now }
	ids := make([]uuid.UUID, 100)
	for i := range ids {
		ids[i] = uuid.New()
		_, ticket, _ := c.get(ids[i])
		c.put(domain.User{ID: ids[i]}, ticket)
		now = now.Add(time.Millisecond)
	}
	extra := uuid.New()
	_, ticket, _ := c.get(extra)
	c.put(domain.User{ID: extra}, ticket)
	if n := c.len(); n != 91 {
		t.Fatalf("len after a full put = %d, want 91 (90 kept + the new one)", n)
	}
	for _, id := range ids[:10] {
		if _, _, ok := c.get(id); ok {
			t.Fatal("one of the oldest entries survived the batch eviction")
		}
	}
	for _, id := range ids[10:] {
		if _, _, ok := c.get(id); !ok {
			t.Fatal("a newer entry was evicted")
		}
	}

	// Expired entries go first: once they free enough room, nothing fresh is evicted.
	now = now.Add(time.Minute - 50*time.Millisecond) // ids[10:51] are now expired
	for len(c.entries) < 100 {
		id := uuid.New()
		_, ticket, _ := c.get(id)
		c.put(domain.User{ID: id}, ticket)
	}
	_, ticket, _ = c.get(extra)
	c.put(domain.User{ID: uuid.New()}, ticket)
	for _, id := range ids[60:] {
		if _, _, ok := c.get(id); !ok {
			t.Fatal("a fresh entry was evicted while expired ones freed enough room")
		}
	}
}

func TestUserCacheConcurrentUse(t *testing.T) {
	c := newUserCache(time.Minute, 50)
	ids := make([]uuid.UUID, 20)
	for i := range ids {
		ids[i] = uuid.New()
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 500 {
				id := ids[(g+i)%len(ids)]
				if _, ticket, ok := c.get(id); !ok {
					c.put(domain.User{ID: id}, ticket)
				}
				if i%50 == 0 {
					c.invalidate(id)
				}
			}
		})
	}
	wg.Wait()
}

// countingUsers is a fake user loader that counts database reads.
type countingUsers struct {
	mu    sync.Mutex
	users map[uuid.UUID]domain.User
	reads atomic.Int32
}

func (f *countingUsers) load(_ context.Context, id uuid.UUID) (domain.User, error) {
	f.reads.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	user, ok := f.users[id]
	if !ok {
		return domain.User{}, domain.NewError(domain.CodeNotFound, "user not found")
	}
	return user, nil
}

func (f *countingUsers) set(u domain.User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[u.ID] = u
}

func newCachedServer(t *testing.T, ttl time.Duration, users *countingUsers) (*Server, *auth.Service) {
	t.Helper()
	authSvc := auth.New("usercache-test-secret-0123456789abcdef")
	s := New(nil, nil, authSvc, realtime.NewHub(nil), nil, "test", WithUserCache(ttl))
	s.loadUser = users.load
	return s, authSvc
}

func getMe(t *testing.T, h http.Handler, token string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestRequireAuthUsesUserCache(t *testing.T) {
	user := domain.User{ID: uuid.New(), Email: "c@b.test", Role: domain.RoleCustomer, CustomerID: uuid.New(), TokenVersion: 3}
	users := &countingUsers{users: map[uuid.UUID]domain.User{user.ID: user}}
	s, authSvc := newCachedServer(t, time.Minute, users)
	h := s.Handler()
	token, _, err := authSvc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		if code := getMe(t, h, token); code != http.StatusOK {
			t.Fatalf("GET /me = %d", code)
		}
	}
	// One read for three requests: requireAuth caches, and /me reuses the row it loaded.
	if n := users.reads.Load(); n != 1 {
		t.Fatalf("reads = %d, want 1", n)
	}

	// Signed out elsewhere (token_version bumped): the cached row still matches until invalidated.
	user.TokenVersion = 4
	users.set(user)
	if code := getMe(t, h, token); code != http.StatusOK {
		t.Fatalf("within the TTL: GET /me = %d, want 200 (cached)", code)
	}
	s.InvalidateUser(user.ID)
	if code := getMe(t, h, token); code != http.StatusUnauthorized {
		t.Fatalf("after InvalidateUser: GET /me = %d, want 401", code)
	}

	// A fresh token newer than a cached row re-reads instead of being rejected for the TTL.
	newToken, _, err := authSvc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	user.TokenVersion = 3
	s.users.put(user, s.users.seq) // pretend the cache still holds the old row
	user.TokenVersion = 4
	if code := getMe(t, h, newToken); code != http.StatusOK {
		t.Fatalf("newer token over an older cached row: GET /me = %d, want 200", code)
	}
}

func TestRequireAuthUserCacheExpires(t *testing.T) {
	user := domain.User{ID: uuid.New(), Email: "d@b.test", Role: domain.RoleDispatcher}
	users := &countingUsers{users: map[uuid.UUID]domain.User{user.ID: user}}
	s, authSvc := newCachedServer(t, time.Minute, users)
	now := time.Now()
	s.users.now = func() time.Time { return now }
	h := s.Handler()
	token, _, err := authSvc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	if code := getMe(t, h, token); code != http.StatusOK {
		t.Fatalf("GET /me = %d", code)
	}
	users.mu.Lock()
	delete(users.users, user.ID) // deactivated
	users.mu.Unlock()
	if code := getMe(t, h, token); code != http.StatusOK {
		t.Fatalf("within the TTL: GET /me = %d, want 200 (cached)", code)
	}
	now = now.Add(time.Minute)
	if code := getMe(t, h, token); code != http.StatusUnauthorized {
		t.Fatalf("after the TTL: GET /me = %d, want 401", code)
	}
}

func TestRequireAuthWithoutCacheReadsEveryTime(t *testing.T) {
	user := domain.User{ID: uuid.New(), Role: domain.RoleAdmin}
	users := &countingUsers{users: map[uuid.UUID]domain.User{user.ID: user}}
	s, authSvc := newCachedServer(t, 0, users)
	h := s.Handler()
	token, _, err := authSvc.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code := getMe(t, h, token); code != http.StatusOK {
			t.Fatalf("GET /me = %d", code)
		}
	}
	if n := users.reads.Load(); n != 2 {
		t.Fatalf("reads = %d, want 2 (cache disabled)", n)
	}
}
