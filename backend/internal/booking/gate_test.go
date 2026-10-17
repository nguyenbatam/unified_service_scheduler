package booking

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// At most width holders per key at once (and exactly width while more wait); another key is never
// held up by the busy one; slots are dropped when idle.
func TestDayGateBoundsHoldersPerKey(t *testing.T) {
	g := newDayGate(2)
	key := dayGateKey(uuid.New(), time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC), time.UTC)
	var inside, peak atomic.Int32
	hold := make(chan struct{})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			release, _, err := g.enter(t.Context(), key)
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			<-hold
			inside.Add(-1)
			release()
		})
	}
	// Wait until the gate is full (two holders, eight waiting).
	for deadline := time.Now().Add(5 * time.Second); inside.Load() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d holders entered", inside.Load())
		}
	}
	// Another dealership-day goes straight in while this one is full.
	otherCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	other, _, err := g.enter(otherCtx, dayGateKey(uuid.New(), time.Now(), time.UTC))
	if err != nil {
		t.Fatalf("another key waited behind the full one: %v", err)
	}
	other()
	close(hold)
	wg.Wait()
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak holders = %d, want exactly 2", p)
	}
	if n := g.size(); n != 0 {
		t.Fatalf("%d idle slots left behind", n)
	}
}

// A waiter whose context ends gives up with its error and leaves no slot behind.
func TestDayGateWaiterHonoursContext(t *testing.T) {
	g := newDayGate(1)
	key := "k"
	release, _, err := g.enter(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := g.enter(ctx, key); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v, want deadline exceeded", err)
	}
	release()
	if n := g.size(); n != 0 {
		t.Fatalf("%d slots left after the waiter gave up and the holder left", n)
	}
}

// The first caller of a key is uncontended; one arriving while it holds (or waits) is contended,
// and once the key is idle again the next caller is not.
func TestDayGateReportsContention(t *testing.T) {
	g := newDayGate(4)
	release, contended, err := g.enter(t.Context(), "k")
	if err != nil || contended {
		t.Fatalf("first caller: contended %v err %v, want false/nil", contended, err)
	}
	second, contended, err := g.enter(t.Context(), "k")
	if err != nil || !contended {
		t.Fatalf("second caller: contended %v err %v, want true/nil", contended, err)
	}
	if _, contended, _ := g.enter(t.Context(), "other"); contended {
		t.Fatal("another key reported contention")
	}
	release()
	second()
	if _, contended, _ := g.enter(t.Context(), "k"); contended {
		t.Fatal("idle key reported contention")
	}
}

// The key is the dealership's local day, like the database day lock: a Sydney dealership's
// opening hours span two UTC days but one gate key, and the next local day is another key.
func TestDayGateKeyIsTheLocalDay(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney") // UTC+11 in March (daylight saving)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	opening := time.Date(2026, 3, 2, 8, 0, 0, 0, sydney)  // 2026-03-01 21:00 UTC
	closing := time.Date(2026, 3, 2, 17, 0, 0, 0, sydney) // 2026-03-02 06:00 UTC
	if opening.UTC().Day() == closing.UTC().Day() {
		t.Fatal("fixture must span two UTC days")
	}
	if a, b := dayGateKey(id, opening, sydney), dayGateKey(id, closing, sydney); a != b {
		t.Fatalf("one local day, two keys: %s vs %s", a, b)
	}
	if a, b := dayGateKey(id, closing, sydney), dayGateKey(id, closing.Add(16*time.Hour), sydney); a == b {
		t.Fatalf("the next local day shares the key %s", a)
	}
}

// A collision makes its dealership hot for hotMemory (and only that dealership); it then cools,
// and the memory never grows past maxHot.
func TestHotDealersCoolDown(t *testing.T) {
	var h hotDealers
	shop, other := uuid.New(), uuid.New()
	at := time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
	if h.hot(shop, at) {
		t.Fatal("hot before any collision")
	}
	h.mark(shop, at)
	if !h.hot(shop, at.Add(hotMemory-time.Millisecond)) {
		t.Fatal("cooled before hotMemory")
	}
	if h.hot(other, at) {
		t.Fatal("another dealership is hot")
	}
	if h.hot(shop, at.Add(hotMemory)) {
		t.Fatal("still hot after hotMemory")
	}
	if len(h.until) != 0 {
		t.Fatalf("a cooled dealership is still remembered: %d entries", len(h.until))
	}
	for range maxHot + 1 {
		h.mark(uuid.New(), at)
	}
	if len(h.until) > maxHot {
		t.Fatalf("memory grew to %d entries, bound %d", len(h.until), maxHot)
	}
}
