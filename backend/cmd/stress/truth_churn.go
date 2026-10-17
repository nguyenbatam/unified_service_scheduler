package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Once the truth day is full, capacity is given back: truthChurnOps random bookings are cancelled
// or rescheduled on the write backend (with If-Match), and after each change every job mix's
// availability is read on the other backend and compared with the oracle again. A cancelled
// booking's start must be offered again for its own job mix (its bay, technician and arrival are
// free once more, and the probing car is fresh): a slot that stays "full" after a cancel is the
// stale-cache / missed-invalidation bug the fill-only phase cannot see. A reschedule goes to a
// start availability offers that booking (excludeAppointmentId: its own hold left out); nothing
// else books the shop meanwhile, so a refusal there is a false offer.

// truthChurnOps is how many bookings of the full day are cancelled or rescheduled (alternately).
const truthChurnOps = 6

// churnStats is what the churn measured.
type churnStats struct {
	cancelled, reopened, stillFull int
	rescheduled, noTarget, refused int
}

// churn gives capacity back on the full day and re-compares; vehicle supplies fresh probe cars.
func (t *truthStats) churn(ctx context.Context, conn *pgx.Conn, readAPI, writeAPI *apiClient, w workshop, mixes []jobMix, day booking.Date, rng *rand.Rand, vehicle func() (uuid.UUID, error)) error {
	order := rng.Perm(len(t.ids))
	for k := 0; k < len(order) && t.churned.cancelled+t.churned.rescheduled+t.churned.noTarget < truthChurnOps; k++ {
		id := t.ids[order[k]]
		mix, ok := t.mixOf[id]
		if !ok {
			continue
		}
		appt, err := getJSON[domain.Appointment](ctx, writeAPI, http.MethodGet, "/api/v1/appointments/"+id.String(), nil)
		if err != nil {
			return fmt.Errorf("truth churn: read %s: %w", id, err)
		}
		if appt.Status != domain.StatusConfirmed {
			continue
		}
		ifMatch := map[string]string{"If-Match": `"` + strconv.Itoa(appt.Version) + `"`}
		label := appt.StartAt.In(w.loc).Format("15:04")
		if t.churned.cancelled <= t.churned.rescheduled+t.churned.noTarget {
			resp, _ := sendUntilDefinite(ctx, writeAPI, http.MethodPost, "/api/v1/appointments/"+id.String()+"/cancel", nil, ifMatch, probeAttempts)
			if resp.code != http.StatusOK {
				t.failf("R1", "cancel %s (%s at %s) answered %d: %s", id, mix.name, label, resp.code, truncate(string(resp.body), 120))
				continue
			}
			t.churned.cancelled++
			probe, err := vehicle()
			if err != nil {
				return err
			}
			avail, oracle, err := truthRead(ctx, conn, readAPI, w, mix, day, probe)
			if err != nil {
				return err
			}
			t.compare(mix, avail, oracle)
			if offeredAt(avail.Slots, appt) {
				t.churned.reopened++
			} else {
				t.churned.stillFull++
				t.failf("R2b", "%s at %s cancelled on the write backend, but availability on the read backend still offers nothing there", mix.name, label)
			}
		} else {
			moved, err := t.reschedule(ctx, readAPI, writeAPI, w, mix, day, appt, ifMatch, rng)
			if err != nil {
				return err
			}
			if !moved {
				continue
			}
		}
		for _, mx := range mixes {
			probe, err := vehicle()
			if err != nil {
				return err
			}
			avail, oracle, err := truthRead(ctx, conn, readAPI, w, mx, day, probe)
			if err != nil {
				return err
			}
			t.compare(mx, avail, oracle)
		}
	}
	return nil
}

// reschedule moves appt to another start availability offers it (its own hold left out), read on
// the read backend, written on the other. It reports whether the day changed.
func (t *truthStats) reschedule(ctx context.Context, readAPI, writeAPI *apiClient, w workshop, mix jobMix, day booking.Date, appt domain.Appointment, ifMatch map[string]string, rng *rand.Rand) (bool, error) {
	avail, err := getJSON[domain.Availability](ctx, readAPI, http.MethodGet, fmt.Sprintf(
		"/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s&excludeAppointmentId=%s", w.dealer.ID, appt.VehicleID, jobQuery(mix.ids), day, appt.ID), nil)
	if err != nil {
		return false, fmt.Errorf("truth churn: availability for %s: %w", appt.ID, err)
	}
	var targets []domain.Slot
	for _, s := range avail.Slots {
		if s.Available && !s.Start.Equal(appt.StartAt) {
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		t.churned.noTarget++
		return false, nil
	}
	target := targets[rng.IntN(len(targets))]
	resp, _ := sendUntilDefinite(ctx, writeAPI, http.MethodPost, "/api/v1/appointments/"+appt.ID.String()+"/reschedule",
		map[string]any{"startAt": target.Start}, ifMatch, probeAttempts)
	switch resp.code {
	case http.StatusOK:
		t.churned.rescheduled++
		return true, nil
	case http.StatusConflict, http.StatusUnprocessableEntity:
		t.churned.refused++
		t.falseOffers++
		t.failf("R2b", "%s moving %s → %s was offered (its own hold left out) but the reschedule answered %d: %s", mix.name,
			appt.StartAt.In(w.loc).Format("15:04"), target.Start.In(w.loc).Format("15:04"), resp.code, truncate(string(resp.body), 120))
		return false, nil
	default:
		t.failf("R1", "reschedule %s answered %d: %s", appt.ID, resp.code, truncate(string(resp.body), 120))
		return false, nil
	}
}

// offeredAt reports whether availability offers appt's start.
func offeredAt(slots []domain.Slot, appt domain.Appointment) bool {
	for _, s := range slots {
		if s.Start.Equal(appt.StartAt) {
			return s.Available
		}
	}
	return false
}

// recordChurn writes the churn's evidence.
func (t *truthStats) recordChurn(m *matrix) {
	c := t.churned
	if c.cancelled == 0 && c.rescheduled == 0 {
		m.skip("R2b", "truth churn", "no booking of the full day could be cancelled or rescheduled (%d without another offered start)", c.noTarget)
		return
	}
	m.check("R2b", "truth churn", c.stillFull == 0 && c.refused == 0 && c.cancelled > 0,
		"after the day filled, %d bookings cancelled and %d rescheduled on one backend (%d had no other offered start): every cancelled start offered again on the other backend for its job mix (%d of %d; still full: %d), every offered reschedule target accepted (%d refused); availability vs the oracle re-checked after each change (mismatches count in the truth line)",
		c.cancelled, c.rescheduled, c.noTarget, c.reopened, c.cancelled, c.stillFull, c.refused)
}
