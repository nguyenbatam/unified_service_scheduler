package httpapi

import (
	"net/http"
	"strings"

	"github.com/klauspost/compress/gzhttp"
)

// compressMinSize is the smallest body worth compressing: below it the framing and CPU cost more
// than the bytes saved (an appointment or an error is a few hundred bytes; lists and the board
// are tens of kilobytes of repeated keys).
const compressMinSize = 1024

// uncompressed lists the path prefixes served as is, whatever the client accepts:
//   - /api/v1/auth/: login, register, refresh and ws-ticket bodies carry tokens, and a secret in a
//     compressed body can leak through its size (BREACH); they are small anyway.
//   - /api/v1/ws/: the WebSocket upgrade hijacks the connection (frames are not HTTP bodies).
//   - /metrics: promhttp negotiates its own compression.
var uncompressed = []string{"/api/v1/auth/", "/api/v1/ws/", "/metrics"}

// withCompression negotiates the response encoding from Accept-Encoding (zstd preferred, then
// gzip; none when the client sends neither) and adds Vary: Accept-Encoding. It runs here, not at
// the gateway: nginx turns a strong ETag into a weak one when it compresses, and If-Match needs
// the strong version ("3") back (parseIfMatch refuses W/"3", as RFC 9110 requires). gzhttp keeps
// the ETag as is: it names the row version, which is the same whatever the encoding.
func withCompression(next http.Handler) http.Handler {
	wrap, err := gzhttp.NewWrapper(gzhttp.MinSize(compressMinSize))
	if err != nil {
		panic("httpapi: compression options: " + err.Error()) // fixed options: a bug, not input
	}
	compressed := wrap(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range uncompressed {
			if strings.HasPrefix(r.URL.Path, prefix) {
				next.ServeHTTP(w, r)
				return
			}
		}
		compressed.ServeHTTP(w, r)
	})
}
