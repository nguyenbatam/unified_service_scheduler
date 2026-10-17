package cache

import (
	"net"
	"testing"
	"time"
)

// A Redis that accepts connections but never answers must cost a request no more than the fast
// path's budget: Redis only accelerates, so the caller falls back to Postgres (idempotency) or
// fails open (limits). go-redis ignores context deadlines unless ContextTimeoutEnabled is set, and
// then waits out its own multi-second read timeout instead.
func TestFastPathHonoursItsBudgetWhenRedisHangs(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	c, err := New("redis://" + ln.Addr().String() + "/0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	t0 := time.Now()
	_, _, err = NewIdempotency(c).Get(t.Context(), "user", "key")
	if took := time.Since(t0); err == nil || took > 10*fastPathTimeout {
		t.Fatalf("Get on a hung Redis: err %v after %s, want an error within about %s", err, took, fastPathTimeout)
	}
}
