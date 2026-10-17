package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func (s *Server) book(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var body struct {
		DealershipID   string   `json:"dealershipId"`
		VehicleID      string   `json:"vehicleId"`
		ServiceTypeID  string   `json:"serviceTypeId"`
		ServiceTypeIDs []string `json:"serviceTypeIds"`
		StartAt        string   `json:"startAt"`
		IsWaiter       bool     `json:"isWaiter"`
	}
	if !s.bindJSON(c, &body, "request body must be a booking JSON object") {
		return
	}
	dealerID, err := parseUUID(body.DealershipID, "dealershipId is required")
	if err != nil {
		s.fail(c, err)
		return
	}
	vehicleID, err := parseUUID(body.VehicleID, "vehicleId is required")
	if err != nil {
		s.fail(c, err)
		return
	}
	serviceID, serviceIDs, err := parseServiceIDs(body.ServiceTypeID, body.ServiceTypeIDs)
	if err != nil {
		s.fail(c, err)
		return
	}
	start, err := time.Parse(time.RFC3339, body.StartAt)
	if err != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, "startAt must be an RFC3339 timestamp"))
		return
	}
	item, created, err := s.svc.Book(c.Request.Context(), actor, booking.BookRequest{
		DealershipID:   dealerID,
		VehicleID:      vehicleID,
		ServiceTypeID:  serviceID,
		ServiceTypeIDs: serviceIDs,
		StartAt:        start,
		IsWaiter:       body.IsWaiter,
		IdempotencyKey: c.GetHeader("Idempotency-Key"),
	})
	if err != nil {
		s.countBooking(c.Request.Context(), err)
		s.fail(c, err)
		return
	}
	// A replay names the same resource as the original response (IETF Idempotency-Key draft).
	c.Header("Location", "/api/v1/appointments/"+item.ID.String())
	status := http.StatusOK
	if created {
		s.bookings.WithLabelValues("created").Inc()
		status = http.StatusCreated
	} else {
		s.bookings.WithLabelValues("replay").Inc()
	}
	s.reply(c, status, item, item.Version, nil)
}

// listAppointments is one page (limit 1–200, default 50), newest first; X-Next-Cursor carries the
// token for ?cursor= of the next page and is absent on the last one.
// appointmentWhen maps GET /appointments ?when= (absent: every appointment, newest first).
var appointmentWhen = map[string]store.AppointmentWhen{"": store.WhenAll, "upcoming": store.WhenUpcoming, "past": store.WhenPast}

func (s *Server) listAppointments(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	customerID, ok := s.optionalQueryUUID(c, "customerId")
	if !ok {
		return
	}
	vehicleID, ok := s.optionalQueryUUID(c, "vehicleId")
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
	mode := c.Query("when")
	when, ok := appointmentWhen[mode]
	if !ok {
		s.fail(c, domain.NewError(domain.CodeInvalid, "when must be upcoming or past"))
		return
	}
	scope := newPageScope("appointments:"+mode, idFilter(customerID), idFilter(vehicleID), idFilter(dealerID))
	after, ok := cursor(s, c, scope, 2, parseAppointmentCursor)
	if !ok {
		return
	}
	items, next, err := s.svc.ListAppointmentsPage(c.Request.Context(), actor, booking.AppointmentQuery{
		DealershipID: dealerID, CustomerID: customerID, VehicleID: vehicleID, Limit: limit, When: when, After: after,
	})
	s.page(c, items, nextToken(scope, next, appointmentCursorKeys), err)
}

func (s *Server) getAppointment(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "appointment id is invalid")
	if !ok {
		return
	}
	item, err := s.svc.GetAppointment(c.Request.Context(), actor, id)
	s.reply(c, http.StatusOK, item, item.Version, err)
}

type transitionFn func(ctx context.Context, actor domain.Actor, id uuid.UUID, ifMatch int) (domain.Appointment, error)

func (s *Server) cancel(c *gin.Context)    { s.transition(c, s.svc.Cancel) }
func (s *Server) checkIn(c *gin.Context)   { s.transition(c, s.svc.CheckIn) }
func (s *Server) startWork(c *gin.Context) { s.transition(c, s.svc.StartWork) }
func (s *Server) complete(c *gin.Context)  { s.transition(c, s.svc.Complete) }
func (s *Server) noShow(c *gin.Context)    { s.transition(c, s.svc.NoShow) }
func (s *Server) reinstate(c *gin.Context) { s.transition(c, s.svc.Reinstate) }

func (s *Server) transition(c *gin.Context, fn transitionFn) {
	actor, id, ifMatch, ok := s.appointmentTarget(c)
	if !ok {
		return
	}
	item, err := fn(c.Request.Context(), actor, id, ifMatch)
	s.reply(c, http.StatusOK, item, item.Version, err)
}

func (s *Server) reschedule(c *gin.Context) {
	actor, id, ifMatch, ok := s.appointmentTarget(c)
	if !ok {
		return
	}
	var body struct {
		StartAt  string `json:"startAt"`
		IsWaiter *bool  `json:"isWaiter"`
	}
	if !s.bindJSON(c, &body, "request body must be a reschedule JSON object") {
		return
	}
	start, err := time.Parse(time.RFC3339, body.StartAt)
	if err != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, "startAt must be an RFC3339 timestamp"))
		return
	}
	item, err := s.svc.Reschedule(c.Request.Context(), actor, id, booking.RescheduleRequest{
		StartAt: start, IsWaiter: body.IsWaiter, IfMatch: ifMatch,
	})
	s.reply(c, http.StatusOK, item, item.Version, err)
}

// extend requires If-Match: the body is a delta, so a client retrying after a lost response or a
// 503 would otherwise extend twice; with the version, the retry is a 412 instead.
func (s *Server) extend(c *gin.Context) {
	actor, id, ifMatch, ok := s.appointmentTarget(c)
	if !ok {
		return
	}
	if ifMatch == 0 {
		s.fail(c, domain.NewError(domain.CodePreconditionRequired, `extend needs If-Match with the appointment's version (ETag), e.g. "3": minutes is a delta, so a blind retry would extend twice`))
		return
	}
	var body struct {
		Minutes int `json:"minutes"`
	}
	if !s.bindJSON(c, &body, "request body must be {\"minutes\": n}") {
		return
	}
	item, err := s.svc.Extend(c.Request.Context(), actor, id, body.Minutes, ifMatch)
	s.reply(c, http.StatusOK, item, item.Version, err)
}

func (s *Server) reassign(c *gin.Context) {
	actor, id, ifMatch, ok := s.appointmentTarget(c)
	if !ok {
		return
	}
	var body struct {
		TechnicianID string `json:"technicianId"`
		ServiceBayID string `json:"serviceBayId"`
	}
	if !s.bindJSON(c, &body, "request body must be {\"technicianId\"?, \"serviceBayId\"?}") {
		return
	}
	req := booking.ReassignRequest{IfMatch: ifMatch}
	var err error
	if req.TechnicianID, err = optionalUUID(body.TechnicianID, "technicianId is invalid"); err != nil {
		s.fail(c, err)
		return
	}
	if req.ServiceBayID, err = optionalUUID(body.ServiceBayID, "serviceBayId is invalid"); err != nil {
		s.fail(c, err)
		return
	}
	item, err := s.svc.Reassign(c.Request.Context(), actor, id, req)
	s.reply(c, http.StatusOK, item, item.Version, err)
}

func (s *Server) history(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "appointment id is invalid")
	if !ok {
		return
	}
	limit, ok := s.pageLimit(c, store.DefaultPageLimit, store.MaxPageLimit)
	if !ok {
		return
	}
	scope := newPageScope("history", id.String())
	after, ok := cursor(s, c, scope, 1, parseHistoryCursor)
	if !ok {
		return
	}
	items, next, err := s.svc.History(c.Request.Context(), actor, id, limit, after)
	s.page(c, items, nextToken(scope, next, historyCursorKeys), err)
}

// parseServiceIDs accepts serviceTypeId (one job line) or serviceTypeIds (several); the booking
// service rejects both or neither.
func parseServiceIDs(single string, many []string) (uuid.UUID, []uuid.UUID, error) {
	var one uuid.UUID
	if single != "" {
		id, err := parseUUID(single, "serviceTypeId is invalid")
		if err != nil {
			return uuid.Nil, nil, err
		}
		one = id
	}
	ids := make([]uuid.UUID, 0, len(many))
	for _, raw := range many {
		id, err := parseUUID(strings.TrimSpace(raw), "serviceTypeIds must be ids")
		if err != nil {
			return uuid.Nil, nil, err
		}
		ids = append(ids, id)
	}
	if one == uuid.Nil && len(ids) == 0 {
		return uuid.Nil, nil, domain.NewError(domain.CodeInvalid, "serviceTypeId (or serviceTypeIds) is required")
	}
	return one, ids, nil
}

// appointmentTarget extracts actor, :id, and optional If-Match for appointment mutations.
func (s *Server) appointmentTarget(c *gin.Context) (domain.Actor, uuid.UUID, int, bool) {
	actor, ok := s.actor(c)
	if !ok {
		return domain.Actor{}, uuid.Nil, 0, false
	}
	id, ok := s.pathUUID(c, "id", "appointment id is invalid")
	if !ok {
		return domain.Actor{}, uuid.Nil, 0, false
	}
	ifMatch, ok := s.ifMatch(c)
	if !ok {
		return domain.Actor{}, uuid.Nil, 0, false
	}
	return actor, id, ifMatch, true
}

// countBooking labels a failed booking like fail answers it: the domain code, busy for database
// waits that ran out (lock/statement timeout or REQUEST_TIMEOUT; it has its own alert), canceled
// when the client went away first, and internal only for real server errors.
func (s *Server) countBooking(ctx context.Context, err error) {
	s.bookings.WithLabelValues(bookingResult(ctx, err)).Inc()
}

// bookingResultCanceled labels bookings whose client disconnected before the answer.
const bookingResultCanceled = "canceled"

func bookingResult(ctx context.Context, err error) string {
	if domainErr, ok := domain.AsError(err); ok && statusFor(domainErr.Code) != http.StatusInternalServerError {
		return domainErr.Code
	}
	switch requestEnded(ctx) {
	case requestCanceled:
		return bookingResultCanceled
	case requestTimedOut:
		return domain.CodeBusy
	}
	if store.IsLockTimeout(err) {
		return domain.CodeBusy
	}
	return domain.CodeInternal
}
