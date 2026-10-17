package booking

import (
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// CreateDealership opens a new dealership (rooftop): group admins only, since a dealership admin
// is scoped to the one they already run.
func (s *Service) CreateDealership(ctx context.Context, actor domain.Actor, d store.NewDealership) (domain.Dealership, error) {
	if err := groupAdminOnly(actor); err != nil {
		return domain.Dealership{}, err
	}
	// "" and "Local" load in Go (UTC, the server's zone) but name no zone Postgres can use: every
	// write computes the dealership-day lock and calendar with AT TIME ZONE in SQL. The store also
	// checks the name against Postgres's own zone list.
	loc, err := loadLocation(d.Timezone)
	if err != nil || d.Timezone == "" || d.Timezone == "Local" {
		return domain.Dealership{}, invalidTimezone()
	}
	item, err := s.store.CreateDealership(ctx, d)
	if err != nil {
		return domain.Dealership{}, err
	}
	item.Today = Today(loc, s.now()).String()
	return item, nil
}

func invalidTimezone() error {
	return domain.NewError(domain.CodeInvalid, "timezone must be an IANA zone name such as Europe/London")
}

// CreateBay adds a bay: a group admin anywhere, a dealership admin at their own dealership.
func (s *Service) CreateBay(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID, name string, bayType domain.BayType) (domain.Bay, error) {
	if err := adminFor(actor, dealershipID); err != nil {
		return domain.Bay{}, err
	}
	if !bayType.Valid() {
		return domain.Bay{}, domain.NewError(domain.CodeInvalid, "bayType must be 1–4")
	}
	return s.store.CreateBay(ctx, dealershipID, name, bayType)
}

// CreateTechnician adds an active technician (no shift pattern: works the opening hours) with
// distinct skills: a group admin anywhere, a dealership admin at their own dealership.
func (s *Service) CreateTechnician(ctx context.Context, actor domain.Actor, t store.NewTechnician) (domain.TechnicianAdmin, error) {
	if err := adminFor(actor, t.DealershipID); err != nil {
		return domain.TechnicianAdmin{}, err
	}
	if !t.SkillLevel.Valid() {
		return domain.TechnicianAdmin{}, domain.NewError(domain.CodeInvalid, "skillLevel must be 1–4")
	}
	skills, err := normalizeSkills(t.Skills)
	if err != nil {
		return domain.TechnicianAdmin{}, err
	}
	t.Skills = skills
	return s.store.CreateTechnician(ctx, t)
}

// normalizeSkills validates a technician's skill list (at least one, every code known) and returns
// it sorted without duplicates.
func normalizeSkills(skills []domain.TechnicianSkill) ([]domain.TechnicianSkill, error) {
	if len(skills) == 0 {
		return nil, domain.NewError(domain.CodeInvalid, "skills must name at least one skill")
	}
	for _, sk := range skills {
		if !sk.Valid() {
			return nil, domain.NewError(domain.CodeInvalid, "skills must be 1–3")
		}
	}
	out := slices.Clone(skills)
	slices.Sort(out)
	return slices.Compact(out), nil
}
