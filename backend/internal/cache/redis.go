package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const idempotencyTTL = 24 * time.Hour

// fastPathTimeout bounds idempotency cache calls: Redis only accelerates replays, so a slow Redis
// must cost a request at most this before it falls back to Postgres.
const fastPathTimeout = 100 * time.Millisecond

// Client wraps the shared Redis connection used by idempotency, rate limiting, and WS tickets.
type Client struct {
	rdb *redis.Client
}

func New(redisURL string) (*Client, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Without it go-redis ignores context deadlines and waits out its own read/write timeouts
	// (seconds) on a Redis that accepts connections but does not answer: every fast-path bound
	// here (fastPathTimeout, the readiness probe) would be void, and a hung Redis would stall
	// every request that touches it instead of failing open at once.
	opts.ContextTimeoutEnabled = true
	return &Client{rdb: redis.NewClient(opts)}, nil
}

func (c *Client) Ping(ctx context.Context) error { return c.rdb.Ping(ctx).Err() }

func (c *Client) Close() error { return c.rdb.Close() }

// Idempotency is a fast-path cache in front of the Postgres unique index (the source of truth).
// It stores only the appointment id and a request fingerprint; replays re-read the row from
// Postgres so they never return a stale status.
type Idempotency struct {
	c *Client
}

func NewIdempotency(c *Client) *Idempotency { return &Idempotency{c: c} }

func (i *Idempotency) Ping(ctx context.Context) error { return i.c.Ping(ctx) }

type IdempotencyRecord struct {
	AppointmentID uuid.UUID `json:"id"`
	Fingerprint   string    `json:"fp"`
}

// idemKey namespaces the client key by the creating user (scope) so keys never collide across users.
func idemKey(scope, key string) string {
	return "idempotency:appointment:" + scope + ":" + key
}

func (i *Idempotency) Get(ctx context.Context, scope, key string) (IdempotencyRecord, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, fastPathTimeout)
	defer cancel()
	raw, err := i.c.rdb.Get(ctx, idemKey(scope, key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	var rec IdempotencyRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return IdempotencyRecord{}, false, err
	}
	return rec, true, nil
}

func (i *Idempotency) Put(ctx context.Context, scope, key string, rec IdempotencyRecord) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, fastPathTimeout)
	defer cancel()
	return i.c.rdb.Set(ctx, idemKey(scope, key), raw, idempotencyTTL).Err()
}

// RateLimiter is a fixed-window counter shared by every API instance.
type RateLimiter struct {
	c *Client
}

func NewRateLimiter(c *Client) *RateLimiter { return &RateLimiter{c: c} }

// Bucket is one fixed-window counter: at most Limit hits per Window under Key.
type Bucket struct {
	Key    string
	Limit  int
	Window time.Duration
	// Cost is the hits one request counts (0 means 1), e.g. a multi-day availability scan.
	Cost int
}

// Hits is the number of hits b counts: Cost, at least 1.
func (b Bucket) Hits() int { return max(b.Cost, 1) }

// Allow counts one hit in bucket and reports whether it is within limit for the current window.
func (r *RateLimiter) Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, error) {
	return r.AllowAll(ctx, Bucket{Key: bucket, Limit: limit, Window: window})
}

// AllowAll counts each bucket's Hits in a single round trip when all of them stay within their
// limits, and reports whether they did (a refused request counts nowhere). Bounded by
// fastPathTimeout: the caller fails open on error, so a slow Redis delays a request by at most
// that.
func (r *RateLimiter) AllowAll(ctx context.Context, buckets ...Bucket) (bool, error) {
	ok, _, err := r.AllowAllRetry(ctx, buckets...)
	return ok, err
}

// allowAllScript is the check-then-count of AllowAllRetry, atomic on the Redis server. KEYS are the
// counters; ARGV holds, per key, its hits, limit, and window (ms). When any counter would pass its
// limit nothing is counted (a refused request must not burn a bucket it shares with others, e.g. a
// client past its own budget draining the per-IP or cluster-wide one), and the reply is 0 then
// the PTTL of every counter that refused (-3: this one did not); otherwise every counter is
// incremented, a new one starting its window, and the reply is 1.
var allowAllScript = redis.NewScript(`
local refused = {}
local over = false
for i, key in ipairs(KEYS) do
  local hits, limit = tonumber(ARGV[3*i-2]), tonumber(ARGV[3*i-1])
  if tonumber(redis.call('GET', key) or '0') + hits > limit then
    over = true
    refused[i] = redis.call('PTTL', key)
  else
    refused[i] = -3
  end
end
if over then
  table.insert(refused, 1, 0)
  return refused
end
for i, key in ipairs(KEYS) do
  redis.call('INCRBY', key, ARGV[3*i-2])
  if redis.call('PTTL', key) < 0 then
    redis.call('PEXPIRE', key, ARGV[3*i])
  end
end
return {1}
`)

// AllowAllRetry is AllowAll that also returns, when refused, how long until the last exceeded
// bucket's window ends (its remaining TTL, read in the same round trip): what Retry-After should
// say, instead of a full window when the window is about to reset. A refused request counts in
// no bucket (see allowAllScript); a bucket refuses a request whose Hits would take it past Limit.
func (r *RateLimiter) AllowAllRetry(ctx context.Context, buckets ...Bucket) (bool, time.Duration, error) {
	if len(buckets) == 0 {
		return true, 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, fastPathTimeout)
	defer cancel()
	keys := make([]string, len(buckets))
	args := make([]any, 0, 3*len(buckets))
	for i, b := range buckets {
		keys[i] = "ratelimit:" + b.Key
		args = append(args, b.Hits(), b.Limit, max(b.Window.Milliseconds(), 1))
	}
	// Run is one EVALSHA round trip once the script is cached (EVAL the first time on a server).
	reply, err := allowAllScript.Run(ctx, r.c.rdb, keys, args...).Int64Slice()
	if err != nil {
		return false, 0, err
	}
	if len(reply) > 0 && reply[0] == 1 {
		return true, 0, nil
	}
	if len(reply) != len(buckets)+1 {
		return false, 0, fmt.Errorf("rate limit script: %d replies for %d buckets", len(reply), len(buckets))
	}
	var retry time.Duration
	for i, b := range buckets {
		if ttl := reply[i+1]; ttl != -3 {
			retry = max(retry, remaining(time.Duration(ttl)*time.Millisecond, b.Window))
		}
	}
	return false, retry, nil
}

// remaining is a counter's TTL as a retry delay: the window when Redis reports none (a negative
// PTTL: no key or no expiry), never more than the window.
func remaining(ttl, window time.Duration) time.Duration {
	if ttl <= 0 || ttl > window {
		return window
	}
	return ttl
}

// Exceeded reports, without counting a hit, whether bucket has already used up its limit in the
// current window.
func (r *RateLimiter) Exceeded(ctx context.Context, b Bucket) (bool, error) {
	exceeded, _, err := r.ExceededRetry(ctx, b)
	return exceeded, err
}

// ExceededRetry is Exceeded that also returns, when exceeded, the window's remaining time.
func (r *RateLimiter) ExceededRetry(ctx context.Context, b Bucket) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, fastPathTimeout)
	defer cancel()
	key := "ratelimit:" + b.Key
	pipe := r.c.rdb.Pipeline()
	get := pipe.Get(ctx, key)
	ttl := pipe.PTTL(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return false, 0, err
	}
	n, err := get.Int64()
	if errors.Is(err, redis.Nil) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if n < int64(b.Limit) {
		return false, 0, nil
	}
	return true, remaining(ttl.Val(), b.Window), nil
}

// Tickets makes WebSocket tickets single-use across every instance.
type Tickets struct {
	c *Client
}

func NewTickets(c *Client) *Tickets { return &Tickets{c: c} }

// Redeem marks ticket id as used for ttl (the ticket's remaining lifetime) and reports whether
// this was the first redemption; false means a replay.
func (t *Tickets) Redeem(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, fastPathTimeout)
	defer cancel()
	return t.c.rdb.SetNX(ctx, "ws-ticket:"+id, 1, ttl).Result()
}
