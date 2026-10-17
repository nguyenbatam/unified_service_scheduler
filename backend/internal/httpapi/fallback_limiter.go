package httpapi

import (
	"strings"
	"sync"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
)

// The shared limiter (Redis) fails open, which is right for reads and writes (an outage must not
// lock everyone out) but would switch off password brute-force protection for as long as Redis is
// down. Login and register buckets therefore fall back to a small per-instance counter while Redis
// errors: limits then hold per instance (N instances allow up to N times the shared limit), which
// still bounds guessing to a few dozen tries per window instead of none.

// fallbackLimiterMaxKeys bounds the fallback's memory. A request already over any of its limits is
// rejected without creating windows (so a blocked client cannot spray keys), and making room only
// ever drops expired or still-under-limit windows: an exhausted window (a blocked IP or account)
// lives until it ends. If every window is exhausted, new keys are refused (fail closed: this path
// only guards logins while Redis is down).
const fallbackLimiterMaxKeys = 10_000

// fallbackBucket reports whether b is protected by the in-memory fallback when Redis is down.
func fallbackBucket(b cache.Bucket) bool {
	return strings.HasPrefix(b.Key, "login:") || strings.HasPrefix(b.Key, "register:")
}

type memoryWindow struct {
	count int
	limit int
	ends  time.Time
}

// memoryLimiter is a bounded fixed-window counter local to one instance.
type memoryLimiter struct {
	mu      sync.Mutex
	maxKeys int
	now     func() time.Time
	windows map[string]*memoryWindow
	// noRoomUntil skips eviction scans for a moment after one freed nothing (a map full of
	// exhausted windows), so a spray of new keys does not pay a full scan per request.
	noRoomUntil time.Time
}

func newMemoryLimiter(maxKeys int) *memoryLimiter {
	return &memoryLimiter{maxKeys: maxKeys, now: time.Now, windows: make(map[string]*memoryWindow)}
}

// allowAll counts one hit in every bucket and reports whether all are within their limits (the
// contract of cache.RateLimiter.AllowAll): a request any bucket refuses counts nothing, and one
// that finds no room for a new window is refused.
func (m *memoryLimiter) allowAll(buckets []cache.Bucket) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for _, b := range buckets {
		count := 0
		if w := m.live(b.Key, now); w != nil {
			count = w.count
		}
		if count+b.Hits() > b.Limit {
			return false
		}
	}
	windows := make([]*memoryWindow, len(buckets))
	for i, b := range buckets {
		if windows[i] = m.window(b, now); windows[i] == nil {
			return false
		}
	}
	for i, b := range buckets {
		windows[i].count += b.Hits()
	}
	return true
}

// exceeded reports, without counting, whether b has used up its limit in the current window.
func (m *memoryLimiter) exceeded(b cache.Bucket) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.live(b.Key, m.now())
	return w != nil && w.count >= b.Limit
}

// retryAfter is how long until the last of buckets' exhausted windows ends (0 when none is).
func (m *memoryLimiter) retryAfter(buckets []cache.Bucket) time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var retry time.Duration
	for _, b := range buckets {
		if w := m.live(b.Key, now); w != nil && w.count >= b.Limit {
			retry = max(retry, w.ends.Sub(now))
		}
	}
	return retry
}

// live returns key's window unless it has ended.
func (m *memoryLimiter) live(key string, now time.Time) *memoryWindow {
	if w, ok := m.windows[key]; ok && now.Before(w.ends) {
		return w
	}
	return nil
}

// window returns b's live window, starting a new one (making room for it) when needed; nil when
// the map is full of exhausted windows.
func (m *memoryLimiter) window(b cache.Bucket, now time.Time) *memoryWindow {
	if w := m.live(b.Key, now); w != nil {
		return w
	}
	if _, ok := m.windows[b.Key]; !ok && len(m.windows) >= m.maxKeys {
		if now.Before(m.noRoomUntil) {
			return nil
		}
		m.evict(now)
		if len(m.windows) >= m.maxKeys {
			m.noRoomUntil = now.Add(time.Second)
			return nil
		}
	}
	w := &memoryWindow{limit: b.Limit, ends: now.Add(b.Window)}
	m.windows[b.Key] = w
	return w
}

// evict drops expired windows, then, if still full, live windows that are under their limit,
// down to 90% of the cap so a spray of new keys does not pay a full scan on every request. An
// exhausted window is never dropped early: that would hand its client a fresh budget; neither is
// an account's failure counter (login:fail:) once it has counted a failure, or spraying new keys
// from many addresses would reset an account's guessing budget.
func (m *memoryLimiter) evict(now time.Time) {
	for key, w := range m.windows {
		if !now.Before(w.ends) {
			delete(m.windows, key)
		}
	}
	for key, w := range m.windows {
		if len(m.windows) < m.maxKeys*9/10 {
			return
		}
		if w.count < w.limit && (w.count == 0 || !strings.HasPrefix(key, "login:fail:")) {
			delete(m.windows, key)
		}
	}
}

// limiterDownLogEvery spaces the "rate limiter unavailable" warnings: while Redis is down every
// throttled request fails over, and one line per request would flood the logs (the outage itself
// is visible as scheduler_dependency_up{dep="redis"} == 0).
const limiterDownLogEvery = 10 * time.Second

// logSampler lets one event through per interval and counts the ones it held back.
type logSampler struct {
	every time.Duration
	now   func() time.Time

	mu         sync.Mutex
	next       time.Time
	suppressed int
}

func newLogSampler(every time.Duration) *logSampler {
	return &logSampler{every: every, now: time.Now}
}

// take reports whether to log now and, if so, how many events were held back since the last one.
func (l *logSampler) take() (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if now.Before(l.next) {
		l.suppressed++
		return false, 0
	}
	n := l.suppressed
	l.next, l.suppressed = now.Add(l.every), 0
	return true, n
}
