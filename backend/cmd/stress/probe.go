package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

type probe struct {
	path  string
	check func([]byte) bool
}

// sideProbes are the read/admin calls a dispatcher's screen keeps making during the hammer; they
// must stay 2xx (no 5xx, no transport errors) while bookings saturate the write path.
func sideProbes(p *plan, vehicle uuid.UUID) []probe {
	nonEmpty := func(b []byte) bool { return json.Valid(b) && len(b) > 2 }
	return []probe{
		// Ready: "ok", or "degraded" while Redis is down (every Redis use fails open, the replica
		// still serves — what the Redis-down chaos run checks).
		{"/api/v1/health", func(b []byte) bool {
			return bytes.Contains(b, []byte(`"status":"ok"`)) || bytes.Contains(b, []byte(`"status":"degraded"`))
		}},
		{"/api/v1/dealerships", nonEmpty},
		{"/api/v1/service-types", nonEmpty},
		{"/api/v1/vehicles", nonEmpty},
		{fmt.Sprintf("/api/v1/availability?dealershipId=%s&vehicleId=%s&%s&date=%s",
			p.dealer.ID, vehicle, jobQuery(p.jobs[0]), p.date), func(b []byte) bool {
			var a domain.Availability
			return json.Unmarshal(b, &a) == nil && a.Date == p.date && (len(a.Slots) > 0 || a.Closed)
		}},
		{p.schedulePath(), func(b []byte) bool {
			var s domain.Schedule
			return json.Unmarshal(b, &s) == nil && s.Date != ""
		}},
		{"/api/v1/auth/me", func(b []byte) bool { return bytes.Contains(b, []byte(`"email"`)) }},
		{"/api/v1/admin/technicians?dealershipId=" + p.dealer.ID.String(), json.Valid},
	}
}

func probeSide(ctx context.Context, api *apiClient, probes []probe, first int, stats *sideStats, done <-chan struct{}) {
	for i := first; ; i++ {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		default:
		}
		p := probes[i%len(probes)]
		t0 := time.Now()
		resp, err := api.do(ctx, http.MethodGet, p.path, nil, nil)
		stats.observe(time.Since(t0))
		switch {
		case err != nil:
			stats.transport.Add(1)
		case resp.code >= 500:
			stats.fiveXX.Add(1)
		case resp.code < 200 || resp.code >= 300 || !p.check(resp.body):
			stats.bad.Add(1)
		default:
			stats.ok.Add(1)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// VIN prefixes of the stress fleets (5 characters + 12 digits = 17, no I/O/Q): the hammer phases,
// the truth/fill day, the quality day, the bench, and the soak each use their own cars, so no phase's bookings can make a
// vehicle busy for another.
const (
	vinPhases = "STRSP"
	vinDay    = "STRSF"
	vinBench  = "STRSB"
	vinSoak   = "STRSK"
	// vinQuality: the quality day's cars (qualityday.go).
	vinQuality = "STRSA"
)

// ensureVehicles returns want vehicles of owner with deterministic VINs prefix+NNN…, creating the
// missing ones (so repeated runs reuse them). One search (every page) lists the existing ones; the
// rest are looked up (or created) one by one.
func ensureVehicles(ctx context.Context, api *apiClient, owner uuid.UUID, prefix string, want, workers int) ([]uuid.UUID, error) {
	listed, err := getAll[domain.Vehicle](ctx, api, "/api/v1/vehicles?q="+url.QueryEscape(prefix), maxListLimit, nil)
	if err != nil {
		return nil, err
	}
	byVIN := make(map[string]uuid.UUID, len(listed))
	for _, v := range listed {
		byVIN[v.VIN] = v.ID
	}
	vins := make([]string, want)
	var missing []string // decided before any worker writes byVIN
	for i := range vins {
		vins[i] = fmt.Sprintf("%s%012d", prefix, i)
		if _, ok := byVIN[vins[i]]; !ok {
			missing = append(missing, vins[i])
		}
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, max(1, min(workers, 64)))
	for _, vin := range missing {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			v, err := findOrCreateVehicle(ctx, api, owner, vin)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("create stress vehicle %s: %w", vin, err)
				}
				return
			}
			byVIN[vin] = v.ID
		})
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	out := make([]uuid.UUID, want)
	for i, vin := range vins {
		out[i] = byVIN[vin]
	}
	return out, nil
}

// findOrCreateVehicle looks the VIN up first: GET /vehicles is a bounded page, so the listed
// vehicles cannot tell whether an earlier run already created it.
func findOrCreateVehicle(ctx context.Context, api *apiClient, owner uuid.UUID, vin string) (domain.Vehicle, error) {
	found, err := getJSON[[]domain.Vehicle](ctx, api, "GET", "/api/v1/vehicles?limit=1&q="+url.QueryEscape(vin), nil)
	if err != nil {
		return domain.Vehicle{}, err
	}
	if len(found) == 1 && found[0].VIN == vin {
		return found[0], nil
	}
	return getJSON[domain.Vehicle](ctx, api, "POST", "/api/v1/vehicles", map[string]any{
		"customerId": owner, "vin": vin, "make": "Stress", "model": "Car", "year": 2024,
	})
}
