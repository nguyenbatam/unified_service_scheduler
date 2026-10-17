package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Extra scenarios, run after the phases' own checks (so they never disturb a hammered slot's
// board) and before the SQL pass (so it covers their rows too: their keys share the run prefix).

// freeStart is the first start at or after cutoff that job is free for (vehicle included), rolling
// over to later dates like preparePhase does.
func freeStart(ctx context.Context, api *apiClient, dealer, vehicle uuid.UUID, job []uuid.UUID, cutoff time.Time) (time.Time, error) {
	from := ""
	for range maxDateRolls {
		date, err := nextFreeDate(ctx, api, dealer, vehicle, job, from)
		if err != nil {
			return time.Time{}, err
		}
		avail, err := availability(ctx, api, dealer, vehicle, job, date)
		if err != nil {
			return time.Time{}, err
		}
		for _, s := range avail.Slots {
			if s.Available && !s.Start.Before(cutoff) {
				return s.Start, nil
			}
		}
		if from, err = nextDate(date); err != nil {
			return time.Time{}, err
		}
	}
	return time.Time{}, fmt.Errorf("no free start after %s in %d dates", cutoff.Format(time.RFC3339), maxDateRolls)
}

func bookingPayload(dealer, vehicle uuid.UUID, job []uuid.UUID, start time.Time) map[string]any {
	payload := map[string]any{"dealershipId": dealer, "vehicleId": vehicle, "startAt": start}
	if len(job) == 1 {
		payload["serviceTypeId"] = job[0]
	} else {
		payload["serviceTypeIds"] = job
	}
	return payload
}

// customerTally counts a customer burst's answers by status. 201 must stay within what the
// customer's vehicles allow; 409/422 (slot taken, lead time, horizon) and 429 (the customer write
// throttle) are the limits doing their job; 503 busy is indefinite; anything else is a bug.
type customerTally struct {
	byCode map[int]int
	ids    []uuid.UUID
	bad    []string
}

func (t *customerTally) add(code int, id uuid.UUID, body string) {
	t.byCode[code]++
	switch code {
	case http.StatusCreated, http.StatusOK:
		if id != uuid.Nil {
			t.ids = append(t.ids, id)
		}
	case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, 0:
	default:
		if len(t.bad) < 5 {
			t.bad = append(t.bad, fmt.Sprintf("%d: %s", code, body))
		}
	}
}

// judge fails a burst with unexpected answers or more bookings than vehicles (all requests target
// one start, so each vehicle fits once).
// A burst with no booking at all (every request throttled, busy, or refused) proves nothing about
// the limits, so it fails too.
func (t *customerTally) judge(vehicles int) error {
	switch {
	case len(t.bad) > 0:
		return fmt.Errorf("unexpected answers %v", t.bad)
	case len(t.ids) > vehicles:
		return fmt.Errorf("%d bookings at one start for %d vehicle(s)", len(t.ids), vehicles)
	case len(t.ids) == 0:
		return fmt.Errorf("no booking succeeded (%s): the burst proved nothing", t)
	}
	return nil
}

// judgeLead classifies the answer to the booking inside the customer lead time: 422 is the check
// working, 201/200 is a booking it should have refused, any other definite answer is a bug, and
// an answer that stayed indefinite (throttled, busy) means the check did not run.
func judgeLead(code int, definite bool, body string) (ran bool, err error) {
	switch {
	case !definite:
		return false, nil
	case code == http.StatusUnprocessableEntity:
		return true, nil
	case code == http.StatusCreated || code == http.StatusOK:
		return true, fmt.Errorf("a booking inside the customer lead time was accepted (%d)", code)
	default:
		return true, fmt.Errorf("the booking inside the customer lead time answered %d, want 422: %s", code, body)
	}
}

// leadProbe books the next half hour as the customer, inside the dealership's lead time, and
// requires 422. It runs before the burst, which spends the customer's write budget (30/min) and
// would leave it 429; indefinite answers are resent with the same key a few times, and if none
// settles it prints that the check did not run rather than passing it. It returns the appointment
// a wrongly accepted probe made (for cleanup), whether it may have committed unheard, and what it
// verified for the report.
func leadProbe(ctx context.Context, cust *apiClient, p *plan, vehicle uuid.UUID, job []uuid.UUID, runID string) (id uuid.UUID, unresolved bool, summary string, err error) {
	lead := time.Duration(p.dealer.Policy.CustomerLeadMinutes) * time.Minute
	if lead <= 30*time.Minute {
		fmt.Printf("note: customer lead-time check skipped (lead time %s leaves no half-hour start inside it)\n", lead)
		return uuid.Nil, false, "lead-time check skipped", nil
	}
	soon := time.Now().Truncate(30 * time.Minute).Add(30 * time.Minute)
	resp, definite := sendUntilDefinite(ctx, cust, http.MethodPost, "/api/v1/appointments",
		bookingPayload(p.dealer.ID, vehicle, job, soon), map[string]string{"Idempotency-Key": runID + ":cust:lead"}, probeAttempts)
	if resp.code == http.StatusCreated || resp.code == http.StatusOK {
		var appt domain.Appointment
		_ = json.Unmarshal(resp.body, &appt)
		id = appt.ID
	}
	ran, err := judgeLead(resp.code, definite, truncate(string(resp.body), 120))
	switch {
	case err != nil:
		return id, false, "", fmt.Errorf("lead time (%s at %s): %w", lead, soon.Format(time.RFC3339), err)
	case !ran:
		last := strconv.Itoa(resp.code)
		if resp.code == 0 {
			last = "transport error"
		}
		fmt.Printf("note: customer lead-time check did NOT run: the booking at %s (inside the %s lead time) stayed indefinite (last answer %s) after %d attempts\n",
			soon.Format(time.RFC3339), lead, last, probeAttempts)
		// A throttled request never ran; any other indefinite answer may have committed.
		return id, resp.code != http.StatusTooManyRequests, "lead-time check did not run", nil
	}
	return id, false, "lead-time 422", nil
}

func (t *customerTally) String() string {
	codes := make([]int, 0, len(t.byCode))
	for c := range t.byCode {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, len(codes))
	for i, c := range codes {
		label := strconv.Itoa(c)
		if c == 0 {
			label = "unresolved"
		}
		parts[i] = fmt.Sprintf("%s=%d", label, t.byCode[c])
	}
	return strings.Join(parts, " ")
}

// customerBurst logs in as a seeded customer, first books inside the dealership's lead time
// (leadProbe: must be 422), then sends requests concurrent bookings (distinct keys, no retries: a
// 429 is the expected answer once the per-user write budget is spent) for the customer's own
// vehicles at the first start the customer may book. It returns the ids booked and how many
// requests stayed indefinite. The burst was asked for (-customer-requests > 0): a customer that
// cannot log in, or has no car, fails it — a skipped burst would let the run pass without the
// customer limits ever being exercised (CUSTOMER_REQUESTS=0 is how to leave it out).
// onLost is called if the customer's session is ever lost for good (nil: ignored).
func customerBurst(ctx context.Context, s settings, p *plan, runID string, onLost func(error)) ([]uuid.UUID, int, error) {
	cust := newAPI(s.base)
	if err := cust.login(ctx, s.customerEmail, s.customerPassword, onLost); err != nil {
		return nil, 0, fmt.Errorf("customer burst: login %s: %w (set STRESS_CUSTOMER_EMAIL/PASSWORD, or CUSTOMER_REQUESTS=0 to leave the burst out)", s.customerEmail, err)
	}
	vehicles, err := getAll[domain.Vehicle](ctx, cust, "/api/v1/vehicles", maxListLimit, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("customer burst: %w", err)
	}
	if len(vehicles) == 0 {
		return nil, 0, fmt.Errorf("customer burst: %s has no vehicle to book", s.customerEmail)
	}
	job := p.jobs[0]
	// The customer's own availability already applies the lead time and horizon.
	start, err := freeStart(ctx, cust, p.dealer.ID, vehicles[0].ID, job, time.Now().Add(s.startMargin))
	if err != nil {
		return nil, 0, fmt.Errorf("customer burst: %w", err)
	}
	leadID, leadUnresolved, leadSummary, err := leadProbe(ctx, cust, p, vehicles[0].ID, job, runID)
	var leadIDs []uuid.UUID
	if leadID != uuid.Nil {
		leadIDs = append(leadIDs, leadID)
	}
	if err != nil {
		return leadIDs, 0, fmt.Errorf("customer burst: %w", err)
	}
	tally, wait := burst(ctx, cust, s.customerRequests, runID+":cust", vehicles, p.dealer.ID, job, start)
	if len(tally.ids) == 0 && tally.byCode[http.StatusTooManyRequests] == s.customerRequests {
		// Every request throttled: the customer's per-minute write budget was spent before the
		// burst (an earlier run moments ago). Wait it out once and burst again with fresh keys.
		wait = min(max(wait, time.Second), 65*time.Second)
		fmt.Printf("note: customer write budget spent before the burst; waiting %s for it to refill\n", wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		tally, _ = burst(ctx, cust, s.customerRequests, runID+":cust2", vehicles, p.dealer.ID, job, start)
	}
	ids := append(leadIDs, tally.ids...)
	unresolved := tally.byCode[0] + tally.byCode[http.StatusBadGateway] + tally.byCode[http.StatusServiceUnavailable] + tally.byCode[http.StatusGatewayTimeout]
	if leadUnresolved {
		unresolved++
	}
	if err := tally.judge(len(vehicles)); err != nil {
		return ids, unresolved, fmt.Errorf("customer burst (%d requests at %s): %w (%s)", s.customerRequests, start.Format(time.RFC3339), err, tally)
	}
	fmt.Printf("customer ok: %s; %d requests as %s for %d vehicle(s) at %s → %s\n",
		leadSummary, s.customerRequests, s.customerEmail, len(vehicles), start.Format(time.RFC3339), tally)
	return ids, unresolved, nil
}

// burst sends n concurrent bookings as the customer (released together, distinct keys prefix:i, no
// retries) for its vehicles in turn at start, and returns the tally and the longest Retry-After.
func burst(ctx context.Context, cust *apiClient, n int, prefix string, vehicles []domain.Vehicle, dealer uuid.UUID, job []uuid.UUID, start time.Time) (*customerTally, time.Duration) {
	tally := &customerTally{byCode: map[int]int{}}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		wait time.Duration
	)
	gate := make(chan struct{})
	for i := range n {
		v := vehicles[i%len(vehicles)].ID
		wg.Go(func() {
			<-gate
			resp, err := cust.do(ctx, http.MethodPost, "/api/v1/appointments", bookingPayload(dealer, v, job, start),
				map[string]string{"Idempotency-Key": fmt.Sprintf("%s:%d", prefix, i)})
			code := resp.code
			if err != nil {
				code = 0
			}
			var appt domain.Appointment
			if code == http.StatusCreated || code == http.StatusOK {
				_ = json.Unmarshal(resp.body, &appt)
			}
			mu.Lock()
			defer mu.Unlock()
			tally.add(code, appt.ID, truncate(string(resp.body), 120))
			wait = max(wait, retryAfter(resp.header))
		})
	}
	close(gate)
	wg.Wait()
	return tally, wait
}
