package integration_test

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// listVehicles GETs /vehicles with the given query parameters and expects status.
func listVehicles(c apiClient, params url.Values, status int) []domain.Vehicle {
	c.t.Helper()
	var out []domain.Vehicle
	path := "/api/v1/vehicles"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	if status == http.StatusOK {
		c.expect(c.do(http.MethodGet, path, nil, nil), status, &out)
	} else {
		c.expect(c.do(http.MethodGet, path, nil, nil), status, nil)
	}
	return out
}

func vehicleIDs(items []domain.Vehicle) []uuid.UUID {
	ids := make([]uuid.UUID, len(items))
	for i, v := range items {
		ids[i] = v.ID
	}
	return ids
}

// addSearchFixtures gives Sofia (CustomerC) cars whose models hold LIKE wildcards, and a bulk owner
// 60 cars so the default page is full. Returns the wildcard cars by model.
func addSearchFixtures(t *testing.T, pool *pgxpool.Pool) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	cars := map[string]uuid.UUID{}
	for i, model := range []string{"Ka_X", "KaYX", "100% EV", "1000 EV"} {
		id := uuid.New()
		cars[model] = id
		if _, err := pool.Exec(ctx, `INSERT INTO vehicles (id, customer_id, vin, make, model, year) VALUES ($1, $2, $3, 'Lit', $4, 2024)`,
			id, seed.CustomerC, "LXT0000000000000"+string(rune('1'+i)), model); err != nil {
			t.Fatal(err)
		}
	}
	bulkOwner := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO customers (id, name, phone, email) VALUES ($1, 'Bulk Owner', '0', 'bulk@example.com')`, bulkOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO vehicles (id, customer_id, vin, make, model, year)
		SELECT gen_random_uuid(), $1, 'BULK' || lpad(g::text, 13, '0'), 'Fleet', 'Van ' || g, 2024
		FROM generate_series(1, 60) g`, bulkOwner); err != nil {
		t.Fatal(err)
	}
	return cars
}

func TestHTTPVehicleSearchAndLimit(t *testing.T) {
	t.Parallel()
	pool, st, svc, _ := openTestDB(t)
	cars := addSearchFixtures(t, pool)
	handler := newAPI(st, svc)
	staff := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)

	for _, tc := range []struct {
		name, q string
		want    []uuid.UUID
	}{
		{"VIN fragment, any case", "wau0000", []uuid.UUID{seed.VehicleB}},
		{"make", "TOYOTA", []uuid.UUID{seed.VehicleC}},
		{"model", "rav", []uuid.UUID{seed.VehicleC}},
		{"customer name", "patel", []uuid.UUID{seed.VehicleB}},
		{"trimmed", "  bmw  ", []uuid.UUID{seed.VehicleA}},
		{"underscore is literal", "a_x", []uuid.UUID{cars["Ka_X"]}},
		{"percent is literal", "100%", []uuid.UUID{cars["100% EV"]}},
		{"backslash is literal", `\`, []uuid.UUID{}},
		{"no match", "nothing-like-this", []uuid.UUID{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vehicleIDs(listVehicles(staff, url.Values{"q": {tc.q}}, http.StatusOK))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("q=%q: got %v want %v", tc.q, got, tc.want)
			}
		})
	}

	// Default page is 50 of the 67 vehicles; limit caps it; pages follow owner, make, model, id.
	all := listVehicles(staff, url.Values{"limit": {"200"}}, http.StatusOK)
	if len(all) != 67 {
		t.Fatalf("limit=200 returned %d vehicles, want all 67", len(all))
	}
	// Compare with the database's own collation rather than Go's byte order.
	rows, err := pool.Query(context.Background(), `
		SELECT v.id FROM vehicles v JOIN customers c ON c.id = v.customer_id
		ORDER BY c.name, v.make, v.model, v.id`)
	if err != nil {
		t.Fatal(err)
	}
	ordered, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(vehicleIDs(all), ordered) {
		t.Fatal("vehicles are not ordered by customer name, make, model, id")
	}
	if page := listVehicles(staff, nil, http.StatusOK); len(page) != booking.DefaultVehicleLimit {
		t.Fatalf("default page = %d, want %d", len(page), booking.DefaultVehicleLimit)
	}
	first := listVehicles(staff, url.Values{"limit": {"5"}}, http.StatusOK)
	if !slices.Equal(vehicleIDs(first), vehicleIDs(all[:5])) {
		t.Fatalf("limit=5 is not the first 5 of the ordered list")
	}
	if got := listVehicles(staff, url.Values{"q": {"bulk"}, "limit": {"7"}}, http.StatusOK); len(got) != 7 {
		t.Fatalf("q=bulk limit=7 returned %d", len(got))
	}

	for _, params := range []url.Values{
		{"limit": {"0"}}, {"limit": {"201"}}, {"limit": {"-1"}}, {"limit": {"ten"}},
		{"q": {strings.Repeat("x", booking.MaxVehicleSearch+1)}},
		{"q": {"a\x00b"}},
	} {
		listVehicles(staff, params, http.StatusBadRequest)
	}
	// The cap counts characters, not bytes; surrounding whitespace is trimmed first.
	listVehicles(staff, url.Values{"q": {" " + strings.Repeat("é", booking.MaxVehicleSearch) + " "}}, http.StatusOK)
}

func TestHTTPCustomerVehicleScope(t *testing.T) {
	t.Parallel()
	pool, st, svc, ctx := openTestDB(t)
	addSearchFixtures(t, pool)
	handler := newAPI(st, svc)
	customer := login(t, handler, seed.DemoCustomerEmail, seed.DemoCustomerPassword) // Amelia, CustomerA
	staff := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)

	for _, tc := range []struct {
		q    string
		want []uuid.UUID
	}{
		{"", []uuid.UUID{seed.VehicleA}},
		{"bmw", []uuid.UUID{seed.VehicleA}},
		{"amelia", []uuid.UUID{seed.VehicleA}},
		{"patel", []uuid.UUID{}}, // another customer's name
		{"WAU", []uuid.UUID{}},   // another customer's VIN
		{"bulk", []uuid.UUID{}},
	} {
		got := vehicleIDs(listVehicles(customer, url.Values{"q": {tc.q}}, http.StatusOK))
		if !slices.Equal(got, tc.want) {
			t.Fatalf("customer q=%q: got %v want %v", tc.q, got, tc.want)
		}
	}
	listVehicles(customer, url.Values{"limit": {"500"}}, http.StatusBadRequest)

	var own domain.Vehicle
	customer.expect(customer.do(http.MethodGet, "/api/v1/vehicles/"+seed.VehicleA.String(), nil, nil), http.StatusOK, &own)
	if own.ID != seed.VehicleA || own.CustomerID != seed.CustomerA {
		t.Fatalf("own vehicle = %+v", own)
	}
	// Someone else's vehicle and a missing one look the same to a customer.
	customer.expect(customer.do(http.MethodGet, "/api/v1/vehicles/"+seed.VehicleB.String(), nil, nil), http.StatusNotFound, nil)
	customer.expect(customer.do(http.MethodGet, "/api/v1/vehicles/"+uuid.NewString(), nil, nil), http.StatusNotFound, nil)
	customer.expect(customer.do(http.MethodGet, "/api/v1/vehicles/not-a-uuid", nil, nil), http.StatusBadRequest, nil)

	// Dealership-scoped staff still see any vehicle (vehicles belong to the group, not a dealership).
	var other domain.Vehicle
	staff.expect(staff.do(http.MethodGet, "/api/v1/vehicles/"+seed.VehicleB.String(), nil, nil), http.StatusOK, &other)
	if other.CustomerName != "Noah Patel" {
		t.Fatalf("staff vehicle = %+v", other)
	}
	anon := apiClient{t: t, handler: handler}
	anon.expect(anon.do(http.MethodGet, "/api/v1/vehicles/"+seed.VehicleA.String(), nil, nil), http.StatusUnauthorized, nil)

	// Service callers get the same bounds as HTTP.
	_, _, err := svc.ListVehicles(ctx, staffAdmin, booking.VehicleQuery{Limit: booking.MaxVehicleLimit + 1})
	expectCode(t, err, domain.CodeInvalid)
	_, err = svc.GetVehicle(ctx, customerAmelia, seed.VehicleC)
	expectCode(t, err, domain.CodeNotFound)
}

// The search indexes from 019 exist (EXPLAIN on a seeded-size table would prefer a seq scan).
func TestVehicleSearchIndexes(t *testing.T) {
	t.Parallel()
	pool, _, _, _ := openTestDB(t)
	for _, name := range []string{
		"vehicles_vin_trgm_idx", "vehicles_make_trgm_idx", "vehicles_model_trgm_idx",
		"customers_name_trgm_idx", "customers_name_idx",
	} {
		if !scalar[bool](t, pool, `SELECT EXISTS (SELECT 1 FROM pg_indexes WHERE indexname = $1)`, name) {
			t.Fatalf("index %s missing", name)
		}
	}
}
