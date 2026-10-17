package integration_test

import (
	"strings"
	"testing"
)

// /metrics exports the pool and in-flight series the bench reads (Little's law, pool saturation).
func TestMetricsExportPoolAndInFlight(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	api := newAPI(st, svc)
	for _, prefix := range []string{
		"scheduler_db_pool_max_conns", "scheduler_db_pool_acquired_conns", "scheduler_db_pool_acquires_total",
		"scheduler_db_pool_empty_acquire_wait_seconds_total", "scheduler_http_in_flight_requests",
	} {
		line := metricLine(t, api, prefix)
		if line == "" {
			t.Fatalf("/metrics has no %s series", prefix)
		}
		if !strings.Contains(line, `instance_id="test"`) {
			t.Fatalf("%s is not labelled by instance: %s", prefix, line)
		}
	}
	// The scrape itself is the one request in flight.
	if line := metricLine(t, api, "scheduler_http_in_flight_requests"); !strings.HasSuffix(line, " 1") {
		t.Fatalf("in-flight during a scrape = %q, want 1", line)
	}
}
