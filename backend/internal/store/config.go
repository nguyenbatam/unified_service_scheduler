package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

const technicianAdminSelect = `
	SELECT t.id, t.dealership_id, t.name, t.skill_level, t.active, t.version,
	       coalesce(array_agg(sk.skill ORDER BY sk.skill) FILTER (WHERE sk.skill IS NOT NULL), '{}')
	FROM technicians t
	LEFT JOIN technician_skills sk ON sk.technician_id = t.id`

func (s *Store) GetTechnicianAdmin(ctx context.Context, id uuid.UUID) (domain.TechnicianAdmin, error) {
	return getTechnicianAdmin(ctx, s.pool, id)
}

func getTechnicianAdmin(ctx context.Context, q queryer, id uuid.UUID) (domain.TechnicianAdmin, error) {
	row := q.QueryRow(ctx, technicianAdminSelect+`
		WHERE t.id = $1
		GROUP BY t.id`, id)
	item, err := scanTechnicianAdmin(row)
	if err != nil {
		return domain.TechnicianAdmin{}, missing(err, "technician not found")
	}
	return item, nil
}

func scanTechnicianAdmin(row pgx.Row) (domain.TechnicianAdmin, error) {
	var item domain.TechnicianAdmin
	var skills []int16
	if err := row.Scan(&item.ID, &item.DealershipID, &item.Name, &item.SkillLevel, &item.Active, &item.Version, &skills); err != nil {
		return domain.TechnicianAdmin{}, err
	}
	item.Skills = make([]domain.TechnicianSkill, len(skills))
	for i, sk := range skills {
		item.Skills[i] = domain.TechnicianSkill(sk)
	}
	return item, nil
}

// DealershipPatch: nil fields keep the current value. Lunch is set as a pair; "" clears it.
type DealershipPatch struct {
	OpenTime                    *string
	CloseTime                   *string
	LunchStart                  *string
	LunchEnd                    *string
	StagingCapacity             *int16
	StagingWindowMinutes        *int16
	CustomerLeadMinutes         *int16
	CustomerChangeCutoffMinutes *int16
	BookingHorizonDays          *int16
	NoShowGraceMinutes          *int16
	ExpressMaxMinutes           *int16
	IfMatch                     int
}

// UpdateDealership merges the patch under a row lock (no lost update between concurrent admins),
// bumps version, and emits CONFIG_UPDATED in the same TX. now (the service clock) decides which
// bookings are upcoming for the stranded check.
func (s *Store) UpdateDealership(ctx context.Context, now time.Time, id uuid.UUID, p DealershipPatch) (domain.Dealership, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.Dealership{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := scanDealership(tx.tx.QueryRow(ctx, dealershipSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return domain.Dealership{}, missing(err, "dealership not found")
	}
	if err := domain.CheckIfMatch(p.IfMatch, cur.Version, "record"); err != nil {
		return domain.Dealership{}, err
	}
	next := cur
	setIf(&next.OpenTime, p.OpenTime)
	setIf(&next.CloseTime, p.CloseTime)
	setIf(&next.LunchStart, p.LunchStart)
	setIf(&next.LunchEnd, p.LunchEnd)
	setIf(&next.StagingCapacity, p.StagingCapacity)
	setIf(&next.StagingWindowMinutes, p.StagingWindowMinutes)
	setIf(&next.Policy.CustomerLeadMinutes, p.CustomerLeadMinutes)
	setIf(&next.Policy.CustomerChangeCutoffMinutes, p.CustomerChangeCutoffMinutes)
	setIf(&next.Policy.BookingHorizonDays, p.BookingHorizonDays)
	setIf(&next.Policy.NoShowGraceMinutes, p.NoShowGraceMinutes)
	setIf(&next.Policy.ExpressMaxMinutes, p.ExpressMaxMinutes)
	updated, err := scanDealership(tx.tx.QueryRow(ctx, `
		UPDATE dealerships SET
			open_time = $2::time,
			close_time = $3::time,
			lunch_start = NULLIF($4, '')::time,
			lunch_end = NULLIF($5, '')::time,
			staging_capacity = $6,
			staging_window_minutes = $7,
			customer_lead_minutes = $8,
			customer_change_cutoff_minutes = $9,
			booking_horizon_days = $10,
			no_show_grace_minutes = $11,
			express_max_minutes = $12,
			version = version + 1
		WHERE id = $1
		RETURNING `+dealershipColumns,
		id, next.OpenTime, next.CloseTime, next.LunchStart, next.LunchEnd, next.StagingCapacity,
		next.StagingWindowMinutes, next.Policy.CustomerLeadMinutes, next.Policy.CustomerChangeCutoffMinutes,
		next.Policy.BookingHorizonDays, next.Policy.NoShowGraceMinutes, next.Policy.ExpressMaxMinutes))
	if err != nil {
		return domain.Dealership{}, translate(err)
	}
	// New default hours or lunch must not strand upcoming bookings on days without a weekday
	// override. Checked after the UPDATE so invalid input fails on the table CHECKs (400) first;
	// the TX rolls back on 409.
	if err := tx.refuseStranded(ctx, id, now); err != nil {
		return domain.Dealership{}, err
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateDealership, id, domain.EventConfigUpdated, updated); err != nil {
		return domain.Dealership{}, err
	}
	return updated, tx.Commit(ctx)
}

type ServiceTypePatch struct {
	DurationMinutes *int
	MinSkillLevel   *domain.SkillLevel
	RequiredSkill   *domain.TechnicianSkill
	RequiredBayType *domain.BayType
	IfMatch         int
}

// UpdateServiceType changes the rules for new bookings only. Existing appointments keep their
// booked window, bay, technician, and requirement snapshot (grandfathered), as dealers do when a
// labour time changes.
func (s *Store) UpdateServiceType(ctx context.Context, id uuid.UUID, p ServiceTypePatch) (domain.ServiceType, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.ServiceType{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := scanServiceType(tx.tx.QueryRow(ctx, serviceTypeSelect+` WHERE id = $1 FOR UPDATE`, id))
	if err != nil {
		return domain.ServiceType{}, missing(err, "service type not found")
	}
	if err := domain.CheckIfMatch(p.IfMatch, cur.Version, "record"); err != nil {
		return domain.ServiceType{}, err
	}
	next := cur
	setIf(&next.DurationMinutes, p.DurationMinutes)
	setIf(&next.MinSkillLevel, p.MinSkillLevel)
	setIf(&next.RequiredSkill, p.RequiredSkill)
	setIf(&next.RequiredBayType, p.RequiredBayType)
	updated, err := scanServiceType(tx.tx.QueryRow(ctx, `
		UPDATE service_types SET
			duration_minutes = $2,
			min_skill_level = $3,
			required_skill = $4,
			required_bay_type = $5,
			version = version + 1
		WHERE id = $1
		RETURNING id, code, name, duration_minutes, required_skill, required_bay_type, min_skill_level, version`,
		id, next.DurationMinutes, next.MinSkillLevel, int16(next.RequiredSkill), int16(next.RequiredBayType)))
	if err != nil {
		return domain.ServiceType{}, translate(err)
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateServiceType, id, domain.EventConfigUpdated, updated); err != nil {
		return domain.ServiceType{}, err
	}
	return updated, tx.Commit(ctx)
}

type TechnicianPatch struct {
	SkillLevel *domain.SkillLevel
	Active     *bool
	Skills     []domain.TechnicianSkill // nil = unchanged; else the full set (distinct, validated by booking)
	IfMatch    int
}

// UpdateTechnician refuses to deactivate a technician who still holds appointments not ended at now
// (the service clock), so no booking is left assigned to someone who will not be there, to
// downgrade them below such an appointment's booked minimum level, and to take away a skill such an
// appointment's booked lines need. The technician row is locked FOR UPDATE first, so a booking's
// LockBookableTechnician (FOR SHARE, then the skills in a fresh statement) either commits before
// these checks count it or reads the new skills.
func (s *Store) UpdateTechnician(ctx context.Context, now time.Time, id uuid.UUID, p TechnicianPatch) (domain.TechnicianAdmin, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.TechnicianAdmin{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var version int
	var active bool
	var level domain.SkillLevel
	err = tx.tx.QueryRow(ctx, `SELECT version, active, skill_level FROM technicians WHERE id = $1 FOR UPDATE`, id).
		Scan(&version, &active, &level)
	if err != nil {
		return domain.TechnicianAdmin{}, missing(err, "technician not found")
	}
	if err := domain.CheckIfMatch(p.IfMatch, version, "record"); err != nil {
		return domain.TechnicianAdmin{}, err
	}
	setIf(&level, p.SkillLevel)
	if p.Active != nil && !*p.Active && active {
		var upcoming int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*)::int FROM appointments
			WHERE technician_id = $1 AND `+holdingStatusesSQL+` AND end_at > $2::timestamptz`, id, now).Scan(&upcoming); err != nil {
			return domain.TechnicianAdmin{}, err
		}
		if upcoming > 0 {
			return domain.TechnicianAdmin{}, domain.NewError(domain.CodeConflict,
				fmt.Sprintf("technician has %d upcoming appointment(s); reassign or cancel them first", upcoming))
		}
	}
	if p.SkillLevel != nil {
		// A downgrade must not leave upcoming jobs with a technician below the service's minimum.
		var underQualified int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*)::int FROM appointments a
			WHERE a.technician_id = $1 AND a.`+holdingStatusesSQL+` AND a.end_at > $3::timestamptz
			  AND a.min_skill_level > $2`, id, level, now).Scan(&underQualified); err != nil {
			return domain.TechnicianAdmin{}, err
		}
		if underQualified > 0 {
			return domain.TechnicianAdmin{}, domain.NewError(domain.CodeConflict,
				fmt.Sprintf("%d upcoming appointment(s) need a higher skill level; reassign them first", underQualified))
		}
	}
	if p.Skills != nil {
		// Removing a skill must not leave upcoming jobs with a technician missing a booked skill.
		var missingSkill int
		if err := tx.tx.QueryRow(ctx, `
			SELECT count(*)::int FROM appointments a
			WHERE a.technician_id = $1 AND a.`+holdingStatusesSQL+` AND a.end_at > $3::timestamptz
			  AND NOT (a.required_skills <@ $2::smallint[])`, id, skillCodes(p.Skills), now).Scan(&missingSkill); err != nil {
			return domain.TechnicianAdmin{}, err
		}
		if missingSkill > 0 {
			return domain.TechnicianAdmin{}, domain.NewError(domain.CodeConflict,
				fmt.Sprintf("%d upcoming appointment(s) need a skill being removed; reassign them first", missingSkill))
		}
		if _, err := tx.tx.Exec(ctx, `
			WITH gone AS (
				DELETE FROM technician_skills WHERE technician_id = $1 AND skill <> ALL ($2::smallint[])
			)
			INSERT INTO technician_skills (technician_id, skill)
			SELECT $1, s FROM unnest($2::smallint[]) AS s
			ON CONFLICT DO NOTHING`, id, skillCodes(p.Skills)); err != nil {
			return domain.TechnicianAdmin{}, translate(err)
		}
	}
	setIf(&active, p.Active)
	if _, err := tx.tx.Exec(ctx, `
		UPDATE technicians SET skill_level = $2, active = $3, version = version + 1 WHERE id = $1`,
		id, level, active); err != nil {
		return domain.TechnicianAdmin{}, translate(err)
	}
	updated, err := getTechnicianAdmin(ctx, tx.tx, id)
	if err != nil {
		return domain.TechnicianAdmin{}, err
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateTechnician, id, domain.EventConfigUpdated, updated); err != nil {
		return domain.TechnicianAdmin{}, err
	}
	return updated, tx.Commit(ctx)
}

func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}
