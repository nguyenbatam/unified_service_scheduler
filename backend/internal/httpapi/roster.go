package httpapi

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Technician rosters (admin): weekly shifts and dated absences. Every change bumps the technician
// version, so the ETag/If-Match pair is the one PATCH /admin/technicians/:id uses.

func (s *Server) getRoster(c *gin.Context) {
	actor, id, ok := s.technicianTarget(c)
	if !ok {
		return
	}
	roster, err := s.svc.Roster(c.Request.Context(), actor, id)
	s.reply(c, http.StatusOK, roster, roster.Version, err)
}

func (s *Server) putShifts(c *gin.Context) {
	actor, id, ok := s.technicianTarget(c)
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body struct {
		Shifts []domain.TechnicianShift `json:"shifts"`
	}
	if !s.bindJSON(c, &body, "request body must be {\"shifts\": [...]}") {
		return
	}
	if body.Shifts == nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, "shifts is required ([] = works the dealership's opening hours)"))
		return
	}
	roster, err := s.svc.ReplaceShifts(c.Request.Context(), actor, id, body.Shifts, ifMatch)
	s.reply(c, http.StatusOK, roster, roster.Version, err)
}

func (s *Server) addAbsence(c *gin.Context) {
	actor, id, ok := s.technicianTarget(c)
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	var body struct {
		StartsAt string `json:"startsAt"`
		EndsAt   string `json:"endsAt"`
		Reason   string `json:"reason"`
	}
	if !s.bindJSON(c, &body, "request body must be {\"startsAt\", \"endsAt\", \"reason\"?}") {
		return
	}
	start, err1 := time.Parse(time.RFC3339, body.StartsAt)
	end, err2 := time.Parse(time.RFC3339, body.EndsAt)
	if err1 != nil || err2 != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, "startsAt and endsAt must be RFC3339 timestamps"))
		return
	}
	roster, err := s.svc.AddAbsence(c.Request.Context(), actor, id,
		domain.TechnicianAbsence{StartsAt: start, EndsAt: end, Reason: body.Reason}, ifMatch)
	s.reply(c, http.StatusCreated, roster, roster.Version, err)
}

func (s *Server) deleteAbsence(c *gin.Context) {
	actor, id, ok := s.technicianTarget(c)
	if !ok {
		return
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return
	}
	absenceID, ok := s.pathUUID(c, "absenceId", "absence id is invalid")
	if !ok {
		return
	}
	roster, err := s.svc.DeleteAbsence(c.Request.Context(), actor, id, absenceID, ifMatch)
	s.reply(c, http.StatusOK, roster, roster.Version, err)
}

func (s *Server) technicianTarget(c *gin.Context) (domain.Actor, uuid.UUID, bool) {
	actor, ok := s.actor(c)
	if !ok {
		return domain.Actor{}, uuid.Nil, false
	}
	id, ok := s.pathUUID(c, "id", "technician id is invalid")
	if !ok {
		return domain.Actor{}, uuid.Nil, false
	}
	return actor, id, true
}
