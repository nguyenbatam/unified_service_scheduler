package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func TestEtagMatches(t *testing.T) {
	const tag = `"c-abc"`
	tests := []struct {
		header string
		want   bool
	}{
		{header: "", want: false},
		{header: `"c-abc"`, want: true},
		{header: `W/"c-abc"`, want: true},
		{header: `"other", "c-abc"`, want: true},
		{header: "*", want: true},
		{header: `"c-abd"`, want: false},
		{header: `c-abc`, want: false},
	}
	for _, tt := range tests {
		if got := etagMatches(tt.header, tag); got != tt.want {
			t.Errorf("etagMatches(%q) = %v, want %v", tt.header, got, tt.want)
		}
	}
}

// catalogServer serves serveCatalog on /cat with a counting loader returning *items.
func catalogServer(ttl time.Duration, now *time.Time, items *[]string, loads *int, loadErr *error) (*Server, http.Handler) {
	gin.SetMode(gin.TestMode)
	s := &Server{log: slog.New(slog.DiscardHandler), catalog: newCatalogCache(ttl)}
	s.catalog.now = func() time.Time { return *now }
	r := gin.New()
	r.GET("/cat", func(c *gin.Context) {
		s.serveCatalog(c, catalogDealerships.page(store.DefaultPageLimit, ""), func(context.Context) (any, string, error) {
			*loads++
			return *items, "", *loadErr
		})
	})
	return s, r
}

func getCatalog(t *testing.T, h http.Handler, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/cat", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestServeCatalogCachesAndRevalidates(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	items := []string{"a", "b"}
	loads := 0
	var loadErr error
	s, h := catalogServer(2*time.Second, &now, &items, &loads, &loadErr)

	first := getCatalog(t, h, "")
	etag := first.Header().Get("ETag")
	if first.Code != 200 || first.Body.String() != `["a","b"]` || etag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("first = %d %q etag %q", first.Code, first.Body.String(), etag)
	}
	if rec := getCatalog(t, h, etag); rec.Code != http.StatusNotModified || rec.Body.Len() != 0 || rec.Header().Get("ETag") != etag {
		t.Fatalf("revalidation = %d %q", rec.Code, rec.Body.String())
	}
	if rec := getCatalog(t, h, `"stale"`); rec.Code != 200 || rec.Body.String() != `["a","b"]` {
		t.Fatalf("other etag = %d", rec.Code)
	}
	if loads != 1 {
		t.Fatalf("loads = %d within the TTL, want 1", loads)
	}

	// Changed elsewhere: served stale until the TTL ends, then reloaded with a new ETag.
	items = []string{"a", "b", "c"}
	now = now.Add(1900 * time.Millisecond)
	if rec := getCatalog(t, h, ""); rec.Body.String() != `["a","b"]` {
		t.Fatalf("within TTL got %q", rec.Body.String())
	}
	now = now.Add(200 * time.Millisecond)
	rec := getCatalog(t, h, etag)
	if rec.Code != 200 || rec.Body.String() != `["a","b","c"]` || rec.Header().Get("ETag") == etag || loads != 2 {
		t.Fatalf("after TTL = %d %q loads %d", rec.Code, rec.Body.String(), loads)
	}

	// A local edit drops the entry at once.
	items = []string{"z"}
	s.InvalidateCatalog()
	if rec := getCatalog(t, h, ""); rec.Body.String() != `["z"]` || loads != 3 {
		t.Fatalf("after invalidate = %q loads %d", rec.Body.String(), loads)
	}

	// Errors are not cached.
	s.InvalidateCatalog()
	loadErr = errors.New("db down")
	if rec := getCatalog(t, h, ""); rec.Code != 500 {
		t.Fatalf("load error = %d", rec.Code)
	}
	loadErr = nil
	if rec := getCatalog(t, h, ""); rec.Code != 200 || loads != 5 {
		t.Fatalf("after error = %d loads %d", rec.Code, loads)
	}
}

func TestServeCatalogWithoutCacheStillSendsETag(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	items := []string{"a"}
	loads := 0
	var loadErr error
	_, h := catalogServer(0, &now, &items, &loads, &loadErr)
	etag := getCatalog(t, h, "").Header().Get("ETag")
	if rec := getCatalog(t, h, etag); rec.Code != http.StatusNotModified || loads != 2 {
		t.Fatalf("uncached revalidation = %d loads %d, want 304 after 2 loads", rec.Code, loads)
	}
}

// A load that started before an invalidation must not repopulate the cache with the old body.
func TestCatalogLoadAfterInvalidateIsDropped(t *testing.T) {
	cc := newCatalogCache(time.Minute)
	if _, err := cc.fetch(t.Context(), catalogServiceTypes.page(store.DefaultPageLimit, ""), false, func(context.Context) ([]byte, string, error) {
		cc.invalidate() // an edit commits while this load runs
		return []byte("old"), "", nil
	}); err != nil {
		t.Fatal(err)
	}
	loads := 0
	e, err := cc.fetch(t.Context(), catalogServiceTypes.page(store.DefaultPageLimit, ""), false, func(context.Context) ([]byte, string, error) {
		loads++
		return []byte("new"), "", nil
	})
	if err != nil || string(e.body) != "new" || loads != 1 {
		t.Fatalf("after invalidate got %v %q loads %d: the stale load was cached", err, e.body, loads)
	}
}

func TestWithCatalogCacheBounds(t *testing.T) {
	for _, tt := range []struct{ in, want time.Duration }{
		{-time.Second, 0}, {0, 0}, {time.Second, time.Second}, {time.Minute, MaxCatalogCacheTTL},
	} {
		s := &Server{}
		WithCatalogCache(tt.in)(s)
		if s.catalogTTL != tt.want {
			t.Errorf("WithCatalogCache(%v) = %v, want %v", tt.in, s.catalogTTL, tt.want)
		}
	}
}

// A reload sent with Cache-Control: no-cache (after an edit or CONFIG_UPDATED) skips the cached body
// and refreshes it for later readers.
func TestServeCatalogNoCacheRequestReadsThrough(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	items := []string{"a"}
	loads := 0
	var loadErr error
	_, h := catalogServer(5*time.Second, &now, &items, &loads, &loadErr)
	getCatalog(t, h, "")

	items = []string{"b"} // changed on another instance; this one has not seen CONFIG_UPDATED yet
	if rec := getCatalog(t, h, ""); rec.Body.String() != `["a"]` {
		t.Fatalf("plain read within TTL = %q, want the cached body", rec.Body.String())
	}
	for _, directive := range []string{"no-cache", "max-age=0", "No-Cache, no-store"} {
		now = now.Add(catalogRefreshGap) // past the refresh gap of the previous load
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/cat", nil)
		req.Header.Set("Cache-Control", directive)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 || rec.Body.String() != `["b"]` {
			t.Fatalf("Cache-Control %q = %d %q, want the fresh body", directive, rec.Code, rec.Body.String())
		}
	}
	if loads != 4 {
		t.Fatalf("loads = %d, want 4 (first read + three bypasses)", loads)
	}
	if rec := getCatalog(t, h, ""); rec.Body.String() != `["b"]` || loads != 4 {
		t.Fatalf("plain read after a bypass = %q loads %d, want the refreshed cache", rec.Body.String(), loads)
	}
}

// A flood of no-cache reloads costs at most one load per catalogRefreshGap: within the gap the
// body just loaded is served.
func TestServeCatalogNoCacheFloodIsThrottled(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	items := []string{"a"}
	loads := 0
	var loadErr error
	_, h := catalogServer(5*time.Second, &now, &items, &loads, &loadErr)
	noCache := func() *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/cat", nil)
		req.Header.Set("Cache-Control", "no-cache")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for range 50 {
		if rec := noCache(); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if loads != 1 {
		t.Fatalf("loads = %d for 50 no-cache requests within the gap, want 1", loads)
	}
	items = []string{"b"}
	now = now.Add(catalogRefreshGap)
	if rec := noCache(); rec.Body.String() != `["b"]` || loads != 2 {
		t.Fatalf("after the gap = %q loads %d, want a fresh load", rec.Body.String(), loads)
	}
}

// joinCountingCtx counts calls to Done: fetch calls it only to park on another request's load in
// flight (the leader's load here never looks at its context), so the count is the number of
// requests that joined, and a test can release the leader once all of them have.
type joinCountingCtx struct {
	context.Context
	joined *atomic.Int32
}

func (c joinCountingCtx) Done() <-chan struct{} {
	c.joined.Add(1)
	return c.Context.Done()
}

// awaitJoined waits until n requests have parked on the load in flight.
func awaitJoined(t *testing.T, joined *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for joined.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d requests joined the load in flight within 10s", joined.Load(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Concurrent misses share one load in flight. The leader's load is held until every other request
// has joined it, so even without a cache (ttl 0: a late request would load again) exactly one load
// runs, whatever the scheduling.
func TestCatalogFetchCoalescesConcurrentLoads(t *testing.T) {
	const n = 20
	for _, ttl := range []time.Duration{0, time.Second} {
		t.Run(ttl.String(), func(t *testing.T) {
			cc := newCatalogCache(ttl)
			release := make(chan struct{})
			var loads, joined atomic.Int32
			load := func(context.Context) ([]byte, string, error) {
				loads.Add(1)
				<-release
				return []byte("x"), "", nil
			}
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for range n {
				wg.Go(func() {
					e, err := cc.fetch(joinCountingCtx{t.Context(), &joined}, catalogDealerships.page(store.DefaultPageLimit, ""), true, load)
					if err == nil && string(e.body) != "x" {
						err = errors.New("wrong body " + string(e.body))
					}
					errs <- err
				})
			}
			// One request leads (inside load, holding it), the other n-1 join it; then release.
			awaitJoined(t, &joined, n-1)
			if got := loads.Load(); got != 1 {
				t.Fatalf("loads = %d with %d requests joined, want 1", got, n-1)
			}
			close(release)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if n := loads.Load(); n != 1 {
				t.Fatalf("loads = %d for 20 concurrent requests, want 1", n)
			}
		})
	}
}

// A waiter whose leader disconnected mid-load runs the load itself instead of failing; a real
// database error is shared.
func TestCatalogFetchRetriesWhenLeaderGoes(t *testing.T) {
	cc := newCatalogCache(time.Second)
	leaderCtx, cancelLeader := context.WithCancel(t.Context())
	inLoad := make(chan struct{})
	leaderDone := make(chan error, 1)
	go func() {
		_, err := cc.fetch(leaderCtx, catalogDealerships.page(store.DefaultPageLimit, ""), false, func(ctx context.Context) ([]byte, string, error) {
			close(inLoad)
			<-ctx.Done()
			return nil, "", ctx.Err()
		})
		leaderDone <- err
	}()
	<-inLoad
	waiter := make(chan error, 1)
	var joined atomic.Int32
	go func() {
		e, err := cc.fetch(joinCountingCtx{t.Context(), &joined}, catalogDealerships.page(store.DefaultPageLimit, ""), false, func(context.Context) ([]byte, string, error) {
			return []byte("ok"), "", nil
		})
		if err == nil && string(e.body) != "ok" {
			err = errors.New("wrong body")
		}
		waiter <- err
	}()
	awaitJoined(t, &joined, 1) // the waiter parked on the leader's call, not loading on its own
	cancelLeader()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader err = %v, want canceled", err)
	}
	if err := <-waiter; err != nil {
		t.Fatalf("waiter after its leader left: %v", err)
	}

	dbDown := errors.New("db down")
	cc.invalidate()
	if _, err := cc.fetch(t.Context(), catalogDealerships.page(store.DefaultPageLimit, ""), false, func(context.Context) ([]byte, string, error) { return nil, "", dbDown }); !errors.Is(err, dbDown) {
		t.Fatalf("err = %v, want the load error", err)
	}
}

// Each canonical page (listing, limit, cursor) is its own entry, cached with its next cursor; the
// ETag covers the cursor. Only the first page and cursors the cache handed out are cached, at the
// default and maximum limits: forged cursors and other limits are served uncached and never
// displace the canonical pages.
func TestCatalogCachesEachPage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	s := &Server{log: slog.New(slog.DiscardHandler), catalog: newCatalogCache(time.Minute)}
	loads := map[string]int{}
	next := "page-2"
	r := gin.New()
	r.GET("/cat", func(c *gin.Context) {
		cursor := c.Query("cursor")
		limit := store.MaxPageLimit
		if c.Query("limit") != "" {
			limit = 7
		}
		s.serveCatalog(c, catalogDealerships.page(limit, cursor), func(context.Context) (any, string, error) {
			loads[c.Query("limit")+"|"+cursor]++
			if cursor == "" {
				return []string{"a", "b"}, next, nil
			}
			return []string{"c"}, "", nil
		})
	})
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return rec
	}
	first, second := get("/cat"), get("/cat?cursor=page-2")
	if first.Body.String() != `["a","b"]` || first.Header().Get(nextCursorHeader) != "page-2" {
		t.Fatalf("first page = %q next %q", first.Body.String(), first.Header().Get(nextCursorHeader))
	}
	if second.Body.String() != `["c"]` || second.Header().Get(nextCursorHeader) != "" {
		t.Fatalf("last page = %q next %q", second.Body.String(), second.Header().Get(nextCursorHeader))
	}
	get("/cat?cursor=page-2")
	if again := get("/cat"); again.Header().Get(nextCursorHeader) != "page-2" || loads["|"] != 1 || loads["|page-2"] != 1 {
		t.Fatalf("cached pages lost their cursor (%q) or reloaded (%v)", again.Header().Get(nextCursorHeader), loads)
	}
	// Same rows, another next cursor: a different response, so a different tag.
	etag := first.Header().Get("ETag")
	s.InvalidateCatalog()
	next = "page-2b"
	if rec := get("/cat"); rec.Header().Get("ETag") == etag {
		t.Fatal("ETag ignores the next cursor")
	}
	cached := len(s.catalog.entries)

	// A cursor the cache never issued, or a limit clients do not use: loaded every time (with an
	// ETag, so revalidation still works), never cached.
	for range 3 * catalogMaxEntries {
		if rec := get("/cat?cursor=forged"); rec.Header().Get("ETag") == "" {
			t.Fatal("uncached page has no ETag")
		}
		get("/cat?limit=7")
	}
	if loads["|forged"] != 3*catalogMaxEntries || loads["7|"] != 3*catalogMaxEntries {
		t.Fatalf("non-canonical pages were cached: %v", loads)
	}
	if n := len(s.catalog.entries); n != cached {
		t.Fatalf("%d cached pages after non-canonical reads, want %d", n, cached)
	}
	if get("/cat"); loads["|"] != 2 {
		t.Fatalf("first page evicted by non-canonical reads: %v", loads)
	}
}

// A full cache evicts its expired entries, then only the oldest one, never everything.
func TestCatalogStoreEvictsOldest(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	cc := newCatalogCache(time.Minute)
	cc.now = func() time.Time { return now }
	for i := range catalogMaxEntries {
		cc.store(catalogDealerships.page(store.MaxPageLimit, fmt.Sprint(i)), &catalogEntry{fetchedAt: now.Add(time.Duration(i) * time.Millisecond)})
	}
	cc.store(catalogServiceTypes.page(store.MaxPageLimit, ""), &catalogEntry{fetchedAt: now})
	if n := len(cc.entries); n != catalogMaxEntries {
		t.Fatalf("%d entries, want %d", n, catalogMaxEntries)
	}
	if _, ok := cc.entries[catalogDealerships.page(store.MaxPageLimit, "0")]; ok {
		t.Fatal("oldest entry kept")
	}
	if _, ok := cc.entries[catalogDealerships.page(store.MaxPageLimit, "1")]; !ok {
		t.Fatal("a newer entry was evicted")
	}

	// Expired entries go before the oldest live one.
	now = now.Add(time.Minute + 2*time.Millisecond) // entries 1 and 2 (and the service types) are expired
	cc.store(catalogServiceTypes.page(store.DefaultPageLimit, ""), &catalogEntry{fetchedAt: now})
	if _, ok := cc.entries[catalogDealerships.page(store.MaxPageLimit, "3")]; !ok {
		t.Fatal("a live entry was evicted while expired ones remained")
	}
	if n := len(cc.entries); n != catalogMaxEntries-2 {
		t.Fatalf("%d entries after dropping the expired ones, want %d", n, catalogMaxEntries-2)
	}
}
