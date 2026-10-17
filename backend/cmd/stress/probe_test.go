package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Run with -race: the workers creating vehicles must not race the loop that dispatches them.
func TestEnsureVehiclesCreatesOnlyMissing(t *testing.T) {
	owner := uuid.New()
	listed := []domain.Vehicle{
		{ID: uuid.New(), VIN: vinPhases + "000000000000", CustomerID: owner},
		{ID: uuid.New(), VIN: vinPhases + "000000000003", CustomerID: owner},
	}
	var (
		mu      sync.Mutex
		created = map[string]uuid.UUID{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			if r.URL.Query().Get("limit") == "200" { // the prefix search: two earlier cars
				_ = json.NewEncoder(w).Encode(listed)
				return
			}
			_, _ = w.Write([]byte("[]")) // no earlier run's vehicle beyond the listed ones
			return
		}
		var body struct {
			VIN        string    `json:"vin"`
			CustomerID uuid.UUID `json:"customerId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CustomerID != owner {
			http.Error(w, "bad body or owner", http.StatusBadRequest)
			return
		}
		mu.Lock()
		id := uuid.New()
		created[body.VIN] = id
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(domain.Vehicle{ID: id, VIN: body.VIN})
	}))
	defer srv.Close()

	ids, err := ensureVehicles(t.Context(), newAPI(srv.URL), owner, vinPhases, 40, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 40 || ids[0] != listed[0].ID || ids[3] != listed[1].ID {
		t.Fatalf("got %d ids; listed vehicles must be reused in place", len(ids))
	}
	if len(created) != 38 {
		t.Fatalf("created %d vehicles, want the 38 missing ones", len(created))
	}
	for i, id := range ids {
		if id == uuid.Nil {
			t.Fatalf("vehicle %d has no id", i)
		}
	}
}
