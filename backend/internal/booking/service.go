package booking

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

type Service struct {
	store *store.Store
	idem  *cache.Idempotency
	now   func() time.Time
	gate  *dayGate    // admission to the dealership-day lock (Book, Reschedule)
	zones *zoneCache  // dealership timezones for the gate key
	hot   *hotDealers // dealerships whose bookings collided lately (allocation.lockFirst)
	// lockFirst makes every booking and reschedule read its candidates under the day lock.
	lockFirst bool
}

type Option func(*Service)

// WithClock overrides "now" (tests, seed fixtures). Postgres now() still stamps lifecycle times.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithLockFirst makes every booking and reschedule take the dealership-day lock before reading its
// candidates, as if the day were always contended (tests and benchmarks of that path).
func WithLockFirst() Option {
	return func(s *Service) { s.lockFirst = true }
}

func New(st *store.Store, idem *cache.Idempotency, opts ...Option) *Service {
	s := &Service{store: st, idem: idem, now: time.Now, gate: newDayGate(dayGateWidth), zones: &zoneCache{}, hot: &hotDealers{}}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) Ping(ctx context.Context) error {
	return s.store.Ping(ctx)
}

func (s *Service) PingRedis(ctx context.Context) error {
	if s.idem == nil {
		return nil
	}
	return s.idem.Ping(ctx)
}

// ListDealerships returns one page of dealerships in (name, id) order, each with its local today.
func (s *Service) ListDealerships(ctx context.Context, limit int, after *store.NameCursor) ([]domain.Dealership, *store.NameCursor, error) {
	items, next, err := s.store.ListDealershipsPage(ctx, limit, after)
	if err != nil {
		return nil, nil, err
	}
	now := s.now()
	for i := range items {
		loc, err := loadLocation(items[i].Timezone)
		if err != nil {
			return nil, nil, err
		}
		items[i].Today = Today(loc, now).String()
	}
	return items, next, nil
}

// ListServiceTypes returns one page of the service catalog in (duration, name, id) order.
func (s *Service) ListServiceTypes(ctx context.Context, limit int, after *store.ServiceTypeCursor) ([]domain.ServiceType, *store.ServiceTypeCursor, error) {
	return s.store.ListServiceTypesPage(ctx, limit, after)
}

// Schedule is the dealership day board. It exposes every customer's booking, so staff only.
func (s *Service) Schedule(ctx context.Context, actor domain.Actor, dealershipID uuid.UUID, day Date) (domain.Schedule, error) {
	if !actor.IsStaff() {
		return domain.Schedule{}, staffOnly()
	}
	if !actor.CanAccessDealership(dealershipID) {
		return domain.Schedule{}, otherDealership()
	}
	dealer, err := s.store.GetDealership(ctx, dealershipID)
	if err != nil {
		return domain.Schedule{}, err
	}
	loc, err := loadLocation(dealer.Timezone)
	if err != nil {
		return domain.Schedule{}, err
	}
	// The day's bounds need the dealership's timezone; everything else comes in one round trip.
	board, err := s.store.ScheduleBoard(ctx, dealer.ID, day.String(), day.ISOWeekday(), day.Start(loc), day.Next(loc))
	if err != nil {
		return domain.Schedule{}, err
	}
	hours := ResolveDay(dealer, day, board.Rules)
	sched := domain.Schedule{
		Date:         day.String(),
		Timezone:     dealer.Timezone,
		OpenTime:     hours.OpenTime,
		CloseTime:    hours.CloseTime,
		LunchStart:   hours.LunchStart,
		LunchEnd:     hours.LunchEnd,
		Bays:         board.Bays,
		Technicians:  board.Technicians,
		Appointments: board.Appointments,
	}
	if hours.Closed {
		// Keep the default grid so work still on the board (e.g. booked before the closure) renders.
		sched.OpenTime, sched.CloseTime = dealer.OpenTime, dealer.CloseTime
		sched.LunchStart, sched.LunchEnd = "", ""
		sched.Closed, sched.ClosedReason = true, hours.Reason
	}
	return sched, nil
}
