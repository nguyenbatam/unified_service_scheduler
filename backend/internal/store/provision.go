package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Provisioning adds workshop resources: a dealership, a bay, a technician. Adding capacity never
// strands a booking, so none of these runs a stranded check; each still emits CONFIG_UPDATED in
// its TX so caches and live boards on every instance pick the change up.

// NewDealership is a dealership to create. Lunch is both clocks or both "".
type NewDealership struct {
	Name                 string
	Timezone             string
	OpenTime, CloseTime  string
	LunchStart, LunchEnd string
	StagingCapacity      int16
	StagingWindowMinutes int16
	Policy               domain.Policy
}

// CreateDealership inserts the dealership (version 1) and emits CONFIG_UPDATED. Out-of-range values
// fail on the table CHECKs (400 with a readable message, see checkMessages).
func (s *Store) CreateDealership(ctx context.Context, d NewDealership) (domain.Dealership, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.Dealership{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Postgres must know the zone too (bookings compute the local day with AT TIME ZONE).
	var known bool
	if err := tx.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)`, d.Timezone).Scan(&known); err != nil {
		return domain.Dealership{}, err
	}
	if !known {
		return domain.Dealership{}, domain.NewError(domain.CodeInvalid, "timezone must be an IANA zone name such as Europe/London")
	}
	created, err := scanDealership(tx.tx.QueryRow(ctx, `
		INSERT INTO dealerships (id, name, timezone, open_time, close_time, lunch_start, lunch_end,
			staging_capacity, staging_window_minutes, customer_lead_minutes, customer_change_cutoff_minutes,
			booking_horizon_days, no_show_grace_minutes, express_max_minutes)
		VALUES ($1, $2, $3, $4::time, $5::time, NULLIF($6, '')::time, NULLIF($7, '')::time,
			$8, $9, $10, $11, $12, $13, $14)
		RETURNING `+dealershipColumns,
		uuid.New(), d.Name, d.Timezone, d.OpenTime, d.CloseTime, d.LunchStart, d.LunchEnd,
		d.StagingCapacity, d.StagingWindowMinutes, d.Policy.CustomerLeadMinutes, d.Policy.CustomerChangeCutoffMinutes,
		d.Policy.BookingHorizonDays, d.Policy.NoShowGraceMinutes, d.Policy.ExpressMaxMinutes))
	if err != nil {
		return domain.Dealership{}, translate(err)
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateDealership, created.ID, domain.EventConfigUpdated, created); err != nil {
		return domain.Dealership{}, err
	}
	return created, tx.Commit(ctx)
}

// CreateBay adds an active bay to a dealership under its row lock (FOR UPDATE, like every other
// dealership config change), bumps the dealership's version, and emits CONFIG_UPDATED for the
// dealership: its capacity changed. A bay name already used at the dealership is a 409.
func (s *Store) CreateBay(ctx context.Context, dealershipID uuid.UUID, name string, bayType domain.BayType) (domain.Bay, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.Bay{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.tx.Exec(ctx, `SELECT 1 FROM dealerships WHERE id = $1 FOR UPDATE`, dealershipID); err != nil {
		return domain.Bay{}, err
	}
	bay, err := scanBay(tx.tx.QueryRow(ctx, `
		INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES ($1, $2, $3, $4)
		RETURNING id, name, bay_type`, uuid.New(), dealershipID, name, int16(bayType)))
	if err != nil {
		return domain.Bay{}, provisionError(err, "a bay with this name already exists at the dealership")
	}
	if err := tx.bumpDealership(ctx, dealershipID); err != nil {
		return domain.Bay{}, err
	}
	return bay, tx.Commit(ctx)
}

// NewTechnician is a technician to create: active, with these skills at this level.
type NewTechnician struct {
	DealershipID uuid.UUID
	Name         string
	SkillLevel   domain.SkillLevel
	Skills       []domain.TechnicianSkill
}

// CreateTechnician inserts an active technician with skills (no shift pattern: works the opening
// hours) and emits CONFIG_UPDATED for the technician. An unknown dealership is a 404.
func (s *Store) CreateTechnician(ctx context.Context, t NewTechnician) (domain.TechnicianAdmin, error) {
	tx, err := s.Begin(ctx)
	if err != nil {
		return domain.TechnicianAdmin{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id := uuid.New()
	if _, err := tx.tx.Exec(ctx, `
		WITH t AS (
			INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES ($1, $2, $3, $4)
			RETURNING id
		)
		INSERT INTO technician_skills (technician_id, skill)
		SELECT t.id, s FROM t CROSS JOIN unnest($5::smallint[]) AS s`,
		id, t.DealershipID, t.Name, t.SkillLevel, skillCodes(t.Skills)); err != nil {
		return domain.TechnicianAdmin{}, provisionError(err, "")
	}
	created, err := getTechnicianAdmin(ctx, tx.tx, id)
	if err != nil {
		return domain.TechnicianAdmin{}, err
	}
	if err := tx.InsertOutbox(ctx, domain.AggregateTechnician, id, domain.EventConfigUpdated, created); err != nil {
		return domain.TechnicianAdmin{}, err
	}
	return created, tx.Commit(ctx)
}

// bumpDealership bumps the dealership's version and emits CONFIG_UPDATED with its new state.
func (t *Tx) bumpDealership(ctx context.Context, id uuid.UUID) error {
	updated, err := scanDealership(t.tx.QueryRow(ctx,
		`UPDATE dealerships SET version = version + 1 WHERE id = $1 RETURNING `+dealershipColumns, id))
	if err != nil {
		return missing(err, "dealership not found")
	}
	return t.InsertOutbox(ctx, domain.AggregateDealership, id, domain.EventConfigUpdated, updated)
}

// provisionError maps a unique violation to 409 (duplicate, "" = generic message) and a missing
// dealership (foreign key) to 404.
func provisionError(err error, duplicate string) error {
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "23505":
			if duplicate == "" {
				duplicate = "already exists"
			}
			return domain.NewError(domain.CodeConflict, duplicate)
		case "23503":
			return domain.NewError(domain.CodeNotFound, "dealership not found")
		}
	}
	return translate(err)
}
