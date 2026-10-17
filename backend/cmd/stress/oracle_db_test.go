package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
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

// The stress oracle (oracle.go, repair.go, rules.go: raw rows and the documented rules, never the
// engine's code) against the server's availability (booking.Service.Availability), so the
// independent oracle runs on every PR and not only in the cluster stress. The shop is the
// truth/fill workshop (shift patterns, a break, a dated absence, a specialty bay), once without and
// once with a staging cap, on a weekday and on a Saturday (one technician works weekdays only).
// Each day is filled by the engine itself (Service.Book, so every hold is one the engine would
// make, repairs included) with random job mixes, waiting customers and starts; some bookings are
// then checked in (a repair can no longer move them). A last day is set up by hand so that starts
// are offered only through the one-move repair, of a bay and of a technician. After every
// booking, every start of the day, for every job mix, waiting or not, must get the same verdict
// from both, and offered starts the same free bay and technician counts.
func TestOracleAgreesWithAvailability(t *testing.T) {
	t.Parallel() // a database of its own (integrityDB)
	_, pool := integrityDB(t)
	ctx := t.Context()
	st := store.New(pool)
	if err := seed.Run(ctx, st, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	j := &oracleJudge{t: t, pool: pool, svc: booking.New(st, nil), mixes: oracleTestMixes(t, pool)}
	vehicles := oracleTestVehicles(t, pool, 400)
	monday := booking.Date{Year: 2030, Month: time.January, Day: 7}
	saturday, tuesday := monday.AddDays(5), monday.AddDays(1)
	next := 0
	car := func() uuid.UUID {
		next++
		return vehicles[next-1]
	}
	var open workshop
	var openIDs map[string]uuid.UUID
	for i, shop := range []struct {
		name            string
		staging, window int16
	}{{"Oracle Open", 0, 60}, {"Oracle Staged", 2, 60}} {
		w, ids := insertTestWorkshop(t, pool, fillWorkshop, shop.name, shop.staging, shop.window, []booking.Date{monday, saturday, tuesday})
		if i == 0 {
			open, openIDs = w, ids
		}
		for k, day := range []booking.Date{monday, saturday} {
			rng := rand.New(rand.NewPCG(uint64(i), uint64(k))) //nolint:gosec // reproducible choices, not security
			probe := car()                                     // never booked: its own holds never refuse a start
			j.judge(w, day, probe, "empty")
			booked := 0
			for attempt := range 80 {
				mix := j.mixes[rng.IntN(len(j.mixes))]
				start := booking.Clock{Hour: 8}.On(day, w.loc).Add(time.Duration(rng.IntN(20)) * booking.SlotStep)
				appt, ok := j.book(w, mix, start, car(), rng.IntN(3) == 0, fmt.Sprintf("oracle-test:%d:%d:%d", i, k, attempt))
				if !ok {
					continue // full, lunch, past closing, staging: the oracle judges the day, not this answer
				}
				booked++
				if rng.IntN(5) == 0 {
					if _, err := pool.Exec(ctx, `UPDATE appointments SET status = `+sqlCode(domain.StatusCheckedIn)+` WHERE id = $1`, appt.ID); err != nil {
						t.Fatal(err)
					}
				}
				j.judge(w, day, probe, fmt.Sprintf("%d bookings", booked))
			}
			if booked < 10 {
				t.Fatalf("%s %s: only %d bookings, the day was barely exercised", shop.name, day, booked)
			}
		}
	}
	j.repairDay(open, openIDs, tuesday, car)
	if j.offered == 0 || j.refused == 0 || j.repairOnly == 0 {
		t.Fatalf("%d starts offered (%d only through a repair), %d refused: the comparison never saw every kind of verdict", j.offered, j.repairOnly, j.refused)
	}
	t.Logf("%d slot verdicts agreed (%d offered, %d of them only through a one-move repair; %d refused)", j.agreed, j.offered, j.repairOnly, j.refused)
}

// oracleJudge compares availability with the oracle and tallies what it saw.
type oracleJudge struct {
	t                                    *testing.T
	pool                                 *pgxpool.Pool
	svc                                  *booking.Service
	mixes                                []jobMix
	agreed, offered, repairOnly, refused int
}

var oracleTestAdmin = domain.Actor{UserID: seed.DemoAdminID, Role: domain.RoleAdmin}

// judge compares every start of day at w, for every job mix, waiting or not.
func (j *oracleJudge) judge(w workshop, day booking.Date, probe uuid.UUID, when string) {
	t := j.t
	t.Helper()
	for _, mix := range j.mixes {
		for _, waiter := range []bool{false, true} {
			avail, err := j.svc.Availability(t.Context(), oracleTestAdmin, booking.AvailabilityQuery{
				DealershipID: w.dealer.ID, VehicleID: probe, ServiceTypeIDs: mix.ids, Day: day, IsWaiter: waiter,
			})
			if err != nil {
				t.Fatalf("availability %s: %v", mix.name, err)
			}
			oracle, err := oracleDay(t.Context(), j.pool, w, mix.req, softUpgrade(mix.req, waiter, int(w.dealer.Policy.ExpressMaxMinutes)), day, mix.req.minutes)
			if err != nil {
				t.Fatalf("oracle %s: %v", mix.name, err)
			}
			var ts truthStats
			ts.compare(mix, avail, oracle)
			if ts.mismatches > 0 {
				t.Fatalf("%s %s (%s), %s waiter=%v: availability and the oracle disagree: %v", w.dealer.Name, day, when, mix.name, waiter, ts.failures)
			}
			j.agreed += ts.slotChecks
			for _, s := range avail.Slots {
				switch {
				case !s.Available:
					j.refused++
				case s.FreeBays == 0 || s.FreeTechnicians == 0:
					j.repairOnly++
					j.offered++
				default:
					j.offered++
				}
			}
		}
	}
}

// book books mix at start through the engine; false when it refused (a domain error).
func (j *oracleJudge) book(w workshop, mix jobMix, start time.Time, vehicle uuid.UUID, waiter bool, key string) (domain.Appointment, bool) {
	j.t.Helper()
	appt, _, err := j.svc.Book(j.t.Context(), oracleTestAdmin, booking.BookRequest{
		DealershipID: w.dealer.ID, VehicleID: vehicle, ServiceTypeIDs: mix.ids, StartAt: start, IsWaiter: waiter, IdempotencyKey: key,
	})
	var derr *domain.Error
	switch {
	case errors.As(err, &derr):
		return appt, false
	case err != nil:
		j.t.Fatalf("book %s at %s: %v", mix.name, start, err)
	}
	return appt, true
}

// repairDay sets day at w (no staging cap) up by hand so that a brake job is offered only through
// the one-move repair — at 09:00 of a general bay (every general bay holds something over
// 09:00–10:30, but the 08:30 job fits General 3, free until 10:00), at 14:00 of a technician
// (every brake technician holds something over 14:00–15:30, but Brakes L3's oil change fits Lube
// L1 and Master L4's general job fits General L3) — and not at 16:00, where the 09:00 layout is
// all checked in. It judges the day, books 09:00 and 14:00 through the engine (it must repair),
// and judges again.
func (j *oracleJudge) repairDay(w workshop, ids map[string]uuid.UUID, day booking.Date, car func() uuid.UUID) {
	t := j.t
	t.Helper()
	var customer uuid.UUID
	if err := j.pool.QueryRow(t.Context(), `SELECT customer_id FROM vehicles WHERE id = $1`, car()).Scan(&customer); err != nil {
		t.Fatal(err)
	}
	oil := j.mixes[len(j.mixes)-1].ids[0]
	at := func(hh, mm int) time.Time { return booking.Clock{Hour: hh, Minute: mm}.On(day, w.loc) }
	for n, h := range []struct {
		bay, tech string
		from, to  time.Time
		bayType   domain.BayType
		skill     domain.TechnicianSkill
		level     int16
		status    domain.AppointmentStatus
	}{
		// Every general bay busy over 09:00–10:30.
		{"General 1", "Lube L1", at(8, 30), at(9, 30), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusConfirmed},
		{"General 2", "Lube L2", at(9, 0), at(9, 30), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusConfirmed},
		{"General 3", "Lube L1", at(10, 0), at(10, 30), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusConfirmed},
		{"General 4", "Lube L2", at(9, 30), at(10, 0), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusConfirmed},
		// Every brake technician busy over 14:00–15:30.
		{"General 1", "Brakes L2", at(13, 30), at(14, 30), domain.BayGeneral, domain.SkillBrakes, 2, domain.StatusConfirmed},
		{"Express 1", "Brakes L3", at(14, 0), at(14, 30), domain.BayExpress, domain.SkillLubrication, 1, domain.StatusConfirmed},
		{"General 2", "Master L4", at(15, 0), at(15, 30), domain.BayGeneral, domain.SkillGeneral, 3, domain.StatusConfirmed},
		// The 09:00 layout again over 16:00–17:30, every job checked in: nothing may move, so
		// nothing is offered to a brake job there.
		{"General 1", "Lube L1", at(15, 30), at(16, 30), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusCheckedIn},
		{"General 2", "Lube L2", at(16, 0), at(16, 30), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusCheckedIn},
		{"General 3", "Lube L1", at(17, 0), at(17, 30), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusCheckedIn},
		{"General 4", "Lube L2", at(16, 30), at(17, 0), domain.BayGeneral, domain.SkillLubrication, 1, domain.StatusCheckedIn},
	} {
		if _, err := j.pool.Exec(t.Context(), `
			INSERT INTO appointments (id, dealership_id, customer_id, vehicle_id, service_type_id, service_bay_id, technician_id,
				start_at, end_at, status, idempotency_key, booked_duration_minutes, required_bay_type, required_skills, min_skill_level)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			uuid.New(), w.dealer.ID, customer, car(), oil, ids[h.bay], ids[h.tech], h.from, h.to, int16(h.status),
			fmt.Sprintf("oracle-test:repair:%d", n), int(h.to.Sub(h.from)/time.Minute), int16(h.bayType), []int16{int16(h.skill)}, h.level); err != nil {
			t.Fatalf("hold %d: %v", n, err)
		}
	}
	probe := car()
	before := j.repairOnly
	j.judge(w, day, probe, "set up for repairs")
	if j.repairOnly == before {
		t.Fatal("the repair day offered nothing only through a repair: the setup no longer exercises it")
	}
	brake := j.mixes[2]
	for _, start := range []time.Time{at(9, 0), at(14, 0)} {
		if _, ok := j.book(w, brake, start, car(), false, "oracle-test:repair:"+start.Format("1504")); !ok {
			t.Fatalf("%s at %s was offered through a repair but the engine refused it", brake.name, start.Format("15:04"))
		}
		j.judge(w, day, probe, "after the repair at "+start.Format("15:04"))
	}
}

// oracleTestMixes are the truth/fill days' job mixes from the seeded catalog.
func oracleTestMixes(t *testing.T, pool *pgxpool.Pool) []jobMix {
	t.Helper()
	c := oracleTestCatalog(t, pool)
	var mixes []jobMix
	for _, codes := range [][]string{{"MAJOR"}, {"OIL", "BRAKE"}, {"BRAKE"}, {"OIL"}} {
		mix, err := newJobMix(c, fmt.Sprint(codes), codes...)
		if err != nil {
			t.Fatal(err)
		}
		mixes = append(mixes, mix)
	}
	return mixes
}

// oracleTestCatalog is the seeded service catalog.
func oracleTestCatalog(t *testing.T, pool *pgxpool.Pool) catalog {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT id, code, duration_minutes, required_skill, required_bay_type, min_skill_level FROM service_types`)
	if err != nil {
		t.Fatal(err)
	}
	services, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.ServiceType, error) {
		var s domain.ServiceType
		var skill, bay int16
		err := row.Scan(&s.ID, &s.Code, &s.DurationMinutes, &skill, &bay, &s.MinSkillLevel)
		s.RequiredSkill, s.RequiredBayType = domain.TechnicianSkill(skill), domain.BayType(bay)
		return s, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return catalog{services: services}
}

// oracleTestVehicles inserts n cars of one customer.
func oracleTestVehicles(t *testing.T, pool *pgxpool.Pool, n int) []uuid.UUID {
	t.Helper()
	ctx := t.Context()
	customer := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO customers (id, name, phone, email) VALUES ($1, 'Oracle Customer', '0', 'oracle@example.com')`, customer); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `
		INSERT INTO vehicles (id, customer_id, vin, make, model, year)
		SELECT gen_random_uuid(), $1, 'ZXT' || lpad(g::text, 14, '0'), 'Oracle', 'Test', 2024 FROM generate_series(1, $2::int) g
		RETURNING id`, customer, n)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

// insertTestWorkshop writes def as a new dealership (London, express limit workshopExpressMax,
// the given staging cap) with its shift patterns, and its dated absences on each of days. It
// returns the shop and its bays' and technicians' ids by name.
func insertTestWorkshop(t *testing.T, pool *pgxpool.Pool, def workshopDef, name string, staging, window int16, days []booking.Date) (workshop, map[string]uuid.UUID) {
	t.Helper()
	return insertTestWorkshopIn(t, pool, def, name, "Europe/London", staging, window, days)
}

// insertTestWorkshopIn is insertTestWorkshop in timezone tz.
func insertTestWorkshopIn(t *testing.T, pool *pgxpool.Pool, def workshopDef, name, tz string, staging, window int16, days []booking.Date) (workshop, map[string]uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	w := workshop{loc: loc, def: def, dealer: domain.Dealership{
		ID: uuid.New(), Name: name, Timezone: tz, OpenTime: def.open, CloseTime: def.close,
		LunchStart: def.lunchStart, LunchEnd: def.lunchEnd, StagingCapacity: staging, StagingWindowMinutes: window,
		Policy: domain.Policy{ExpressMaxMinutes: workshopExpressMax},
	}}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("workshop %s: %v", name, err)
		}
	}
	exec(`INSERT INTO dealerships (id, name, timezone, open_time, close_time, lunch_start, lunch_end,
			staging_capacity, staging_window_minutes, express_max_minutes)
		VALUES ($1, $2, $10, $3::time, $4::time, NULLIF($5, '')::time, NULLIF($6, '')::time, $7, $8, $9)`,
		w.dealer.ID, name, def.open, def.close, def.lunchStart, def.lunchEnd, staging, window, workshopExpressMax, tz)
	ids := map[string]uuid.UUID{}
	for _, b := range def.bays {
		ids[b.name] = uuid.New()
		exec(`INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES ($1, $2, $3, $4)`, ids[b.name], w.dealer.ID, b.name, int16(b.bayType))
	}
	techs := ids
	for _, td := range def.techs {
		id := uuid.New()
		techs[td.name] = id
		exec(`INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES ($1, $2, $3, $4)`, id, w.dealer.ID, td.name, td.level)
		for _, sk := range td.skills {
			exec(`INSERT INTO technician_skills (technician_id, skill) VALUES ($1, $2)`, id, int16(sk))
		}
	}
	for tech, shifts := range def.shifts {
		for _, sh := range shifts {
			exec(`INSERT INTO technician_shifts (technician_id, weekday, start_time, end_time, break_start, break_end)
				VALUES ($1, $2, $3::time, $4::time, NULLIF($5, '')::time, NULLIF($6, '')::time)`,
				techs[tech], sh.Weekday, sh.StartTime, sh.EndTime, sh.BreakStart, sh.BreakEnd)
		}
	}
	for _, day := range days {
		for _, a := range def.absences {
			from, err1 := parseClockMinutes(a.from)
			to, err2 := parseClockMinutes(a.to)
			if err1 != nil || err2 != nil {
				t.Fatalf("absence %s–%s", a.from, a.to)
			}
			exec(`INSERT INTO technician_absences (id, technician_id, starts_at, ends_at, reason) VALUES ($1, $2, $3, $4, 'oracle test')`,
				uuid.New(), techs[a.tech], from.on(day, loc), to.on(day, loc))
		}
	}
	return w, ids
}
