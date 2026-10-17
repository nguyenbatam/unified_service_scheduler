package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Public catalog reads (GET /dealerships, /service-types) are the same for every caller and
// change only through admin edits, yet every screen loads them. Each instance keeps the encoded
// body for a short TTL (≤ MaxCatalogCacheTTL) and tags it with a content-hash ETag, so a client
// revalidating with If-None-Match gets a body-less 304. Multi-instance: an edit made through this
// instance drops the entries at once; an edit made on another instance shows here within the TTL.
// That bounded staleness is accepted for these two reads only (every booking path re-reads
// Postgres, and admin writes still carry If-Match, so a stale version is a 412, never a lost
// update). A request sent with Cache-Control: no-cache skips a cached body older than
// catalogRefreshGap and refreshes it, so a client reloading right after an edit or a CONFIG_UPDATED
// event reads Postgres even on an instance that has not consumed the event yet. Loads are coalesced
// per key (one query in flight per instance; concurrent requests wait for it, so a no-cache request
// may get the result of a query that started up to one query's duration before it arrived), and
// the gap bounds a flood of no-cache requests to one load per gap per instance. Cached dealership bodies carry
// `today` as of the fetch, so it can lag midnight by up to the TTL. Both reads are paged (keyset,
// cursor.go): each page (listing, limit, cursor) is its own entry with its own ETag, and keeps its
// next page's cursor with the body.
//
// Cursors and limits are client input, so only canonical pages are cached: a limit the clients
// use (the default, or the maximum the web UI pages with) and either the first page or a page
// whose cursor this cache itself handed out (the next cursor of a cached page of the same listing
// and limit). Any other page is loaded and tagged the same way but neither cached nor coalesced,
// so arbitrary cursors cannot evict the pages everyone reads (the routes are throttled for
// customers like the other plain reads).

// catalogListing is one cached listing.
type catalogListing string

const (
	catalogDealerships  catalogListing = "dealerships"
	catalogServiceTypes catalogListing = "service-types"
)

// catalogKey names one page: the listing, its limit, and its cursor ("" = first page).
type catalogKey struct {
	listing catalogListing
	limit   int
	cursor  string
}

// page is the key of one page of the listing l.
func (l catalogListing) page(limit int, cursor string) catalogKey {
	return catalogKey{listing: l, limit: limit, cursor: cursor}
}

// catalogCachedLimit reports whether pages of this size are cached: the default (no ?limit=)
// and the maximum (the web UI's fetch-all page).
func catalogCachedLimit(limit int) bool {
	return limit == store.DefaultPageLimit || limit == store.MaxPageLimit
}

// catalogMaxEntries bounds the cached pages per instance. Only canonical pages are cached (above),
// so this is a safety net: when full, expired entries go first, then the oldest one.
const catalogMaxEntries = 64

// catalogRefreshGap is the age below which a no-cache request is still served the cached body: a
// body loaded that recently is as fresh as a new query would be for a reload, and it caps no-cache
// floods at one load per gap.
const catalogRefreshGap = 500 * time.Millisecond

// catalogMaxJoins bounds how often a request re-joins a load whose leader went away mid-query.
const catalogMaxJoins = 3

type catalogEntry struct {
	body []byte
	// next is the following page's cursor ("" on the last page), sent as X-Next-Cursor.
	next string
	etag string
	// fetchedAt is when the load started (its snapshot is at least that fresh).
	fetchedAt time.Time
}

// catalogCall is one load in flight; done closes when entry/err are set.
type catalogCall struct {
	done       chan struct{}
	generation uint64
	entry      *catalogEntry
	err        error
	// leaderGone: the load failed because the request running it went away (disconnect or its
	// deadline), not because of the database, so waiters retry rather than share the error.
	leaderGone bool
}

// catalogCache holds at most one entry, and one load in flight, per canonical catalogKey (at most
// catalogMaxEntries entries).
type catalogCache struct {
	ttl time.Duration
	now func() time.Time
	mu  sync.Mutex
	// generation is bumped by invalidate, so a fetch that started before an edit cannot store
	// the pre-edit body afterwards (nor be joined by a request that arrives after the edit).
	generation uint64
	entries    map[catalogKey]*catalogEntry
	inflight   map[catalogKey]*catalogCall
}

func newCatalogCache(ttl time.Duration) *catalogCache {
	return &catalogCache{ttl: ttl, now: time.Now, entries: map[catalogKey]*catalogEntry{}, inflight: map[catalogKey]*catalogCall{}}
}

// cacheable reports whether key is a canonical page (called with mu held): a cached limit, and
// the first page or the next page of a page in the cache. A later page whose predecessor has
// expired (or was evicted) is served uncached until a client walks the chain from the start again.
func (cc *catalogCache) cacheable(key catalogKey) bool {
	if !catalogCachedLimit(key.limit) {
		return false
	}
	if key.cursor == "" {
		return true
	}
	for k, e := range cc.entries {
		if k.listing == key.listing && k.limit == key.limit && e.next == key.cursor {
			return true
		}
	}
	return false
}

// store caches e under key, making room first when the cache is full (called with mu held):
// expired entries go first, then the oldest.
func (cc *catalogCache) store(key catalogKey, e *catalogEntry) {
	if _, ok := cc.entries[key]; !ok && len(cc.entries) >= catalogMaxEntries {
		now := cc.now()
		var oldest catalogKey
		var oldestAt time.Time
		for k, old := range cc.entries {
			if now.Sub(old.fetchedAt) >= cc.ttl {
				delete(cc.entries, k)
				continue
			}
			if oldestAt.IsZero() || old.fetchedAt.Before(oldestAt) {
				oldest, oldestAt = k, old.fetchedAt
			}
		}
		if len(cc.entries) >= catalogMaxEntries {
			delete(cc.entries, oldest)
		}
	}
	cc.entries[key] = e
}

// invalidate drops every entry. Safe on a nil cache.
func (cc *catalogCache) invalidate() {
	if cc == nil {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.generation++
	clear(cc.entries)
}

// InvalidateCatalog drops this instance's cached catalog bodies (wire it to CONFIG_UPDATED so
// edits made on other instances apply before the TTL ends).
func (s *Server) InvalidateCatalog() {
	s.catalog.invalidate()
}

// fetch returns a cached entry (younger than the TTL; with fresh, also younger than
// catalogRefreshGap) or the result of a load: it joins the load in flight for key when one of the
// current generation exists, otherwise it runs load itself and caches the result for the others.
//
// A page that is not canonical (cacheable) is loaded on its own, outside the cache.
func (cc *catalogCache) fetch(ctx context.Context, key catalogKey, fresh bool, load catalogLoad) (*catalogEntry, error) {
	for joins := 0; ; joins++ {
		cc.mu.Lock()
		now := cc.now()
		if !cc.cacheable(key) {
			cc.mu.Unlock()
			return loadEntry(ctx, now, load)
		}
		if e := cc.entries[key]; e != nil && cc.ttl > 0 {
			age := now.Sub(e.fetchedAt)
			if age < cc.ttl && (!fresh || age < min(catalogRefreshGap, cc.ttl)) {
				cc.mu.Unlock()
				return e, nil
			}
		}
		call := cc.inflight[key]
		// After catalogMaxJoins loads whose leader went away, run the load ourselves rather than
		// wait again or return another request's cancellation as our own error.
		if call == nil || call.generation != cc.generation || joins >= catalogMaxJoins {
			call = &catalogCall{done: make(chan struct{}), generation: cc.generation}
			cc.inflight[key] = call
			cc.mu.Unlock()
			cc.run(ctx, key, call, now, load)
			return call.entry, call.err
		}
		cc.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if call.err == nil || !call.leaderGone {
			return call.entry, call.err
		}
	}
}

// run performs call's load and publishes its result (cached when it succeeded and no invalidation
// happened meanwhile). Waiters are released even if load panics.
func (cc *catalogCache) run(ctx context.Context, key catalogKey, call *catalogCall, started time.Time, load catalogLoad) {
	defer func() {
		cc.mu.Lock()
		if cc.inflight[key] == call {
			delete(cc.inflight, key)
		}
		if call.err == nil && call.entry != nil && cc.ttl > 0 && call.generation == cc.generation {
			cc.store(key, call.entry)
		}
		cc.mu.Unlock()
		close(call.done)
	}()
	call.err = context.Canceled // until load returns: a panic must not leave a nil entry and nil err
	call.leaderGone = true
	entry, err := loadEntry(ctx, started, load)
	if err != nil {
		call.err, call.leaderGone = err, ctx.Err() != nil
		return
	}
	call.entry, call.err, call.leaderGone = entry, nil, false
}

// loadEntry runs load and tags its result. The tag covers the next cursor too: a page whose rows
// are unchanged but whose successor moved is a different response.
func loadEntry(ctx context.Context, started time.Time, load catalogLoad) (*catalogEntry, error) {
	body, next, err := load(ctx)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(body)
	h.Write([]byte{0})
	h.Write([]byte(next))
	sum := h.Sum(nil)
	return &catalogEntry{body: body, next: next, etag: `"c-` + hex.EncodeToString(sum[:12]) + `"`, fetchedAt: started}, nil
}

// catalogLoad reads one page: its encoded body and the next page's cursor ("" on the last page).
type catalogLoad func(context.Context) (body []byte, next string, err error)

// serveCatalog answers one page from the cache, or loads (coalesced per key), encodes, and caches
// the body with its next cursor. key is the page's key (catalogListing.page).
func (s *Server) serveCatalog(c *gin.Context, key catalogKey, load func(context.Context) (items any, next string, err error)) {
	e, err := s.catalog.fetch(c.Request.Context(), key, wantsFresh(c.GetHeader("Cache-Control")),
		func(ctx context.Context) ([]byte, string, error) {
			items, next, err := load(ctx)
			if err != nil {
				return nil, "", err
			}
			body, err := json.Marshal(items)
			return body, next, err
		})
	if err != nil {
		s.fail(c, err)
		return
	}
	if e.next != "" {
		c.Header(nextCursorHeader, e.next)
	}
	c.Header("ETag", e.etag)
	// The routes need a token: a browser may store the body but must revalidate every time.
	c.Header("Cache-Control", "private, no-cache")
	if etagMatches(c.GetHeader("If-None-Match"), e.etag) {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", e.body)
}

// wantsFresh reports whether a request's Cache-Control asks to bypass caches (no-cache, or
// max-age=0 as some clients send on reload).
func wantsFresh(header string) bool {
	for directive := range strings.SplitSeq(header, ",") {
		switch strings.ToLower(strings.TrimSpace(directive)) {
		case "no-cache", "max-age=0":
			return true
		}
	}
	return false
}

// etagMatches applies If-None-Match's weak comparison: "*" or any listed tag equal to etag,
// ignoring a W/ prefix.
func etagMatches(header, etag string) bool {
	for tag := range strings.SplitSeq(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "*" || strings.TrimPrefix(tag, "W/") == etag {
			return true
		}
	}
	return false
}
