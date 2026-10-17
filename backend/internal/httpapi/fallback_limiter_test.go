package httpapi

import (
	"fmt"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
)

func TestMemoryLimiterWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := newMemoryLimiter(100)
	m.now = func() time.Time { return now }
	ip := cache.Bucket{Key: "login:ip:1", Limit: 3, Window: time.Minute}
	email := cache.Bucket{Key: "login:ip-email:1|a@x", Limit: 2, Window: time.Minute}
	want := []bool{true, true, false, false}
	for i, w := range want {
		if got := m.allowAll([]cache.Bucket{ip, email}); got != w {
			t.Fatalf("hit %d: allowed %v, want %v", i+1, got, w)
		}
	}
	if !m.exceeded(email) {
		t.Fatal("exceeded = false after the limit")
	}
	if m.exceeded(ip) || m.windows[ip.Key].count != 2 {
		t.Fatalf("rejected requests counted in the other bucket: ip count %d, want 2", m.windows[ip.Key].count)
	}
	now = now.Add(time.Minute) // the window ends
	if m.exceeded(email) || !m.allowAll([]cache.Bucket{email}) {
		t.Fatal("a new window still counts the old hits")
	}
	if m.exceeded(cache.Bucket{Key: "login:fail:unknown", Limit: 1, Window: time.Minute}) {
		t.Fatal("an unseen bucket is exceeded")
	}
}

// The map never grows past its cap, whatever keys an attacker sprays.
func TestMemoryLimiterBounded(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := newMemoryLimiter(50)
	m.now = func() time.Time { return now }
	for i := range 1000 {
		m.allowAll([]cache.Bucket{{Key: fmt.Sprintf("login:ip:%d", i), Limit: 5, Window: time.Minute}})
		if len(m.windows) > 50 {
			t.Fatalf("%d windows after %d keys, cap 50", len(m.windows), i+1)
		}
	}
}

func TestFallbackBucket(t *testing.T) {
	for key, want := range map[string]bool{
		"login:ip:1": true, "login:fail:a@x": true, "register:ip:1": true,
		"read:ip:1": false, "write:user:x": false, "availability:ip:1": false,
	} {
		if got := fallbackBucket(cache.Bucket{Key: key}); got != want {
			t.Errorf("fallbackBucket(%q) = %v, want %v", key, got, want)
		}
	}
}

// A blocked client cannot spray fresh keys to evict its own exhausted window (or a victim
// account's): rejected requests create no windows, and eviction keeps exhausted windows.
func TestMemoryLimiterKeepsExhaustedWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := newMemoryLimiter(20)
	m.now = func() time.Time { return now }
	ip := cache.Bucket{Key: "login:ip:attacker", Limit: 3, Window: time.Minute}
	victim := cache.Bucket{Key: "login:fail:victim@x", Limit: 2, Window: time.Minute}
	m.allowAll([]cache.Bucket{victim})
	m.allowAll([]cache.Bucket{victim})
	for i := range 1000 {
		m.allowAll([]cache.Bucket{ip, {Key: fmt.Sprintf("login:ip-email:attacker|%d", i), Limit: 10, Window: time.Minute}})
	}
	if len(m.windows) != 5 { // ip, victim, and the 3 ip-email keys counted before the IP ran dry
		t.Fatalf("%d windows, want 5: rejected requests created windows", len(m.windows))
	}
	// Other clients fill the map: under-limit windows make room, exhausted ones stay.
	for i := range 1000 {
		m.allowAll([]cache.Bucket{{Key: fmt.Sprintf("login:ip:%d", i), Limit: 5, Window: time.Minute}})
	}
	if !m.exceeded(ip) || !m.exceeded(victim) {
		t.Fatal("an exhausted window was evicted early")
	}
	// A map full of exhausted windows refuses new keys rather than growing.
	full := newMemoryLimiter(2)
	full.now = m.now
	for _, key := range []string{"login:ip:a", "login:ip:b"} {
		full.allowAll([]cache.Bucket{{Key: key, Limit: 1, Window: time.Minute}})
	}
	if full.allowAll([]cache.Bucket{{Key: "login:ip:c", Limit: 1, Window: time.Minute}}) || len(full.windows) != 2 {
		t.Fatalf("full of exhausted windows: allowed a new key (%d windows)", len(full.windows))
	}
}

// An account's failure counter survives eviction once it has counted a failure (a spray of new
// keys from many addresses must not reset its guessing budget), and a map with no room stops
// scanning for a moment instead of paying a full scan per request.
func TestMemoryLimiterEvictionKeepsAccountFailures(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := newMemoryLimiter(10)
	m.now = func() time.Time { return now }
	victim := cache.Bucket{Key: "login:fail:victim@x", Limit: 5, Window: time.Minute}
	m.allowAll([]cache.Bucket{victim}) // one failure: under the limit
	for i := range 100 {
		m.allowAll([]cache.Bucket{{Key: fmt.Sprintf("login:ip:%d", i), Limit: 5, Window: time.Minute}})
	}
	if w := m.live(victim.Key, now); w == nil || w.count != 1 {
		t.Fatalf("victim's failure counter = %+v, want kept at 1", w)
	}

	full := newMemoryLimiter(1)
	full.now = m.now
	exhausted := cache.Bucket{Key: "login:ip:a", Limit: 1, Window: time.Minute}
	full.allowAll([]cache.Bucket{exhausted})
	newKey := cache.Bucket{Key: "login:ip:b", Limit: 1, Window: time.Minute}
	if full.allowAll([]cache.Bucket{newKey}) {
		t.Fatal("a full map of exhausted windows took a new key")
	}
	if !now.Before(full.noRoomUntil) {
		t.Fatal("a scan that freed nothing did not pause further scans")
	}
	now = now.Add(2 * time.Minute) // the exhausted window ends: room again
	if !full.allowAll([]cache.Bucket{newKey}) {
		t.Fatal("no room after the exhausted window ended")
	}
}

// While Redis is down every throttled request fails over: the warning is logged once per
// interval, and the next line says how many were held back.
func TestLogSamplerSpacesWarnings(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newLogSampler(10 * time.Second)
	l.now = func() time.Time { return now }
	if ok, n := l.take(); !ok || n != 0 {
		t.Fatalf("first: ok %v suppressed %d, want logged with 0", ok, n)
	}
	for range 5 {
		if ok, _ := l.take(); ok {
			t.Fatal("logged again within the interval")
		}
	}
	now = now.Add(10 * time.Second)
	if ok, n := l.take(); !ok || n != 5 {
		t.Fatalf("after the interval: ok %v suppressed %d, want logged with 5", ok, n)
	}
}

// A request any bucket refuses counts in none of them (a client past its own budget must not
// drain a bucket it shares), and a cost that does not fit is refused without counting.
func TestMemoryLimiterRefusalCountsNowhere(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := newMemoryLimiter(100)
	m.now = func() time.Time { return now }
	own := cache.Bucket{Key: "register:ip:1", Limit: 1, Window: time.Minute}
	shared := cache.Bucket{Key: "register:global", Limit: 3, Window: time.Minute}
	if !m.allowAll([]cache.Bucket{own, shared}) {
		t.Fatal("first hit refused")
	}
	for range 10 {
		if m.allowAll([]cache.Bucket{own, shared}) {
			t.Fatal("hit past the per-IP limit allowed")
		}
	}
	if got := m.windows[shared.Key].count; got != 1 {
		t.Fatalf("shared bucket counted %d, want 1: refused hits drained it", got)
	}
	big := shared
	big.Cost = 3
	if m.allowAll([]cache.Bucket{big}) || m.windows[shared.Key].count != 1 {
		t.Fatalf("cost 3 with 2 left: allowed or counted (%d)", m.windows[shared.Key].count)
	}
}
