package cache

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/testdb"
)

// newTestClient connects to TEST_REDIS_URL; these tests skip without it (and fail instead under
// REQUIRE_INTEGRATION=1, the one rule testdb applies to every integration dependency).
func newTestClient(t *testing.T) *Client {
	t.Helper()
	c, err := New(testdb.RedisURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("redis: %v", err)
	}
	return c
}

func TestRateLimiterAllowAll(t *testing.T) {
	r := NewRateLimiter(newTestClient(t))
	ctx := context.Background()
	ip := Bucket{Key: "test:ip:" + uuid.NewString(), Limit: 3, Window: time.Minute}
	email := Bucket{Key: "test:email:" + uuid.NewString(), Limit: 2, Window: time.Minute}

	for i, want := range []bool{true, true, false, false} {
		ok, err := r.AllowAll(ctx, ip, email)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Fatalf("hit %d: ok = %v, want %v", i+1, ok, want)
		}
	}
	// The refused hits counted nowhere: the IP bucket (2 of 3) still has room for exactly one.
	for i, want := range []bool{true, false} {
		if ok, err := r.Allow(ctx, ip.Key, ip.Limit, ip.Window); err != nil || ok != want {
			t.Fatalf("ip-only hit %d: ok = %v, err = %v, want %v", i+1, ok, err, want)
		}
	}
}

// A request that one bucket refuses must not burn the others it shares (one client past its own
// budget draining the per-IP or cluster-wide bucket), and a request whose cost does not fit is
// refused without counting, so a cheaper one may still pass.
func TestRateLimiterRefusalCountsNowhere(t *testing.T) {
	r := NewRateLimiter(newTestClient(t))
	ctx := context.Background()
	user := Bucket{Key: "test:user:" + uuid.NewString(), Limit: 1, Window: time.Minute}
	shared := Bucket{Key: "test:shared:" + uuid.NewString(), Limit: 3, Window: time.Minute}
	if ok, err := r.AllowAll(ctx, user, shared); err != nil || !ok {
		t.Fatalf("first hit: ok %v err %v", ok, err)
	}
	for range 10 { // the user is over its own limit: refused, and the shared bucket untouched
		if ok, err := r.AllowAll(ctx, user, shared); err != nil || ok {
			t.Fatalf("over the user limit: ok %v err %v, want refused", ok, err)
		}
	}
	expensive := shared
	expensive.Cost = 3
	if ok, err := r.AllowAll(ctx, expensive); err != nil || ok {
		t.Fatalf("cost 3 with 2 left: ok %v err %v, want refused", ok, err)
	}
	for i, want := range []bool{true, true, false} { // 1 of 3 used: two more single hits fit
		if ok, err := r.AllowAll(ctx, shared); err != nil || ok != want {
			t.Fatalf("shared hit %d: ok %v err %v, want %v", i+1, ok, err, want)
		}
	}
}

// Exceeded peeks without counting: it turns true exactly when the next Allow would be refused.
func TestRateLimiterExceededDoesNotCount(t *testing.T) {
	r := NewRateLimiter(newTestClient(t))
	ctx := context.Background()
	b := Bucket{Key: "test:peek:" + uuid.NewString(), Limit: 2, Window: time.Minute}

	for hit := range 3 {
		for range 3 { // peeking repeatedly must not use up the bucket
			exceeded, err := r.Exceeded(ctx, b)
			if err != nil {
				t.Fatal(err)
			}
			if want := hit >= b.Limit; exceeded != want {
				t.Fatalf("after %d hits: exceeded = %v, want %v", hit, exceeded, want)
			}
		}
		if ok, err := r.AllowAll(ctx, b); err != nil || ok != (hit < b.Limit) {
			t.Fatalf("hit %d: ok = %v, err = %v", hit+1, ok, err)
		}
	}
}

// A refusal reports the time left in the exceeded bucket's window, not a full window, and never
// more than the window; a bucket that is not exceeded does not stretch it.
func TestRateLimiterRetryIsRemainingWindow(t *testing.T) {
	r := NewRateLimiter(newTestClient(t))
	ctx := context.Background()
	short := Bucket{Key: "test:retry:short:" + uuid.NewString(), Limit: 1, Window: 2 * time.Second}
	long := Bucket{Key: "test:retry:long:" + uuid.NewString(), Limit: 100, Window: time.Hour}
	if ok, _, err := r.AllowAllRetry(ctx, short, long); err != nil || !ok {
		t.Fatalf("first hit: ok %v err %v", ok, err)
	}
	ok, retry, err := r.AllowAllRetry(ctx, short, long)
	if err != nil || ok {
		t.Fatalf("second hit: ok %v err %v, want refused", ok, err)
	}
	if retry <= 0 || retry > short.Window {
		t.Fatalf("retry %s, want within (0, %s] (the long bucket is not exceeded)", retry, short.Window)
	}
	exceeded, peekRetry, err := r.ExceededRetry(ctx, short)
	if err != nil || !exceeded || peekRetry <= 0 || peekRetry > short.Window {
		t.Fatalf("peek: exceeded %v retry %s err %v", exceeded, peekRetry, err)
	}
	if exceeded, retry, err := r.ExceededRetry(ctx, long); err != nil || exceeded || retry != 0 {
		t.Fatalf("peek under limit: exceeded %v retry %s err %v", exceeded, retry, err)
	}
}

func TestTicketsRedeemOnce(t *testing.T) {
	tickets := NewTickets(newTestClient(t))
	ctx := context.Background()
	id := uuid.NewString()
	for i, want := range []bool{true, false} {
		first, err := tickets.Redeem(ctx, id, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if first != want {
			t.Fatalf("redeem %d: first = %v, want %v", i+1, first, want)
		}
	}
}
