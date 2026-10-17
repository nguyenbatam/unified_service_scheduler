package main

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// plan is one phase: a dealership, a date with free capacity, the job mixes requests rotate
// through, and the slots to hammer with their capacity bounds.
type plan struct {
	name   string
	dealer domain.Dealership
	loc    *time.Location
	date   string
	jobs   [][]uuid.UUID // service type ids per job mix; request i books jobs[i%len(jobs)]
	mixes  []jobMix      // the same job mixes with their combined requirement (the refusal oracle)
	slots  []slotPlan
	// exact: the slots never overlap, no hold of the shop overlaps them, and the dealership has no
	// staging cap, so each slot must end with exactly its bound (bound.go) of bookings. Otherwise
	// only upper bounds (per slot, per staging window) are checked here and SQL proves the
	// invariants.
	exact       bool
	board       *domain.Schedule   // the date's board when the phase was planned
	preexisting map[uuid.UUID]bool // holding appointments on the board before the hammer
	arrivals    map[int64]int      // pre-existing holding arrivals per staging window start (unix)

	// maxUnresolvedPct fails the phase when more of its requests than this stay unresolved.
	maxUnresolvedPct float64
	requests         int // requests sent by hammer

	// cutoff: no slot or move target starts before it (prepare time + -start-margin), so a slot
	// never slides into the past while it is hammered and checked.
	cutoff time.Time
	// prepared is when the phase was planned: the refusal oracle adds every position a hold of the
	// dealership had since (refusals.go).
	prepared time.Time
	// maxJobMinutes is the longest job mix's labour time.
	maxJobMinutes int
	// moveTargets are free starts of the date whose windows overlap no hammered slot's, for the
	// mixed phase's reschedules (they must not change what a hammered slot can hold).
	moveTargets []time.Time
	bays        []domain.Bay        // the dealership's bays (reassign targets)
	technicians []domain.Technician // its active technicians (reassign targets)
	// slotDone, when set (mixed phase), gets each slot's index once every request of it answered.
	slotDone chan int

	// offeredAt[j] holds the starts (unix) availability offered job mix j when the phase was
	// prepared: only refusals at those starts are judged (their calendar is vouched for).
	offeredAt []map[int64]bool
	refusals  *refusalLog

	mu         sync.Mutex
	created    map[int64]map[uuid.UUID]bool // slot start (unix) → appointment ids (201 or replay 200)
	unresolved map[int64]int                // requests still without a definite answer
	mutated    map[uuid.UUID]bool           // touched by the mixed phase (moved, reassigned, cancelled)
	// before are the holds the mixed phase may have changed, as they were before the change: the
	// refusal oracle adds them back to the end state (see refusals.go).
	before []oHold
	// lost are the requests left unresolved, with their key and payload, for the post-restart
	// replay (-chaos-restart); sent are the created bookings' requests (replayed after it).
	lost []lostRequest
	sent map[uuid.UUID]lostRequest
	cuts []cutRequest
	// extra are the phase's bookings outside its hammered slots (the burst's, mixburst.go).
	extra []uuid.UUID
}

// addExtra records a booking of the phase made outside its hammered slots.
func (p *plan) addExtra(id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.extra = append(p.extra, id)
}

// lostRequest is a hammer request that never got a definite answer.
type lostRequest struct {
	key     string
	payload map[string]any
	start   time.Time
	cut     bool // an attempt lost its answer on the way (transport, 502/504)
}

// cutRequest is a request one of whose attempts lost its answer on the way but that still got a
// definite answer on a retry: outcome and id are what that answer said.
type cutRequest struct {
	lostRequest
	outcome outcome
	id      uuid.UUID
}

// recordCut keeps a request that resolved after a cut-off attempt.
func (p *plan) recordCut(c cutRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cuts = append(p.cuts, c)
}

// markMutated excludes a from the board and staging checks: the mixed phase changed it after its
// slot was counted (SQL still checks it); its hold as it was is kept for the refusal oracle.
func (p *plan) markMutated(a domain.Appointment) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mutated[a.ID] = true
	p.before = append(p.before, oHold{
		id: uuid.New(), bay: a.ServiceBayID, tech: a.TechnicianID, vehicle: a.VehicleID,
		status: int16(a.Status), window: timeWindow{a.StartAt, a.EndAt},
	})
}

// phantomHolds returns the mixed phase's pre-change holds.
func (p *plan) phantomHolds() []oHold {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.before)
}

// recordRefusal logs a 409/422 for job mix j (waiting or not) at start with vehicle, when
// availability offered that start to that mix at preparation (offered to a non-waiter: a waiter's
// options only add the soft upgrade); sent and at bracket the request (refusals.go: judged against
// the holds then).
func (p *plan) recordRefusal(j int, waiter bool, start time.Time, vehicle uuid.UUID, sent, at time.Time) {
	if j >= len(p.offeredAt) || !p.offeredAt[j][start.Unix()] {
		return
	}
	soft := softUpgrade(p.mixes[j].req, waiter, int(p.dealer.Policy.ExpressMaxMinutes))
	p.refusals.add(refusal{mix: p.mixes[j], soft: soft, start: start, vehicle: vehicle, sent: sent, at: at})
}

// hammerWaiterEvery: every that many-th hammer request is a waiter (isWaiter), so the soft upgrade
// is raced too (a waiter's general job may take a free express bay); the slot bounds then also
// count what availability offers a waiter.
const hammerWaiterEvery = 4

func hammerWaiter(n int) bool { return n%hammerWaiterEvery == hammerWaiterEvery-1 }

// slotIDs returns the ids created in slot i (sorted, so a run's choices are reproducible).
func (p *plan) slotIDs(i int) []uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []uuid.UUID
	for id := range p.created[p.slots[i].start.Unix()] {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	return ids
}

// createdIDs returns every appointment id clients were told about in this phase (its extra
// bookings included).
func (p *plan) createdIDs() []uuid.UUID {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := slices.Clone(p.extra)
	for _, slot := range p.created {
		for id := range slot {
			ids = append(ids, id)
		}
	}
	return ids
}

// phaseOutcome fails a phase that proved too little: no booking created at all (every request
// unresolved or refused passes the capacity and board checks vacuously), or more than
// maxUnresolvedPct percent of its requests left without a definite answer.
func phaseOutcome(created, unresolved, requests int, maxUnresolvedPct float64) error {
	if created == 0 {
		return fmt.Errorf("created no bookings (%d of %d requests unresolved)", unresolved, requests)
	}
	if requests > 0 && float64(unresolved)*100 > maxUnresolvedPct*float64(requests) {
		return fmt.Errorf("%d of %d requests unresolved (%.1f%%), over the %.1f%% limit (-max-unresolved-pct)",
			unresolved, requests, float64(unresolved)*100/float64(requests), maxUnresolvedPct)
	}
	return nil
}

type slotPlan struct {
	start      time.Time
	maxWinners int // never more bookings than this start can hold (oracle bound, bound.go) or vehicles
}

func (p *plan) record(start time.Time, id uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := start.Unix()
	if p.created[k] == nil {
		p.created[k] = map[uuid.UUID]bool{}
	}
	p.created[k][id] = true
}

// recordSent remembers the request that created id (its key and payload).
func (p *plan) recordSent(id uuid.UUID, req lostRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent == nil {
		p.sent = map[uuid.UUID]lostRequest{}
	}
	p.sent[id] = req
}

// sentFor returns the request that created id.
func (p *plan) sentFor(id uuid.UUID) (lostRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.sent[id]
	return r, ok
}

// resolve takes one unresolved request of the slot at start off the count (its replay settled it).
func (p *plan) resolve(start time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.unresolved[start.Unix()] > 0 {
		p.unresolved[start.Unix()]--
	}
}

func (p *plan) markUnresolved(req lostRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.unresolved[req.start.Unix()]++
	p.lost = append(p.lost, req)
}

func serviceByCode(c catalog, code string) (uuid.UUID, error) {
	for _, s := range c.services {
		if s.Code == code {
			return s.ID, nil
		}
	}
	return uuid.Nil, fmt.Errorf("service %s missing", code)
}

func dealerByPrefix(c catalog, prefix string) (domain.Dealership, error) {
	for _, d := range c.dealers {
		if strings.HasPrefix(d.Name, prefix) {
			return d, nil
		}
	}
	return domain.Dealership{}, fmt.Errorf("dealership %s… missing", prefix)
}

// preparePhase plans one hammer phase. river books its own provisioned shop (river: ensureWorkshop
// with riverWorkshop, which has no staging cap); north books the seeded Northside dealership. q
// (nil without -dsn) is where the slot bounds read the shop from (bound.go).
func preparePhase(ctx context.Context, api *apiClient, q querier, c catalog, name string, pool []uuid.UUID, s settings, river domain.Dealership) (*plan, error) {
	p := &plan{
		name: name, maxUnresolvedPct: s.maxUnresolvedPct, prepared: time.Now(), cutoff: time.Now().Add(s.startMargin),
		created: map[int64]map[uuid.UUID]bool{}, unresolved: map[int64]int{}, arrivals: map[int64]int{},
		mutated: map[uuid.UUID]bool{}, refusals: newRefusalLog(),
	}
	var specs [][]string
	var err error
	switch name {
	case "river": // no staging cap, one job line, never-overlapping slots: exact capacity check
		p.dealer = river
		specs = [][]string{{"OIL"}}
	case "north": // staging cap, single- and multi-line jobs into overlapping slots
		if p.dealer, err = dealerByPrefix(c, "Northside"); err != nil {
			return nil, err
		}
		if p.dealer, err = ensureExpressMax(ctx, api, p.dealer); err != nil {
			return nil, err
		}
		specs = [][]string{{"BRAKE"}, {"OIL", "BRAKE"}}
	default:
		return nil, fmt.Errorf("unknown phase (want river or north)")
	}
	for _, codes := range specs {
		mix, err := newJobMix(c, strings.Join(codes, "+"), codes...)
		if err != nil {
			return nil, err
		}
		p.mixes = append(p.mixes, mix)
		p.jobs = append(p.jobs, mix.ids)
	}
	if p.loc, err = time.LoadLocation(p.dealer.Timezone); err != nil {
		return nil, err
	}
	p.exact = name == "river"
	if p.exact && p.dealer.StagingCapacity != 0 {
		// Never degrade silently to upper bounds: the river shop is provisioned without a cap.
		return nil, fmt.Errorf("river needs a dealership without a staging cap for its exact check; %s has %d", p.dealer.Name, p.dealer.StagingCapacity)
	}
	// The first date with enough room after the cutoff: "today" runs out of starts in the
	// afternoon (London), and its next starts may be minutes away, so a fixed date — or the
	// earliest free start — would make the run fail by time of day. A date whose free starts all
	// lie before the cutoff rolls over to the next one.
	from := ""
	var pickErr error // why the last date tried had no slot to hammer
	for range maxDateRolls {
		if p.date, err = nextFreeDate(ctx, api, p.dealer.ID, pool[0], p.jobs[0], from); err != nil {
			return nil, err
		}
		avails := make([]domain.Availability, len(p.jobs))
		waiting := make([]domain.Availability, len(p.jobs)) // what a waiter is offered (soft upgrade)
		for i, job := range p.jobs {
			if avails[i], err = availability(ctx, api, p.dealer.ID, pool[0], job, p.date); err != nil {
				return nil, err
			}
			if waiting[i], err = availabilityAs(ctx, api, p.dealer.ID, pool[0], job, p.date, true); err != nil {
				return nil, err
			}
			p.maxJobMinutes = max(p.maxJobMinutes, avails[i].DurationMinutes)
		}
		if err = p.loadBoard(ctx, api); err != nil {
			return nil, err
		}
		var shop *oracleShop
		if shop, err = p.boundShop(ctx, q); err != nil {
			return nil, err
		}
		if pickErr = p.pickSlots(append(slices.Clone(avails), waiting...), shop, len(pool), s.slots); pickErr == nil {
			p.offeredAt = make([]map[int64]bool, len(avails))
			for j, a := range avails {
				p.offeredAt[j] = map[int64]bool{}
				for _, sl := range a.Slots {
					if sl.Available {
						p.offeredAt[j][sl.Start.Unix()] = true
					}
				}
			}
			p.moveTargets = moveTargets(avails[0], p.slots, p.maxJobMinutes, p.cutoff)
			return p, nil
		}
		if from, err = nextDate(p.date); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("no date with free starts after %s in %d tries: %w", p.cutoff.Format(time.RFC3339), maxDateRolls, pickErr)
}

// ensureExpressMax raises d's express-lane limit to workshopExpressMax when it is lower, so the
// hammer's waiters (every hammerWaiterEvery-th request) can soft-upgrade: Northside's jobs are
// BRAKE (90 min) and OIL+BRAKE (120 min), and the seeded default (60) would keep every one of them
// out of the quick lane, leaving the upgrade unraced. The change stays (idempotent across runs):
// the refusal oracle reads the limit from the database when it judges, after the phase.
func ensureExpressMax(ctx context.Context, api *apiClient, d domain.Dealership) (domain.Dealership, error) {
	if d.Policy.ExpressMaxMinutes >= workshopExpressMax {
		return d, nil
	}
	updated, err := getJSONWith[domain.Dealership](ctx, api, "PATCH", "/api/v1/admin/dealerships/"+d.ID.String(),
		map[string]any{"policy": map[string]any{"expressMaxMinutes": workshopExpressMax}},
		map[string]string{"If-Match": fmt.Sprintf("%q", fmt.Sprint(d.Version))})
	if err != nil {
		return d, fmt.Errorf("raise the express limit of %s to %d min (waiters' soft upgrade): %w", d.Name, workshopExpressMax, err)
	}
	fmt.Printf("phase north: %s express limit %d → %d min, so the hammer's waiters may soft-upgrade\n",
		d.Name, d.Policy.ExpressMaxMinutes, updated.Policy.ExpressMaxMinutes)
	return updated, nil
}

// maxDateRolls bounds how many dates preparePhase tries before giving up.
const maxDateRolls = 14

// nextDate is the civil date after date (YYYY-MM-DD).
func nextDate(date string) (string, error) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return "", fmt.Errorf("date %q: %w", date, err)
	}
	return d.AddDate(0, 0, 1).Format(time.DateOnly), nil
}

// moveTargets are avail's free starts at or after cutoff whose window (maxJob minutes, the
// longest job mix) overlaps no hammered slot's window: moving a booking there leaves every
// hammered slot's capacity as it was.
func moveTargets(avail domain.Availability, slots []slotPlan, maxJob int, cutoff time.Time) []time.Time {
	length := time.Duration(maxJob) * time.Minute
	var out []time.Time
	for _, s := range avail.Slots {
		if !s.Available || s.Start.Before(cutoff) {
			continue
		}
		free := true
		for _, h := range slots {
			if booking.Overlaps(booking.Interval{Start: s.Start, End: s.Start.Add(length)},
				booking.Interval{Start: h.start, End: h.start.Add(length)}) {
				free = false
				break
			}
		}
		if free {
			out = append(out, s.Start)
		}
	}
	return out
}

// pickSlots chooses the starts to hammer. Exact mode takes non-overlapping offered starts that no
// hold of the shop overlaps; otherwise consecutive starts where any job mix is offered (they
// overlap and share staging windows on purpose). avails are every view the requests may get (each
// job mix, waiting or not), all on the same start grid as avails[0]; a slot's bound is the
// oracle's (bound.go) on shop, capped by the vehicles.
func (p *plan) pickSlots(avails []domain.Availability, shop *oracleShop, vehicles, want int) error {
	variants := p.boundVariants(shop.expressMax)
	var lastEnd time.Time
	p.slots = nil
	for i, s := range avails[0].Slots {
		if len(p.slots) == want {
			break
		}
		if s.Start.Before(p.cutoff) { // would start (or be checked) too soon: see -start-margin
			continue
		}
		offered := false
		for _, a := range avails {
			if i < len(a.Slots) && a.Slots[i].Start.Equal(s.Start) && a.Slots[i].Available {
				offered = true
			}
		}
		bound, clean := shop.slotBound(variants, s.Start)
		bound = min(bound, vehicles)
		if !offered || bound == 0 || (p.exact && (!clean || s.Start.Before(lastEnd))) {
			continue
		}
		p.slots = append(p.slots, slotPlan{start: s.Start, maxWinners: bound})
		lastEnd = s.End
	}
	if len(p.slots) == 0 {
		return fmt.Errorf("no free slots on %s from %s", p.date, p.cutoff.In(p.loc).Format("15:04"))
	}
	return nil
}

// loadBoard snapshots the day's holding appointments: their ids (so the post-check ignores them)
// and arrivals per staging window (so the per-window bound accounts for them).
func (p *plan) loadBoard(ctx context.Context, api *apiClient) error {
	board, err := getJSON[domain.Schedule](ctx, api, "GET", p.schedulePath(), nil)
	if err != nil {
		return err
	}
	p.board = &board
	p.preexisting, p.arrivals = map[uuid.UUID]bool{}, map[int64]int{}
	p.bays, p.technicians = board.Bays, board.Technicians
	for _, a := range board.Appointments {
		if a.Status.HoldsResources() {
			p.preexisting[a.ID] = true
			from := arrivalWindowStart(a.StartAt, p.loc, int(p.dealer.StagingWindowMinutes))
			p.arrivals[from.Unix()]++
		}
	}
	return nil
}

func (p *plan) schedulePath() string {
	return fmt.Sprintf("/api/v1/schedule?dealershipId=%s&date=%s", p.dealer.ID, p.date)
}

func jobQuery(ids []uuid.UUID) string {
	if len(ids) == 1 {
		return "serviceTypeId=" + ids[0].String()
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return "serviceTypeIds=" + url.QueryEscape(strings.Join(parts, ","))
}

// nextFreeDate is the first date, from today or from (YYYY-MM-DD, when set), with a free start.
func nextFreeDate(ctx context.Context, api *apiClient, dealer, vehicle uuid.UUID, job []uuid.UUID, from string) (string, error) {
	path := fmt.Sprintf("/api/v1/availability/next?dealershipId=%s&vehicleId=%s&%s&days=14&limit=1", dealer, vehicle, jobQuery(job))
	if from != "" {
		path += "&from=" + from
	}
	next, err := getJSON[domain.NextAvailability](ctx, api, "GET", path, nil)
	if err != nil {
		return "", err
	}
	if len(next.Slots) == 0 {
		return "", fmt.Errorf("no free start in the next 14 days (searched %s–%s)", next.SearchedFrom, next.SearchedTo)
	}
	return next.Slots[0].Date, nil
}

func availability(ctx context.Context, api *apiClient, dealer, vehicle uuid.UUID, job []uuid.UUID, date string) (domain.Availability, error) {
	return availabilityAs(ctx, api, dealer, vehicle, job, date, false)
}

// availabilityAs is availability for a waiting customer (waiter) or not.
func availabilityAs(ctx context.Context, api *apiClient, dealer, vehicle uuid.UUID, job []uuid.UUID, date string, waiter bool) (domain.Availability, error) {
	path := fmt.Sprintf("/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s", dealer, vehicle, jobQuery(job), date)
	if waiter {
		path += "&isWaiter=true"
	}
	return getJSON[domain.Availability](ctx, api, "GET", path, nil)
}

// intendedStart is when request n of an open-loop run should be sent: n/rate seconds after t0.
func intendedStart(t0 time.Time, n int, rate float64) time.Time {
	return t0.Add(time.Duration(float64(n) * float64(time.Second) / rate))
}

// hammer fires concurrency requests at every slot of p through a bounded worker pool. Closed loop
// (s.rate == 0): a request goes out as soon as a worker is free. Open loop (s.rate > 0): request n
// is due at n/rate seconds after the phase starts, and its end-to-end latency counts from that
// intended time, so a saturated pool shows up as latency instead of a silently lower send rate.
func hammer(ctx context.Context, api *apiClient, p *plan, pool []uuid.UUID, s settings, runID string, stats *runStats) {
	sem := make(chan struct{}, s.workers)
	var wg sync.WaitGroup
	slotWG := make([]sync.WaitGroup, len(p.slots))
	n := 0
	t0 := time.Now()
	defer func() {
		wg.Wait()
		p.requests = n
		if p.slotDone != nil {
			close(p.slotDone)
		}
	}()
	for si, slot := range p.slots {
		if si > 0 && p.slotDone != nil { // the previous slot is fully dispatched
			wg.Go(func() {
				slotWG[si-1].Wait()
				p.slotDone <- si - 1
			})
		}
		// Closed loop: the slot's first wave (as many requests as there are workers) waits at a
		// gate and is released together once dispatched, so they truly race for the slot instead
		// of trickling in as workers are handed out.
		wave := min(s.workers, s.concurrency)
		gate := make(chan struct{})
		var openGate func()
		if s.rate > 0 {
			close(gate)
			openGate = func() {}
		} else {
			openGate = sync.OnceFunc(func() { close(gate) })
		}
		for i := range s.concurrency {
			if i == wave {
				openGate()
			}
			j := n % len(p.jobs)
			job := p.jobs[j]
			waiter := hammerWaiter(n)
			vehicle := pool[i%len(pool)]
			key := fmt.Sprintf("%s:%s:%d", runID, p.name, n)
			var origin time.Time
			if s.rate > 0 {
				origin = intendedStart(t0, n, s.rate)
				if wait := time.Until(origin); wait > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(wait):
					}
				}
			}
			select {
			case <-ctx.Done():
				openGate()
				return
			case sem <- struct{}{}:
			}
			n++
			slotWG[si].Add(1)
			wg.Go(func() {
				defer slotWG[si].Done()
				defer func() { <-sem }()
				<-gate
				payload := bookingPayload(p.dealer.ID, vehicle, job, slot.start)
				if waiter {
					payload["isWaiter"] = true
				}
				sent := time.Now()
				r := bookWithRetryFrom(ctx, api, payload, key, s.maxAttempts, stats, origin)
				answered := time.Now()
				req := lostRequest{key: key, payload: payload, start: slot.start, cut: r.cut}
				switch r.outcome {
				case outCreated, outReplayed:
					p.record(slot.start, r.id)
					p.recordSent(r.id, req)
				case outConflict, outUnavailable:
					p.recordRefusal(j, waiter, slot.start, vehicle, sent, answered)
				case outUnresolved:
					p.markUnresolved(req)
				}
				if r.cut && r.outcome != outUnresolved && r.outcome != outError {
					p.recordCut(cutRequest{lostRequest: req, outcome: r.outcome, id: r.id})
				}
			})
		}
		openGate()
	}
	if last := len(p.slots) - 1; last >= 0 && p.slotDone != nil {
		wg.Go(func() {
			slotWG[last].Wait()
			p.slotDone <- last
		})
	}
}

// check compares each slot's bookings with its capacity and the board, and returns the created ids
// plus notes for slots left short only because some requests never got a definite answer.
//
// In exact mode a slot short of its capacity with requests unresolved is judged by rows: counted
// in Postgres (slotRows: committed bookings whose answer was lost included) it must hold exactly
// its capacity; without a DSN (slotRows nil) the unresolved requests must at least cover the gap.
func (p *plan) check(ctx context.Context, api *apiClient, slotRows func(context.Context, time.Time, []uuid.UUID) (int, error)) (map[uuid.UUID]bool, []string, error) {
	all := map[uuid.UUID]bool{}
	var notes []string
	perWindow := map[int64]int{}
	totalUnresolved := 0
	for _, slot := range p.slots {
		k := slot.start.Unix()
		got, unresolved := len(p.created[k]), p.unresolved[k]
		totalUnresolved += unresolved
		label := slot.start.In(p.loc).Format("15:04")
		switch {
		case got > slot.maxWinners:
			return nil, nil, fmt.Errorf("slot %s: created=%d but only %d could fit (data integrity)", label, got, slot.maxWinners)
		case p.exact && got < slot.maxWinners && unresolved == 0:
			return nil, nil, fmt.Errorf("slot %s: created=%d want %d with every request answered (capacity lost)", label, got, slot.maxWinners)
		case p.exact && got < slot.maxWinners && slotRows != nil:
			known := make([]uuid.UUID, 0, got)
			for id := range p.created[k] {
				known = append(known, id)
			}
			rows, err := slotRows(ctx, slot.start, known)
			if err != nil {
				return nil, nil, err
			}
			if rows != slot.maxWinners {
				return nil, nil, fmt.Errorf("slot %s: %d rows in Postgres (created=%d heard, %d unresolved) want %d (capacity lost)", label, rows, got, unresolved, slot.maxWinners)
			}
			notes = append(notes, fmt.Sprintf("%s/%s created=%d/%d heard, %d rows in Postgres (answers lost to %d unresolved)", p.name, label, got, slot.maxWinners, rows, unresolved))
		case p.exact && got+unresolved < slot.maxWinners:
			return nil, nil, fmt.Errorf("slot %s: created=%d + %d unresolved < %d (capacity lost)", label, got, unresolved, slot.maxWinners)
		case p.exact && got < slot.maxWinners:
			notes = append(notes, fmt.Sprintf("%s/%s created=%d/%d unresolved=%d (no DSN to count rows)", p.name, label, got, slot.maxWinners, unresolved))
		}
		from := arrivalWindowStart(slot.start, p.loc, int(p.dealer.StagingWindowMinutes))
		for id := range p.created[k] {
			all[id] = true
			// A booking the mixed phase moved or cancelled no longer arrives here; its room may
			// have gone to a later request (SQL checks the windows as they are now).
			if !p.mutated[id] {
				perWindow[from.Unix()]++
			}
		}
	}
	if err := phaseOutcome(len(all), totalUnresolved, p.requests, p.maxUnresolvedPct); err != nil {
		return nil, nil, err
	}
	if p.dealer.StagingCapacity > 0 {
		windows := make([]int64, 0, len(perWindow))
		for w := range perWindow {
			windows = append(windows, w)
		}
		slices.Sort(windows)
		for _, w := range windows {
			room := int(p.dealer.StagingCapacity) - p.arrivals[w]
			label := time.Unix(w, 0).In(p.loc).Format("15:04")
			// No lower bound: overlapping slots of one run legitimately starve each other.
			if perWindow[w] > room {
				return nil, nil, fmt.Errorf("staging window %s: %d new arrivals, room for %d (data integrity)", label, perWindow[w], room)
			}
		}
	}
	return all, notes, p.checkBoard(ctx, api)
}

// checkBoard reads the day board back: every new holding appointment in a hammered slot was
// reported to some client as created (unless a request stayed unresolved), and every created one
// is on the board and readable by id. Bookings the mixed phase touched are left to SQL: they may
// have moved, changed resources, or been cancelled since.
func (p *plan) checkBoard(ctx context.Context, api *apiClient) error {
	board, err := getJSON[domain.Schedule](ctx, api, "GET", p.schedulePath(), nil)
	if err != nil {
		return err
	}
	for _, slot := range p.slots {
		k := slot.start.Unix()
		want := map[uuid.UUID]bool{}
		for id := range p.created[k] {
			if !p.mutated[id] {
				want[id] = true
			}
		}
		expected := maps.Clone(want)
		unknown := 0
		for _, a := range board.Appointments {
			if !a.Status.HoldsResources() || a.StartAt.Unix() != k || p.preexisting[a.ID] || p.mutated[a.ID] {
				continue
			}
			if !want[a.ID] {
				unknown++
				continue
			}
			delete(want, a.ID)
		}
		label := slot.start.In(p.loc).Format("15:04")
		if unknown > p.unresolved[k] {
			return fmt.Errorf("slot %s: %d holding appointments no client was told about (only %d requests unresolved)", label, unknown, p.unresolved[k])
		}
		if len(want) > 0 {
			return fmt.Errorf("slot %s: %d created appointments missing from the board", label, len(want))
		}
		for id := range expected {
			got, err := getJSON[domain.Appointment](ctx, api, "GET", "/api/v1/appointments/"+id.String(), nil)
			if err != nil {
				return fmt.Errorf("get appointment %s: %w", id, err)
			}
			if got.ID != id || !got.Status.HoldsResources() {
				return fmt.Errorf("get appointment inconsistent: id=%s status=%d", got.ID, got.Status)
			}
		}
	}
	return boardOverlaps(board.Appointments)
}

// boardOverlaps is the client-side double-booking check (the SQL pass repeats it in Postgres when
// a DSN is given): no two holding appointments overlap on a bay, technician, or vehicle.
func boardOverlaps(appts []domain.Appointment) error {
	var holding []domain.Appointment
	for _, a := range appts {
		if a.Status.HoldsResources() {
			holding = append(holding, a)
		}
	}
	for i := range holding {
		for j := i + 1; j < len(holding); j++ {
			a, b := holding[i], holding[j]
			if !booking.Overlaps(booking.Interval{Start: a.StartAt, End: a.EndAt}, booking.Interval{Start: b.StartAt, End: b.EndAt}) {
				continue
			}
			switch {
			case a.ServiceBayID == b.ServiceBayID:
				return fmt.Errorf("bay double-book: %s vs %s", a.ID, b.ID)
			case a.TechnicianID == b.TechnicianID:
				return fmt.Errorf("technician double-book: %s vs %s", a.ID, b.ID)
			case a.VehicleID == b.VehicleID:
				return fmt.Errorf("vehicle double-book: %s vs %s", a.ID, b.ID)
			}
		}
	}
	return nil
}
