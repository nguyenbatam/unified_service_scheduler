package integration_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

var eveSkills = []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes, domain.SkillGeneral}

// Taking lubrication from Eve (the only Riverside lubrication technician) races an oil booking:
// either the booking lands on her and the change is refused, or the change commits and the booking
// finds nobody.
func TestTechnicianSkillRemovalRacingBookingNeverBothWin(t *testing.T) {
	t.Parallel()
	technicianRace(t, seed.ServiceOil,
		store.TechnicianPatch{Skills: []domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral}},
		store.TechnicianPatch{Skills: eveSkills},
		func(tech domain.TechnicianAdmin) bool { return slices.Contains(tech.Skills, domain.SkillLubrication) })
}

// Skills are edited as a full set: validated, deduplicated, versioned and broadcast, and refused
// while an upcoming booking needs a skill being removed.
func TestAdminEditsTechnicianSkills(t *testing.T) {
	t.Parallel()
	pool, _, svc, ctx := openTestDB(t)

	for _, bad := range [][]domain.TechnicianSkill{{}, {9}, {0, 1}} {
		_, err := svc.UpdateTechnician(ctx, staffAdmin, seed.TechFinn, store.TechnicianPatch{Skills: bad})
		expectCode(t, err, domain.CodeInvalid)
	}

	before := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE aggregate_id = $1`, seed.TechFinn)
	finn, err := svc.UpdateTechnician(ctx, staffAdmin, seed.TechFinn, store.TechnicianPatch{
		Skills: []domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral, domain.SkillBrakes},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(finn.Skills, []domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral}) || finn.Version != 2 {
		t.Fatalf("finn = %+v", finn)
	}
	if after := scalar[int](t, pool, `SELECT count(*)::int FROM outbox_events WHERE aggregate_id = $1 AND event_type = `+code(domain.EventConfigUpdated), seed.TechFinn); after != before+1 {
		t.Fatalf("CONFIG_UPDATED rows %d → %d", before, after)
	}

	// River MAJOR needs general L3: only Eve qualifies.
	item, _, err := svc.Book(ctx, staffAdmin, booking.BookRequest{
		DealershipID: seed.DealerRiver, VehicleID: seed.VehicleA, ServiceTypeID: seed.ServiceMajor,
		StartAt: londonOn(t, londonDay(t, 1), 8, 0), IdempotencyKey: "test:skills",
	})
	if err != nil || item.TechnicianID != seed.TechEve {
		t.Fatalf("book = %+v %v", item, err)
	}
	_, err = svc.UpdateTechnician(ctx, staffAdmin, seed.TechEve, store.TechnicianPatch{
		Skills: []domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes},
	})
	expectCode(t, err, domain.CodeConflict)
	// Removing a skill the booking does not need is fine.
	eve, err := svc.UpdateTechnician(ctx, staffAdmin, seed.TechEve, store.TechnicianPatch{
		Skills: []domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral},
	})
	if err != nil || slices.Contains(eve.Skills, domain.SkillLubrication) {
		t.Fatalf("eve = %+v %v", eve, err)
	}
	assertAllocationsValid(t, pool)
}

// The PATCH accepts skills next to level/active, with If-Match like the rest of the config.
func TestHTTPTechnicianSkillsPatch(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	h := newAPI(st, svc)
	admin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)
	path := fmt.Sprintf("/api/v1/admin/technicians/%s", seed.TechFinn)

	admin.expect(admin.do(http.MethodPatch, path, map[string]any{"skills": []int{}}, map[string]string{"If-Match": `"1"`}), http.StatusBadRequest, nil)
	var out domain.TechnicianAdmin
	admin.expect(admin.do(http.MethodPatch, path, map[string]any{"skills": []int{2, 3}}, map[string]string{"If-Match": `"1"`}), http.StatusOK, &out)
	if !slices.Equal(out.Skills, []domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral}) {
		t.Fatalf("skills = %v", out.Skills)
	}
	admin.expect(admin.do(http.MethodPatch, path, map[string]any{"skills": []int{3}}, map[string]string{"If-Match": `"1"`}), http.StatusPreconditionFailed, nil)
}
