package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// requireCapacity pins a race test's precondition: the slot has exactly the free bays and
// technicians the test relies on, so a seed change fails here instead of silently testing nothing.
func requireCapacity(t *testing.T, svc *booking.Service, q booking.AvailabilityQuery, start time.Time, bays, techs int) {
	t.Helper()
	avail, err := svc.Availability(t.Context(), staffAdmin, q)
	if err != nil {
		t.Fatal(err)
	}
	slot := slotAt(t, avail, start)
	if !slot.Available || slot.FreeBays != bays || slot.FreeTechnicians != techs {
		t.Fatalf("precondition at %s: available=%v freeBays=%d freeTechnicians=%d, want %d bays and %d technicians",
			start, slot.Available, slot.FreeBays, slot.FreeTechnicians, bays, techs)
	}
}

// oneWinnerRounds books two different cars into the same slot on five fresh days and requires
// exactly one 201 per round, the loser a clean 409/422, and the database to agree.
func oneWinnerRounds(t *testing.T, service uuid.UUID, hour int, bays, techs int) []domain.Appointment {
	t.Helper()
	return oneWinnerRoundsAt(t, func(context.Context, *booking.Service) uuid.UUID { return seed.DealerNorth }, service, hour, bays, techs)
}

// oneWinnerRoundsAt is oneWinnerRounds at the dealership dealer opens.
func oneWinnerRoundsAt(t *testing.T, dealer func(context.Context, *booking.Service) uuid.UUID, service uuid.UUID, hour int, bays, techs int) []domain.Appointment {
	t.Helper()
	pool, _, svc, ctx := openTestDB(t)
	dealerID := dealer(ctx, svc)
	var winners []domain.Appointment
	for offset := 1; offset <= 5; offset++ {
		day := londonDay(t, offset)
		start := londonOn(t, day, hour, 0)
		requireCapacity(t, svc, booking.AvailabilityQuery{
			DealershipID: dealerID, VehicleID: seed.VehicleA, ServiceTypeID: service, Day: day,
		}, start, bays, techs)
		vehicles := []uuid.UUID{seed.VehicleA, seed.VehicleB}
		items := make([]domain.Appointment, len(vehicles))
		errs := make([]error, len(vehicles))
		raceN(len(vehicles), func(i int) {
			items[i], _, errs[i] = svc.Book(ctx, staffAdmin, booking.BookRequest{
				DealershipID: dealerID, VehicleID: vehicles[i], ServiceTypeID: service,
				StartAt: start, IdempotencyKey: uuid.NewString(),
			})
		})
		winner := items[oneWinner(t, day.String(), allLose(errs, refusals...)...)]
		holds := scalar[int](t, pool, `
			SELECT count(*)::int FROM appointments
			WHERE dealership_id = $1 AND status IN (`+holdingCodes+`) AND start_at = $2`, dealerID, start)
		if holds != 1 {
			t.Fatalf("%s: %d holds in the slot, want 1", day, holds)
		}
		assertEventsFor(t, pool, winner.ID)
		winners = append(winners, winner)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertStagingWithinCapacity(t, pool)
	return winners
}

// Northside MAJOR needs a general level-3 technician: only Blair qualifies, while both general bays
// (Bay 2, Bay 3) are free. The technician alone decides the race.
func TestOneQualifiedTechnicianTwoBaysOneWinner(t *testing.T) {
	t.Parallel()
	for _, w := range oneWinnerRounds(t, seed.ServiceMajor, 8, 2, 1) {
		if w.TechnicianID != seed.TechBlair {
			t.Fatalf("winner on %s, want Blair (the only qualified technician)", w.TechnicianName)
		}
	}
}

// An oil change at a shop whose only bay is the express lane (no general bay to spill into), with
// two lubrication technicians: the bay alone decides the race.
func TestOneBayTwoQualifiedTechniciansOneWinner(t *testing.T) {
	t.Parallel()
	quickLaneOnly := func(ctx context.Context, svc *booking.Service) uuid.UUID {
		d, _ := policyShop(ctx, t, svc, []domain.BayType{domain.BayExpress},
			[]policyTech{{name: "Aaron", level: 1, skills: lube}, {name: "Bea", level: 1, skills: lube}})
		return d
	}
	for _, w := range oneWinnerRoundsAt(t, quickLaneOnly, seed.ServiceOil, 8, 1, 2) {
		if w.ServiceBayName != "Bay A" {
			t.Fatalf("winner on %s, want Bay A (the only bay)", w.ServiceBayName)
		}
	}
}

// rawAppointment inserts a row straight into appointments (no service checks), so only the
// database constraints stand between it and a double booking.
func rawAppointment(ctx context.Context, pool *pgxpool.Pool, vehicle, bay, tech uuid.UUID, start, end time.Time, status domain.AppointmentStatus) (uuid.UUID, error) {
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id,
			service_bay_id, technician_id, start_at, end_at, status,
			booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
		SELECT $1, $2, v.customer_id, v.id, $3, $4, $5, $6, $7, $8, $9, 2, '{2}', 2 FROM vehicles v WHERE v.id = $10`,
		id, seed.DealerNorth, seed.ServiceBrake, bay, tech, start, end, int16(status),
		int(end.Sub(start).Minutes()), vehicle)
	return id, err
}

// The GiST exclusion constraints themselves (not the service's pre-checks) refuse an overlapping
// hold on a bay, a technician, or a vehicle, honour half-open bounds, and ignore released rows
// (4, 5, 6). Every subtest works on a day of its own with its own base hold, and removes what it
// inserted, so one failure cannot change what the others see.
func TestExclusionConstraints(t *testing.T) {
	t.Parallel()
	pool, _, _, ctx := openTestDB(t)
	day := 0
	// newBase gives a subtest the next free day and a confirmed base hold there: VehicleA on Bay 2
	// with Blair, 10:00–11:00. It returns the base id and the day's clock.
	newBase := func(t *testing.T) (uuid.UUID, func(h, m int) time.Time) {
		t.Helper()
		day++
		d := londonDay(t, day)
		at := func(h, m int) time.Time { return londonOn(t, d, h, m) }
		base, err := rawAppointment(ctx, pool, seed.VehicleA, seed.BayN2, seed.TechBlair, at(10, 0), at(11, 0), domain.StatusConfirmed)
		if err != nil {
			t.Fatal(err)
		}
		cleanupRows(ctx, t, pool, base)
		return base, at
	}

	type hold struct {
		vehicle, bay, tech uuid.UUID
		startH, startM     int
		endH, endM         int
	}
	cases := []struct {
		name       string
		row        hold
		constraint string // "" = must insert
	}{
		{"same bay overlapping", hold{seed.VehicleB, seed.BayN2, seed.TechCasey, 10, 30, 11, 30}, store.ConstraintBayOverlap},
		{"same bay inside", hold{seed.VehicleB, seed.BayN2, seed.TechCasey, 10, 15, 10, 45}, store.ConstraintBayOverlap},
		{"same technician overlapping", hold{seed.VehicleB, seed.BayN3, seed.TechBlair, 9, 30, 10, 30}, store.ConstraintTechOverlap},
		{"same technician enclosing", hold{seed.VehicleB, seed.BayN3, seed.TechBlair, 9, 0, 12, 0}, store.ConstraintTechOverlap},
		{"same vehicle elsewhere", hold{seed.VehicleA, seed.BayN3, seed.TechCasey, 10, 30, 11, 30}, store.ConstraintVehicleOverlap},
		{"same bay and technician back to back after", hold{seed.VehicleB, seed.BayN2, seed.TechBlair, 11, 0, 12, 0}, ""},
		{"same bay and technician back to back before", hold{seed.VehicleC, seed.BayN2, seed.TechBlair, 9, 0, 10, 0}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, at := newBase(t)
			id, err := rawAppointment(ctx, pool, tc.row.vehicle, tc.row.bay, tc.row.tech,
				at(tc.row.startH, tc.row.startM), at(tc.row.endH, tc.row.endM), domain.StatusConfirmed)
			if err == nil {
				cleanupRows(ctx, t, pool, id)
			}
			if tc.constraint == "" {
				if err != nil {
					t.Fatalf("half-open [start, end) rows must not collide: %v", err)
				}
				return
			}
			if got := store.ExclusionConstraint(err); got != tc.constraint {
				t.Fatalf("want %s violation, got %q (%v)", tc.constraint, got, err)
			}
		})
	}

	// Released rows (completed, cancelled, no-show) hold nothing: the same bay, technician, and
	// vehicle can be booked over them, and a released row can be written over a hold.
	for _, status := range []domain.AppointmentStatus{domain.StatusCompleted, domain.StatusCancelled, domain.StatusNoShow} {
		t.Run("over a released row "+status.Name(), func(t *testing.T) {
			base, at := newBase(t)
			if _, err := pool.Exec(ctx, `UPDATE appointments SET status = $2 WHERE id = $1`, base, int16(status)); err != nil {
				t.Fatal(err)
			}
			id, err := rawAppointment(ctx, pool, seed.VehicleA, seed.BayN2, seed.TechBlair, at(10, 0), at(11, 0), domain.StatusConfirmed)
			if err != nil {
				t.Fatalf("a %s row must not block: %v", status.Name(), err)
			}
			cleanupRows(ctx, t, pool, id)
			// While the new row holds, the old one cannot come back to a holding status.
			_, err = pool.Exec(ctx, `UPDATE appointments SET status = `+code(domain.StatusConfirmed)+` WHERE id = $1`, base)
			if store.ExclusionConstraint(err) == "" {
				t.Fatalf("re-holding an overlapping released row must violate GiST, got %v", err)
			}
			released, err := rawAppointment(ctx, pool, seed.VehicleA, seed.BayN2, seed.TechBlair, at(10, 0), at(11, 0), status)
			if err != nil {
				t.Fatalf("a %s row over a hold must insert: %v", status.Name(), err)
			}
			cleanupRows(ctx, t, pool, released)
		})
	}
}

// cleanupRows deletes the appointments ids when t ends.
func cleanupRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.WithoutCancel(ctx), `DELETE FROM appointments WHERE id = ANY($1)`, ids); err != nil {
			t.Error(err)
		}
	})
}

// One user resending one Idempotency-Key with different bodies at once: the first committed body
// wins, every other gets 409 (never a 500, never a second appointment for the key).
func TestSameKeyDifferentPayloadsOneCreated(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	day := londonDay(t, 1)
	key := "same-key-" + uuid.NewString()
	const n = 8
	var created, conflicts atomic.Int32
	ids := make([]uuid.UUID, n)
	raceN(n, func(i int) {
		// Distinct starts (08:00, 08:30, …) at Riverside: each body alone would be bookable.
		item, ok, err := svc.Book(ctx, customerAmelia, booking.BookRequest{
			DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceOil,
			StartAt: londonOn(t, day, 8, 0).Add(time.Duration(i) * 30 * time.Minute), IdempotencyKey: key,
		})
		switch {
		case err == nil && ok:
			created.Add(1)
			ids[i] = item.ID
		case err == nil:
			t.Errorf("body %d replayed another body's appointment %s", i, item.ID)
		default:
			if e, isDomain := domain.AsError(err); isDomain && e.Code == domain.CodeConflict {
				conflicts.Add(1)
				return
			}
			t.Errorf("body %d: want 409, got %v", i, err)
		}
	})
	if created.Load() != 1 || conflicts.Load() != n-1 {
		t.Fatalf("created=%d conflicts=%d, want 1 and %d", created.Load(), conflicts.Load(), n-1)
	}
	if rows := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE idempotency_key = $1`, key); rows != 1 {
		t.Fatalf("%d appointments for one key, want 1", rows)
	}
	for _, id := range ids {
		if id != uuid.Nil {
			assertEventsFor(t, pool, id)
		}
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
