package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDocsAreLockedDown(t *testing.T) {
	_, h := newTestServer(t)
	for _, path := range []string{"/api/v1/docs", "/api/v1/docs/", "/api/v1/docs/swagger-init.js", "/api/v1/openapi.yaml"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		// Scripts: our own files and the pinned swagger-ui-dist path only — no other unpkg package,
		// no inline or eval allowance.
		if !strings.Contains(csp, "script-src 'self' https://unpkg.com/swagger-ui-dist@5.27.1/;") ||
			strings.Contains(csp, "https://unpkg.com;") || strings.Contains(csp, "https://unpkg.com ") ||
			!strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: CSP %q", path, csp)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/docs", nil))
	page := rec.Body.String()
	// Every unpkg asset the page loads sits under the path the CSP allows.
	if strings.Count(page, "https://unpkg.com/") != strings.Count(page, "https://unpkg.com/swagger-ui-dist@5.27.1/") {
		t.Fatal("docs page loads an unpkg asset outside the pinned swagger-ui-dist path")
	}
	if strings.Count(page, `integrity="sha384-`) != 2 || strings.Count(page, `crossorigin="anonymous"`) != 2 {
		t.Fatal("unpkg assets are not SRI-pinned")
	}
	// Every script is external: the CSP has no inline-script allowance.
	if strings.Count(page, "<script") != strings.Count(page, "<script src=") {
		t.Fatal("docs page has an inline script")
	}
}
