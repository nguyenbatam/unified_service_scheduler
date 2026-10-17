package httpapi

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Throttling shared across instances through Redis. Login/register: per client IP, per
// (client IP, email), and per account on failed passwords only. "Client IP" is the address for
// IPv4 and its /64 for IPv6 (one host usually holds a whole /64), see ipBucket. Residual risk,
// accepted and documented: the account bucket is global, so anyone sending
// loginFailLimitPerEmail wrong passwords for an email within loginFailWindow (from enough
// addresses to pass the per-IP limits) makes every login for that account 429 — the owner's
// included — until the window ends. CAPTCHA / step-up verification is the follow-up.
// Self-service reads and writes: per user and per client IP (several accounts behind one
// address share the IP bucket).
const (
	loginLimitPerIP      = 20
	loginLimitPerIPEmail = 10
	loginWindow          = 5 * time.Minute
	// loginFailLimitPerEmail caps wrong passwords for one account from all addresses together.
	// Past it every attempt, right password or not, is 429 until the window ends, so a botnet
	// cannot keep guessing; below it nobody else's failures block the owner's correct login.
	loginFailLimitPerEmail = 50
	loginFailWindow        = 15 * time.Minute

	// Register answers 409 for a taken email (see register), so its limits are what bound probing
	// for accounts: a few sign-ups per client IP, and a cluster-wide cap that also bounds a probe
	// spread over many addresses. The cap is the trade-off: past it every sign-up is 429 until the
	// window ends, legitimate ones included.
	registerLimitPerIP  = 5
	registerLimitGlobal = 300
	registerWindow      = 15 * time.Minute

	wsTicketLimitPerUser = 30
	// Customer search and walk-in records are throttled for staff too (unlike their other desk
	// work): the search reads group-wide PII, so one token must not page through every customer.
	customerSearchLimitPerUser = 120
	customerCreateLimitPerUser = 30
	userWindow                 = time.Minute

	// maxEmailLength is the RFC 5321 path limit; register refuses longer addresses, so a longer
	// login email cannot belong to an account (and is never counted).
	maxEmailLength = 254
)

// authResponse answers login, register, and refresh: the access token, its expiry, and the login
// session's absolute end (the refresh family's; the access token's sexp claim).
type authResponse struct {
	Token            string      `json:"token"`
	ExpiresAt        string      `json:"expiresAt"`
	SessionExpiresAt string      `json:"sessionExpiresAt"`
	User             domain.User `json:"user"`
}

func (s *Server) register(c *gin.Context) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if !s.originGuard(c) || !s.jsonContentType(c) || !s.bindJSON(c, &body, "request body must be register JSON") {
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	name := strings.TrimSpace(body.Name)
	switch {
	case email == "" || !strings.Contains(email, "@") || len(email) > maxEmailLength:
		s.fail(c, domain.NewError(domain.CodeInvalid, "a valid email is required"))
		return
	case len(body.Password) < auth.MinPasswordLength || len(body.Password) > 72:
		s.fail(c, domain.NewError(domain.CodeInvalid, "password must be 8–72 characters"))
		return
	case name == "" || len(name) > 120:
		s.fail(c, domain.NewError(domain.CodeInvalid, "name is required (max 120 characters)"))
		return
	}
	if !s.allow(c,
		cache.Bucket{Key: "register:ip:" + ipBucket(c), Limit: registerLimitPerIP, Window: registerWindow},
		cache.Bucket{Key: "register:global", Limit: registerLimitGlobal, Window: registerWindow}) {
		return
	}
	// A taken email answers 409 "email already registered", which lets anyone test whether an
	// address has an account. Accepted for this demo (one-step sign-up, no real email delivery):
	// the probe is bounded by the register limits above (registerLimitPerIP per client IP and
	// registerLimitGlobal across the cluster per registerWindow; in memory per instance while Redis
	// is down). The fix, if it ever matters, is to answer 202 for both cases and finish sign-up
	// through an emailed link.
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		s.fail(c, err)
		return
	}
	user, err := s.store.CreateCustomerUser(c.Request.Context(), domain.User{
		ID: uuid.New(), Email: email, Name: name, PasswordHash: hash,
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	s.startSession(c, user, http.StatusCreated)
}

func (s *Server) login(c *gin.Context) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !s.originGuard(c) || !s.jsonContentType(c) || !s.bindJSON(c, &body, "request body must be login JSON") {
		return
	}
	email := strings.TrimSpace(strings.ToLower(body.Email))
	if email == "" || len(email) > maxEmailLength {
		// No account can have this email: count the IP only, then fail like a wrong password.
		if s.allow(c, cache.Bucket{Key: "login:ip:" + ipBucket(c), Limit: loginLimitPerIP, Window: loginWindow}) {
			s.fail(c, domain.NewError(domain.CodeUnauthorized, "invalid email or password"))
		}
		return
	}
	ip := ipBucket(c)
	// Keys carry a pseudonym of the email, never the address itself (Redis keys are listed by
	// operators and tooling); the per-instance fallback reuses the same keys.
	account := s.auth.Pseudonym(email)
	if !s.allow(c,
		cache.Bucket{Key: "login:ip:" + ip, Limit: loginLimitPerIP, Window: loginWindow},
		cache.Bucket{Key: "login:ip-email:" + ip + "|" + account, Limit: loginLimitPerIPEmail, Window: loginWindow}) {
		return
	}
	user, err := s.store.FindUserByEmail(c.Request.Context(), email)
	if err != nil {
		if domainErr, ok := domain.AsError(err); !ok || domainErr.Code != domain.CodeNotFound {
			s.fail(c, err)
			return
		}
		user = domain.User{} // unknown email still pays one bcrypt compare (timing)
	}
	passwordOK := auth.CheckPassword(user.PasswordHash, body.Password)
	// The account bucket counts failures only. It is consulted after the compare and answers the
	// same 429 whether the password was right or wrong (a correct one only peeks: locked once the
	// failures reached the limit, exactly when a wrong one would push past it), so a locked
	// account is no oracle.
	failBucket := cache.Bucket{Key: "login:fail:" + account, Limit: loginFailLimitPerEmail, Window: loginFailWindow}
	if passwordOK {
		if !s.notExceeded(c, failBucket) {
			return
		}
	} else if !s.allow(c, failBucket) {
		return
	}
	if !passwordOK {
		s.fail(c, domain.NewError(domain.CodeUnauthorized, "invalid email or password"))
		return
	}
	s.startSession(c, user, http.StatusOK)
}

// ipBucket is the client-IP part of a rate-limit key: the address for IPv4, its /64 prefix for
// IPv6, since a single host is routinely given a whole /64 and could otherwise rotate through
// fresh buckets at will.
func ipBucket(c *gin.Context) string {
	return ipBucketKey(c.ClientIP())
}

func ipBucketKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.WithZone("").Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

// allow counts one hit in every bucket and writes 429 when any is over its limit; when Redis is
// unavailable or slow it fails open (logged) rather than locking everyone out, except for login
// and register buckets, which then count on this instance (see memoryLimiter).
func (s *Server) allow(c *gin.Context, buckets ...cache.Bucket) bool {
	if s.limiter == nil || len(buckets) == 0 {
		return true
	}
	ok, retry, err := s.allowAll(c.Request.Context(), buckets)
	if err != nil {
		s.warnLimiterDown(c, err)
		ok, retry = s.fallbackAllow(buckets)
	}
	if !ok {
		if retry <= 0 { // limiter that cannot tell: the longest window
			for _, b := range buckets {
				retry = max(retry, b.Window)
			}
		}
		s.rateLimited(c, retry)
		return false
	}
	return true
}

// warnLimiterDown logs a shared-limiter failure, at most once per limiterDownLogEvery per
// instance, with the number of failures held back since the previous line.
func (s *Server) warnLimiterDown(c *gin.Context, err error) {
	if ok, suppressed := s.limiterDownLog.take(); ok {
		s.log.Warn("rate limiter unavailable", "err", err, "suppressed", suppressed,
			"request_id", requestID(c.Request.Context()))
	}
}

// rateLimited writes 429 with Retry-After: retry rounded up to whole seconds (at least 1).
func (s *Server) rateLimited(c *gin.Context, retry time.Duration) {
	c.Header("Retry-After", strconv.Itoa(max(1, int((retry+time.Second-1)/time.Second))))
	s.fail(c, domain.NewError(domain.CodeRateLimited, "too many requests; try again shortly"))
}

// notExceeded is allow without counting a hit, for limiters that support it; others count one
// (allow). Fails open like allow.
func (s *Server) notExceeded(c *gin.Context, b cache.Bucket) bool {
	var (
		exceeded bool
		retry    time.Duration
		err      error
	)
	switch peek := s.limiter.(type) {
	case retryPeekLimiter:
		exceeded, retry, err = peek.ExceededRetry(c.Request.Context(), b)
	case peekLimiter:
		exceeded, err = peek.Exceeded(c.Request.Context(), b)
	default:
		return s.allow(c, b)
	}
	if err != nil {
		s.warnLimiterDown(c, err)
		exceeded = s.loginFallback != nil && fallbackBucket(b) && s.loginFallback.exceeded(b)
		if exceeded {
			retry = s.loginFallback.retryAfter([]cache.Bucket{b})
		}
	}
	if exceeded {
		if retry <= 0 {
			retry = b.Window
		}
		s.rateLimited(c, retry)
		return false
	}
	return true
}

// fallbackAllow decides a request whose shared limiter failed: buckets protected by the fallback
// (login, register) are counted in memory; the rest fail open. When refused it also returns the
// time left in the exhausted windows.
func (s *Server) fallbackAllow(buckets []cache.Bucket) (bool, time.Duration) {
	if s.loginFallback == nil {
		return true, 0
	}
	protected := make([]cache.Bucket, 0, len(buckets))
	for _, b := range buckets {
		if fallbackBucket(b) {
			protected = append(protected, b)
		}
	}
	if len(protected) == 0 || s.loginFallback.allowAll(protected) {
		return true, 0
	}
	return false, s.loginFallback.retryAfter(protected)
}

// allowAll counts buckets and reports whether all are within their limits, and when not (if the
// limiter can tell) how long until the exceeded windows end; 0 means unknown.
func (s *Server) allowAll(ctx context.Context, buckets []cache.Bucket) (bool, time.Duration, error) {
	switch l := s.limiter.(type) {
	case retryLimiter:
		return l.AllowAllRetry(ctx, buckets...)
	case batchLimiter:
		ok, err := l.AllowAll(ctx, buckets...)
		return ok, 0, err
	}
	for _, b := range buckets {
		for range b.Hits() {
			if ok, err := s.limiter.Allow(ctx, b.Key, b.Limit, b.Window); err != nil || !ok {
				return ok, 0, err
			}
		}
	}
	return true, 0, nil
}

// throttleRule limits authenticated requests per user and per client IP in userWindow.
type throttleRule struct {
	name    string
	perUser int
	perIP   int
	// staff: also throttle staff. By default staff are trusted internal users whose desk work is
	// bursty (and the stress client), so only self-service users are counted.
	staff bool
	// cost weighs one request (default 1), e.g. a multi-day availability scan.
	cost func(*gin.Context) int
}

// throttle returns middleware enforcing rule; a request of cost n counts n hits in each bucket.
func (s *Server) throttle(rule throttleRule) gin.HandlerFunc {
	return func(c *gin.Context) {
		actor, ok := s.actor(c)
		if !ok {
			return
		}
		if actor.IsStaff() && !rule.staff {
			c.Next()
			return
		}
		cost := 1
		if rule.cost != nil {
			cost = max(rule.cost(c), 1)
		}
		// A cost of n counts n hits in each bucket (one INCRBY per bucket).
		buckets := []cache.Bucket{{Key: rule.name + ":user:" + actor.UserID.String(), Limit: rule.perUser, Window: userWindow, Cost: cost}}
		if rule.perIP > 0 {
			buckets = append(buckets, cache.Bucket{Key: rule.name + ":ip:" + ipBucket(c), Limit: rule.perIP, Window: userWindow, Cost: cost})
		}
		if !s.allow(c, buckets...) {
			return
		}
		c.Next()
	}
}

// nextAvailabilityCost weighs GET /availability/next by the days it scans: one unit per started
// week (default 14 days = 2, the 31-day maximum = 5). Bad input costs 1; the handler rejects it.
func nextAvailabilityCost(c *gin.Context) int {
	days := nextDefaultDays
	if raw := c.Query("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > nextMaxDays {
			return 1
		}
		days = n
	}
	return (days + 6) / 7
}

// me returns the row requireAuth loaded (a server without a store has none: 401).
func (s *Server) me(c *gin.Context) {
	user, ok := currentUserFrom(c)
	if !ok {
		s.fail(c, domain.NewError(domain.CodeUnauthorized, "authentication required"))
		return
	}
	user.PasswordHash = ""
	s.ok(c, user)
}

// wsTicket mints a 60-second ticket for opening the WebSocket, so the access token
// never appears in a URL (proxy and access logs).
func (s *Server) wsTicket(c *gin.Context) {
	actor, ok := s.actor(c)
	if !ok {
		return
	}
	claims, _ := auth.FromContext(c.Request.Context())
	ticket, exp, err := s.auth.IssueSessionTicket(actor, claims.TokenVersion, claims.SessionID, claims.SessionExpiry())
	if err != nil {
		s.fail(c, err)
		return
	}
	c.Header("Cache-Control", "no-store") // a credential, like the auth responses
	s.ok(c, gin.H{"ticket": ticket, "expiresAt": exp.UTC().Format(time.RFC3339)})
}

func (s *Server) parseToken(raw, audience string) (auth.Claims, error) {
	if raw == "" {
		return auth.Claims{}, domain.NewError(domain.CodeUnauthorized, "authentication required")
	}
	claims, err := s.auth.Parse(raw, audience)
	if err != nil {
		return auth.Claims{}, domain.NewError(domain.CodeUnauthorized, "invalid or expired token")
	}
	return claims, nil
}

// requireAuth verifies the token, then reloads the user (through the short per-instance cache) so
// deactivation and role changes take effect within the cache TTL instead of when the token expires.
func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, err := s.parseToken(auth.Bearer(c.GetHeader("Authorization")), auth.AudienceAPI)
		if err != nil {
			s.fail(c, err)
			return
		}
		actor := claims.Actor()
		if s.loadUser != nil {
			user, err := s.currentUser(c.Request.Context(), claims, true)
			if err != nil {
				s.fail(c, err)
				return
			}
			actor = domain.Actor{UserID: user.ID, Role: user.Role, CustomerID: user.CustomerID, DealershipID: user.DealershipID}
			c.Set(currentUserKey, user)
		}
		c.Request = c.Request.WithContext(auth.WithSession(c.Request.Context(), claims, actor))
		c.Next()
	}
}

func (s *Server) requireRole(roles ...domain.Role) gin.HandlerFunc {
	allowed := make(map[domain.Role]struct{}, len(roles))
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(c *gin.Context) {
		actor, ok := s.actor(c)
		if !ok {
			return
		}
		if _, ok := allowed[actor.Role]; !ok {
			s.fail(c, domain.NewError(domain.CodeForbidden, "your role is not allowed to do this"))
			return
		}
		c.Next()
	}
}

// currentUserKey holds the domain.User that requireAuth loaded, so handlers need not re-query.
const currentUserKey = "httpapi.currentUser"

func currentUserFrom(c *gin.Context) (domain.User, bool) {
	v, ok := c.Get(currentUserKey)
	if !ok {
		return domain.User{}, false
	}
	user, ok := v.(domain.User)
	return user, ok
}

// currentUser reloads the token's user and rejects disabled accounts and revoked sessions, so
// deactivation, role changes, and logout apply on the next request (cached: within the TTL;
// logout on this instance invalidates at once). The token-version check runs on the cached row
// too, so a cached entry never outlives a revocation by more than the TTL. A token minted for a
// refresh family (sid) also needs that family live: a new login replacing it on the browser (or
// any revocation) ends its access tokens within the TTL, with no extra query
// (store.FindUserSession reads the user and the family together).
func (s *Server) currentUser(ctx context.Context, claims auth.Claims, cached bool) (domain.User, error) {
	user, live, err := s.lookupUser(ctx, claims, cached)
	if err != nil {
		if domainErr, ok := domain.AsError(err); ok && domainErr.Code == domain.CodeNotFound {
			return domain.User{}, domain.NewError(domain.CodeUnauthorized, "account is disabled or no longer exists")
		}
		return domain.User{}, err
	}
	if user.TokenVersion != claims.TokenVersion {
		// This token is older than the row (signed out) or newer (the cached row predates a fresh
		// login after a sign-out elsewhere): drop the entry so a newer token gets a fresh read.
		if s.users != nil && user.TokenVersion < claims.TokenVersion {
			s.users.invalidate(user.ID)
			if user, live, err = s.lookupUser(ctx, claims, false); err == nil && user.TokenVersion == claims.TokenVersion && live {
				return user, nil
			}
		}
		return domain.User{}, domain.NewError(domain.CodeUnauthorized, "session was signed out; please log in again")
	}
	if !live {
		return domain.User{}, domain.NewError(domain.CodeUnauthorized, "session was signed out; please log in again")
	}
	return user, nil
}

// lookupUser loads the claims' user and whether their refresh family is live (true for a token
// without one), through the per-instance caches when cached: a hit on both costs no query, any
// miss one query for both.
func (s *Server) lookupUser(ctx context.Context, claims auth.Claims, cached bool) (domain.User, bool, error) {
	sid := claims.SessionID
	if sid == uuid.Nil || s.loadUserSession == nil {
		var user domain.User
		var err error
		if cached {
			user, err = s.cachedUser(ctx, claims.UserID)
		} else {
			user, err = s.loadUser(ctx, claims.UserID)
		}
		return user, true, err
	}
	if !cached || s.users == nil {
		return s.loadUserSession(ctx, claims.UserID, sid)
	}
	user, ticket, userHit := s.users.get(claims.UserID)
	live, familyHit := s.families.get(sid)
	if userHit && familyHit {
		return user, live, nil
	}
	readAt := s.families.now()
	user, live, err := s.loadUserSession(ctx, claims.UserID, sid)
	if err != nil {
		return domain.User{}, false, err
	}
	s.users.put(user, ticket)
	s.families.put(sid, live, readAt)
	return user, live, nil
}

// cachedUser returns the user from the per-instance cache, loading it on a miss. Missing or
// disabled users are not cached.
func (s *Server) cachedUser(ctx context.Context, id uuid.UUID) (domain.User, error) {
	if s.users == nil {
		return s.loadUser(ctx, id)
	}
	user, ticket, ok := s.users.get(id)
	if ok {
		return user, nil
	}
	user, err := s.loadUser(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	s.users.put(user, ticket)
	return user, nil
}

// InvalidateUser drops id from this instance's user cache, so the next request re-reads the row.
// Call it for every user change this instance learns of (main wires SESSION_REVOKED events).
func (s *Server) InvalidateUser(id uuid.UUID) {
	if s.users != nil {
		s.users.invalidate(id)
	}
}

// actor returns the authenticated actor or writes 401.
func (s *Server) actor(c *gin.Context) (domain.Actor, bool) {
	actor, ok := auth.ActorFrom(c.Request.Context())
	if !ok {
		s.fail(c, domain.NewError(domain.CodeUnauthorized, "authentication required"))
		return domain.Actor{}, false
	}
	return actor, true
}
