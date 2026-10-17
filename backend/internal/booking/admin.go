package booking

import (
	"context"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Admin configuration goes through the service so the role check does not depend on how
// routes happen to be grouped.

func adminOnly(actor domain.Actor) error {
	if actor.Role != domain.RoleAdmin {
		return domain.NewError(domain.CodeForbidden, "admin role required")
	}
	return nil
}

// adminFor allows a group admin anywhere and a dealership admin only at their own dealership.
func adminFor(actor domain.Actor, dealershipID uuid.UUID) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	if !actor.CanAccessDealership(dealershipID) {
		return otherDealership()
	}
	return nil
}

// groupAdminOnly guards the shared catalog (service types apply to every dealership).
func groupAdminOnly(actor domain.Actor) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	if actor.DealershipID != uuid.Nil {
		return domain.NewError(domain.CodeForbidden, "the service catalog is shared by every dealership; a group admin must change it")
	}
	return nil
}

// adminForTechnician resolves the technician's dealership (it never changes) and checks scope.
func (s *Service) adminForTechnician(ctx context.Context, actor domain.Actor, id uuid.UUID) error {
	if err := adminOnly(actor); err != nil {
		return err
	}
	tech, err := s.store.GetTechnicianAdmin(ctx, id)
	if err != nil {
		return err
	}
	if !actor.CanAccessDealership(tech.DealershipID) {
		return domain.NewError(domain.CodeNotFound, "technician not found")
	}
	return nil
}

// ListTechniciansAdmin returns one page of technicians in (name, id) order: every dealership's for a
// group admin (or the one asked for), only their own for a dealership admin.
func (s *Service) ListTechniciansAdmin(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID, limit int, after *store.NameCursor) ([]domain.TechnicianAdmin, *store.NameCursor, error) {
	if err := adminOnly(actor); err != nil {
		return nil, nil, err
	}
	if actor.DealershipID != uuid.Nil {
		if dealershipID != uuid.Nil && dealershipID != actor.DealershipID {
			return nil, nil, otherDealership()
		}
		dealershipID = actor.DealershipID
	}
	return s.store.ListTechniciansAdmin(ctx, dealershipID, limit, after)
}

func (s *Service) UpdateDealership(ctx context.Context, actor domain.Actor, id uuid.UUID, p store.DealershipPatch) (domain.Dealership, error) {
	if err := adminFor(actor, id); err != nil {
		return domain.Dealership{}, err
	}
	item, err := s.store.UpdateDealership(ctx, s.now(), id, p)
	if err != nil {
		return domain.Dealership{}, err
	}
	if loc, err := loadLocation(item.Timezone); err == nil {
		item.Today = Today(loc, s.now()).String()
	}
	return item, nil
}

func (s *Service) UpdateServiceType(ctx context.Context, actor domain.Actor, id uuid.UUID, p store.ServiceTypePatch) (domain.ServiceType, error) {
	if err := groupAdminOnly(actor); err != nil {
		return domain.ServiceType{}, err
	}
	if p.DurationMinutes != nil {
		if err := s.checkDurationFits(ctx, *p.DurationMinutes); err != nil {
			return domain.ServiceType{}, err
		}
	}
	return s.store.UpdateServiceType(ctx, id, p)
}

func (s *Service) UpdateTechnician(ctx context.Context, actor domain.Actor, id uuid.UUID, p store.TechnicianPatch) (domain.TechnicianAdmin, error) {
	if err := s.adminForTechnician(ctx, actor, id); err != nil {
		return domain.TechnicianAdmin{}, err
	}
	if p.Skills != nil {
		skills, err := normalizeSkills(p.Skills)
		if err != nil {
			return domain.TechnicianAdmin{}, err
		}
		p.Skills = skills
	}
	return s.store.UpdateTechnician(ctx, s.now(), id, p)
}
