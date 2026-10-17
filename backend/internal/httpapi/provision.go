package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// maxNameLen bounds dealership, bay, and technician names.
const maxNameLen = 100

// validName trims name and reports whether it is non-blank and at most maxNameLen characters.
func validName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	return name, name != "" && len([]rune(name)) <= maxNameLen
}

func (s *Server) createDealership(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var body struct {
		Name                 string  `json:"name"`
		Timezone             string  `json:"timezone"`
		OpenTime             string  `json:"openTime"`
		CloseTime            string  `json:"closeTime"`
		LunchStart           string  `json:"lunchStart"`
		LunchEnd             string  `json:"lunchEnd"`
		StagingCapacity      int16   `json:"stagingCapacity"`
		StagingWindowMinutes *int16  `json:"stagingWindowMinutes"`
		Policy               *policy `json:"policy"`
	}
	if !s.bindJSON(c, &body, "request body must be dealership JSON") {
		return
	}
	name, ok := validName(body.Name)
	if !ok {
		s.fail(c, domain.NewError(domain.CodeInvalid, "name is required (at most 100 characters)"))
		return
	}
	if !validClock(body.OpenTime) || !validClock(body.CloseTime) {
		s.fail(c, domain.NewError(domain.CodeInvalid, "openTime and closeTime must be HH:MM"))
		return
	}
	if (body.LunchStart == "") != (body.LunchEnd == "") ||
		(body.LunchStart != "" && (!validClock(body.LunchStart) || !validClock(body.LunchEnd))) {
		s.fail(c, domain.NewError(domain.CodeInvalid, "lunchStart and lunchEnd must both be HH:MM, or both omitted"))
		return
	}
	d := store.NewDealership{
		Name: name, Timezone: strings.TrimSpace(body.Timezone),
		OpenTime: body.OpenTime, CloseTime: body.CloseTime, LunchStart: body.LunchStart, LunchEnd: body.LunchEnd,
		StagingCapacity: body.StagingCapacity, StagingWindowMinutes: 60, Policy: defaultPolicy(),
	}
	if body.StagingWindowMinutes != nil {
		d.StagingWindowMinutes = *body.StagingWindowMinutes
	}
	patch := store.DealershipPatch{StagingCapacity: &d.StagingCapacity, StagingWindowMinutes: &d.StagingWindowMinutes}
	if p := body.Policy; p != nil {
		p.apply(&d.Policy)
		patch.CustomerLeadMinutes, patch.CustomerChangeCutoffMinutes = p.CustomerLeadMinutes, p.CustomerChangeCutoffMinutes
		patch.BookingHorizonDays, patch.NoShowGraceMinutes, patch.ExpressMaxMinutes = p.BookingHorizonDays, p.NoShowGraceMinutes, p.ExpressMaxMinutes
	}
	if msg := validateDealershipPatch(patch); msg != "" {
		s.fail(c, domain.NewError(domain.CodeInvalid, msg))
		return
	}
	item, err := s.svc.CreateDealership(c.Request.Context(), actor, d)
	if err == nil {
		s.catalog.invalidate()
	}
	s.reply(c, http.StatusCreated, item, item.Version, err)
}

// policy is the optional policy object of a dealership body: nil fields keep the default.
type policy struct {
	CustomerLeadMinutes         *int16 `json:"customerLeadMinutes"`
	CustomerChangeCutoffMinutes *int16 `json:"customerChangeCutoffMinutes"`
	BookingHorizonDays          *int16 `json:"bookingHorizonDays"`
	NoShowGraceMinutes          *int16 `json:"noShowGraceMinutes"`
	ExpressMaxMinutes           *int16 `json:"expressMaxMinutes"`
}

func (p *policy) apply(dst *domain.Policy) {
	for _, f := range []struct {
		src *int16
		dst *int16
	}{
		{p.CustomerLeadMinutes, &dst.CustomerLeadMinutes},
		{p.CustomerChangeCutoffMinutes, &dst.CustomerChangeCutoffMinutes},
		{p.BookingHorizonDays, &dst.BookingHorizonDays},
		{p.NoShowGraceMinutes, &dst.NoShowGraceMinutes},
		{p.ExpressMaxMinutes, &dst.ExpressMaxMinutes},
	} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
}

// defaultPolicy is the dealerships column defaults (001_schema.sql).
func defaultPolicy() domain.Policy {
	return domain.Policy{
		CustomerLeadMinutes: 60, CustomerChangeCutoffMinutes: 60, BookingHorizonDays: 90,
		NoShowGraceMinutes: 30, ExpressMaxMinutes: 60,
	}
}

func (s *Server) createBay(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "dealership id is invalid")
	if !ok {
		return
	}
	var body struct {
		Name    string         `json:"name"`
		BayType domain.BayType `json:"bayType"`
	}
	if !s.bindJSON(c, &body, "request body must be bay JSON") {
		return
	}
	name, ok := validName(body.Name)
	if !ok {
		s.fail(c, domain.NewError(domain.CodeInvalid, "name is required (at most 100 characters)"))
		return
	}
	item, err := s.svc.CreateBay(c.Request.Context(), actor, id, name, body.BayType)
	if err == nil {
		s.catalog.invalidate() // the dealership version changed
	}
	s.reply(c, http.StatusCreated, item, 0, err)
}

func (s *Server) createTechnician(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var body struct {
		DealershipID string                   `json:"dealershipId"`
		Name         string                   `json:"name"`
		SkillLevel   domain.SkillLevel        `json:"skillLevel"`
		Skills       []domain.TechnicianSkill `json:"skills"`
	}
	if !s.bindJSON(c, &body, "request body must be technician JSON") {
		return
	}
	dealerID, err := parseUUID(body.DealershipID, "dealershipId is required (uuid)")
	if err != nil {
		s.fail(c, err)
		return
	}
	name, ok := validName(body.Name)
	if !ok {
		s.fail(c, domain.NewError(domain.CodeInvalid, "name is required (at most 100 characters)"))
		return
	}
	item, err := s.svc.CreateTechnician(c.Request.Context(), actor, store.NewTechnician{
		DealershipID: dealerID, Name: name, SkillLevel: body.SkillLevel, Skills: body.Skills,
	})
	s.reply(c, http.StatusCreated, item, item.Version, err)
}
