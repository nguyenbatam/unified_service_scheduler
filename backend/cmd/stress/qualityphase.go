package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The quality phase plays the scripted quality day (qualityday.go) against the server, one
// request at a time through the gateway, on the "Stress Quality" workshop (today+5, reset first),
// and judges it independently of the engine:
//
//   - Q5 "beats first fit": the labour minutes the day's bookings hold (from Postgres) are at
//     least -quality-margin-pct above the best of the offline first-fit replays (every fixed
//     order of the bays and technicians, naive and own-type-first, on the oracle's model of the
//     shop read from the tables);
//   - Q5 "repair": at least one booking made room with the one-move repair, attributed from
//     appointment_history like the truth phase does (APPOINTMENT_REASSIGNED rows of the shop
//     after the request's mark: nothing else books it meanwhile), each move tied to the request
//     that needed it and moving one of the day's own bookings, and the SQL integrity pass over
//     the day's rows (fit, qualification, duty, overlaps, events, snapshot) is clean;
//   - Q5 "quality" (INFO): booked minutes vs the offline optimum (exhaustive assignment search at
//     the fixed starts) and vs the max-flow capacity bound; exceeding either fails.

// qualityDayOffset is the quality day's distance from the shop's today.
const qualityDayOffset = 5

// qualityMove is one one-move repair a request made: the appointment moved, which request of the
// day it was ("" when none: then it is not the day's own), and its bay / technician before → after
// (names; equal when that resource did not change).
type qualityMove struct {
	appointment         uuid.UUID
	label               string
	fromBay, toBay      string
	fromTech, toTech    string
	bayMoved, techMoved bool
}

func (mv qualityMove) String() string {
	var parts []string
	if mv.bayMoved {
		parts = append(parts, fmt.Sprintf("bay %s → %s", mv.fromBay, mv.toBay))
	}
	if mv.techMoved {
		parts = append(parts, fmt.Sprintf("technician %s → %s", mv.fromTech, mv.toTech))
	}
	who := mv.label
	if who == "" {
		who = mv.appointment.String()
	}
	return who + " (" + strings.Join(parts, ", ") + ")"
}

// qualityOutcome is what one request got from the server.
type qualityOutcome struct {
	booked    bool
	id        uuid.UUID
	bay, tech string
	answer    string // the HTTP status (or the engine's error in the DB test) when not booked
	moves     []qualityMove
}

// qualityMovesSince lists the one-move repairs at dealer recorded after history row mark, with
// each moved appointment's position before (its previous history row) and after, by name.
func qualityMovesSince(ctx context.Context, q querier, dealer uuid.UUID, mark int64, names map[uuid.UUID]string) ([]qualityMove, error) {
	rows, err := q.Query(ctx, `
		SELECT h.appointment_id, p.service_bay_id, h.service_bay_id, p.technician_id, h.technician_id
		FROM appointment_history h
		JOIN appointments a ON a.id = h.appointment_id
		CROSS JOIN LATERAL (
			SELECT p.service_bay_id, p.technician_id FROM appointment_history p
			WHERE p.appointment_id = h.appointment_id AND p.id < h.id ORDER BY p.id DESC LIMIT 1) p
		WHERE h.id > $1 AND a.dealership_id = $2 AND h.event_type = `+sqlCode(domain.EventAppointmentReassigned)+`
		ORDER BY h.id`, mark, dealer)
	if err != nil {
		return nil, fmt.Errorf("quality: moves: %w", err)
	}
	defer rows.Close()
	var out []qualityMove
	for rows.Next() {
		var mv qualityMove
		var fromBay, toBay, fromTech, toTech uuid.UUID
		if err := rows.Scan(&mv.appointment, &fromBay, &toBay, &fromTech, &toTech); err != nil {
			return nil, fmt.Errorf("quality: moves: %w", err)
		}
		mv.fromBay, mv.toBay, mv.fromTech, mv.toTech = names[fromBay], names[toBay], names[fromTech], names[toTech]
		mv.bayMoved, mv.techMoved = fromBay != toBay, fromTech != toTech
		out = append(out, mv)
	}
	return out, rows.Err()
}

// qualityVerdict is everything the quality day is judged on.
type qualityVerdict struct {
	shop       string
	day        booking.Date
	demand     []qualityRequest
	outcomes   []qualityOutcome
	asked      int // labour minutes asked
	server     int // labour minutes the day's bookings hold now (Postgres)
	spread     firstFitSpread
	optimum    int
	bound      int
	bays       int
	techs      int
	integrity  []checkFailure
	names      map[uuid.UUID]string
	unexpected []string
}

// judgeQualityDay reads the day back after the server played demand (outcomes, one per request;
// keyPrefix: the Idempotency-Key prefix of its bookings, mixKeys: its job mixes for the lines
// check) and computes the offline baselines on the oracle's model of the shop.
func judgeQualityDay(ctx context.Context, q querier, w workshop, day booking.Date, demand []qualityRequest, outcomes []qualityOutcome, keyPrefix string, mixKeys []string, names map[uuid.UUID]string) (*qualityVerdict, error) {
	v := &qualityVerdict{shop: w.dealer.Name, day: day, demand: demand, outcomes: outcomes, names: names}
	if err := queryOne(ctx, q, bookedMinutesSQL, []any{keyPrefix + ":%"}, &v.server); err != nil {
		return nil, fmt.Errorf("quality: booked minutes: %w", err)
	}
	from, to := dayBounds(day, w.loc)
	shop, err := loadShop(ctx, q, w.dealer.ID, from, to)
	if err != nil {
		return nil, err
	}
	v.bays, v.techs = len(shop.bays), len(shop.techs)
	if v.spread, err = firstFitEveryOrder(shop, demand); err != nil {
		return nil, err
	}
	v.optimum = fixedOptimum(shop, demand)
	classes := map[string]*demandClass{}
	var order []string
	for _, r := range demand {
		v.asked += r.mix.req.minutes
		k := fillClassKey(r.mix, r.waiter)
		if classes[k] == nil {
			classes[k] = &demandClass{mix: r.mix, waiter: r.waiter}
			order = append(order, k)
		}
		classes[k].minutes += r.mix.req.minutes
	}
	list := make([]demandClass, 0, len(order))
	for _, k := range order {
		list = append(list, *classes[k])
	}
	if v.bound, _, _, err = capacityBound(shop, w, day, list); err != nil {
		return nil, err
	}
	if v.integrity, err = runIntegrityChecks(ctx, q, keyPrefix, mixKeys); err != nil {
		return nil, err
	}
	return v, nil
}

// repairs counts the moves the day's requests made, and lists those that cannot be attributed
// (moved an appointment that is not one of the day's bookings, or more than one per resource).
func (v *qualityVerdict) repairs() (moves int, stray []string) {
	for i, o := range v.outcomes {
		bays, techs := 0, 0
		for _, mv := range o.moves {
			moves++
			if mv.label == "" {
				stray = append(stray, fmt.Sprintf("%s moved %s, not one of the day's bookings", v.demand[i].label, mv.appointment))
			}
			if mv.bayMoved {
				bays++
			}
			if mv.techMoved {
				techs++
			}
		}
		if bays > 1 || techs > 1 || (len(o.moves) > 0 && !o.booked) {
			stray = append(stray, fmt.Sprintf("%s: %d bay / %d technician move(s), booked=%v (one move per missing resource, only for a booking)", v.demand[i].label, bays, techs, o.booked))
		}
	}
	return moves, stray
}

// refusedLabels lists the requests a replay left out (booked[i] false).
func (v *qualityVerdict) refusedLabels(booked []bool) string {
	var out []string
	for i, ok := range booked {
		if !ok {
			out = append(out, v.demand[i].label)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, " ")
}

// orderLabel names a first-fit replay's resource order.
func (v *qualityVerdict) orderLabel(run firstFitRun) string {
	name := func(ids []uuid.UUID) string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = v.names[id]
		}
		return strings.Join(out, ", ")
	}
	variant := "naive"
	if run.ownTypeFirst {
		variant = "own bay type first"
	}
	return fmt.Sprintf("%s; bays %s; technicians %s", variant, name(run.bays), name(run.techs))
}

// record writes the quality day's evidence into the matrix.
func (v *qualityVerdict) record(m *matrix, marginPct float64) {
	booked := 0
	var serverRefused []string
	var placed []string
	for i, o := range v.outcomes {
		if o.booked {
			booked++
			placed = append(placed, fmt.Sprintf("%s→%s/%s", v.demand[i].label, o.bay, o.tech))
		} else {
			serverRefused = append(serverRefused, v.demand[i].label+" "+o.answer)
		}
	}
	m.check("R1", "quality", booked > 0, "%d of %d scripted bookings (exact starts, staff, waiting and not) on one day of %s", booked, len(v.demand), v.shop)
	best, worst := v.spread.best, v.spread.worst
	err := qualityShortfall(v.server, best.minutes, marginPct)
	m.check("Q5", "beats first fit", err == nil && len(v.unexpected) == 0,
		"scripted quality day at %s on %s (%d requests at exact starts, one at a time: best fit, quick lane + bay repair, technician repair): the server booked %d of the %d labour minutes asked (%d/%d requests; refused: %v); "+
			"first fit replayed offline on the oracle's model in every fixed order of the %d bays and %d technicians, naive and own bay type first (%d replays), books %d … %d; "+
			"server %s vs the best first fit (%s; refuses %s; margin +%.0f%%), %s vs the worst (refuses %s)%s%s",
		v.shop, v.day, len(v.demand), v.server, v.asked, booked, len(v.demand), serverRefused,
		v.bays, v.techs, v.spread.runs, worst.minutes, best.minutes,
		gainPct(v.server, best.minutes), v.orderLabel(best), v.refusedLabels(best.booked), marginPct,
		gainPct(v.server, worst.minutes), v.refusedLabels(worst.booked), errSuffix(err), unexpectedSuffix(v.unexpected))
	moves, stray := v.repairs()
	var list []string
	for i, o := range v.outcomes {
		for _, mv := range o.moves {
			list = append(list, v.demand[i].label+" moved "+mv.String())
		}
	}
	var integrity []string
	for _, f := range v.integrity {
		integrity = append(integrity, f.String())
	}
	m.check("Q5", "repair", moves > 0 && len(stray) == 0 && len(v.integrity) == 0,
		"%d one-move repair(s) on the quality day, each by the request that found nothing free and moving one of the day's own confirmed bookings (same window): %v%s; SQL integrity over the day's rows (fit, qualification, duty, overlaps, events, snapshot): %s",
		moves, list, strayed(stray), cleanOr(integrity))
	m.infoUnless("Q5", "quality", v.server <= v.optimum && v.server <= v.bound,
		"quality day utilisation: %d labour minutes = %s of the offline optimum %d for this demand at its fixed starts (exhaustive assignment search), %s of the max-flow capacity bound %d; placements %s",
		v.server, pct(v.server, v.optimum), v.optimum, pct(v.server, v.bound), v.bound, strings.Join(placed, " "))
	m.addHeadline("Q5", "quality day %s vs best first fit, %d repair(s)", gainPct(v.server, best.minutes), moves)
	m.metric("quality", map[string]any{
		"asked": v.asked, "booked": v.server, "optimum": v.optimum, "bound": v.bound,
		"firstFitBest": best.minutes, "firstFitWorst": worst.minutes, "firstFitRuns": v.spread.runs, "repairs": moves,
	})
}

func unexpectedSuffix(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("; unexpected answers: %v", items)
}

func strayed(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("; unattributable: %v", items)
}

func cleanOr(failures []string) string {
	if len(failures) == 0 {
		return "clean"
	}
	return strings.Join(failures, "; ")
}

// workshopNames maps w's bays and technicians to their names.
func workshopNames(w workshop) map[uuid.UUID]string {
	names := map[uuid.UUID]string{}
	for _, b := range w.bays {
		names[b.ID] = b.Name
	}
	for _, t := range w.techs {
		names[t.ID] = t.Name
	}
	return names
}

// qualityMixes are the script's services as one-line job mixes from the catalog.
func qualityMixes(c catalog) (map[string]jobMix, error) {
	out := map[string]jobMix{}
	for _, code := range qualityServices {
		mix, err := newJobMix(c, code, code)
		if err != nil {
			return nil, err
		}
		out[code] = mix
	}
	return out, nil
}

// qualityDay runs the quality phase and records it; it returns the bookings made.
func (ss *session) qualityDay(ctx context.Context, mixes map[string]bool) ([]uuid.UUID, error) {
	s := ss.s
	w, err := ensureWorkshop(ctx, ss.api, qualityWorkshop)
	if err != nil {
		return nil, err
	}
	clearUpcoming := func(ctx context.Context) error {
		return ss.resetDay(ctx, w, time.Now(), time.Now().AddDate(1, 0, 1))
	}
	if err := ensureShifts(ctx, ss.api, w, clearUpcoming); err != nil {
		return nil, err
	}
	jobs, err := qualityMixes(ss.catalog)
	if err != nil {
		return nil, err
	}
	var mixKeys []string
	for _, code := range qualityServices {
		mixes[mixKey(jobs[code].ids)] = true
		mixKeys = append(mixKeys, mixKey(jobs[code].ids))
	}
	cars, err := ensureVehicles(ctx, ss.api, ss.owner, vinQuality, len(qualityScript), s.workers)
	if err != nil {
		return nil, err
	}
	today, err := booking.ParseDate(w.dealer.Today)
	if err != nil {
		return nil, err
	}
	day := today.AddDays(qualityDayOffset)
	if err := ss.prepareDay(ctx, w, day); err != nil {
		return nil, err
	}
	demand, err := qualityDemand(w, day, jobs, cars)
	if err != nil {
		return nil, err
	}
	fmt.Printf("phase quality start: %s on %s, %d scripted requests at exact starts, one at a time, through %s\n", w.dealer.Name, day, len(demand), s.base)
	names := workshopNames(w)
	keyPrefix := ss.runID + ":quality"
	outcomes := make([]qualityOutcome, len(demand))
	var ids []uuid.UUID
	var unexpected []string
	label := map[uuid.UUID]string{} // the day's bookings → request label
	for i, r := range demand {
		mark, err := historyMark(ctx, ss.conn)
		if err != nil {
			return ids, err
		}
		payload := bookingPayload(w.dealer.ID, r.vehicle, r.mix.ids, r.start)
		payload["isWaiter"] = r.waiter
		resp, definite := sendUntilDefinite(ctx, ss.api, http.MethodPost, "/api/v1/appointments", payload,
			map[string]string{"Idempotency-Key": fmt.Sprintf("%s:%d", keyPrefix, i)}, probeAttempts)
		o := &outcomes[i]
		o.answer = fmt.Sprint(resp.code)
		switch {
		case resp.code == http.StatusCreated || resp.code == http.StatusOK:
			var appt domain.Appointment
			if err := json.Unmarshal(resp.body, &appt); err != nil || appt.ID == uuid.Nil {
				unexpected = append(unexpected, fmt.Sprintf("%s: %d with an unreadable body", r.label, resp.code))
				continue
			}
			o.booked, o.id, o.bay, o.tech = true, appt.ID, appt.ServiceBayName, appt.TechnicianName
			ids = append(ids, appt.ID)
			label[appt.ID] = r.label
			if o.moves, err = qualityMovesSince(ctx, ss.conn, w.dealer.ID, mark, names); err != nil {
				return ids, err
			}
			for k := range o.moves {
				o.moves[k].label = label[o.moves[k].appointment]
			}
		case definite && (resp.code == http.StatusConflict || resp.code == http.StatusUnprocessableEntity):
			// Refused: what the margin judges.
		default:
			unexpected = append(unexpected, fmt.Sprintf("%s: %d definite=%v %s", r.label, resp.code, definite, truncate(string(resp.body), 100)))
		}
		fmt.Printf("quality %s %-5s %s waiter=%-5v → %s\n", r.label, r.mix.name, r.start.In(w.loc).Format("15:04"), r.waiter, describeOutcome(*o))
	}
	v, err := judgeQualityDay(ctx, ss.conn, w, day, demand, outcomes, keyPrefix, mixKeys, names)
	if err != nil {
		return ids, err
	}
	v.unexpected = unexpected
	v.record(ss.m, s.qualityMarginPct)
	return ids, nil
}

// describeOutcome is one request's line in the phase's log.
func describeOutcome(o qualityOutcome) string {
	if !o.booked {
		return "refused " + o.answer
	}
	out := "booked " + o.bay + " / " + o.tech
	if len(o.moves) > 0 {
		moved := make([]string, len(o.moves))
		for i, mv := range o.moves {
			moved[i] = mv.String()
		}
		out += ", moving " + strings.Join(moved, "; ")
	}
	return out
}
