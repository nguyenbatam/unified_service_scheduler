package booking

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Technician rosters (admin): weekly shifts with an optional break, and dated absences. Changes
// that would leave an upcoming booking with a technician who is not on duty return 409.

// maxAbsence bounds one absence row (long leave is entered as several).
const maxAbsence = 90 * 24 * time.Hour

func (s *Service) Roster(ctx context.Context, actor domain.Actor, technicianID uuid.UUID) (domain.TechnicianRoster, error) {
	if err := s.adminForTechnician(ctx, actor, technicianID); err != nil {
		return domain.TechnicianRoster{}, err
	}
	return s.store.GetRoster(ctx, s.now(), technicianID)
}

func (s *Service) ReplaceShifts(ctx context.Context, actor domain.Actor, technicianID uuid.UUID, shifts []domain.TechnicianShift, ifMatch int) (domain.TechnicianRoster, error) {
	if err := s.adminForTechnician(ctx, actor, technicianID); err != nil {
		return domain.TechnicianRoster{}, err
	}
	if err := validateShifts(shifts); err != nil {
		return domain.TechnicianRoster{}, err
	}
	return s.store.ReplaceShifts(ctx, s.now(), technicianID, shifts, ifMatch)
}

func (s *Service) AddAbsence(ctx context.Context, actor domain.Actor, technicianID uuid.UUID, a domain.TechnicianAbsence, ifMatch int) (domain.TechnicianRoster, error) {
	if err := s.adminForTechnician(ctx, actor, technicianID); err != nil {
		return domain.TechnicianRoster{}, err
	}
	switch {
	case a.StartsAt.IsZero() || a.EndsAt.IsZero() || !a.EndsAt.After(a.StartsAt):
		return domain.TechnicianRoster{}, domain.NewError(domain.CodeInvalid, "startsAt and endsAt are required and endsAt must be after startsAt")
	case a.EndsAt.Sub(a.StartsAt) > maxAbsence:
		return domain.TechnicianRoster{}, domain.NewError(domain.CodeInvalid, "an absence is at most 90 days; add longer leave in parts")
	case !a.EndsAt.After(s.now()):
		return domain.TechnicianRoster{}, domain.NewError(domain.CodeInvalid, "the absence has already ended")
	case len(a.Reason) > 120:
		return domain.TechnicianRoster{}, domain.NewError(domain.CodeInvalid, "reason is at most 120 characters")
	}
	a.ID = uuid.New()
	return s.store.AddAbsence(ctx, s.now(), technicianID, a, ifMatch)
}

func (s *Service) DeleteAbsence(ctx context.Context, actor domain.Actor, technicianID, absenceID uuid.UUID, ifMatch int) (domain.TechnicianRoster, error) {
	if err := s.adminForTechnician(ctx, actor, technicianID); err != nil {
		return domain.TechnicianRoster{}, err
	}
	return s.store.DeleteAbsence(ctx, s.now(), technicianID, absenceID, ifMatch)
}

func validateShifts(shifts []domain.TechnicianShift) error {
	seen := map[int16]bool{}
	for _, sh := range shifts {
		if sh.Weekday < 1 || sh.Weekday > 7 {
			return domain.NewError(domain.CodeInvalid, "weekday must be 1 (Monday) – 7 (Sunday)")
		}
		if seen[sh.Weekday] {
			return domain.NewError(domain.CodeInvalid, fmt.Sprintf("weekday %d is listed twice", sh.Weekday))
		}
		seen[sh.Weekday] = true
		if !validHHMM(sh.StartTime) || !validHHMM(sh.EndTime) || sh.EndTime <= sh.StartTime {
			return domain.NewError(domain.CodeInvalid, "startTime and endTime must be HH:MM with endTime after startTime")
		}
		if (sh.BreakStart == "") != (sh.BreakEnd == "") {
			return domain.NewError(domain.CodeInvalid, "breakStart and breakEnd must be set together")
		}
		if sh.BreakStart != "" {
			if !validHHMM(sh.BreakStart) || !validHHMM(sh.BreakEnd) || sh.BreakEnd <= sh.BreakStart ||
				sh.BreakStart < sh.StartTime || sh.BreakEnd > sh.EndTime {
				return domain.NewError(domain.CodeInvalid, "the break must be HH:MM inside the shift, with breakEnd after breakStart")
			}
		}
	}
	return nil
}
