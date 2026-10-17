package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// insertRawHold inserts a confirmed appointment row in tx (no service checks, no event), so only
// the GiST constraints stand between it and a concurrent booking. Left uncommitted, it makes a
// booking that collides with it wait on the constraint until tx ends.
func insertRawHold(ctx context.Context, tx pgx.Tx, dealer, vehicle, bay, tech uuid.UUID, start, end time.Time) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id,
			service_bay_id, technician_id, start_at, end_at, status,
			booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
		SELECT $1, $2, v.customer_id, v.id, $3, $4, $5, $6, $7, 1, $8, 2, '{2}', 2 FROM vehicles v WHERE v.id = $9`,
		uuid.New(), dealer, seed.ServiceBrake, bay, tech, start, end, int(end.Sub(start).Minutes()), vehicle)
	return err
}

// gistFallbackCase blocks the booking's first-choice bay or technician with an uncommitted row,
// waits until the booking's insert is queued behind it, then commits or rolls the row back.
type gistFallbackCase struct {
	name               string
	dealer, service    uuid.UUID
	duration           time.Duration
	blockBay, blockTec uuid.UUID // the uncommitted row's bay and technician (one of them collides)
	commit             bool
	wantBay, wantTech  uuid.UUID // where the booking must land; uuid.Nil with wantCode set = refused
	wantCode           string
}

// The candidate loop's fallbacks (allocate): when the first-choice bay is taken by a booking that
// commits while ours waits on its GiST entry, ours moves to the next bay; when the technician is
// taken, to the next technician; with no candidate left it is a 409 conflict, never a 500. When
// the blocker rolls back instead, the first choice is kept. (An oil change whose express bay is
// taken spills to the general bay, so the bay side of "nothing left" is
// TestCandidateLoopNoBayLeftIsConflict.)
func TestCandidateLoopGiSTFallbacks(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	// Northside brake: general bays Bay 2 then Bay 3 (name order, equal gaps); Blair then Casey
	// (both level 3 with brakes, equal load, name order). Riverside oil: Bay A and Eve only.
	brake, oil := 90*time.Minute, 30*time.Minute
	cases := []gistFallbackCase{
		{"bay taken, next bay", seed.DealerNorth, seed.ServiceBrake, brake, seed.BayN2, seed.TechDrew, true, seed.BayN3, seed.TechBlair, ""},
		{"bay freed, first bay kept", seed.DealerNorth, seed.ServiceBrake, brake, seed.BayN2, seed.TechDrew, false, seed.BayN2, seed.TechBlair, ""},
		{"technician taken, next technician", seed.DealerNorth, seed.ServiceBrake, brake, seed.BayN4, seed.TechBlair, true, seed.BayN2, seed.TechCasey, ""},
		{"technician freed, first technician kept", seed.DealerNorth, seed.ServiceBrake, brake, seed.BayN4, seed.TechBlair, false, seed.BayN2, seed.TechBlair, ""},
		{"express bay taken, spills to the general bay", seed.DealerRiver, seed.ServiceOil, oil, seed.BayR1, seed.TechFinn, true, seed.BayR2, seed.TechEve, ""},
		{"only technician taken", seed.DealerRiver, seed.ServiceOil, oil, seed.BayR2, seed.TechEve, true, uuid.Nil, uuid.Nil, domain.CodeConflict},
		{"only bay freed", seed.DealerRiver, seed.ServiceOil, oil, seed.BayR1, seed.TechFinn, false, seed.BayR1, seed.TechEve, ""},
	}
	for round := range 2 { // every case twice, each run on its own day
		for i, tc := range cases {
			day := londonDay(t, 1+round*len(cases)+i)
			t.Run(day.String()+" "+tc.name, func(t *testing.T) {
				runGiSTFallback(t, svc, pool, tc, londonOn(t, day, 14, 0))
			})
		}
	}
	assertNoOverlaps(t, pool) // not assertAllocationsValid: the blockers are raw rows, unqualified on purpose
	assertNoOrphanEvents(t, pool)
	assertStagingWithinCapacity(t, pool)
}

func runGiSTFallback(t *testing.T, svc *booking.Service, pool *pgxpool.Pool, tc gistFallbackCase, start time.Time) {
	t.Helper()
	ctx := t.Context()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if err := insertRawHold(ctx, blocker, tc.dealer, seed.VehicleC, tc.blockBay, tc.blockTec, start, start.Add(tc.duration)); err != nil {
		t.Fatal(err)
	}

	type result struct {
		item domain.Appointment
		err  error
	}
	done := make(chan result, 1)
	go func() {
		item, err := bookAt(t, svc, tc.dealer, seed.VehicleA, tc.service, start)
		done <- result{item, err}
	}()
	// The booking's candidates were read without the uncommitted row, so it picks the blocked
	// first choice and its insert waits on the blocker's GiST entry. Only then end the blocker.
	awaitLockWaiter(t, pool, "INSERT INTO appointments")
	if tc.commit {
		err = blocker.Commit(ctx)
	} else {
		err = blocker.Rollback(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	r := <-done
	if tc.wantCode != "" {
		expectCode(t, r.err, tc.wantCode)
		return
	}
	if r.err != nil {
		t.Fatalf("booking: %v", r.err)
	}
	if r.item.ServiceBayID != tc.wantBay || r.item.TechnicianID != tc.wantTech {
		t.Fatalf("landed on %s / %s, want bay %s / technician %s", r.item.ServiceBayName, r.item.TechnicianName, tc.wantBay, tc.wantTech)
	}
	assertEventsFor(t, pool, r.item.ID)
}

// The bay side of the candidate loop running dry: Riverside's general bay (Bay B, the express
// spill) is already held, so when the uncommitted blocker on the express bay (Bay A) commits, the
// oil change has no bay left: 409, never a 500. The general bay's hold is a brake job by a
// technician of its own, which no other bay or technician fits, so no move can make room.
func TestCandidateLoopNoBayLeftIsConflict(t *testing.T) {
	t.Parallel()
	pool, _, svc, _ := openTestDB(t)
	holder := addTechnician(t, svc, seed.DealerRiver, "Bay B Holder", 2, domain.SkillBrakes)
	for i := range 2 {
		start := londonOn(t, londonDay(t, 1+i), 14, 0)
		hold, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := insertRawHold(t.Context(), hold, seed.DealerRiver, seed.VehicleB, seed.BayR2, holder, start.Add(-time.Hour), start.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := hold.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		runGiSTFallback(t, svc, pool, gistFallbackCase{
			name: "no bay left", dealer: seed.DealerRiver, service: seed.ServiceOil, duration: 30 * time.Minute,
			blockBay: seed.BayR1, blockTec: seed.TechFinn, commit: true, wantCode: domain.CodeConflict,
		}, start)
	}
	assertNoOverlaps(t, pool)
}
