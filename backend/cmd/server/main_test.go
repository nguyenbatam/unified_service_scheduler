package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/cache"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/config"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/notify"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/worker"
)

// Shutdown must outwait the longest flush or send that ignores cancellation, whichever it is.
func TestDrainBudgetCoversInFlightWork(t *testing.T) {
	outbox, notifier := worker.DefaultOutboxConfig(), notify.DefaultConfig()
	got := drainBudget(outbox, notifier)
	if got < outbox.DrainTimeout() || got < notifier.DrainTimeout() {
		t.Fatalf("drain budget %s shorter than in-flight work (outbox %s, notify %s)",
			got, outbox.DrainTimeout(), notifier.DrainTimeout())
	}
	notifier.SendTimeout = time.Minute // the larger term wins
	if got := drainBudget(outbox, notifier); got != notifier.DrainTimeout()+drainMargin {
		t.Fatalf("drain budget = %s with a slow sender, want %s", got, notifier.DrainTimeout()+drainMargin)
	}
}

// The defaults main runs with must pass the startup checks against the default lease.
func TestDefaultConfigsFitTheLease(t *testing.T) {
	lease := worker.DefaultMaintenanceConfig().Lease
	if err := worker.DefaultOutboxConfig().Validate(lease); err != nil {
		t.Fatal(err)
	}
	if err := notify.DefaultConfig().Validate(lease); err != nil {
		t.Fatal(err)
	}
}

// The HTTP drain lets a request reach its own deadline, but never outlasts the background drain the
// stop grace period is sized for.
func TestHTTPShutdownBudget(t *testing.T) {
	drain := drainBudget(worker.DefaultOutboxConfig(), notify.DefaultConfig())
	if got, want := httpShutdownBudget(20*time.Second, drain), 21*time.Second; got != want || want > drain {
		t.Fatalf("default request timeout: shutdown budget %s, want %s (drain %s)", got, want, drain)
	}
	if got := httpShutdownBudget(29*time.Second, drain); got != drain {
		t.Fatalf("long request timeout: shutdown budget %s, want the drain budget %s", got, drain)
	}
}

// closeBudget is what the deferred closes after the drain may take: both Kafka consumers leaving
// their groups (the fan-out one also deletes its group, up to 3 s), the writer, Redis and the pool.
const closeBudget = 10 * time.Second

// Every backend container's stop_grace_period in docker-compose.yml must exceed the default drain
// budget plus the deferred closes, or the orchestrator kills the process mid-drain and leased rows
// wait out the lease (and go out twice).
func TestComposeStopGraceCoversDrain(t *testing.T) {
	path := filepath.Join("..", "..", "..", "docker-compose.yml")
	grace, err := composeStopGrace(path)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("%s not found", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(grace) == 0 {
		t.Fatalf("no backend service with a stop_grace_period in %s", path)
	}
	need := drainBudget(worker.DefaultOutboxConfig(), notify.DefaultConfig()) + closeBudget
	for service, d := range grace {
		if d <= need {
			t.Errorf("%s: stop_grace_period %s must exceed drain budget + closes %s", service, d, need)
		}
	}
}

// The scan understands both a service's own stop_grace_period and one merged from an anchor.
func TestComposeStopGraceParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	const compose = `x-backend: &backend
  image: x
  stop_grace_period: 40s
services:
  db:
    stop_grace_period: 5s
  backend-1:
    <<: *backend
  backend-2:
    <<: *backend
    stop_grace_period: "50s"
  backend-3:
    image: y
`
	if err := os.WriteFile(path, []byte(compose), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := composeStopGrace(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]time.Duration{"backend-1": 40 * time.Second, "backend-2": 50 * time.Second, "backend-3": 0}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for name, d := range want {
		if got[name] != d {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// composeStopGrace reads the stop_grace_period of every service under services: whose name starts
// with "backend", set on the service itself or merged in from a top-level anchor (`x-backend:
// &backend` ... `<<: *backend`). A plain text scan of the compose file's two-space layout, enough
// for this repository; a backend service with neither gets 0 (Docker's 10 s default is too short).
func composeStopGrace(path string) (map[string]time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	anchorGrace := map[string]time.Duration{} // anchor name -> its stop_grace_period
	own := map[string]time.Duration{}         // service -> its own stop_grace_period
	merges := map[string]string{}             // service -> anchor merged in with <<
	var services []string
	block, anchor, service := "", "", ""
	parse := func(trimmed string) (time.Duration, error) {
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "stop_grace_period:"))
		return time.ParseDuration(strings.Trim(value, `"'`))
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), " \t")
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case indent == 0:
			key, rest, _ := strings.Cut(trimmed, ":")
			block, anchor, service = key, "", ""
			if name, ok := strings.CutPrefix(strings.TrimSpace(rest), "&"); ok {
				anchor = name
			}
		case anchor != "" && indent == 2 && strings.HasPrefix(trimmed, "stop_grace_period:"):
			d, err := parse(trimmed)
			if err != nil {
				return nil, err
			}
			anchorGrace[anchor] = d
		case block == "services" && indent == 2:
			service = ""
			if name, ok := strings.CutSuffix(trimmed, ":"); ok && strings.HasPrefix(name, "backend") {
				service = name
				services = append(services, name)
			}
		case service != "" && indent == 4 && strings.HasPrefix(trimmed, "<<:"):
			merges[service] = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "<<:")), "*")
		case service != "" && indent == 4 && strings.HasPrefix(trimmed, "stop_grace_period:"):
			d, err := parse(trimmed)
			if err != nil {
				return nil, err
			}
			own[service] = d
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]time.Duration, len(services))
	for _, name := range services {
		d, ok := own[name]
		if !ok {
			d = anchorGrace[merges[name]]
		}
		out[name] = d
	}
	return out, nil
}

// A serve failure must reach run's return value (main exits 1 on it); a signal or a clean close
// is a normal exit.
func TestAwaitStop(t *testing.T) {
	listenErr := errors.New("listen tcp :8080: bind: address already in use")
	failed := make(chan error, 1)
	failed <- listenErr
	close(failed)
	if err := awaitStop(t.Context(), failed); !errors.Is(err, listenErr) {
		t.Fatalf("serve failure: err = %v, want it wrapped", err)
	}

	closed := make(chan error)
	close(closed)
	if err := awaitStop(t.Context(), closed); err != nil {
		t.Fatalf("clean close: err = %v", err)
	}

	signaled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := awaitStop(signaled, make(chan error)); err != nil {
		t.Fatalf("signal: err = %v", err)
	}
}

// Redis down at startup is a warning (every Redis use fails open), unless REDIS_REQUIRED.
func TestCheckRedisFailsOnlyWhenRequired(t *testing.T) {
	down, err := cache.New("redis://127.0.0.1:1/0") // nothing listens on port 1
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = down.Close() }()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := checkRedis(t.Context(), down, false, log); err != nil {
		t.Fatalf("optional Redis down: err = %v, want startup to continue", err)
	}
	if err := checkRedis(t.Context(), down, true, log); err == nil {
		t.Fatal("required Redis down: want an error")
	}
}

// The HTTP write timeout must outlast the default request deadline, or the 503 for an expired
// deadline could never be written.
func TestWriteTimeoutCoversRequestDeadline(t *testing.T) {
	if httpapi.DefaultRequestTimeout >= config.HTTPWriteTimeout {
		t.Fatalf("request timeout %s >= write timeout %s", httpapi.DefaultRequestTimeout, config.HTTPWriteTimeout)
	}
}
