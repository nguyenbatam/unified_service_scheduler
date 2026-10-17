package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// rosterEdit is an open roster writer's TX holding a technician FOR UPDATE, as the admin roster
// endpoints do, with its transaction id (what a session waiting for that row waits on).
type rosterEdit struct {
	tx  pgx.Tx
	xid string
}

// editRoster locks tech FOR UPDATE in a TX of its own, plus, when absent, an absence over [from, to)
// committed only with the TX.
func editRoster(t *testing.T, pool *pgxpool.Pool, tech uuid.UUID, absent bool, from, to time.Time) rosterEdit {
	t.Helper()
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(ctx, `SELECT 1 FROM technicians WHERE id = $1 FOR UPDATE`, tech); err != nil {
		t.Fatal(err)
	}
	if absent {
		if _, err := tx.Exec(ctx, `INSERT INTO technician_absences (id, technician_id, starts_at, ends_at) VALUES ($1, $2, $3, $4)`,
			uuid.New(), tech, from, to); err != nil {
			t.Fatal(err)
		}
	}
	var xid string
	if err := tx.QueryRow(ctx, `SELECT (pg_current_xact_id()::text::bigint % 4294967296)::text`).Scan(&xid); err != nil {
		t.Fatal(err)
	}
	return rosterEdit{tx: tx, xid: xid}
}

// awaitWaiterOn waits until a session is blocked on the roster edit's transaction and returns the
// statement it is running. pg_locks and pg_stat_activity are separate snapshots, so a session
// that has just started waiting can still show its previous statement (the savepoint rollback
// that precedes the wait): the answer counts only once pg_stat_activity itself reports a lock
// wait, and the same session and statement are seen twice in a row (the wait lasts until the
// test ends the edit).
func awaitWaiterOn(t *testing.T, pool *pgxpool.Pool, e rosterEdit) string {
	t.Helper()
	var last string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var pid int
		var query string
		err := pool.QueryRow(t.Context(), `
			SELECT a.pid, a.query FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
			WHERE l.locktype = 'transactionid' AND NOT l.granted AND l.transactionid::text = $1
				AND a.wait_event_type = 'Lock'
			LIMIT 1`, e.xid).Scan(&pid, &query)
		switch {
		case err == nil:
			seen := fmt.Sprintf("%d\x00%s", pid, query)
			if seen == last {
				return query
			}
			last = seen
		case errors.Is(err, pgx.ErrNoRows):
			last = ""
		default:
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no session blocked on roster edit %s within 10s", e.xid)
		}
	}
}

// giveBackWait is the statement a booking waits for a technician with after giving its day lock
// back (store.Tx.ShareLockTechnician); plainWait is the plain pass's own check (LockBookableTechnician).
const giveBackWait, plainWait = "SELECT 1 FROM technicians WHERE id = $1 FOR SHARE", "SELECT true FROM technicians"

// Four technicians could take the job, each under a roster edit; the first three edits end by
// marking their technician absent, one after another. The booking gives its day lock back for each
// of the first three (waiting for each outside the day's queue, never holding the day lock), and
// after maxGiveBacks (3) the last pass is plain lock-last: it waits for the fourth in its own
// technician check — still before taking the day lock, as lock-last does — and gets it once that
// edit ends without a change.
func TestGiveBacksBoundedThenPlainPass(t *testing.T) {
	t.Parallel()
	for _, path := range dayLockPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			pool, st, svc, ctx := openTestDB(t)
			d := provisionShop(t, svc, "Bounded give-backs")
			if _, err := svc.CreateBay(ctx, staffAdmin, d.ID, "Lane", domain.BayExpress); err != nil {
				t.Fatal(err)
			}
			techs := make([]uuid.UUID, 4)
			for i := range techs {
				tech, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{
					DealershipID: d.ID, Name: fmt.Sprintf("Lube %d", i+1), SkillLevel: 1, Skills: []domain.TechnicianSkill{domain.SkillLubrication},
				})
				if err != nil {
					t.Fatal(err)
				}
				techs[i] = tech.ID // equal in every rank but name: tried in this order
			}
			day := londonDay(t, 1)
			at := londonOn(t, day, 10, 0)
			edits := make([]rosterEdit, len(techs))
			for i, tech := range techs {
				_, loc := suiteToday()
				edits[i] = editRoster(t, pool, tech, i < 3, day.Start(loc), day.Next(loc))
			}
			type result struct {
				appt domain.Appointment
				err  error
			}
			done := make(chan result, 1)
			car := newVehicle(t, svc)
			go func() {
				waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				appt, _, err := path.instance(st).Book(waitCtx, staffAdmin, booking.BookRequest{
					DealershipID: d.ID, VehicleID: car, ServiceTypeID: seed.ServiceOil, StartAt: at, IdempotencyKey: uuid.NewString(),
				})
				done <- result{appt, err}
			}()
			for i, e := range edits {
				want := giveBackWait
				if i == 3 {
					want = plainWait
				}
				if q := awaitWaiterOn(t, pool, e); !strings.Contains(q, want) {
					t.Fatalf("technician %d: the booking waits in %q, want %q", i+1, q, want)
				}
				if !dayLockFree(t, pool, d.ID, at) {
					t.Fatalf("technician %d: the booking waits for it holding the dealership-day lock", i+1)
				}
				end := e.tx.Commit
				if i == 3 {
					end = e.tx.Rollback
				}
				if err := end(ctx); err != nil {
					t.Fatal(err)
				}
			}
			r := <-done
			if r.err != nil {
				t.Fatalf("booking after the roster edits: %v", r.err)
			}
			if r.appt.TechnicianID != techs[3] {
				t.Fatalf("booked technician %s, want the fourth (the others are absent now)", r.appt.TechnicianID)
			}
			if n := scalar[int](t, pool, `SELECT count(*)::int FROM appointments WHERE dealership_id = $1`, d.ID); n != 1 {
				t.Fatalf("%d appointments at the shop, want 1", n)
			}
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
		})
	}
}

// A give-back after a repair move was applied undoes it: the booking moves a confirmed oil change
// out of the only general bay (bay repair, under the day lock), then finds its technician under a
// roster edit and rolls the day-lock savepoint back — the move, its row lock, and its event with
// it. Once the edit ends the next pass moves the oil change again and books: the oil change ends up
// moved exactly once (one version, one APPOINTMENT_REASSIGNED event and history row).
func TestGiveBackUndoesAnAppliedRepairMove(t *testing.T) {
	t.Parallel()
	for _, path := range dayLockPaths {
		t.Run(path.name, func(t *testing.T) {
			t.Parallel()
			pool, st, svc, ctx := openTestDB(t)
			d := provisionShop(t, svc, "Undone move")
			bays := map[domain.BayType]uuid.UUID{}
			for _, bt := range []domain.BayType{domain.BayExpress, domain.BayGeneral} {
				bay, err := svc.CreateBay(ctx, staffAdmin, d.ID, bt.Name(), bt)
				if err != nil {
					t.Fatal(err)
				}
				bays[bt] = bay.ID
			}
			var brakes uuid.UUID
			for _, tc := range []struct {
				name   string
				level  int16
				skills []domain.TechnicianSkill
			}{
				{"Lube A", 1, []domain.TechnicianSkill{domain.SkillLubrication}},
				{"Lube B", 1, []domain.TechnicianSkill{domain.SkillLubrication}},
				{"Brakes", 2, []domain.TechnicianSkill{domain.SkillBrakes}},
			} {
				tech, err := svc.CreateTechnician(ctx, staffAdmin, store.NewTechnician{DealershipID: d.ID, Name: tc.name, SkillLevel: domain.SkillLevel(tc.level), Skills: tc.skills})
				if err != nil {
					t.Fatal(err)
				}
				brakes = tech.ID // the last one
			}
			day := londonDay(t, 1)
			at := londonOn(t, day, 10, 0)
			// The first oil change takes the quick lane, the second spills into the general bay; the
			// first is then cancelled, so the second could move back to the quick lane.
			blocker, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceOil, at)
			if err != nil {
				t.Fatal(err)
			}
			oil, err := bookAt(t, svc, d.ID, newVehicle(t, svc), seed.ServiceOil, at)
			if err != nil {
				t.Fatal(err)
			}
			if oil.ServiceBayID != bays[domain.BayGeneral] {
				t.Fatalf("the second oil change is in bay %s, want the general bay (fixture changed?)", oil.ServiceBayID)
			}
			if _, err := svc.Cancel(ctx, staffAdmin, blocker.ID, 0); err != nil {
				t.Fatal(err)
			}
			edit := editRoster(t, pool, brakes, false, time.Time{}, time.Time{})

			type result struct {
				appt domain.Appointment
				err  error
			}
			done := make(chan result, 1)
			car := newVehicle(t, svc)
			go func() {
				waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				appt, _, err := path.instance(st).Book(waitCtx, staffAdmin, booking.BookRequest{
					DealershipID: d.ID, VehicleID: car, ServiceTypeID: seed.ServiceBrake, StartAt: at, IdempotencyKey: uuid.NewString(),
				})
				done <- result{appt, err}
			}()
			if q := awaitWaiterOn(t, pool, edit); !strings.Contains(q, giveBackWait) {
				t.Fatalf("the booking waits in %q, want the give-back wait %q", q, giveBackWait)
			}
			if !dayLockFree(t, pool, d.ID, at) {
				t.Fatal("the booking waits for its technician holding the dealership-day lock")
			}
			if !rowUnlocked(t, pool, oil.ID) {
				t.Fatal("the move the booking gave back still holds the oil change's row")
			}
			if err := edit.tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			r := <-done
			if r.err != nil {
				t.Fatalf("brake booking after the roster edit ended: %v", r.err)
			}
			if r.appt.ServiceBayID != bays[domain.BayGeneral] || r.appt.TechnicianID != brakes {
				t.Fatalf("brake booking got bay %s / technician %s, want the general bay and the brake technician", r.appt.ServiceBayID, r.appt.TechnicianID)
			}
			var bay uuid.UUID
			var version int
			if err := pool.QueryRow(ctx, `SELECT service_bay_id, version FROM appointments WHERE id = $1`, oil.ID).Scan(&bay, &version); err != nil {
				t.Fatal(err)
			}
			if bay != bays[domain.BayExpress] || version != oil.Version+1 {
				t.Fatalf("oil change: bay %s version %d, want moved once to the quick lane (version %d)", bay, version, oil.Version+1)
			}
			outbox := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE aggregate_id = $1 AND event_type = `+code(domain.EventAppointmentReassigned), oil.ID)
			history := scalar[int](t, pool, `SELECT count(*)::int FROM appointment_history WHERE appointment_id = $1 AND event_type = `+code(domain.EventAppointmentReassigned), oil.ID)
			if outbox != 1 || history != 1 {
				t.Fatalf("APPOINTMENT_REASSIGNED for the oil change: %d outbox, %d history rows, want 1 each (the given-back move left none)", outbox, history)
			}
			assertNoOverlaps(t, pool)
			assertAllocationsValid(t, pool)
		})
	}
}
