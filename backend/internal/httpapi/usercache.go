package httpapi

import (
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// defaultUserCacheSize bounds the per-instance user cache; past it, expired entries are dropped
// first, then the oldest tenth (a miss only costs one indexed query).
const defaultUserCacheSize = 10_000

// minInvalidationMemory is the shortest time an invalidation is remembered per user. A load that
// started before an invalidation it can no longer see is not stored (see floor).
const minInvalidationMemory = time.Minute

// userCache keeps recently loaded users for a short TTL so requireAuth does not query Postgres on
// every request. Changes made elsewhere (another instance, SQL) are seen within ttl; this
// instance's own changes call invalidate. Safe for concurrent use.
//
// A miss hands out a load ticket (the current invalidation sequence number); put drops the row
// if that user was invalidated after the ticket was issued, since the load may have read the row
// before the change committed. Invalidations are tracked per user, so a logout elsewhere never
// discards another user's in-flight load. They are remembered for a bounded time (memory); when
// one is forgotten, floor rises to its sequence number and older tickets are refused outright,
// which only costs a load that has been running longer than memory a cache miss.
type userCache struct {
	ttl    time.Duration
	size   int
	memory time.Duration
	now    func() time.Time

	mu          sync.Mutex
	entries     map[uuid.UUID]userCacheEntry
	seq         uint64
	floor       uint64
	invalidated map[uuid.UUID]invalidation
	lastPrune   time.Time
}

type userCacheEntry struct {
	user    domain.User
	expires time.Time
}

type invalidation struct {
	seq uint64
	at  time.Time
}

func newUserCache(ttl time.Duration, size int) *userCache {
	if size <= 0 {
		size = defaultUserCacheSize
	}
	return &userCache{
		ttl: ttl, size: size, memory: max(ttl, minInvalidationMemory), now: time.Now,
		entries:     make(map[uuid.UUID]userCacheEntry),
		invalidated: make(map[uuid.UUID]invalidation),
	}
}

// get returns a fresh cached user, plus the load ticket to hand to put after a miss.
func (u *userCache) get(id uuid.UUID) (domain.User, uint64, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entry, ok := u.entries[id]
	if ok && u.now().Before(entry.expires) {
		return entry.user, u.seq, true
	}
	if ok {
		delete(u.entries, id)
	}
	return domain.User{}, u.seq, false
}

// put stores user unless that user was invalidated after ticket was issued by get.
func (u *userCache) put(user domain.User, ticket uint64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if ticket < u.floor {
		return
	}
	if inv, ok := u.invalidated[user.ID]; ok && inv.seq > ticket {
		return
	}
	now := u.now()
	if _, exists := u.entries[user.ID]; !exists && len(u.entries) >= u.size {
		u.evictLocked(now)
	}
	u.entries[user.ID] = userCacheEntry{user: user, expires: now.Add(u.ttl)}
}

// evictLocked makes room in one batch, so a full cache does not scan the map on every miss:
// expired entries go first, then the oldest until a tenth of the capacity is free.
func (u *userCache) evictLocked(now time.Time) {
	for id, entry := range u.entries {
		if !now.Before(entry.expires) {
			delete(u.entries, id)
		}
	}
	target := u.size - max(u.size/10, 1)
	excess := len(u.entries) - target
	if excess <= 0 {
		return
	}
	type aged struct {
		id      uuid.UUID
		expires time.Time
	}
	all := make([]aged, 0, len(u.entries))
	for id, entry := range u.entries {
		all = append(all, aged{id, entry.expires})
	}
	// Every entry gets the same ttl, so the earliest expiry is the oldest load.
	slices.SortFunc(all, func(a, b aged) int { return a.expires.Compare(b.expires) })
	for _, e := range all[:excess] {
		delete(u.entries, e.id)
	}
}

func (u *userCache) invalidate(id uuid.UUID) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := u.now()
	u.seq++
	u.invalidated[id] = invalidation{seq: u.seq, at: now}
	delete(u.entries, id)
	if now.Sub(u.lastPrune) >= u.memory {
		u.pruneLocked(now)
	}
}

// pruneLocked forgets invalidations older than memory (at most one scan per memory period) and
// raises floor past them, so a ticket that could have missed one is refused instead.
func (u *userCache) pruneLocked(now time.Time) {
	u.lastPrune = now
	for id, inv := range u.invalidated {
		if now.Sub(inv.at) >= u.memory {
			u.floor = max(u.floor, inv.seq)
			delete(u.invalidated, id)
		}
	}
}

func (u *userCache) len() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.entries)
}
