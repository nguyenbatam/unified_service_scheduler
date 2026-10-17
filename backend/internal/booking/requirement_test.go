package booking

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func svc(minutes int, skill domain.TechnicianSkill, bay domain.BayType, level int16) domain.ServiceType {
	return domain.ServiceType{ID: uuid.New(), Code: "X", Name: "X", DurationMinutes: minutes, RequiredSkill: skill, RequiredBayType: bay, MinSkillLevel: domain.SkillLevel(level)}
}

func TestCombine(t *testing.T) {
	oil := svc(30, domain.SkillLubrication, domain.BayExpress, 1)
	brake := svc(90, domain.SkillBrakes, domain.BayGeneral, 2)
	diag := svc(60, domain.SkillGeneral, domain.BayDiagnostic, 3)
	align := svc(60, domain.SkillGeneral, domain.BayAlignment, 2)

	tests := []struct {
		name      string
		lines     []domain.ServiceType
		wantBay   domain.BayType
		wantMins  int
		wantLevel domain.SkillLevel
		wantSkill []domain.TechnicianSkill
		wantErr   bool
	}{
		{"single oil", []domain.ServiceType{oil}, domain.BayExpress, 30, 1, []domain.TechnicianSkill{domain.SkillLubrication}, false},
		{"oil + brake needs general", []domain.ServiceType{oil, brake}, domain.BayGeneral, 120, 2,
			[]domain.TechnicianSkill{domain.SkillLubrication, domain.SkillBrakes}, false},
		{"brake + diagnostic needs diagnostic", []domain.ServiceType{brake, diag}, domain.BayDiagnostic, 150, 3,
			[]domain.TechnicianSkill{domain.SkillBrakes, domain.SkillGeneral}, false},
		{"diagnostic + alignment cannot share a bay", []domain.ServiceType{diag, align}, 0, 0, 0, nil, true},
		{"none", nil, 0, 0, 0, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Combine(tt.lines)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.BayType != tt.wantBay || r.DurationMinutes != tt.wantMins || r.MinSkillLevel != tt.wantLevel {
				t.Fatalf("got bay %d, %d min, level %d", r.BayType, r.DurationMinutes, r.MinSkillLevel)
			}
			if len(r.Skills) != len(tt.wantSkill) {
				t.Fatalf("skills = %v, want %v", r.Skills, tt.wantSkill)
			}
			for i := range r.Skills {
				if r.Skills[i] != tt.wantSkill[i] {
					t.Fatalf("skills = %v, want %v", r.Skills, tt.wantSkill)
				}
			}
			snap := r.Snapshot()
			if len(snap.Lines) != len(tt.lines) || snap.Lines[0].LineNo != 1 || snap.ServiceTypeID != tt.lines[0].ID {
				t.Fatalf("snapshot lines = %+v", snap.Lines)
			}
		})
	}
}

func TestSoftUpgradeOnlyForShortWaiterJobs(t *testing.T) {
	p := domain.Policy{ExpressMaxMinutes: 60}
	short := Requirement{BayType: domain.BayGeneral, DurationMinutes: 60}
	long := Requirement{BayType: domain.BayGeneral, DurationMinutes: 90}
	express := Requirement{BayType: domain.BayExpress, DurationMinutes: 30}
	switch {
	case !short.SoftUpgrade(true, p):
		t.Fatal("a 60 min waiter job may use express")
	case long.SoftUpgrade(true, p):
		t.Fatal("a 90 min job must not block the express lane")
	case short.SoftUpgrade(false, p):
		t.Fatal("non-waiters never soft-upgrade")
	case express.SoftUpgrade(true, p):
		t.Fatal("only general-bay work is upgraded")
	case short.SoftUpgrade(true, domain.Policy{ExpressMaxMinutes: 0}):
		t.Fatal("0 disables the upgrade")
	}
}

func TestServiceIDs(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if ids, err := serviceIDs(a, nil); err != nil || len(ids) != 1 || ids[0] != a {
		t.Fatalf("single: %v %v", ids, err)
	}
	if ids, err := serviceIDs(uuid.Nil, []uuid.UUID{a, b}); err != nil || len(ids) != 2 {
		t.Fatalf("many: %v %v", ids, err)
	}
	for name, tc := range map[string]struct {
		single uuid.UUID
		many   []uuid.UUID
	}{
		"both":      {a, []uuid.UUID{b}},
		"none":      {uuid.Nil, nil},
		"duplicate": {uuid.Nil, []uuid.UUID{a, a}},
		"too many":  {uuid.Nil, []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}},
		"nil id":    {uuid.Nil, []uuid.UUID{uuid.Nil}},
	} {
		if _, err := serviceIDs(tc.single, tc.many); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

// BayFits: the exact type; the express spill (any express job into a general bay); a short
// waiter's general job into the quick lane; nothing into a specialty bay it does not need.
func TestBayFits(t *testing.T) {
	cases := []struct {
		bay, required domain.BayType
		upgrade, want bool
	}{
		{domain.BayExpress, domain.BayExpress, false, true},
		{domain.BayGeneral, domain.BayExpress, false, true},
		{domain.BayDiagnostic, domain.BayExpress, false, false},
		{domain.BayExpress, domain.BayGeneral, false, false},
		{domain.BayExpress, domain.BayGeneral, true, true},
		{domain.BayGeneral, domain.BayGeneral, false, true},
		{domain.BayDiagnostic, domain.BayGeneral, true, false},
		{domain.BayGeneral, domain.BayDiagnostic, false, false},
		{domain.BayAlignment, domain.BayDiagnostic, false, false},
	}
	for _, c := range cases {
		if got := BayFits(c.bay, c.required, c.upgrade); got != c.want {
			t.Errorf("BayFits(%v, %v, upgrade=%v) = %v, want %v", c.bay, c.required, c.upgrade, got, c.want)
		}
	}
}
