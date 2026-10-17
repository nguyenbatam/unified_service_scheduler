package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The truth phase proves the real-time availability check (R2b) and the whole-duration rule (R2a)
// without contention, so every answer can be judged exactly. It fills one day of the stress
// workshop booking by booking, reading availability on one backend and booking on the other:
//
//   - every slot availability reports (available or not, and its free bay / technician counts)
//     must match the SQL oracle computed from the tables; each read after the first follows a
//     booking made on the other backend, so this is also the cross-instance real-time check (no
//     cache or replica in between);
//   - a start reported available must book (201) — no false offers. One offered with no free bay
//     or no free technician (availability counts what is free without a move) must book by
//     moving another hold of the dealership overlapping it (one-move repair: an
//     APPOINTMENT_REASSIGNED in the same request), and one offered with both free must move
//     nothing;
//   - a start reported full must be refused (409/422) — no overbooking. Availability already
//     offers every start a repair could open, and nothing else books the shop during the phase
//     (it runs alone, one request at a time), so a full start that books is a mismatch, repair or
//     not;
//   - a start whose bay and technician are free at its beginning but not for the whole job must be
//     refused — the check covers the entire duration;
//   - once nothing is offered for any job, the oracle must agree that nothing fits: the day is
//     truly full, not refused early;
//   - then bookings are cancelled and rescheduled on the write backend, and every change is read
//     back on the other one against the oracle: a cancelled start is offered again (truth_churn.go).

// truthStats is what the phase measured.
type truthStats struct {
	slotChecks, mismatches     int
	booked, falseOffers        int
	fullProbes, overbooked     int
	partialProbes, partialMiss int
	// repaired: offered starts with no free bay or technician that booked by moving another hold;
	// badMoves: offered starts that moved nothing although no resource was free, or moved a hold
	// although both were.
	repaired, badMoves int
	// Probes that never got a definite answer (busy, transport) or got one that is neither a
	// booking nor a refusal: neither is an overbooking, and neither is judged as one.
	probeUndecided, probeErrors int
	full                        bool
	failures                    []truthFailure
	ids                         []uuid.UUID
	mixOf                       map[uuid.UUID]jobMix // each booking's job mix (the churn reopens it)
	churned                     churnStats
}

// truthFailure is one failed check, filed under the requirement it breaks.
type truthFailure struct {
	req, detail string
}

// failf records a failure of requirement req (the first 8 of the phase are kept).
func (t *truthStats) failf(req, format string, args ...any) {
	if len(t.failures) < 8 {
		t.failures = append(t.failures, truthFailure{req: req, detail: fmt.Sprintf(format, args...)})
	}
}

// truthMaxBookings bounds the phase (the workshop day holds far fewer).
const truthMaxBookings = 400

// runTruth fills day at w. readAPI and writeAPI are two different backends (direct URLs).
func runTruth(ctx context.Context, conn *pgx.Conn, readAPI, writeAPI *apiClient, w workshop, mixes []jobMix, vehicles []uuid.UUID, day booking.Date, runID string) (*truthStats, error) {
	st := &truthStats{mixOf: map[uuid.UUID]jobMix{}}
	rng := rand.New(rand.NewPCG(uint64(day.Year), uint64(day.Month)*31+uint64(day.Day))) //nolint:gosec // reproducible choices, not security
	next := 0
	for st.booked < truthMaxBookings && ctx.Err() == nil {
		progressed := false
		for _, mix := range mixes {
			// Up to three fresh cars per mix and round (two probes, one booking); a car reused the
			// same day would be busy, which neither the oracle nor the checks expect.
			if next+3 > len(vehicles) {
				return st, fmt.Errorf("truth: ran out of fresh cars after %d bookings (fleet %d)", st.booked, len(vehicles))
			}
			avail, oracle, err := truthRead(ctx, conn, readAPI, w, mix, day, vehicles[next%len(vehicles)])
			if err != nil {
				return st, err
			}
			st.compare(mix, avail, oracle)
			var offered, full, partial []int
			// Which starts have a fitting bay and technician free for their first half hour.
			firstHalfHour, err := oracleDay(ctx, conn, w, mix.req, false, day, slotMinutes)
			if err != nil {
				return st, err
			}
			freeAtStart := map[int64]bool{}
			for _, o := range firstHalfHour {
				freeAtStart[o.start.Unix()] = o.feasibleAt
			}
			for i, s := range avail.Slots {
				switch {
				case s.Available:
					offered = append(offered, i)
				case s.Reason == "no_bay" || s.Reason == "no_technician" || s.Reason == "no_resources":
					full = append(full, i)
					if freeAtStart[s.Start.Unix()] {
						partial = append(partial, i) // free at its start, not for the whole job
					}
				}
			}
			if len(full) > 0 {
				i := full[rng.IntN(len(full))]
				if st.probeRefused(ctx, writeAPI, w, mix, avail.Slots[i].Start, vehicles[next%len(vehicles)], fmt.Sprintf("%s:truth:full:%d", runID, st.fullProbes+st.probeUndecided+st.probeErrors), &st.overbooked, "full") {
					st.fullProbes++
				}
				next++
			}
			if len(partial) > 0 {
				i := partial[rng.IntN(len(partial))]
				if st.probeRefused(ctx, writeAPI, w, mix, avail.Slots[i].Start, vehicles[next%len(vehicles)], fmt.Sprintf("%s:truth:partial:%d", runID, st.partialProbes+st.probeUndecided+st.probeErrors), &st.partialMiss, "partial window") {
					st.partialProbes++
				}
				next++
			}
			if len(offered) == 0 {
				continue
			}
			slot := avail.Slots[offered[rng.IntN(len(offered))]]
			vehicle := vehicles[next%len(vehicles)]
			next++
			mark, err := historyMark(ctx, conn)
			if err != nil {
				return st, err
			}
			resp, definite := sendUntilDefinite(ctx, writeAPI, http.MethodPost, "/api/v1/appointments",
				bookingPayload(w.dealer.ID, vehicle, mix.ids, slot.Start),
				map[string]string{"Idempotency-Key": fmt.Sprintf("%s:truth:%d", runID, st.booked+st.falseOffers)}, probeAttempts)
			if resp.code != http.StatusCreated && resp.code != http.StatusOK {
				st.falseOffers++
				st.failf("R2b", "%s at %s was offered (bays=%d techs=%d) but booking answered %d (definite=%v): %s",
					mix.name, slot.Start.In(w.loc).Format("15:04"), slot.FreeBays, slot.FreeTechnicians, resp.code, definite, truncate(string(resp.body), 120))
				continue
			}
			var appt domain.Appointment
			if err := json.Unmarshal(resp.body, &appt); err != nil || appt.ID == uuid.Nil {
				return st, fmt.Errorf("truth: booking body: %s", truncate(string(resp.body), 120))
			}
			st.ids = append(st.ids, appt.ID)
			st.mixOf[appt.ID] = mix
			st.booked++
			progressed = true
			if err := st.checkMoves(ctx, conn, w, mix, slot, appt, mark); err != nil {
				return st, err
			}
		}
		if !progressed {
			break
		}
	}
	// Nothing offered any more: the oracle must agree that no job fits anywhere that day.
	st.full = true
	for _, mix := range mixes {
		oracle, err := oracleDay(ctx, conn, w, mix.req, false, day, mix.req.minutes)
		if err != nil {
			return st, err
		}
		if anyFeasible(oracle) {
			st.full = false
			st.failf("R2b", "availability offers nothing for %s but the oracle still finds a feasible start", mix.name)
		}
	}
	// Then give capacity back and check it is offered again (truth_churn.go).
	fresh := func() (uuid.UUID, error) {
		if next >= len(vehicles) {
			return uuid.Nil, fmt.Errorf("truth churn: ran out of fresh cars (fleet %d)", len(vehicles))
		}
		next++
		return vehicles[next-1], nil
	}
	return st, st.churn(ctx, conn, readAPI, writeAPI, w, mixes, day, rng, fresh)
}

// truthRead reads availability for mix on day and the oracle's view of it.
func truthRead(ctx context.Context, conn *pgx.Conn, api *apiClient, w workshop, mix jobMix, day booking.Date, vehicle uuid.UUID) (domain.Availability, []oracleSlot, error) {
	avail, err := availability(ctx, api, w.dealer.ID, vehicle, mix.ids, day.String())
	if err != nil {
		return avail, nil, fmt.Errorf("truth: availability %s: %w", mix.name, err)
	}
	oracle, err := oracleDay(ctx, conn, w, mix.req, false, day, mix.req.minutes)
	return avail, oracle, err
}

// compare judges every reported slot against the oracle: the same starts, the same verdict, and
// for offered starts the same free bay and technician counts.
func (t *truthStats) compare(mix jobMix, avail domain.Availability, oracle []oracleSlot) {
	if len(avail.Slots) != len(oracle) {
		t.mismatches++
		t.failf("R2b", "%s: availability lists %d starts, the oracle %d", mix.name, len(avail.Slots), len(oracle))
		return
	}
	for i, s := range avail.Slots {
		o := oracle[i]
		t.slotChecks++
		label := s.Start.Format("15:04")
		switch {
		case !s.Start.Equal(o.start):
			t.mismatches++
			t.failf("R2b", "%s: start %d is %s, oracle %s", mix.name, i, s.Start, o.start)
		case s.Available != o.feasibleAt:
			t.mismatches++
			t.failf("R2b", "%s %s: available=%v reason=%q, oracle feasible=%v (bays=%d techs=%d lunch=%v)",
				mix.name, label, s.Available, s.Reason, o.feasibleAt, o.bays, o.techs, o.lunch)
		case s.Available && (s.FreeBays != o.bays || s.FreeTechnicians != o.techs):
			t.mismatches++
			t.failf("R2b", "%s %s: free bays/techs %d/%d, oracle %d/%d", mix.name, label, s.FreeBays, s.FreeTechnicians, o.bays, o.techs)
		}
	}
}

// historyMark is the newest appointment_history row: moves made by a later request come after it.
func historyMark(ctx context.Context, conn querier) (int64, error) {
	var mark int64
	if err := queryOne(ctx, conn, `SELECT coalesce(max(id), 0) FROM appointment_history`, nil, &mark); err != nil {
		return 0, fmt.Errorf("truth: history mark: %w", err)
	}
	return mark, nil
}

// movedFor counts the other holds of the dealership overlapping appt's window that were
// reassigned after mark: the moves its request made (nothing else books the shop meanwhile).
func movedFor(ctx context.Context, conn *pgx.Conn, w workshop, appt domain.Appointment, mark int64) (int, error) {
	var moved int
	err := queryOne(ctx, conn, `
		SELECT count(*)::int FROM appointment_history h JOIN appointments a ON a.id = h.appointment_id
		WHERE h.id > $1 AND a.dealership_id = $2 AND a.id <> $3 AND h.event_type = `+sqlCode(domain.EventAppointmentReassigned)+`
		  AND h.start_at < $5 AND $4 < h.end_at`,
		[]any{mark, w.dealer.ID, appt.ID, appt.StartAt, appt.EndAt}, &moved)
	if err != nil {
		return 0, fmt.Errorf("truth: moves of %s: %w", appt.ID, err)
	}
	return moved, nil
}

// checkMoves judges the booking of an offered slot by the moves its request made: a start offered
// with no free bay or no free technician needs a one-move repair, one offered with both free needs
// none.
func (t *truthStats) checkMoves(ctx context.Context, conn *pgx.Conn, w workshop, mix jobMix, slot domain.Slot, appt domain.Appointment, mark int64) error {
	moved, err := movedFor(ctx, conn, w, appt, mark)
	if err != nil {
		return err
	}
	needed := slot.FreeBays == 0 || slot.FreeTechnicians == 0
	switch {
	case needed && moved > 0:
		t.repaired++
	case needed:
		t.badMoves++
		t.failf("Q5", "%s at %s was offered with bays/techs %d/%d (a repair) but booked without moving any hold", mix.name, slot.Start.In(w.loc).Format("15:04"), slot.FreeBays, slot.FreeTechnicians)
	case moved > 0:
		t.badMoves++
		t.failf("Q5", "%s at %s was offered with bays/techs %d/%d free but its booking moved %d hold(s)", mix.name, slot.Start.In(w.loc).Format("15:04"), slot.FreeBays, slot.FreeTechnicians, moved)
	}
	return nil
}

// probeRefused books a start that must be refused and counts it in miss when it is booked (the
// accidental booking is recorded for cleanup). Availability offers every start a one-move repair
// could open, so an acceptance is a mismatch whether or not the booking moved a hold. A probe
// left without a definite answer (503 busy, transport) is undecided and one answered anything
// else (4xx/5xx) is an error: counted apart, never as an overbooking. It reports whether the probe
// was decided (booked or refused).
func (t *truthStats) probeRefused(ctx context.Context, api *apiClient, w workshop, mix jobMix, start time.Time, vehicle uuid.UUID, key string, miss *int, kind string) bool {
	resp, definite := sendUntilDefinite(ctx, api, http.MethodPost, "/api/v1/appointments",
		bookingPayload(w.dealer.ID, vehicle, mix.ids, start), map[string]string{"Idempotency-Key": key}, probeAttempts)
	label := start.In(w.loc).Format("15:04")
	switch {
	case !definite:
		t.probeUndecided++
		t.failf("R2b", "%s probe %s at %s: no definite answer after %d attempts (last %d): neither booked nor refused", kind, mix.name, label, probeAttempts, resp.code)
		return false
	case resp.code == http.StatusConflict || resp.code == http.StatusUnprocessableEntity:
		return true
	case resp.code == http.StatusCreated || resp.code == http.StatusOK:
		var appt domain.Appointment
		if json.Unmarshal(resp.body, &appt) == nil && appt.ID != uuid.Nil {
			t.ids = append(t.ids, appt.ID)
		}
		*miss++
		t.failf("R2a", "%s probe %s at %s: answered %d (booked), want 409/422: %s", kind, mix.name, label, resp.code, truncate(string(resp.body), 120))
		return true
	default:
		t.probeErrors++
		t.failf("R1", "%s probe %s at %s: answered %d, neither a booking nor a refusal: %s", kind, mix.name, label, resp.code, truncate(string(resp.body), 120))
		return false
	}
}

// record writes the phase's evidence into the matrix.
func (t *truthStats) record(m *matrix, day booking.Date) {
	src := "truth"
	m.check("R2b", src, t.mismatches == 0, "%d slot verdicts and free counts read on one backend, each right after a booking on the other, vs the SQL oracle on %s: %d mismatches", t.slotChecks, day, t.mismatches)
	m.headline("R2b", "%d slot verdicts vs SQL oracle, %d mismatches", t.slotChecks, t.mismatches)
	m.check("R2b", src, t.falseOffers == 0 && t.booked > 0, "%d offered starts booked on the other backend: %d false offers", t.booked+t.falseOffers, t.falseOffers)
	m.check("R2b", src, t.full, "day filled until nothing was offered; oracle confirms no job fits anywhere (refusals only when truly full)")
	m.check("R2a", src, t.overbooked == 0 && t.fullProbes > 0, "%d bookings at starts reported full, answered booked or refused: %d accepted", t.fullProbes, t.overbooked)
	m.check("R2a", src, t.partialMiss == 0 && t.partialProbes > 0, "%d bookings at starts free at their beginning but not for the whole job, answered booked or refused: %d accepted", t.partialProbes, t.partialMiss)
	m.check("R2b", src, t.probeUndecided == 0 && t.probeErrors == 0, "probes of full starts left without a definite answer (busy, transport): %d; answered neither booked nor refused: %d (counted apart, not as overbooking)", t.probeUndecided, t.probeErrors)
	moves := fmt.Sprintf("%d offered starts with no free bay or technician booked by moving another hold overlapping them (one-move repair; the SQL pass checks every row); %d bookings with a wrong move count (a needed move missing, or a move with both resources free)", t.repaired, t.badMoves)
	if t.repaired == 0 && t.badMoves == 0 {
		m.info("Q5", src, "%s — no offered start needed a repair this run", moves)
	} else {
		m.check("Q5", src, t.badMoves == 0, "%s", moves)
	}
	t.recordChurn(m)
	m.check("R1", src, t.booked > 0, "%d bookings across %d job mixes (single and multi-line) on one day of the stress workshop", t.booked, 4)
	for _, f := range t.failures {
		m.fail(f.req, src, "%s", f.detail)
	}
	m.metric("truth", map[string]int{
		"slotChecks": t.slotChecks, "mismatches": t.mismatches, "booked": t.booked, "falseOffers": t.falseOffers,
		"fullProbes": t.fullProbes, "overbooked": t.overbooked, "partialProbes": t.partialProbes, "partialAccepted": t.partialMiss, "probesUndecided": t.probeUndecided, "probeErrors": t.probeErrors,
		"repaired": t.repaired, "badMoves": t.badMoves,
	})
}
