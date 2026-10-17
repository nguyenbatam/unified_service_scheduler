package booking

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

type allocation struct {
	plan           slotPlan
	dealer         domain.Dealership
	vehicle        domain.Vehicle
	req            Requirement
	isWaiter       bool
	idempotencyKey *string
	fingerprint    string
	by             *uuid.UUID          // acting user (created_by on insert, history actor)
	moving         *domain.Appointment // reschedule: update this row in place instead of inserting
	eventType      domain.EventType
	// repairBy is the history actor of a booking moved to make room (makeRoom): the acting staff
	// member, or nil (system) for a customer, who should not appear on someone else's booking.
	repairBy *uuid.UUID
	// lockFirst waits for the vehicle and day locks before reading the candidates (the gate saw
	// other bookings of this day ahead, or a booking of this dealership collided lately on this
	// instance, Service.hot): under contention a read taken before the lock is stale, so a loser
	// would collide and re-read while holding the lock, and a winner would be placed on a ranking
	// (idle gaps, loads) the bookings ahead have since changed. Otherwise the day lock is only
	// tried before the read (taken if free: the read is exact at no cost) and else waited for last,
	// so bookings of one day that do not compete for the same resources read in parallel.
	lockFirst bool
}

func (a allocation) exclude() uuid.UUID {
	if a.moving != nil {
		return a.moving.ID
	}
	return uuid.Nil
}

// arrivalLock is the vehicle + day locks (and staging count) of the slot. A reschedule to another
// day also locks the day it leaves (store.ArrivalLock.Leaving), in day order: its move rewrites a
// GiST entry there too.
func (a allocation) arrivalLock() store.ArrivalLock {
	l := arrivalLock(a.dealer, a.plan.loc, a.vehicle.ID, a.plan.slot.Start, a.exclude())
	if a.moving != nil && DateFrom(a.moving.StartAt, a.plan.loc) != a.plan.day {
		l.Leaving = a.moving.StartAt
	}
	return l
}

// allocate picks a free bay + qualified, on-duty technician and inserts (or moves) the appointment
// with its event in tx. The GiST exclusion constraints remain the double-booking guarantee; the
// pre-checks only make errors precise and skip candidates that are certain to fail.
//
// Lock order (Book, Reschedule, Reinstate, Reassign, Extend, an early Start, and the overrun sweep
// all follow it, so waits cannot form a cycle; a writer touching several local days — a hold that
// may cross midnight, a reschedule to another day — takes their locks in day order):
//
//	appointment row FOR NO KEY UPDATE (reschedule, reinstate, …) / idempotency advisory lock (book)
//	→ share locks: dealership, technician (LockBookableTechnician)
//	  — in any relative order: share locks never conflict with each other
//	→ vehicle advisory lock → dealership-day advisory lock(s) → GiST inserts/updates.
//
// Each pass takes the vehicle + day locks in a savepoint it can give back
// (store.LockArrivalAndCandidatesRevocably), in the round trip that reads the candidates: waiting
// for the day lock when the day is contended (lockFirst), else only trying it (ArrivalLock.Try;
// a hold of one local day). Holding it, the read is exact. Not holding it (busy), the read is
// stale and the day lock is waited for right before the first insert (lock-last); only the vehicle
// lock is held meanwhile, which no booking of another car waits for. While the pass holds a day
// lock it can give back, it checks technicians — candidates and the repair's target — without
// waiting (TechCheck.NoWait): a technician a roster writer holds (FOR UPDATE, up to lock_timeout)
// makes it roll the savepoint back — releasing the day lock and everything done under it — wait
// for that technician's share lock on the TX itself, outside the day's queue (share locks come
// before the vehicle and day locks in the order above), and place the booking again. The share
// lock then holds until the TX ends, so the next pass checks that technician without waiting. So a
// roster edit never stalls the whole dealership-day, and the outcome is the one a booking that
// waited for the technician would get (no technician is passed over for being mid-edit). After
// maxGiveBacks give-backs (that many technicians the booking needs under concurrent roster edits)
// the last pass is plain lock-last, as a cross-day hold that is not lockFirst always is: it may
// check a technician after the locks (a later candidate, the repair's target), which is safe
// because the exclusive lockers it can wait for — roster writers, deactivation — hold one
// technician row and never wait for a dealership-day lock, an appointment row, or a GiST entry; a
// pass may check one while holding only the vehicle lock, safe for the same reason.
//
// Round trips are kept low while the day lock is held: lock + count + the first candidate's
// SAVEPOINT is one (pgx batch), each later technician check is one, the insert (or move) with its
// event is one, and a winning candidate's savepoint is never released explicitly (see try), so a
// first-candidate win holds the lock for that write and the caller's COMMIT.
func (s *Service) allocate(ctx context.Context, tx *store.Tx, a allocation) (domain.Appointment, error) {
	for pass := 0; ; pass++ {
		saved, locked, err := s.placeOnce(ctx, tx, a, pass < maxGiveBacks)
		if !errors.Is(err, errDayGivenBack) {
			return saved, err
		}
		// A technician was mid-edit: the day lock is given back; wait for it outside the queue.
		if err := tx.ShareLockTechnician(ctx, locked); err != nil {
			return domain.Appointment{}, err
		}
	}
}

// maxGiveBacks bounds the passes that give the day lock back (each for another technician met
// mid-edit, then share-locked): past it the last pass is plain lock-last.
const maxGiveBacks = 3

// errDayGivenBack is a pass that met a technician locked by a roster writer and rolled its
// day-lock savepoint back (placeOnce): allocate waits for that technician and places again.
var errDayGivenBack = errors.New("dealership-day lock given back: technician locked by a concurrent writer")

// placeOnce is one allocation pass (revocable: its locks may be given back, see allocate); after a
// give-back it returns errDayGivenBack and the technician met mid-edit. A pass that collided with
// another booking's write marks the dealership hot, so this instance's next bookings there wait
// for the day lock before reading (Service.hot).
func (s *Service) placeOnce(ctx context.Context, tx *store.Tx, a allocation, revocable bool) (domain.Appointment, uuid.UUID, error) {
	p := newPlacement(s, tx, a)
	p.first = revocable
	saved, err := p.place(ctx)
	if p.collided {
		s.hot.mark(a.dealer.ID, time.Now())
	}
	if errors.Is(err, store.ErrTechnicianLocked) && p.revocable != nil {
		if rbErr := p.revocable.Rollback(ctx); rbErr != nil {
			return domain.Appointment{}, uuid.Nil, rbErr
		}
		return domain.Appointment{}, p.lockedTech, errDayGivenBack
	}
	return saved, uuid.Nil, err
}

func (p *placement) place(ctx context.Context) (domain.Appointment, error) {
	cand, err := p.firstCandidates(ctx)
	if err != nil {
		return domain.Appointment{}, err
	}
	// A read before the day lock is stale under contention: a loser would try every listed bay and
	// technician in turn (a failed insert each) while holding the lock everyone else of that day
	// waits for. Once the lock is held no other booking of that dealership-day can commit, so one
	// fresh read is exact: the loser fails at once, or finds what is really free. A list read under
	// the lock to begin with is exact already.
	exact := p.staged
	for {
		saved, collided, err := p.try(ctx, cand, !exact)
		if err != nil || !collided {
			return saved, err
		}
		exact = true
		if cand, err = p.reread(ctx); err != nil {
			return domain.Appointment{}, err
		}
	}
}

// placement is one placement pass's state across its candidate rounds (placeOnce makes a fresh one
// per pass, so a give-back starts over).
type placement struct {
	s        *Service
	tx       *store.Tx
	a        allocation
	bq       store.BayQuery
	tq       store.TechQuery
	snapshot domain.Appointment
	staged   bool // vehicle + day locks held (until the TX ends) and the staging cap judged
	first    bool // a revocable pass: its locks are taken in a savepoint it can give back (allocate)
	// lockedTech is the technician a NoWait check found locked by a roster writer (ErrTechnicianLocked).
	lockedTech uuid.UUID
	// revocable is that savepoint: once the day lock is held in it, technicians are checked NoWait,
	// and a locked one rolls it back (placeOnce) instead of waiting.
	revocable *store.Tx
	arrivals  int  // the staging count read under them
	repaired  bool // makeRoom moved a booking (at most once per allocation)
	// bookable caches LockBookableTechnician per technician across bays and rounds. The check runs
	// on tx, not inside a candidate's savepoint: rolling a savepoint back would release the FOR
	// SHARE row lock taken in it, while here it holds until the booking TX ends, so a cached
	// "bookable" stays true (roster writers lock the technician FOR UPDATE and wait for this TX).
	bookable  map[uuid.UUID]bool
	collided  bool // lost a race to another booking (vs. candidates that simply became unbookable)
	deadlocks int
}

func newPlacement(s *Service, tx *store.Tx, a allocation) *placement {
	slot := a.plan.slot
	window := store.Slot{
		Start: slot.Start, End: slot.End,
		DayStart: a.plan.day.Start(a.plan.loc), DayEnd: a.plan.day.Next(a.plan.loc),
		Lo: a.plan.lo, Hi: a.plan.hi,
		Exclude: a.exclude(),
	}
	return &placement{
		s: s, tx: tx, a: a,
		bq:       store.BayQuery{DealershipID: a.dealer.ID, Required: a.req.BayType, SoftUpgrade: a.req.SoftUpgrade(a.isWaiter, a.dealer.Policy), Slot: window},
		tq:       store.TechQuery{DealershipID: a.dealer.ID, Skills: a.req.Skills, MinLevel: a.req.MinSkillLevel, Slot: window},
		snapshot: a.req.Snapshot(),
		bookable: map[uuid.UUID]bool{},
	}
}

// firstCandidates reads the free bays and technicians. A revocable pass reads them in the round
// trip that takes the vehicle lock and waits for (lockFirst) or tries the day lock, inside a
// savepoint it can give back; when the day lock is held the read and the staging count are exact
// (each read a statement of its own after the locks). A hold of several local days that is not
// lockFirst, and the plain last pass, read without locks. With nothing free as read, one confirmed booking may move to
// make room (repair.go); that is judged before staging, like availability, so every replica
// refuses the same way.
func (p *placement) firstCandidates(ctx context.Context) (store.Candidates, error) {
	var cand store.Candidates
	var err error
	if l := p.a.arrivalLock(); p.first && (p.a.lockFirst || l.SingleDay()) {
		l.Try = !p.a.lockFirst
		var r store.RevocableRead
		r, err = p.tx.LockArrivalAndCandidatesRevocably(ctx, l, p.a.vehicle.ID, p.bq, p.tq)
		cand, p.revocable, p.staged, p.arrivals = r.Candidates, r.Savepoint, r.Held, r.Arrivals
	} else {
		cand, err = p.tx.AllocationCandidates(ctx, p.a.vehicle.ID, p.bq, p.tq)
	}
	switch {
	case err != nil:
		return store.Candidates{}, err
	case cand.VehicleBusy:
		return store.Candidates{}, vehicleOverlap()
	case len(cand.Bays) == 0 || len(cand.Techs) == 0:
		return p.makeRoom(ctx, cand) // judges staging itself, before it moves anything
	case p.staged:
		return cand, stagingFull(p.a.dealer, p.arrivals, "")
	}
	return cand, nil
}

// reread reads the candidates again after the first lost race, under the day lock (a write was
// tried, so it is held): exact. With nothing free any more the one-move repair may still make room,
// as availability and a retry would (the staging cap was judged under this same lock); otherwise
// the bookings ahead in the queue took everything left.
func (p *placement) reread(ctx context.Context) (store.Candidates, error) {
	cand, err := p.tx.AllocationCandidates(ctx, p.a.vehicle.ID, p.bq, p.tq)
	switch {
	case err != nil:
		return store.Candidates{}, err
	case cand.VehicleBusy:
		return store.Candidates{}, vehicleOverlap()
	case len(cand.Bays) > 0 && len(cand.Techs) > 0:
		return cand, nil
	case !p.repaired:
		cand, err = p.makeRoom(ctx, cand)
		if de, ok := domain.AsError(err); !ok || de.Code != domain.CodeUnavailable {
			return cand, err
		}
	}
	return store.Candidates{}, raceLost()
}

// try walks the candidates, each pair in its own savepoint, until one is placed. A lost race on a
// bay or technician (GiST) or a deadlock marks the allocation collided; with reread it returns at
// once (collided true) so the caller re-reads, else it moves on: past the bay, or past the
// technician. A second deadlock means the other side holds something outside this savepoint, so it
// gives up instead of looping. Exhausting the list is a 409 after a collision, else 422.
func (p *placement) try(ctx context.Context, cand store.Candidates, reread bool) (domain.Appointment, bool, error) {
	skip := map[uuid.UUID]bool{} // technicians unbookable or taken since the list was read
bays:
	for _, bay := range cand.Bays {
		for _, tech := range cand.Techs {
			if skip[tech.ID] {
				continue
			}
			sp, err := p.claim(ctx, tech.ID)
			if err != nil {
				return domain.Appointment{}, false, err
			}
			if sp == nil {
				// Deactivated, downgraded, or rostered off by an admin since the candidates were read.
				skip[tech.ID] = true
				continue
			}
			saved, err := p.s.place(ctx, sp, p.a, p.snapshot, bay.ID, tech.ID)
			if err == nil {
				// The winning savepoint is left open instead of RELEASEd (pgx's nested Commit is a
				// round trip under the dealership-day lock): the caller's COMMIT releases it
				// implicitly and its rollback discards it with everything else. Safe because this is
				// the last savepoint of the TX — allocate returns at once, and Book/Reschedule only
				// commit after it — and a nested Tx holds no resources: the handle is just dropped.
				return saved, false, nil
			}
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return domain.Appointment{}, false, rbErr
			}
			constraint := store.ExclusionConstraint(err)
			deadlock := store.IsDeadlock(err)
			switch {
			case constraint == store.ConstraintVehicleOverlap:
				return domain.Appointment{}, false, vehicleOverlap()
			case store.IsIdempotencyViolation(err):
				// Belt and braces (see store.IsIdempotencyViolation): the same key committed
				// concurrently; a retry with that key replays it.
				return domain.Appointment{}, false, domain.NewError(domain.CodeConflict, "a request with this Idempotency-Key was saved concurrently; retry it to get the appointment")
			case constraint != store.ConstraintBayOverlap && constraint != store.ConstraintTechOverlap && !deadlock:
				return domain.Appointment{}, false, err
			}
			// Lost a race. Two writes conflicting on a GiST constraint can also each wait on the
			// other's uncommitted entry; Postgres aborts one: a lost race too, not a server error.
			p.collided = true
			if deadlock {
				if p.deadlocks++; p.deadlocks > 1 {
					return domain.Appointment{}, false, raceLost()
				}
			}
			if reread {
				return domain.Appointment{}, true, nil
			}
			switch constraint {
			case store.ConstraintBayOverlap:
				continue bays // taken concurrently: every technician would fail on it
			case store.ConstraintTechOverlap:
				skip[tech.ID] = true
			}
		}
	}
	if p.collided {
		return domain.Appointment{}, false, raceLost()
	}
	// Every candidate became unbookable (e.g. technician deactivated mid-request).
	return domain.Appointment{}, false, nothingFree()
}

// claim re-checks the technician under its share lock (cached per technician) and opens the
// savepoint the candidate writes in; nil (and no error) when the technician is not bookable. Under
// the day lock the savepoint opens in the check's round trip (after the share lock, so rolling the
// savepoint back keeps it); the first candidate of an uncontended booking opens it in the round
// trip of the vehicle + day locks (stage).
func (p *placement) claim(ctx context.Context, techID uuid.UUID) (*store.Tx, error) {
	ok, checked := p.bookable[techID]
	var sp *store.Tx
	if !checked {
		check := store.TechCheck{
			TechnicianID: techID, DealershipID: p.a.dealer.ID, Skills: p.a.req.Skills, MinLevel: p.a.req.MinSkillLevel,
			Start: p.a.plan.slot.Start, End: p.a.plan.slot.End, NoWait: p.noWait(),
		}
		var err error
		if p.staged {
			ok, sp, err = p.tx.LockBookableTechnicianSavepoint(ctx, check)
		} else {
			ok, err = p.tx.LockBookableTechnician(ctx, check)
		}
		if errors.Is(err, store.ErrTechnicianLocked) {
			p.lockedTech = techID
		}
		if err != nil {
			return nil, err
		}
		p.bookable[techID] = ok
	}
	switch {
	case !ok && sp != nil:
		return nil, sp.Rollback(ctx)
	case !ok:
		return nil, nil
	case sp != nil:
		return sp, nil
	case p.staged:
		return p.tx.Begin(ctx)
	}
	return p.stage(ctx)
}

// noWait reports whether technician checks must not wait: the day lock is held in a savepoint this
// pass can give back (see allocate).
func (p *placement) noWait() bool {
	return p.staged && p.revocable != nil
}

// stage takes the vehicle + day locks and the staging count with the first candidate's savepoint in
// one round trip (store.ArrivalLock.Savepoint), right before the first insert, and judges the cap.
func (p *placement) stage(ctx context.Context) (*store.Tx, error) {
	l := p.a.arrivalLock()
	l.Savepoint = true
	n, sp, err := p.tx.LockArrival(ctx, l)
	if err != nil {
		return nil, err
	}
	p.staged, p.arrivals = true, n
	return sp, stagingFull(p.a.dealer, n, "")
}

// claimStagingRoom takes the vehicle and dealership-day advisory locks (Tx.LockArrival), held until
// the TX ends, and enforces the arrival window's cap — Reinstate, before re-entering a holding
// status (allocate stages its own: placement.stage, firstCandidates). The locks are taken even when
// the dealership has no cap, so writers that could collide queue instead of deadlocking on each
// other's GiST entries. The count is a separate statement queued after the locks, so its READ
// COMMITTED snapshot includes every booking that committed into the window while this one waited.
// exclude is the appointment's own arrival; hint is appended to the 422 message.
func claimStagingRoom(ctx context.Context, tx *store.Tx, dealer domain.Dealership, loc *time.Location, vehicleID uuid.UUID, start time.Time, exclude uuid.UUID, hint string) error {
	n, _, err := tx.LockArrival(ctx, arrivalLock(dealer, loc, vehicleID, start, exclude))
	if err != nil {
		return err
	}
	return stagingFull(dealer, n, hint)
}

// stagingFull is the staging check: 422 when the window already holds the cap.
func stagingFull(dealer domain.Dealership, arrivals int, hint string) error {
	if dealer.StagingCapacity > 0 && arrivals >= int(dealer.StagingCapacity) {
		return domain.NewError(domain.CodeUnavailable, "staging / check-in area is at capacity for that arrival window"+hint)
	}
	return nil
}

// place writes the chosen bay + technician: a new row (book) or an in-place move (reschedule).
func (s *Service) place(ctx context.Context, sp *store.Tx, a allocation, snapshot domain.Appointment, bayID, techID uuid.UUID) (domain.Appointment, error) {
	slot := a.plan.slot
	if a.moving != nil {
		// One round trip under the day lock: the move and its event.
		return sp.MoveAppointmentWithEvent(ctx, store.Move{
			ID: a.moving.ID, ServiceBayID: bayID, TechnicianID: techID,
			StartAt: slot.Start, EndAt: slot.End, IsWaiter: a.isWaiter,
		}, a.eventType, a.by)
	}
	appt := snapshot
	appt.ID = uuid.New()
	appt.DealershipID = a.dealer.ID
	appt.CustomerID = a.vehicle.CustomerID
	appt.VehicleID = a.vehicle.ID
	appt.ServiceBayID = bayID
	appt.TechnicianID = techID
	appt.StartAt, appt.EndAt = slot.Start, slot.End
	appt.Status = domain.StatusConfirmed
	appt.IsWaiter = a.isWaiter
	return sp.InsertAppointmentWithEvent(ctx, store.NewAppointment{
		Appointment: appt, IdempotencyKey: a.idempotencyKey, Fingerprint: a.fingerprint, CreatedBy: a.by,
	}, a.eventType)
}

func raceLost() error {
	return domain.NewError(domain.CodeConflict, "resources were booked by a concurrent request")
}
