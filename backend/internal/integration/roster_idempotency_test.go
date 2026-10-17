package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Concurrent retries of one keyed booking with the Redis cache in front: the first attempts
// (cache empty) create exactly one appointment and every other replays it; then retries are
// answered from Redis (proven with a second key cached in Redis only: Postgres has no row for it,
// so only the cache can replay it), and a different body under the key is a 409 from the cache.
func TestConcurrentIdempotentRetriesThroughRedis(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	idem := testRedis(t)
	svc := booking.New(st, idem, booking.WithClock(startOfLondonDay))
	scope := customerAmelia.UserID.String()
	for round, dealer := range []uuid.UUID{seed.DealerNorth, seed.DealerRiver} { // with and without staging
		req := oilAt(dealer, seed.VehicleA, londonOn(t, londonDay(t, 1+round), 14, 0))
		req.IdempotencyKey = "redis-" + uuid.NewString()
		const n = 8
		id := raceSameKey(t, svc, req, n)
		rec, ok, err := idem.Get(ctx, scope, req.IdempotencyKey)
		if err != nil || !ok || rec.AppointmentID != id {
			t.Fatalf("round %d: cache entry %+v ok=%v err=%v, want appointment %s", round, rec, ok, err, id)
		}

		cachedOnly := req
		cachedOnly.IdempotencyKey = "redis-only-" + uuid.NewString()
		if err := idem.Put(ctx, scope, cachedOnly.IdempotencyKey, rec); err != nil {
			t.Fatal(err)
		}
		other := req
		other.StartAt = req.StartAt.Add(30 * time.Minute)
		raceN(2*n, func(i int) {
			if i%2 == 1 {
				_, _, err := svc.Book(ctx, customerAmelia, other)
				if e, isDomain := domain.AsError(err); !isDomain || e.Code != domain.CodeConflict {
					t.Errorf("different body under the key: want 409, got %v", err)
				}
				return
			}
			item, ok, err := svc.Book(ctx, customerAmelia, cachedOnly)
			if err != nil || ok || item.ID != id {
				t.Errorf("retry through Redis: created=%v id=%s err=%v, want replay of %s", ok, item.ID, err, id)
			}
		})
		if rows := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE idempotency_key = ANY($1)`,
			[]string{req.IdempotencyKey, cachedOnly.IdempotencyKey}); rows != 1 {
			t.Fatalf("round %d: %d appointments for the keys, want 1", round, rows)
		}
		assertEventsFor(t, pool, id)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}

// The stranded-booking and roster checks compare against the service clock, not Postgres now():
// with the clock pinned at midnight a 14:00 booking today is upcoming, so every change that would
// strand it is a 409 even when the suite runs after 14:00; with the clock at 15:00 it is over, so
// the same changes go through even when the suite runs in the morning.
func TestStrandedChecksUseServiceClock(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today, _ := suiteToday()
	if _, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleA, seed.ServiceOil, londonAt(t, 14, 0)); err != nil {
		t.Fatal(err)
	}
	cutHours := []domain.WeekdayHours{{Weekday: today.ISOWeekday(), OpenTime: "08:00", CloseTime: "12:00"}}
	absence := domain.TechnicianAbsence{StartsAt: londonAt(t, 13, 0), EndsAt: londonAt(t, 16, 0), Reason: "Dentist"}
	changes := []struct {
		name string
		run  func(svc *booking.Service) error
	}{
		{"weekday hours", func(svc *booking.Service) error {
			_, err := svc.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, cutHours, 0)
			return err
		}},
		{"default hours", func(svc *booking.Service) error {
			_, err := svc.UpdateDealership(ctx, staffAdmin, seed.DealerRiver, store.DealershipPatch{CloseTime: new("12:00")})
			return err
		}},
		{"closure", func(svc *booking.Service) error {
			_, err := svc.AddClosure(ctx, staffAdmin, seed.DealerRiver, domain.Closure{Date: today.String()}, 0)
			return err
		}},
		{"absence", func(svc *booking.Service) error {
			_, err := svc.AddAbsence(ctx, staffAdmin, seed.TechEve, absence, 0)
			return err
		}},
		{"deactivation", func(svc *booking.Service) error {
			_, err := svc.UpdateTechnician(ctx, staffAdmin, seed.TechEve, store.TechnicianPatch{Active: new(false)})
			return err
		}},
	}
	for _, c := range changes {
		expectCode(t, c.run(svc), domain.CodeConflict)
	}
	at1500 := serviceOn(t, st, londonDay(t, 0), 15, 0)
	for _, c := range changes {
		if err := c.run(at1500); err != nil {
			t.Fatalf("%s at 15:00, after the booking ended: %v", c.name, err)
		}
	}
	// The read views use the same clock: today's closure and the absence are listed at 15:00 and
	// gone once the clock has moved past them.
	cal, err := at1500.Calendar(ctx, staffAdmin, seed.DealerRiver)
	if err != nil || len(cal.Closures) != 1 {
		t.Fatalf("calendar at 15:00: %+v %v", cal.Closures, err)
	}
	tomorrow := serviceOn(t, st, londonDay(t, 1), 0, 0)
	if cal, err := tomorrow.Calendar(ctx, staffAdmin, seed.DealerRiver); err != nil || len(cal.Closures) != 0 {
		t.Fatalf("calendar tomorrow lists past closures: %+v %v", cal.Closures, err)
	}
	if r, err := at1500.Roster(ctx, staffAdmin, seed.TechEve); err != nil || len(r.Absences) != 1 {
		t.Fatalf("roster at 15:00: %+v %v", r.Absences, err)
	}
	if r, err := tomorrow.Roster(ctx, staffAdmin, seed.TechEve); err != nil || len(r.Absences) != 0 {
		t.Fatalf("roster tomorrow lists an ended absence: %+v %v", r.Absences, err)
	}
}
