package booking

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// AvailabilityQuery asks which starts on Day fit the job lines. Send ServiceTypeID for a single
// operation or ServiceTypeIDs for several.
type AvailabilityQuery struct {
	DealershipID   uuid.UUID
	VehicleID      uuid.UUID
	ServiceTypeID  uuid.UUID
	ServiceTypeIDs []uuid.UUID
	Day            Date
	IsWaiter       bool
	// WaiterGiven says IsWaiter was asked for. With ExcludeAppointmentID and no flag given, the
	// booked one applies, as Reschedule keeps it when its body leaves isWaiter out.
	WaiterGiven bool
	// ExcludeAppointmentID is the appointment being rescheduled: its own hold (bay, technician,
	// vehicle, staging arrival) is ignored, as Reschedule does. It must be an appointment of
	// VehicleID at DealershipID that the actor can access.
	ExcludeAppointmentID uuid.UUID
}

// NextQuery searches day by day from From for the first Limit bookable starts (PerDay at most
// from one day).
type NextQuery struct {
	AvailabilityQuery
	From  Date
	Days  int // how many days to scan (default 14, max 31; customers also stop at the horizon)
	Limit int // slots to return (default 5, max 20)
	// PerDay caps the starts taken from one day (0: no cap), so a short list spans several days
	// instead of repeating the first day's grid.
	PerDay int
	// PreferRecommended spends each day's share on its recommended starts (fit.go) first, then on
	// its other bookable ones, so a short limit goes to starts that leave no new idle gap. The list
	// stays in time order either way.
	PreferRecommended bool
}

const (
	defaultNextDays  = 14
	maxNextDays      = 31
	defaultNextLimit = 5
	maxNextLimit     = 20
	// nextFirstChunk is how many days NextAvailable reads first: most searches fill their limit in
	// the first days, so the rest of the range is read only when they do not.
	nextFirstChunk = 3
)

// ReasonDayTooShort is the day-level reason when an open day's hours are shorter than the job, so
// no start is offered (other days, e.g. a longer weekday override, may still fit it).
const ReasonDayTooShort = "day_too_short"

// availInputs is everything availability needs that does not change from day to day.
type availInputs struct {
	actor       domain.Actor
	dealer      domain.Dealership
	vehicle     domain.Vehicle
	req         Requirement
	loc         *time.Location
	softUpgrade bool
	exclude     uuid.UUID
	excluded    *domain.Appointment           // the appointment being rescheduled (nil = none)
	weekly      map[int16]domain.WeekdayHours // weekday overrides
}

// excludedHoldsBay reports whether the appointment being rescheduled currently holds bay over w.
// Its hold is left out of the slot's own checks, but a repair move lands before the reschedule's
// own move, so the move's target must be free of it too (store.freeForMovedSQL).
func (in availInputs) excludedHoldsBay(bay uuid.UUID, w Interval) bool {
	return in.excludedHoldsOver(w) && in.excluded.ServiceBayID == bay
}

// excludedHoldsTechnician is excludedHoldsBay for a technician.
func (in availInputs) excludedHoldsTechnician(tech uuid.UUID, w Interval) bool {
	return in.excludedHoldsOver(w) && in.excluded.TechnicianID == tech
}

func (in availInputs) excludedHoldsOver(w Interval) bool {
	x := in.excluded
	return x != nil && x.Status.HoldsResources() && Overlaps(Interval{Start: x.StartAt, End: x.EndAt}, w)
}

// availRange is the data for a run of days [first, last]: every active bay and technician plus
// one query per day-dependent kind (closures, holds, vehicle holds, staging arrivals, technician
// rosters), all read in one round trip (store.AvailabilityRange) and indexed once in Go
// (newAvailRange), so a multi-day search costs the same as a single day. Bays and technicians that
// do not fit the job are kept for the one-move repair (repair.go): they may take the booking a
// fitting one holds.
type availRange struct {
	bayIDs    []uuid.UUID
	bayNames  []string
	bayTypes  []domain.BayType
	bayFit    []bool     // per bayIDs[i]: fits the job (BayFits)
	bayHolds  []holdList // per bayIDs[i]
	techs     []domain.Technician
	techFit   []bool     // per techs[i]: qualified for the job
	techHolds []holdList // per techs[i]
	rosters   []roster   // per techs[i], clocks parsed once
	holds     []store.Hold
	closures  map[string]domain.Closure
	vehicle   holdList // the vehicle's holds at any dealership
	arrivals  map[int64]int
}

// newAvailRange indexes a range read: holds per bay and per technician (each sorted by start, so
// a day's holds are found by binary search), rosters parsed once per technician, staging
// arrivals counted per window.
func newAvailRange(in availInputs, data store.AvailabilityRange) (availRange, error) {
	rng := availRange{
		bayIDs: make([]uuid.UUID, len(data.Bays)), bayNames: make([]string, len(data.Bays)), bayTypes: make([]domain.BayType, len(data.Bays)), bayFit: make([]bool, len(data.Bays)),
		techs: data.Techs, techFit: make([]bool, len(data.Techs)), rosters: make([]roster, len(data.Techs)),
		holds: data.Busy, closures: make(map[string]domain.Closure, len(data.Closures)), arrivals: map[int64]int{},
	}
	bayIndex := make(map[uuid.UUID]int, len(data.Bays))
	for i, bay := range data.Bays {
		rng.bayIDs[i], rng.bayNames[i], rng.bayTypes[i] = bay.ID, bay.Name, bay.BayType
		rng.bayFit[i] = BayFits(bay.BayType, in.req.BayType, in.softUpgrade)
		bayIndex[bay.ID] = i
	}
	techIndex := make(map[uuid.UUID]int, len(data.Techs))
	for i, tech := range data.Techs {
		techIndex[tech.ID] = i
		rng.techFit[i] = qualifies(tech.SkillLevel, tech.Skills, in.req.Skills, in.req.MinSkillLevel)
		r, err := newRoster(data.Rosters.Shifts[tech.ID], data.Rosters.Absences[tech.ID])
		if err != nil {
			return availRange{}, err
		}
		rng.rosters[i] = r
	}
	rng.bayHolds = groupHolds(data.Busy, bayIndex, func(b store.Hold) uuid.UUID { return b.BayID })
	rng.techHolds = groupHolds(data.Busy, techIndex, func(b store.Hold) uuid.UUID { return b.TechnicianID })
	vehicle := make([]Interval, len(data.VehicleHolds))
	for i, row := range data.VehicleHolds {
		vehicle[i] = Interval{Start: row.Start, End: row.End}
	}
	rng.vehicle = newHoldList(vehicle, nil)
	for _, c := range data.Closures {
		rng.closures[c.Date] = c
	}
	for _, at := range data.Arrivals {
		window, _ := ArrivalWindow(at, in.loc, in.dealer.StagingWindowMinutes)
		rng.arrivals[window.Unix()]++
	}
	return rng, nil
}

func (s *Service) Availability(ctx context.Context, actor domain.Actor, q AvailabilityQuery) (domain.Availability, error) {
	in, err := s.availabilityInputs(ctx, actor, q)
	if err != nil {
		return domain.Availability{}, err
	}
	rng, err := s.loadRange(ctx, in, q.Day, q.Day)
	if err != nil {
		return domain.Availability{}, err
	}
	return dayAvailability(in, rng, q.Day, s.now())
}

// NextAvailable is the "first available appointment" search customers and advisors start from.
func (s *Service) NextAvailable(ctx context.Context, actor domain.Actor, q NextQuery) (domain.NextAvailability, error) {
	in, err := s.availabilityInputs(ctx, actor, q.AvailabilityQuery)
	if err != nil {
		return domain.NextAvailability{}, err
	}
	days := q.Days
	if days <= 0 {
		days = defaultNextDays
	}
	days = min(days, maxNextDays)
	limit := q.Limit
	if limit <= 0 {
		limit = defaultNextLimit
	}
	limit = min(limit, maxNextLimit)

	now := s.now()
	first := q.From
	if today := Today(in.loc, now); first.Before(today) {
		first = today
	}
	out := domain.NextAvailability{
		Timezone: in.dealer.Timezone, DurationMinutes: in.req.DurationMinutes,
		SearchedFrom: first.String(), SearchedTo: first.String(), Slots: []domain.NextSlot{},
	}
	// The days to scan: up to days, stopping at the customer's booking horizon.
	latest, capped := latestStart(in.actor, now, in.dealer.Policy)
	scan := make([]Date, 0, days)
	for day := first; len(scan) < days; day = day.AddDays(1) {
		if capped && day.Start(in.loc).After(latest) {
			break
		}
		scan = append(scan, day)
	}
	// The first days in one read; the rest, in a second, only when they do not fill the limit. Each
	// day's answer depends only on that day's data, so the split never changes the result.
	split := min(len(scan), nextFirstChunk)
	for _, chunk := range [][]Date{scan[:split], scan[split:]} {
		if len(chunk) == 0 || len(out.Slots) == limit {
			break
		}
		rng, err := s.loadRange(ctx, in, chunk[0], chunk[len(chunk)-1])
		if err != nil {
			return domain.NextAvailability{}, err
		}
		for _, day := range chunk {
			if len(out.Slots) == limit {
				break
			}
			out.SearchedTo = day.String()
			avail, err := dayAvailability(in, rng, day, now)
			if err != nil {
				return domain.NextAvailability{}, err
			}
			take := limit - len(out.Slots)
			if q.PerDay > 0 {
				take = min(take, q.PerDay)
			}
			out.Slots = appendNextSlots(out.Slots, day, avail.Slots, take, q.PreferRecommended)
		}
	}
	return out, nil
}

// appendNextSlots appends up to take of day's bookable slots to out, in time order: the earliest
// ones, or with preferRecommended the earliest recommended ones topped up with the earliest others.
func appendNextSlots(out []domain.NextSlot, day Date, slots []domain.Slot, take int, preferRecommended bool) []domain.NextSlot {
	picked := make([]bool, len(slots))
	passes := []func(domain.Slot) bool{func(domain.Slot) bool { return true }}
	if preferRecommended {
		passes = []func(domain.Slot) bool{
			func(sl domain.Slot) bool { return sl.Recommended },
			func(sl domain.Slot) bool { return !sl.Recommended },
		}
	}
	for _, want := range passes {
		for i, slot := range slots {
			if take == 0 {
				break
			}
			if slot.Available && want(slot) {
				picked[i] = true
				take--
			}
		}
	}
	for i, slot := range slots {
		if !picked[i] {
			continue
		}
		out = append(out, domain.NextSlot{
			Date: day.String(), Start: slot.Start, End: slot.End,
			FreeBays: slot.FreeBays, FreeTechnicians: slot.FreeTechnicians, Recommended: slot.Recommended,
		})
	}
	return out
}

// availabilityInputs reads what sizes the search in one round trip (store.AvailabilityBase) and
// then applies the checks in the same order as one read at a time would: dealership (exists, in
// scope), vehicle (exists, the customer's own), the excluded appointment, the job lines.
func (s *Service) availabilityInputs(ctx context.Context, actor domain.Actor, q AvailabilityQuery) (availInputs, error) {
	ids, err := serviceIDs(q.ServiceTypeID, q.ServiceTypeIDs)
	if err != nil {
		return availInputs{}, err
	}
	base, err := s.store.AvailabilityBase(ctx, q.DealershipID, q.VehicleID, q.ExcludeAppointmentID, ids)
	if err != nil {
		return availInputs{}, err
	}
	if base.Dealer == nil {
		return availInputs{}, domain.NewError(domain.CodeNotFound, "dealership not found")
	}
	dealer := *base.Dealer
	if !actor.CanAccessDealership(dealer.ID) {
		return availInputs{}, otherDealership()
	}
	if base.Vehicle == nil || !actor.CanAccessCustomer(base.Vehicle.CustomerID) {
		return availInputs{}, notFoundVehicle()
	}
	vehicle := *base.Vehicle
	var req Requirement
	isWaiter := q.IsWaiter
	if q.ExcludeAppointmentID != uuid.Nil {
		if err := checkExclude(actor, base.Exclude, dealer, vehicle.ID, s.now()); err != nil {
			return availInputs{}, err
		}
		// Reschedule keeps the booked snapshot (lines, labour time, bay type, skills, level), so the
		// grid is computed for it, never for the catalog as edited since: otherwise it would hide
		// starts the move accepts and offer starts it refuses. The lines asked for must be the booked
		// ones, in order (a different job is a new booking, not a move).
		if !slices.Equal(ids, lineServiceIDs(*base.Exclude)) {
			return availInputs{}, domain.NewError(domain.CodeInvalid,
				"with excludeAppointmentId, the service types must be that appointment's booked job lines, in order")
		}
		req = bookedRequirement(*base.Exclude)
		if !q.WaiterGiven {
			isWaiter = base.Exclude.IsWaiter
		}
	} else {
		if base.Services == nil {
			return availInputs{}, domain.NewError(domain.CodeNotFound, "service type not found")
		}
		if req, err = Combine(base.Services); err != nil {
			return availInputs{}, err
		}
	}
	loc, err := loadLocation(dealer.Timezone)
	if err != nil {
		return availInputs{}, err
	}
	weekly := make(map[int16]domain.WeekdayHours, len(base.Weekly))
	for _, h := range base.Weekly {
		weekly[h.Weekday] = h
	}
	// No weekday can ever hold a job longer than this (a single short day is a 200 with
	// ReasonDayTooShort).
	if err := fitsSomeWeekday(dealer, weekly, req.DurationMinutes); err != nil {
		return availInputs{}, err
	}
	return availInputs{
		actor: actor, dealer: dealer, vehicle: vehicle, req: req, loc: loc,
		softUpgrade: req.SoftUpgrade(isWaiter, dealer.Policy),
		exclude:     q.ExcludeAppointmentID, excluded: base.Exclude, weekly: weekly,
	}, nil
}

// checkExclude authorises excluding an appointment's own hold: the actor must be able to see it
// (404 otherwise, like every other appointment read) and it must belong to the queried
// dealership and vehicle, so a reschedule view cannot free someone else's resources. It then
// applies Reschedule's own preconditions (confirmed, and for a customer the online change cutoff),
// so the reschedule view never offers starts that Reschedule would refuse anyway.
func checkExclude(actor domain.Actor, appt *domain.Appointment, dealer domain.Dealership, vehicleID uuid.UUID, now time.Time) error {
	if appt == nil || !actor.CanAccessAppointment(*appt) {
		return notFoundAppointment()
	}
	if appt.DealershipID != dealer.ID || appt.VehicleID != vehicleID {
		return domain.NewError(domain.CodeInvalid, "excludeAppointmentId must be an appointment of this vehicle at this dealership")
	}
	if appt.Status != domain.StatusConfirmed {
		return domain.NewError(domain.CodeConflict,
			"only confirmed appointments can be rescheduled (current: "+appt.Status.Name()+")")
	}
	return customerMayChange(actor, *appt, now, dealer.Policy)
}

// loadRange reads the candidates and the day-dependent data for every day in [first, last] in one
// round trip. Holds and absences are read for the whole local-day span; staging arrivals one
// window past it, so every arrival window a slot can fall into is complete.
func (s *Service) loadRange(ctx context.Context, in availInputs, first, last Date) (availRange, error) {
	from, to := first.Start(in.loc), last.Next(in.loc)
	q := store.RangeQuery{
		DealershipID: in.dealer.ID, VehicleID: in.vehicle.ID, Exclude: in.exclude,
		FromDay: first.String(), ToDay: last.String(), From: from, To: to,
	}
	if in.dealer.StagingCapacity > 0 {
		windowStart, windowEnd := ArrivalWindow(from, in.loc, in.dealer.StagingWindowMinutes)
		q.StagingTo = to.Add(windowEnd.Sub(windowStart))
	}
	data, err := s.store.AvailabilityRange(ctx, q)
	if err != nil {
		return availRange{}, err
	}
	return newAvailRange(in, data)
}

// hoursOn applies the calendar to day from the preloaded weekday overrides and closures (the
// same rules as store.DayRules + ResolveDay, without a query per day).
func (in availInputs) hoursOn(rng availRange, day Date) DayHours {
	var rules store.DayRules
	if c, ok := rng.closures[day.String()]; ok {
		rules.Closure = &c
	}
	if h, ok := in.weekly[day.ISOWeekday()]; ok {
		rules.Override = &h
	}
	return ResolveDay(in.dealer, day, rules)
}

// dayAvailability evaluates every grid start of day. An open day shorter than the job has no
// slots and Reason ReasonDayTooShort.
func dayAvailability(in availInputs, rng availRange, day Date, now time.Time) (domain.Availability, error) {
	result := domain.Availability{
		Date: day.String(), Timezone: in.dealer.Timezone, DurationMinutes: in.req.DurationMinutes, Slots: []domain.Slot{},
	}
	hours := in.hoursOn(rng, day)
	if hours.Closed {
		result.Closed, result.ClosedReason = true, hours.Reason
		return result, nil
	}
	clocks, err := hours.clocks()
	if err != nil {
		return domain.Availability{}, err
	}
	slots := windows(clocks, in.req.DurationMinutes, day, in.loc)
	if len(slots) == 0 {
		result.Reason = ReasonDayTooShort
		return result, nil
	}
	// Narrow every sorted hold list of a fitting resource to the day's span once; slots then scan
	// only those (the one-move repair, rarely needed, searches the full lists).
	from, to := slots[0].Start, slots[len(slots)-1].End
	vehicleBusy := rng.vehicle.overlapping(from, to)
	bayBusy := make([][]Interval, len(rng.bayHolds))
	for i, l := range rng.bayHolds {
		if rng.bayFit[i] {
			bayBusy[i] = l.overlapping(from, to)
		}
	}
	techBusy := make([][]Interval, len(rng.techHolds))
	for i, l := range rng.techHolds {
		if rng.techFit[i] {
			techBusy[i] = l.overlapping(from, to)
		}
	}

	var loads []int // the technicians' booked minutes that day, read once a slot needs them (fit.go)
	result.Slots = make([]domain.Slot, 0, len(slots))
	for _, slot := range slots {
		local := localOf(slot, in.loc)
		reason := tooEarly(in.actor, slot.Start, now, in.dealer.Policy)
		switch {
		case reason != "":
		case clocks.crossesLunch(local):
			reason = "lunch_break"
		case busyAt(vehicleBusy, slot):
			reason = ReasonVehicleBusy
		}
		if reason != "" {
			result.Slots = append(result.Slots, domain.Slot{Start: slot.Start, End: slot.End, Reason: reason})
			continue
		}
		freeBays := 0
		for i, busy := range bayBusy {
			if rng.bayFit[i] && !busyAt(busy, slot) {
				freeBays++
			}
		}
		freeTechs := 0
		for i, busy := range techBusy {
			if rng.techFit[i] && !busyAt(busy, slot) && rng.rosters[i].onDuty(slot, local) {
				freeTechs++
			}
		}
		// With nothing free, the slot is still offered when one confirmed booking can move to make
		// room (repair.go), as the booking will do; the counts stay what is free right now.
		bayOK := freeBays > 0 || rng.bayRepairable(in, slot, now)
		techOK := freeTechs > 0 || rng.techRepairable(in, slot, local, now)
		ok, reason := Classify(bayOK, techOK)
		if ok && in.dealer.StagingCapacity > 0 {
			window, _ := ArrivalWindow(slot.Start, in.loc, in.dealer.StagingWindowMinutes)
			if rng.arrivals[window.Unix()] >= int(in.dealer.StagingCapacity) {
				ok, reason = false, "staging_full"
			}
		}
		recommended := false
		if ok && freeBays > 0 && freeTechs > 0 {
			if loads == nil {
				loads = rng.techLoads(day.Start(in.loc), day.Next(in.loc))
			}
			lo, hi := clocks.stretch(slot, day, in.loc)
			recommended = rng.recommended(in, loads, slot, local, lo, hi)
		}
		result.Slots = append(result.Slots, domain.Slot{
			Start: slot.Start, End: slot.End, Available: ok,
			FreeBays: freeBays, FreeTechnicians: freeTechs, Reason: reason, Recommended: recommended,
		})
	}
	return result, nil
}

// groupHolds splits rows into one holdList per resource of index (rows of other resources are
// dropped), carving every list out of one backing array sized in a first pass. Each hold keeps its
// row's index (holdList.refs), so the repair can read what the booking behind it requires.
func groupHolds(rows []store.Hold, index map[uuid.UUID]int, key func(store.Hold) uuid.UUID) []holdList {
	owner := make([]int, len(rows))
	counts := make([]int, len(index))
	kept := 0
	for i, row := range rows {
		owner[i] = -1
		if j, ok := index[key(row)]; ok {
			owner[i] = j
			counts[j]++
			kept++
		}
	}
	backing := make([]Interval, kept)
	backingRefs := make([]int, kept)
	lists := make([][]Interval, len(index))
	refs := make([][]int, len(index))
	off := 0
	for j, n := range counts {
		lists[j] = backing[off : off : off+n]
		refs[j] = backingRefs[off : off : off+n]
		off += n
	}
	for i, row := range rows {
		if j := owner[i]; j >= 0 {
			lists[j] = append(lists[j], Interval{Start: row.Start, End: row.End})
			refs[j] = append(refs[j], i)
		}
	}
	out := make([]holdList, len(lists))
	for j, l := range lists {
		out[j] = newHoldList(l, refs[j])
	}
	return out
}

// holdList is a resource's holds sorted by start, with the longest one's length: a hold overlaps
// [from, to) only if it starts after from - maxSpan, which bounds the binary search in overlapping.
// refs, when set, is each hold's row in availRange.holds.
type holdList struct {
	holds   []Interval
	refs    []int
	maxSpan time.Duration
}

func newHoldList(holds []Interval, refs []int) holdList {
	if !slices.IsSortedFunc(holds, compareStart) {
		order := make([]int, len(holds))
		for i := range order {
			order[i] = i
		}
		slices.SortStableFunc(order, func(a, b int) int { return compareStart(holds[a], holds[b]) })
		sorted := make([]Interval, len(holds))
		var sortedRefs []int
		if refs != nil {
			sortedRefs = make([]int, len(refs))
		}
		for k, i := range order {
			sorted[k] = holds[i]
			if refs != nil {
				sortedRefs[k] = refs[i]
			}
		}
		holds, refs = sorted, sortedRefs
	}
	l := holdList{holds: holds, refs: refs}
	for _, h := range holds {
		l.maxSpan = max(l.maxSpan, h.End.Sub(h.Start))
	}
	return l
}

// overlapping is the run of holds that may overlap [from, to) (half-open): every one that does,
// found by two binary searches without copying, plus at most a few that end before from (a
// neighbour shorter than the longest hold), which busyAt skips as it checks each slot anyway.
func (l holdList) overlapping(from, to time.Time) []Interval {
	lo, hi := l.span(from, to)
	return l.holds[lo:hi]
}

// span is overlapping's run as indexes into holds (and refs).
func (l holdList) span(from, to time.Time) (lo, hi int) {
	after := from.Add(-l.maxSpan)
	lo, _ = slices.BinarySearchFunc(l.holds, after, func(h Interval, t time.Time) int {
		if h.Start.After(t) {
			return 1
		}
		return -1
	})
	hi, _ = slices.BinarySearchFunc(l.holds, to, func(h Interval, t time.Time) int {
		if h.Start.Before(t) {
			return -1
		}
		return 1
	})
	return lo, max(lo, hi)
}

// busy reports whether any hold overlaps w.
func (l holdList) busy(w Interval) bool {
	return busyAt(l.overlapping(w.Start, w.End), w)
}

func compareStart(a, b Interval) int { return a.Start.Compare(b.Start) }

// onSlotGrid reports whether start (to the minute, like MatchingSlot) sits on the day's slot grid,
// which is anchored at opening time, whether or not it falls inside the opening hours.
func onSlotGrid(hours DayHours, day Date, loc *time.Location, start time.Time) bool {
	open, err := ParseClock(hours.OpenTime)
	if err != nil {
		return false
	}
	return start.Truncate(time.Minute).Sub(open.On(day, loc))%SlotStep == 0
}

// windows lists the job-length slots inside that day's opening hours (lunch not applied): the
// grid from opening time, each kept only if its local wall clocks are within the hours the way
// Tx.refuseStranded reads them (dayClocks.withinHours), so on a clock-change day Go never offers or
// books a window the stranded check would call outside the hours.
func windows(c dayClocks, durationMinutes int, day Date, loc *time.Location) []Interval {
	all := GenerateSlots(day, loc, c.openClock, c.closeClock, time.Duration(durationMinutes)*time.Minute, SlotStep)
	kept := all[:0]
	for _, slot := range all {
		if c.withinHours(localOf(slot, loc)) {
			kept = append(kept, slot)
		}
	}
	return kept
}
