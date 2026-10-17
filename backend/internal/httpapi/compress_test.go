package httpapi

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithCompression(t *testing.T) {
	big := `{"items":"` + strings.Repeat("appointment ", 400) + `"}`
	small := `{"id":"a1"}`
	h := withCompression(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := big
		if r.URL.Query().Has("small") {
			body = small
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("ETag", `"3"`)
		_, _ = io.WriteString(w, body)
	}))

	tests := []struct {
		name     string
		path     string
		accept   string
		encoding string // expected Content-Encoding ("" = identity)
		body     string
	}{
		{name: "gzip when accepted", path: "/api/v1/schedule", accept: "gzip", encoding: "gzip", body: big},
		{name: "zstd preferred when both accepted", path: "/api/v1/schedule", accept: "gzip, zstd", encoding: "zstd"},
		{name: "identity without Accept-Encoding", path: "/api/v1/schedule", body: big},
		{name: "small bodies stay as they are", path: "/api/v1/appointments/x?small", accept: "gzip", body: small},
		{name: "token-bearing auth responses never compressed", path: "/api/v1/auth/login", accept: "gzip", body: big},
		{name: "metrics negotiate their own encoding", path: "/metrics", accept: "gzip", body: big},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tt.path, nil)
			if tt.accept != "" {
				req.Header.Set("Accept-Encoding", tt.accept)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			res := rec.Result()
			defer func() { _ = res.Body.Close() }()

			if got := res.Header.Get("Content-Encoding"); got != tt.encoding {
				t.Fatalf("Content-Encoding = %q, want %q", got, tt.encoding)
			}
			// The ETag is the row version: If-Match needs it strong whatever the encoding.
			if got := res.Header.Get("ETag"); got != `"3"` {
				t.Errorf("ETag = %q, want the strong \"3\"", got)
			}
			if tt.encoding != "" && !strings.Contains(res.Header.Get("Vary"), "Accept-Encoding") {
				t.Errorf("Vary = %q, want Accept-Encoding", res.Header.Get("Vary"))
			}
			if tt.body == "" {
				return
			}
			var r io.Reader = res.Body
			if tt.encoding == "gzip" {
				zr, err := gzip.NewReader(res.Body)
				if err != nil {
					t.Fatal(err)
				}
				r = zr
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.body {
				t.Errorf("body = %.40q…, want %.40q…", got, tt.body)
			}
		})
	}
}
