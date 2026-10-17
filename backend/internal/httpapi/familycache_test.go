package httpapi

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// A verdict is served until the TTL counted from its read (not its store), invalidate drops it,
// and a full cache makes room instead of growing.
func TestFamilyCache(t *testing.T) {
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	f := newFamilyCache(5*time.Second, 2)
	f.now = func() time.Time { return now }
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	if _, ok := f.get(a); ok {
		t.Fatal("hit on an empty cache")
	}
	f.put(a, true, now.Add(-2*time.Second)) // read 2 s ago
	if live, ok := f.get(a); !ok || !live {
		t.Fatalf("get after put = %v, %v", live, ok)
	}
	now = now.Add(3 * time.Second) // 5 s after the read
	if _, ok := f.get(a); ok {
		t.Fatal("served past the TTL counted from the read")
	}
	f.put(b, false, now)
	f.invalidate(b)
	if _, ok := f.get(b); ok {
		t.Fatal("served after invalidate")
	}
	f.put(a, true, now)
	f.put(b, false, now)
	f.put(c, true, now)
	if len(f.entries) > 2 {
		t.Fatalf("%d entries in a cache of 2", len(f.entries))
	}
	if live, ok := f.get(c); !ok || !live {
		t.Fatal("the newest entry was not kept")
	}
}
