package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func (s *Server) ok(c *gin.Context, body any) {
	c.JSON(http.StatusOK, body)
}

// result writes err, or body as 200.
func (s *Server) result(c *gin.Context, body any, err error) {
	s.reply(c, http.StatusOK, body, 0, err)
}

// reply writes err, or body with status plus, for a versioned row (version > 0), its ETag.
func (s *Server) reply(c *gin.Context, status int, body any, version int, err error) {
	if err != nil {
		s.fail(c, err)
		return
	}
	setETag(c, version)
	c.JSON(status, body)
}

// writeError writes the standard error envelope and aborts the chain.
func writeError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

// busyMessage is the 503 message for every database wait that ran out (lock_timeout,
// statement_timeout, or the request deadline).
const busyMessage = "the dealership is busy; retry in a moment"

func (s *Server) fail(c *gin.Context, err error) {
	ctx := c.Request.Context()
	status := http.StatusInternalServerError
	code := domain.CodeInternal
	message := "internal error"
	if domainErr, ok := domain.AsError(err); ok {
		code = domainErr.Code
		message = domainErr.Message
		status = statusFor(code)
		if status != http.StatusInternalServerError && err.Error() != message {
			// Wrapped client errors carry server-side detail (e.g. the CHECK constraint behind a
			// 400): log it, send only the message.
			s.log.Info("request rejected", "err", err, "status", status, "path", c.Request.URL.Path, "request_id", requestID(ctx))
		}
	}
	if status == http.StatusInternalServerError {
		// Never send an internal (or unmapped) domain error's message: it may carry
		// implementation detail. What happened to the request decides how it is reported.
		code, message = domain.CodeInternal, "internal error"
		switch requestEnded(ctx) {
		case requestCanceled:
			// The client went away (a 57014 or a context error from pgx is the consequence, not a
			// fault): nobody reads the answer, so it is neither a 500 nor an error log.
			s.log.Debug("request canceled by the client", "err", err, "path", c.Request.URL.Path, "request_id", requestID(ctx))
			c.AbortWithStatus(statusClientClosed)
			return
		case requestTimedOut:
			// REQUEST_TIMEOUT ran out (pool acquire, lock waits, many statements): retry with the
			// same Idempotency-Key, which replays a booking that committed after all.
			s.log.Warn("request deadline exceeded", "err", err, "path", c.Request.URL.Path, "request_id", requestID(ctx))
			code, message, status = domain.CodeBusy, busyMessage, http.StatusServiceUnavailable
		default:
			if store.IsLockTimeout(err) {
				// lock_timeout / statement_timeout: e.g. an admin change holds the dealership row.
				s.log.Warn("database busy", "err", err, "path", c.Request.URL.Path, "request_id", requestID(ctx))
				code, message, status = domain.CodeBusy, busyMessage, http.StatusServiceUnavailable
			} else {
				s.log.Error("request failed", "err", err, "path", c.Request.URL.Path, "request_id", requestID(ctx))
			}
		}
	}
	switch {
	case status == http.StatusServiceUnavailable:
		c.Header("Retry-After", "1")
	case status == http.StatusTooManyRequests && c.Writer.Header().Get("Retry-After") == "":
		// The limiter sets the bucket's window; anything else rate limited waits one window.
		c.Header("Retry-After", strconv.Itoa(int(userWindow.Seconds())))
	}
	writeError(c, status, code, message)
}

// maxBodyBytes caps every JSON request body.
const maxBodyBytes = 1 << 20

// codeTooLarge is the error code of a 413 (domain has no code for it: only bindJSON produces it).
const codeTooLarge = "too_large"

// bindJSON decodes exactly one JSON value into dst: unknown fields and anything but whitespace
// after the value are 400, a body over maxBodyBytes is 413.
func (s *Server) bindJSON(c *gin.Context, dst any, invalidMsg string) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if err = dec.Decode(&json.RawMessage{}); errors.Is(err, io.EOF) {
			return true
		}
		if err == nil {
			err = errors.New("trailing data after the JSON value")
		}
	}
	if tooLarge := (*http.MaxBytesError)(nil); errors.As(err, &tooLarge) {
		writeError(c, http.StatusRequestEntityTooLarge, codeTooLarge, "request body must be at most 1 MiB")
		return false
	}
	s.fail(c, domain.NewError(domain.CodeInvalid, invalidMsg))
	return false
}

func (s *Server) pathUUID(c *gin.Context, name, message string) (uuid.UUID, bool) {
	id, err := parseUUID(c.Param(name), message)
	if err != nil {
		s.fail(c, err)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) queryUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := parseUUID(c.Query(name), name+" is required")
	if err != nil {
		s.fail(c, err)
		return uuid.Nil, false
	}
	return id, true
}

func (s *Server) optionalQueryUUID(c *gin.Context, name string) (uuid.UUID, bool) {
	raw := c.Query(name)
	if raw == "" {
		return uuid.Nil, true
	}
	id, err := parseUUID(raw, name+" is invalid")
	if err != nil {
		s.fail(c, err)
		return uuid.Nil, false
	}
	return id, true
}

// optionalQueryBool reads boolean query parameter name (strconv.ParseBool: true/false, 1/0); given
// is false when it is absent. A value that is not a boolean is answered 400 and ok is false.
func (s *Server) optionalQueryBool(c *gin.Context, name string) (value, given, ok bool) {
	raw := c.Query(name)
	if raw == "" {
		return false, false, true
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		s.fail(c, domain.NewError(domain.CodeInvalid, name+" must be true or false"))
		return false, false, false
	}
	return value, true, true
}

// optionalUUID parses raw, treating "" as uuid.Nil.
func optionalUUID(raw, message string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, nil
	}
	return parseUUID(raw, message)
}

func parseUUID(raw, message string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, domain.NewError(domain.CodeInvalid, message)
	}
	return id, nil
}

func statusFor(code string) int {
	switch code {
	case domain.CodeInvalid:
		return http.StatusBadRequest
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeConflict:
		return http.StatusConflict
	case domain.CodeUnavailable:
		return http.StatusUnprocessableEntity
	case domain.CodeUnauthorized:
		return http.StatusUnauthorized
	case domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodePrecondition:
		return http.StatusPreconditionFailed
	case domain.CodePreconditionRequired:
		return http.StatusPreconditionRequired
	case domain.CodeRateLimited:
		return http.StatusTooManyRequests
	case domain.CodeBusy:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// setETag exposes the row version so clients can send it back as If-Match.
func setETag(c *gin.Context, version int) {
	if version > 0 {
		c.Header("ETag", `"`+strconv.Itoa(version)+`"`)
	}
}

// ifMatch parses an optional If-Match header; 0 means absent or "*". Only the strong ETag this
// API sends ("3", quotes included) is accepted, alone or as a one-element list; anything else
// (bare 3, weak W/"3", "3, lists of several versions) is 400.
func (s *Server) ifMatch(c *gin.Context) (int, bool) {
	n, err := parseIfMatch(c.GetHeader("If-Match"))
	if err != nil {
		s.fail(c, err)
		return 0, false
	}
	return n, true
}

func parseIfMatch(header string) (int, error) {
	invalid := domain.NewError(domain.CodeInvalid, `If-Match must be the quoted version from ETag, e.g. "3"`)
	var tags []string
	for tag := range strings.SplitSeq(header, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	switch {
	case len(tags) == 0:
		return 0, nil
	case len(tags) > 1:
		return 0, invalid
	case tags[0] == "*":
		return 0, nil
	}
	tag := tags[0]
	if len(tag) < 3 || len(tag) > 12 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return 0, invalid
	}
	n, err := strconv.Atoi(tag[1 : len(tag)-1])
	if err != nil || n < 1 || strconv.Itoa(n) != tag[1:len(tag)-1] {
		return 0, invalid
	}
	return n, nil
}
