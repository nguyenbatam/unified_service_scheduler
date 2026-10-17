package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/config"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// A refusal is judged against the dealership as it was when the refusal happened, not as it is at
// the end of the phase: the end state holds every booking made after the refusal too (the fill
// day's top-up fills the day, so against it nearly every refusal looks justified). The holds at
// that moment are rebuilt from appointment_history (one row per change, written in the change's
// TX, `at` = that TX's start on the database clock):
//
//   - a booking whose TX began after the refusal's answer came back cannot have been in its way,
//     so it is left out (its first history row is later than the answer);
//   - every other position an appointment had while the request could have been decided counts
//     as held: its position as of txSlack before the request went out (a change whose TX began
//     earlier may still have been uncommitted while the request ran, so its previous position is
//     kept too), plus every position it was given until the answer came back.
//
// That over-occupies the shop (a moved booking holds both places; a booking whose TX began just
// before the answer but committed after the decision is counted): it can only hide a false
// refusal, never invent one. Database and client clocks are compared through the offset measured
// against the database (pgClock), widened by its uncertainty.

// txSlack bounds how long a write TX can stay open: the request deadline (REQUEST_TIMEOUT, which
// config keeps below config.HTTPWriteTimeout) plus the detached COMMIT (store.commitTimeout, 5 s),
// plus a margin. A change whose TX began that long before the request was committed (or rolled
// back) before it went out.
const txSlack = config.HTTPWriteTimeout + 5*time.Second + 5*time.Second

// histPos is one history row: an appointment's position from the TX that began at at.
type histPos struct {
	hold oHold
	at   time.Time
}

// holdTimeline is every position the dealership's appointments around the judged stretch had,
// per appointment in history order.
type holdTimeline map[uuid.UUID][]histPos

// loadTimeline reads the history of dealer's appointments starting in [from - 1 day, to + 1 day)
// with their booked requirement (a snapshot, never changed after booking).
func loadTimeline(ctx context.Context, q querier, dealer uuid.UUID, from, to time.Time) (holdTimeline, error) {
	rows, err := q.Query(ctx, `
		SELECT h.appointment_id, h.service_bay_id, h.technician_id, a.vehicle_id, h.status, h.start_at, h.end_at, h.at,
		       a.required_bay_type, a.required_skills, a.min_skill_level, a.booked_duration_minutes, a.is_waiter
		FROM appointments a JOIN appointment_history h ON h.appointment_id = a.id
		WHERE a.dealership_id = $1 AND a.start_at >= $2::timestamptz - interval '1 day' AND a.start_at < $3::timestamptz + interval '1 day'
		ORDER BY h.appointment_id, h.id`, dealer, from, to)
	if err != nil {
		return nil, fmt.Errorf("oracle: timeline: %w", err)
	}
	positions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (histPos, error) {
		var p histPos
		var bayType int16
		var skills []int16
		err := row.Scan(&p.hold.id, &p.hold.bay, &p.hold.tech, &p.hold.vehicle, &p.hold.status, &p.hold.window.start, &p.hold.window.end, &p.at,
			&bayType, &skills, &p.hold.req.minLevel, &p.hold.req.minutes, &p.hold.waiter)
		p.hold.req.bayType = domain.BayType(bayType)
		for _, sk := range skills {
			p.hold.req.skills = append(p.hold.req.skills, domain.TechnicianSkill(sk))
		}
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("oracle: timeline: %w", err)
	}
	tl := holdTimeline{}
	for _, p := range positions {
		tl[p.hold.id] = append(tl[p.hold.id], p)
	}
	return tl, nil
}

// holding reports whether a status holds its bay, technician and car (confirmed, checked in, in progress).
func holding(status int16) bool { return status >= 1 && status <= 3 }

// between returns the holds that may have been in the way of a request decided within [from, to]
// (database clock): per appointment, its position as of from (the newest row before it) and every
// position given at or before to; nothing for an appointment created after to. An appointment
// with more than one such position is marked phantom in all of them (the repair rule never moves
// it, as for a hold changed during the phase).
func (tl holdTimeline) between(from, to time.Time) []oHold {
	var out []oHold
	for _, rows := range tl {
		var pos []oHold
		last := -1
		for i, r := range rows {
			if r.at.Before(from) {
				last = i
			}
		}
		for i, r := range rows {
			inWindow := i == last || (!r.at.Before(from) && !r.at.After(to))
			if !inWindow || !holding(r.hold.status) {
				continue
			}
			dup := false
			for _, p := range pos {
				if p.bay == r.hold.bay && p.tech == r.hold.tech && p.window == r.hold.window && p.status == r.hold.status {
					dup = true
				}
			}
			if !dup {
				pos = append(pos, r.hold)
			}
		}
		if len(pos) > 1 {
			for i := range pos {
				pos[i].phantom = true
			}
		}
		out = append(out, pos...)
	}
	return out
}

// pgClock maps client instants to the database clock: db ≈ client + offset, within ± uncertainty.
type pgClock struct {
	offset, uncertainty time.Duration
}

// measurePGClock reads clock_timestamp() a few times and keeps the tightest round trip.
func measurePGClock(ctx context.Context, q querier) (pgClock, error) {
	best := pgClock{uncertainty: time.Hour}
	for range 5 {
		var db time.Time
		t0 := time.Now()
		if err := queryOne(ctx, q, `SELECT clock_timestamp()`, nil, &db); err != nil {
			return best, fmt.Errorf("oracle: database clock: %w", err)
		}
		t1 := time.Now()
		if half := t1.Sub(t0) / 2; half < best.uncertainty {
			best = pgClock{offset: db.Sub(t0.Add(half)), uncertainty: half}
		}
	}
	return best, nil
}

// decisionWindow is, on the database clock, the stretch within which r was decided, widened by
// txSlack before it went out and by the clock's uncertainty (plus a margin) after its answer.
func (c pgClock) decisionWindow(r refusal) (from, to time.Time) {
	margin := c.uncertainty + 100*time.Millisecond
	return r.sent.Add(c.offset - margin - txSlack), r.at.Add(c.offset + margin)
}
