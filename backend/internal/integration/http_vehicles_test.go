package integration_test

import (
	"net/http"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// A customer cannot learn whether a VIN is registered (to anyone) from the duplicate error; staff
// still get the precise reason. Registration is a throttled write.
func TestHTTPVehicleDuplicateVINIsOpaqueToCustomers(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	limiter := newTestLimiter()
	h := newAPI(st, svc, httpapi.WithRateLimiter(limiter))
	customer := login(t, h, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	admin := login(t, h, seed.DemoAdminEmail, seed.DemoAdminPassword)

	var errBody struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// WAU00000000000002 is another customer's car; WBA00000000000001 is the caller's own.
	for _, vin := range []string{"WAU00000000000002", "WBA00000000000001"} {
		body := map[string]any{"vin": vin, "make": "Audi", "model": "A4", "year": 2021}
		customer.expect(customer.do(http.MethodPost, "/api/v1/vehicles", body, nil), http.StatusConflict, &errBody)
		if errBody.Error.Message != booking.VehicleNotRegistrableMessage {
			t.Fatalf("customer, %s: message %q", vin, errBody.Error.Message)
		}
	}

	body := map[string]any{"customerId": seed.CustomerA, "vin": "WAU00000000000002", "make": "Audi", "model": "A4", "year": 2021}
	admin.expect(admin.do(http.MethodPost, "/api/v1/vehicles", body, nil), http.StatusConflict, &errBody)
	if errBody.Error.Message == booking.VehicleNotRegistrableMessage {
		t.Fatal("staff got the opaque customer message")
	}

	limiter.denyPrefix("write:user:")
	fresh := map[string]any{"vin": "1HGCM82633A004352", "make": "Honda", "model": "Accord", "year": 2020}
	customer.expect(customer.do(http.MethodPost, "/api/v1/vehicles", fresh, nil), http.StatusTooManyRequests, nil)
}
