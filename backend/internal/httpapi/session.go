package httpapi

import (
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// Login sessions: a short-lived access token (JWT, Bearer) plus a rotating refresh token in an
// HttpOnly cookie (store.RefreshSession: rotation, a short grace for racing tabs, reuse detection).
// The cookie is SameSite=Strict. With AUTH_COOKIE_SECURE (the default) it is __Host-uss_refresh:
// Secure, Path=/, no Domain, so no sibling subdomain or plain-http page can set one this host
// would read (cookie tossing); a plain-http stack uses uss_refresh scoped to /api/v1/auth. Only
// the configured name is read, and a request carrying it twice is refused (never "pick one").
// The two endpoints that act on the cookie alone (refresh, logout without a Bearer) also verify
// Origin and require X-Requested-With: fetch, a header no cross-site form can send and that CORS
// never allows (it is not in Access-Control-Allow-Headers), so a cross-site page cannot use the
// cookie even where SameSite is not enforced; Sec-Fetch-Site: cross-site is refused on every auth
// write.
const (
	// RefreshCookieName is the refresh token cookie on a plain-http stack (AUTH_COOKIE_SECURE=false),
	// scoped to RefreshCookiePath.
	RefreshCookieName = "uss_refresh"
	// RefreshCookiePath limits the plain-http cookie to the auth endpoints.
	RefreshCookiePath = "/api/v1/auth"
	// SecureRefreshCookieName is the cookie with AUTH_COOKIE_SECURE=true: the __Host- prefix makes
	// browsers require Secure, Path=/ and no Domain attribute.
	SecureRefreshCookieName = "__Host-uss_refresh"
	// CSRFHeader / CSRFHeaderValue must accompany every request that authenticates with the cookie.
	CSRFHeader      = "X-Requested-With"
	CSRFHeaderValue = "fetch"
	// DefaultRefreshLimitPerIP caps POST /auth/refresh per client IP per minute.
	DefaultRefreshLimitPerIP = 60
	refreshWindow            = time.Minute
)

// SessionConfig sets the refresh session lifetimes and cookie (WithSessions).
type SessionConfig struct {
	// IdleTTL: each refresh token expires this long after it was issued.
	IdleTTL time.Duration
	// MaxTTL: the login session's absolute end; refreshing never extends it.
	MaxTTL time.Duration
	// CookieSecure sets Secure on the cookie (off only for a plain-http local stack).
	CookieSecure bool
	// RefreshLimitPerIP caps POST /auth/refresh per client IP per minute (fail-open).
	RefreshLimitPerIP int
}

// DefaultSessionConfig is the production default (config.Load reads the same from the environment).
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		IdleTTL: auth.DefaultSessionIdleTTL, MaxTTL: auth.DefaultSessionMaxTTL,
		CookieSecure: true, RefreshLimitPerIP: DefaultRefreshLimitPerIP,
	}
}

// WithSessions sets the refresh session lifetimes, the cookie's Secure flag (taken as given), and
// the refresh throttle. Zero durations and limits keep the defaults; config validates the real
// values.
func WithSessions(cfg SessionConfig) Option {
	return func(s *Server) {
		if cfg.IdleTTL > 0 {
			s.sessions.IdleTTL = cfg.IdleTTL
		}
		if cfg.MaxTTL > 0 {
			s.sessions.MaxTTL = cfg.MaxTTL
		}
		if cfg.RefreshLimitPerIP > 0 {
			s.sessions.RefreshLimitPerIP = cfg.RefreshLimitPerIP
		}
		s.sessions.CookieSecure = cfg.CookieSecure
		s.sessions.MaxTTL = max(s.sessions.MaxTTL, s.sessions.IdleTTL)
	}
}

// WithAllowedOrigins lists browser origins (scheme://host[:port]) the cookie endpoints accept
// besides the request's own origin (ALLOWED_ORIGINS, shared with the WebSocket check).
func WithAllowedOrigins(origins []string) Option {
	return func(s *Server) {
		s.allowedOrigins = make([]string, 0, len(origins))
		for _, o := range origins {
			s.allowedOrigins = append(s.allowedOrigins, strings.ToLower(strings.TrimSuffix(o, "/")))
		}
	}
}

func newRefreshCounter(constLabels prometheus.Labels) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Name:        "scheduler_auth_refresh_total",
		Help:        "POST /auth/refresh by result (rotated, grace, reused, revoked, expired, unknown, inactive, missing, duplicate, forbidden, rate_limited, error).",
		ConstLabels: constLabels,
	}, []string{"result"})
}

// refresh trades the refresh cookie for a new access token, rotating the cookie.
func (s *Server) refresh(c *gin.Context) {
	if !s.cookieGuard(c) {
		s.refreshes.WithLabelValues("forbidden").Inc()
		return
	}
	if !s.allow(c, s.refreshBucket(c)) {
		s.refreshes.WithLabelValues("rate_limited").Inc()
		return
	}
	presented, state := s.refreshCookie(c)
	if state != cookiePresent || s.store == nil {
		result := "missing"
		if state == cookieDuplicate {
			result = "duplicate"
		}
		s.refreshes.WithLabelValues(result).Inc()
		s.sessionEnded(c)
		return
	}
	// The successor is derived from the presented token, so a grace answer can send it again.
	token, hash := s.auth.SuccessorRefreshToken(presented)
	// Every verdict (grace, reuse, expiry) and stored time is the database clock's: replicas whose
	// own clocks drift still agree (store.RefreshSession).
	res, err := s.store.RefreshSession(c.Request.Context(), store.RefreshRequest{
		TokenHash: auth.HashRefreshToken(presented), NewHash: hash, UserAgent: c.Request.UserAgent(),
		IdleTTL: s.sessions.IdleTTL, Grace: auth.RefreshReuseGrace,
	})
	if err != nil {
		// Transient (busy, client gone): the cookie is kept for the retry.
		s.refreshes.WithLabelValues("error").Inc()
		s.fail(c, err)
		return
	}
	switch res.Outcome {
	case store.RefreshRotated:
		s.refreshes.WithLabelValues("rotated").Inc()
		s.setRefreshCookie(c, token, res.Session.ExpiresAt, res.Now)
		s.writeAccess(c, http.StatusOK, res.User, res.Session)
	case store.RefreshGrace:
		// Rotated moments ago (a racing tab, or a refresh whose response was lost): no second
		// rotation; the same successor cookie again while it is the family's live token, so the
		// client holds it even if the winner's response never arrived.
		s.refreshes.WithLabelValues("grace").Inc()
		if res.Resend {
			s.setRefreshCookie(c, token, res.Session.ExpiresAt, res.Now)
		}
		s.writeAccess(c, http.StatusOK, res.User, res.Session)
	case store.RefreshReused:
		s.refreshes.WithLabelValues("reused").Inc()
		s.InvalidateUser(res.User.ID)
		s.log.Warn("refresh token reuse detected: every session of the user revoked",
			"user_id", res.User.ID, "family_id", res.Session.FamilyID, "request_id", requestID(c.Request.Context()))
		s.sessionEnded(c)
	case store.RefreshInactive:
		s.refreshes.WithLabelValues("inactive").Inc()
		s.InvalidateUser(res.User.ID)
		s.sessionEnded(c)
	case store.RefreshExpired:
		s.refreshes.WithLabelValues("expired").Inc()
		s.sessionEnded(c)
	case store.RefreshRevoked:
		s.refreshes.WithLabelValues("revoked").Inc()
		s.sessionEnded(c)
	default:
		s.refreshes.WithLabelValues("unknown").Inc()
		s.sessionEnded(c)
	}
}

// startSession starts a refresh family for user (login, register): revokes the family of the
// refresh cookie the request carried, if any, sets the new cookie, and answers with an access
// token for the new family.
func (s *Server) startSession(c *gin.Context, user domain.User, status int) {
	token, hash, err := auth.NewRefreshToken()
	if err != nil {
		s.fail(c, err)
		return
	}
	var replace []byte
	if old, state := s.refreshCookie(c); state == cookiePresent {
		replace = auth.HashRefreshToken(old)
	}
	session, err := s.store.StartSession(c.Request.Context(), store.NewSession{
		UserID: user.ID, TokenHash: hash, UserAgent: c.Request.UserAgent(),
		IdleTTL: s.sessions.IdleTTL, MaxTTL: s.sessions.MaxTTL, Replace: replace,
	})
	if err != nil {
		s.fail(c, err)
		return
	}
	if session.Replaced != uuid.Nil && s.families != nil {
		s.families.invalidate(session.Replaced) // its access tokens stop here at once
	}
	s.setRefreshCookie(c, token, session.ExpiresAt, session.IssuedAt) // both database times
	s.writeAccess(c, status, user, session)
}

// writeAccess answers with an access token for session's family.
func (s *Server) writeAccess(c *gin.Context, status int, user domain.User, session store.Session) {
	token, exp, err := s.auth.IssueSession(user, session.FamilyID, session.FamilyExpiresAt)
	if err != nil {
		s.fail(c, err)
		return
	}
	user.PasswordHash = ""
	c.Header("Cache-Control", "no-store")
	c.JSON(status, authResponse{
		Token:            token,
		ExpiresAt:        exp.UTC().Format(time.RFC3339),
		SessionExpiresAt: session.FamilyExpiresAt.UTC().Truncate(time.Second).Format(time.RFC3339),
		User:             user,
	})
}

// logout revokes every token of the caller (all devices/tabs, every refresh family), not just the
// one sent, and clears the cookie. The caller is identified by a valid Bearer token or, failing
// that, a live refresh cookie (with the cookie guard).
func (s *Server) logout(c *gin.Context) {
	if !s.notCrossSite(c) {
		return
	}
	id, ok := s.logoutUser(c)
	if !ok {
		return
	}
	err := s.store.RevokeTokens(c.Request.Context(), id)
	// Drop the cached row even on error: the bump may have committed before the error surfaced.
	// Other instances learn of it from SESSION_REVOKED (or within the cache TTL).
	s.InvalidateUser(id)
	if err != nil {
		s.fail(c, err) // the cookie is kept for the retry
		return
	}
	s.clearRefreshCookie(c)
	c.Status(http.StatusNoContent)
}

// logoutUser is the user a logout acts for, or false after writing the refusal.
func (s *Server) logoutUser(c *gin.Context) (uuid.UUID, bool) {
	if bearer := auth.Bearer(c.GetHeader("Authorization")); bearer != "" && s.store != nil {
		if claims, err := s.parseToken(bearer, auth.AudienceAPI); err == nil {
			user, err := s.currentUser(c.Request.Context(), claims, true)
			if err == nil {
				return user.ID, true
			}
			if e, ok := domain.AsError(err); !ok || e.Code != domain.CodeUnauthorized {
				s.fail(c, err)
				return uuid.Nil, false
			}
		}
		// An expired or signed-out access token: the cookie may still identify the session.
	}
	presented, state := s.refreshCookie(c)
	if state != cookiePresent || s.store == nil {
		s.sessionEnded(c)
		return uuid.Nil, false
	}
	// Cookie-only logout: the CSRF guard, and the refresh endpoint's per-IP budget (it is the same
	// lookup an attacker could use to probe tokens).
	if !s.cookieGuard(c) || !s.allow(c, s.refreshBucket(c)) {
		return uuid.Nil, false
	}
	id, live, err := s.store.SessionUser(c.Request.Context(), auth.HashRefreshToken(presented))
	if err != nil {
		s.fail(c, err)
		return uuid.Nil, false
	}
	if !live {
		s.sessionEnded(c)
		return uuid.Nil, false
	}
	return id, true
}

// sessionEnded answers 401 and clears the refresh cookie.
func (s *Server) sessionEnded(c *gin.Context) {
	s.clearRefreshCookie(c)
	s.fail(c, domain.NewError(domain.CodeUnauthorized, "session expired or signed out; please log in again"))
}

// cookieGuard is the CSRF check of a request authenticated by the refresh cookie: its Origin (when
// sent) must be the request's own origin or allowlisted, and it must carry X-Requested-With: fetch.
// It writes 403 and returns false otherwise.
func (s *Server) cookieGuard(c *gin.Context) bool {
	if !s.notCrossSite(c) {
		return false
	}
	if !s.originAllowed(c) {
		s.fail(c, domain.NewError(domain.CodeForbidden, "cross-origin request refused"))
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.GetHeader(CSRFHeader)), CSRFHeaderValue) {
		s.fail(c, domain.NewError(domain.CodeForbidden, CSRFHeader+": "+CSRFHeaderValue+" header is required"))
		return false
	}
	return true
}

// originGuard writes 403 for a browser request from a foreign origin (login, register: they set
// the session cookie, so a cross-site form must not sign the browser into another account).
func (s *Server) originGuard(c *gin.Context) bool {
	if !s.notCrossSite(c) {
		return false
	}
	if s.originAllowed(c) {
		return true
	}
	s.fail(c, domain.NewError(domain.CodeForbidden, "cross-origin request refused"))
	return false
}

// notCrossSite writes 403 for a request the browser marks Sec-Fetch-Site: cross-site (sent by
// every current browser, even where Origin is not, e.g. some navigations); other values and
// non-browser clients (no header) pass.
func (s *Server) notCrossSite(c *gin.Context) bool {
	if !strings.EqualFold(strings.TrimSpace(c.GetHeader("Sec-Fetch-Site")), "cross-site") {
		return true
	}
	s.fail(c, domain.NewError(domain.CodeForbidden, "cross-site request refused"))
	return false
}

// jsonContentType writes 400 unless the request declares a JSON body (Content-Type
// application/json, parameters such as charset allowed): login and register, so an HTML form
// (text/plain, urlencoded, multipart) cannot post a forged JSON body to them.
func (s *Server) jsonContentType(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err == nil && mediaType == "application/json" {
		return true
	}
	s.fail(c, domain.NewError(domain.CodeInvalid, "Content-Type must be application/json"))
	return false
}

// refreshBucket is the per-client-IP budget of the cookie endpoints (refresh, cookie-only logout).
func (s *Server) refreshBucket(c *gin.Context) cache.Bucket {
	return cache.Bucket{Key: "refresh:ip:" + ipBucket(c), Limit: s.sessions.RefreshLimitPerIP, Window: refreshWindow}
}

// originAllowed: no Origin (a non-browser client), an allowlisted origin (ALLOWED_ORIGINS), or the
// request's own origin — scheme (X-Forwarded-Proto from a trusted proxy, else the connection) and
// Host, the same host rule as the WebSocket's check.
func (s *Server) originAllowed(c *gin.Context) bool {
	origin := c.GetHeader("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false // includes "null" (sandboxed frames, some redirects)
	}
	o := strings.ToLower(u.Scheme + "://" + u.Host)
	if slices.Contains(s.allowedOrigins, o) {
		return true
	}
	return o == strings.ToLower(s.requestScheme(c)+"://"+c.Request.Host)
}

// requestScheme is the scheme the client used: X-Forwarded-Proto (first value) when the TCP peer
// is a trusted proxy, otherwise that of this connection.
func (s *Server) requestScheme(c *gin.Context) string {
	if proto := c.GetHeader("X-Forwarded-Proto"); proto != "" && s.fromTrustedProxy(c) {
		proto = strings.ToLower(strings.TrimSpace(strings.Split(proto, ",")[0]))
		if proto == "http" || proto == "https" {
			return proto
		}
	}
	if c.Request.TLS != nil {
		return "https"
	}
	return "http"
}

// fromTrustedProxy reports whether the TCP peer is one of TRUSTED_PROXIES (the same list gin
// trusts for X-Forwarded-For).
func (s *Server) fromTrustedProxy(c *gin.Context) bool {
	addr, err := netip.ParseAddrPort(c.Request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := addr.Addr().Unmap()
	for _, p := range s.trustedNets {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// trustedNets parses proxies (IPs or CIDRs, validated by config); invalid entries trust nobody.
func trustedNets(proxies []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(proxies))
	for _, entry := range proxies {
		if p, err := netip.ParsePrefix(entry); err == nil {
			out = append(out, p.Masked())
			continue
		}
		if a, err := netip.ParseAddr(entry); err == nil {
			a = a.Unmap()
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}

// cookieState is what a request carries under the configured refresh cookie name.
type cookieState int

const (
	cookieAbsent    cookieState = iota // none, or not a well-formed token
	cookiePresent                      // exactly one, well-formed
	cookieDuplicate                    // more than one: refused, never one picked
)

// RefreshCookieName is the refresh cookie's name in this configuration (SecureRefreshCookieName
// with Secure cookies, else RefreshCookieName).
func (s *Server) RefreshCookieName() string {
	if s.sessions.CookieSecure {
		return SecureRefreshCookieName
	}
	return RefreshCookieName
}

// refreshCookiePath is "/" for the __Host- cookie (the prefix requires it), else the auth path.
func (s *Server) refreshCookiePath() string {
	if s.sessions.CookieSecure {
		return "/"
	}
	return RefreshCookiePath
}

// refreshCookie is the request's refresh token under the configured name only. Two cookies of
// that name (one set for another path or a parent domain: cookie tossing) are cookieDuplicate:
// the request cannot tell which is genuine, so the caller refuses it.
func (s *Server) refreshCookie(c *gin.Context) (string, cookieState) {
	name := s.RefreshCookieName()
	var value string
	n := 0
	for _, ck := range c.Request.Cookies() {
		if ck.Name == name {
			n++
			value = ck.Value
		}
	}
	switch {
	case n > 1:
		return "", cookieDuplicate
	case n == 1 && auth.WellFormedRefreshToken(value):
		return value, cookiePresent
	}
	return "", cookieAbsent
}

func (s *Server) setRefreshCookie(c *gin.Context, token string, expires, now time.Time) {
	http.SetCookie(c.Writer, s.newRefreshCookie(token, expires, now))
}

func (s *Server) clearRefreshCookie(c *gin.Context) {
	http.SetCookie(c.Writer, s.newRefreshCookie("", time.Time{}, time.Time{}))
}

// newRefreshCookie builds the refresh cookie: HttpOnly, SameSite=Strict; with Secure (config)
// __Host-uss_refresh on Path=/, else uss_refresh on Path=/api/v1/auth; Max-Age until expires (the token's idle expiry, already capped at its
// family's end; at least 1 s) counted from now — both database times, never this instance's
// clock. An empty token clears it: same attributes, Max-Age=0.
func (s *Server) newRefreshCookie(token string, expires, now time.Time) *http.Cookie {
	maxAge := -1 // net/http writes Max-Age=0
	if token != "" {
		maxAge = max(int(expires.Sub(now)/time.Second), 1)
	}
	// Secure is configuration (AUTH_COOKIE_SECURE, default true; off only for a plain-http stack).
	return &http.Cookie{ //nolint:gosec // G124: HttpOnly and SameSite=Strict always; Secure per config, above
		Name: s.RefreshCookieName(), Value: token, Path: s.refreshCookiePath(), MaxAge: maxAge,
		HttpOnly: true, Secure: s.sessions.CookieSecure, SameSite: http.SameSiteStrictMode,
	}
}
