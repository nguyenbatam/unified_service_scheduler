package seed

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// Demo users — fixed local credentials (SEED=true).
const (
	DemoAdminEmail         = "admin@scheduler.local"
	DemoAdminPassword      = "admin123"
	DemoDispatcherEmail    = "dispatcher@scheduler.local"
	DemoDispatcherPassword = "dispatch123"
	DemoCustomerEmail      = "customer@scheduler.local" // linked to CustomerA (Amelia Clarke)
	DemoCustomerPassword   = "customer123"
)

var (
	DemoAdminID      = uuid.MustParse("a1000000-0000-4000-8000-000000000001")
	DemoDispatcherID = uuid.MustParse("a1000000-0000-4000-8000-000000000002")
	DemoCustomerID   = uuid.MustParse("a1000000-0000-4000-8000-000000000003")
)

var (
	DealerNorth = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	DealerRiver = uuid.MustParse("22222222-2222-4222-8222-222222222222")

	CustomerA = uuid.MustParse("c1000000-0000-4000-8000-000000000001")
	CustomerB = uuid.MustParse("c1000000-0000-4000-8000-000000000002")
	CustomerC = uuid.MustParse("c1000000-0000-4000-8000-000000000003")

	VehicleA = uuid.MustParse("e1000000-0000-4000-8000-000000000001")
	VehicleB = uuid.MustParse("e1000000-0000-4000-8000-000000000002")
	VehicleC = uuid.MustParse("e1000000-0000-4000-8000-000000000003")

	ServiceOil   = uuid.MustParse("51000000-0000-4000-8000-000000000001")
	ServiceBrake = uuid.MustParse("51000000-0000-4000-8000-000000000002")
	ServiceMajor = uuid.MustParse("51000000-0000-4000-8000-000000000003")

	BayN1 = uuid.MustParse("b1000000-0000-4000-8000-000000000001")
	BayN2 = uuid.MustParse("b1000000-0000-4000-8000-000000000002")
	BayN3 = uuid.MustParse("b1000000-0000-4000-8000-000000000003")
	BayN4 = uuid.MustParse("b1000000-0000-4000-8000-000000000004")
	BayR1 = uuid.MustParse("b2000000-0000-4000-8000-000000000001")
	BayR2 = uuid.MustParse("b2000000-0000-4000-8000-000000000002")

	TechAlex  = uuid.MustParse("71000000-0000-4000-8000-000000000001")
	TechBlair = uuid.MustParse("71000000-0000-4000-8000-000000000002")
	TechCasey = uuid.MustParse("71000000-0000-4000-8000-000000000003")
	TechDrew  = uuid.MustParse("71000000-0000-4000-8000-000000000004")
	TechEve   = uuid.MustParse("72000000-0000-4000-8000-000000000001")
	TechFinn  = uuid.MustParse("72000000-0000-4000-8000-000000000002")
)

// demoLead is how far ahead of now the first demo booking must start; otherwise the demo bookings
// go on the next day (a mid-day start must not book the morning, where the no-show sweep would
// flip them within seconds).
const demoLead = time.Hour

// Option configures Run.
type Option func(*options)

type options struct {
	now func() time.Time
}

// WithClock sets "now" for placing the demo bookings (tests pin it to the start of the day so the
// fixtures land on today's board).
func WithClock(now func() time.Time) Option {
	return func(o *options) { o.now = now }
}

// Run inserts the demo dealerships, catalog, bays, and technicians, creates the demo users if missing
// (fixed local credentials; an existing account is never reset), then books a few demo jobs. Configuration inserts are ON CONFLICT DO NOTHING
// (any unique key, not just the id: a VIN or email registered through the API before the seed
// runs skips that row instead of failing startup): a restart never rewrites what an admin has
// edited since (hours, lunch, staging, skill levels, skills), which would bypass the
// stranded-booking checks. Demo bookings are keyed per day, so a
// restart the same day replays them.
func Run(ctx context.Context, st *store.Store, log *slog.Logger, opts ...Option) error {
	if log == nil {
		log = slog.Default()
	}
	o := options{now: time.Now}
	for _, opt := range opts {
		opt(&o)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO dealerships (id, name, timezone, open_time, close_time, lunch_start, lunch_end, staging_capacity) VALUES
			($1, 'Northside Motors', 'Europe/London', TIME '08:00', TIME '17:00', TIME '12:00', TIME '13:00', 2),
			($2, 'Riverside Service', 'Europe/London', TIME '08:00', TIME '18:00', NULL, NULL, 0)
			ON CONFLICT DO NOTHING`, []any{DealerNorth, DealerRiver}},
		{`INSERT INTO customers (id, name, phone, email) VALUES
			($1, 'Amelia Clarke', '07700 900111', 'amelia.clarke@example.com'),
			($2, 'Noah Patel', '07700 900222', 'noah.patel@example.com'),
			($3, 'Sofia Rahman', '07700 900333', 'sofia.rahman@example.com')
			ON CONFLICT DO NOTHING`, []any{CustomerA, CustomerB, CustomerC}},
		{`INSERT INTO vehicles (id, customer_id, vin, make, model, year) VALUES
			($1, $2, 'WBA00000000000001', 'BMW', '3 Series', 2022),
			($3, $4, 'WAU00000000000002', 'Audi', 'A4', 2021),
			($5, $6, 'SB100000000000003', 'Toyota', 'RAV4', 2023)
			ON CONFLICT DO NOTHING`, []any{VehicleA, CustomerA, VehicleB, CustomerB, VehicleC, CustomerC}},
		{`INSERT INTO service_types (id, code, name, duration_minutes, required_skill, required_bay_type, min_skill_level) VALUES
			($1, 'OIL', 'Oil Change', 30, 1, 1, 1),
			($2, 'BRAKE', 'Brake Service', 90, 2, 2, 2),
			($3, 'MAJOR', 'Major Service', 180, 3, 2, 3)
			ON CONFLICT DO NOTHING`, []any{ServiceOil, ServiceBrake, ServiceMajor}},
		{`INSERT INTO service_bays (id, dealership_id, name, bay_type) VALUES
			($1, $2, 'Bay 1', 1),
			($3, $2, 'Bay 2', 2),
			($4, $2, 'Bay 3', 2),
			($5, $2, 'Bay 4', 3),
			($6, $7, 'Bay A', 1),
			($8, $7, 'Bay B', 2)
			ON CONFLICT DO NOTHING`, []any{BayN1, DealerNorth, BayN2, BayN3, BayN4, BayR1, DealerRiver, BayR2}},
		// Skills only for technicians inserted now: a restart must not re-add a skill an admin removed.
		{`WITH added AS (
			INSERT INTO technicians (id, dealership_id, name, skill_level) VALUES
				($1, $2, 'Alex Morgan', 2), ($3, $2, 'Blair Singh', 3), ($4, $2, 'Casey Nguyen', 3), ($5, $2, 'Drew Walsh', 1),
				($6, $7, 'Eve Okonkwo', 3), ($8, $7, 'Finn Adler', 2)
			ON CONFLICT DO NOTHING
			RETURNING id
		)
		INSERT INTO technician_skills (technician_id, skill)
		SELECT s.technician_id, s.skill
		FROM (VALUES
			($1::uuid, 1::smallint), ($1, 3),
			($3, 2), ($3, 3),
			($4, 1), ($4, 2),
			($5, 3),
			($6, 1), ($6, 2), ($6, 3),
			($8, 3)
		) AS s (technician_id, skill)
		JOIN added ON added.id = s.technician_id
		ON CONFLICT DO NOTHING`, []any{TechAlex, DealerNorth, TechBlair, TechCasey, TechDrew, TechEve, DealerRiver, TechFinn}},
	}
	for _, statement := range statements {
		if err := st.Exec(ctx, statement.sql, statement.args...); err != nil {
			return err
		}
	}
	users := []struct {
		user     domain.User
		password string
	}{
		{domain.User{ID: DemoAdminID, Email: DemoAdminEmail, Name: "Demo Admin", Role: domain.RoleAdmin}, DemoAdminPassword},
		// The dispatcher works at Northside only; the admin is group-wide (no dealership).
		{domain.User{ID: DemoDispatcherID, Email: DemoDispatcherEmail, Name: "Demo Dispatcher", Role: domain.RoleDispatcher, DealershipID: DealerNorth}, DemoDispatcherPassword},
		{domain.User{ID: DemoCustomerID, Email: DemoCustomerEmail, Name: "Amelia Clarke", Role: domain.RoleCustomer, CustomerID: CustomerA}, DemoCustomerPassword},
	}
	for _, u := range users {
		hash, err := auth.HashPassword(u.password)
		if err != nil {
			return err
		}
		u.user.PasswordHash = hash
		if err := st.EnsureUser(ctx, u.user); err != nil {
			return err
		}
		log.Info("seed user", "email", u.user.Email, "role", u.user.Role.Name())
	}

	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		return err
	}
	now := o.now()
	day := booking.Today(loc, now)
	if first := (booking.Clock{Hour: 9}).On(day, loc); first.Before(now.Add(demoLead)) {
		day = day.AddDays(1)
	}
	svc := booking.New(st, nil, booking.WithClock(o.now))
	demos := []booking.BookRequest{
		{
			DealershipID: DealerNorth, VehicleID: VehicleA, ServiceTypeID: ServiceOil,
			StartAt:        booking.Clock{Hour: 9}.On(day, loc),
			IdempotencyKey: "seed:north:oil:" + day.String(),
		},
		{
			DealershipID: DealerNorth, VehicleID: VehicleB, ServiceTypeID: ServiceBrake,
			StartAt:        booking.Clock{Hour: 10}.On(day, loc),
			IdempotencyKey: "seed:north:brake-1:" + day.String(),
		},
		{
			DealershipID: DealerNorth, VehicleID: VehicleC, ServiceTypeID: ServiceBrake,
			StartAt:        booking.Clock{Hour: 10}.On(day, loc),
			IdempotencyKey: "seed:north:brake-2:" + day.String(),
		},
	}
	for _, demo := range demos {
		if _, _, err := svc.Book(ctx, domain.SystemActor, demo); err != nil {
			if domainErr, ok := domain.AsError(err); ok &&
				(domainErr.Code == domain.CodeUnavailable ||
					domainErr.Code == domain.CodeConflict ||
					domainErr.Code == domain.CodeInvalid ||
					domainErr.Code == domain.CodeNotFound) { // e.g. a demo vehicle skipped above
				log.Warn("demo booking skipped", "key", demo.IdempotencyKey, "err", err)
				continue
			}
			return err
		}
	}
	log.Info("seed complete", "date", day.String())
	return nil
}
