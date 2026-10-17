package httpapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Every list endpoint pages the same way (store/page.go: keyset over a stable, unique order ending
// with the id): the body is a plain JSON array, the next page's token rides in X-Next-Cursor
// (absent on the last page), and the client echoes it back as ?cursor= with the same filters;
// ?limit= sizes the page (each listing has its default and maximum).
//
// The token is opaque and versioned: base64url of a JSON array [version, listing, filter
// fingerprint, keyset values…]. The listing includes its mode (appointments?when=), and the
// fingerprint covers the filters the page was read with, so a token replayed against another
// listing or other filters is refused (400) instead of seeking into a different order. Clients
// must not parse it: the format may change (bump cursorVersion).

const (
	cursorVersion    = "1"
	nextCursorHeader = "X-Next-Cursor"
	// maxCursorLength bounds what a decode looks at. The longest keyset (vehicles: owner name,
	// make, model, id) at the names' maximum lengths in the characters JSON escapes to 6 bytes
	// (control characters, U+2028) takes about 2 KiB; TestLongestPageTokenFits derives that worst
	// case from the booking limits, so raising one past this bound fails there, not in production.
	maxCursorLength = 4096
)

func invalidCursor() error {
	return domain.NewError(domain.CodeInvalid, "cursor is invalid; use the X-Next-Cursor value of the previous page with the same filters")
}

// pageScope is one listing read with one set of filters: the context a token is valid in.
type pageScope struct {
	listing string
	filter  string
}

// newPageScope names a listing and fingerprints its filters (in a fixed order, "" for unset).
func newPageScope(listing string, filters ...string) pageScope {
	h := sha256.New()
	for _, f := range filters {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return pageScope{listing: listing, filter: hex.EncodeToString(h.Sum(nil)[:8])}
}

// encode makes the token continuing after the row with these keyset values. HTML escaping is
// off (&, <, > stay one byte; the token is base64 anyway), which keeps tokens short.
func (p pageScope) encode(keys ...string) string {
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(append([]string{cursorVersion, p.listing, p.filter}, keys...)); err != nil {
		panic(err) // a []string always marshals
	}
	return base64.RawURLEncoding.EncodeToString(bytes.TrimSuffix(raw.Bytes(), []byte("\n")))
}

// decode returns the n keyset values of a token made by encode in the same scope; "" is the
// first page (nil keys).
func (p pageScope) decode(token string, n int) (cursorKeys, error) {
	if token == "" {
		return nil, nil
	}
	if len(token) > maxCursorLength {
		return nil, invalidCursor()
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, invalidCursor()
	}
	var parts []string
	if json.Unmarshal(raw, &parts) != nil || len(parts) != 3+n ||
		parts[0] != cursorVersion || parts[1] != p.listing || parts[2] != p.filter {
		return nil, invalidCursor()
	}
	return cursorKeys(parts[3:]), nil
}

// cursorKeys are a decoded token's keyset values; the typed getters refuse a malformed one.
type cursorKeys []string

func (k cursorKeys) uuid(i int) (uuid.UUID, error) {
	id, err := uuid.Parse(k[i])
	if err != nil {
		return uuid.Nil, invalidCursor()
	}
	return id, nil
}

func (k cursorKeys) time(i int) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, k[i])
	if err != nil {
		return time.Time{}, invalidCursor()
	}
	return t, nil
}

func (k cursorKeys) int64(i int) (int64, error) {
	n, err := strconv.ParseInt(k[i], 10, 64)
	if err != nil {
		return 0, invalidCursor()
	}
	return n, nil
}

func cursorTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// pageLimit reads ?limit= (1–max; def when absent); a bad value has been answered with 400.
func (s *Server) pageLimit(c *gin.Context, def, maxLimit int) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return def, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		s.fail(c, domain.NewError(domain.CodeInvalid, fmt.Sprintf("limit must be 1–%d", maxLimit)))
		return 0, false
	}
	return n, true
}

// cursor decodes ?cursor= within scope into a typed store cursor with parse (nil on the first
// page); a bad token has been answered with 400.
func cursor[C any](s *Server, c *gin.Context, scope pageScope, n int, parse func(cursorKeys) (C, error)) (*C, bool) {
	keys, err := scope.decode(c.Query("cursor"), n)
	if err == nil && keys != nil {
		var v C
		if v, err = parse(keys); err == nil {
			return &v, true
		}
	}
	if err != nil {
		s.fail(c, err)
		return nil, false
	}
	return nil, true
}

// nextToken encodes the store's next cursor within scope ("" on the last page).
func nextToken[C any](scope pageScope, next *C, keys func(C) []string) string {
	if next == nil {
		return ""
	}
	return scope.encode(keys(*next)...)
}

// page answers one page: the items as a plain array, the next token in X-Next-Cursor.
func (s *Server) page(c *gin.Context, items any, next string, err error) {
	if err != nil {
		s.fail(c, err)
		return
	}
	if next != "" {
		c.Header(nextCursorHeader, next)
	}
	s.ok(c, items)
}
