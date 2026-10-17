package httpapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
	"github.com/prometheus/client_golang/prometheus"
)

// queueDepthTTL bounds how often scrapes hit Postgres: Prometheus scrapes every few seconds per
// instance, while a backlog worth alerting on builds over minutes.
const queueDepthTTL = 15 * time.Second

// queueCollector exports outbox and notification queue depths per status, so alerts fire on a
// growing backlog or on poisoned (failed) rows that need a human. The query result is reused for
// queueDepthTTL.
type queueCollector struct {
	store *store.Store
	log   *slog.Logger
	desc  *prometheus.Desc

	// mu also serialises refreshes, so concurrent scrapes run at most one query.
	mu        sync.Mutex
	depths    []store.QueueDepth
	ok        bool // depths holds the last successful query
	fetchedAt time.Time
}

func newQueueCollector(st *store.Store, constLabels prometheus.Labels, log *slog.Logger) *queueCollector {
	return &queueCollector{
		store: st,
		log:   log,
		desc: prometheus.NewDesc("scheduler_queue_rows",
			"Outbox and notification rows by queue and status (pending, processing/sending, failed), and workshop/stale_overrun: flagged overruns left open past store.StaleOverrunAge.",
			[]string{"queue", "status"}, constLabels),
	}
}

func (q *queueCollector) Describe(ch chan<- *prometheus.Desc) { ch <- q.desc }

// cached returns the queue depths, querying at most once per queueDepthTTL (a failed query is
// not retried before then either). ok is false when the last query failed.
func (q *queueCollector) cached() ([]store.QueueDepth, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.fetchedAt.IsZero() && time.Since(q.fetchedAt) < queueDepthTTL {
		return q.depths, q.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	depths, err := q.store.QueueDepths(ctx)
	q.fetchedAt = time.Now()
	if err != nil {
		q.log.Warn("queue depth metric", "err", err)
		q.depths, q.ok = nil, false
		return nil, false
	}
	q.depths, q.ok = depths, true
	return depths, true
}

func (q *queueCollector) Collect(ch chan<- prometheus.Metric) {
	depths, ok := q.cached()
	if !ok {
		return
	}
	seen := map[[2]string]bool{}
	for _, d := range depths {
		seen[[2]string{d.Queue, d.Status}] = true
		ch <- prometheus.MustNewConstMetric(q.desc, prometheus.GaugeValue, float64(d.Count), d.Queue, d.Status)
	}
	// Report zero for empty statuses so alert rules see a series instead of "no data".
	for _, k := range [][2]string{{"outbox", "pending"}, {"outbox", "failed"}, {"notifications", "pending"}, {"notifications", "failed"}, {"workshop", "stale_overrun"}} {
		if !seen[k] {
			ch <- prometheus.MustNewConstMetric(q.desc, prometheus.GaugeValue, 0, k[0], k[1])
		}
	}
}

// poolCollector exports the pgx pool's counters at scrape time: how many connections requests hold
// (acquired), how often a request found the pool empty and how long such requests waited. Under
// load, acquired near max with a growing empty-acquire wait means the pool, not Postgres, is the
// bottleneck.
type poolCollector struct {
	store                                  *store.Store
	acquired, idle, total, max             *prometheus.Desc
	acquires, emptyAcquires, emptyWaitSecs *prometheus.Desc
	acquireSecs                            *prometheus.Desc
}

func newPoolCollector(st *store.Store, constLabels prometheus.Labels) *poolCollector {
	desc := func(name, help string) *prometheus.Desc {
		return prometheus.NewDesc("scheduler_db_pool_"+name, help, nil, constLabels)
	}
	return &poolCollector{
		store:         st,
		acquired:      desc("acquired_conns", "Pool connections held by requests or workers right now."),
		idle:          desc("idle_conns", "Idle pool connections."),
		total:         desc("total_conns", "Open pool connections (acquired + idle + being built)."),
		max:           desc("max_conns", "Pool size limit (DB_MAX_CONNS)."),
		acquires:      desc("acquires_total", "Successful connection acquires."),
		emptyAcquires: desc("empty_acquires_total", "Acquires that found no idle connection and had to wait or dial."),
		emptyWaitSecs: desc("empty_acquire_wait_seconds_total", "Time spent waiting by acquires that found the pool empty."),
		acquireSecs:   desc("acquire_seconds_total", "Time spent in all successful acquires."),
	}
}

func (p *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{p.acquired, p.idle, p.total, p.max, p.acquires, p.emptyAcquires, p.emptyWaitSecs, p.acquireSecs} {
		ch <- d
	}
}

func (p *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := p.store.PoolStat()
	gauge := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	gauge(p.acquired, float64(s.AcquiredConns()))
	gauge(p.idle, float64(s.IdleConns()))
	gauge(p.total, float64(s.TotalConns()))
	gauge(p.max, float64(s.MaxConns()))
	counter(p.acquires, float64(s.AcquireCount()))
	counter(p.emptyAcquires, float64(s.EmptyAcquireCount()))
	counter(p.emptyWaitSecs, s.EmptyAcquireWaitTime().Seconds())
	counter(p.acquireSecs, s.AcquireDuration().Seconds())
}
