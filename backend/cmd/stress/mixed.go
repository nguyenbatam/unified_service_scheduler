package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The mixed phase (-mixed-ops) changes bookings while the hammer is still running: as soon as
// every request of a slot has answered, some of that slot's bookings are reassigned (technician or
// bay), rescheduled (to a free start whose window overlaps no hammered slot), or cancelled, each
// with If-Match; each success is followed by probes with the now-stale version (see mutate). A slot is only touched once it is fully answered and moves only go where no
// hammered slot reaches, so the per-slot capacity checks stay exact (a cancel only gives back room
// the hammer had when the slots were picked); the touched bookings are left
// out of the board checks, and the SQL pass checks them all (overlaps, staging, technician,
// bay, snapshot, events).

type mixOp int

const (
	opReassign mixOp = iota
	opReschedule
	opCancel
)

func (o mixOp) String() string {
	switch o {
	case opReassign:
		return "reassign"
	case opReschedule:
		return "reschedule"
	case opBurst:
		return "burst"
	case opRace:
		return "race"
	default:
		return "cancel"
	}
}

// mixOutcome classifies one answer to a mutation. stale is the follow-up sent with the version
// the first call already bumped, asking for a different change: only 412 is right (it is only
// judged on a definite answer; mutate counts one that never settles as stale_busy). Otherwise 200
// is done, 409/422 are refusals (the target is taken / not allowed), and 429/502/503/504 or a
// transport error (code 0) are indefinite. Anything else is a bug: 400/404/500, and also 412 —
// the version was read just before — unless a re-read shows another writer bumped it (mutate:
// "raced", the engine's repair may reassign a confirmed hold).
func mixOutcome(code int, stale bool) (label string, ok bool) {
	if stale {
		return "stale_" + strconv.Itoa(code), code == http.StatusPreconditionFailed
	}
	switch {
	case code == http.StatusOK:
		return "ok", true
	case code == http.StatusConflict:
		return "conflict", true
	case code == http.StatusUnprocessableEntity:
		return "unavailable", true
	case code == 0 || indefinite(code, nil): // 0: transport error
		return "busy", true
	default:
		return strconv.Itoa(code), false
	}
}

// mixStats counts mutation outcomes per "op/label" and keeps a sample of failures.
type mixStats struct {
	mu     sync.Mutex
	counts map[string]int
	errs   []string
}

func newMixStats() *mixStats { return &mixStats{counts: map[string]int{}} }

func (m *mixStats) add(op mixOp, label string, ok bool, detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[op.String()+"/"+label]++
	if !ok && len(m.errs) < 5 {
		m.errs = append(m.errs, fmt.Sprintf("%s %s: %s", op, label, detail))
	}
}

// addN adds n to "op/label" (a measured quantity, not an answer).
func (m *mixStats) addN(op mixOp, label string, n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.counts[op.String()+"/"+label] += n
}

func (m *mixStats) err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.errs) > 0 {
		return fmt.Errorf("mixed phase: unexpected answers %v", m.errs)
	}
	return nil
}

// vacuity fails a mixed phase that proved too little: every answer may be "fine" (busy, skipped,
// refused) while no mutation ever applied, or no stale version was ever refused. It needs at least
// one success (200) of each operation and at least one stale If-Match answered 412.
func (m *mixStats) vacuity() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var missing []string
	for _, op := range []mixOp{opReassign, opReschedule, opCancel} {
		if m.counts[op.String()+"/ok"] == 0 {
			missing = append(missing, op.String()+" never applied (no 200)")
		}
	}
	stale := 0
	for _, op := range []mixOp{opReassign, opReschedule, opCancel} {
		stale += m.counts[op.String()+"/stale_412"]
	}
	if stale == 0 {
		missing = append(missing, "no stale If-Match was refused 412")
	}
	if m.counts["burst/versions"] < 2 {
		missing = append(missing, "the burst made fewer than two versions of one appointment back to back")
	}
	if m.counts["race/one_winner"] == 0 {
		missing = append(missing, "no If-Match race ended with exactly one winner and the rest refused 412")
	}
	if len(missing) > 0 {
		return fmt.Errorf("mixed phase proved too little: %s", strings.Join(missing, "; "))
	}
	return nil
}

func (m *mixStats) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.counts))
	for k := range m.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, m.counts[k])
	}
	return strings.Join(parts, " ")
}

// slotQuota splits the remaining ops over the remaining slots (rounding up).
func slotQuota(remainingOps, remainingSlots int) int {
	if remainingOps <= 0 || remainingSlots <= 0 {
		return 0
	}
	return (remainingOps + remainingSlots - 1) / remainingSlots
}

// slotOrder is the order of the operations within one slot: a cancel first gives back a bay and a
// technician in the slot's window, so the reassign after it has somewhere to go even in a slot
// the hammer filled to capacity (river's exact slots have no free bay or technician at all
// otherwise, and a reassign there could only ever be refused).
var slotOrder = []mixOp{opCancel, opReschedule, opReassign}

// mixPhase consumes p.slotDone until the hammer closes it, mutating up to ops bookings in all.
// Each slot it touches gets at least the three operations in slotOrder (when ops remain), so a
// small -mixed-ops still applies every operation once instead of only cancelling. Then it runs the
// burst and the If-Match race (mixburst.go) on a booking of its own made with vehicle.
func mixPhase(ctx context.Context, api *apiClient, p *plan, ops int, stats *mixStats, vehicle uuid.UUID, key string) {
	defer func() {
		if ctx.Err() == nil {
			if id := mixBurst(ctx, api, p, vehicle, key, stats); id != uuid.Nil {
				p.addExtra(id)
			}
		}
	}()
	remaining, slotsLeft, n := ops, len(p.slots), 0
	for si := range p.slotDone {
		quota := max(slotQuota(remaining, slotsLeft), min(len(slotOrder), remaining))
		slotsLeft--
		k := 0
		for _, id := range p.slotIDs(si) {
			if quota == 0 || ctx.Err() != nil {
				break
			}
			mutate(ctx, api, p, id, slotOrder[k%len(slotOrder)], n, stats)
			n++
			k++
			quota--
			remaining--
		}
	}
}

// mixCall is one POST of the mixed phase.
type mixCall struct {
	path string
	body map[string]any
}

// mutate applies op to id with If-Match, and on success probes the version check twice with the
// version it just made stale: the same call again must replay (200, version unchanged — the server
// treats it as a retry of a change that already applied), and a different change (back to the
// original resources/start, or a check-in after a cancel) must be refused 412.
//
// Under the hammer any call may be answered busy (503 at the request deadline, 429, or 502/504
// from the gateway). The mutation records that as "busy"; the read before it and both probes are
// safe to resend, so they retry a few times (sendUntilDefinite) and only then count as
// read_busy / replay_busy / stale_busy — fine, not a failure. A definite answer is held to the
// strict expectations above.
func mutate(ctx context.Context, api *apiClient, p *plan, id uuid.UUID, op mixOp, n int, stats *mixStats) {
	cur, ok := mixRead(ctx, api, id, op, stats)
	if !ok {
		return
	}
	calls, undo, ok := mixRequest(p, cur, op, n)
	if !ok {
		stats.add(op, "skipped", true, "")
		return
	}
	p.markMutated(cur) // before sending: an indefinite answer may still have applied it
	ifMatch := map[string]string{"If-Match": `"` + strconv.Itoa(cur.Version) + `"`}
	// Candidates in turn: a target that is taken (409) or does not qualify (422) leaves the
	// version as it was, so the next one is tried with the same If-Match.
	var call mixCall
	var resp response
	for _, c := range calls {
		call = c
		var err error
		resp, err = api.do(ctx, http.MethodPost, call.path, call.body, ifMatch)
		if err != nil {
			resp.code = 0
		}
		label, good := mixOutcome(resp.code, false)
		if resp.code == http.StatusPreconditionFailed && movedSince(ctx, api, cur) {
			// Someone else wrote the booking between the read and the call: the engine's one-step
			// repair may reassign another confirmed hold to make room for a refused booking.
			label, good = "raced", true
		}
		stats.add(op, label, good, truncate(string(resp.body), 120))
		if resp.code != http.StatusConflict && resp.code != http.StatusUnprocessableEntity {
			break
		}
	}
	if resp.code != http.StatusOK {
		return
	}
	var after domain.Appointment
	if jerr := json.Unmarshal(resp.body, &after); jerr != nil || after.Version <= cur.Version {
		stats.add(op, "version_not_bumped", false, truncate(string(resp.body), 120))
		return
	}
	if resp, definite := sendUntilDefinite(ctx, api, http.MethodPost, call.path, call.body, ifMatch, probeAttempts); !definite {
		stats.add(op, "replay_busy", true, "")
	} else {
		var again domain.Appointment
		replayed := resp.code == http.StatusOK && json.Unmarshal(resp.body, &again) == nil && again.Version == after.Version
		label := "replay_" + strconv.Itoa(resp.code)
		if !replayed && (resp.code == http.StatusPreconditionFailed || resp.code == http.StatusOK) && movedSince(ctx, api, after) {
			label, replayed = "replay_raced", true // a repair moved it since: the replay meets a newer version
		}
		stats.add(op, label, replayed, truncate(string(resp.body), 120))
	}
	if resp, definite := sendUntilDefinite(ctx, api, http.MethodPost, undo.path, undo.body, ifMatch, probeAttempts); !definite {
		stats.add(op, "stale_busy", true, "")
	} else {
		label, good := mixOutcome(resp.code, true)
		stats.add(op, label, good, truncate(string(resp.body), 120))
	}
}

// movedSince reports whether a now has a newer version than cur (read with retries; false when the
// read never settles).
func movedSince(ctx context.Context, api *apiClient, cur domain.Appointment) bool {
	resp, definite := sendUntilDefinite(ctx, api, http.MethodGet, "/api/v1/appointments/"+cur.ID.String(), nil, nil, probeAttempts)
	var now domain.Appointment
	return definite && resp.code == http.StatusOK && json.Unmarshal(resp.body, &now) == nil && now.Version > cur.Version
}

// mixRead reads the appointment a mutation starts from, retrying indefinite answers. It records
// read_busy when they never settle, and a failure for any definite answer other than 200 with a
// readable body. Nothing is recorded once the run is interrupted.
func mixRead(ctx context.Context, api *apiClient, id uuid.UUID, op mixOp, stats *mixStats) (domain.Appointment, bool) {
	var cur domain.Appointment
	resp, definite := sendUntilDefinite(ctx, api, http.MethodGet, "/api/v1/appointments/"+id.String(), nil, nil, probeAttempts)
	switch {
	case ctx.Err() != nil:
		return cur, false
	case !definite:
		stats.add(op, "read_busy", true, "")
		return cur, false
	case resp.code != http.StatusOK:
		stats.add(op, "read_"+strconv.Itoa(resp.code), false, truncate(string(resp.body), 120))
		return cur, false
	}
	if err := json.Unmarshal(resp.body, &cur); err != nil {
		stats.add(op, "read", false, "decode: "+err.Error())
		return cur, false
	}
	return cur, true
}

// mixRequest builds op for appointment a — for a reassign every candidate target in turn, starting
// at the n-th, so one taken target does not decide it — and a different change to probe the stale
// version with. Reassign alternates technician and bay (another one of the same bay type, so a
// waiter's soft upgrade stays valid); reschedule picks the n-th move target.
func mixRequest(p *plan, a domain.Appointment, op mixOp, n int) (calls []mixCall, undo mixCall, ok bool) {
	base := "/api/v1/appointments/" + a.ID.String()
	rotate := func(ids []uuid.UUID, field string) []mixCall {
		out := make([]mixCall, len(ids))
		for i := range ids {
			out[i] = mixCall{base + "/reassign", map[string]any{field: ids[(n+i)%len(ids)]}}
		}
		return out
	}
	switch op {
	case opReassign:
		if n%2 == 0 {
			var techs []uuid.UUID
			for _, t := range p.technicians {
				if t.ID != a.TechnicianID {
					techs = append(techs, t.ID)
				}
			}
			if len(techs) == 0 {
				return nil, undo, false
			}
			return rotate(techs, "technicianId"), mixCall{base + "/reassign", map[string]any{"technicianId": a.TechnicianID}}, true
		}
		var bays []uuid.UUID
		for _, b := range p.bays {
			if b.ID != a.ServiceBayID && b.BayType == a.ServiceBayType {
				bays = append(bays, b.ID)
			}
		}
		if len(bays) == 0 {
			return nil, undo, false
		}
		return rotate(bays, "serviceBayId"), mixCall{base + "/reassign", map[string]any{"serviceBayId": a.ServiceBayID}}, true
	case opReschedule:
		if len(p.moveTargets) == 0 {
			return nil, undo, false
		}
		return []mixCall{{base + "/reschedule", map[string]any{"startAt": p.moveTargets[n%len(p.moveTargets)]}}},
			mixCall{base + "/reschedule", map[string]any{"startAt": a.StartAt}}, true
	default:
		return []mixCall{{base + "/cancel", nil}}, mixCall{base + "/check-in", nil}, true
	}
}
