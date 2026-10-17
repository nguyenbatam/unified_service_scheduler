package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

func listenerWith(name string, drops int64, ids ...uuid.UUID) *wsListener {
	l := &wsListener{name: name, errs: &atomic.Int64{}}
	l.drops.Store(drops)
	for _, id := range ids {
		l.seen.Store(id, struct{}{})
	}
	return l
}

func TestCoverageGapsPerListener(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	want := map[uuid.UUID]bool{a: true, b: true}
	cases := []struct {
		name      string
		chaos     bool // -chaos-restart: lost sessions are excused
		listeners []*wsListener
		wantGaps  []string // substrings, one per expected gap
	}{
		{"every listener saw everything", false, []*wsListener{listenerWith("A#0", 0, a, b), listenerWith("A#1", 0, a, b)}, nil},
		{"one steady listener missed an event (another got it)", false,
			[]*wsListener{listenerWith("A#0", 0, a, b), listenerWith("A#1", 0, a)}, []string{"listener A#1 missed 1 of 2"}},
		{"no chaos: a lost session fails even while the backend covers it", false,
			[]*wsListener{listenerWith("A#0", 0, a, b), listenerWith("A#1", 1, a, b)}, []string{"listener A#1 lost its session 1 time(s)"}},
		{"chaos: a reconnected listener is excused while the backend still covers it", true,
			[]*wsListener{listenerWith("A#0", 0, a, b), listenerWith("A#1", 1, a)}, nil},
		{"no chaos: all reconnected and together still missing one", false,
			[]*wsListener{listenerWith("A#0", 1, a), listenerWith("A#1", 2, a)},
			[]string{"listener A#0 lost its session 1 time(s)", "listener A#1 lost its session 2 time(s)", "backend A (all listeners) missed 1 of 2"}},
		{"steady listener missing everything also fails the backend", false,
			[]*wsListener{listenerWith("A#0", 0)}, []string{"listener A#0 missed 2 of 2", "backend A (all listeners) missed 2 of 2"}},
		{"no listeners, nothing to check", false, nil, nil},
		{"chaos: every listener dropped (the backend was restarted)", true,
			[]*wsListener{listenerWith("A#0", 1, a), listenerWith("A#1", 1, a)}, nil},
		{"chaos: a steady one still holds the backend to its union", true,
			[]*wsListener{listenerWith("A#0", 0, a), listenerWith("A#1", 1)}, []string{"listener A#0 missed 1 of 2", "backend A (all listeners) missed 1 of 2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gaps := coverageGaps("A", tc.listeners, want, tc.chaos)
			if len(gaps) != len(tc.wantGaps) {
				t.Fatalf("gaps = %v, want %d matching %v", gaps, len(tc.wantGaps), tc.wantGaps)
			}
			for i, sub := range tc.wantGaps {
				if !strings.Contains(gaps[i], sub) {
					t.Fatalf("gap %d = %q, want it to contain %q", i, gaps[i], sub)
				}
			}
		})
	}
}

// A slow-consumer eviction (the hub's close 4000) is not a drop: outside chaos it excuses the
// listener's own misses (the backend's union still has to cover them) and makes no gap; any other
// close is a drop and fails, with its reason.
func TestSlowConsumerEvictionIsNotADrop(t *testing.T) {
	if hubCloseSlowConsumer != realtime.CloseSlowConsumer {
		t.Fatalf("the stress client's slow-consumer code %d is not the hub's %d", hubCloseSlowConsumer, realtime.CloseSlowConsumer)
	}
	a, b := uuid.New(), uuid.New()
	want := map[uuid.UUID]bool{a: true, b: true}
	evicted := listenerWith("A#1", 0, a)
	evicted.sessionEnded(&websocket.CloseError{Code: realtime.CloseSlowConsumer, Text: "slow consumer"})
	if gaps := coverageGaps("A", []*wsListener{listenerWith("A#0", 0, a, b), evicted}, want, false); len(gaps) != 0 {
		t.Fatalf("an evicted listener covered by its backend: gaps %v", gaps)
	}
	if gaps := coverageGaps("A", []*wsListener{evicted}, want, false); len(gaps) != 1 || !strings.Contains(gaps[0], "backend A (all listeners) missed 1 of 2") {
		t.Fatalf("an eviction still needs the backend's union: gaps %v", gaps)
	}
	dropped := listenerWith("A#2", 0, a, b)
	dropped.sessionEnded(&websocket.CloseError{Code: websocket.CloseAbnormalClosure})
	if gaps := coverageGaps("A", []*wsListener{dropped}, want, false); len(gaps) != 1 || !strings.Contains(gaps[0], "lost its session 1 time(s)") || !strings.Contains(gaps[0], "1006") {
		t.Fatalf("any other close fails with its reason: gaps %v", gaps)
	}
	w := &wsWatch{listenersA: []*wsListener{evicted}, listenersB: []*wsListener{dropped}}
	if w.evictions() != 1 || !strings.Contains(w.summary(want), "A#1=1/2(evicted=1)") {
		t.Fatalf("evictions %d, summary %q", w.evictions(), w.summary(want))
	}
}

// eventGaps applies the same drop rule to the "id|version" events.
func TestEventGapsFailDropsOutsideChaos(t *testing.T) {
	l := listenerWith("B#0", 1)
	l.events.Store("x|1", struct{}{})
	want := map[string]bool{"x|1": true}
	if gaps := eventGaps("B", []*wsListener{l}, want, false); len(gaps) != 1 || !strings.Contains(gaps[0], "lost its session") {
		t.Fatalf("no chaos: gaps = %v, want the lost session", gaps)
	}
	if gaps := eventGaps("B", []*wsListener{l}, want, true); len(gaps) != 0 {
		t.Fatalf("chaos: gaps = %v, want none", gaps)
	}
}

func TestWSSummaryReportsEveryListener(t *testing.T) {
	a := uuid.New()
	w := &wsWatch{
		listenersA: []*wsListener{listenerWith("A#0", 0, a)},
		listenersB: []*wsListener{listenerWith("B#0", 1)},
	}
	got := w.summary(map[uuid.UUID]bool{a: true})
	if got != "created_seen A#0=1/1 B#0=0/1(drops=1)" {
		t.Fatalf("summary = %q", got)
	}
}

// A handshake refused 429 at the hub's per-user cap fails start at once with a clear error,
// instead of re-dialling until the readiness timeout.
func TestWSStartReportsFullHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many connections", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	w := newWSWatch(t.Context(), tokenSession("token"))
	began := time.Now()
	err := w.start(srv.URL, srv.URL, 2, 10*time.Second)
	w.stop()
	if !errors.Is(err, errWSFull) || !strings.Contains(err.Error(), "max 10 per user per backend") {
		t.Fatalf("start = %v, want the 429 cap reported", err)
	}
	if time.Since(began) > 5*time.Second {
		t.Fatalf("start took %s: it waited for the timeout instead of failing on the 429", time.Since(began))
	}
	if got := w.listenersA[0].rejected.Load() + w.listenersB[0].rejected.Load() + w.listenersA[1].rejected.Load() + w.listenersB[1].rejected.Load(); got == 0 {
		t.Fatal("no listener counted the refused handshake")
	}
}

func TestJudgeOrder(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ev := func(id uuid.UUID, v int, typ string) wsArrival { return wsArrival{id: id, version: v, typ: typ} }
	cases := []struct {
		name                  string
		seq                   []wsArrival
		wantDup, wantReorders int
		wantChanged           int
	}{
		{"in order, interleaved appointments", []wsArrival{ev(a, 1, "C"), ev(b, 1, "C"), ev(a, 2, "M"), ev(b, 2, "X"), ev(a, 3, "X")}, 0, 0, 2},
		{"a redelivered notice is a duplicate, not a reordering", []wsArrival{ev(a, 1, "C"), ev(a, 2, "M"), ev(a, 2, "M")}, 1, 0, 1},
		{"an older version after a newer one is a reordering", []wsArrival{ev(a, 1, "C"), ev(a, 3, "M"), ev(a, 2, "R")}, 0, 1, 1},
		{"a late duplicate of an older version counts as a duplicate only", []wsArrival{ev(a, 1, "C"), ev(a, 2, "M"), ev(a, 1, "C")}, 1, 0, 1},
		{"versions may skip (a listener reconnected)", []wsArrival{ev(a, 1, "C"), ev(a, 4, "X")}, 0, 0, 1},
		{"different notices sharing a version are allowed", []wsArrival{ev(a, 2, "EXTENDED"), ev(a, 2, "OVERRUNNING")}, 0, 0, 0},
		{"first versions only: nothing to order", []wsArrival{ev(a, 1, "C"), ev(b, 1, "C")}, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := judgeOrder(tc.seq)
			if rep.duplicates != tc.wantDup || rep.outOfOrder != tc.wantReorders || rep.events != len(tc.seq) || rep.changed != tc.wantChanged {
				t.Fatalf("judgeOrder = %+v, want %d duplicates, %d reorderings", rep, tc.wantDup, tc.wantReorders)
			}
			if tc.wantReorders > 0 && len(rep.samples) == 0 {
				t.Fatal("a reordering without a sample")
			}
		})
	}
}
