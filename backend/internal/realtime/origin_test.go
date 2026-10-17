package realtime

import (
	"net/http/httptest"
	"testing"
)

func TestCheckOrigin(t *testing.T) {
	tests := []struct {
		name    string
		allowed []string
		host    string
		origin  string
		want    bool
	}{
		{name: "no origin (service client)", host: "localhost:8081", want: true},
		{name: "same host and port", host: "localhost:8081", origin: "http://localhost:8081", want: true},
		{name: "same host, case differs", host: "LocalHost:8081", origin: "http://localhost:8081", want: true},
		{name: "port dropped by proxy", host: "localhost", origin: "http://localhost:8081", want: false},
		{name: "other host", host: "localhost:8081", origin: "http://evil.example", want: false},
		{name: "malformed origin", host: "localhost:8081", origin: "::nope", want: false},
		{name: "null origin", host: "localhost:8081", origin: "null", want: false},
		{name: "allowlisted", allowed: []string{"https://ui.example.com"}, host: "api.internal", origin: "https://UI.example.com", want: true},
		{name: "allowlist checks scheme", allowed: []string{"https://ui.example.com"}, host: "ui.example.com", origin: "http://ui.example.com", want: false},
		{name: "allowlist replaces same-host", allowed: []string{"https://ui.example.com"}, host: "localhost:8081", origin: "http://localhost:8081", want: false},
		{name: "allowlist, no origin", allowed: []string{"https://ui.example.com"}, host: "localhost:8081", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewHub(nil)
			h.SetAllowedOrigins(tt.allowed)
			r := httptest.NewRequestWithContext(t.Context(), "GET", "/api/v1/ws/dispatch", nil)
			r.Host = tt.host
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if got := h.checkOrigin(r); got != tt.want {
				t.Fatalf("checkOrigin = %v, want %v", got, tt.want)
			}
		})
	}
}
