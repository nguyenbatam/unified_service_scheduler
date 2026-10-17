package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
)

// customers is one page of the staff customer search (GET /customers?q=&limit=&cursor=), in
// (name, id) order.
func (s *Server) customers(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	limit, ok := s.pageLimit(c, booking.DefaultCustomers, booking.MaxCustomerLimit)
	if !ok {
		return
	}
	scope := newPageScope("customers", strings.TrimSpace(c.Query("q")))
	after, ok := cursor(s, c, scope, 2, parseNameCursor)
	if !ok {
		return
	}
	items, next, err := s.svc.ListCustomers(c.Request.Context(), actor, c.Query("q"), limit, after)
	s.page(c, items, nextToken(scope, next, nameCursorKeys), err)
}

// createCustomer records a walk-in customer (POST /customers, staff).
func (s *Server) createCustomer(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var body struct {
		Name  string `json:"name"`
		Phone string `json:"phone"`
		Email string `json:"email"`
	}
	if !s.bindJSON(c, &body, "request body must be a customer JSON object") {
		return
	}
	item, err := s.svc.CreateCustomer(c.Request.Context(), actor, booking.CustomerInput{
		Name: body.Name, Phone: body.Phone, Email: body.Email,
	})
	s.reply(c, http.StatusCreated, item, 0, err)
}
