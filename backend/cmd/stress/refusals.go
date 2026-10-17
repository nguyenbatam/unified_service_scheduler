package main

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Wrong refusals under contention. Every booking the fill, customers, and hammer phases send that
// is answered 409/422 is recorded with its job mix, start, and car. During those phases capacity
// only shrinks, so a start the oracle still finds feasible at the end — a fitting bay and a
// qualified, on-duty technician free for the whole window, staging room, the car free — was
// feasible when it was refused: a false refusal. The customer's own later booking is left out of
// the end state (it did not exist when the refusal happened), which only makes the check stricter.
// Two things give capacity back during a phase: the mixed phase (cancel, reschedule, reassign),
// and the engine's one-step repair (a refused booking may move one other confirmed hold to another
// fitting bay or technician in the same window, so a cell that hold left can be free at the end
// although it was taken when an earlier refusal happened). Both are undone the same way: the
// holds as they were are added back ("phantoms") — the mixed phase's own list, and every position
// a hold of the dealership had since the phase began, read from appointment_history (the position
// it held when the phase began, plus each one it was moved to). The oracle's state then holds at
// least everything that held when any refusal happened (a moved booking holds both its old and
// its new place: over-occupied, which can only hide a false refusal, never invent one).
//
// A refusal timed by its sender (sent, at) is judged instead against the holds as they were when it
// was decided, rebuilt from appointment_history (refusal_timeline.go): bookings made after it are
// left out, which the end state cannot do (the fill day's top-up, run after the demand, fills the
// day, so against the end state nearly every refusal looks justified). The phantoms above are
// still added (positions a hold had before the mixed phase moved it, possibly to another day).
//
// The calendar (hours, lunch, closures) is not re-judged: every recorded start was offered by
// availability for that job mix just before (fill, customers) or when the phase was prepared
// (hammer, which records only starts offered for the request's own mix).

// refusal is one booking answered 409/422.
type refusal struct {
	mix     jobMix
	soft    bool // a waiter's soft upgrade applied to the request
	start   time.Time
	vehicle uuid.UUID
	exclude uuid.UUID // the requester's own later booking (uuid.Nil: none)
	// sent and at are when the request went out and its 409/422 came back (client clock); zero:
	// judged against the end state only.
	sent, at time.Time
}

func (r refusal) window() timeWindow {
	return timeWindow{r.start, r.start.Add(time.Duration(r.mix.req.minutes) * time.Minute)}
}

func (r refusal) key() string {
	return fmt.Sprintf("%s|%v|%d|%s|%s|%d", r.mix.name, r.soft, r.start.Unix(), r.vehicle, r.exclude, r.sent.UnixNano())
}

// refusalLog collects refusals, one per distinct (mix, upgrade, start, car, own booking, send
// time); safe for concurrent use.
type refusalLog struct {
	mu    sync.Mutex
	items map[string]refusal
}

func newRefusalLog() *refusalLog { return &refusalLog{items: map[string]refusal{}} }

func (l *refusalLog) add(rs ...refusal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rs {
		l.items[r.key()] = r
	}
}

// list returns the refusals in a stable order.
func (l *refusalLog) list() []refusal {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]refusal, 0, len(l.items))
	for _, r := range l.items {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

func (l *refusalLog) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.items)
}

// refusalVerdict is what judgeRefusals found.
type refusalVerdict struct {
	recorded, judged, phantoms, falseRefusals int
	timed                                     int // of judged: against the holds when it was decided
	samples                                   []string
}

func (v refusalVerdict) String() string {
	if v.judged == 0 {
		return "no booking was refused, so there was no refusal to judge (vacuous on its own)"
	}
	against := "the end state"
	switch {
	case v.timed == v.judged:
		against = "the holds when each was decided (appointment_history, later bookings left out)"
	case v.timed > 0:
		against = fmt.Sprintf("the holds when each was decided (%d of them; the rest against the end state)", v.timed)
	}
	s := fmt.Sprintf("%d distinct refused bookings (409/422) judged against %s: %d false (the oracle still finds a fitting bay + qualified on-duty technician free for the whole window, staging room, and the car free)",
		v.judged, against, v.falseRefusals)
	if v.phantoms > 0 {
		s += fmt.Sprintf("; the %d holds the mixed phase changed counted as they were before", v.phantoms)
	}
	return s + sampleSuffix(v.samples)
}

// record writes the verdict under R2b for src: PASS/FAIL when a refusal was judged, SKIP when none
// happened (no evidence either way, so it must not make the row pass).
func (v refusalVerdict) record(m *matrix, src string) {
	if v.judged == 0 {
		m.skip("R2b", src, "%s", v)
		return
	}
	m.check("R2b", src, v.falseRefusals == 0, "%s", v)
}

// judgeRefusals loads dealer's end state around the refusals, adds phantoms (holds as they were
// before the mixed phase changed them, and every position a hold had since the phase began at
// since), and judges each refusal: a timed one against the holds when it was decided (the
// timeline, plus the mixed phase's phantoms), the others against that end state.
func judgeRefusals(ctx context.Context, q querier, dealer uuid.UUID, refusals []refusal, phantoms []oHold, since time.Time) (refusalVerdict, error) {
	v := refusalVerdict{recorded: len(refusals), phantoms: len(phantoms)}
	if len(refusals) == 0 {
		return v, nil
	}
	from, to := refusals[0].start, refusals[0].window().end
	vehicles := map[uuid.UUID]bool{}
	for _, r := range refusals {
		if r.start.Before(from) {
			from = r.start
		}
		if e := r.window().end; e.After(to) {
			to = e
		}
		vehicles[r.vehicle] = true
	}
	shop, err := loadShop(ctx, q, dealer, from.Add(-24*time.Hour), to.Add(24*time.Hour))
	if err != nil {
		return v, err
	}
	ids := make([]uuid.UUID, 0, len(vehicles))
	for id := range vehicles {
		ids = append(ids, id)
	}
	if err := shop.loadVehicleHolds(ctx, q, ids, from.Add(-24*time.Hour), to.Add(24*time.Hour)); err != nil {
		return v, err
	}
	mixed := slices.Clone(phantoms) // the mixed phase's own: also added to a timed refusal's state
	for i := range mixed {
		mixed[i].phantom = true
	}
	if !since.IsZero() {
		past, pinned, err := historyHolds(ctx, q, dealer, from.Add(-24*time.Hour), to.Add(24*time.Hour), since)
		if err != nil {
			return v, err
		}
		phantoms = append(slices.Clone(phantoms), past...)
		shop.pinned = pinned
	}
	phantoms = withoutLiveDuplicates(shop.holds, phantoms)
	for i := range phantoms {
		phantoms[i].phantom = true
	}
	endHolds := append(slices.Clone(shop.holds), phantoms...)
	for _, h := range phantoms {
		shop.vehicleHolds[h.vehicle] = append(shop.vehicleHolds[h.vehicle], h)
	}
	timedState, err := refusalStates(ctx, q, dealer, refusals, shop.holds, mixed, from.Add(-24*time.Hour), to.Add(24*time.Hour))
	if err != nil {
		return v, err
	}
	endNow := shop.now
	for _, r := range refusals {
		v.judged++
		shop.holds, shop.now = endHolds, endNow
		if state, timed := timedState(r); timed {
			v.timed++
			shop.holds, shop.now = state, r.at
		}
		if shop.refusalFeasible(r) {
			v.falseRefusals++
			if len(v.samples) < 5 {
				when := ""
				if !r.at.IsZero() {
					when = ", refused at " + r.at.Format("15:04:05.000")
				}
				v.samples = append(v.samples, fmt.Sprintf("%s at %s (car %s%s)", r.mix.name, r.start.In(shop.loc).Format("2006-01-02 15:04"), r.vehicle, when))
			}
		}
	}
	return v, nil
}

// refusalStates returns, for a timed refusal, the holds that may have been in its way when it was
// decided (false for an untimed one): the timeline's positions over its decision window, the mixed
// phase's phantoms, and the live holds of appointments the timeline does not cover (no history in its
// range) or that overrunHold widened past their recorded positions: counted as held throughout,
// conservative. It reads nothing when no refusal is timed.
func refusalStates(ctx context.Context, q querier, dealer uuid.UUID, refusals []refusal, live, phantoms []oHold, from, to time.Time) (func(refusal) ([]oHold, bool), error) {
	if !slices.ContainsFunc(refusals, func(r refusal) bool { return !r.at.IsZero() }) {
		return func(refusal) ([]oHold, bool) { return nil, false }, nil
	}
	clock, err := measurePGClock(ctx, q)
	if err != nil {
		return nil, err
	}
	tl, err := loadTimeline(ctx, q, dealer, from, to)
	if err != nil {
		return nil, err
	}
	always := alwaysHeld(tl, live, phantoms)
	return func(r refusal) ([]oHold, bool) {
		if r.at.IsZero() {
			return nil, false
		}
		return append(tl.between(clock.decisionWindow(r)), always...), true
	}, nil
}

// alwaysHeld are the holds counted in every timed refusal's state: live holds the timeline does
// not cover (no history in its range), live flagged overruns (overrunHold reaches past every raw
// position history has), and the phantoms.
func alwaysHeld(tl holdTimeline, live, phantoms []oHold) []oHold {
	var always []oHold
	for _, h := range live {
		if _, covered := tl[h.id]; !covered || h.overrun {
			always = append(always, h)
		}
	}
	return append(always, phantoms...)
}

// withoutLiveDuplicates drops the phantoms that are exactly a live hold's position (same
// appointment, bay, technician, window): historyHolds returns every unchanged appointment's
// position before the phase and every booking's creation, and a copy next to the hold itself
// would count its arrival twice (staging) and make its resource look doubly held (repair).
func withoutLiveDuplicates(live, phantoms []oHold) []oHold {
	type pos struct {
		id, bay, tech uuid.UUID
		start, end    int64
	}
	key := func(h oHold) pos {
		return pos{h.id, h.bay, h.tech, h.window.start.UnixNano(), h.window.end.UnixNano()}
	}
	seen := make(map[pos]bool, len(live))
	for _, h := range live {
		seen[key(h)] = true
	}
	out := make([]oHold, 0, len(phantoms))
	for _, h := range phantoms {
		if k := key(h); !seen[k] {
			seen[k] = true // two identical phantoms are one position too
			out = append(out, h)
		}
	}
	return out
}

// refusalFeasible reports whether r could still be booked in the shop's current state (with the
// requester's own later booking left out).
func (s *oracleShop) refusalFeasible(r refusal) bool {
	w := r.window()
	bays, techs := s.counts(r.mix.req, r.soft, w, r.exclude)
	return s.fits(r.mix.req, r.soft, w, r.exclude, bays, techs) && s.stagingRoom(r.start, r.exclude) && s.vehicleFree(r.vehicle, w, r.exclude)
}

// historyHolds are the holding positions (bay, technician, car, window) overlapping [from, to)
// that dealer's appointments starting there (now; a day of slack before from) had since: each one's position when since came (its latest history row
// before it) and every position it was given after (history rows at or after since: bookings,
// moves, reassigns — the engine's repairs included). The ids are the appointments' own, so a
// refusal's exclude (the requester's later booking) leaves out its history too. A second of
// slack before since only adds rows (conservative). A hold moved to another day entirely is
// found only through the mixed phase's own phantoms (repairs never change a window).
//
// pinned are the appointments changed during the phase (any history row since other than their
// creation): the repair rule never counts them as movable, since a booking that met one locked by
// its writer skips it (store: SKIP LOCKED) — a refusal then is not false.
func historyHolds(ctx context.Context, q querier, dealer uuid.UUID, from, to, since time.Time) ([]oHold, map[uuid.UUID]bool, error) {
	pinned := map[uuid.UUID]bool{}
	rows, err := q.Query(ctx, `
		SELECT DISTINCT h.appointment_id
		FROM appointments a JOIN appointment_history h ON h.appointment_id = a.id
		WHERE a.dealership_id = $1 AND a.start_at >= $2::timestamptz - interval '1 day' AND a.start_at < $3
		  AND h.at >= $4 AND h.event_type <> `+sqlCode(domain.EventAppointmentCreated), dealer, from, to, since.Add(-time.Second))
	if err != nil {
		return nil, nil, fmt.Errorf("oracle: changed holds: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, nil, fmt.Errorf("oracle: changed holds: %w", err)
	}
	for _, id := range ids {
		pinned[id] = true
	}
	rows, err = q.Query(ctx, `
		SELECT h.appointment_id, h.service_bay_id, h.technician_id, a.vehicle_id, h.status, h.start_at, h.end_at
		FROM appointments a JOIN appointment_history h ON h.appointment_id = a.id
		WHERE a.dealership_id = $1 AND a.start_at >= $2::timestamptz - interval '1 day' AND a.start_at < $3
		  AND h.status IN (`+holdingSQL+`) AND h.start_at < $3 AND $2 < h.end_at
		  AND (h.at >= $4 OR h.id = (SELECT max(h2.id) FROM appointment_history h2
		                             WHERE h2.appointment_id = h.appointment_id AND h2.at < $4))`,
		dealer, from, to, since.Add(-time.Second))
	if err != nil {
		return nil, nil, fmt.Errorf("oracle: history holds: %w", err)
	}
	holds, err := pgx.CollectRows(rows, scanHold)
	if err != nil {
		return nil, nil, fmt.Errorf("oracle: history holds: %w", err)
	}
	return holds, pinned, nil
}
