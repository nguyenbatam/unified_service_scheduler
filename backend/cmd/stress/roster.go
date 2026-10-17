package main

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Technician rosters of the stress workshops, provisioned through the admin roster API
// (PUT /admin/technicians/{id}/shifts, POST …/absences) like any dealership would set them: the
// weekly patterns once per shop (re-applied when an earlier run or a person changed them), and the
// dated absences on every phase day at the same local times, so each day phase sees the same
// roster and the oracle's duty rule (rules.go) is judged on shifts, breaks, and absences alike.

// techByName finds a provisioned technician of w.
func (w workshop) techByName(name string) (domain.TechnicianAdmin, error) {
	i := slices.IndexFunc(w.techs, func(t domain.TechnicianAdmin) bool { return t.Name == name })
	if i < 0 {
		return domain.TechnicianAdmin{}, fmt.Errorf("%s has no active technician %q", w.dealer.Name, name)
	}
	return w.techs[i], nil
}

// shiftKey renders a pattern canonically (weekday order, absent break as "-") for comparison.
func shiftKey(shifts []domain.TechnicianShift) string {
	parts := make([]string, len(shifts))
	for i, s := range shifts {
		bs, be := s.BreakStart, s.BreakEnd
		if bs == "" {
			bs, be = "-", "-"
		}
		parts[i] = fmt.Sprintf("%d %s-%s %s-%s", s.Weekday, s.StartTime, s.EndTime, bs, be)
	}
	slices.Sort(parts)
	return strings.Join(parts, ";")
}

// ensureShifts gives each technician of w.def.shifts its weekly pattern when it has another one.
// A pattern change that would strand an upcoming booking is refused (409); cancelUpcoming then
// clears the shop's upcoming bookings (left by an interrupted run) and the change is retried once.
func ensureShifts(ctx context.Context, api *apiClient, w workshop, cancelUpcoming func(context.Context) error) error {
	names := make([]string, 0, len(w.def.shifts))
	for name := range w.def.shifts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		want := w.def.shifts[name]
		tech, err := w.techByName(name)
		if err != nil {
			return err
		}
		path := "/api/v1/admin/technicians/" + tech.ID.String()
		roster, err := getJSON[domain.TechnicianRoster](ctx, api, http.MethodGet, path+"/roster", nil)
		if err != nil {
			return fmt.Errorf("roster of %s: %w", name, err)
		}
		if shiftKey(roster.Shifts) == shiftKey(want) {
			continue
		}
		resp, err := api.do(ctx, http.MethodPut, path+"/shifts", map[string]any{"shifts": want}, nil)
		if err == nil && resp.code == http.StatusConflict && cancelUpcoming != nil {
			if cerr := cancelUpcoming(ctx); cerr != nil {
				return fmt.Errorf("shifts of %s refused (409) and clearing the shop failed: %w", name, cerr)
			}
			resp, err = api.do(ctx, http.MethodPut, path+"/shifts", map[string]any{"shifts": want}, nil)
		}
		if err != nil {
			return fmt.Errorf("shifts of %s: %w", name, err)
		}
		if resp.code != http.StatusOK {
			return fmt.Errorf("shifts of %s: %d %s", name, resp.code, truncate(string(resp.body), 160))
		}
		fmt.Printf("%s: %s now works %s\n", w.dealer.Name, name, shiftKey(want))
	}
	return nil
}

// ensureDayRoster adds w.def.absences on day (local times) unless that very absence exists. It
// runs after the day was reset (an absence overlapping a booking is refused); an overlapping
// absence with other times (a person edited the shop) fails the phase instead of guessing.
func ensureDayRoster(ctx context.Context, api *apiClient, w workshop, day booking.Date) error {
	for _, a := range w.def.absences {
		tech, err := w.techByName(a.tech)
		if err != nil {
			return err
		}
		from, err := parseClockMinutes(a.from)
		if err != nil {
			return err
		}
		to, err := parseClockMinutes(a.to)
		if err != nil {
			return err
		}
		start, end := from.on(day, w.loc), to.on(day, w.loc)
		path := "/api/v1/admin/technicians/" + tech.ID.String()
		roster, err := getJSON[domain.TechnicianRoster](ctx, api, http.MethodGet, path+"/roster", nil)
		if err != nil {
			return fmt.Errorf("roster of %s: %w", a.tech, err)
		}
		if slices.ContainsFunc(roster.Absences, func(x domain.TechnicianAbsence) bool {
			return x.StartsAt.Equal(start) && x.EndsAt.Equal(end)
		}) {
			continue
		}
		resp, err := api.do(ctx, http.MethodPost, path+"/absences", map[string]any{
			"startsAt": start.Format(time.RFC3339), "endsAt": end.Format(time.RFC3339), "reason": "stress: training",
		}, nil)
		if err != nil {
			return fmt.Errorf("absence of %s on %s: %w", a.tech, day, err)
		}
		if resp.code != http.StatusCreated && resp.code != http.StatusOK {
			return fmt.Errorf("absence of %s on %s %s–%s: %d %s", a.tech, day, a.from, a.to, resp.code, truncate(string(resp.body), 160))
		}
	}
	return nil
}
