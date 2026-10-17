package httpapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
)

func (s *Server) serveWS(c *gin.Context) {
	// The deadline middleware skips the socket (it lives for hours), so bound the handshake's own
	// database work here: a reconnect storm on a saturated pool must not queue on Acquire forever.
	session, err := s.authenticateWSWithin(c)
	if errors.Is(err, errSocketCap) {
		s.hub.RefuseFull(c.Writer)
		return
	}
	if err != nil {
		s.fail(c, err)
		return
	}
	s.hub.ServeWS(c.Writer, c.Request, session)
}

// authenticateWSWithin runs authenticateWS under the request timeout. Only the handshake: the
// upgraded socket never reads the request context.
func (s *Server) authenticateWSWithin(c *gin.Context) (realtime.Session, error) {
	if s.requestTimeout <= 0 {
		return s.authenticateWS(c)
	}
	req := c.Request
	ctx, cancel := context.WithTimeout(req.Context(), s.requestTimeout)
	defer cancel()
	c.Request = req.WithContext(ctx)
	defer func() { c.Request = req }()
	session, err := s.authenticateWS(c)
	if err != nil && ctx.Err() == context.DeadlineExceeded && req.Context().Err() == nil {
		// Our own budget ran out (fail sees the restored, live request): busy, like any request.
		return session, domain.NewError(domain.CodeBusy, "the scheduler is busy; retry in a moment")
	}
	return session, err
}

// authenticateWS accepts a single-use ?ticket=<ws ticket> from POST /auth/ws-ticket (browsers
// cannot set headers on WebSocket), or, for staff service clients only (stress, ops tools),
// Authorization: Bearer <access token>. Customers must use tickets, so every customer socket is
// minted through the throttled ticket endpoint. Optional ?dealershipId= sets the initial filter.
func (s *Server) authenticateWS(c *gin.Context) (realtime.Session, error) {
	var claims auth.Claims
	var err error
	bearer := auth.Bearer(c.GetHeader("Authorization"))
	if bearer != "" {
		claims, err = s.parseToken(bearer, auth.AudienceAPI)
	} else {
		claims, err = s.parseToken(strings.TrimSpace(c.Query("ticket")), auth.AudienceWS)
	}
	if err != nil {
		return realtime.Session{}, err
	}
	session := realtime.Session{
		UserID:       claims.UserID,
		Role:         claims.Role,
		CustomerID:   claims.CustomerID,
		TokenVersion: claims.TokenVersion,
		SessionID:    claims.SessionID,
		ExpiresAt:    claims.SessionExpiry(),
	}
	if s.loadUser != nil {
		// Same rule as requireAuth, but always a fresh read (sockets are long-lived and opened
		// rarely): disabled, demoted, or signed-out users get no events.
		user, err := s.currentUser(c.Request.Context(), claims, false)
		if err != nil {
			return realtime.Session{}, err
		}
		session.Role, session.CustomerID, session.Scope = user.Role, user.CustomerID, user.DealershipID
	}
	if bearer != "" && !session.Role.IsStaff() {
		return realtime.Session{}, domain.NewError(domain.CodeForbidden, "open the socket with a ticket from POST /api/v1/auth/ws-ticket")
	}
	if raw := c.Query("dealershipId"); raw != "" {
		id, err := parseUUID(raw, "dealershipId is invalid")
		if err != nil {
			return realtime.Session{}, err
		}
		session.DealershipID = id
	}
	if !session.Role.Valid() {
		return realtime.Session{}, domain.NewError(domain.CodeUnauthorized, "invalid token role")
	}
	if s.hub != nil && s.hub.Full(session.UserID) {
		return realtime.Session{}, errSocketCap // before the ticket is burned, like every refusal here
	}
	if bearer == "" {
		// Burned last: a request refused above (a transient 503 reading the user, a bad
		// dealershipId, the per-user socket cap) leaves the ticket usable for the client's retry
		// within its 60 s.
		if err := s.redeemTicket(c.Request.Context(), claims); err != nil {
			return realtime.Session{}, err
		}
	}
	return session, nil
}

// errSocketCap refuses a handshake over the hub's per-user socket cap (429 + Retry-After).
var errSocketCap = errors.New("too many connections")

// redeemTicket burns the ticket's id so a leaked URL (logs, history) cannot open a second socket.
// The marker lives as long as the ticket could still parse. If Redis is down or slow this fails
// open: the ticket stays valid until it expires (60 s), as before single-use tickets.
func (s *Server) redeemTicket(ctx context.Context, claims auth.Claims) error {
	if claims.ID == "" || claims.ExpiresAt == nil {
		return domain.NewError(domain.CodeUnauthorized, "invalid or expired token")
	}
	if s.tickets == nil {
		return nil
	}
	ttl := max(time.Until(claims.ExpiresAt.Time)+time.Second, time.Second)
	first, err := s.tickets.Redeem(ctx, claims.ID, ttl)
	if err != nil {
		s.log.Warn("ws ticket store unavailable; accepting ticket", "err", err, "request_id", requestID(ctx))
		return nil
	}
	if !first {
		return domain.NewError(domain.CodeUnauthorized, "ticket already used; request a new one")
	}
	return nil
}

// validateSessions is the hub's periodic re-check, one round trip for every connected socket: the
// user must still be active, with the same role, customer link, dealership scope, and token
// version the socket was opened with, and the refresh family it was opened for (if any) must still
// be live — a new login replacing it, or any revocation, closes it here. A database error keeps
// every socket (next sweep retries).
func (s *Server) validateSessions(ctx context.Context, sessions []realtime.Session) ([]bool, error) {
	ids := make([]uuid.UUID, 0, len(sessions))
	sids := make([]uuid.UUID, 0, len(sessions))
	seen := make(map[uuid.UUID]struct{}, 2*len(sessions))
	for _, session := range sessions {
		if _, ok := seen[session.UserID]; !ok {
			seen[session.UserID] = struct{}{}
			ids = append(ids, session.UserID)
		}
		if _, ok := seen[session.SessionID]; session.SessionID != uuid.Nil && !ok {
			seen[session.SessionID] = struct{}{}
			sids = append(sids, session.SessionID)
		}
	}
	users, liveFamilies, err := s.store.FindUsersAndSessions(ctx, ids, sids)
	if err != nil {
		return nil, err
	}
	valid := make([]bool, len(sessions))
	for i, session := range sessions {
		user, ok := users[session.UserID]
		valid[i] = ok && user.TokenVersion == session.TokenVersion && user.Role == session.Role &&
			user.CustomerID == session.CustomerID && user.DealershipID == session.Scope &&
			(session.SessionID == uuid.Nil || liveFamilies[session.SessionID])
	}
	return valid, nil
}
