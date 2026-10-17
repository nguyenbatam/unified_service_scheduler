package integration_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A checked-in Extend re-checks the roster and calendar under the technician and dealership share
// locks, so it races a roster or hours edit over the extended tail like a booking does: never
// both. Either the extension lands and the edit is refused with 409 (it would strand it), or the
// edit lands and the extension is refused with 422.
func TestExtendCheckedInRacingConfigNeverBothWin(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	for offset := 1; offset <= 4; offset++ {
		day := londonDay(t, offset)
		at0800 := serviceOn(t, st, day, 8, 0)
		checkedIn := func(vehicle uuid.UUID) domain.Appointment {
			t.Helper()
			item, _, err := at0800.Book(ctx, staffAdmin, oilAt(seed.DealerRiver, vehicle, londonOn(t, day, 14, 0)))
			if err != nil {
				t.Fatal(err)
			}
			if item, err = at0800.CheckIn(ctx, staffDispatcher, item.ID, 0); err != nil {
				t.Fatal(err)
			}
			return item
		}

		// Eve's absence over 14:30–15:30 vs extending her 14:00–14:30 job to 15:00.
		item := checkedIn(newVehicle(t, svc))
		var absenceErr, extendErr error
		race(func() {
			_, absenceErr = at0800.AddAbsence(ctx, staffAdmin, seed.TechEve, domain.TechnicianAbsence{
				StartsAt: londonOn(t, day, 14, 30), EndsAt: londonOn(t, day, 15, 30), Reason: "training",
			}, 0)
		}, func() {
			_, extendErr = at0800.Extend(ctx, staffDispatcher, item.ID, 30, 0)
		})
		oneWinner(t, day.String()+" absence", loses(absenceErr, domain.CodeConflict), loses(extendErr, domain.CodeUnavailable))

		// Riverside closing at 14:30 on that weekday vs extending a 14:00–14:30 job a week later
		// past it (the absence above is on the earlier date, so it does not get in the way).
		second := londonDay(t, offset+7) // same weekday next week, so the override applies to it
		at0800b := serviceOn(t, st, second, 8, 0)
		job := mustBook(t, at0800b, oilAt(seed.DealerRiver, newVehicle(t, svc), londonOn(t, second, 14, 0)))
		job, err := at0800b.CheckIn(ctx, staffDispatcher, job.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		var hoursErr error
		race(func() {
			_, hoursErr = at0800b.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, []domain.WeekdayHours{
				{Weekday: second.ISOWeekday(), OpenTime: "08:00", CloseTime: "14:30"},
			}, 0)
		}, func() {
			_, extendErr = at0800b.Extend(ctx, staffDispatcher, job.ID, 30, 0)
		})
		t.Logf("%s: absence won=%v, hours won=%v", day, absenceErr == nil, hoursErr == nil)
		oneWinner(t, second.String()+" hours", loses(hoursErr, domain.CodeConflict), loses(extendErr, domain.CodeUnavailable))
		if hoursErr == nil { // reset, so the next round's weekday starts from the defaults
			if _, err := at0800b.ReplaceWeeklyHours(ctx, staffAdmin, seed.DealerRiver, nil, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Recount: no hold of Eve's overlaps one of her absences.
	clash := scalar[int](t, pool, `
		SELECT count(*)::int FROM appointments a
		JOIN technician_absences x ON x.technician_id = a.technician_id
		WHERE a.technician_id = $1 AND a.status IN (`+holdingCodes+`)
		  AND tstzrange(a.start_at, a.end_at, '[)') && tstzrange(x.starts_at, x.ends_at, '[)')`, seed.TechEve)
	if clash != 0 {
		t.Fatalf("%d holds overlap the technician's absences", clash)
	}
	assertNoOverlaps(t, pool)
	assertAllocationsValid(t, pool)
	assertNoOrphanEvents(t, pool)
}
