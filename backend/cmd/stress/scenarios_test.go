package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestJudgeIdempotentRace(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	ok201 := idemAnswer{code: http.StatusCreated, id: a}
	ok200 := idemAnswer{code: http.StatusOK, id: a}
	cases := []struct {
		name    string
		answers []idemAnswer
		wantErr string
	}{
		{"one 201, the rest replays", []idemAnswer{ok200, ok201, ok200}, ""},
		{"an unresolved racer is tolerated", []idemAnswer{ok201, {code: 0, retried: true}, ok200}, ""},
		{"lost 201 after a retry", []idemAnswer{ok200, {code: http.StatusOK, id: a, retried: true}}, ""},
		{"no 201 and nobody retried", []idemAnswer{ok200, ok200}, "no 201"},
		{"two 201s", []idemAnswer{ok201, ok201}, "2 racers got 201"},
		{"two appointments for one key", []idemAnswer{ok201, {code: http.StatusOK, id: b}}, "two appointments"},
		{"the key collides with itself", []idemAnswer{ok201, {code: http.StatusConflict, body: "conflict"}}, "racer answered 409"},
		{"a 500", []idemAnswer{ok201, {code: http.StatusInternalServerError}}, "racer answered 500"},
		{"nothing definite", []idemAnswer{{code: 0, retried: true}}, "no racer got a definite answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := judgeIdempotentRace(tc.answers)
			if tc.wantErr == "" {
				if err != nil || id != a {
					t.Fatalf("= %s, %v; want %s", id, err, a)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestCustomerTally(t *testing.T) {
	tally := func(codes ...int) *customerTally {
		ct := &customerTally{byCode: map[int]int{}}
		for _, c := range codes {
			id := uuid.Nil
			if c == http.StatusCreated {
				id = uuid.New()
			}
			ct.add(c, id, "body")
		}
		return ct
	}
	cases := []struct {
		name     string
		tally    *customerTally
		vehicles int
		wantErr  string
	}{
		{"limits answered 409/422/429, one booking", tally(201, 409, 422, 429, 429, 503, 0), 1, ""},
		{"throttled before any booking proves nothing", tally(429, 429), 1, "no booking succeeded"},
		{"two bookings for one vehicle", tally(201, 201), 1, "2 bookings at one start for 1 vehicle"},
		{"a 500", tally(201, 500), 1, "unexpected answers [500: body]"},
		{"a 400", tally(400), 1, "unexpected answers"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tally.judge(tc.vehicles)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("judge = %v, want %q", err, tc.wantErr)
			}
		})
	}
	if got := tally(429, 201, 0, 429).String(); got != "unresolved=1 201=1 429=2" {
		t.Fatalf("String = %q", got)
	}
}

func TestJudgeLead(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		definite bool
		ran      bool
		wantErr  string
	}{
		{"refused: the check works", http.StatusUnprocessableEntity, true, true, ""},
		{"booked inside the lead time", http.StatusCreated, true, true, "lead time was accepted"},
		{"replayed booking inside the lead time", http.StatusOK, true, true, "lead time was accepted"},
		{"slot taken is not the lead-time answer", http.StatusConflict, true, true, "answered 409, want 422"},
		{"a 500", http.StatusInternalServerError, true, true, "answered 500"},
		{"still throttled: did not run", http.StatusTooManyRequests, false, false, ""},
		{"still busy: did not run", http.StatusServiceUnavailable, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran, err := judgeLead(tc.code, tc.definite, "body")
			if ran != tc.ran || tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("judgeLead(%d, %v) = %v, %v; want %v, %q", tc.code, tc.definite, ran, err, tc.ran, tc.wantErr)
			}
		})
	}
}

// customerFake serves the customer burst: login, one vehicle, one free start, and bookings. The
// lead-time booking (key suffix :cust:lead) gets leadCodes in turn (the last one repeats); the
// burst gets 201 for its first booking and 429 after (the per-user throttle).
type customerFake struct {
	start     time.Time
	leadCodes []int

	mu     sync.Mutex
	keys   []string // Idempotency-Key of every POST, in arrival order
	leadN  int
	booked bool
}

func (f *customerFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/auth/login":
		writeJSON(w, map[string]string{"token": "t"})
	case "/api/v1/vehicles":
		writeJSON(w, []domain.Vehicle{{ID: uuid.New()}})
	case "/api/v1/availability/next":
		writeJSON(w, domain.NextAvailability{Slots: []domain.NextSlot{{Date: f.start.Format(time.DateOnly), Start: f.start}}})
	case "/api/v1/availability":
		writeJSON(w, domain.Availability{Slots: []domain.Slot{slotAt(f.start, true)}})
	case "/api/v1/appointments":
		key := r.Header.Get("Idempotency-Key")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.keys = append(f.keys, key)
		code := http.StatusTooManyRequests
		switch {
		case strings.HasSuffix(key, ":cust:lead"):
			code = f.leadCodes[min(f.leadN, len(f.leadCodes)-1)]
			f.leadN++
		case !f.booked:
			code, f.booked = http.StatusCreated, true
		}
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(code)
		if code == http.StatusCreated {
			writeJSON(w, map[string]any{"id": uuid.New()})
		}
	default:
		http.NotFound(w, r)
	}
}

// writeJSON encodes v as a fake's answer body.
func writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// The lead-time booking goes out before the burst spends the write budget; it must be 422, and a
// lead-time booking that stays throttled is reported as not run, never as passed.
func TestCustomerBurstProbesLeadTimeFirst(t *testing.T) {
	cases := []struct {
		name           string
		leadCodes      []int
		wantErr        string
		wantIDs        int
		wantUnresolved int
	}{
		{"422 after one 429", []int{http.StatusTooManyRequests, http.StatusUnprocessableEntity}, "", 1, 0},
		{"throttled throughout: not run", []int{http.StatusTooManyRequests}, "", 1, 0},
		{"busy throughout: not run, may have committed", []int{http.StatusServiceUnavailable}, "", 1, 1},
		{"booked inside the lead time", []int{http.StatusCreated}, "lead time was accepted", 1, 0},
		{"a definite non-422", []int{http.StatusConflict}, "want 422", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &customerFake{start: time.Now().Add(3 * time.Hour).Truncate(30 * time.Minute).UTC(), leadCodes: tc.leadCodes}
			srv := httptest.NewServer(f)
			defer srv.Close()
			p := &plan{dealer: domain.Dealership{ID: uuid.New(), Policy: domain.Policy{CustomerLeadMinutes: 120}}, jobs: [][]uuid.UUID{{uuid.New()}}}
			s := settings{base: srv.URL, customerEmail: "c@example.com", customerRequests: 5}
			ids, unresolved, err := customerBurst(t.Context(), s, p, "stress:t", nil)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if len(ids) != tc.wantIDs || unresolved != tc.wantUnresolved {
				t.Fatalf("ids %v unresolved %d, want %d id(s), %d unresolved", ids, unresolved, tc.wantIDs, tc.wantUnresolved)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			leads := min(f.leadN, probeAttempts)
			for i, key := range f.keys {
				if lead := strings.HasSuffix(key, ":cust:lead"); lead != (i < leads) {
					t.Fatalf("POST %d has key %q; want the %d lead-time attempt(s) first, then the burst: %v", i, key, leads, f.keys)
				}
			}
			if tc.wantErr == "" && len(f.keys) != leads+s.customerRequests {
				t.Fatalf("%d POSTs, want %d lead-time + %d burst", len(f.keys), leads, s.customerRequests)
			}
		})
	}
}

func TestInstanceShortfall(t *testing.T) {
	n := func(answers int) instanceTally { return instanceTally{answers: answers} }
	cases := []struct {
		name     string
		answered map[string]instanceTally
		min      int
		wantErr  bool
	}{
		{"both backends", map[string]instanceTally{"a": n(10), "b": n(3)}, 2, false},
		{"one backend only refused, still counts", map[string]instanceTally{"a": {answers: 10, created: 2}, "b": n(4)}, 2, false},
		{"one backend took everything", map[string]instanceTally{"a": n(13)}, 2, true},
		{"missing header names no instance", map[string]instanceTally{"a": n(10), "": n(5)}, 2, true},
		{"zero counts do not count", map[string]instanceTally{"a": n(10), "b": n(0)}, 2, true},
		{"one required", map[string]instanceTally{"a": n(1)}, 1, false},
		{"nothing answered, nothing required", map[string]instanceTally{}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := instanceShortfall(tc.answered, tc.min); (err != nil) != tc.wantErr {
				t.Fatalf("instanceShortfall = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	got := instanceSummary(map[string]instanceTally{"b": {answers: 2, created: 1}, "a": n(1), "": n(3)})
	if got != "(no X-Instance-ID)=3 (created 0) a=1 (created 0) b=2 (created 1)" {
		t.Fatalf("instanceSummary = %q", got)
	}
}

// Every definite booking answer (201/200/409/422) counts for the instance that sent it, so a run
// with few winners still sees both backends; indefinite answers and bugs count for none.
func TestSendWithRetryCountsInstances(t *testing.T) {
	answers := []struct {
		inst string
		code int
	}{
		{"a", http.StatusCreated}, {"b", http.StatusConflict}, {"b", http.StatusUnprocessableEntity},
		{"a", http.StatusOK}, {"b", http.StatusServiceUnavailable}, {"b", http.StatusInternalServerError},
	}
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		a := answers[n%len(answers)]
		n++
		mu.Unlock()
		w.Header().Set("X-Instance-ID", a.inst)
		w.WriteHeader(a.code)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New()})
	}))
	defer srv.Close()
	stats := newRunStats()
	for i := range answers {
		bookWithRetry(t.Context(), newAPI(srv.URL), map[string]any{}, fmt.Sprint("k", i), 1, stats)
	}
	want := map[string]instanceTally{"a": {answers: 2, created: 1}, "b": {answers: 2}}
	if got := stats.answeredBy(); !maps.Equal(got, want) {
		t.Fatalf("answeredBy = %v, want %v", got, want)
	}
}

func TestSettingsNormalize(t *testing.T) {
	ok := settings{concurrency: 10, wsClients: hubMaxSocketsPerUser, phases: []string{"river"}, base: "http://gw/", backendA: "http://a", backendB: "http://b"}
	if err := ok.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if ok.base != "http://gw" || ok.vehicles != 10 || ok.workers != 10 || !ok.viaGateway() {
		t.Fatalf("normalized %+v", ok)
	}
	direct := settings{phases: []string{"river"}, base: "http://a", backendA: "http://a"}
	if err := direct.normalize(); err != nil || direct.viaGateway() {
		t.Fatalf("direct backend: err %v, viaGateway %v", err, direct.viaGateway())
	}
	tooMany := settings{wsClients: hubMaxSocketsPerUser + 1, phases: []string{"river"}}
	if err := tooMany.normalize(); err == nil || !strings.Contains(err.Error(), "at most 10") {
		t.Fatalf("ws-clients over the hub cap: err = %v", err)
	}
}

func TestNextDate(t *testing.T) {
	if got, err := nextDate("2026-12-31"); err != nil || got != "2027-01-01" {
		t.Fatalf("nextDate = %q, %v", got, err)
	}
	if _, err := nextDate("31/12/2026"); err == nil {
		t.Fatal("nextDate accepted a malformed date")
	}
}

func slotAt(start time.Time, avail bool) domain.Slot {
	return domain.Slot{Start: start, End: start.Add(30 * time.Minute), Available: avail, FreeBays: 2, FreeTechnicians: 2}
}

// oilShop is a shop of n express bays and n lubrication technicians, no holds, and the plan's
// one job mix: an oil change.
func oilShop(n int) (*oracleShop, []jobMix) {
	s := &oracleShop{loc: time.UTC, now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), pinned: map[uuid.UUID]bool{}}
	for range n {
		s.bays = append(s.bays, oBay{id: uuid.New(), bayType: domain.BayExpress})
		s.techs = append(s.techs, oTech{id: uuid.New(), level: 1, skills: map[domain.TechnicianSkill]bool{domain.SkillLubrication: true}})
	}
	oil := jobRequirement{minutes: 30, bayType: domain.BayExpress, skills: []domain.TechnicianSkill{domain.SkillLubrication}, minLevel: 1}
	return s, []jobMix{{name: "OIL", req: oil}}
}

// Starts before the cutoff are never hammered: a slot minutes away would slide into the past
// while it is hammered and checked.
func TestPickSlotsSkipsStartsBeforeCutoff(t *testing.T) {
	t0 := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	avail := domain.Availability{Slots: []domain.Slot{
		slotAt(t0, true), slotAt(t0.Add(30*time.Minute), true), slotAt(t0.Add(time.Hour), false), slotAt(t0.Add(90*time.Minute), true),
	}}
	shop, mixes := oilShop(2)
	p := &plan{date: "2030-01-07", loc: time.UTC, cutoff: t0.Add(15 * time.Minute), mixes: mixes}
	if err := p.pickSlots([]domain.Availability{avail}, shop, 10, 5); err != nil {
		t.Fatal(err)
	}
	if len(p.slots) != 2 || !p.slots[0].start.Equal(t0.Add(30*time.Minute)) || !p.slots[1].start.Equal(t0.Add(90*time.Minute)) {
		t.Fatalf("slots %+v, want 09:30 and 10:30", p.slots)
	}
	if p.slots[0].maxWinners != 2 {
		t.Fatalf("bound %d, want 2 (two bays, two technicians)", p.slots[0].maxWinners)
	}
	p.cutoff = t0.Add(2 * time.Hour)
	if err := p.pickSlots([]domain.Availability{avail}, shop, 10, 5); err == nil || len(p.slots) != 0 {
		t.Fatalf("every start before the cutoff: err %v slots %v; want an error (the caller rolls to the next date)", err, p.slots)
	}
}

// The slot bound is the oracle's, not availability's free counts: a confirmed hold that has not
// started may be moved by a one-move repair, so its bay and technician still count as room; a
// checked-in hold never moves. Exact mode only hammers clean starts (no hold overlaps them), where
// that bound is exactly what fills.
func TestSlotBoundCountsRepairableRoom(t *testing.T) {
	t0 := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	shop, mixes := oilShop(3)
	p := &plan{date: "2030-01-07", loc: time.UTC, cutoff: t0, mixes: mixes}
	variants := p.boundVariants(0)
	movable := oHold{id: uuid.New(), bay: shop.bays[0].id, tech: shop.techs[0].id, status: 1, window: timeWindow{t0.Add(-15 * time.Minute), t0.Add(15 * time.Minute)}}
	checkedIn := oHold{id: uuid.New(), bay: shop.bays[1].id, tech: shop.techs[1].id, status: 2, window: timeWindow{t0, t0.Add(time.Hour)}}
	shop.holds = []oHold{movable, checkedIn}
	if bound, clean := shop.slotBound(variants, t0); bound != 2 || clean {
		t.Fatalf("bound %d clean %v, want 2 (the checked-in hold's bay and technician are stuck) and not clean", bound, clean)
	}
	if bound, clean := shop.slotBound(variants, t0.Add(time.Hour)); bound != 3 || !clean {
		t.Fatalf("after both holds: bound %d clean %v, want 3 and clean", bound, clean)
	}
	shop.now = t0 // the movable hold has started: it stays where it is
	if bound, _ := shop.slotBound(variants, t0); bound != 1 {
		t.Fatalf("started hold: bound %d, want 1", bound)
	}
	avail := domain.Availability{Slots: []domain.Slot{slotAt(t0, true), slotAt(t0.Add(time.Hour), true)}}
	p.exact = true
	if err := p.pickSlots([]domain.Availability{avail}, shop, 10, 5); err != nil {
		t.Fatal(err)
	}
	if len(p.slots) != 1 || !p.slots[0].start.Equal(t0.Add(time.Hour)) || p.slots[0].maxWinners != 3 {
		t.Fatalf("exact slots %+v, want only the clean 10:00 with bound 3", p.slots)
	}
}

func TestMoveTargetsAvoidHammeredWindows(t *testing.T) {
	t0 := time.Date(2030, 1, 7, 9, 0, 0, 0, time.UTC)
	var slots []domain.Slot
	for i := range 10 { // 09:00 … 13:30
		slots = append(slots, slotAt(t0.Add(time.Duration(i)*30*time.Minute), i != 8))
	}
	hammered := []slotPlan{{start: t0.Add(time.Hour)}} // 10:00, jobs up to 60 min: [10:00, 11:00)
	got := moveTargets(domain.Availability{Slots: slots}, hammered, 60, t0.Add(10*time.Minute))
	var labels []string
	for _, s := range got {
		labels = append(labels, s.Format("15:04"))
	}
	// 09:00 is before the cutoff; 09:30–10:30 overlap [10:00, 11:00) with a 60-minute job; 13:00
	// is not available.
	if strings.Join(labels, " ") != "11:00 11:30 12:00 12:30 13:30" {
		t.Fatalf("moveTargets = %v", labels)
	}
}

// The scenario end to end against two fake instances sharing one store: the racers of each key
// are split across both, the fake books once per key and replays, both instances answer, and the
// same key with another start is 409.
func TestIdempotencyRaceAgainstFake(t *testing.T) {
	start := time.Now().Add(time.Hour).Truncate(30 * time.Minute).UTC()
	var mu sync.Mutex
	booked := map[string]uuid.UUID{} // key → id
	payloads := map[string]string{}  // key → startAt
	fake := func(instance string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Instance-ID", instance)
			switch r.URL.Path {
			case "/api/v1/availability/next":
				_ = json.NewEncoder(w).Encode(domain.NextAvailability{Slots: []domain.NextSlot{{Date: start.Format(time.DateOnly), Start: start}}})
			case "/api/v1/availability":
				var slots []domain.Slot
				for i := range 6 { // every key races at a start of its own
					slots = append(slots, slotAt(start.Add(time.Duration(i)*30*time.Minute), true))
				}
				_ = json.NewEncoder(w).Encode(domain.Availability{Slots: slots})
			case "/api/v1/appointments":
				var body struct {
					StartAt string `json:"startAt"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				key := r.Header.Get("Idempotency-Key")
				mu.Lock()
				defer mu.Unlock()
				id, ok := booked[key]
				switch {
				case ok && payloads[key] != body.StartAt:
					w.WriteHeader(http.StatusConflict)
				case ok:
					w.WriteHeader(http.StatusOK)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
				default:
					id = uuid.New()
					booked[key], payloads[key] = id, body.StartAt
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
				}
			default:
				http.NotFound(w, r)
			}
		}))
	}
	a, b := fake("backend-a"), fake("backend-b")
	defer a.Close()
	defer b.Close()
	tgt := idemTarget{dealer: uuid.New(), job: []uuid.UUID{uuid.New()}, vehicles: []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}}
	res, err := idempotencyRace(t.Context(), []*apiClient{newAPI(a.URL), newAPI(b.URL)}, nil, tgt, 3, 12, "stress:t", settings{maxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(booked) != 3 || len(res.ids) != 3 {
		t.Fatalf("booked %v, ids %v: want one appointment per key", booked, res.ids)
	}
	starts := map[string]bool{}
	for k, key := range res.keys {
		if booked[key] != res.ids[k] {
			t.Fatalf("key %s booked %s, race reported %s", key, booked[key], res.ids[k])
		}
		starts[payloads[key]] = true
	}
	if len(starts) != 3 {
		t.Fatalf("keys booked starts %v: each key must race at a start of its own", payloads)
	}
	if res.instances["backend-a"] == 0 || res.instances["backend-b"] == 0 {
		t.Fatalf("instances %v: both fakes must have answered", res.instances)
	}
}

// A race split across two backends that only one instance answered proves nothing about
// cross-instance idempotency.
func TestCrossInstanceNeedsBothInstances(t *testing.T) {
	if err := crossInstance(idemResult{crossed: true, instances: map[string]int{"a": 8, "b": 8}}); err != nil {
		t.Fatalf("both answered: %v", err)
	}
	if err := crossInstance(idemResult{crossed: true, instances: map[string]int{"a": 16}}); err == nil {
		t.Fatal("one instance answered every racer but the race passed")
	}
	if err := crossInstance(idemResult{crossed: true, instances: map[string]int{"a": 8, "": 8}}); err == nil {
		t.Fatal("answers without X-Instance-ID name no instance")
	}
	if err := crossInstance(idemResult{instances: map[string]int{"gw": 16}}); err != nil {
		t.Fatalf("through the gateway only: %v", err)
	}
}
