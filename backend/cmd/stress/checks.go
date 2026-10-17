package main

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// recordHammer judges the hammer phases: unexpected answers, per-slot capacity, the board, the
// refusals against the holds when each was decided, the mixed phase, and the hammer's latency budget.
func (ss *session) recordHammer(ctx context.Context, plans []*plan, stats *runStats, mix *mixStats, created map[uuid.UUID]bool, mixes map[string]bool) {
	m, s := ss.m, ss.s
	if len(plans) == 0 {
		return
	}
	errs := stats.errorsSample()
	m.check("Q1", "hammer", len(errs) == 0, "no 5xx or malformed answer among %d booking attempts%s", stats.attemptCount(), sampleSuffix(errs))
	if s.mixedOps > 0 {
		if err := mix.err(); err != nil {
			m.fail("Q1", "mixed", "%v", err)
		} else if err := mix.vacuity(); err != nil {
			m.fail("Q1", "mixed", "%v (%s)", err, mix)
		} else {
			m.pass("Q1", "mixed", "reassign / reschedule / cancel with If-Match during the hammer, each applied at least once; stale versions get 412; a burst of back-to-back reassigns of one booking and concurrent reschedules on one If-Match version (one winner, the rest 412): %s", mix)
		}
	}
	for _, p := range plans {
		ids, notes, err := p.check(ctx, ss.api, ss.slotCounter(p.name))
		for id := range ids {
			created[id] = true
		}
		p.mu.Lock()
		for _, id := range p.extra { // the burst's booking: WS coverage, reconciliation, integrity
			created[id] = true
		}
		p.mu.Unlock()
		for _, job := range p.jobs {
			mixes[mixKey(job)] = true
		}
		total := 0
		for _, sl := range p.slots {
			total += sl.maxWinners
		}
		detail := fmt.Sprintf("%d requests for %d contended start(s) (vehicle + service + dealership + start each) → %d bookings, capacity %d", p.requests, len(p.slots), len(ids), total)
		if p.exact {
			detail += " (exact: every slot filled to its capacity, never beyond)"
		} else {
			detail += " (overlapping slots + staging cap: upper bounds per slot and window here; lower bounds from the refusal oracle below)"
		}
		if len(notes) > 0 {
			detail += fmt.Sprintf("; notes: %v", notes)
		}
		m.check("Q1", "hammer "+p.name, err == nil, "%s%s", detail, errSuffix(err))
		if p.exact {
			m.headline("Q1", "%s: %d requests → %d bookings, capacity %d (exact)", p.name, p.requests, len(ids), total)
		}
		m.check("R1", "hammer "+p.name, err == nil && len(ids) > 0, "%d bookings created at %s", len(ids), p.dealer.Name)
		ss.recordHammerRefusals(ctx, p)
	}
	all := stats.bookingLatency()
	if s.chaosRestart {
		// Requests cut off by the kill retry until the backend is back: their latency is the restart's.
		m.skip("Q3", "hammer", "-chaos-restart: a backend was killed mid-hammer, so end-to-end latency includes its restart — %s", all)
		return
	}
	m.headline("Q3", "hammer p99 %.0f ms (budget %.0f)", all.p99, s.sloHammerMS)
	m.check("Q3", "hammer", all.n > 0 && all.p99 <= s.sloHammerMS,
		"hammered bookings end to end (first send to definite answer, retries and Retry-After included): p99 %.0f ms (budget %.0f ms) — %s",
		all.p99, s.sloHammerMS, all)
}

// recordHammerRefusals judges every refusal of p against the holds when it was decided: a slot that
// turned a request away must have been saturated for that job then — no fitting bay and qualified on-duty technician
// free for the whole window, its staging window full, or the car busy — or the refusal was
// false. For north (overlapping slots under a staging cap) this is the per-slot lower bound the
// capacity check cannot give: every slot that refused anyone is full for what it refused.
func (ss *session) recordHammerRefusals(ctx context.Context, p *plan) {
	m := ss.m
	src := "hammer " + p.name
	if ss.conn == nil {
		m.skip("R2b", src, "STRESS_DATABASE_URL not set: the refusal oracle reads Postgres")
		return
	}
	v, err := judgeRefusals(ctx, ss.conn, p.dealer.ID, p.refusals.list(), p.phantomHolds(), p.prepared)
	if err != nil {
		m.fail("R2b", src, "refusal oracle: %v", err)
		return
	}
	v.record(m, src)
}

// recordPool judges one hammer phase's connection-pool use (pool.go).
func (ss *session) recordPool(ctx context.Context, phase string, before []promSample, beforeErr error, samples poolSamples) {
	m, s := ss.m, ss.s
	src := "pool " + phase
	after, err := clusterScrape(ctx, s.backendA, s.backendB)
	if beforeErr != nil || err != nil {
		if s.chaosRestart {
			m.skip("Q3", src, "-chaos-restart: a backend was down for a scrape around the phase (%v)", firstErr(beforeErr, err))
			return
		}
		m.fail("Q3", src, "scrape around the phase: %v", firstErr(beforeErr, err))
		return
	}
	v := judgePool(before, after, samples, s.poolWaitBudgetMS, s.poolCapFails)
	if v.unknown && s.chaosRestart {
		m.skip("Q3", src, "-chaos-restart: a backend restarted during the phase, its pool counters reset: %s", v.detail)
		return
	}
	m.check("Q3", src, v.ok, "admission gate holds the pool: %s", v.detail)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// recordSide judges the dispatcher reads made during the hammer: they stay 2xx and fast.
func (ss *session) recordSide(side *sideStats) {
	m, s := ss.m, ss.s
	lat := side.latency()
	ok, fiveXX, transport, bad := side.ok.Load(), side.fiveXX.Load(), side.transport.Load(), side.bad.Load()
	detail := fmt.Sprintf("reads during the hammer: ok=%d 5xx=%d transport=%d bad=%d, latency (ms) %s", ok, fiveXX, transport, bad, lat)
	if s.chaosRestart {
		// A backend killed mid-hammer drops the reads in flight on it (502 from the gateway or a
		// reset); wrong bodies are still bugs.
		m.check("Q4", "side", bad == 0 && ok > 0, "%s (-chaos-restart: 5xx/transport errors while a backend was down are tolerated)", detail)
		return
	}
	m.check("Q4", "side", fiveXX == 0 && transport == 0 && bad == 0 && ok > 0, "%s", detail)
	m.check("Q3", "side", lat.n > 0 && lat.p99 <= s.sloSideMS, "dispatcher reads during the hammer: p99 %.0f ms (budget %.0f ms)", lat.p99, s.sloSideMS)
}

// recordWS judges the live board fan-out: coverage of every created booking and every event, and
// each listener's arrival order.
func (ss *session) recordWS(ctx context.Context, ws *wsWatch, created map[uuid.UUID]bool) {
	m, s := ss.m, ss.s
	if s.wsClients == 0 {
		m.skip("Q4", "ws", "WS_CLIENTS=0 — fan-out check skipped")
		return
	}
	if ss.conn == nil {
		// Without Postgres the run's events cannot be listed: judge the creations clients heard.
		gaps := ws.waitCovered(created, 15*time.Second)
		m.check("Q4", "ws", len(gaps) == 0, "every listener on both backends got APPOINTMENT_CREATED for all %d bookings (%s)%s",
			len(created), ws.summary(created), gapSuffix(gaps))
	} else if want, err := runEvents(ctx, ss.conn, ss.runID); err != nil {
		m.fail("Q4", "ws", "list the run's events: %v", err)
	} else {
		// Every event of the run's appointments (the outbox rows, so creations whose answer was
		// lost too) includes each created booking's APPOINTMENT_CREATED (version 1).
		gaps := ws.waitEvents(want, 15*time.Second)
		m.check("Q4", "ws", len(gaps) == 0, "every appointment event of the run (%d: created, moved, reassigned, cancelled, …) reached every listener on both backends (%s)%s",
			len(want), ws.summary(created), gapSuffix(gaps))
		m.headline("Q4", "%d events × %d listeners on both backends, %d gaps", len(want), len(ws.listenersA)+len(ws.listenersB), len(gaps))
	}
	if n := ws.evictions(); n > 0 {
		m.info("Q4", "ws evictions", "%d listener session(s) closed by the hub as a slow consumer (close %d, send buffer full: by design, the client reconnects and resyncs); those listeners were excused from the events they missed while away, their backend's listeners together still had to cover every event", n, hubCloseSlowConsumer)
	}
	events, dups, reordered, changed, samples := ws.orderCheck()
	switch {
	case reordered > 0:
		m.fail("Q4", "ws order", "%d appointment notices across %d listeners: %d arrived after a newer version of the same appointment%s",
			events, len(ws.listenersA)+len(ws.listenersB), reordered, sampleSuffix(samples))
	case changed == 0:
		// Only CREATED (v1) notices were heard while listening (cleanup's cancels come after this
		// check): with one version per appointment there is no order to get wrong.
		m.skip("Q4", "ws order", "no listener saw any appointment at two versions (%d notices, all first versions): order not judged — run with -mixed-ops > 0", events)
	default:
		m.pass("Q4", "ws order", "%d appointment notices across %d listeners, up to %d appointments seen changing on one listener: per appointment, versions never went back (0 reordered); %d duplicate deliveries (at-least-once, reported only)",
			events, len(ws.listenersA)+len(ws.listenersB), changed, dups)
	}
	m.metric("wsOrder", map[string]int{"events": events, "duplicates": dups, "reordered": reordered})
}

// afterRestart (-chaos-restart) waits for both backends, then replays, with its own key and
// payload, every hammer request the kill cut off — any request one of whose attempts lost its
// answer on the way (transport error, 502/504):
//
//   - left unresolved (every retry failed while the backend was down): it must now get a definite
//     answer — 200 (it had committed unheard), 201 (it had not; booked now) or 409/422 (it had
//     not; the slot is full);
//   - resolved on a retry as created: the replay must answer 200 with the same appointment;
//   - resolved on a retry as refused: the key must not have committed (200 here would mean a
//     refusal was answered for a booking that exists); 201/409/422 are consistent.
//
// Requests left unresolved by anything else (busy) are replayed the same way, and so is a sample of
// the created bookings (200, same id). At least one request must have been cut off: a kill that hit
// nothing in flight proves nothing about lost answers, so the check fails instead of passing
// vacuously. Afterwards no request is left unresolved and the row reconciliation is exact.
func (ss *session) afterRestart(ctx context.Context, plans []*plan, stats *runStats) error {
	m, s := ss.m, ss.s
	if err := waitHealthy(ctx, []string{s.backendA, s.backendB, s.base}, 3*time.Minute); err != nil {
		return fmt.Errorf("after the restart: %w", err)
	}
	var lost, cut, resolved, committed, cutReplays, replays int
	var mismatched []string
	bad := func(format string, args ...any) {
		if len(mismatched) < 5 {
			mismatched = append(mismatched, fmt.Sprintf(format, args...))
		}
	}
	for _, p := range plans {
		p.mu.Lock()
		pending, cuts := slices.Clone(p.lost), slices.Clone(p.cuts)
		p.lost = nil
		p.mu.Unlock()
		lost += len(pending)
		for _, r := range pending {
			if r.cut {
				cut++
			}
			a := sendIdem(ctx, ss.api, r.payload, r.key, s.maxAttempts)
			switch a.code {
			case http.StatusOK, http.StatusCreated:
				if a.code == http.StatusOK {
					committed++
				}
				p.record(r.start, a.id)
				p.recordSent(a.id, r)
				p.resolve(r.start)
				resolved++
			case http.StatusConflict, http.StatusUnprocessableEntity:
				p.resolve(r.start)
				resolved++
			default:
				bad("lost %s answered %d on replay: %s", r.key, a.code, a.body)
			}
		}
		for _, c := range cuts {
			cut++
			cutReplays++
			a := sendIdem(ctx, ss.api, c.payload, c.key, s.maxAttempts)
			switch {
			case c.outcome == outCreated || c.outcome == outReplayed:
				if a.code != http.StatusOK || a.id != c.id {
					bad("cut-off %s was answered created %s, replayed %d %s", c.key, c.id, a.code, a.id)
				}
			case a.code == http.StatusOK:
				bad("cut-off %s was answered %s but its key had committed %s", c.key, c.outcome, a.id)
			case a.code == http.StatusCreated:
				p.record(c.start, a.id) // never committed before; room has opened since
				p.recordSent(a.id, c.lostRequest)
			case a.code != http.StatusConflict && a.code != http.StatusUnprocessableEntity:
				bad("cut-off %s (answered %s) replayed %d: %s", c.key, c.outcome, a.code, a.body)
			}
		}
		for _, id := range firstIDs(p.createdIDs(), 25) {
			req, ok := p.sentFor(id)
			if !ok {
				continue
			}
			replays++
			if a := sendIdem(ctx, ss.api, req.payload, req.key, s.maxAttempts); a.code != http.StatusOK || a.id != id {
				bad("created %s (key %s) replayed %d id %s", id, req.key, a.code, a.id)
			}
		}
	}
	stats.unresolved.Add(-int64(resolved))
	none := ""
	if cut == 0 {
		none = " — none: the kill hit nothing in flight, so lost answers were never exercised"
	}
	m.check("Q1", "restart", len(mismatched) == 0 && cut > 0,
		"after the backend restart: %d requests had an attempt cut off by the kill (transport error / 502 / 504)%s; %d left unresolved replayed with their own key → %d definite (%d had committed unheard: 200); %d cut-off requests that resolved on a retry replayed consistently; %d created bookings replayed 200 with the same id%s",
		cut, none, lost, resolved, committed, cutReplays, replays, sampleSuffix(mismatched))
	return nil
}

func firstIDs(ids []uuid.UUID, n int) []uuid.UUID {
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return ids[:min(len(ids), n)]
}

// waitHealthy polls each base's /api/v1/health until it answers 200, up to timeout.
func waitHealthy(ctx context.Context, bases []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, base := range bases {
		api := newAPI(base)
		for {
			resp, err := api.do(ctx, http.MethodGet, "/api/v1/health", nil, nil)
			if err == nil && resp.code == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				if err == nil {
					err = fmt.Errorf("last answer %d", resp.code)
				}
				return fmt.Errorf("%s not healthy within %s: %w", base, timeout, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

// slotCounter counts, in Postgres, a phase's rows booked into a slot or among its known ids
// (moved since): bookings that committed although their answer was lost included. nil without a DSN.
func (ss *session) slotCounter(phase string) func(context.Context, time.Time, []uuid.UUID) (int, error) {
	if ss.conn == nil {
		return nil
	}
	return func(ctx context.Context, start time.Time, known []uuid.UUID) (int, error) {
		var n int
		err := ss.conn.QueryRow(ctx, `
			SELECT count(*)::int FROM appointments
			WHERE idempotency_key LIKE $1 AND (start_at = $2 OR id = ANY($3))`,
			ss.runID+":"+phase+":%", start, known).Scan(&n)
		return n, err
	}
}

// sqlChecks runs the integrity pass, the reconciliation, the outbox drain, and the read-back.
func (ss *session) sqlChecks(ctx context.Context, created map[uuid.UUID]bool, unresolved int64, mixes []string) error {
	m := ss.m
	if _, err := recordIntegrity(ctx, m, ss.conn, ss.runID, mixes, "sql"); err != nil {
		return err
	}
	rows, err := ss.conn.Query(ctx, `SELECT id FROM appointments WHERE idempotency_key LIKE $1`, ss.runID+":%")
	if err != nil {
		return err
	}
	inDB, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	rerr := reconcile(created, inDB, unresolved)
	m.check("R3", "sql", rerr == nil, "every one of the %d bookings clients were told about is a row in Postgres; %d rows in all (at most %d unheard)%s",
		len(created), len(inDB), unresolved, errSuffix(rerr))

	ids := slices.Collect(maps.Keys(created))
	checked, diffs, err := checkRecords(ctx, ss.conn, ss.direct, ids, 500)
	if err != nil {
		return err
	}
	m.check("R3", "read-back", len(diffs) == 0 && checked > 0, "%d records read back through both backends match their rows (customer, vehicle, technician, bay, window, status)%s", checked, sampleSuffix(diffs))
	m.check("Q2", "read-back", len(diffs) == 0 && checked > 0, "%d bookings read on alternating instances: same record everywhere", checked)

	// A backend killed mid-publish leaves its claimed rows "processing" until the lease (30 s)
	// expires and another instance reclaims them.
	drain := 30 * time.Second
	if ss.s.chaosRestart {
		drain = 2 * time.Minute
	}
	return recordOutbox(ctx, m, ss.conn, ss.runID, drain, "outbox")
}

// recordIntegrity runs the SQL integrity checks over the run's rows and records one verdict per
// check under the requirement it proves. It returns how many rows the run has: with none, every
// per-run check would pass over nothing, so that is one failure instead.
func recordIntegrity(ctx context.Context, m *matrix, conn *pgx.Conn, runID string, mixes []string, src string) (int, error) {
	var rows int
	if err := queryOne(ctx, conn, `SELECT count(*)::int FROM appointments WHERE idempotency_key LIKE $1`, []any{runID + ":%"}, &rows); err != nil {
		return 0, fmt.Errorf("sql integrity: count the run's rows: %w", err)
	}
	if rows == 0 {
		m.fail("R3", src, "no appointment of the run in Postgres (Idempotency-Key %s:…): the integrity checks would pass over zero rows", runID)
		return 0, nil
	}
	failures, err := runIntegrityChecks(ctx, conn, runID, mixes)
	if err != nil {
		return rows, err
	}
	byName := map[string]checkFailure{}
	for _, f := range failures {
		byName[f.name] = f
	}
	for _, c := range integrityChecks() {
		req := integrityRequirement(c.name)
		if f, bad := byName[c.name]; bad {
			m.fail(req, src, "%s: %v", c.name, f.rows)
		} else {
			m.pass(req, src, "%s: none (%s; the run has %d rows)", c.name, integrityMeaning(c.name), rows)
		}
	}
	return rows, nil
}

// recordOutbox waits for the run's outbox rows to drain and records the verdict.
func recordOutbox(ctx context.Context, m *matrix, conn *pgx.Conn, runID string, timeout time.Duration, src string) error {
	total, left, failed, err := waitOutboxDrained(ctx, conn, runID, timeout)
	if err != nil {
		return err
	}
	m.check("Q4", src, left == 0 && failed == 0 && total > 0, "%d outbox events of the run: %d not yet published, %d failed (waited up to %s)", total, left, failed, timeout)
	return nil
}

// integrityRequirement maps an integrity check to the requirement it proves.
func integrityRequirement(name string) string {
	switch name {
	case checkEvents:
		return "Q4"
	case checkLines, checkDuration, checkRequirement, checkRecord:
		return "R3"
	default:
		return "R2a"
	}
}

// integrityMeaning says in a few words what an empty result of a check proves.
func integrityMeaning(name string) string {
	switch name {
	case checkBayOverlap:
		return "no bay holds two jobs at any moment, whole database"
	case checkTechOverlap:
		return "no technician holds two jobs at any moment"
	case checkVehicleOverlap:
		return "no car is in two places"
	case checkStaging:
		return "arrival windows within the staging cap"
	case checkUnqualified:
		return "every job's technician has each skill at the level"
	case checkOffDuty:
		return "every job's technician is on duty for the whole window"
	case checkBay:
		return "every job is in a bay of the type it needs"
	case checkEvents:
		return "one CREATED outbox + audit row per booking, in its TX"
	case checkRecord:
		return "customer is the car's owner; bay and technician of the dealership"
	default:
		return "booked snapshot consistent with its lines"
	}
}

func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return ": " + err.Error()
}

func gapSuffix(gaps []string) string {
	if len(gaps) == 0 {
		return ""
	}
	return fmt.Sprintf(" — gaps %v", gaps)
}

func sampleSuffix(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf(" — %v", items)
}
