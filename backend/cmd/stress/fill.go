package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The fill phase is a day of real demand on the stress workshop, more than it can hold: customers
// with a job mix, a desired time and (some) waiting in the lounge arrive on an open-loop schedule
// (-fill-rate per second: a slow server shows up as latency rather than as a quietly lower arrival
// rate; the time a customer waited for the generator's in-flight cap is reported apart from the
// server's latency) through the gateway; each reads
// availability, books the offered start nearest its wish, and on losing that start to someone else
// (409/422) reads again, up to fillTries times; with nothing offered it is turned away. Then a
// top-up (runTopUp) keeps offering every job mix until a round finds nothing offered at all, so
// the day ends full by availability's account. Afterwards, from the tables (dayphases.go):
//
//   - no booking was refused while a start for it still existed: each 409/422 is judged against
//     the holds when it was decided, rebuilt from appointment_history (refusals.go), right after
//     the demand and before the top-up fills the day; refusals mean truly full (R2b); and the
//     oracle agrees the day is full for every job mix after the top-up;
//   - allocation quality (Q5): no job, waiter or not, sits on a technician more senior than a
//     qualified one that is still free for that window (best fit keeps senior staff for the work
//     only they can do), no waiter's general job took the quick lane while a general bay is still free for
//     it; the booked labour minutes stay within an offline capacity bound of the same demand and
//     close to the same demand replayed one customer at a time on a fresh day.

const fillTries = 5

// fillRequest is one customer of the day.
type fillRequest struct {
	mix     jobMix
	waiter  bool
	desired time.Time
	vehicle uuid.UUID
}

type fillOutcome int

const (
	fillBooked     fillOutcome = iota
	fillTurnedAway             // nothing offered: judged against the oracle
	fillGaveUp                 // lost an offered start fillTries times in a row (contention, not a refusal)
	fillUnresolved             // no definite answer (busy, transport): may have committed unheard
	fillError                  // a definite answer that is neither a booking, a refusal, nor busy: a bug
)

// fillStats is what one run of customers (the day, its top-up, or the sequential replay) measured.
type fillStats struct {
	mu                  sync.Mutex
	requests            int
	booked, turnedAway  int
	gaveUp              int
	unresolved, errored int
	waiterGeneral       int // waiters' general-bay jobs booked (soft upgrade possible)
	waiterExpress       int // of them, in an express bay (only allowed with no general bay free)
	expressJobs         int // express jobs booked (the express spill possible)
	expressSpilled      int // of them, in a general bay (only allowed with no express bay free)
	firstTry, retried   int
	staleConflicts      int
	offeredMinutes      int
	bookedMinutes       int
	ids                 []uuid.UUID
	availMS, bookMS     []float64 // each request from its actual send (the Q3 budgets)
	queueMS, arrivalMS  []float64 // open loop: wait before the first read; first read from the intended arrival
	unexpected          []string  // answers that are neither a booking, a refusal, nor busy
	distanceFromDesired []float64
	refusals            *refusalLog // every 409/422 and every "nothing offered", for the oracle
	turnedClasses       map[string]bool
}

func newFillStats(requests int) *fillStats {
	return &fillStats{requests: requests, refusals: newRefusalLog(), turnedClasses: map[string]bool{}}
}

// fillDemand draws n customers for day (reproducible for a seed): job mixes by weight, a quarter of
// them waiting, desired starts uniform over the opening hours.
func fillDemand(seed uint64, n int, w workshop, day booking.Date, mixes []jobMix, weights []int, vehicles []uuid.UUID) []fillRequest {
	rng := rand.New(rand.NewPCG(seed, uint64(n))) //nolint:gosec // reproducible demand, not security
	starts, _ := dayStarts(w, day, slotMinutes)
	total := 0
	for _, wt := range weights {
		total += wt
	}
	out := make([]fillRequest, n)
	for i := range out {
		pick, k := rng.IntN(total), 0
		for pick >= weights[k] {
			pick -= weights[k]
			k++
		}
		out[i] = fillRequest{
			mix: mixes[k], waiter: rng.IntN(4) == 0,
			desired: starts[rng.IntN(len(starts))].start, vehicle: vehicles[i%len(vehicles)], // the fleet holds one car per customer
		}
	}
	return out
}

// onDay moves the demand to another day of the same shop: the same customers, mixes, waiters and
// times of day (the sequential replay books the same demand on a fresh day).
func onDay(demand []fillRequest, w workshop, from, to booking.Date) []fillRequest {
	out := make([]fillRequest, len(demand))
	shift := to.Start(w.loc).Sub(from.Start(w.loc))
	for i, r := range demand {
		r.desired = r.desired.Add(shift)
		out[i] = r
	}
	return out
}

// nearestOffered picks the available start closest to desired (the earlier one on a tie).
func nearestOffered(slots []domain.Slot, desired time.Time) (domain.Slot, bool) {
	best, found := domain.Slot{}, false
	var bestGap time.Duration
	for _, s := range slots {
		if !s.Available {
			continue
		}
		gap := s.Start.Sub(desired)
		if gap < 0 {
			gap = -gap
		}
		if !found || gap < bestGap {
			best, bestGap, found = s, gap, true
		}
	}
	return best, found
}

// runFill sends demand through api. rate > 0: open loop, customer i is due i/rate seconds after
// the start and its first request's latency counts from then; at most workers customers are in
// flight (a due customer waits for one to finish, and that wait counts too). rate == 0: closed
// loop, workers customers at a time (workers 1 = one after another, the sequential replay).
func runFill(ctx context.Context, api *apiClient, w workshop, day booking.Date, demand []fillRequest, workers int, rate float64, keyPrefix string) *fillStats {
	st := newFillStats(len(demand))
	sem := make(chan struct{}, max(1, workers))
	var wg sync.WaitGroup
	t0 := time.Now()
	for i, r := range demand {
		st.offeredMinutes += r.mix.req.minutes
		var due time.Time
		if rate > 0 {
			due = intendedStart(t0, i, rate)
			if wait := time.Until(due); wait > 0 {
				select {
				case <-ctx.Done():
					wg.Wait()
					return st
				case <-time.After(wait):
				}
			}
		}
		select {
		case <-ctx.Done():
			wg.Wait() // the customers already sent finish before st is read
			return st
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			st.serve(ctx, api, w, day, r, fmt.Sprintf("%s:%d", keyPrefix, i), due)
		})
	}
	wg.Wait()
	return st
}

// serve plays one customer. origin, when set, is the intended arrival (open loop): every request's
// latency counts from its actual send (what the server is judged on), and the wait for a free
// in-flight slot before the first read, plus that read measured from the arrival, are kept
// apart (the customer's view, which includes the load generator's own queue); later requests
// follow the customer's previous answer at once.
func (st *fillStats) serve(ctx context.Context, api *apiClient, w workshop, day booking.Date, r fillRequest, key string, origin time.Time) {
	path := fmt.Sprintf("/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s", w.dealer.ID, r.vehicle, jobQuery(r.mix.ids), day)
	if r.waiter {
		path += "&isWaiter=true"
	}
	soft := softUpgrade(r.mix.req, r.waiter, int(w.dealer.Policy.ExpressMaxMinutes))
	var refused []refusal
	done := func(o fillOutcome, id uuid.UUID, try int, at time.Time) {
		for i := range refused {
			refused[i].exclude = id // the customer's own booking came after its refusals
		}
		st.refusals.add(refused...)
		st.finish(r, o, id, try, at)
	}
	for try := 1; try <= fillTries; try++ {
		t0 := time.Now()
		if try == 1 && !origin.IsZero() {
			// Open loop: how long the customer waited for an in-flight slot of the generator before
			// its first read went out (the client's queue, not the server's).
			st.observe(&st.queueMS, t0.Sub(origin))
		}
		resp, definite := sendUntilDefinite(ctx, api, http.MethodGet, path, nil, nil, probeAttempts)
		st.observe(&st.availMS, time.Since(t0))
		if try == 1 && !origin.IsZero() {
			st.observe(&st.arrivalMS, time.Since(origin))
		}
		var avail domain.Availability
		if definite && (resp.code != http.StatusOK || json.Unmarshal(resp.body, &avail) != nil || avail.Date == "") {
			st.unexpectedf("availability %d: %s", resp.code, truncate(string(resp.body), 100))
			done(fillError, uuid.Nil, try, time.Time{})
			return
		}
		if !definite {
			done(fillUnresolved, uuid.Nil, try, time.Time{})
			return
		}
		slot, ok := nearestOffered(avail.Slots, r.desired)
		if !ok {
			st.turnedClass(r)
			done(fillTurnedAway, uuid.Nil, try, time.Time{})
			return
		}
		payload := bookingPayload(w.dealer.ID, r.vehicle, r.mix.ids, slot.Start)
		payload["isWaiter"] = r.waiter
		t0 = time.Now()
		// One key per attempt: a different start is a different booking.
		resp, definite = sendUntilDefinite(ctx, api, http.MethodPost, "/api/v1/appointments", payload,
			map[string]string{"Idempotency-Key": fmt.Sprintf("%s:%d", key, try)}, probeAttempts)
		answered := time.Now()
		st.observe(&st.bookMS, answered.Sub(t0))
		switch {
		case resp.code == http.StatusCreated || resp.code == http.StatusOK:
			var appt domain.Appointment
			if err := json.Unmarshal(resp.body, &appt); err != nil || appt.ID == uuid.Nil {
				st.unexpectedf("%d with an unreadable body: %s", resp.code, truncate(string(resp.body), 100))
				done(fillError, uuid.Nil, try, time.Time{})
				return
			}
			st.mu.Lock()
			if r.waiter && appt.RequiredBayType == domain.BayGeneral {
				st.waiterGeneral++
				if appt.ServiceBayType == domain.BayExpress {
					st.waiterExpress++
				}
			}
			if appt.RequiredBayType == domain.BayExpress {
				st.expressJobs++
				if appt.ServiceBayType == domain.BayGeneral {
					st.expressSpilled++
				}
			}
			st.mu.Unlock()
			done(fillBooked, appt.ID, try, slot.Start)
			return
		case resp.code == http.StatusConflict || resp.code == http.StatusUnprocessableEntity:
			st.mu.Lock()
			st.staleConflicts++ // someone else took it between the read and the write
			st.mu.Unlock()
			refused = append(refused, refusal{mix: r.mix, soft: soft, start: slot.Start, vehicle: r.vehicle, sent: t0, at: answered})
		case !definite:
			done(fillUnresolved, uuid.Nil, try, time.Time{})
			return
		default:
			st.unexpectedf("%d: %s", resp.code, truncate(string(resp.body), 120))
			done(fillError, uuid.Nil, try, time.Time{})
			return
		}
	}
	done(fillGaveUp, uuid.Nil, fillTries, time.Time{})
}

func (st *fillStats) unexpectedf(format string, args ...any) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.unexpected) < 5 {
		st.unexpected = append(st.unexpected, fmt.Sprintf(format, args...))
	}
}

// fillClassKey names a (mix, waiter) class of customers.
func fillClassKey(mix jobMix, waiter bool) string {
	return fmt.Sprintf("%s|waiter=%v", mix.name, waiter)
}

func (st *fillStats) turnedClass(r fillRequest) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.turnedClasses[fillClassKey(r.mix, r.waiter)] = true
}

func (st *fillStats) observe(dst *[]float64, d time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	*dst = append(*dst, float64(d)/float64(time.Millisecond))
}

func (st *fillStats) finish(r fillRequest, o fillOutcome, id uuid.UUID, tries int, at time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch o {
	case fillBooked:
		st.booked++
		st.bookedMinutes += r.mix.req.minutes
		st.ids = append(st.ids, id)
		if tries == 1 {
			st.firstTry++
		} else {
			st.retried++
		}
		gap := at.Sub(r.desired).Minutes()
		if gap < 0 {
			gap = -gap
		}
		st.distanceFromDesired = append(st.distanceFromDesired, gap)
	case fillTurnedAway:
		st.turnedAway++
	case fillGaveUp:
		st.gaveUp++
	case fillError:
		st.errored++
	default:
		st.unresolved++
	}
}

// topUpMaxRounds bounds the top-up (each round offers every class once; a day fills in far fewer).
const topUpMaxRounds = 60

// runTopUp fills what the day's demand left: in rounds, one customer of every job mix, waiting and
// not, arrives at once (cars from vehicles, none reused); it stops after a round in which every
// customer was offered nothing at all — so the day is full by availability's account, and the
// caller asks the oracle whether it agrees. A round with a booking, a give-up (lost every offered
// start to the round's other customers), or an unresolved answer goes on.
func runTopUp(ctx context.Context, api *apiClient, w workshop, day booking.Date, mixes []jobMix, vehicles []uuid.UUID, keyPrefix string) (*fillStats, int, error) {
	st := newFillStats(0)
	next := 0
	for round := 1; round <= topUpMaxRounds; round++ {
		before := st.counts()
		var wg sync.WaitGroup
		for _, mix := range mixes {
			for _, waiter := range []bool{false, true} {
				if next >= len(vehicles) {
					wg.Wait()
					return st, round, fmt.Errorf("top-up ran out of fresh cars after %d rounds (fleet %d)", round, len(vehicles))
				}
				r := fillRequest{mix: mix, waiter: waiter, desired: day.Start(w.loc).Add(12 * time.Hour), vehicle: vehicles[next]}
				key := fmt.Sprintf("%s:%d", keyPrefix, next)
				next++
				st.mu.Lock()
				st.requests++
				st.offeredMinutes += mix.req.minutes
				st.mu.Unlock()
				wg.Go(func() { st.serve(ctx, api, w, day, r, key, time.Time{}) })
			}
		}
		wg.Wait()
		if ctx.Err() != nil {
			return st, round, ctx.Err()
		}
		after := st.counts()
		if after.turnedAway-before.turnedAway == 2*len(mixes) {
			return st, round, nil
		}
	}
	return st, topUpMaxRounds, fmt.Errorf("top-up still booking after %d rounds", topUpMaxRounds)
}

// fillCounts is a snapshot of the outcome counters.
type fillCounts struct{ booked, turnedAway, gaveUp, unresolved, errored int }

func (st *fillStats) counts() fillCounts {
	st.mu.Lock()
	defer st.mu.Unlock()
	return fillCounts{st.booked, st.turnedAway, st.gaveUp, st.unresolved, st.errored}
}

// heldSinceSQL is true when the resource (column col of an appointment row, = res) was held over
// appointment a's window at any time since $3 (the demand's start): by a hold now, by a position a
// hold had since (history rows at or after $3: bookings, reassigns, the engine's one-move
// repairs), or by an unchanged hold's position when $3 came (its latest history row before).
// A repair moves a whole appointment off a resource while the new job takes only its own slot, so
// "free right now" can be a resource that was busy when an earlier job was placed; judging against
// every position since keeps the quality checks to what a best-fit allocator could have seen. A
// second of slack before $3 only adds rows (conservative). History is reached through the
// dealership's appointments starting within a day of the job (the demand books, it never moves a
// job to another day), so a probe reads one day's rows, not the whole history table.
func heldSinceSQL(col, res string) string {
	return `(EXISTS (SELECT 1 FROM appointments x WHERE x.` + col + ` = ` + res + ` AND x.status IN (` + holdingSQL + `)
		              AND tstzrange(x.start_at, x.end_at, '[)') && tstzrange(a.start_at, a.end_at, '[)'))
		  OR EXISTS (SELECT 1 FROM appointments y JOIN appointment_history h ON h.appointment_id = y.id
		              WHERE y.dealership_id = a.dealership_id
		                AND y.start_at > a.start_at - interval '1 day' AND y.start_at < a.end_at + interval '1 day'
		                AND h.` + col + ` = ` + res + ` AND h.status IN (` + holdingSQL + `)
		                AND tstzrange(h.start_at, h.end_at, '[)') && tstzrange(a.start_at, a.end_at, '[)')
		              AND (h.at >= $3::timestamptz - interval '1 second'
		                   OR h.id = (SELECT max(h2.id) FROM appointment_history h2
		                              WHERE h2.appointment_id = h.appointment_id AND h2.at < $3::timestamptz - interval '1 second'))))`
}

// overQualifiedSQL: jobs of the run (waiters' too: they get the same best-fit order) on a
// technician more senior than another active, qualified, on-duty technician of the dealership who
// was free for the whole window throughout the demand (heldSinceSQL).
var overQualifiedSQL = `
	SELECT a.id::text || ' on ' || t.name || ' (level ' || t.skill_level || ') while ' || t2.name || ' (level ' || t2.skill_level || ') is free'
	FROM appointments a
	JOIN technicians t ON t.id = a.technician_id
	JOIN dealerships d ON d.id = a.dealership_id
	JOIN LATERAL (
		SELECT t2.name, t2.skill_level FROM technicians t2
		WHERE t2.dealership_id = a.dealership_id AND t2.active AND t2.id <> t.id
		  AND t2.skill_level < t.skill_level AND t2.skill_level >= a.min_skill_level
		  AND NOT EXISTS (SELECT unnest(a.required_skills) EXCEPT SELECT sk.skill FROM technician_skills sk WHERE sk.technician_id = t2.id)
		  AND ` + onDutySQL("t2", "d.timezone", "a.start_at", "a.end_at") + `
		  AND NOT ` + heldSinceSQL("technician_id", "t2.id") + `
		LIMIT 1) t2 ON true
	WHERE a.dealership_id = $1 AND a.idempotency_key LIKE $2 AND a.status IN (` + notStartedSQL + `)
	LIMIT 5`

// quickLaneMisuseSQL: waiters' general jobs of the run in an express bay while a general bay of the
// dealership was free for the whole window throughout the demand (heldSinceSQL).
var quickLaneMisuseSQL = `
	SELECT a.id::text || ' in express bay ' || b.name || ' while ' || g.name || ' is free'
	FROM appointments a
	JOIN service_bays b ON b.id = a.service_bay_id
	JOIN LATERAL (
		SELECT g.name FROM service_bays g
		WHERE g.dealership_id = a.dealership_id AND g.active AND g.bay_type = ` + bayGeneralSQL + `
		  AND NOT ` + heldSinceSQL("service_bay_id", "g.id") + `
		LIMIT 1) g ON true
	WHERE a.dealership_id = $1 AND a.idempotency_key LIKE $2 AND a.status IN (` + notStartedSQL + `)
	  AND a.is_waiter AND a.required_bay_type = ` + bayGeneralSQL + ` AND b.bay_type = ` + bayExpressSQL + `
	LIMIT 5`

// spillMisuseSQL: express jobs of the run in a general bay (the express spill) while an express
// bay of the dealership was free for the whole window throughout the demand (heldSinceSQL).
var spillMisuseSQL = `
	SELECT a.id::text || ' in general bay ' || b.name || ' while express bay ' || e.name || ' is free'
	FROM appointments a
	JOIN service_bays b ON b.id = a.service_bay_id
	JOIN LATERAL (
		SELECT e.name FROM service_bays e
		WHERE e.dealership_id = a.dealership_id AND e.active AND e.bay_type = ` + bayExpressSQL + `
		  AND NOT ` + heldSinceSQL("service_bay_id", "e.id") + `
		LIMIT 1) e ON true
	WHERE a.dealership_id = $1 AND a.idempotency_key LIKE $2 AND a.status IN (` + notStartedSQL + `)
	  AND a.required_bay_type = ` + bayExpressSQL + ` AND b.bay_type = ` + bayGeneralSQL + `
	LIMIT 5`

// bayUseSQL / techUseSQL: per bay type / per technician level, the resources and the minutes of
// holding appointments that day.
var bayUseSQL = `
	SELECT CASE b.bay_type WHEN ` + bayExpressSQL + ` THEN 'express' WHEN ` + bayGeneralSQL + ` THEN 'general' WHEN ` + bayDiagnosticSQL + ` THEN 'diagnostic' ELSE 'alignment' END,
	       count(DISTINCT b.id)::int,
	       COALESCE(sum(extract(epoch FROM a.end_at - a.start_at) / 60), 0)::int
	FROM service_bays b
	LEFT JOIN appointments a ON a.service_bay_id = b.id AND a.status IN (` + holdingSQL + `) AND a.start_at >= $2 AND a.start_at < $3
	WHERE b.dealership_id = $1 AND b.active
	GROUP BY b.bay_type ORDER BY b.bay_type`

var techUseSQL = `
	SELECT 'level ' || t.skill_level, count(DISTINCT t.id)::int,
	       COALESCE(sum(extract(epoch FROM a.end_at - a.start_at) / 60), 0)::int
	FROM technicians t
	LEFT JOIN appointments a ON a.technician_id = t.id AND a.status IN (` + holdingSQL + `) AND a.start_at >= $2 AND a.start_at < $3
	WHERE t.dealership_id = $1 AND t.active
	GROUP BY t.skill_level ORDER BY t.skill_level`

func utilisation(ctx context.Context, conn *pgx.Conn, sql string, w workshop, from, to time.Time) (map[string][2]int, error) {
	rows, err := conn.Query(ctx, sql, w.dealer.ID, from, to)
	if err != nil {
		return nil, err
	}
	out := map[string][2]int{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var label string
		var n, minutes int
		err := row.Scan(&label, &n, &minutes)
		out[label] = [2]int{minutes, n * w.openMinutes()}
		return struct{}{}, err
	})
	return out, err
}

// bookedMinutesSQL is the labour time the run's holding bookings under a key pattern hold.
var bookedMinutesSQL = `
	SELECT coalesce(sum(booked_duration_minutes), 0)::int FROM appointments
	WHERE idempotency_key LIKE $1 AND status IN (` + holdingSQL + `)`

// useSummary is "general 1440/2160 (66.7%) …" in label order, plus the total.
func useSummary(use map[string][2]int) string {
	labels := make([]string, 0, len(use))
	for l := range use {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels)+1)
	booked, capacity := 0, 0
	for _, l := range labels {
		u := use[l]
		booked += u[0]
		capacity += u[1]
		parts = append(parts, fmt.Sprintf("%s %d/%d min (%s)", l, u[0], u[1], pct(u[0], u[1])))
	}
	parts = append(parts, fmt.Sprintf("total %s", pct(booked, capacity)))
	return strings.Join(parts, ", ")
}
