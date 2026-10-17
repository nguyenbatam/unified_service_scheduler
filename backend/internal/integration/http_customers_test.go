package integration_test

import (
	"net/http"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A dispatcher records a walk-in (no account), finds them again by a phone or email fragment, and
// adds their car; customers can do none of it.
func TestHTTPStaffWalkInCustomer(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	h := newAPI(st, svc)
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)

	for _, bad := range []map[string]any{
		{"name": "  ", "phone": "0400 111 222"},
		{"name": "Walk In"},                                   // no way to reach them
		{"name": "Walk In", "email": "not-an-address"},        // email shape
		{"name": "Walk In", "phone": "12ab34"},                // phone characters
		{"name": "Walk In", "phone": "0400", "extra": "nope"}, // unknown field
	} {
		dispatcher.expect(dispatcher.do(http.MethodPost, "/api/v1/customers", bad, nil), http.StatusBadRequest, nil)
	}

	var created domain.Customer
	dispatcher.expect(dispatcher.do(http.MethodPost, "/api/v1/customers",
		map[string]any{"name": " Walk-in Wanda ", "phone": "+61 400 123 987", "email": " Wanda@Example.COM "}, nil),
		http.StatusCreated, &created)
	if created.Name != "Walk-in Wanda" || created.Email != "wanda@example.com" || created.Phone != "+61 400 123 987" {
		t.Fatalf("created = %+v", created)
	}

	for _, q := range []string{"123%20987", "wanda@example", "WALK-IN"} {
		var found []domain.Customer
		dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/customers?q="+q, nil, nil), http.StatusOK, &found)
		if len(found) != 1 || found[0].ID != created.ID {
			t.Fatalf("q=%s: found %+v", q, found)
		}
	}
	var page []domain.Customer
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/customers?limit=1", nil, nil), http.StatusOK, &page)
	if len(page) != 1 {
		t.Fatalf("limit=1 returned %d", len(page))
	}
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/customers?limit=0", nil, nil), http.StatusBadRequest, nil)

	var car domain.Vehicle
	dispatcher.expect(dispatcher.do(http.MethodPost, "/api/v1/vehicles",
		map[string]any{"customerId": created.ID, "vin": "1HGCM82633A004352", "make": "Honda", "model": "Accord", "year": 2020}, nil),
		http.StatusCreated, &car)
	if car.CustomerID != created.ID || car.CustomerName != created.Name {
		t.Fatalf("car = %+v", car)
	}

	customer.expect(customer.do(http.MethodPost, "/api/v1/customers", map[string]any{"name": "X", "phone": "0400111222"}, nil), http.StatusForbidden, nil)
	customer.expect(customer.do(http.MethodGet, "/api/v1/customers", nil, nil), http.StatusForbidden, nil)
}

// The customer search reads group-wide PII, so unlike most desk work it is throttled for staff
// too: one staff token cannot page through every customer unthrottled.
func TestHTTPCustomerRoutesThrottleStaff(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	limiter := newTestLimiter()
	h := newAPI(st, svc, httpapi.WithRateLimiter(limiter))
	dispatcher := login(t, h, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/customers?q=a", nil, nil), http.StatusOK, nil)

	limiter.denyPrefix("customer-search:user:")
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/customers?q=a", nil, nil), http.StatusTooManyRequests, nil)
	limiter.denyPrefix("customer-create:user:")
	dispatcher.expect(dispatcher.do(http.MethodPost, "/api/v1/customers", map[string]any{"name": "Walk In", "phone": "0400111222"}, nil),
		http.StatusTooManyRequests, nil)
}
