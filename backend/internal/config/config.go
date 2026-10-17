package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL string
	RedisURL    string
	// RedisRequired makes startup fail while Redis is unreachable. Off by default: every Redis use
	// fails open (idempotency, limits, tickets) and go-redis reconnects by itself, so a Redis outage
	// during a restart must not keep the API down.
	RedisRequired bool
	KafkaBrokers  []string
	KafkaTopic    string
	// KafkaPartitions / KafkaReplication are used when the backend creates the topic at startup
	// (an existing topic is left alone).
	KafkaPartitions  int
	KafkaReplication int
	HTTPAddr         string
	InstanceID       string
	JWTSecret        string
	// UserCacheTTL is how long each instance may reuse a user row when authenticating requests
	// (0 disables). Changes made on another instance take up to this long to apply here.
	UserCacheTTL time.Duration
	// CatalogCacheTTL is how long each instance reuses the GET /dealerships and /service-types
	// bodies (0 disables, at most 5s): edits made on another instance show here within it.
	CatalogCacheTTL time.Duration
	// ReadLimitPerUser / ReadLimitPerIP cap customers' plain reads (appointments, vehicles,
	// dealerships, service types; never /auth/me) per minute; staff are not counted.
	ReadLimitPerUser int
	ReadLimitPerIP   int
	// AvailabilityLimitPerUser/PerIP and WriteLimitPerUser/PerIP cap customers' availability
	// reads and booking writes per minute; staff are not counted.
	AvailabilityLimitPerUser, AvailabilityLimitPerIP int
	WriteLimitPerUser, WriteLimitPerIP               int
	Seed                                             bool
	// NoShowSweep marks confirmed appointments not checked in within their dealership's
	// no-show grace (dealerships.no_show_grace_minutes) as no_show.
	NoShowSweep bool
	// OverrunStep: in-progress jobs past their end are auto-extended in these steps. 0 disables.
	OverrunStep time.Duration
	// NotifyWebhookURL, when set, receives every customer notification as a JSON POST (SMS/email
	// gateway); otherwise notifications are only logged (recipient masked).
	NotifyWebhookURL string
	// DBMaxConns caps the per-instance pool (two backends + stress must fit max_connections).
	DBMaxConns int32
	// DBLockTimeout / DBStatementTimeout bound how long a request waits on Postgres.
	DBLockTimeout      time.Duration
	DBStatementTimeout time.Duration
	// RequestTimeout is every request's deadline (pool acquire and all its statements together):
	// above DBStatementTimeout, below HTTPWriteTimeout.
	RequestTimeout time.Duration
	// TrustedProxies are the addresses (IPs or CIDRs) whose X-Forwarded-For / X-Real-IP the API
	// believes when deriving the client IP for rate limits; anyone else is identified by the TCP
	// peer. Empty trusts nobody.
	TrustedProxies []string
	// AllowedOrigins, when set, are the only browser origins (scheme://host[:port]) that may open
	// the WebSocket; otherwise the Origin host must equal the request Host. The cookie endpoints
	// (POST /auth/refresh, /auth/logout with the cookie) accept these origins besides same-origin.
	AllowedOrigins []string
	// AccessTokenTTL is the access token (JWT) lifetime: 1m–1h, default 15m.
	AccessTokenTTL time.Duration
	// SessionIdleTTL is how long one refresh token stays usable (default 24h): longer than
	// AccessTokenTTL, at most SessionMaxTTL.
	SessionIdleTTL time.Duration
	// SessionMaxTTL is a login session's absolute end (default 168h = 7 days, at most 90 days):
	// refreshing never extends it.
	SessionMaxTTL time.Duration
	// AuthCookieSecure sets Secure on the refresh cookie (default true). Only a plain-http local
	// stack turns it off; anything served over HTTPS must keep it.
	AuthCookieSecure bool
	// RefreshLimitPerIP caps POST /auth/refresh per client IP per minute (default 60).
	RefreshLimitPerIP int
}

// Bounds of the session lifetimes (validateSessionTTLs).
const (
	MinAccessTokenTTL = time.Minute
	MaxAccessTokenTTL = time.Hour
	MaxSessionMaxTTL  = 90 * 24 * time.Hour
)

// HTTPWriteTimeout is the HTTP server's WriteTimeout (cmd/server): a response must be written
// before it, so RequestTimeout has to end first.
const HTTPWriteTimeout = 30 * time.Second

// devSecretPrefix starts the JWT_SECRET published in .env.example: a known value anyone can
// sign tokens with, so it is refused (scripts/ensure-env.sh writes a random one into .env).
const devSecretPrefix = "dev-only-change-me" //nolint:gosec // G101: the published, refused example value, not a credential

// defaultTrustedProxies covers a backend run directly behind a local proxy (Vite dev server).
// Compose overrides it with the gateway's fixed address.
const defaultTrustedProxies = "127.0.0.1/32,::1/128"

// LoadDotEnv loads the first .env found (cwd, then parent for `go run` from backend/).
// Existing OS environment variables always win (godotenv default).
func LoadDotEnv() {
	if err := godotenv.Load(".env"); err == nil {
		return
	}
	_ = godotenv.Load("../.env")
}

func Load() (Config, error) {
	LoadDotEnv()
	cfg := Config{
		DatabaseURL:  os.Getenv("DATABASE_URL"),
		RedisURL:     getenv("REDIS_URL", "redis://127.0.0.1:6379/0"),
		KafkaBrokers: splitCSV(getenv("KAFKA_BROKERS", "127.0.0.1:9094")),
		KafkaTopic:   getenv("KAFKA_TOPIC", "appointments.events"),
		HTTPAddr:     getenv("HTTP_ADDR", ":8080"),
		InstanceID:   instanceID(),
		JWTSecret:    os.Getenv("JWT_SECRET"),
	}
	var err error
	if cfg.Seed, err = boolEnv("SEED", false); err != nil {
		return Config{}, err
	}
	if cfg.NoShowSweep, err = boolEnv("NO_SHOW_SWEEP", true); err != nil {
		return Config{}, err
	}
	if cfg.RedisRequired, err = boolEnv("REDIS_REQUIRED", false); err != nil {
		return Config{}, err
	}
	cfg.NotifyWebhookURL = strings.TrimSpace(os.Getenv("NOTIFY_WEBHOOK_URL"))
	maxConns, err := strconv.Atoi(getenv("DB_MAX_CONNS", "20"))
	if err != nil || maxConns < 4 || maxConns > 500 {
		return Config{}, fmt.Errorf("DB_MAX_CONNS must be 4–500")
	}
	cfg.DBMaxConns = int32(maxConns) //nolint:gosec // G115: bounded to 500 above
	if cfg.DBLockTimeout, err = positiveDuration("DB_LOCK_TIMEOUT", "3s"); err != nil {
		return Config{}, err
	}
	if cfg.DBStatementTimeout, err = positiveDuration("DB_STATEMENT_TIMEOUT", "15s"); err != nil {
		return Config{}, err
	}
	if cfg.RequestTimeout, err = positiveDuration("REQUEST_TIMEOUT", "20s"); err != nil {
		return Config{}, err
	}
	if err := validateTimeouts(cfg.DBStatementTimeout, cfg.RequestTimeout); err != nil {
		return Config{}, err
	}
	step, err := time.ParseDuration(getenv("OVERRUN_EXTEND_STEP", "15m"))
	if err != nil || step < 0 {
		return Config{}, fmt.Errorf("OVERRUN_EXTEND_STEP must be a non-negative duration like 15m")
	}
	cfg.OverrunStep = step
	if cfg.KafkaPartitions, err = intInRange("KAFKA_TOPIC_PARTITIONS", "3", 1, 256); err != nil {
		return Config{}, err
	}
	if cfg.KafkaReplication, err = intInRange("KAFKA_TOPIC_REPLICATION", "1", 1, 7); err != nil {
		return Config{}, err
	}
	if cfg.UserCacheTTL, err = time.ParseDuration(getenv("USER_CACHE_TTL", "5s")); err != nil ||
		cfg.UserCacheTTL < 0 || cfg.UserCacheTTL > time.Minute {
		return Config{}, fmt.Errorf("USER_CACHE_TTL must be a duration from 0s to 1m, like 5s")
	}
	if cfg.CatalogCacheTTL, err = time.ParseDuration(getenv("CATALOG_CACHE_TTL", "2s")); err != nil ||
		cfg.CatalogCacheTTL < 0 || cfg.CatalogCacheTTL > 5*time.Second {
		return Config{}, fmt.Errorf("CATALOG_CACHE_TTL must be a duration from 0s to 5s, like 2s")
	}
	if cfg.ReadLimitPerUser, err = intInRange("READ_LIMIT_PER_USER", "600", 1, 1_000_000); err != nil {
		return Config{}, err
	}
	if cfg.ReadLimitPerIP, err = intInRange("READ_LIMIT_PER_IP", "3000", 1, 1_000_000); err != nil {
		return Config{}, err
	}
	// Availability budgets start at 5: GET /availability/next over its 31-day maximum costs five
	// units (one per started week), so a smaller budget would refuse it forever.
	for _, l := range []struct {
		dst      *int
		key, def string
		min      int
	}{
		{&cfg.AvailabilityLimitPerUser, "AVAILABILITY_LIMIT_PER_USER", "120", 5},
		{&cfg.AvailabilityLimitPerIP, "AVAILABILITY_LIMIT_PER_IP", "600", 5},
		{&cfg.WriteLimitPerUser, "WRITE_LIMIT_PER_USER", "30", 1},
		{&cfg.WriteLimitPerIP, "WRITE_LIMIT_PER_IP", "120", 1},
	} {
		if *l.dst, err = intInRange(l.key, l.def, l.min, 1_000_000); err != nil {
			return Config{}, err
		}
	}
	if cfg.TrustedProxies, err = trustedProxies(getenv("TRUSTED_PROXIES", defaultTrustedProxies)); err != nil {
		return Config{}, err
	}
	if cfg.AllowedOrigins, err = allowedOrigins(os.Getenv("ALLOWED_ORIGINS")); err != nil {
		return Config{}, err
	}
	if err := loadSessions(&cfg); err != nil {
		return Config{}, err
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if len(cfg.KafkaBrokers) == 0 {
		return Config{}, fmt.Errorf("KAFKA_BROKERS is required")
	}
	if err := validateJWTSecret(cfg.JWTSecret); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// loadSessions reads the access token and refresh session settings.
func loadSessions(cfg *Config) error {
	var err error
	if cfg.AccessTokenTTL, err = positiveDuration("ACCESS_TOKEN_TTL", "15m"); err != nil {
		return err
	}
	if cfg.SessionIdleTTL, err = positiveDuration("SESSION_IDLE_TTL", "24h"); err != nil {
		return err
	}
	if cfg.SessionMaxTTL, err = positiveDuration("SESSION_MAX_TTL", "168h"); err != nil {
		return err
	}
	if err := validateSessionTTLs(cfg.AccessTokenTTL, cfg.SessionIdleTTL, cfg.SessionMaxTTL); err != nil {
		return err
	}
	if cfg.AuthCookieSecure, err = boolEnv("AUTH_COOKIE_SECURE", true); err != nil {
		return err
	}
	if cfg.RefreshLimitPerIP, err = intInRange("REFRESH_LIMIT_PER_IP", "60", 1, 1_000_000); err != nil {
		return err
	}
	return nil
}

// validateSessionTTLs keeps the lifetimes nested: an access token short enough to be a bearer
// credential (1m–1h), a refresh token outliving it (else the session ends at the first expiry),
// and a family at least one idle period long and at most MaxSessionMaxTTL.
func validateSessionTTLs(access, idle, maxTTL time.Duration) error {
	switch {
	case access < MinAccessTokenTTL || access > MaxAccessTokenTTL:
		return fmt.Errorf("ACCESS_TOKEN_TTL (%s) must be from %s to %s", access, MinAccessTokenTTL, MaxAccessTokenTTL)
	case idle <= access:
		return fmt.Errorf("SESSION_IDLE_TTL (%s) must be longer than ACCESS_TOKEN_TTL (%s)", idle, access)
	case maxTTL < idle:
		return fmt.Errorf("SESSION_MAX_TTL (%s) must be at least SESSION_IDLE_TTL (%s)", maxTTL, idle)
	case maxTTL > MaxSessionMaxTTL:
		return fmt.Errorf("SESSION_MAX_TTL (%s) must be at most %s", maxTTL, MaxSessionMaxTTL)
	}
	return nil
}

// MinJWTSecretLength is the HS256 key size: a shorter secret is cheaper to brute-force offline
// from any token, which would let an attacker mint tokens for any user.
const MinJWTSecretLength = 32

func validateJWTSecret(secret string) error {
	switch {
	case secret == "":
		return fmt.Errorf("JWT_SECRET is required")
	case strings.HasPrefix(secret, devSecretPrefix):
		return fmt.Errorf("JWT_SECRET is the published example value; generate one (openssl rand -hex 32, or scripts/ensure-env.sh)")
	case len(secret) < MinJWTSecretLength:
		return fmt.Errorf("JWT_SECRET must be at least %d bytes (e.g. openssl rand -hex 32)", MinJWTSecretLength)
	}
	return nil
}

// validateTimeouts keeps the deadlines nested: one statement times out in Postgres (57014, the
// connection stays usable) before the request deadline cancels it from the client side, and the
// request deadline's 503 is written before the server's WriteTimeout drops the connection.
func validateTimeouts(statement, request time.Duration) error {
	switch {
	case statement >= request:
		return fmt.Errorf("DB_STATEMENT_TIMEOUT (%s) must be shorter than REQUEST_TIMEOUT (%s)", statement, request)
	case request >= HTTPWriteTimeout:
		return fmt.Errorf("REQUEST_TIMEOUT (%s) must be shorter than the HTTP write timeout (%s)", request, HTTPWriteTimeout)
	}
	return nil
}

// boolEnv parses key with strconv.ParseBool (1/0, t/f, true/false, any case); unset or blank is
// fallback, anything else is an error rather than a silent default.
func boolEnv(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false, got %q", key, raw)
	}
	return v, nil
}

func intInRange(key, fallback string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(getenv(key, fallback))
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be an integer %d–%d", key, lo, hi)
	}
	return n, nil
}

// instanceID names this replica in logs, metrics, and its per-instance Kafka consumer group, so two
// replicas must never share one: unset defaults to the hostname (the container id under Docker).
func instanceID() string {
	if id := strings.TrimSpace(os.Getenv("INSTANCE_ID")); id != "" {
		return id
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "local"
}

// trustedProxies parses a comma-separated list of IPs and CIDRs; "none" trusts nobody.
func trustedProxies(raw string) ([]string, error) {
	if strings.EqualFold(strings.TrimSpace(raw), "none") {
		return nil, nil
	}
	out := splitCSV(raw)
	for _, entry := range out {
		if _, err := netip.ParsePrefix(entry); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(entry); err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: %q is not an IP or CIDR", entry)
		}
	}
	return out, nil
}

// allowedOrigins parses a comma-separated list of browser origins into lower-case
// scheme://host[:port] (what browsers send in the Origin header).
func allowedOrigins(raw string) ([]string, error) {
	entries := splitCSV(raw)
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		u, err := url.Parse(strings.TrimSuffix(entry, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" ||
			u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("ALLOWED_ORIGINS: %q must look like https://scheduler.example.com", entry)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
}

func positiveDuration(key, fallback string) (time.Duration, error) {
	d, err := time.ParseDuration(getenv(key, fallback))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like %s", key, fallback)
	}
	return d, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
