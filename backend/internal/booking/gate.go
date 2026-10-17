package booking

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// dayGateWidth is how many bookings (and reschedules) of one dealership-day an instance lets into
// the database at once. They all queue on that day's advisory lock anyway (store.LockArrival); every extra waiter
// would only pin a pool connection, so a burst on one hot day (a promotion, Monday 08:00) could
// drain the pool and stall every other request of the instance. Past the width, bookings wait here,
// in memory, bounded by their request deadline (then 503 busy, like a lock timeout).
const dayGateWidth = 4

// dayGate is the per-instance admission gate in front of the dealership-day lock. Keys are the
// dealership and the local day of the start in its timezone (dayGateKey), the same day the
// database lock is keyed on (store.dealershipDayLockSQL): a UTC day would split the opening hours
// of a dealership far from UTC (Sydney opens on the previous UTC day) into two keys, doubling the
// waiters let into the database and leaving the second half's bookings uncontended, reading stale
// candidates and colliding under the lock.
type dayGate struct {
	mu    sync.Mutex
	width int
	slots map[string]*gateSlot
}

type gateSlot struct {
	tokens chan struct{}
	users  int // holders + waiters; the slot is dropped at zero
}

func newDayGate(width int) *dayGate {
	return &dayGate{width: max(1, width), slots: map[string]*gateSlot{}}
}

func dayGateKey(dealershipID uuid.UUID, start time.Time, loc *time.Location) string {
	return dealershipID.String() + "|" + DateFrom(start, loc).String()
}

// enter waits for a place under key (or ctx's end) and returns the function that gives it back,
// and whether anyone else held or awaited a place under key when this caller arrived: a contended
// booking reads its candidates after taking the day lock (allocation.lockFirst), since the ones
// ahead of it would make an earlier read stale.
func (g *dayGate) enter(ctx context.Context, key string) (release func(), contended bool, err error) {
	g.mu.Lock()
	sl := g.slots[key]
	if sl == nil {
		sl = &gateSlot{tokens: make(chan struct{}, g.width)}
		g.slots[key] = sl
	}
	sl.users++
	contended = sl.users > 1
	g.mu.Unlock()
	select {
	case sl.tokens <- struct{}{}:
		return func() {
			<-sl.tokens
			g.leave(key, sl)
		}, contended, nil
	case <-ctx.Done():
		g.leave(key, sl)
		return nil, false, ctx.Err()
	}
}

func (g *dayGate) leave(key string, sl *gateSlot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sl.users--; sl.users == 0 {
		delete(g.slots, key)
	}
}

// size is the number of keys with holders or waiters (tests).
func (g *dayGate) size() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.slots)
}

// hotMemory is how long an instance keeps reading a dealership's candidates under the day lock
// after one of its bookings there collided (Service.hot). A collision means another booking of
// that day wrote what this one had read as free, almost always from another instance (the gate
// orders this instance's own): a burst on one slot keeps colliding for as long as it lasts, while
// bookings of one day that do not compete for the same bay or technician never collide and keep
// reading in parallel, outside the lock. Keyed by dealership, not day: a burst moves on to the next
// day as one fills, and reading under a free day lock costs nothing (it saves a round trip).
const hotMemory = time.Second

// maxHot bounds the hot-dealership memory (past it the memory starts over: it is only a hint).
const maxHot = 4096

// hotDealers remembers, per dealership, until when this instance takes the day lock before reading
// candidates (allocation.lockFirst). The per-instance gate cannot see another instance's bookings;
// their collisions can, without any shared state.
type hotDealers struct {
	mu    sync.Mutex
	until map[uuid.UUID]time.Time
}

// mark makes the dealership hot until hotMemory after now.
func (h *hotDealers) mark(id uuid.UUID, now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.until == nil || len(h.until) >= maxHot {
		h.until = map[uuid.UUID]time.Time{}
	}
	h.until[id] = now.Add(hotMemory)
}

// hot reports whether the dealership is hot at now, forgetting it once it has cooled.
func (h *hotDealers) hot(id uuid.UUID, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.until[id]
	if ok && !now.Before(until) {
		delete(h.until, id)
		return false
	}
	return ok
}

// maxZones bounds the dealership timezone cache (a group has far fewer dealerships; past it the
// cache starts over rather than grow).
const maxZones = 4096

// zoneCache maps dealerships to their timezone for the gate key. A dealership's timezone never
// changes once it is opened (DealershipPatch has no timezone), so entries never go stale.
type zoneCache struct {
	mu    sync.Mutex
	zones map[uuid.UUID]*time.Location
}

func (z *zoneCache) get(id uuid.UUID) *time.Location {
	z.mu.Lock()
	defer z.mu.Unlock()
	return z.zones[id]
}

func (z *zoneCache) put(id uuid.UUID, loc *time.Location) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.zones == nil || len(z.zones) >= maxZones {
		z.zones = map[uuid.UUID]*time.Location{}
	}
	z.zones[id] = loc
}

// gateKey is dayGateKey for a dealership known by id: its timezone comes from the cache, or is read
// once (a plain read before the booking takes its TX connection). An unknown dealership or a failed
// read falls back to UTC, uncached: the gate only limits throughput, and the TX then answers 404
// or the same error.
func (s *Service) gateKey(ctx context.Context, dealershipID uuid.UUID, start time.Time) string {
	if loc := s.zones.get(dealershipID); loc != nil {
		return dayGateKey(dealershipID, start, loc)
	}
	tz, found, err := s.store.DealershipTimezone(ctx, dealershipID)
	if err != nil || !found {
		return dayGateKey(dealershipID, start, time.UTC)
	}
	return s.gateKeyIn(dealershipID, tz, start)
}

// gateKeyIn is gateKey when the caller already read the timezone (Reschedule), caching it.
func (s *Service) gateKeyIn(dealershipID uuid.UUID, tz string, start time.Time) string {
	loc, err := loadLocation(tz)
	if err != nil {
		return dayGateKey(dealershipID, start, time.UTC)
	}
	s.zones.put(dealershipID, loc)
	return dayGateKey(dealershipID, start, loc)
}

// readsUnderLock is allocation.lockFirst for a booking or reschedule at dealershipID: when this
// instance's gate saw others of its day ahead (contended), or the dealership is hot here (a booking
// there collided lately, hotDealers).
func (s *Service) readsUnderLock(contended bool, dealershipID uuid.UUID) bool {
	return s.lockFirst || contended || s.hot.hot(dealershipID, time.Now())
}
