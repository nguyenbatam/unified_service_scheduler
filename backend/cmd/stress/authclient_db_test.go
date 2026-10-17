package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The real API with ACCESS_TOKEN_TTL forced to 2 s (the config minimum is 1 min; the auth service
// takes any positive TTL): the stress client, many goroutines through two clients of one session,
// keeps working across several access-token expiries by refreshing with its cookie, and the server
// never sees a replayed refresh token (reuse detection would revoke the session, and the calls
// after it would fail).
func TestStressClientOutlivesShortAccessTTL(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	ctx := t.Context()
	st := store.New(pool)
	if err := seed.Run(ctx, st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	signer := auth.New("stress-client-test-secret-0123456789abcdef", auth.WithAccessTTL(2*time.Second))
	srv := httptest.NewServer(httpapi.New(booking.New(st, nil), st, signer, nil, slog.New(slog.DiscardHandler), "test",
		httpapi.WithSessions(httpapi.SessionConfig{CookieSecure: false})).Handler())
	defer srv.Close()

	api := newAPI(srv.URL)
	var lost atomic.Pointer[error]
	if err := api.login(ctx, "admin@scheduler.local", "admin123", func(err error) { lost.Store(&err) }); err != nil {
		t.Fatal(err)
	}
	if api.auth.noCookie.Load() {
		t.Fatal("login set no refresh cookie the client sends back")
	}
	other := api.asUser(srv.URL)
	began := time.Now()
	var wg sync.WaitGroup
	var calls, failed atomic.Int64
	for g := range 8 {
		c := api
		if g%2 == 1 {
			c = other
		}
		wg.Go(func() {
			for time.Since(began) < 7*time.Second {
				calls.Add(1)
				if _, err := getAll[domain.Dealership](ctx, c, "/api/v1/dealerships", 0, nil); err != nil {
					failed.Add(1)
					t.Errorf("call %s after start: %v", time.Since(began).Round(time.Millisecond), err)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
	wg.Wait()
	if p := lost.Load(); p != nil {
		t.Fatalf("session lost: %v", *p)
	}
	if failed.Load() > 0 {
		t.Fatalf("%d of %d calls failed", failed.Load(), calls.Load())
	}
	if n := api.auth.refreshes.Load(); n < 3 || api.auth.relogins.Load() != 0 {
		t.Fatalf("over 7 s of 2 s tokens: %s; want ≥ 3 refreshes and no re-login", api.auth.report())
	}
	// Still the same, unrevoked session: a replayed refresh token would have bumped token_version.
	if resp, err := api.do(ctx, http.MethodGet, "/api/v1/auth/me", nil, nil); err != nil || resp.code != http.StatusOK {
		t.Fatalf("GET /auth/me after the run = %d, %v", resp.code, err)
	}
	t.Logf("%d calls; session: %s", calls.Load(), api.auth.report())
	m := newMatrix()
	recordSession(ctx, m, api.auth, time.Since(began))
	if ev := m.rows["Q4"]; len(ev) != 1 || ev[0].Verdict != verdictPass {
		t.Fatalf("client session evidence = %+v, want one PASS", ev)
	}
}
