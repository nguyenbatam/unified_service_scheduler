package main

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestMixOutcome(t *testing.T) {
	cases := []struct {
		code  int
		stale bool
		label string
		ok    bool
	}{
		{200, false, "ok", true},
		{409, false, "conflict", true},
		{422, false, "unavailable", true},
		{503, false, "busy", true},
		{0, false, "busy", true},
		{429, false, "busy", true},
		{504, false, "busy", true},
		{412, false, "412", false}, // the version was read just before: nothing else writes
		{404, false, "404", false},
		{500, false, "500", false},
		{412, true, "stale_412", true},
		{200, true, "stale_200", false},
		{409, true, "stale_409", false},
	}
	for _, tc := range cases {
		label, ok := mixOutcome(tc.code, tc.stale)
		if label != tc.label || ok != tc.ok {
			t.Errorf("mixOutcome(%d, stale=%v) = %q, %v; want %q, %v", tc.code, tc.stale, label, ok, tc.label, tc.ok)
		}
	}
}

func TestSlotQuota(t *testing.T) {
	cases := []struct{ ops, slots, want int }{{10, 5, 2}, {11, 5, 3}, {1, 5, 1}, {0, 5, 0}, {3, 0, 0}}
	for _, tc := range cases {
		if got := slotQuota(tc.ops, tc.slots); got != tc.want {
			t.Errorf("slotQuota(%d, %d) = %d, want %d", tc.ops, tc.slots, got, tc.want)
		}
	}
}

// fakeAppointments serves GET /appointments/:id and the four mutations the mixed phase sends,
// with the server's order of checks: already there → replay, then If-Match, then the state.
// lax skips the If-Match check (a server bug the phase must catch).
type fakeAppointments struct {
	mu   sync.Mutex
	rows map[uuid.UUID]*domain.Appointment
	lax  bool
	// book, when set, lets POST /appointments create a booking like it at the requested start (the
	// burst's own booking).
	book *domain.Appointment
}

func (f *fakeAppointments) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v1/appointments" && r.Method == http.MethodPost && f.book != nil {
		var body struct {
			StartAt time.Time `json:"startAt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		a := *f.book
		a.ID, a.Version, a.Status, a.StartAt = uuid.New(), 1, domain.StatusConfirmed, body.StartAt
		f.rows[a.ID] = &a
		w.WriteHeader(http.StatusCreated)
		f.reply(w, &a)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/appointments/"), "/")
	id, err := uuid.Parse(parts[0])
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.rows[id]
	if err != nil || a == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodGet {
		f.reply(w, a)
		return
	}
	var body struct {
		TechnicianID uuid.UUID `json:"technicianId"`
		ServiceBayID uuid.UUID `json:"serviceBayId"`
		StartAt      time.Time `json:"startAt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	match, _ := strconv.Atoi(strings.Trim(r.Header.Get("If-Match"), `"`))
	var there bool
	var apply func()
	switch parts[1] {
	case "cancel":
		there, apply = a.Status == domain.StatusCancelled, func() { a.Status = domain.StatusCancelled }
	case "check-in":
		there, apply = a.Status == domain.StatusCheckedIn, func() { a.Status = domain.StatusCheckedIn }
	case "reassign":
		tech, bay := a.TechnicianID, a.ServiceBayID
		if body.TechnicianID != uuid.Nil {
			tech = body.TechnicianID
		}
		if body.ServiceBayID != uuid.Nil {
			bay = body.ServiceBayID
		}
		there, apply = tech == a.TechnicianID && bay == a.ServiceBayID, func() { a.TechnicianID, a.ServiceBayID = tech, bay }
	case "reschedule":
		there, apply = a.StartAt.Equal(body.StartAt), func() { a.StartAt = body.StartAt }
	}
	switch {
	case there:
	case match != 0 && match != a.Version && !f.lax: // no If-Match: unconditional, like the server
		w.WriteHeader(http.StatusPreconditionFailed)
		return
	default:
		apply()
		a.Version++
	}
	f.reply(w, a)
}

func (f *fakeAppointments) reply(w http.ResponseWriter, a *domain.Appointment) {
	if err := json.NewEncoder(w).Encode(a); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func mixedPlan(f *fakeAppointments) *plan {
	start := time.Date(2030, 1, 7, 10, 0, 0, 0, time.UTC)
	techs := []domain.Technician{{ID: uuid.New()}, {ID: uuid.New()}}
	bays := []domain.Bay{{ID: uuid.New(), BayType: domain.BayGeneral}, {ID: uuid.New(), BayType: domain.BayGeneral}}
	p := &plan{
		slots:       []slotPlan{{start: start, maxWinners: 3}},
		technicians: techs, bays: bays,
		moveTargets: []time.Time{start.Add(3 * time.Hour)},
		created:     map[int64]map[uuid.UUID]bool{start.Unix(): {}},
		mutated:     map[uuid.UUID]bool{},
		slotDone:    make(chan int, 1),
	}
	for range 3 {
		id := uuid.New()
		f.rows[id] = &domain.Appointment{ID: id, Version: 1, Status: domain.StatusConfirmed, StartAt: start,
			TechnicianID: techs[0].ID, ServiceBayID: bays[0].ID, ServiceBayType: domain.BayGeneral}
		p.created[start.Unix()][id] = true
	}
	return p
}

// Each op applies with the read version, its replay with the stale version answers 200 unchanged,
// and a different change with the stale version is 412; every touched booking is marked.
func TestMixPhaseProbesStaleVersions(t *testing.T) {
	f := &fakeAppointments{rows: map[uuid.UUID]*domain.Appointment{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := mixedPlan(f)
	stats := newMixStats()
	p.slotDone <- 0
	close(p.slotDone)
	mixPhase(t.Context(), newAPI(srv.URL), p, 3, stats, uuid.New(), "test:burst")
	if err := stats.err(); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"reassign", "reschedule", "cancel"} {
		for _, label := range []string{"ok", "replay_200", "stale_412"} {
			if stats.counts[op+"/"+label] != 1 {
				t.Fatalf("counts %s, want one %s/%s", stats, op, label)
			}
		}
	}
	if len(p.mutated) != 3 {
		t.Fatalf("mutated %d bookings, want 3", len(p.mutated))
	}
	statuses := map[domain.AppointmentStatus]int{}
	for _, a := range f.rows {
		statuses[a.Status]++
		if a.Version != 2 {
			t.Fatalf("appointment %s at version %d, want 2 (one change each)", a.ID, a.Version)
		}
	}
	if statuses[domain.StatusCancelled] != 1 {
		t.Fatalf("statuses %v, want one cancelled", statuses)
	}
}

func TestMixPhaseCatchesIgnoredIfMatch(t *testing.T) {
	f := &fakeAppointments{rows: map[uuid.UUID]*domain.Appointment{}, lax: true}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := mixedPlan(f)
	stats := newMixStats()
	p.slotDone <- 0
	close(p.slotDone)
	mixPhase(t.Context(), newAPI(srv.URL), p, 3, stats, uuid.New(), "test:burst")
	if err := stats.err(); err == nil || !strings.Contains(err.Error(), "stale_200") {
		t.Fatalf("err = %v, want the stale 200s reported", err)
	}
}

// flakyAppointments sits in front of fakeAppointments and answers some calls of the mixed phase
// busy (503, Retry-After: 0) before the fake sees them: busy[kind] times per appointment (-1 =
// forever), then wrong[kind] as a definite answer if set, else the fake's own. A call's kind is
// read (GET), mutate (the first POST that got through), replay (that POST again) or stale (any
// other POST).
type flakyAppointments struct {
	next  http.Handler
	busy  map[string]int
	wrong map[string]int

	mu      sync.Mutex
	left    map[uuid.UUID]map[string]int
	mutated map[uuid.UUID]string // the mutation that got through: path + If-Match + body
}

func (f *flakyAppointments) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/appointments/"), "/")[0])
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	call := r.URL.Path + "|" + r.Header.Get("If-Match") + "|" + string(raw)
	f.mu.Lock()
	kind := "read"
	if r.Method == http.MethodPost {
		switch first, ok := f.mutated[id]; {
		case !ok:
			kind = "mutate"
		case first == call:
			kind = "replay"
		default:
			kind = "stale"
		}
	}
	if f.left[id] == nil {
		f.left[id] = maps.Clone(f.busy)
	}
	busy := f.left[id][kind] != 0
	if f.left[id][kind] > 0 {
		f.left[id][kind]--
	}
	if !busy && kind == "mutate" {
		f.mutated[id] = call
	}
	f.mu.Unlock()
	switch code := f.wrong[kind]; {
	case busy:
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	case code != 0:
		w.WriteHeader(code)
	default:
		f.next.ServeHTTP(w, r)
	}
}

// runFlakyMix runs the mixed phase for the three ops against a flaky fake.
func runFlakyMix(t *testing.T, busy, wrong map[string]int) (*mixStats, *plan) {
	t.Helper()
	f := &fakeAppointments{rows: map[uuid.UUID]*domain.Appointment{}}
	srv := httptest.NewServer(&flakyAppointments{next: f, busy: busy, wrong: wrong,
		left: map[uuid.UUID]map[string]int{}, mutated: map[uuid.UUID]string{}})
	t.Cleanup(srv.Close)
	p := mixedPlan(f)
	stats := newMixStats()
	p.slotDone <- 0
	close(p.slotDone)
	mixPhase(t.Context(), newAPI(srv.URL), p, 3, stats, uuid.New(), "test:burst")
	return stats, p
}

// A busy answer to the read or a probe is resent, and the answer it settles on is judged as usual.
func TestMixPhaseRetriesBusyReadAndProbes(t *testing.T) {
	t.Parallel()
	stats, p := runFlakyMix(t, map[string]int{"read": 1, "replay": 1, "stale": 2}, nil)
	if err := stats.err(); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"reassign", "reschedule", "cancel"} {
		for _, label := range []string{"ok", "replay_200", "stale_412"} {
			if stats.counts[op+"/"+label] != 1 {
				t.Fatalf("counts %s, want one %s/%s", stats, op, label)
			}
		}
	}
	if len(p.mutated) != 3 {
		t.Fatalf("mutated %d bookings, want 3", len(p.mutated))
	}
}

// Calls that stay busy through every retry count as busy, never as failures.
func TestMixPhaseCountsLastingBusyAsBusy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		busy   map[string]int
		labels []string
	}{
		{"read", map[string]int{"read": -1}, []string{"read_busy"}},
		{"probes", map[string]int{"replay": -1, "stale": -1}, []string{"ok", "replay_busy", "stale_busy"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stats, _ := runFlakyMix(t, tc.busy, nil)
			if err := stats.err(); err != nil {
				t.Fatal(err)
			}
			for _, op := range []string{"reassign", "reschedule", "cancel"} {
				for _, label := range tc.labels {
					if stats.counts[op+"/"+label] != 1 {
						t.Fatalf("counts %s, want one %s/%s", stats, op, label)
					}
				}
			}
		})
	}
}

// After a busy answer, a wrong definite one still fails the phase.
func TestMixPhaseFailsWrongAnswerAfterBusy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		wrong map[string]int
		want  string
	}{
		{"read", map[string]int{"read": http.StatusInternalServerError}, "read_500"},
		{"replay", map[string]int{"replay": http.StatusConflict}, "replay_409"},
		{"stale", map[string]int{"stale": http.StatusOK}, "stale_200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stats, _ := runFlakyMix(t, map[string]int{"read": 1, "replay": 1, "stale": 1}, tc.wrong)
			if err := stats.err(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %s reported", err, tc.want)
			}
		})
	}
}

// hammer reports a slot on slotDone only once every request of it has answered, and closes the
// channel when it returns.
func TestHammerSignalsFinishedSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New()})
	}))
	defer srv.Close()
	t0 := time.Date(2030, 1, 7, 10, 0, 0, 0, time.UTC)
	p := &plan{
		name: "t", jobs: [][]uuid.UUID{{uuid.New()}},
		slots:   []slotPlan{{start: t0}, {start: t0.Add(time.Hour)}, {start: t0.Add(2 * time.Hour)}},
		created: map[int64]map[uuid.UUID]bool{}, unresolved: map[int64]int{}, mutated: map[uuid.UUID]bool{},
		slotDone: make(chan int, 3),
	}
	const perSlot = 4
	var seen []int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for si := range p.slotDone {
			if got := len(p.slotIDs(si)); got != perSlot {
				t.Errorf("slot %d signalled with %d of %d answers", si, got, perSlot)
			}
			seen = append(seen, si)
		}
	}()
	s := settings{concurrency: perSlot, workers: 3, maxAttempts: 1}
	hammer(t.Context(), newAPI(srv.URL), p, []uuid.UUID{uuid.New(), uuid.New()}, s, "stress:t", newRunStats())
	<-done
	slices.Sort(seen)
	if !slices.Equal(seen, []int{0, 1, 2}) {
		t.Fatalf("signalled slots %v, want 0 1 2", seen)
	}
}

// takenTech answers 409 to a reassign onto one technician (it is busy in that window) and passes
// everything else to the fake.
type takenTech struct {
	next  http.Handler
	taken uuid.UUID
}

func (h takenTech) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/reassign") {
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if bytes.Contains(raw, []byte(h.taken.String())) {
			w.WriteHeader(http.StatusConflict)
			return
		}
	}
	h.next.ServeHTTP(w, r)
}

// With few ops over several slots, the first slot still gets cancel → reschedule → reassign (a
// small -mixed-ops must apply every operation, and the cancel frees room for the reassign), and a
// reassign whose first target is taken tries the next one with the same version.
func TestMixPhaseSmallOpsCoverEveryOperation(t *testing.T) {
	f := &fakeAppointments{rows: map[uuid.UUID]*domain.Appointment{}}
	p := mixedPlan(f)
	p.technicians = append(p.technicians, domain.Technician{ID: uuid.New()})
	start := p.slots[0].start
	p.slots = append(p.slots, slotPlan{start: start.Add(time.Hour)}, slotPlan{start: start.Add(2 * time.Hour)})
	// Room for the burst and the If-Match race (mixburst.go): a booking of its own at a move target,
	// a second bay of its type, and two more free starts.
	p.moveTargets = []time.Time{start.Add(3 * time.Hour), start.Add(4 * time.Hour), start.Add(5 * time.Hour)}
	p.jobs = [][]uuid.UUID{{uuid.New()}}
	f.book = &domain.Appointment{TechnicianID: p.technicians[0].ID, ServiceBayID: p.bays[0].ID, ServiceBayType: domain.BayGeneral}
	// The reassign is the third op (n = 2): its first candidate is the first technician other than
	// the booking's own.
	srv := httptest.NewServer(takenTech{next: f, taken: p.technicians[1].ID})
	defer srv.Close()
	p.slotDone = make(chan int, 3)
	for si := range 3 {
		p.slotDone <- si
	}
	close(p.slotDone)
	stats := newMixStats()
	mixPhase(t.Context(), newAPI(srv.URL), p, 3, stats, uuid.New(), "test:burst")
	if err := stats.err(); err != nil {
		t.Fatal(err)
	}
	if err := stats.vacuity(); err != nil {
		t.Fatalf("%v (%s)", err, stats)
	}
	if stats.counts["reassign/conflict"] != 1 || stats.counts["reassign/ok"] != 1 {
		t.Fatalf("counts %s: want the taken target refused, then the next one applied", stats)
	}
	if stats.counts["burst/versions"] < 2 || stats.counts["race/one_winner"] != 1 || len(p.extra) != 1 {
		t.Fatalf("counts %s, extra %v: want the burst's versions, one won race, and its booking kept", stats, p.extra)
	}
}
