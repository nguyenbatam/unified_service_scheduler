package httpapi

import (
	"context"
	"errors"
	"time"

	"github.com/gin-gonic/gin"
)

// DefaultRequestTimeout bounds one request end to end (config REQUEST_TIMEOUT). It must exceed the
// pool's statement_timeout, so a single slow statement still surfaces as Postgres' own 57014, and
// stay below the HTTP server's WriteTimeout, so the 503 still reaches the client.
const DefaultRequestTimeout = 20 * time.Second

// WithRequestTimeout sets the per-request deadline (default DefaultRequestTimeout; 0 disables).
// Every database wait (pool acquire included) honours it, so a request never queues behind a
// stuck lock or a full pool longer than this: it answers 503 busy + Retry-After instead.
func WithRequestTimeout(d time.Duration) Option {
	return func(s *Server) { s.requestTimeout = max(d, 0) }
}

// deadline gives every request a context deadline of s.requestTimeout. The WebSocket (a long-lived
// connection whose handshake reads the user itself) and /metrics are left alone.
func (s *Server) deadline() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.requestTimeout <= 0 {
			c.Next()
			return
		}
		switch c.Request.URL.Path {
		case "/api/v1/ws/dispatch", "/metrics":
			c.Next()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), s.requestTimeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// statusClientClosed is nginx's "client closed request": the client went away before the answer,
// so nobody reads it. It keeps a disconnect out of the 5xx series and alerts.
const statusClientClosed = 499

// requestEnd says why a request's context ended, if it did.
type requestEnd int

const (
	requestLive     requestEnd = iota
	requestCanceled            // the client disconnected (net/http cancels the request context)
	requestTimedOut            // the deadline middleware's REQUEST_TIMEOUT ran out
)

// requestEnded classifies ctx. Only the client's disconnect cancels a request context (the server
// never cancels in-flight requests: Shutdown waits for them), and only the deadline middleware sets
// a deadline on it, so Canceled means "client gone" and DeadlineExceeded means "our budget".
func requestEnded(ctx context.Context) requestEnd {
	switch err := ctx.Err(); {
	case err == nil:
		return requestLive
	case errors.Is(err, context.DeadlineExceeded):
		return requestTimedOut
	default:
		return requestCanceled
	}
}
