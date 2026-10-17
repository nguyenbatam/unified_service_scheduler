package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Dealership calendar (admin): weekday hour overrides and dated closures. Every change bumps the
// dealership version, so the ETag/If-Match pair is the same one PATCH /admin/dealerships uses.

func (s *Server) getCalendar(c *gin.Context) {
	actor, id, ok := s.calendarTarget(c)
	if !ok {
		return
	}
	cal, err := s.svc.Calendar(c.Request.Context(), actor, id)
	s.reply(c, http.StatusOK, cal, cal.Version, err)
}

func (s *Server) putWeeklyHours(c *gin.Context) {
	actor, id, ok := s.calendarTarget(c)
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body struct {
		Weekly []domain.WeekdayHours `json:"weekly"`
	}
	if !s.bindJSON(c, &body, "request body must be {\"weekly\": [...]}") {
		return
	}
	if body.Weekly == nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, "weekly is required ([] clears every override)"))
		return
	}
	cal, err := s.svc.ReplaceWeeklyHours(c.Request.Context(), actor, id, body.Weekly, ifMatch)
	s.catalog.invalidate() // the dealership version changed
	s.reply(c, http.StatusOK, cal, cal.Version, err)
}

func (s *Server) addClosure(c *gin.Context) {
	actor, id, ok := s.calendarTarget(c)
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body domain.Closure
	if !s.bindJSON(c, &body, "request body must be {\"date\": \"YYYY-MM-DD\", \"reason\": \"...\"}") {
		return
	}
	cal, err := s.svc.AddClosure(c.Request.Context(), actor, id, body, ifMatch)
	s.catalog.invalidate() // the dealership version changed
	s.reply(c, http.StatusCreated, cal, cal.Version, err)
}

func (s *Server) deleteClosure(c *gin.Context) {
	actor, id, ok := s.calendarTarget(c)
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	day, err := booking.ParseDate(c.Param("date"))
	if err != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, err.Error()))
		return
	}
	cal, err := s.svc.DeleteClosure(c.Request.Context(), actor, id, day, ifMatch)
	s.catalog.invalidate() // the dealership version changed
	s.reply(c, http.StatusOK, cal, cal.Version, err)
}

func (s *Server) calendarTarget(c *gin.Context) (domain.Actor, uuid.UUID, bool) {
	actor, ok := s.actor(c)
	if !ok {
		return domain.Actor{}, uuid.Nil, false
	}
	id, ok := s.pathUUID(c, "id", "dealership id is invalid")
	if !ok {
		return domain.Actor{}, uuid.Nil, false
	}
	return actor, id, true
}
