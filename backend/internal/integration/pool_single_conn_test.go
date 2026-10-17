package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// The second-pool-connection rule for every booking write, not only transitions (see
// TestTransitionsNeverWaitForASecondPoolConnection): three times as many concurrent Book,
// Reschedule, Reinstate, Reassign, and Extend calls as the pool has connections must all complete.
// A write that took a second connection while holding its TX's would wait forever once every
// connection is held by such a write (here: until the 20 s deadline).
func TestBookingWritesNeverWaitForASecondPoolConnection(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)
	const conns = 4
	today, day2, day3 := londonDay(t, 0), londonDay(t, 2), londonDay(t, 3)
	at := func(day booking.Date, h, m int) time.Time { return londonOn(t, day, h, m) }

	// Fixtures, each op on its own vehicle and window so none of them can lose to another.
	var moves, reinstates, reassigns, extends []domain.Appointment
	for _, h := range []int{8, 9, 10} { // Riverside day 3: moved to 13:00–15:00
		moves = append(moves, mustBook(t, svc, oilAt(seed.DealerRiver, newVehicle(t, svc), at(day3, h, 0))))
	}
	for _, h := range []int{14, 15} { // Riverside today, then marked no-show
		item := mustBook(t, svc, oilAt(seed.DealerRiver, newVehicle(t, svc), at(today, h, 0)))
		mustExec(t, pool, `UPDATE appointments SET status = `+code(domain.StatusNoShow)+`, version = version + 1 WHERE id = $1`, item.ID)
		reinstates = append(reinstates, item)
	}
	for _, hm := range [][2]int{{14, 0}, {15, 30}} { // Northside brake jobs: Blair, then Casey
		reassigns = append(reassigns, mustBook(t, svc, brakeAt(seed.DealerNorth, newVehicle(t, svc), at(today, hm[0], hm[1]))))
	}
	for _, h := range []int{16, 17} { // Riverside today, checked in
		item := mustBook(t, svc, oilAt(seed.DealerRiver, newVehicle(t, svc), at(today, h, 0)))
		item, err := svc.CheckIn(ctx, staffDispatcher, item.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		extends = append(extends, item)
	}

	opts := store.DefaultPoolOptions()
	opts.MaxConns = conns
	tight := booking.New(store.New(newPoolLike(t, pool, opts)), nil, booking.WithClock(startOfLondonDay))
	deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var done atomic.Int32
	var ops []func()
	run := func(name string, op func() error) {
		ops = append(ops, func() {
			if err := op(); err != nil {
				t.Errorf("%s: %v", name, err)
				return
			}
			done.Add(1)
		})
	}
	for _, h := range []int{8, 9, 10} {
		req := oilAt(seed.DealerRiver, newVehicle(t, svc), at(day2, h, 0)) // Riverside day 2
		run("book", func() error {
			_, _, err := tight.Book(deadline, staffAdmin, req)
			return err
		})
	}
	for i, item := range moves {
		run("reschedule", func() error {
			_, err := tight.Reschedule(deadline, staffDispatcher, item.ID, booking.RescheduleRequest{StartAt: at(day3, 13+i, 0)})
			return err
		})
	}
	for _, item := range reinstates {
		run("reinstate", func() error {
			_, err := tight.Reinstate(deadline, staffDispatcher, item.ID, 0)
			return err
		})
	}
	for _, item := range reassigns {
		other := seed.TechCasey
		if item.TechnicianID == seed.TechCasey {
			other = seed.TechBlair
		}
		run("reassign", func() error {
			_, err := tight.Reassign(deadline, staffDispatcher, item.ID, booking.ReassignRequest{TechnicianID: other})
			return err
		})
	}
	for _, item := range extends {
		run("extend", func() error {
			_, err := tight.Extend(deadline, staffDispatcher, item.ID, 15, 0)
			return err
		})
	}
	race(ops...)
	if want := len(ops); int(done.Load()) != want {
		t.Fatalf("completed %d of %d booking writes on a %d-connection pool", done.Load(), want, conns)
	}
	if len(ops) < 3*conns {
		t.Fatalf("%d ops, want at least %d to saturate the pool", len(ops), 3*conns)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
}
