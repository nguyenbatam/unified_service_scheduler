package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The customers phase is what production looks like from the customers' side: many people, each
// signed in to their own account, booking their own car at the same moment. N stress customers
// (stress-cust-NNN@scheduler.local, one car each) all read their availability (the customer
// policy applies: lead time, horizon) and book the offered start nearest their wish, retrying on a
// lost start, all released together through the gateway. Then:
//
//   - every booking of the phase belongs to the customer who made it (appointment customer, the
//     user recorded as creator), and each customer's vehicle list holds only their own car;
//   - isolation: a customer reading another customer's appointment, or booking another customer's
//     car, gets 404 — nothing about other customers leaks.
//
// The accounts are provisioned straight in Postgres and their access tokens minted (and re-minted
// before they expire: no refresh cookie) with the server's JWT_SECRET: from one machine, registering and logging in this many accounts would run
// into the per-IP register/login limits that protect the real endpoints (a load generator is one
// IP, production customers are many). The harness already holds the database DSN; the server still
// reloads each user per request (active, role, token version), as for any token.

const customerEmailFormat = "stress-cust-%03d@scheduler.local"

var stressNamespace = uuid.MustParse("6f1d2c3b-5a4e-4f00-9b8a-7c6d5e4f3a2b")

// stressCustomer is one provisioned account with its car and access token.
type stressCustomer struct {
	userID, customerID, vehicle uuid.UUID
	email                       string
	api                         *apiClient
}

// provisionCustomers creates (idempotently) n customer accounts with one car each and returns them
// signed in.
func provisionCustomers(ctx context.Context, conn *pgx.Conn, admin *apiClient, base string, n, workers int) ([]stressCustomer, error) {
	secret := strings.TrimSpace(os.Getenv("JWT_SECRET"))
	if secret == "" {
		return nil, fmt.Errorf("JWT_SECRET not set (it is read from .env)")
	}
	hash, err := auth.HashPassword("stress-customer-pass")
	if err != nil {
		return nil, err
	}
	signer := auth.New(secret)
	out := make([]stressCustomer, n)
	b := &pgx.Batch{}
	for i := range out {
		email := fmt.Sprintf(customerEmailFormat, i)
		c := &out[i]
		c.email = email
		c.userID = uuid.NewSHA1(stressNamespace, []byte("user|"+email))
		c.customerID = uuid.NewSHA1(stressNamespace, []byte("customer|"+email))
		name := fmt.Sprintf("Stress Customer %03d", i)
		b.Queue(`INSERT INTO customers (id, name, phone, email) VALUES ($1, $2, '07700 900000', $3) ON CONFLICT DO NOTHING`, c.customerID, name, email)
		b.Queue(`INSERT INTO users (id, email, password_hash, name, role, customer_id) VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`,
			c.userID, email, hash, name, int16(domain.RoleCustomer), c.customerID)
	}
	if err := conn.SendBatch(ctx, b).Close(); err != nil {
		return nil, fmt.Errorf("provision customers: %w", err)
	}
	versions := map[uuid.UUID]int{}
	rows, err := conn.Query(ctx, `SELECT id, token_version FROM users WHERE email LIKE 'stress-cust-%@scheduler.local' AND active`)
	if err != nil {
		return nil, err
	}
	if _, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id uuid.UUID
		var v int
		err := row.Scan(&id, &v)
		versions[id] = v
		return struct{}{}, err
	}); err != nil {
		return nil, err
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, max(1, min(workers, 32)))
	for i := range out {
		c := &out[i]
		v, ok := versions[c.userID]
		if !ok {
			return nil, fmt.Errorf("stress customer %s is missing or inactive", c.email)
		}
		user := domain.User{ID: c.userID, Email: c.email, Role: domain.RoleCustomer, CustomerID: c.customerID, TokenVersion: v}
		var onLost func(error)
		if admin.auth != nil {
			onLost = admin.auth.onLost
		}
		// No refresh cookie: the session renews itself by minting the next token (same claims)
		// before this one expires, so a long phase or bench outlives ACCESS_TOKEN_TTL too.
		sess, err := newMintedSession(c.email, func() (string, time.Time, error) { return signer.Issue(user) }, onLost)
		if err != nil {
			return nil, err
		}
		c.api = newAPI(base)
		c.api.auth = sess
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			veh, err := findOrCreateVehicle(ctx, admin, c.customerID, fmt.Sprintf("STRSC%012d", i))
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("car of %s: %w", c.email, err)
			}
			c.vehicle = veh.ID
		})
	}
	wg.Wait()
	return out, firstErr
}

// customersStats is what the phase measured.
type customersStats struct {
	mu                   sync.Mutex
	customers            int
	booked, turnedAway   int // turnedAway: nothing offered (judged against the oracle)
	gaveUp               int // lost an offered start fillTries times in a row
	unresolved, retried  int // unresolved: no definite answer (may have committed unheard)
	errored              int // a definite answer that is a bug (5xx, 400, unreadable 2xx …)
	turnedMixes          []jobMix
	falseRefusals        int
	refusals             *refusalLog // every 409/422, judged against the holds when it was decided
	refusalVerdict       refusalVerdict
	started              time.Time // when the customers were released (refusals.go: holds' positions since)
	unexpected           []string
	ids                  []uuid.UUID
	byCustomer           map[uuid.UUID]uuid.UUID // appointment → customer who booked it
	isolationChecks      int
	leaks                []string
	ownershipMismatches  []string
	foreignVehiclesShown int
}

// runCustomers releases every customer at once to book on day at w.
func runCustomers(ctx context.Context, customers []stressCustomer, w workshop, day booking.Date, mixes []jobMix, runID string) *customersStats {
	st := &customersStats{customers: len(customers), byCustomer: map[uuid.UUID]uuid.UUID{}, refusals: newRefusalLog(), started: time.Now()}
	starts, _ := dayStarts(w, day, slotMinutes)
	rng := rand.New(rand.NewPCG(uint64(len(customers)), 7)) //nolint:gosec // reproducible wishes, not security
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i, c := range customers {
		mix := mixes[rng.IntN(len(mixes))]
		desired := starts[rng.IntN(len(starts))].start
		wg.Go(func() {
			<-gate
			st.serve(ctx, c, w, day, mix, desired, fmt.Sprintf("%s:custmany:%d", runID, i))
		})
	}
	close(gate)
	wg.Wait()
	return st
}

func (st *customersStats) serve(ctx context.Context, c stressCustomer, w workshop, day booking.Date, mix jobMix, desired time.Time, key string) {
	path := fmt.Sprintf("/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s", w.dealer.ID, c.vehicle, jobQuery(mix.ids), day)
	var refused []refusal
	defer func() { st.refusals.add(refused...) }()
	for try := 1; try <= fillTries; try++ {
		resp, definite := sendUntilDefinite(ctx, c.api, http.MethodGet, path, nil, nil, probeAttempts)
		var avail domain.Availability
		switch {
		case !definite:
			st.note(&st.unresolved, "", false)
			return
		case resp.code != http.StatusOK || json.Unmarshal(resp.body, &avail) != nil:
			st.note(&st.errored, fmt.Sprintf("availability %d: %s", resp.code, truncate(string(resp.body), 100)), true)
			return
		}
		slot, ok := nearestOffered(avail.Slots, desired)
		if !ok {
			st.note(&st.turnedAway, "", false)
			st.mu.Lock()
			st.turnedMixes = append(st.turnedMixes, mix)
			st.mu.Unlock()
			return
		}
		sent := time.Now()
		resp, definite = sendUntilDefinite(ctx, c.api, http.MethodPost, "/api/v1/appointments",
			bookingPayload(w.dealer.ID, c.vehicle, mix.ids, slot.Start), map[string]string{"Idempotency-Key": fmt.Sprintf("%s:%d", key, try)}, probeAttempts)
		answered := time.Now()
		switch {
		case resp.code == http.StatusCreated || resp.code == http.StatusOK:
			var appt domain.Appointment
			if err := json.Unmarshal(resp.body, &appt); err != nil || appt.ID == uuid.Nil {
				st.note(&st.errored, fmt.Sprintf("booking %d with an unreadable body", resp.code), true)
				return
			}
			for i := range refused {
				refused[i].exclude = appt.ID // the customer's own booking came after its refusals
			}
			st.mu.Lock()
			st.booked++
			if try > 1 {
				st.retried++
			}
			st.ids = append(st.ids, appt.ID)
			st.byCustomer[appt.ID] = c.customerID
			st.mu.Unlock()
			return
		case resp.code == http.StatusConflict || resp.code == http.StatusUnprocessableEntity:
			refused = append(refused, refusal{mix: mix, start: slot.Start, vehicle: c.vehicle, sent: sent, at: answered})
			continue
		case !definite:
			st.note(&st.unresolved, "", false)
			return
		default:
			st.note(&st.errored, fmt.Sprintf("booking %d: %s", resp.code, truncate(string(resp.body), 100)), true)
			return
		}
	}
	st.note(&st.gaveUp, "", false)
}

func (st *customersStats) note(counter *int, msg string, unexpected bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	*counter++
	if unexpected && len(st.unexpected) < 5 {
		st.unexpected = append(st.unexpected, msg)
	}
}

// checkIsolation has customers probe each other's data (up to pairs probes) and verifies the
// phase's rows in Postgres belong to the customers who booked them.
func (st *customersStats) checkIsolation(ctx context.Context, conn *pgx.Conn, customers []stressCustomer, w workshop, day booking.Date, mix jobMix, pairs int, runID string) error {
	var bookedIDs []uuid.UUID
	for id := range st.byCustomer {
		bookedIDs = append(bookedIDs, id)
	}
	owner := map[uuid.UUID]stressCustomer{}
	for _, c := range customers {
		owner[c.customerID] = c
	}
	for i := 0; i < pairs && i < len(bookedIDs); i++ {
		appt := bookedIDs[i]
		victim := owner[st.byCustomer[appt]]
		var probe stressCustomer
		for _, c := range customers {
			if c.customerID != victim.customerID {
				probe = c
				break
			}
		}
		st.isolationChecks++
		if r, _ := sendUntilDefinite(ctx, probe.api, http.MethodGet, "/api/v1/appointments/"+appt.String(), nil, nil, probeAttempts); r.code != http.StatusNotFound {
			st.leaks = append(st.leaks, fmt.Sprintf("%s read %s's appointment: %d", probe.email, victim.email, r.code))
		}
		start := day.Start(w.loc).Add(9 * time.Hour)
		if r, _ := sendUntilDefinite(ctx, probe.api, http.MethodPost, "/api/v1/appointments", bookingPayload(w.dealer.ID, victim.vehicle, mix.ids, start),
			map[string]string{"Idempotency-Key": fmt.Sprintf("%s:leak:%d", runID, i)}, probeAttempts); r.code != http.StatusNotFound {
			st.leaks = append(st.leaks, fmt.Sprintf("%s booked %s's car: %d", probe.email, victim.email, r.code))
		}
		if vs, err := getAll[domain.Vehicle](ctx, probe.api, "/api/v1/vehicles", maxListLimit, nil); err != nil {
			st.leaks = append(st.leaks, fmt.Sprintf("%s vehicle list: %v", probe.email, err))
		} else {
			for _, v := range vs {
				if v.CustomerID != probe.customerID {
					st.foreignVehiclesShown++
				}
			}
		}
	}
	// Nothing offered must mean nothing fits (capacity only shrank since).
	seen := map[string]bool{}
	for _, mx := range st.turnedMixes {
		ok, done := seen[mx.name]
		if !done {
			oracle, err := oracleDay(ctx, conn, w, mx.req, false, day, mx.req.minutes)
			if err != nil {
				return err
			}
			ok = anyFeasible(oracle)
			seen[mx.name] = ok
		}
		if ok {
			st.falseRefusals++
		}
	}
	var err error
	if st.refusalVerdict, err = judgeRefusals(ctx, conn, w.dealer.ID, st.refusals.list(), nil, st.started); err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `
		SELECT a.id, a.customer_id, u.customer_id
		FROM appointments a LEFT JOIN users u ON u.id = a.created_by
		WHERE a.id = ANY($1)`, bookedIDs)
	if err != nil {
		return err
	}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id, customer uuid.UUID
		var creator *uuid.UUID
		if err := row.Scan(&id, &customer, &creator); err != nil {
			return struct{}{}, err
		}
		if want := st.byCustomer[id]; customer != want || creator == nil || *creator != want {
			st.ownershipMismatches = append(st.ownershipMismatches, fmt.Sprintf("%s: customer %s creator's customer %v, booked by %s", id, customer, creator, want))
		}
		return struct{}{}, nil
	})
	return err
}

func (st *customersStats) record(m *matrix, day booking.Date) {
	src := "customers"
	m.check("Q7", src, st.booked > 0 && st.errored == 0 && len(st.unexpected) == 0,
		"%d signed-in customers released together, each booking their own car for %s: %d booked (%d after losing a start), %d offered nothing, %d gave up after losing %d starts in a row, %d unresolved, %d errors%s",
		st.customers, day, st.booked, st.retried, st.turnedAway, st.gaveUp, fillTries, st.unresolved, st.errored, sampleSuffix(st.unexpected))
	if st.turnedAway == 0 {
		m.skip("R2b", src, "no customer was offered nothing, so there is no \"turned away\" verdict to judge (the fill day's top-up proves the day-full case)")
	} else {
		m.check("R2b", src, st.falseRefusals == 0, "%d customers offered nothing; the oracle finds a fitting start for %d of them", st.turnedAway, st.falseRefusals)
	}
	st.refusalVerdict.record(m, src)
	m.check("Q7", src, len(st.ownershipMismatches) == 0, "every booking belongs to the customer who made it (appointment customer and creator): %d mismatches%s",
		len(st.ownershipMismatches), sampleSuffix(st.ownershipMismatches))
	m.check("Q7", src, len(st.leaks) == 0 && st.foreignVehiclesShown == 0 && st.isolationChecks > 0,
		"%d cross-customer probes (read another's appointment, book another's car → 404; own vehicle list only): %d leaks, %d foreign cars listed%s",
		st.isolationChecks, len(st.leaks), st.foreignVehiclesShown, sampleSuffix(st.leaks))
	m.check("R1", src, st.booked > 0, "%d customers booked through the self-service API (customer policy: lead time, horizon)", st.booked)
	m.metric("customers", map[string]int{"customers": st.customers, "booked": st.booked, "turnedAway": st.turnedAway, "gaveUp": st.gaveUp,
		"unresolved": st.unresolved, "errors": st.errored, "leaks": len(st.leaks), "falseRefusals": st.falseRefusals + st.refusalVerdict.falseRefusals})
}
