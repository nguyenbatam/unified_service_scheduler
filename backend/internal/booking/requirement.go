package booking

import (
	"slices"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// MaxJobLines caps the operations on one appointment (a repair order rarely books more up front).
const MaxJobLines = 5

// Requirement is what a set of job lines needs from the workshop, done by one technician in one
// bay over one window: the summed labour time, the most capable bay any line needs, every skill,
// and the highest minimum skill level.
type Requirement struct {
	Services        []domain.ServiceType
	DurationMinutes int
	BayType         domain.BayType
	Skills          []domain.TechnicianSkill
	MinSkillLevel   domain.SkillLevel
}

// serviceIDs validates and normalises the requested job lines (1..MaxJobLines, no duplicates).
func serviceIDs(single uuid.UUID, many []uuid.UUID) ([]uuid.UUID, error) {
	switch {
	case single != uuid.Nil && len(many) > 0:
		return nil, domain.NewError(domain.CodeInvalid, "send serviceTypeId or serviceTypeIds, not both")
	case single != uuid.Nil:
		return []uuid.UUID{single}, nil
	case len(many) == 0:
		return nil, domain.NewError(domain.CodeInvalid, "at least one service type is required")
	case len(many) > MaxJobLines:
		return nil, domain.NewError(domain.CodeInvalid, "an appointment can carry at most 5 services")
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range many {
		if id == uuid.Nil {
			return nil, domain.NewError(domain.CodeInvalid, "service type ids must be valid")
		}
		if seen[id] {
			return nil, domain.NewError(domain.CodeInvalid, "a service type is listed twice")
		}
		seen[id] = true
	}
	return slices.Clone(many), nil
}

// Combine derives the requirement of the job lines. Bays rank express < general < a specialty
// (diagnostic or alignment) — work that fits a smaller bay also fits a more capable one — but a
// job needing both specialty bays cannot be done in one bay and must be booked separately.
func Combine(services []domain.ServiceType) (Requirement, error) {
	if len(services) == 0 {
		return Requirement{}, domain.NewError(domain.CodeInvalid, "at least one service type is required")
	}
	r := Requirement{Services: services, BayType: domain.BayExpress, MinSkillLevel: domain.MinSkillLevel}
	specialty := domain.BayType(0)
	for _, st := range services {
		r.DurationMinutes += st.DurationMinutes
		r.MinSkillLevel = max(r.MinSkillLevel, st.MinSkillLevel)
		if !slices.Contains(r.Skills, st.RequiredSkill) {
			r.Skills = append(r.Skills, st.RequiredSkill)
		}
		switch st.RequiredBayType {
		case domain.BayDiagnostic, domain.BayAlignment:
			if specialty != 0 && specialty != st.RequiredBayType {
				return Requirement{}, domain.NewError(domain.CodeInvalid,
					"diagnostic and alignment work need different bays; book them as separate appointments")
			}
			specialty = st.RequiredBayType
		case domain.BayGeneral:
			r.BayType = domain.BayGeneral
		}
	}
	if specialty != 0 {
		r.BayType = specialty
	}
	slices.Sort(r.Skills)
	return r, nil
}

// SoftUpgrade reports whether a waiter's general-bay job may take a free express bay: only when it
// is short enough for the express lane (policy.ExpressMaxMinutes), so a 90-minute brake job does
// not block three quick-lane oil changes.
func (r Requirement) SoftUpgrade(waiter bool, p domain.Policy) bool {
	return waiter && r.BayType == domain.BayGeneral && p.ExpressMaxMinutes > 0 && r.DurationMinutes <= int(p.ExpressMaxMinutes)
}

// BayFits reports whether a bay of type bay can host work requiring required, upgrade being the
// job's waiter soft upgrade (Requirement.SoftUpgrade): the exact type; express for a soft-upgraded
// general job; or general for an express job (the express spill: a general bay holds anything the
// quick lane does). Allocation ranks the exact type first, so either fallback is used only when no
// bay of the job's own type is free. The SQL twin is store.bayFitsSQL; keep both in step.
func BayFits(bay, required domain.BayType, upgrade bool) bool {
	return bay == required ||
		(upgrade && required == domain.BayGeneral && bay == domain.BayExpress) ||
		(required == domain.BayExpress && bay == domain.BayGeneral)
}

// qualifies reports whether a technician of level with skills can do work needing every skill in
// need at minLevel (the SQL: skill_level >= min AND hasSkillsSQL).
func qualifies(level domain.SkillLevel, skills, need []domain.TechnicianSkill, minLevel domain.SkillLevel) bool {
	if level < minLevel {
		return false
	}
	for _, sk := range need {
		if !slices.Contains(skills, sk) {
			return false
		}
	}
	return true
}

// Snapshot is the booked copy stored on the appointment (lines + requirement).
func (r Requirement) Snapshot() domain.Appointment {
	lines := make([]domain.AppointmentLine, len(r.Services))
	for i, st := range r.Services {
		lines[i] = domain.AppointmentLine{
			LineNo: int16(i + 1), ServiceTypeID: st.ID, Code: st.Code, Name: st.Name, DurationMinutes: st.DurationMinutes,
		}
	}
	return domain.Appointment{
		ServiceTypeID:   r.Services[0].ID,
		DurationMinutes: r.DurationMinutes,
		Lines:           lines,
		RequiredBayType: r.BayType,
		RequiredSkills:  slices.Clone(r.Skills),
		MinSkillLevel:   r.MinSkillLevel,
	}
}

// bookedRequirement rebuilds the requirement from the appointment's booked snapshot, so moving
// the appointment never re-prices it against a catalog edited since (lines, labour time, bay,
// skills, and level stay as booked).
func bookedRequirement(appt domain.Appointment) Requirement {
	services := make([]domain.ServiceType, len(appt.Lines))
	for i, l := range appt.Lines {
		services[i] = domain.ServiceType{ID: l.ServiceTypeID, Code: l.Code, Name: l.Name, DurationMinutes: l.DurationMinutes}
	}
	if len(services) == 0 { // a row without line records: one line from the primary operation
		services = []domain.ServiceType{{ID: appt.ServiceTypeID, Name: appt.ServiceName, DurationMinutes: appt.DurationMinutes}}
	}
	return Requirement{
		Services:        services,
		DurationMinutes: appt.DurationMinutes,
		BayType:         appt.RequiredBayType,
		Skills:          slices.Clone(appt.RequiredSkills),
		MinSkillLevel:   appt.MinSkillLevel,
	}
}

func lineServiceIDs(appt domain.Appointment) []uuid.UUID {
	ids := make([]uuid.UUID, len(appt.Lines))
	for i, l := range appt.Lines {
		ids[i] = l.ServiceTypeID
	}
	if len(ids) == 0 {
		ids = []uuid.UUID{appt.ServiceTypeID}
	}
	return ids
}
