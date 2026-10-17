package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// burstBudget is a coarse guard on one burst (generous for -race on small CI runners). Without the
// arrival locks a burst of n inserts conflicting on one GiST constraint could deadlock in a cascade
// that Postgres broke one cycle per deadlock_timeout (1 s): tens of seconds for 40 contenders, the
// tail ending in statement_timeout (a 503, which already fails the clean-loss check) with no winner.
const burstBudget = 20 * time.Second

// TestSlotBurst: many simultaneous bookings of one slot, in every shape the API sees them.
// Riverside's oil slot holds exactly one car (one express bay, one lubrication technician; the
// availability says so before each burst), so exactly one booking wins however the racers come:
//   - distinct cars through one service (one instance: its admission gate in front of the day lock);
//   - distinct cars with a service per racer (as many instances: all reach the database at once);
//   - distinct cars through two services on two pools (the two replicas);
//   - distinct cars, an instance per racer, half of them only trying the day lock before reading
//     and half waiting for it (dayLockPaths): a racer that read stale candidates collides and
//     re-reads under the lock, one that read under it fails at once;
//   - one car, 40 racers, three slots (double clicks, two tabs, customer and dispatcher at once):
//     they must queue on the vehicle + day locks instead of deadlocking on the vehicle exclusion
//     constraint (Postgres would break one cycle per deadlock_timeout: a tail of 503s, maybe no winner).
//
// At Northside two bays and two brake technicians are free, so only the vehicle rule can stop one
// car's racers: every loser is a 409. In every shape the losers lose cleanly within burstBudget, the
// winner has its events and no loser left any, the slot holds one booking, the holds never overlap,
// and no place is lost: one more car is refused where the slot held one, and fits where it held two.
func TestSlotBurst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		dealer   uuid.UUID
		service  uuid.UUID
		n        int
		services int  // booking.Service values the racers are spread over, round robin
		pools    int  // connection pools those services are spread over, round robin
		sameCar  bool // every racer books the same (fresh) car
		paths    bool // alternate the racers' services between the day lock tried and waited for
		hours    []int
		capacity int      // bookings each slot holds before the burst
		lose     []string // codes a loser may get
	}{
		{"distinct cars, one service", seed.DealerRiver, seed.ServiceOil, 20, 1, 1, false, false, []int{15}, 1, refusals},
		{"distinct cars, a service per racer", seed.DealerRiver, seed.ServiceOil, 24, 24, 1, false, false, []int{14}, 1, refusals},
		{"distinct cars, two instances", seed.DealerRiver, seed.ServiceOil, 16, 2, 2, false, false, []int{10}, 1, refusals},
		{"distinct cars, day lock tried and waited for", seed.DealerRiver, seed.ServiceOil, 16, 16, 1, false, true, []int{9, 13, 15}, 1, refusals},
		{"one car, 40 racers", seed.DealerRiver, seed.ServiceOil, 40, 1, 1, true, false, []int{9, 11, 15}, 1, refusals},
		{"one car, room for two", seed.DealerNorth, seed.ServiceBrake, 8, 1, 1, true, false, []int{14}, 2, []string{domain.CodeConflict}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pool, st, svc, ctx := openTestDB(t)
			stores := []*store.Store{st}
			for range tc.pools - 1 {
				stores = append(stores, store.New(newPoolLike(t, pool, store.DefaultPoolOptions())))
			}
			racers := make([]*booking.Service, tc.services)
			for i := range racers {
				racers[i] = atStartOfLondonDay(stores[i%len(stores)])
				if tc.paths {
					racers[i] = dayLockPaths[i%len(dayLockPaths)].instance(stores[i%len(stores)])
				}
			}
			day := londonDay(t, 0)
			for _, hour := range tc.hours {
				label := fmt.Sprintf("%02d:00", hour)
				start := londonOn(t, day, hour, 0)
				vehicles := make([]uuid.UUID, tc.n)
				for i := range vehicles {
					if i == 0 || !tc.sameCar {
						vehicles[i] = newVehicle(t, svc) // fresh: the seed books its own vehicles today
					} else {
						vehicles[i] = vehicles[0]
					}
				}
				if got := slotCapacity(t, svc, tc.dealer, vehicles[0], tc.service, day, start); got != tc.capacity {
					t.Fatalf("%s: the slot holds %d bookings, want %d (fixture changed?)", label, got, tc.capacity)
				}
				items := make([]domain.Appointment, tc.n)
				errs := make([]error, tc.n)
				began := time.Now()
				raceN(tc.n, func(i int) {
					var created bool
					items[i], created, errs[i] = racers[i%len(racers)].Book(ctx, staffAdmin, booking.BookRequest{
						DealershipID: tc.dealer, VehicleID: vehicles[i], ServiceTypeID: tc.service,
						StartAt: start, IdempotencyKey: fmt.Sprintf("test:burst:%d:%s", i, uuid.NewString()),
					})
					if errs[i] == nil && !created {
						errs[i] = fmt.Errorf("racer %d: a fresh key replayed", i)
					}
				})
				took := time.Since(began)
				w := oneWinner(t, label, allLose(errs, tc.lose...)...)
				if took > burstBudget {
					t.Fatalf("%s: %d bookings took %v, want < %v (deadlock cascade?)", label, tc.n, took, burstBudget)
				}
				assertEventsFor(t, pool, items[w].ID)
				if held := scalar[int](t, pool, `
					SELECT count(*)::int FROM appointments WHERE dealership_id = $1 AND start_at = $2 AND status IN (`+holdingCodes+`)`,
					tc.dealer, start); held != 1 {
					t.Fatalf("%s: %d holds at the slot, want 1", label, held)
				}
				// Every loser lost to the real winner: the burst left exactly capacity-1 places.
				_, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
					DealershipID: tc.dealer, VehicleID: newVehicle(t, svc), ServiceTypeID: tc.service,
					StartAt: start, IdempotencyKey: "test:burst:after:" + uuid.NewString(),
				})
				switch {
				case tc.capacity > 1 && err != nil:
					t.Fatalf("%s: another car after the burst: %v, want booked (the slot had room for %d)", label, err, tc.capacity)
				case tc.capacity == 1 && !loses(err, refusals...).lostCleanly():
					t.Fatalf("%s: another car after the burst got %v, want a refusal: the burst left capacity unused", label, err)
				}
			}
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
			assertNoOrphanEvents(t, pool)
		})
	}
}

// slotCapacity is how many bookings of service the slot at start holds per the availability
// (free bays vs free technicians), or -1 when it is not available.
func slotCapacity(t *testing.T, svc *booking.Service, dealer, vehicle, service uuid.UUID, day booking.Date, start time.Time) int {
	t.Helper()
	avail, err := svc.Availability(t.Context(), staffAdmin, booking.AvailabilityQuery{
		DealershipID: dealer, VehicleID: vehicle, ServiceTypeID: service, Day: day,
	})
	if err != nil {
		t.Fatal(err)
	}
	slot := slotAt(t, avail, start)
	if !slot.Available {
		return -1
	}
	return min(slot.FreeBays, slot.FreeTechnicians)
}
