package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/events"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// busRules decide which messages testBus refuses. A whole-batch error fails the call; otherwise
// each message gets the first per-message error that matches it (by Kafka key, by eventLabel, or
// every message), and the rest are recorded as sent.
type busRules struct {
	batch  error            // the whole PublishBatch call fails
	byKey  map[string]error // per message, by Kafka key (the aggregate id)
	byType map[string]error // per message, by eventLabel
	every  error            // per message, every message
}

// eventLabel names an event for busRules and testBus.types: the "label" the outbox tests put in a
// payload to tell their rows apart (event types are a closed set of codes), else the type's name.
func eventLabel(ev realtime.Event) string {
	if l := payloadLabel(ev.Payload); l != "" {
		return l
	}
	return ev.Type.Name()
}

// payloadLabel is the "label" field of an event payload, or "".
func payloadLabel(raw []byte) string {
	var p struct {
		Label string `json:"label"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.Label
}

func (r busRules) messageErr(env events.Envelope) error {
	if err := r.byKey[env.Key]; err != nil {
		return err
	}
	if err := r.byType[eventLabel(env.Event)]; err != nil {
		return err
	}
	return r.every
}

// testBus is this package's events bus: it records published envelopes in publish order and
// refuses messages by its rules. Safe for concurrent workers.
type testBus struct {
	mu    sync.Mutex
	rules busRules
	sent  []events.Envelope
}

func newTestBus(rules busRules) *testBus { return &testBus{rules: rules} }

func (b *testBus) PublishBatch(_ context.Context, batch []events.Envelope) ([]error, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.rules.batch != nil {
		return nil, b.rules.batch
	}
	errs := make([]error, len(batch))
	for i, env := range batch {
		if errs[i] = b.rules.messageErr(env); errs[i] == nil {
			b.sent = append(b.sent, env)
		}
	}
	return errs, nil
}

// update changes the rules between flushes.
func (b *testBus) update(change func(*busRules)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	change(&b.rules)
}

func (b *testBus) envelopes() []events.Envelope {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]events.Envelope(nil), b.sent...)
}

func (b *testBus) events() []realtime.Event {
	envs := b.envelopes()
	out := make([]realtime.Event, len(envs))
	for i, env := range envs {
		out[i] = env.Event
	}
	return out
}

// types is the published events' eventLabel in publish order.
func (b *testBus) types() []string {
	envs := b.envelopes()
	out := make([]string, len(envs))
	for i, env := range envs {
		out[i] = eventLabel(env.Event)
	}
	return out
}

// testLimiter is this package's rate limiter: in memory, with windows that never expire (tests
// are shorter than any window). It counts every hit. By default it allows everything; deny
// refuses buckets by key prefix, and an enforcing limiter applies each bucket's limit, overridden
// by key prefix in limits (so a test can reach a threshold of 50 without 50 bcrypt compares).
type testLimiter struct {
	mu      sync.Mutex
	enforce bool
	limits  map[string]int
	deny    string
	counts  map[string]int
}

func newTestLimiter() *testLimiter { return &testLimiter{counts: map[string]int{}} }

func newEnforcingLimiter(limits map[string]int) *testLimiter {
	return &testLimiter{enforce: true, limits: limits, counts: map[string]int{}}
}

// denyPrefix refuses every bucket whose key starts with prefix from now on.
func (l *testLimiter) denyPrefix(prefix string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deny = prefix
}

func (l *testLimiter) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, error) {
	return l.AllowAll(ctx, cache.Bucket{Key: bucket, Limit: limit, Window: window})
}

func (l *testLimiter) AllowAll(_ context.Context, buckets ...cache.Bucket) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ok := true
	for _, b := range buckets {
		l.counts[b.Key]++
		if l.deniedLocked(b) || (l.enforce && l.counts[b.Key] > l.limitLocked(b)) {
			ok = false
		}
	}
	return ok, nil
}

func (l *testLimiter) Exceeded(_ context.Context, b cache.Bucket) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.deniedLocked(b) || (l.enforce && l.counts[b.Key] >= l.limitLocked(b)), nil
}

func (l *testLimiter) deniedLocked(b cache.Bucket) bool {
	return l.deny != "" && strings.HasPrefix(b.Key, l.deny)
}

func (l *testLimiter) limitLocked(b cache.Bucket) int {
	limit := b.Limit
	for prefix, n := range l.limits {
		if strings.HasPrefix(b.Key, prefix) {
			limit = n
		}
	}
	return limit
}

// count is how many hits bucket key took.
func (l *testLimiter) count(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.counts[key]
}

// recordingSender is this package's notification sender: it records what it sends, or fails
// every send with fail.
type recordingSender struct {
	mu   sync.Mutex
	sent []store.Notification
	fail error
}

func (r *recordingSender) Send(_ context.Context, n store.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, n)
	return nil
}
