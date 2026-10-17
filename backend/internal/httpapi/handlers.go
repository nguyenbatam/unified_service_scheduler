package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

type requestIDKey struct{}

// GET /availability/next scan bounds (booking applies the same default and cap).
const (
	nextDefaultDays = 14
	nextMaxDays     = 31
)

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

// liveness reports that the process serves HTTP; it touches no dependency, so a database outage
// never gets a healthy replica restarted.
func (s *Server) liveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok", "instance": s.instanceID})
}

// readiness is 503 only when Postgres is unreachable. Redis is reported but not fatal: every Redis
// use fails open. The probe result is shared for healthCacheTTL so polling (gateway, monitors,
// anonymous callers) costs at most one round trip per dependency per TTL. The Postgres probe has
// its own connection (store.Ping), so a saturated request pool is not an outage: it is reported
// as checks.pool "saturated" (read from the pool's counters on every call, no I/O) and
// "degraded", still 200 — the instance is up and its requests queue for a connection.
func (s *Server) readiness(c *gin.Context) {
	r := s.health.get(c.Request.Context())
	checks := gin.H{"postgres": r.postgres, "redis": r.redis}
	saturated := false
	if s.poolSaturated != nil {
		saturated = s.poolSaturated()
		checks["pool"] = checkOK
		if saturated {
			checks["pool"] = checkSaturated
		}
	}
	body := gin.H{"status": "ok", "instance": s.instanceID, "checks": checks}
	switch {
	case r.postgres != checkOK:
		body["status"] = "unavailable"
		c.JSON(http.StatusServiceUnavailable, body)
		return
	case r.redis != checkOK || saturated:
		body["status"] = "degraded"
	}
	c.JSON(http.StatusOK, body)
}

// probeDependencies pings Postgres and Redis concurrently. Redis gets its own short budget: it is
// optional (every use fails open), so an outage must not make readiness slow — go-redis would
// otherwise spend its dial retries (about 2 s) on every probe, and every readiness caller waits
// for the shared probe (healthCache).
func (s *Server) probeDependencies(ctx context.Context) (dbErr, redisErr error) {
	if s.svc == nil {
		return errors.New("no booking service"), errors.New("no booking service")
	}
	done := make(chan error, 1)
	go func() {
		rctx, cancel := context.WithTimeout(ctx, redisProbeTimeout)
		defer cancel()
		done <- s.svc.PingRedis(rctx)
	}()
	dbErr = s.svc.Ping(ctx)
	return dbErr, <-done
}

// dealerships is one page of dealerships in (name, id) order, cached per page (catalog.go).
func (s *Server) dealerships(c *gin.Context) {
	limit, ok := s.pageLimit(c, store.DefaultPageLimit, store.MaxPageLimit)
	if !ok {
		return
	}
	scope := newPageScope("dealerships")
	after, ok := cursor(s, c, scope, 2, parseNameCursor)
	if !ok {
		return
	}
	s.serveCatalog(c, catalogDealerships.page(limit, c.Query("cursor")), func(ctx context.Context) (any, string, error) {
		items, next, err := s.svc.ListDealerships(ctx, limit, after)
		return items, nextToken(scope, next, nameCursorKeys), err
	})
}

// serviceTypes is one page of the service catalog in (duration, name, id) order, cached per page.
func (s *Server) serviceTypes(c *gin.Context) {
	limit, ok := s.pageLimit(c, store.DefaultPageLimit, store.MaxPageLimit)
	if !ok {
		return
	}
	scope := newPageScope("service-types")
	after, ok := cursor(s, c, scope, 3, parseServiceTypeCursor)
	if !ok {
		return
	}
	s.serveCatalog(c, catalogServiceTypes.page(limit, c.Query("cursor")), func(ctx context.Context) (any, string, error) {
		items, next, err := s.svc.ListServiceTypes(ctx, limit, after)
		return items, nextToken(scope, next, serviceTypeCursorKeys), err
	})
}

// vehicles is one page (limit 1–200, default 50) in (owner, make, model, id) order, optionally
// narrowed by the search text q.
func (s *Server) vehicles(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	limit, ok := s.pageLimit(c, booking.DefaultVehicleLimit, booking.MaxVehicleLimit)
	if !ok {
		return
	}
	q := booking.VehicleQuery{Search: c.Query("q"), Limit: limit}
	scope := newPageScope("vehicles", strings.TrimSpace(q.Search))
	if q.After, ok = cursor(s, c, scope, 4, parseVehicleCursor); !ok {
		return
	}
	items, next, err := s.svc.ListVehicles(c.Request.Context(), actor, q)
	s.page(c, items, nextToken(scope, next, vehicleCursorKeys), err)
}

// getVehicle lets the UI show a vehicle outside the current search page (e.g. a focused booking's).
func (s *Server) getVehicle(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	id, ok := s.pathUUID(c, "id", "vehicle id is invalid")
	if !ok {
		return
	}
	item, err := s.svc.GetVehicle(c.Request.Context(), actor, id)
	s.result(c, item, err)
}

func (s *Server) createVehicle(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	var body struct {
		CustomerID string `json:"customerId"`
		VIN        string `json:"vin"`
		Make       string `json:"make"`
		Model      string `json:"model"`
		Year       int    `json:"year"`
	}
	if !s.bindJSON(c, &body, "request body must be a vehicle JSON object") {
		return
	}
	in := booking.VehicleInput{VIN: body.VIN, Make: body.Make, Model: body.Model, Year: body.Year}
	if body.CustomerID != "" {
		id, err := parseUUID(body.CustomerID, "customerId is invalid")
		if err != nil {
			s.fail(c, err)
			return
		}
		in.CustomerID = id
	}
	item, err := s.svc.CreateVehicle(c.Request.Context(), actor, in)
	s.reply(c, http.StatusCreated, item, 0, err)
}

func (s *Server) availability(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	q, ok := s.availabilityQuery(c)
	if !ok {
		return
	}
	day, err := booking.ParseDate(c.Query("date"))
	if err != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, err.Error()))
		return
	}
	q.Day = day
	items, err := s.svc.Availability(c.Request.Context(), actor, q)
	s.result(c, items, err)
}

// nextAvailability is the multi-day "first available" search.
func (s *Server) nextAvailability(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	q, ok := s.availabilityQuery(c)
	if !ok {
		return
	}
	next := booking.NextQuery{AvailabilityQuery: q}
	if raw := c.Query("from"); raw != "" {
		day, err := booking.ParseDate(raw)
		if err != nil {
			s.fail(c, domain.NewError(domain.CodeInvalid, "from: "+err.Error()))
			return
		}
		next.From = day
	}
	for _, p := range []struct {
		name string
		max  int
		dst  *int
	}{{"days", nextMaxDays, &next.Days}, {"limit", 20, &next.Limit}, {"perDay", 20, &next.PerDay}} {
		raw := c.Query(p.name)
		if raw == "" {
			continue
		}
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > p.max {
			s.fail(c, domain.NewError(domain.CodeInvalid, fmt.Sprintf("%s must be 1–%d", p.name, p.max)))
			return
		}
		*p.dst = n
	}
	switch c.Query("prefer") {
	case "":
	case "recommended":
		next.PreferRecommended = true
	default:
		s.fail(c, domain.NewError(domain.CodeInvalid, "prefer must be recommended"))
		return
	}
	items, err := s.svc.NextAvailable(c.Request.Context(), actor, next)
	s.result(c, items, err)
}

// availabilityQuery reads dealershipId, vehicleId, serviceTypeId | serviceTypeIds (comma
// separated), isWaiter, and the optional excludeAppointmentId (the appointment being rescheduled).
func (s *Server) availabilityQuery(c *gin.Context) (booking.AvailabilityQuery, bool) {
	dealerID, ok := s.queryUUID(c, "dealershipId")
	if !ok {
		return booking.AvailabilityQuery{}, false
	}
	vehicleID, ok := s.queryUUID(c, "vehicleId")
	if !ok {
		return booking.AvailabilityQuery{}, false
	}
	var many []string
	if raw := c.Query("serviceTypeIds"); raw != "" {
		many = strings.Split(raw, ",")
	}
	serviceID, serviceIDs, err := parseServiceIDs(c.Query("serviceTypeId"), many)
	if err != nil {
		s.fail(c, err)
		return booking.AvailabilityQuery{}, false
	}
	excludeID, ok := s.optionalQueryUUID(c, "excludeAppointmentId")
	if !ok {
		return booking.AvailabilityQuery{}, false
	}
	isWaiter, waiterGiven, ok := s.optionalQueryBool(c, "isWaiter")
	if !ok {
		return booking.AvailabilityQuery{}, false
	}
	return booking.AvailabilityQuery{
		DealershipID: dealerID, VehicleID: vehicleID, ServiceTypeID: serviceID, ServiceTypeIDs: serviceIDs,
		IsWaiter:             isWaiter,
		WaiterGiven:          waiterGiven,
		ExcludeAppointmentID: excludeID,
	}, true
}

func (s *Server) schedule(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	dealerID, ok := s.queryUUID(c, "dealershipId")
	if !ok {
		return
	}
	day, err := booking.ParseDate(c.Query("date"))
	if err != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, err.Error()))
		return
	}
	items, err := s.svc.Schedule(c.Request.Context(), actor, dealerID, day)
	s.result(c, items, err)
}
