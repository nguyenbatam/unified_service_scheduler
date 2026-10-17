package httpapi

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// familyCache remembers, per refresh family (an access token's sid), whether it was live when
// last read, for the user cache's TTL: requireAuth then answers a cached user and family with no
// query, and loads both in one query otherwise (store.FindUserSession). A family revoked on
// another instance is noticed within the TTL; this instance's own revocations call invalidate.
// A revoked family never becomes live again, so only "live" can be stale. Bounded like the user
// cache: past size, expired entries go first, then everything (a miss costs the same one query).
type familyCache struct {
	ttl  time.Duration
	size int
	now  func() time.Time

	mu      sync.Mutex
	entries map[uuid.UUID]familyEntry
}

type familyEntry struct {
	live    bool
	expires time.Time
}

func newFamilyCache(ttl time.Duration, size int) *familyCache {
	if size <= 0 {
		size = defaultUserCacheSize
	}
	return &familyCache{ttl: ttl, size: size, now: time.Now, entries: make(map[uuid.UUID]familyEntry)}
}

// get reports a fresh cached verdict for sid.
func (f *familyCache) get(sid uuid.UUID) (live, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[sid]
	if !ok {
		return false, false
	}
	if !f.now().Before(e.expires) {
		delete(f.entries, sid)
		return false, false
	}
	return e.live, true
}

// put stores a verdict read at readAt (its age counts against the TTL from the read, not from the
// store, so a slow load never stretches the staleness bound).
func (f *familyCache) put(sid uuid.UUID, live bool, readAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.now()
	if _, exists := f.entries[sid]; !exists && len(f.entries) >= f.size {
		for id, e := range f.entries {
			if !now.Before(e.expires) {
				delete(f.entries, id)
			}
		}
		if len(f.entries) >= f.size {
			clear(f.entries)
		}
	}
	f.entries[sid] = familyEntry{live: live, expires: readAt.Add(f.ttl)}
}

// invalidate forgets sid (this instance revoked it).
func (f *familyCache) invalidate(sid uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, sid)
}
