package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	healthCacheTTL     = time.Second
	healthProbeTimeout = 2 * time.Second
	redisProbeTimeout  = 250 * time.Millisecond // Redis is optional: report it down fast

	checkOK        = "ok"
	checkDown      = "down"
	checkSaturated = "saturated" // checks.pool: every request-pool connection in use
)

type healthResult struct {
	postgres string
	redis    string
}

// healthCache shares one dependency probe among all readiness calls within ttl. Callers wait for
// an in-flight probe rather than starting their own.
type healthCache struct {
	ttl   time.Duration
	now   func() time.Time
	log   *slog.Logger
	probe func(context.Context) (dbErr, redisErr error)

	mu      sync.Mutex
	last    healthResult
	expires time.Time
}

func newHealthCache(ttl time.Duration, log *slog.Logger, probe func(context.Context) (dbErr, redisErr error)) *healthCache {
	if log == nil {
		log = slog.Default()
	}
	return &healthCache{ttl: ttl, now: time.Now, log: log, probe: probe}
}

func (h *healthCache) get(ctx context.Context) healthResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.now().Before(h.expires) {
		return h.last
	}
	// Detached from the caller: one impatient client must not record an outage for everyone.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthProbeTimeout)
	defer cancel()
	dbErr, redisErr := h.probe(ctx)
	h.last = healthResult{postgres: status(dbErr), redis: status(redisErr)}
	h.expires = h.now().Add(h.ttl)
	if dbErr != nil || redisErr != nil {
		h.log.Warn("readiness probe", "db", dbErr, "redis", redisErr)
	}
	return h.last
}

func status(err error) string {
	if err != nil {
		return checkDown
	}
	return checkOK
}

// dependencyCollector exports scheduler_dependency_up{dep} (1 up, 0 down) for Postgres and Redis
// from the shared readiness probe, run at scrape time (healthCache: at most one probe per TTL
// however many scrapes and readiness calls arrive), so a Redis outage, which every caller rides
// out by failing open, still shows up and can alert.
type dependencyCollector struct {
	health *healthCache
	desc   *prometheus.Desc
}

func newDependencyCollector(h *healthCache, constLabels prometheus.Labels) *dependencyCollector {
	return &dependencyCollector{health: h, desc: prometheus.NewDesc("scheduler_dependency_up",
		"Whether this instance's last readiness probe reached the dependency (1) or not (0).",
		[]string{"dep"}, constLabels)}
}

func (d *dependencyCollector) Describe(ch chan<- *prometheus.Desc) { ch <- d.desc }

func (d *dependencyCollector) Collect(ch chan<- prometheus.Metric) {
	r := d.health.get(context.Background())
	for dep, state := range map[string]string{"postgres": r.postgres, "redis": r.redis} {
		up := 0.0
		if state == checkOK {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(d.desc, prometheus.GaugeValue, up, dep)
	}
}
