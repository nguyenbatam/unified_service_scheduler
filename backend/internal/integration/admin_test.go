package integration_test

import (
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func TestAdminDealershipUpdateIsVersionedAndBroadcast(t *testing.T) {
	t.Parallel()
	pool, st, _, ctx := openTestDB(t)
	cur, err := st.GetDealership(ctx, seed.DealerNorth)
	if err != nil {
		t.Fatal(err)
	}

	_, err = st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{StagingCapacity: new(int16(3)), IfMatch: cur.Version + 1})
	expectCode(t, err, domain.CodePrecondition)

	updated, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{StagingCapacity: new(int16(3)), IfMatch: cur.Version})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != cur.Version+1 || updated.StagingCapacity != 3 || updated.LunchStart != cur.LunchStart {
		t.Fatalf("updated = %+v", updated)
	}
	events := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE aggregate_id = $1 AND event_type = `+code(domain.EventConfigUpdated), seed.DealerNorth)
	if events != 1 {
		t.Fatalf("config events = %d", events)
	}

	_, err = st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{LunchStart: new("07:00"), LunchEnd: new("08:00")})
	expectCode(t, err, domain.CodeInvalid) // lunch outside opening hours
	_, err = st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{OpenTime: new("18:00")})
	expectCode(t, err, domain.CodeInvalid) // open after close

	cleared, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{LunchStart: new(""), LunchEnd: new("")})
	if err != nil || cleared.LunchStart != "" {
		t.Fatalf("clear lunch: %+v %v", cleared, err)
	}
}

func TestAdminCannotDeactivateTechnicianWithUpcomingWork(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	// River MAJOR needs general L3: only Eve qualifies.
	item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceMajor,
		StartAt: londonOn(t, londonDay(t, 1), 8, 0), IdempotencyKey: "test:deactivate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.TechnicianID != seed.TechEve {
		t.Fatalf("tech = %s", item.TechnicianName)
	}
	_, err = st.UpdateTechnician(ctx, startOfLondonDay(), seed.TechEve, store.TechnicianPatch{Active: new(false)})
	expectCode(t, err, domain.CodeConflict)

	finn, err := st.UpdateTechnician(ctx, startOfLondonDay(), seed.TechFinn, store.TechnicianPatch{Active: new(false), SkillLevel: new(domain.LevelMaster)})
	if err != nil || finn.Active || finn.SkillLevel != 4 || finn.Version != 2 {
		t.Fatalf("finn = %+v %v", finn, err)
	}

	oilVersion := scalar[int](t, pool, `SELECT version FROM service_types WHERE id = $1`, seed.ServiceOil)
	_, err = st.UpdateServiceType(ctx, seed.ServiceOil, store.ServiceTypePatch{DurationMinutes: new(60), IfMatch: oilVersion + 3})
	expectCode(t, err, domain.CodePrecondition)
}

func storePatchStaging(n int16) store.DealershipPatch {
	return store.DealershipPatch{StagingCapacity: new(n)}
}

// Config changes must not strand upcoming bookings (they are moved first, deliberately).
func TestAdminChangesCannotStrandUpcomingBookings(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	// Tomorrow 15:00–16:30 at Northside (open until 17:00), brake service (min level 2) on Blair/Casey.
	item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerNorth, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceBrake,
		StartAt: londonOn(t, londonDay(t, 1), 15, 0), IdempotencyKey: "test:strand",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{CloseTime: new("16:00")})
	expectCode(t, err, domain.CodeConflict) // 15:00–16:30 would end after closing
	_, err = st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{LunchStart: new("15:30"), LunchEnd: new("16:00")})
	expectCode(t, err, domain.CodeConflict) // lunch would overlap the job
	if _, err := st.UpdateDealership(ctx, startOfLondonDay(), seed.DealerNorth, store.DealershipPatch{CloseTime: new("16:30")}); err != nil {
		t.Fatalf("closing exactly at the job end is fine: %v", err)
	}

	_, err = st.UpdateTechnician(ctx, startOfLondonDay(), item.TechnicianID, store.TechnicianPatch{SkillLevel: new(domain.LevelApprentice)})
	expectCode(t, err, domain.CodeConflict) // brake needs level 2
	if _, err := st.UpdateTechnician(ctx, startOfLondonDay(), item.TechnicianID, store.TechnicianPatch{SkillLevel: new(domain.LevelTechnician)}); err != nil {
		t.Fatalf("downgrade that still qualifies: %v", err)
	}
}
