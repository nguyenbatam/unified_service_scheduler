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
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The storm phase races reschedules against new bookings for one start. The hammer phases only
// race new bookings; a reschedule takes the same day lock and GiST entries but also excludes its
// own hold and moves it in place, so a storm of them into one start is its own path to prove. On
// the river shop (no staging cap, one job, so a start's capacity is exact), a target start T with
// room for C jobs is picked; stormFactor·C bookings are made at other starts of the day whose
// windows do not touch T's (the sources), one car each; then, released together, every source is
// rescheduled to T with If-Match and stormFactor·C new bookings with fresh cars ask for T. Judged:
//
//   - exactly C holds end at T (counted in Postgres: committed answers lost to a busy reply
//     included): never more (double-booking, which GiST also refuses), and never fewer while
//     every request got a definite answer (a refusal while T still had room);
//   - every refused reschedule left its booking where it was, at its version; every won one moved
//     it to T with version + 1 (in place: the same id);
//   - only 200/201/409/422 and busy answers; a 412 (nobody else writes these bookings) or a 5xx
//     is a bug.

const stormFactor = 2

// stormCars is the least pool the storm needs: half of it (the middle, see run.go) holds the
// sources and the new bookings of a start with room for up to 5 jobs (the river shop's 5 bays).
const stormCars = 2 * (2*stormFactor*5 + 1)

// stormStats is what the storm measured.
type stormStats struct {
	date           string
	target         time.Time
	capacity       int
	sources        int
	moved, booked  int // winners heard: reschedules (200), new bookings (201/200)
	refused        int
	unresolved     int
	holdsAtTarget  int
	stayed, bumped int // refused reschedules left in place / won ones at T with version + 1
	errs           []string
	ids            []uuid.UUID
}

func (st *stormStats) errf(format string, args ...any) {
	if len(st.errs) < 6 {
		st.errs = append(st.errs, fmt.Sprintf(format, args...))
	}
}

// storm runs the phase on dealer (the river shop) with job, cars from pool (at least
// 2·stormFactor·C of them), and returns what it measured.
// The search starts at from (a date, "" = today): after the hammered date, whose cars are busy.
func (ss *session) storm(ctx context.Context, dealer domain.Dealership, job []uuid.UUID, pool []uuid.UUID, from string) (*stormStats, error) {
	st := &stormStats{}
	cutoff := time.Now().Add(ss.s.startMargin)
	loc, err := time.LoadLocation(dealer.Timezone)
	if err != nil {
		return st, err
	}
	var sources []time.Time
	for range maxDateRolls {
		if st.date, err = nextFreeDate(ctx, ss.api, dealer.ID, pool[0], job, from); err != nil {
			return st, err
		}
		avail, err := availability(ctx, ss.api, dealer.ID, pool[0], job, st.date)
		if err != nil {
			return st, err
		}
		if sources = st.pick(avail, cutoff, len(pool)); sources != nil {
			break
		}
		if from, err = nextDate(st.date); err != nil {
			return st, err
		}
	}
	if sources == nil {
		return st, fmt.Errorf("storm: no date with a start holding 2+ jobs and room for %d× that elsewhere", stormFactor)
	}
	fmt.Printf("phase storm start: %s on %s, %d reschedules + %d new bookings race for %s (room for %d)\n",
		dealer.Name, st.date, len(sources), stormFactor*st.capacity, st.target.In(loc).Format("15:04"), st.capacity)

	// The sources, one car each, booked one by one.
	type source struct {
		appt domain.Appointment
		key  string
	}
	var booked []source
	for i, start := range sources {
		key := fmt.Sprintf("%s:storm:src:%d", ss.runID, i)
		resp, definite := sendUntilDefinite(ctx, ss.api, http.MethodPost, "/api/v1/appointments",
			bookingPayload(dealer.ID, pool[i], job, start), map[string]string{"Idempotency-Key": key}, probeAttempts)
		var a domain.Appointment
		if !definite || (resp.code != http.StatusCreated && resp.code != http.StatusOK) || json.Unmarshal(resp.body, &a) != nil || a.ID == uuid.Nil {
			return st, fmt.Errorf("storm: source booking at %s answered %d: %s", start, resp.code, truncate(string(resp.body), 120))
		}
		st.ids = append(st.ids, a.ID)
		booked = append(booked, source{appt: a, key: key})
	}
	st.sources = len(booked)

	// Released together: every source to T with If-Match, and as many new bookings for T.
	type answer struct {
		code int
		body []byte
	}
	moves := make([]answer, len(booked))
	news := make([]answer, stormFactor*st.capacity)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i, src := range booked {
		wg.Go(func() {
			<-gate
			resp, _ := sendUntilDefinite(ctx, ss.api, http.MethodPost, "/api/v1/appointments/"+src.appt.ID.String()+"/reschedule",
				map[string]any{"startAt": st.target}, map[string]string{"If-Match": `"` + strconv.Itoa(src.appt.Version) + `"`}, probeAttempts)
			moves[i] = answer{resp.code, resp.body}
		})
	}
	for j := range news {
		car := pool[len(booked)+j]
		wg.Go(func() {
			<-gate
			resp, _ := sendUntilDefinite(ctx, ss.api, http.MethodPost, "/api/v1/appointments",
				bookingPayload(dealer.ID, car, job, st.target), map[string]string{"Idempotency-Key": fmt.Sprintf("%s:storm:new:%d", ss.runID, j)}, probeAttempts)
			news[j] = answer{resp.code, resp.body}
		})
	}
	close(gate)
	wg.Wait()

	for i, a := range moves {
		src := booked[i].appt
		switch {
		case a.code == http.StatusOK:
			st.moved++
		case a.code == http.StatusConflict || a.code == http.StatusUnprocessableEntity:
			st.refused++
		case a.code == 0 || indefinite(a.code, nil):
			st.unresolved++
		default:
			st.errf("reschedule %s answered %d: %s", src.ID, a.code, truncate(string(a.body), 100))
		}
		now, ok := readAppointment(ctx, ss.api, src.ID)
		switch {
		case !ok:
			st.errf("read %s back after the storm", src.ID)
		case a.code == http.StatusOK && now.StartAt.Equal(st.target) && now.Version == src.Version+1:
			st.bumped++
		case a.code == http.StatusOK:
			st.errf("reschedule %s won but it is at %s v%d (was v%d)", src.ID, now.StartAt, now.Version, src.Version)
		case (a.code == http.StatusConflict || a.code == http.StatusUnprocessableEntity) && now.StartAt.Equal(src.StartAt) && now.Version == src.Version:
			st.stayed++
		case a.code == http.StatusConflict || a.code == http.StatusUnprocessableEntity:
			st.errf("reschedule %s refused %d but it is at %s v%d (was %s v%d)", src.ID, a.code, now.StartAt, now.Version, src.StartAt, src.Version)
		}
	}
	for _, a := range news {
		switch {
		case a.code == http.StatusCreated || a.code == http.StatusOK:
			var appt domain.Appointment
			if json.Unmarshal(a.body, &appt) != nil || appt.ID == uuid.Nil {
				st.errf("new booking answered %d with an unreadable body", a.code)
				continue
			}
			st.booked++
			st.ids = append(st.ids, appt.ID)
		case a.code == http.StatusConflict || a.code == http.StatusUnprocessableEntity:
			st.refused++
		case a.code == 0 || indefinite(a.code, nil):
			st.unresolved++
		default:
			st.errf("new booking answered %d: %s", a.code, truncate(string(a.body), 100))
		}
	}
	if ss.conn != nil {
		// Only the storm's own appointments (its sources and new bookings, by their Idempotency-Key,
		// lost answers included): the capacity is the room the target had when the storm began, and
		// holds already there then (the other-day writer books this dealership's next dates too) are
		// not part of it.
		if err := queryOne(ctx, ss.conn, `
			SELECT count(*)::int FROM appointments
			WHERE dealership_id = $1 AND start_at = $2 AND status IN (`+holdingSQL+`) AND idempotency_key LIKE $3`,
			[]any{dealer.ID, st.target, ss.runID + ":storm:%"}, &st.holdsAtTarget); err != nil {
			return st, err
		}
	} else {
		st.holdsAtTarget = st.moved + st.booked
	}
	return st, nil
}

// pick chooses the target (the last start after cutoff with room for 2+ jobs) and the sources:
// stormFactor·C places at other starts whose windows do not touch the target's, with enough cars
// for them and the new bookings. nil when the date cannot hold a storm.
func (st *stormStats) pick(avail domain.Availability, cutoff time.Time, cars int) []time.Time {
	length := time.Duration(avail.DurationMinutes) * time.Minute
	target := -1
	for i, s := range avail.Slots {
		if s.Available && !s.Start.Before(cutoff) && min(s.FreeBays, s.FreeTechnicians) >= 2 {
			target = i
		}
	}
	if target < 0 {
		return nil
	}
	t := avail.Slots[target]
	st.target, st.capacity = t.Start, min(t.FreeBays, t.FreeTechnicians)
	want := stormFactor * st.capacity
	tw := booking.Interval{Start: t.Start, End: t.Start.Add(length)}
	var out []time.Time
	for _, s := range avail.Slots {
		if !s.Available || s.Start.Before(cutoff) || booking.Overlaps(tw, booking.Interval{Start: s.Start, End: s.Start.Add(length)}) {
			continue
		}
		for range min(s.FreeBays, s.FreeTechnicians) {
			if len(out) < want {
				out = append(out, s.Start)
			}
		}
	}
	if len(out) < want || cars < 2*want {
		return nil
	}
	return out
}

// record writes the storm's verdict.
func (st *stormStats) record(m *matrix) {
	exact := st.holdsAtTarget == st.capacity || (st.holdsAtTarget < st.capacity && st.holdsAtTarget+st.unresolved >= st.capacity)
	m.check("Q1", "storm", exact && len(st.errs) == 0 && st.moved+st.booked > 0 && st.stayed+st.bumped == st.sources-st.unresolved,
		"%d reschedules (If-Match) and %d new bookings released together for one start on %s with room for %d: %d moved there, %d booked there, %d refused, %d unresolved; %d holds at that start in Postgres (exactly the room: never more, and fewer only by requests that never got an answer); %d refused reschedules left in place at their version, %d won ones moved in place with version + 1%s",
		st.sources, stormFactor*st.capacity, st.date, st.capacity, st.moved, st.booked, st.refused, st.unresolved,
		st.holdsAtTarget, st.stayed, st.bumped, sampleSuffix(st.errs))
}
