package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The mixed phase's sequential mutations leave a full HTTP round trip between two commits of one
// appointment, so they barely exercise event order: the outbox publishes one event long before the
// next exists. Once a phase's slots are all answered, the burst makes changes of one aggregate
// commit as close together as the row lock allows, and races writers on one version:
//
//   - burst: a booking of its own (at a move target, outside every hammered slot) is reassigned
//     back and forth between two bays of its type by burstSize requests without If-Match, in
//     burstWaves back-to-back waves: a wave's requests all ask for the same bay at once, so they
//     queue on the row lock and one commits a new version while the rest replay without one, and
//     the next wave asks for the other bay right after. Every wave makes exactly one version (were
//     they all concurrent, the ones asking for the bay it is already in could all run first and
//     leave a single move). Every version must reach every listener, in order (the run's WS order
//     and event checks, which read the outbox);
//   - race: raceSize concurrent reschedules of that booking to distinct free starts, all with the
//     same If-Match: at most one may win (200, version + 1, the booking at its start), and once one
//     has won every later one must be refused 412; 409/422 (start taken meanwhile) leave the version
//     as it was. Two winners would be a lost update.

const (
	burstSize  = 8
	burstWaves = 4
	raceSize   = 6
)

const (
	opBurst mixOp = iota + 100
	opRace
)

// mixBurst runs the burst and the race on a booking of its own at one of p's move targets. vehicle
// is the car it books; it returns the booking's id (uuid.Nil when none could be made).
func mixBurst(ctx context.Context, api *apiClient, p *plan, vehicle uuid.UUID, key string, stats *mixStats) uuid.UUID {
	if len(p.moveTargets) < 2 || len(p.jobs) == 0 {
		stats.add(opBurst, "skipped", true, "")
		return uuid.Nil
	}
	var appt domain.Appointment
	var booked bool
	for _, start := range p.moveTargets { // the first move target that books
		resp, definite := sendUntilDefinite(ctx, api, http.MethodPost, "/api/v1/appointments",
			bookingPayload(p.dealer.ID, vehicle, p.jobs[0], start), map[string]string{"Idempotency-Key": key + ":" + strconv.FormatInt(start.Unix(), 10)}, probeAttempts)
		if definite && (resp.code == http.StatusCreated || resp.code == http.StatusOK) && json.Unmarshal(resp.body, &appt) == nil && appt.ID != uuid.Nil {
			booked = true
			break
		}
	}
	if !booked {
		stats.add(opBurst, "skipped", true, "")
		return uuid.Nil
	}
	p.markMutated(appt)
	burstReassign(ctx, api, p, appt, stats)
	raceReschedule(ctx, api, p, appt.ID, stats)
	return appt.ID
}

// burstReassign moves a to another free bay of its type, then fires burstSize reassigns in
// burstWaves concurrent waves alternating between the original bay and that one, and records how
// many versions they made (one per wave).
func burstReassign(ctx context.Context, api *apiClient, p *plan, a domain.Appointment, stats *mixStats) {
	path := "/api/v1/appointments/" + a.ID.String() + "/reassign"
	other := uuid.Nil
	for _, b := range p.bays {
		if b.ID == a.ServiceBayID || b.BayType != a.ServiceBayType {
			continue
		}
		resp, definite := sendUntilDefinite(ctx, api, http.MethodPost, path, map[string]any{"serviceBayId": b.ID}, nil, probeAttempts)
		if definite && resp.code == http.StatusOK {
			other = b.ID
			break
		}
	}
	if other == uuid.Nil {
		stats.add(opBurst, "no_free_bay", true, "")
		return
	}
	start, ok := readVersion(ctx, api, a.ID)
	if !ok {
		stats.add(opBurst, "read_busy", true, "")
		return
	}
	for wave := range burstWaves {
		target := a.ServiceBayID
		if wave%2 == 1 {
			target = other
		}
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for range burstSize / burstWaves {
			wg.Go(func() {
				<-gate
				resp, err := api.do(ctx, http.MethodPost, path, map[string]any{"serviceBayId": target}, nil)
				if err != nil {
					resp.code = 0
				}
				label, good := mixOutcome(resp.code, false)
				stats.add(opBurst, label, good, truncate(string(resp.body), 120))
			})
		}
		close(gate)
		wg.Wait()
	}
	end, ok := readVersion(ctx, api, a.ID)
	if !ok {
		stats.add(opBurst, "read_busy", true, "")
		return
	}
	stats.addN(opBurst, "versions", end-start)
}

// raceReschedule fires raceSize concurrent reschedules of id, one per distinct free start, all with the
// version just read, and judges the outcome: at most one winner, and a winner means version + 1.
func raceReschedule(ctx context.Context, api *apiClient, p *plan, id uuid.UUID, stats *mixStats) {
	cur, ok := readAppointment(ctx, api, id)
	if !ok {
		stats.add(opRace, "read_busy", true, "")
		return
	}
	var targets []time.Time
	for _, t := range p.moveTargets {
		if !t.Equal(cur.StartAt) && len(targets) < raceSize {
			targets = append(targets, t) // distinct starts: the same start twice would replay 200
		}
	}
	if len(targets) < 2 {
		stats.add(opRace, "skipped", true, "")
		return
	}
	ifMatch := map[string]string{"If-Match": `"` + strconv.Itoa(cur.Version) + `"`}
	codes := make([]int, len(targets))
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i, t := range targets {
		wg.Go(func() {
			<-gate
			resp, err := api.do(ctx, http.MethodPost, "/api/v1/appointments/"+id.String()+"/reschedule", map[string]any{"startAt": t}, ifMatch)
			if err == nil {
				codes[i] = resp.code
			}
		})
	}
	close(gate)
	wg.Wait()
	after, ok := readAppointment(ctx, api, id)
	if !ok {
		stats.add(opRace, "read_busy", true, "")
		return
	}
	label, good, detail := raceVerdict(codes, targets, cur, after)
	stats.add(opRace, label, good, detail)
}

// raceVerdict judges one If-Match race: codes are the answers to reschedules to targets (0 =
// transport error), cur the booking before and after it after.
func raceVerdict(codes []int, targets []time.Time, cur, after domain.Appointment) (label string, ok bool, detail string) {
	winners, stale, unsettled := 0, 0, 0
	var won time.Time
	for i, c := range codes {
		switch {
		case c == http.StatusOK:
			winners++
			won = targets[i]
		case c == http.StatusPreconditionFailed:
			stale++
		case c == http.StatusConflict || c == http.StatusUnprocessableEntity:
		case c == 0 || indefinite(c, nil):
			unsettled++
		default:
			return "unexpected_" + strconv.Itoa(c), false, fmt.Sprintf("answers %v", codes)
		}
	}
	switch {
	case winners > 1:
		return "lost_update", false, fmt.Sprintf("%d reschedules with If-Match %d all answered 200: %v", winners, cur.Version, codes)
	case winners == 1 && (after.Version != cur.Version+1 || !after.StartAt.Equal(won)):
		return "winner_not_applied", false, fmt.Sprintf("won %s, now v%d at %s (was v%d)", won, after.Version, after.StartAt, cur.Version)
	case winners == 0 && unsettled == 0 && after.Version != cur.Version:
		return "changed_without_winner", false, fmt.Sprintf("v%d → v%d with answers %v", cur.Version, after.Version, codes)
	case winners == 1 && stale > 0:
		return "one_winner", true, ""
	default:
		return "inconclusive", true, "" // nobody won (every start taken, or busy), or the winner was last
	}
}

// readAppointment reads id, retrying indefinite answers.
func readAppointment(ctx context.Context, api *apiClient, id uuid.UUID) (domain.Appointment, bool) {
	var a domain.Appointment
	resp, definite := sendUntilDefinite(ctx, api, http.MethodGet, "/api/v1/appointments/"+id.String(), nil, nil, probeAttempts)
	return a, definite && resp.code == http.StatusOK && json.Unmarshal(resp.body, &a) == nil && a.ID == id
}

func readVersion(ctx context.Context, api *apiClient, id uuid.UUID) (int, bool) {
	a, ok := readAppointment(ctx, api, id)
	return a.Version, ok
}
