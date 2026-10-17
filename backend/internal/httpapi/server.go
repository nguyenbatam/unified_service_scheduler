package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	svc     *booking.Service
	store   *store.Store
	auth    *auth.Service
	hub     *realtime.Hub
	limiter Limiter
	tickets TicketRedeemer
	proxies []string
	// loadUser reads an active user by id (store.FindUserByID); nil without a store.
	loadUser func(ctx context.Context, id uuid.UUID) (domain.User, error)
	// loadUserSession is loadUser plus whether a refresh family is live, in the same query
	// (store.FindUserSession), for access tokens carrying a sid; nil without a store.
	loadUserSession func(ctx context.Context, id, sid uuid.UUID) (domain.User, bool, error)
	// families caches loadUserSession's family verdict next to users (same TTL); nil when users is.
	families *familyCache
	// users caches loadUser for userCacheTTL; nil when the TTL is 0.
	users          *userCache
	userCacheTTL   time.Duration
	userCacheClock func() time.Time
	health         *healthCache
	// poolSaturated reports the request pool fully in use (store.PoolSaturated); nil without a
	// store. Readiness shows it as checks.pool, apart from Postgres reachability.
	poolSaturated func() bool
	// catalog caches GET /dealerships and /service-types for catalogTTL (0 disables).
	catalog    *catalogCache
	catalogTTL time.Duration
	// readPerUser / readPerIP bound customers' plain reads (appointments, vehicles, catalog) per
	// minute.
	readPerUser int
	readPerIP   int
	// availPerUser / availPerIP and writePerUser / writePerIP bound customers' availability reads
	// and booking writes per minute (WithCustomerLimits).
	availPerUser, availPerIP int
	writePerUser, writePerIP int
	// requestTimeout is the per-request deadline (deadline middleware); 0 disables it.
	requestTimeout time.Duration
	// loginFallback counts login/register buckets on this instance while Redis is unavailable.
	loginFallback *memoryLimiter
	// limiterDownLog spaces the warnings logged while the shared limiter fails.
	limiterDownLog *logSampler
	log            *slog.Logger
	instanceID     string
	registry       *prometheus.Registry
	requests       *prometheus.CounterVec
	latency        *prometheus.HistogramVec
	bookings       *prometheus.CounterVec
	inFlight       prometheus.Gauge
	// eventsSkipped reports events skipped after repeated handler panics (WithEventsSkipped).
	eventsSkipped func() uint64
	// sessions sets the refresh cookie and its lifetimes (WithSessions); refreshes counts
	// POST /auth/refresh outcomes.
	sessions  SessionConfig
	refreshes *prometheus.CounterVec
	// allowedOrigins are the browser origins the cookie endpoints accept besides their own
	// (WithAllowedOrigins); trustedNets is proxies parsed, for X-Forwarded-Proto.
	allowedOrigins []string
	trustedNets    []netip.Prefix
}

type Option func(*Server)

// WithEventsSkipped exports skipped() as scheduler_events_skipped_total (events.SkippedEvents).
func WithEventsSkipped(skipped func() uint64) Option {
	return func(s *Server) { s.eventsSkipped = skipped }
}

// Limiter counts hits per bucket in a fixed window shared by every instance (cache.RateLimiter).
type Limiter interface {
	Allow(ctx context.Context, bucket string, limit int, window time.Duration) (bool, error)
}

// batchLimiter is implemented by limiters that count several buckets in one round trip
// (cache.RateLimiter); others are called once per bucket.
type batchLimiter interface {
	AllowAll(ctx context.Context, buckets ...cache.Bucket) (bool, error)
}

// retryLimiter / retryPeekLimiter are batchLimiter / peekLimiter that also report how long until
// the exceeded window ends (cache.RateLimiter), so Retry-After is the real wait, not a full window.
type retryLimiter interface {
	AllowAllRetry(ctx context.Context, buckets ...cache.Bucket) (bool, time.Duration, error)
}

type retryPeekLimiter interface {
	ExceededRetry(ctx context.Context, b cache.Bucket) (bool, time.Duration, error)
}

// peekLimiter is implemented by limiters that can check a bucket without counting a hit
// (cache.RateLimiter); login uses it so a correct password never counts as a failure.
type peekLimiter interface {
	Exceeded(ctx context.Context, b cache.Bucket) (bool, error)
}

// WithRateLimiter enables throttling: login/register per IP, IP+email, and failed passwords per
// account; customers' availability searches and writes per user and IP; WS tickets per user.
func WithRateLimiter(l Limiter) Option {
	return func(s *Server) { s.limiter = l }
}

// TicketRedeemer makes WebSocket tickets single-use across instances (cache.Tickets): Redeem
// reports false when ticket id was already used.
type TicketRedeemer interface {
	Redeem(ctx context.Context, id string, ttl time.Duration) (bool, error)
}

// WithTicketRedeemer rejects replayed WebSocket tickets. Without it a ticket can be reused until
// it expires (60 s).
func WithTicketRedeemer(t TicketRedeemer) Option {
	return func(s *Server) { s.tickets = t }
}

// WithTrustedProxies sets the proxies (IPs/CIDRs) whose X-Forwarded-For / X-Real-IP decide the
// client IP used by rate limits. By default nobody is trusted and the TCP peer is the client.
func WithTrustedProxies(proxies []string) Option {
	return func(s *Server) { s.proxies = proxies }
}

// WithUserCache lets requireAuth reuse a user row for up to ttl (0, the default, disables it):
// one query fewer per request, at the price of role changes, deactivation, and sign-outs made on
// another instance (or in SQL) taking up to ttl to apply here. Logout on this instance, and
// anything passed to InvalidateUser, applies at once.
func WithUserCache(ttl time.Duration) Option {
	return func(s *Server) { s.userCacheTTL = max(ttl, 0) }
}

// Defaults for the options below (config.Load reads the same defaults from the environment).
const (
	DefaultCatalogCacheTTL  = 2 * time.Second
	MaxCatalogCacheTTL      = 5 * time.Second
	DefaultReadLimitPerUser = 600
	DefaultReadLimitPerIP   = 3000
	// Customers' availability reads and booking writes (book, reschedule, cancel) per minute.
	DefaultAvailabilityLimitPerUser = 120
	DefaultAvailabilityLimitPerIP   = 600
	DefaultWriteLimitPerUser        = 30
	DefaultWriteLimitPerIP          = 120
)

// WithCatalogCache sets how long each instance reuses the GET /dealerships and /service-types
// bodies (default DefaultCatalogCacheTTL, capped at MaxCatalogCacheTTL; 0 disables). These public
// catalog reads are the only responses an instance may serve stale, by at most ttl, after a change
// made on another instance; changes made through this instance invalidate at once.
func WithCatalogCache(ttl time.Duration) Option {
	return func(s *Server) { s.catalogTTL = min(max(ttl, 0), MaxCatalogCacheTTL) }
}

// MinAvailabilityLimit is the smallest availability budget that still admits the dearest single
// read: GET /availability/next over the maximum range costs one unit per started week.
const MinAvailabilityLimit = (nextMaxDays + 6) / 7

// WithCustomerLimits sets the per-minute budgets of customers' availability reads and booking
// writes per user and per client IP (staff are never counted). Values below 1 keep the defaults;
// availability budgets are raised to MinAvailabilityLimit, or a 31-day next-available search
// could never pass.
// A shared NAT or a load generator on one machine hits the per-IP buckets first; the bench raises
// them to measure the customer path instead of the limiter.
func WithCustomerLimits(availPerUser, availPerIP, writePerUser, writePerIP int) Option {
	return func(s *Server) {
		for _, v := range []struct {
			dst *int
			val int
		}{{&s.availPerUser, availPerUser}, {&s.availPerIP, availPerIP}, {&s.writePerUser, writePerUser}, {&s.writePerIP, writePerIP}} {
			if v.val > 0 {
				*v.dst = v.val
			}
		}
		s.availPerUser = max(s.availPerUser, MinAvailabilityLimit)
		s.availPerIP = max(s.availPerIP, MinAvailabilityLimit)
	}
}

// WithReadLimits sets the per-minute budget of customers' plain reads (GET /appointments,
// /appointments/:id, /vehicles, /vehicles/:id, /dealerships, /service-types) per user and per
// client IP. Values below 1 keep the
// defaults. /auth/me is left out: the web app treats a failed session check as a logout, so a
// shared-IP bucket running dry must not sign customers out.
func WithReadLimits(perUser, perIP int) Option {
	return func(s *Server) {
		if perUser > 0 {
			s.readPerUser = perUser
		}
		if perIP > 0 {
			s.readPerIP = perIP
		}
	}
}

// WithUserCacheClock replaces the user cache's clock. For tests only: it lets a test expire
// cached rows without sleeping. No effect without WithUserCache.
func WithUserCacheClock(now func() time.Time) Option {
	return func(s *Server) { s.userCacheClock = now }
}

func New(svc *booking.Service, st *store.Store, authSvc *auth.Service, hub *realtime.Hub, log *slog.Logger, instanceID string, opts ...Option) *Server {
	if log == nil {
		log = slog.Default()
	}
	if hub == nil {
		hub = realtime.NewHub(log)
	}
	if authSvc == nil {
		// No silent fallback secret: a server must be built with the configured JWT secret.
		panic("httpapi.New: auth service is required")
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	constLabels := prometheus.Labels{"instance_id": instanceID}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "scheduler_http_requests_total",
		Help:        "HTTP requests by route and status.",
		ConstLabels: constLabels,
	}, []string{"method", "route", "status"})
	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "scheduler_http_request_duration_seconds",
		Help:        "HTTP request latency.",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: constLabels,
	}, []string{"method", "route"})
	bookings := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "scheduler_bookings_total",
		Help:        "Booking attempts by result.",
		ConstLabels: constLabels,
	}, []string{"result"})
	wsClients := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        "scheduler_ws_clients",
		Help:        "Connected WebSocket clients on this instance.",
		ConstLabels: constLabels,
	}, func() float64 { return float64(hub.ClientCount()) })
	wsRejected := prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        "scheduler_ws_rejected_total",
		Help:        "WebSocket connections refused because the user was at the per-user cap.",
		ConstLabels: constLabels,
	}, func() float64 { return float64(hub.RejectedConnections()) })
	inFlight := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "scheduler_http_in_flight_requests",
		Help:        "HTTP requests being served right now (WebSocket sessions excluded): L in Little's law L = λW.",
		ConstLabels: constLabels,
	})
	refreshes := newRefreshCounter(constLabels)
	registry.MustRegister(requests, latency, bookings, wsClients, wsRejected, inFlight, refreshes)
	if st != nil {
		registry.MustRegister(newQueueCollector(st, constLabels, log), newPoolCollector(st, constLabels))
	}
	s := &Server{
		svc: svc, store: st, auth: authSvc, hub: hub, log: log, instanceID: instanceID,
		registry: registry, requests: requests, latency: latency, bookings: bookings, inFlight: inFlight,
		catalogTTL: DefaultCatalogCacheTTL, readPerUser: DefaultReadLimitPerUser, readPerIP: DefaultReadLimitPerIP,
		availPerUser: DefaultAvailabilityLimitPerUser, availPerIP: DefaultAvailabilityLimitPerIP,
		writePerUser: DefaultWriteLimitPerUser, writePerIP: DefaultWriteLimitPerIP,
		requestTimeout: DefaultRequestTimeout, loginFallback: newMemoryLimiter(fallbackLimiterMaxKeys),
		limiterDownLog: newLogSampler(limiterDownLogEvery),
		sessions:       DefaultSessionConfig(), refreshes: refreshes,
	}
	if st != nil {
		s.loadUser = st.FindUserByID
		s.loadUserSession = st.FindUserSession
		s.poolSaturated = st.PoolSaturated
	}
	for _, opt := range opts {
		opt(s)
	}
	s.trustedNets = trustedNets(s.proxies)
	if s.userCacheTTL > 0 {
		s.users = newUserCache(s.userCacheTTL, defaultUserCacheSize)
		s.families = newFamilyCache(s.userCacheTTL, defaultUserCacheSize)
		if s.userCacheClock != nil {
			s.users.now = s.userCacheClock
			s.families.now = s.userCacheClock
		}
	}
	s.catalog = newCatalogCache(s.catalogTTL)
	s.health = newHealthCache(healthCacheTTL, log, s.probeDependencies)
	if svc != nil {
		registry.MustRegister(newDependencyCollector(s.health, constLabels))
	}
	if s.eventsSkipped != nil {
		registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name:        "scheduler_events_skipped_total",
			Help:        "Kafka events skipped after repeated handler panics (each one is lost to its consumer group).",
			ConstLabels: constLabels,
		}, func() float64 { return float64(s.eventsSkipped()) }))
	}
	if st != nil {
		hub.SetValidator(s.validateSessions)
	}
	return s
}

// Handler is the server's HTTP handler: the routes (engine) behind response compression.
func (s *Server) Handler() http.Handler {
	return withCompression(s.engine())
}

// engine builds the gin router: middleware and every route.
func (s *Server) engine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.RedirectTrailingSlash = true
	// gin trusts every proxy by default, which lets any client pick its own rate-limit bucket
	// with X-Forwarded-For. Options were validated by config; a bad list trusts nobody.
	if err := r.SetTrustedProxies(s.proxies); err != nil {
		s.log.Error("trusted proxies", "err", err)
		_ = r.SetTrustedProxies(nil)
	}
	// observe runs first so it records (and logs) requests whose handler panicked: recovery
	// below it turns the panic into the standard JSON 500 before observe reads the status. The
	// deadline comes last, so it bounds the handlers' (and auth's) database waits only.
	r.Use(s.observe(), s.recovery(), cors(), s.deadline())

	// Scraped directly on each backend; the gateway does not expose it.
	r.GET("/metrics", gin.WrapH(promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{})))

	v1 := r.Group("/api/v1")
	{
		v1.GET("/health", s.readiness)
		v1.GET("/health/live", s.liveness)
		v1.GET("/health/ready", s.readiness)
		v1.GET("/openapi.yaml", s.openapiYAML)
		v1.GET("/docs", s.swaggerUI)
		v1.GET("/docs/", s.swaggerUI)
		v1.GET("/docs/swagger-init.js", s.swaggerInit)
		v1.POST("/auth/register", s.register)
		v1.POST("/auth/login", s.login)
		// Cookie-authenticated (refresh), or Bearer or cookie (logout): see session.go.
		v1.POST("/auth/refresh", s.refresh)
		v1.POST("/auth/logout", s.logout)
		v1.GET("/ws/dispatch", s.serveWS)

		authed := v1.Group("/")
		authed.Use(s.requireAuth())
		{
			// Plain reads: a generous per-user (and per-IP) budget for self-service users, so a
			// scripted client cannot hammer Postgres through them; staff are not counted.
			plainReads := s.throttle(throttleRule{name: "read", perUser: s.readPerUser, perIP: s.readPerIP})
			authed.GET("/auth/me", s.me)
			authed.POST("/auth/ws-ticket", s.throttle(throttleRule{name: "ws-ticket", perUser: wsTicketLimitPerUser, staff: true}), s.wsTicket)
			// Cached per instance with an ETag, and counted like the other plain reads: a 304 or a
			// cache hit is cheap, but uncached pages (cursors the cache did not hand out) reach Postgres.
			authed.GET("/dealerships", plainReads, s.dealerships)
			authed.GET("/service-types", plainReads, s.serviceTypes)
			authed.GET("/vehicles", plainReads, s.vehicles)
			reads := s.throttle(throttleRule{name: "availability", perUser: s.availPerUser, perIP: s.availPerIP})
			nextReads := s.throttle(throttleRule{name: "availability", perUser: s.availPerUser, perIP: s.availPerIP, cost: nextAvailabilityCost})
			writes := s.throttle(throttleRule{name: "write", perUser: s.writePerUser, perIP: s.writePerIP})
			authed.POST("/vehicles", writes, s.createVehicle)
			authed.GET("/vehicles/:id", plainReads, s.getVehicle)
			authed.GET("/availability", reads, s.availability)
			authed.GET("/availability/next", nextReads, s.nextAvailability)

			// POST without trailing slash — clients call /appointments (not /appointments/).
			authed.POST("/appointments", writes, s.book)
			authed.GET("/appointments", plainReads, s.listAppointments)
			appts := authed.Group("/appointments")
			{
				// Ownership is enforced in booking.Service: customers act only on their own.
				appts.GET("/:id", plainReads, s.getAppointment)
				appts.POST("/:id/cancel", writes, s.cancel)
				appts.POST("/:id/reschedule", writes, s.reschedule)
			}

			staff := authed.Group("/")
			staff.Use(s.requireRole(domain.RoleAdmin, domain.RoleDispatcher))
			{
				staff.GET("/schedule", s.schedule)
				staff.POST("/appointments/:id/check-in", s.checkIn)
				staff.POST("/appointments/:id/start", s.startWork)
				staff.POST("/appointments/:id/complete", s.complete)
				staff.POST("/appointments/:id/no-show", s.noShow)
				staff.POST("/appointments/:id/reinstate", s.reinstate)
				staff.POST("/appointments/:id/extend", s.extend)
				staff.POST("/appointments/:id/reassign", s.reassign)
				staff.GET("/appointments/:id/history", s.history)
				staff.GET("/customers", s.throttle(throttleRule{name: "customer-search", perUser: customerSearchLimitPerUser, staff: true}), s.customers)
				staff.POST("/customers", s.throttle(throttleRule{name: "customer-create", perUser: customerCreateLimitPerUser, staff: true}), s.createCustomer)
			}

			admin := authed.Group("/admin")
			admin.Use(s.requireRole(domain.RoleAdmin))
			{
				admin.GET("/technicians", s.listTechniciansAdmin)
				admin.POST("/dealerships", s.createDealership)
				admin.PATCH("/dealerships/:id", s.patchDealership)
				admin.POST("/dealerships/:id/bays", s.createBay)
				admin.GET("/dealerships/:id/calendar", s.getCalendar)
				admin.PUT("/dealerships/:id/hours", s.putWeeklyHours)
				admin.POST("/dealerships/:id/closures", s.addClosure)
				admin.DELETE("/dealerships/:id/closures/:date", s.deleteClosure)
				admin.PATCH("/service-types/:id", s.patchServiceType)
				admin.POST("/technicians", s.createTechnician)
				admin.PATCH("/technicians/:id", s.patchTechnician)
				admin.GET("/technicians/:id/roster", s.getRoster)
				admin.PUT("/technicians/:id/shifts", s.putShifts)
				admin.POST("/technicians/:id/absences", s.addAbsence)
				admin.DELETE("/technicians/:id/absences/:absenceId", s.deleteAbsence)
				admin.GET("/outbox/failed", s.listFailedOutbox)
				admin.POST("/outbox/requeue", s.requeueOutbox)
			}
		}
	}
	return r
}

// panickedKey marks a request whose handler panicked after writing its response (see recovery).
const panickedKey = "httpapi.panicked"

func (s *Server) observe() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/api/v1/ws/dispatch" {
			c.Next()
			return
		}
		s.inFlight.Inc()
		defer s.inFlight.Dec()
		start := time.Now()
		id := c.GetHeader("X-Request-ID")
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		c.Header("X-Request-ID", id)
		c.Header("X-Instance-ID", s.instanceID)
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), requestIDKey{}, id))
		// Deferred, so a panic that escapes recovery is still counted (as a 500) before it
		// propagates.
		completed := false
		defer func() {
			status := c.Writer.Status()
			if (!completed && !c.Writer.Written()) || c.GetBool(panickedKey) {
				status = http.StatusInternalServerError
			}
			s.record(c, start, status)
		}()
		c.Next()
		completed = true
	}
}

// record counts one request in the metrics and writes its access log line.
func (s *Server) record(c *gin.Context, start time.Time, status int) {
	route := c.FullPath()
	if route == "" {
		route = "unmatched"
	}
	method := metricMethod(c.Request.Method)
	elapsed := time.Since(start)
	s.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	s.latency.WithLabelValues(method, route).Observe(elapsed.Seconds())
	level, ok := accessLogLevel(method, route, status)
	if !ok || !s.log.Enabled(c.Request.Context(), level) {
		return
	}
	s.log.Log(c.Request.Context(), level, "request",
		"method", method,
		"path", c.Request.URL.Path,
		"route", route,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
		"request_id", requestID(c.Request.Context()),
		"instance", s.instanceID,
	)
}

// accessLogLevel is the level of a request's access log line, and false to skip it. Successful
// scrapes and health probes are skipped (every few seconds per prober, and the metrics count them
// anyway); other successful reads go to Debug, since at load the synchronous JSON line is a
// measurable share of a cheap read's cost; writes and every 4xx/5xx stay at Info.
func accessLogLevel(method, route string, status int) (slog.Level, bool) {
	if status >= http.StatusBadRequest {
		return slog.LevelInfo, true
	}
	if route == "/metrics" || strings.HasPrefix(route, "/api/v1/health") {
		return 0, false
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return slog.LevelDebug, true
	default:
		return slog.LevelInfo, true
	}
}

// metricMethod bounds the method label: any token is a valid HTTP method, so unknown ones
// (which reach the 404 chain) would otherwise create a new series each.
func metricMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodOptions, http.MethodHead:
		return method
	default:
		return "OTHER"
	}
}

// recovery turns a handler panic into the standard JSON 500 and logs it (with the stack) through
// slog instead of gin's plain-text stderr dump. Broken connections are aborted silently by gin.
func (s *Server) recovery() gin.HandlerFunc {
	return gin.CustomRecoveryWithWriter(nil, func(c *gin.Context, rec any) {
		s.log.Error("panic recovered", "panic", fmt.Sprint(rec), "stack", string(debug.Stack()),
			"path", c.Request.URL.Path, "request_id", requestID(c.Request.Context()))
		if c.Writer.Written() {
			// The status line (and maybe part of the body) is already out: a JSON 500 cannot replace
			// it, so drop the connection rather than let the client take a truncated body for a
			// complete one. net/http aborts silently on ErrAbortHandler; observe counts a 500.
			c.Set(panickedKey, true)
			panic(http.ErrAbortHandler)
		}
		writeError(c, http.StatusInternalServerError, domain.CodeInternal, "internal error")
	})
}

func cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		// The API authenticates with Bearer tokens, so a wildcard origin does not enable CSRF. The
		// only cookie (the refresh token) is SameSite=Strict, scoped to /api/v1/auth, never allowed
		// with credentials (no Access-Control-Allow-Credentials), and the endpoints acting on it
		// require X-Requested-With, deliberately absent from Allow-Headers, so a cross-origin
		// preflight for it fails (session.go cookieGuard).
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key, If-Match, If-None-Match, Cache-Control, X-Request-ID")
		c.Header("Access-Control-Expose-Headers", "ETag, Location, Retry-After, X-Request-ID, X-Instance-ID, X-Next-Cursor")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
