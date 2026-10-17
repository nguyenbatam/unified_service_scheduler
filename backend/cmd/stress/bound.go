package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// The hammer's per-slot bound comes from the oracle's model of the shop, not from the counts
// availability reports: those are what is free without a move, and the one-move repair
// (repair.go) can legitimately book a start beyond them. Every booking of a start holds, at that
// instant, a bay and a technician of its own that no other booking holds, so a start can take at
// most as many bookings as there are bays (technicians) fitting some request variant — job mix,
// waiting or not — over its window, minus those held there by an appointment that can never move
// (checked in or started, already started, or changed during the phase). A movable hold may be
// pushed elsewhere, so it does not lower the bound. In exact mode the slots are also clean (no
// hold of the shop overlaps them), where the bound is exactly what fills: every fitting bay pairs
// with every qualified technician.

// boundVariant is one kind of request a hammered slot receives.
type boundVariant struct {
	req  jobRequirement
	soft bool
}

// boundVariants are the request kinds of p: each job mix, waiting or not (the hammer sends both).
func (p *plan) boundVariants(expressMax int) []boundVariant {
	var out []boundVariant
	for _, mx := range p.mixes {
		for _, waiter := range []bool{false, true} {
			out = append(out, boundVariant{req: mx.req, soft: softUpgrade(mx.req, waiter, expressMax)})
		}
	}
	return out
}

// slotBound is the most bookings start can hold for variants, and whether its windows are clean
// (no hold of the shop overlaps the longest of them).
func (s *oracleShop) slotBound(variants []boundVariant, start time.Time) (bound int, clean bool) {
	window := func(v boundVariant) timeWindow {
		return timeWindow{start, start.Add(time.Duration(v.req.minutes) * time.Minute)}
	}
	stuck := func(owned func(oHold) bool, w timeWindow) bool {
		for _, h := range s.holds {
			if owned(h) && h.window.overlaps(w) && !s.movable(h) {
				return true
			}
		}
		return false
	}
	bays, techs := 0, 0
	for _, b := range s.bays {
		for _, v := range variants {
			if bayFits(v.req, v.soft, b.bayType) && !stuck(func(h oHold) bool { return h.bay == b.id }, window(v)) {
				bays++
				break
			}
		}
	}
	for _, t := range s.techs {
		for _, v := range variants {
			w := window(v)
			if qualified(v.req, t.level, t.skills) && t.roster.onDuty(w, s.loc) && !stuck(func(h oHold) bool { return h.tech == t.id }, w) {
				techs++
				break
			}
		}
	}
	longest := timeWindow{start, start}
	for _, v := range variants {
		if w := window(v); w.end.After(longest.end) {
			longest = w
		}
	}
	clean = true
	for _, h := range s.holds {
		if h.window.overlaps(longest) {
			clean = false
			break
		}
	}
	return min(bays, techs), clean
}

// boundShop is the oracle's model of p's dealership on its date: read from Postgres when q is set
// (rosters included), else from the board (loadBoard: bays, active technicians, the day's holds),
// where every technician counts as on duty — still an upper bound, and exact for the river shop,
// whose technicians have no shift pattern or absence.
func (p *plan) boundShop(ctx context.Context, q querier) (*oracleShop, error) {
	day, err := booking.ParseDate(p.date)
	if err != nil {
		return nil, err
	}
	if q != nil {
		from, to := dayBounds(day, p.loc)
		return loadShop(ctx, q, p.dealer.ID, from, to)
	}
	if p.board == nil {
		return nil, fmt.Errorf("phase %s: the board of %s is not loaded", p.name, p.date)
	}
	shop := &oracleShop{loc: p.loc, now: time.Now(), pinned: map[uuid.UUID]bool{}, expressMax: int(p.dealer.Policy.ExpressMaxMinutes)}
	for _, b := range p.board.Bays {
		shop.bays = append(shop.bays, oBay{id: b.ID, bayType: b.BayType})
	}
	for _, t := range p.board.Technicians {
		skills := map[domain.TechnicianSkill]bool{}
		for _, sk := range t.Skills {
			skills[sk] = true
		}
		shop.techs = append(shop.techs, oTech{id: t.ID, level: int16(t.SkillLevel), skills: skills})
	}
	for _, a := range p.board.Appointments {
		if a.Status.HoldsResources() {
			shop.holds = append(shop.holds, oHold{id: a.ID, bay: a.ServiceBayID, tech: a.TechnicianID, vehicle: a.VehicleID,
				status: int16(a.Status), window: timeWindow{a.StartAt, a.EndAt}})
		}
	}
	return shop, nil
}
