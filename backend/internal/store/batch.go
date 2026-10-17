package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Pipelined reads. A pgx.Batch sends several statements in one round trip; the server still runs
// them one after another, in queue order, and under READ COMMITTED each statement takes its own
// snapshot when it starts — after the previous one (including a lock wait) has finished. So a
// read queued after a lock sees everything committed before the lock was granted, exactly as two
// separate round trips would; what a batch cannot do is feed one statement's result into the
// next, so only independent statements are queued together.

// batcher is a pool or a TX.
type batcher interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// sendBatch runs b in one round trip and its callbacks in order; it returns the first error (the
// statements after a failing one are not run: the TX is aborted).
func sendBatch(ctx context.Context, q batcher, b *pgx.Batch) error {
	return q.SendBatch(ctx, b).Close()
}

// queueRows queues a query whose rows are scanned into *dst.
func queueRows[T any](b *pgx.Batch, dst *[]T, scan func(pgx.Row) (T, error), sql string, args ...any) {
	b.Queue(sql, args...).Query(func(rows pgx.Rows) error {
		items, err := collect(rows, scan)
		*dst = items
		return err
	})
}

// queueOptional queues a single-row query: *dst stays nil when there is no row, so the caller can
// apply its authorization checks in order before reporting which row is missing.
func queueOptional[T any](b *pgx.Batch, dst **T, scan func(pgx.Row) (T, error), sql string, args ...any) {
	b.Queue(sql, args...).QueryRow(func(row pgx.Row) error {
		item, err := scan(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		*dst = &item
		return nil
	})
}

// BookingInputs is what a booking reads before planning the slot. A nil pointer (or nil Services:
// any line unknown) means not found.
type BookingInputs struct {
	// Existing is the appointment already booked under the request's idempotency key (nil: unused,
	// or no key given), read after the key's advisory lock.
	Existing *domain.Appointment
	Dealer   *domain.Dealership
	Vehicle  *domain.Vehicle
	Services []domain.ServiceType // in the order of the requested ids
	// Day is the calendar of the local day containing the requested start, read after the
	// dealership's FOR SHARE (so it cannot change before commit).
	Day DayRulesAt
}

// IdempotencyLookup is a booking's idempotency key in its creating user's namespace (Scope is
// the advisory lock's namespace: the user, or "system").
type IdempotencyLookup struct {
	Scope, Key string
	CreatedBy  *uuid.UUID
}

// BookingInputs reads, in one round trip: with idem, the idempotency key's advisory lock and then
// its lookup (serialising transactions carrying the same scope and key: the lookup runs once the
// lock is held, so it sees a racing original that committed while this TX waited); then the
// dealership FOR SHARE (as Tx.GetDealership), the calendar of the day containing startAt, the
// vehicle, and the service types (a plain read: the booking snapshots them, see
// queueServiceTypes) — the same locks in the same order as one read at a time (idempotency lock
// before the share locks, see booking.allocate). A replay of the key needs none of the rest; it is
// read anyway rather than spend a round trip on every first try.
func (t *Tx) BookingInputs(ctx context.Context, idem *IdempotencyLookup, dealershipID, vehicleID uuid.UUID, serviceIDs []uuid.UUID, startAt time.Time) (BookingInputs, error) {
	var in BookingInputs
	b := &pgx.Batch{}
	if idem != nil {
		b.Queue(advisoryLockSQL, advisoryKey("idem|"+idem.Scope+"|"+idem.Key)).Exec(ignoreTag)
		where, args := idempotencyWhere(idem.CreatedBy, idem.Key)
		queueOptional(b, &in.Existing, scanAppointment, appointmentSelect+` WHERE `+where, args...)
	}
	queueOptional(b, &in.Dealer, scanDealership, dealershipSelect+` WHERE id = $1 FOR SHARE`, dealershipID)
	queueDayRulesAt(b, &in.Day, dealershipID, startAt)
	queueOptional(b, &in.Vehicle, scanVehicle, vehicleSelect+` WHERE v.id = $1`, vehicleID)
	queueServiceTypes(b, &in.Services, serviceIDs)
	return in, sendBatch(ctx, t.tx, b)
}

// MoveInputs is BookingInputs for a reschedule (the booked lines are not re-read): the dealership
// FOR SHARE, the calendar of the day containing startAt, and the vehicle in one round trip.
// Services stays nil.
func (t *Tx) MoveInputs(ctx context.Context, dealershipID, vehicleID uuid.UUID, startAt time.Time) (BookingInputs, error) {
	var in BookingInputs
	b := &pgx.Batch{}
	queueOptional(b, &in.Dealer, scanDealership, dealershipSelect+` WHERE id = $1 FOR SHARE`, dealershipID)
	queueDayRulesAt(b, &in.Day, dealershipID, startAt)
	queueOptional(b, &in.Vehicle, scanVehicle, vehicleSelect+` WHERE v.id = $1`, vehicleID)
	return in, sendBatch(ctx, t.tx, b)
}

// Candidates is the allocation pre-check for one slot: whether the vehicle is already held, and
// the free bays and technicians, best first (see freeBaysSQL and freeTechniciansSQL).
type Candidates struct {
	VehicleBusy bool
	Bays        []domain.Bay
	Techs       []domain.Technician
}

// AllocationCandidates reads whether the vehicle is busy, the free bays, and the free technicians
// in one round trip (plain reads: no locks are taken, so their order does not matter).
func (t *Tx) AllocationCandidates(ctx context.Context, vehicleID uuid.UUID, bq BayQuery, tq TechQuery) (Candidates, error) {
	var c Candidates
	b := &pgx.Batch{}
	queueCandidates(b, &c, vehicleID, bq, tq)
	return c, sendBatch(ctx, t.tx, b)
}

// LockArrivalAndCandidates is LockArrival (without a savepoint) followed by AllocationCandidates in
// the same round trip: a booking that met others of its day in the admission gate reads its
// candidates under the day lock. Each read is its own statement queued after the locks, so under
// READ COMMITTED it takes its snapshot once they are granted and sees every booking of that day
// committed before (see the top of this file) — exact, at one round trip less while the lock is
// held.
func (t *Tx) LockArrivalAndCandidates(ctx context.Context, l ArrivalLock, vehicleID uuid.UUID, bq BayQuery, tq TechQuery) (int, Candidates, error) {
	var n int
	var c Candidates
	b := &pgx.Batch{}
	l.Savepoint, l.Try = false, false
	queueArrivalLock(b, l, &n, nil)
	queueCandidates(b, &c, vehicleID, bq, tq)
	return n, c, sendBatch(ctx, t.tx, b)
}

// daySavepoint names the savepoint LockArrivalAndCandidatesRevocably takes its locks in.
const daySavepoint = "alloc_day"

// RevocableRead is what LockArrivalAndCandidatesRevocably read and took.
type RevocableRead struct {
	Candidates
	// Held is true when the dealership-day lock is held (always, unless ArrivalLock.Try found it
	// taken): the candidates and Arrivals were read under it, exact. Otherwise they are a plain
	// read like AllocationCandidates, and only the vehicle lock is held.
	Held     bool
	Arrivals int
	// Savepoint is the savepoint the locks were taken in, opened in the same round trip. Rolling it
	// back gives them back — transaction-level advisory locks, like row locks, are released when the
	// savepoint they were taken in is rolled back — together with every share lock, savepoint, and
	// write made since. Locks taken before it (the idempotency lock, the dealership's share lock, a
	// moving appointment's row lock) stay held.
	Savepoint *Tx
}

// LockArrivalAndCandidatesRevocably is LockArrivalAndCandidates with the locks taken inside a
// savepoint the caller can roll back (RevocableRead.Savepoint), and with l.Try, the day lock taken
// only if it is free. A booking that would otherwise wait for a technician while holding the day
// lock (TechCheck.NoWait) rolls the savepoint back to step out of the day's queue instead.
func (t *Tx) LockArrivalAndCandidatesRevocably(ctx context.Context, l ArrivalLock, vehicleID uuid.UUID, bq BayQuery, tq TechQuery) (RevocableRead, error) {
	r := RevocableRead{Held: true}
	if l.Try && !l.SingleDay() {
		return RevocableRead{}, errors.New("store: ArrivalLock.Try needs a single-day hold")
	}
	b := &pgx.Batch{}
	l.Savepoint = false
	b.Queue(`SAVEPOINT ` + daySavepoint).Exec(ignoreTag)
	queueArrivalLock(b, l, &r.Arrivals, &r.Held)
	queueCandidates(b, &r.Candidates, vehicleID, bq, tq)
	if err := sendBatch(ctx, t.tx, b); err != nil {
		return RevocableRead{}, err
	}
	r.Savepoint = &Tx{tx: t.tx, st: t.st, events: t.events, nested: true, savepoint: daySavepoint}
	return r, nil
}

func queueCandidates(b *pgx.Batch, c *Candidates, vehicleID uuid.UUID, bq BayQuery, tq TechQuery) {
	sql, args := vehicleBusySQL(vehicleID, bq.Start, bq.End, bq.Exclude)
	b.Queue(sql, args...).QueryRow(func(row pgx.Row) error { return row.Scan(&c.VehicleBusy) })
	sql, args = freeBaysSQL(bq)
	queueRows(b, &c.Bays, scanBay, sql, args...)
	sql, args = freeTechniciansSQL(tq)
	queueRows(b, &c.Techs, scanFreeTechnician, sql, args...)
}

func ignoreTag(pgconn.CommandTag) error { return nil }
