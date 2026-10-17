package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func (s *Server) listTechniciansAdmin(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	dealerID, ok := s.optionalQueryUUID(c, "dealershipId")
	if !ok {
		return
	}
	limit, ok := s.pageLimit(c, store.DefaultPageLimit, store.MaxPageLimit)
	if !ok {
		return
	}
	scope := newPageScope("technicians", idFilter(dealerID))
	after, ok := cursor(s, c, scope, 2, parseNameCursor)
	if !ok {
		return
	}
	items, next, err := s.svc.ListTechniciansAdmin(c.Request.Context(), actor, dealerID, limit, after)
	s.page(c, items, nextToken(scope, next, nameCursorKeys), err)
}

func (s *Server) patchDealership(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "dealership id is invalid")
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body struct {
		OpenTime             *string `json:"openTime"`
		CloseTime            *string `json:"closeTime"`
		LunchStart           *string `json:"lunchStart"`
		LunchEnd             *string `json:"lunchEnd"`
		StagingCapacity      *int16  `json:"stagingCapacity"`
		StagingWindowMinutes *int16  `json:"stagingWindowMinutes"`
		Policy               *policy `json:"policy"`
	}
	if !s.bindJSON(c, &body, "request body must be dealership config JSON") {
		return
	}
	if (body.LunchStart == nil) != (body.LunchEnd == nil) {
		s.fail(c, domain.NewError(domain.CodeInvalid, "lunchStart and lunchEnd must be sent together (both \"\" clears lunch)"))
		return
	}
	for name, v := range map[string]*string{"openTime": body.OpenTime, "closeTime": body.CloseTime} {
		if v != nil && !validClock(*v) {
			s.fail(c, domain.NewError(domain.CodeInvalid, name+" must be HH:MM"))
			return
		}
	}
	if body.LunchStart != nil && (*body.LunchStart != "" || *body.LunchEnd != "") {
		if !validClock(*body.LunchStart) || !validClock(*body.LunchEnd) {
			s.fail(c, domain.NewError(domain.CodeInvalid, "lunchStart/lunchEnd must be HH:MM, or both empty to clear"))
			return
		}
	}
	patch := store.DealershipPatch{
		OpenTime:             body.OpenTime,
		CloseTime:            body.CloseTime,
		LunchStart:           body.LunchStart,
		LunchEnd:             body.LunchEnd,
		StagingCapacity:      body.StagingCapacity,
		StagingWindowMinutes: body.StagingWindowMinutes,
		IfMatch:              ifMatch,
	}
	if p := body.Policy; p != nil {
		patch.CustomerLeadMinutes, patch.CustomerChangeCutoffMinutes = p.CustomerLeadMinutes, p.CustomerChangeCutoffMinutes
		patch.BookingHorizonDays, patch.NoShowGraceMinutes, patch.ExpressMaxMinutes = p.BookingHorizonDays, p.NoShowGraceMinutes, p.ExpressMaxMinutes
	}
	if msg := validateDealershipPatch(patch); msg != "" {
		s.fail(c, domain.NewError(domain.CodeInvalid, msg))
		return
	}
	item, err := s.svc.UpdateDealership(c.Request.Context(), actor, id, patch)
	s.catalog.invalidate()
	s.reply(c, http.StatusOK, item, item.Version, err)
}

func (s *Server) patchServiceType(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "service type id is invalid")
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body struct {
		DurationMinutes *int                    `json:"durationMinutes"`
		MinSkillLevel   *domain.SkillLevel      `json:"minSkillLevel"`
		RequiredSkill   *domain.TechnicianSkill `json:"requiredSkill"`
		RequiredBayType *domain.BayType         `json:"requiredBayType"`
	}
	if !s.bindJSON(c, &body, "request body must be service type config JSON") {
		return
	}
	if d := body.DurationMinutes; d != nil && (*d <= 0 || *d > 600 || *d%30 != 0) {
		s.fail(c, domain.NewError(domain.CodeInvalid, "durationMinutes must be a positive multiple of 30, at most 600"))
		return
	}
	if l := body.MinSkillLevel; l != nil && !l.Valid() {
		s.fail(c, domain.NewError(domain.CodeInvalid, "minSkillLevel must be 1–4"))
		return
	}
	if sk := body.RequiredSkill; sk != nil && !sk.Valid() {
		s.fail(c, domain.NewError(domain.CodeInvalid, "requiredSkill must be 1–3"))
		return
	}
	if b := body.RequiredBayType; b != nil && !b.Valid() {
		s.fail(c, domain.NewError(domain.CodeInvalid, "requiredBayType must be 1–4"))
		return
	}
	item, err := s.svc.UpdateServiceType(c.Request.Context(), actor, id, store.ServiceTypePatch{
		DurationMinutes: body.DurationMinutes,
		MinSkillLevel:   body.MinSkillLevel,
		RequiredSkill:   body.RequiredSkill,
		RequiredBayType: body.RequiredBayType,
		IfMatch:         ifMatch,
	})
	s.catalog.invalidate()
	s.reply(c, http.StatusOK, item, item.Version, err)
}

func (s *Server) patchTechnician(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "technician id is invalid")
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body struct {
		SkillLevel *domain.SkillLevel       `json:"skillLevel"`
		Active     *bool                    `json:"active"`
		Skills     []domain.TechnicianSkill `json:"skills"`
	}
	if !s.bindJSON(c, &body, "request body must be technician config JSON") {
		return
	}
	if l := body.SkillLevel; l != nil && !l.Valid() {
		s.fail(c, domain.NewError(domain.CodeInvalid, "skillLevel must be 1–4"))
		return
	}
	item, err := s.svc.UpdateTechnician(c.Request.Context(), actor, id, store.TechnicianPatch{
		SkillLevel: body.SkillLevel, Active: body.Active, Skills: body.Skills, IfMatch: ifMatch,
	})
	s.reply(c, http.StatusOK, item, item.Version, err)
}

// validateDealershipPatch mirrors the table CHECKs with readable messages ("" = valid).
func validateDealershipPatch(p store.DealershipPatch) string {
	inRange := func(v *int16, lo, hi int16) bool { return v == nil || (*v >= lo && *v <= hi) }
	switch {
	case !inRange(p.StagingCapacity, 0, 100):
		return "stagingCapacity must be 0–100 (0 = unlimited)"
	case p.StagingWindowMinutes != nil && *p.StagingWindowMinutes != 15 && *p.StagingWindowMinutes != 30 && *p.StagingWindowMinutes != 60:
		return "stagingWindowMinutes must be 15, 30, or 60"
	case !inRange(p.CustomerLeadMinutes, 0, 10080):
		return "policy.customerLeadMinutes must be 0–10080"
	case !inRange(p.CustomerChangeCutoffMinutes, 0, 10080):
		return "policy.customerChangeCutoffMinutes must be 0–10080"
	case !inRange(p.BookingHorizonDays, 1, 365):
		return "policy.bookingHorizonDays must be 1–365"
	case !inRange(p.NoShowGraceMinutes, 5, 240):
		return "policy.noShowGraceMinutes must be 5–240"
	case !inRange(p.ExpressMaxMinutes, 0, 600):
		return "policy.expressMaxMinutes must be 0–600 (0 = never soft-upgrade)"
	}
	return ""
}

func validClock(v string) bool {
	_, err := booking.ParseClock(v)
	return err == nil && len(v) == 5
}
