package config

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTrustedProxies(t *testing.T) {
	tests := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{raw: "127.0.0.1/32, ::1/128", want: []string{"127.0.0.1/32", "::1/128"}},
		{raw: "10.0.0.0/8,192.168.1.7", want: []string{"10.0.0.0/8", "192.168.1.7"}},
		{raw: "none", want: nil},
		{raw: "NONE", want: nil},
		{raw: "gateway", wantErr: true},
		{raw: "10.0.0.0/33", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := trustedProxies(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAllowedOrigins(t *testing.T) {
	tests := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{raw: "", want: []string{}},
		{raw: "http://localhost:8081, HTTPS://Scheduler.Example.com/", want: []string{"http://localhost:8081", "https://scheduler.example.com"}},
		{raw: "localhost:8081", wantErr: true},
		{raw: "ftp://example.com", wantErr: true},
		{raw: "https://example.com/app", wantErr: true},
		{raw: "https://example.com?x=1", wantErr: true},
		{raw: "https://user@example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			got, err := allowedOrigins(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInstanceIDDefaultsToHostname(t *testing.T) {
	t.Setenv("INSTANCE_ID", "")
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("no hostname on this machine")
	}
	if got := instanceID(); got != host {
		t.Fatalf("instanceID() = %q, want hostname %q", got, host)
	}
	t.Setenv("INSTANCE_ID", " backend-1 ")
	if got := instanceID(); got != "backend-1" {
		t.Fatalf("instanceID() = %q, want backend-1", got)
	}
}

func TestValidateJWTSecret(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{name: "empty", secret: "", wantErr: true},
		{name: "short", secret: "change-me", wantErr: true},
		{name: "31 bytes", secret: strings.Repeat("a", 31), wantErr: true},
		{name: "32 bytes", secret: strings.Repeat("a", 32)},
		{name: "64 bytes", secret: strings.Repeat("ab", 32)},
		{name: "published example", secret: devSecretPrefix + "-bdf36b71b3f15c7603f5775b48877d291f641b17e2ba6254", wantErr: true},
		{name: "example prefix", secret: devSecretPrefix + strings.Repeat("x", 40), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateJWTSecret(tt.secret); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsShortJWTSecret(t *testing.T) {
	t.Chdir(t.TempDir()) // no .env to pick up
	t.Setenv("DATABASE_URL", "postgres://x@127.0.0.1/x")
	t.Setenv("JWT_SECRET", "too-short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("Load() err = %v, want a JWT_SECRET error", err)
	}
	t.Setenv("JWT_SECRET", strings.Repeat("k", MinJWTSecretLength))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with a 32-byte secret: %v", err)
	}
	if cfg.UserCacheTTL != 5*time.Second {
		t.Fatalf("UserCacheTTL = %v, want the 5s default", cfg.UserCacheTTL)
	}
	t.Setenv("USER_CACHE_TTL", "2h")
	if _, err := Load(); err == nil {
		t.Fatal("USER_CACHE_TTL=2h accepted")
	}
}

func TestLoadCatalogCacheAndReadLimits(t *testing.T) {
	t.Chdir(t.TempDir()) // no .env to pick up
	t.Setenv("DATABASE_URL", "postgres://x@127.0.0.1/x")
	t.Setenv("JWT_SECRET", strings.Repeat("k", MinJWTSecretLength))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatalogCacheTTL != 2*time.Second || cfg.ReadLimitPerUser != 600 || cfg.ReadLimitPerIP != 3000 {
		t.Fatalf("defaults = %v %d %d, want 2s 600 3000", cfg.CatalogCacheTTL, cfg.ReadLimitPerUser, cfg.ReadLimitPerIP)
	}
	tests := []struct {
		key, value string
		wantErr    bool
	}{
		{key: "CATALOG_CACHE_TTL", value: "0s"},
		{key: "CATALOG_CACHE_TTL", value: "5s"},
		{key: "CATALOG_CACHE_TTL", value: "6s", wantErr: true},
		{key: "CATALOG_CACHE_TTL", value: "-1s", wantErr: true},
		{key: "READ_LIMIT_PER_USER", value: "50"},
		{key: "READ_LIMIT_PER_USER", value: "0", wantErr: true},
		{key: "READ_LIMIT_PER_IP", value: "lots", wantErr: true},
		{key: "AVAILABILITY_LIMIT_PER_IP", value: "5000"},
		{key: "AVAILABILITY_LIMIT_PER_USER", value: "4", wantErr: true}, // a 31-day next search costs 5
		{key: "WRITE_LIMIT_PER_USER", value: "0", wantErr: true},
		{key: "WRITE_LIMIT_PER_IP", value: "many", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := Load(); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// loadEnv isolates Load from any .env and sets the two required variables.
func loadEnv(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir()) // no .env to pick up
	t.Setenv("DATABASE_URL", "postgres://x@127.0.0.1/x")
	t.Setenv("JWT_SECRET", strings.Repeat("k", MinJWTSecretLength))
	// A shell that sourced .env (scripts/ensure-env.sh) must not change the defaults under test.
	for _, key := range []string{"SEED", "NO_SHOW_SWEEP", "REDIS_REQUIRED", "DB_STATEMENT_TIMEOUT", "REQUEST_TIMEOUT",
		"ACCESS_TOKEN_TTL", "SESSION_IDLE_TTL", "SESSION_MAX_TTL", "AUTH_COOKIE_SECURE", "REFRESH_LIMIT_PER_IP"} {
		t.Setenv(key, "")
	}
}

func TestLoadBooleans(t *testing.T) {
	loadEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Seed || !cfg.NoShowSweep || cfg.RedisRequired {
		t.Fatalf("defaults Seed=%v NoShowSweep=%v RedisRequired=%v, want false true false", cfg.Seed, cfg.NoShowSweep, cfg.RedisRequired)
	}
	tests := []struct {
		key, value string
		want       bool
		wantErr    bool
	}{
		{key: "SEED", value: "true", want: true},
		{key: "SEED", value: "1", want: true},
		{key: "SEED", value: "TRUE", want: true},
		{key: "SEED", value: " false ", want: false},
		{key: "SEED", value: "", want: false},
		{key: "SEED", value: "yes", wantErr: true},
		{key: "NO_SHOW_SWEEP", value: "false", want: false},
		{key: "NO_SHOW_SWEEP", value: "0", want: false},
		{key: "NO_SHOW_SWEEP", value: "", want: true},
		{key: "NO_SHOW_SWEEP", value: "off", wantErr: true},
		{key: "NO_SHOW_SWEEP", value: "flase", wantErr: true},
		{key: "REDIS_REQUIRED", value: "true", want: true},
		{key: "REDIS_REQUIRED", value: "", want: false},
		{key: "REDIS_REQUIRED", value: "maybe", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			cfg, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), tt.key) {
					t.Fatalf("err %q does not name %s", err, tt.key)
				}
				return
			}
			got := map[string]bool{"SEED": cfg.Seed, "NO_SHOW_SWEEP": cfg.NoShowSweep, "REDIS_REQUIRED": cfg.RedisRequired}[tt.key]
			if got != tt.want {
				t.Fatalf("%s=%q parsed as %v, want %v", tt.key, tt.value, got, tt.want)
			}
		})
	}
}

func TestLoadRequestTimeout(t *testing.T) {
	loadEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequestTimeout != 20*time.Second || cfg.DBStatementTimeout >= cfg.RequestTimeout || cfg.RequestTimeout >= HTTPWriteTimeout {
		t.Fatalf("defaults statement %s request %s write %s are not nested", cfg.DBStatementTimeout, cfg.RequestTimeout, HTTPWriteTimeout)
	}
	tests := []struct {
		name      string
		statement string
		request   string
		wantErr   bool
	}{
		{name: "nested", statement: "10s", request: "25s"},
		{name: "statement equals request", statement: "20s", request: "20s", wantErr: true},
		{name: "statement above request", statement: "25s", request: "20s", wantErr: true},
		{name: "request at write timeout", statement: "15s", request: "30s", wantErr: true},
		{name: "request above write timeout", statement: "15s", request: "1m", wantErr: true},
		{name: "request not a duration", statement: "15s", request: "20", wantErr: true},
		{name: "request zero", statement: "15s", request: "0s", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DB_STATEMENT_TIMEOUT", tt.statement)
			t.Setenv("REQUEST_TIMEOUT", tt.request)
			if _, err := Load(); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadSessions(t *testing.T) {
	loadEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AccessTokenTTL != 15*time.Minute || cfg.SessionIdleTTL != 24*time.Hour || cfg.SessionMaxTTL != 168*time.Hour ||
		!cfg.AuthCookieSecure || cfg.RefreshLimitPerIP != 60 {
		t.Fatalf("defaults access %s idle %s max %s secure %v refresh/ip %d, want 15m 24h 168h true 60",
			cfg.AccessTokenTTL, cfg.SessionIdleTTL, cfg.SessionMaxTTL, cfg.AuthCookieSecure, cfg.RefreshLimitPerIP)
	}
	tests := []struct {
		name                string
		access, idle, max   string
		secure, refreshByIP string
		wantErr             string
	}{
		{name: "custom nested", access: "5m", idle: "1h", max: "12h"},
		{name: "access at minimum", access: "1m", idle: "2m", max: "2m"},
		{name: "access at maximum", access: "1h", idle: "2h", max: "2h"},
		{name: "max at 90 days", access: "15m", idle: "24h", max: "2160h"},
		{name: "access below minimum", access: "30s", wantErr: "ACCESS_TOKEN_TTL"},
		{name: "access above maximum", access: "2h", idle: "24h", wantErr: "ACCESS_TOKEN_TTL"},
		{name: "access not a duration", access: "15", wantErr: "ACCESS_TOKEN_TTL"},
		{name: "idle equals access", access: "15m", idle: "15m", wantErr: "SESSION_IDLE_TTL"},
		{name: "idle shorter than access", access: "30m", idle: "10m", wantErr: "SESSION_IDLE_TTL"},
		{name: "idle zero", idle: "0s", wantErr: "SESSION_IDLE_TTL"},
		{name: "max shorter than idle", idle: "24h", max: "12h", wantErr: "SESSION_MAX_TTL"},
		{name: "max above 90 days", max: "2161h", wantErr: "SESSION_MAX_TTL"},
		{name: "cookie not secure", secure: "false"},
		{name: "cookie flag invalid", secure: "sometimes", wantErr: "AUTH_COOKIE_SECURE"},
		{name: "refresh limit raised", refreshByIP: "6000"},
		{name: "refresh limit zero", refreshByIP: "0", wantErr: "REFRESH_LIMIT_PER_IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ACCESS_TOKEN_TTL", tt.access)
			t.Setenv("SESSION_IDLE_TTL", tt.idle)
			t.Setenv("SESSION_MAX_TTL", tt.max)
			t.Setenv("AUTH_COOKIE_SECURE", tt.secure)
			t.Setenv("REFRESH_LIMIT_PER_IP", tt.refreshByIP)
			cfg, err := Load()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.access != "" && cfg.AccessTokenTTL.String() != (mustDuration(t, tt.access)).String() {
				t.Fatalf("ACCESS_TOKEN_TTL=%s parsed as %s", tt.access, cfg.AccessTokenTTL)
			}
			if tt.secure == "false" && cfg.AuthCookieSecure {
				t.Fatal("AUTH_COOKIE_SECURE=false left Secure on")
			}
			if tt.refreshByIP == "6000" && cfg.RefreshLimitPerIP != 6000 {
				t.Fatalf("REFRESH_LIMIT_PER_IP=6000 parsed as %d", cfg.RefreshLimitPerIP)
			}
		})
	}
}

func mustDuration(t *testing.T, raw string) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatal(err)
	}
	return d
}
