package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The oracle answers "could this job start here?" straight from the tables, independently of the
// API's availability code (Go, in-memory bucketing) and of the allocator (ranked candidates under
// locks). It reads only raw rows — the dealership's active bays, its active technicians with
// their skills, shift patterns and absences, and the holding appointments (status 1–3) of the
// day — and applies the documented rules (rules.go) in Go, the calendar's too (clock parsing, the
// 30-minute grid, local instants via time.Date, staging windows: never booking.ParseClock,
// Clock.On, SlotStep or ArrivalWindow, so a calendar bug shared with the engine cannot hide): for each start it counts the bays of a
// fitting type free for the whole window and the technicians holding every required skill at the
// level, on duty and free for the whole window. A start is feasible when both are available — free,
// or freed by the documented one-move repair (repair.go) —, the window lies inside the opening
// hours without touching lunch, and its arrival window has staging room. The stress workshops have no closures or weekday overrides, so those calendar
// rules are not modelled (a hammered start's calendar is vouched for by the availability that
// offered it).

// oracleSlot is the oracle's view of one start.
type oracleSlot struct {
	start      time.Time
	bays       int
	techs      int
	lunch      bool // the window touches lunch (never bookable)
	inHours    bool // inside the opening hours
	staging    bool // its arrival window has room (always true without a staging cap)
	feasibleAt bool // bays > 0 && techs > 0 && !lunch && inHours && staging
}

// jobMix is a named job (one or more lines) and its combined requirement.
type jobMix struct {
	name string
	ids  []uuid.UUID
	req  jobRequirement
}

// newJobMix combines the catalog's service types into one requirement (combineLines, the
// documented rule; the SQL integrity pass re-derives it once more from the booked lines).
func newJobMix(c catalog, name string, codes ...string) (jobMix, error) {
	m := jobMix{name: name}
	var services []domain.ServiceType
	for _, code := range codes {
		i := -1
		for j, st := range c.services {
			if st.Code == code {
				i = j
			}
		}
		if i < 0 {
			return jobMix{}, fmt.Errorf("service %s missing", code)
		}
		services = append(services, c.services[i])
		m.ids = append(m.ids, c.services[i].ID)
	}
	req, err := combineLines(services)
	if err != nil {
		return jobMix{}, err
	}
	m.req = req
	return m, nil
}

// dayStarts lists every start of day from opening time, slotStep apart in elapsed time (so a day
// whose clocks change between opening and closing has one start fewer or more per half hour
// skipped or repeated, never a duplicate), whose window of minutes ends by closing time, flagging
// those that touch lunch. Clocks are parsed and placed here (rules.go), never by the engine.
func dayStarts(w workshop, day booking.Date, minutes int) ([]oracleSlot, error) {
	open, err := parseClockMinutes(w.dealer.OpenTime)
	if err != nil {
		return nil, err
	}
	closeAt, err := parseClockMinutes(w.dealer.CloseTime)
	if err != nil {
		return nil, err
	}
	var lunchFrom, lunchTo time.Time
	if w.dealer.LunchStart != "" {
		ls, err1 := parseClockMinutes(w.dealer.LunchStart)
		le, err2 := parseClockMinutes(w.dealer.LunchEnd)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("lunch %s–%s", w.dealer.LunchStart, w.dealer.LunchEnd)
		}
		lunchFrom, lunchTo = ls.on(day, w.loc), le.on(day, w.loc)
	}
	length := time.Duration(minutes) * time.Minute
	var out []oracleSlot
	for t := open.on(day, w.loc); !t.Add(length).After(closeAt.on(day, w.loc)); t = t.Add(slotStep) {
		lunch := !lunchFrom.IsZero() && t.Before(lunchTo) && lunchFrom.Before(t.Add(length))
		out = append(out, oracleSlot{start: t, lunch: lunch, inHours: true})
	}
	return out, nil
}

// oBay, oTech and oHold are the raw rows the oracle reasons over (an oHold's window is the time it
// holds its bay and technician: a flagged overrun's runs until it is completed, overrunOpenEnd).
type oBay struct {
	id      uuid.UUID
	bayType domain.BayType
}

type oTech struct {
	id     uuid.UUID
	level  int16
	skills map[domain.TechnicianSkill]bool
	roster techRoster
}

type oHold struct {
	id, bay, tech, vehicle uuid.UUID
	status                 int16
	window                 timeWindow
	// req and waiter are the hold's booked requirement (where the one-move repair may put it,
	// repair.go); phantom marks a position it had before, not where it is (never moved again).
	req     jobRequirement
	waiter  bool
	phantom bool
	// overrun: the window was widened by overrunHold (a flagged overrun holding until completed), which
	// appointment_history's raw positions do not show.
	overrun bool
}

// oracleShop is one dealership's resources and the holds around a stretch of time.
type oracleShop struct {
	loc           *time.Location
	now           time.Time          // when it was read: only holds starting later may move (repair.go)
	pinned        map[uuid.UUID]bool // holds changed during the phase being judged: never counted as movable
	stagingCap    int
	stagingWindow int
	expressMax    int
	bays          []oBay
	techs         []oTech
	holds         []oHold
	// vehicleHolds are the holds of the vehicles asked for (loadVehicleHolds), at any dealership.
	vehicleHolds map[uuid.UUID][]oHold
}

// loadShop reads dealer's active bays and technicians (skills, shifts, absences around [from, to))
// and its holding appointments overlapping [from, to).
func loadShop(ctx context.Context, q querier, dealer uuid.UUID, from, to time.Time) (*oracleShop, error) {
	s := &oracleShop{vehicleHolds: map[uuid.UUID][]oHold{}, now: time.Now(), pinned: map[uuid.UUID]bool{}}
	var tz string
	var staging, window, express int16
	if err := queryOne(ctx, q, `
		SELECT timezone, staging_capacity, staging_window_minutes, express_max_minutes
		FROM dealerships WHERE id = $1`, []any{dealer}, &tz, &staging, &window, &express); err != nil {
		return nil, fmt.Errorf("oracle: dealership: %w", err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	s.loc, s.stagingCap, s.stagingWindow, s.expressMax = loc, int(staging), int(window), int(express)

	rows, err := q.Query(ctx, `SELECT id, bay_type FROM service_bays WHERE dealership_id = $1 AND active ORDER BY id`, dealer)
	if err != nil {
		return nil, fmt.Errorf("oracle: bays: %w", err)
	}
	if s.bays, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (oBay, error) {
		var b oBay
		var t int16
		err := row.Scan(&b.id, &t)
		b.bayType = domain.BayType(t)
		return b, err
	}); err != nil {
		return nil, fmt.Errorf("oracle: bays: %w", err)
	}

	rows, err = q.Query(ctx, `
		SELECT t.id, t.skill_level, coalesce(array_agg(sk.skill) FILTER (WHERE sk.skill IS NOT NULL), '{}')
		FROM technicians t LEFT JOIN technician_skills sk ON sk.technician_id = t.id
		WHERE t.dealership_id = $1 AND t.active
		GROUP BY t.id, t.skill_level ORDER BY t.id`, dealer)
	if err != nil {
		return nil, fmt.Errorf("oracle: technicians: %w", err)
	}
	if s.techs, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (oTech, error) {
		t := oTech{skills: map[domain.TechnicianSkill]bool{}, roster: techRoster{shifts: map[int]shiftDef{}}}
		var skills []int16
		err := row.Scan(&t.id, &t.level, &skills)
		for _, sk := range skills {
			t.skills[domain.TechnicianSkill(sk)] = true
		}
		return t, err
	}); err != nil {
		return nil, fmt.Errorf("oracle: technicians: %w", err)
	}
	if err := s.loadRosters(ctx, q, dealer, from, to); err != nil {
		return nil, err
	}

	// A flagged overrun (in progress, overrun_at set) holds until it is completed, so it is read
	// whatever its end (there is no lower bound on start_at here either).
	rows, err = q.Query(ctx, `
		SELECT id, service_bay_id, technician_id, vehicle_id, status, start_at, end_at, overrun_at IS NOT NULL,
		       required_bay_type, required_skills, min_skill_level, booked_duration_minutes, is_waiter
		FROM appointments
		WHERE dealership_id = $1 AND status IN (`+holdingSQL+`) AND start_at < $3
		  AND ($2 < end_at OR (status = `+statusInProgressSQL+` AND overrun_at IS NOT NULL))`, dealer, from, to)
	if err != nil {
		return nil, fmt.Errorf("oracle: holds: %w", err)
	}
	if s.holds, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (oHold, error) {
		var h oHold
		var flagged bool
		var bayType int16
		var skills []int16
		err := row.Scan(&h.id, &h.bay, &h.tech, &h.vehicle, &h.status, &h.window.start, &h.window.end, &flagged,
			&bayType, &skills, &h.req.minLevel, &h.req.minutes, &h.waiter)
		raw := h.window
		h.window = overrunHold(h.window, h.status, flagged)
		h.overrun = h.window != raw
		h.req.bayType = domain.BayType(bayType)
		for _, sk := range skills {
			h.req.skills = append(h.req.skills, domain.TechnicianSkill(sk))
		}
		return h, err
	}); err != nil {
		return nil, fmt.Errorf("oracle: holds: %w", err)
	}
	return s, nil
}

// overrunOpenEnd stands for "until completed": later than any window the oracle is asked about.
var overrunOpenEnd = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// overrunHold is the documented overrun rule (Assumption 3, as the overrun sweep applies it): an
// in-progress job flagged as overrunning (overrun_at set: the next booking was in the way, or the
// extension cap was reached) holds its bay and technician until it is completed — a carry-over
// job, across days if need be — not just until its current end_at, because nobody knows when it
// will really finish. The car's own hold is not widened (only bay and technician are).
func overrunHold(w timeWindow, status int16, flagged bool) timeWindow {
	if status != int16(domain.StatusInProgress) || !flagged {
		return w
	}
	w.end = overrunOpenEnd
	return w
}

// loadRosters fills each technician's shift pattern and the absences touching [from, to).
func (s *oracleShop) loadRosters(ctx context.Context, q querier, dealer uuid.UUID, from, to time.Time) error {
	byID := map[uuid.UUID]*oTech{}
	for i := range s.techs {
		byID[s.techs[i].id] = &s.techs[i]
	}
	rows, err := q.Query(ctx, `
		SELECT p.technician_id, p.weekday, to_char(p.start_time, 'HH24:MI'), to_char(p.end_time, 'HH24:MI'),
		       coalesce(to_char(p.break_start, 'HH24:MI'), ''), coalesce(to_char(p.break_end, 'HH24:MI'), '')
		FROM technician_shifts p JOIN technicians t ON t.id = p.technician_id
		WHERE t.dealership_id = $1`, dealer)
	if err != nil {
		return fmt.Errorf("oracle: shifts: %w", err)
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id uuid.UUID
		var weekday int16
		var start, end, bs, be string
		if err := row.Scan(&id, &weekday, &start, &end, &bs, &be); err != nil {
			return struct{}{}, err
		}
		sh, err := parseShift(start, end, bs, be)
		if err != nil {
			return struct{}{}, err
		}
		if t := byID[id]; t != nil {
			t.roster.shifts[int(weekday)] = sh
		}
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("oracle: shifts: %w", err)
	}
	rows, err = q.Query(ctx, `
		SELECT ab.technician_id, ab.starts_at, ab.ends_at
		FROM technician_absences ab JOIN technicians t ON t.id = ab.technician_id
		WHERE t.dealership_id = $1 AND ab.starts_at < $3 AND $2 < ab.ends_at`, dealer, from, to)
	if err != nil {
		return fmt.Errorf("oracle: absences: %w", err)
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id uuid.UUID
		var w timeWindow
		if err := row.Scan(&id, &w.start, &w.end); err != nil {
			return struct{}{}, err
		}
		if t := byID[id]; t != nil {
			t.roster.absences = append(t.roster.absences, w)
		}
		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("oracle: absences: %w", err)
	}
	return nil
}

// parseShift builds a shift from its "HH:MM" clocks (empty break clocks: no break).
func parseShift(start, end, breakStart, breakEnd string) (shiftDef, error) {
	var sh shiftDef
	var err error
	if sh.start, err = parseClockMinutes(start); err != nil {
		return sh, err
	}
	if sh.end, err = parseClockMinutes(end); err != nil {
		return sh, err
	}
	if breakStart == "" {
		return sh, nil
	}
	sh.hasBreak = true
	if sh.breakStart, err = parseClockMinutes(breakStart); err != nil {
		return sh, err
	}
	sh.breakEnd, err = parseClockMinutes(breakEnd)
	return sh, err
}

// loadVehicleHolds reads every holding appointment of vehicles overlapping [from, to), at any
// dealership (the vehicle GiST constraint is global).
func (s *oracleShop) loadVehicleHolds(ctx context.Context, q querier, vehicles []uuid.UUID, from, to time.Time) error {
	rows, err := q.Query(ctx, `
		SELECT id, service_bay_id, technician_id, vehicle_id, status, start_at, end_at
		FROM appointments
		WHERE vehicle_id = ANY($1) AND status IN (`+holdingSQL+`) AND start_at < $3 AND $2 < end_at`, vehicles, from, to)
	if err != nil {
		return fmt.Errorf("oracle: vehicle holds: %w", err)
	}
	holds, err := pgx.CollectRows(rows, scanHold)
	if err != nil {
		return fmt.Errorf("oracle: vehicle holds: %w", err)
	}
	for _, h := range holds {
		s.vehicleHolds[h.vehicle] = append(s.vehicleHolds[h.vehicle], h)
	}
	return nil
}

func scanHold(row pgx.CollectableRow) (oHold, error) {
	var h oHold
	err := row.Scan(&h.id, &h.bay, &h.tech, &h.vehicle, &h.status, &h.window.start, &h.window.end)
	return h, err
}

// queryOne scans the single row of sql into dst.
func queryOne(ctx context.Context, q querier, sql string, args []any, dst ...any) error {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	if err := rows.Scan(dst...); err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// counts is how many fitting bays and qualified, on-duty technicians are free for the whole of w,
// ignoring the hold exclude (an appointment that came later than the question being judged).
func (s *oracleShop) counts(r jobRequirement, soft bool, w timeWindow, exclude uuid.UUID) (bays, techs int) {
	busyBay, busyTech := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for _, h := range s.holds {
		if h.id != exclude && h.window.overlaps(w) {
			busyBay[h.bay], busyTech[h.tech] = true, true
		}
	}
	for _, b := range s.bays {
		if !busyBay[b.id] && bayFits(r, soft, b.bayType) {
			bays++
		}
	}
	for _, t := range s.techs {
		if !busyTech[t.id] && qualified(r, t.level, t.skills) && t.roster.onDuty(w, s.loc) {
			techs++
		}
	}
	return bays, techs
}

// stagingRoom reports whether the arrival window of start (stagingWindow minutes, aligned to the
// local hour, arrivalWindowStart) holds fewer than stagingCap confirmed or checked-in arrivals, exclude left out.
func (s *oracleShop) stagingRoom(start time.Time, exclude uuid.UUID) bool {
	if s.stagingCap <= 0 || s.stagingWindow <= 0 {
		return true
	}
	key := func(t time.Time) int64 { return arrivalWindowStart(t, s.loc, s.stagingWindow).Unix() }
	// Distinct appointments: a phantom (a position it had during the phase) and its live hold are
	// one arrival.
	want, arrivals := key(start), map[uuid.UUID]bool{}
	for _, h := range s.holds {
		if h.id != exclude && (h.status == 1 || h.status == 2) && key(h.window.start) == want {
			arrivals[h.id] = true
		}
	}
	return len(arrivals) < s.stagingCap
}

// vehicleFree reports whether vehicle has no hold overlapping w (exclude left out). It needs
// loadVehicleHolds for that vehicle.
func (s *oracleShop) vehicleFree(vehicle uuid.UUID, w timeWindow, exclude uuid.UUID) bool {
	for _, h := range s.vehicleHolds[vehicle] {
		if h.id != exclude && h.window.overlaps(w) {
			return false
		}
	}
	return true
}

// evaluate fills each slot's counts (what is free right now) and verdict (free, or free after the
// one-move repair, repair.go) for r over a window of minutes.
func (s *oracleShop) evaluate(slots []oracleSlot, r jobRequirement, soft bool, minutes int) {
	length := time.Duration(minutes) * time.Minute
	for i := range slots {
		sl := &slots[i]
		w := timeWindow{sl.start, sl.start.Add(length)}
		sl.bays, sl.techs = s.counts(r, soft, w, uuid.Nil)
		sl.staging = s.stagingRoom(sl.start, uuid.Nil)
		sl.feasibleAt = !sl.lunch && sl.inHours && sl.staging && s.fits(r, soft, w, uuid.Nil, sl.bays, sl.techs)
	}
}

// oracleDay evaluates every start of day for req over a window of minutes (normally the job's
// labour time; shorter to ask whether a start is free at its beginning only).
func oracleDay(ctx context.Context, q querier, w workshop, req jobRequirement, soft bool, day booking.Date, minutes int) ([]oracleSlot, error) {
	slots, err := dayStarts(w, day, minutes)
	if err != nil {
		return nil, err
	}
	from, to := dayBounds(day, w.loc)
	shop, err := loadShop(ctx, q, w.dealer.ID, from, to)
	if err != nil {
		return nil, err
	}
	shop.evaluate(slots, req, soft, minutes)
	return slots, nil
}

// anyFeasible reports whether some start of the day can still hold the job.
func anyFeasible(slots []oracleSlot) bool {
	for _, s := range slots {
		if s.feasibleAt {
			return true
		}
	}
	return false
}
