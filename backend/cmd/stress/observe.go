package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// Cross-checks of what the run saw against what the system recorded: server metrics (Q6), the
// outbox (Q4), and the persisted records read back through another instance (R3).

// promSample is one scraped series: metric name, its labels, and the value.
type promSample struct {
	name   string
	labels map[string]string
	value  float64
}

// scrapeClient reads /metrics with a deadline of its own: a backend that stops answering (killed,
// or stuck) must fail the scrape, not hang a sampler or the run's final cross-checks.
var scrapeClient = &http.Client{Timeout: 5 * time.Second}

// scrape reads a backend's /metrics (Prometheus text format; HELP/TYPE lines skipped).
func scrape(ctx context.Context, base string) ([]promSample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := scrapeClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/metrics: %d", base, resp.StatusCode)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return parseProm(buf.String()), nil
}

// parseProm parses the Prometheus text exposition format (no escapes in label values needed here).
func parseProm(text string) []promSample {
	var out []promSample
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		series, value, ok := strings.CutLast(line, " ")
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			continue
		}
		s := promSample{name: series, labels: map[string]string{}, value: v}
		if i := strings.IndexByte(series, '{'); i >= 0 && strings.HasSuffix(series, "}") {
			s.name = series[:i]
			for kv := range strings.SplitSeq(series[i+1:len(series)-1], ",") {
				k, val, ok := strings.Cut(kv, "=")
				if ok {
					s.labels[strings.TrimSpace(k)] = strings.Trim(val, `"`)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// promSum adds up name's series whose labels include every pair of match.
func promSum(samples []promSample, name string, match map[string]string) float64 {
	total := 0.0
	for _, s := range samples {
		if s.name != name {
			continue
		}
		ok := true
		for k, v := range match {
			if s.labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			total += s.value
		}
	}
	return total
}

// clusterScrape scrapes every backend and concatenates the samples. Each carries instance_id: the
// scheduler_* series have it, and the backend's other series (go_*, process_*, without labels of
// their own) get the one their scrape reported, so process_start_time_seconds tells per instance
// whether it restarted between two scrapes.
func clusterScrape(ctx context.Context, backends ...string) ([]promSample, error) {
	var all []promSample
	for _, b := range backends {
		s, err := scrape(ctx, b)
		if err != nil {
			return nil, err
		}
		id := ""
		for _, x := range s {
			if id = x.labels["instance_id"]; id != "" {
				break
			}
		}
		for _, x := range s {
			if _, ok := x.labels["instance_id"]; !ok && id != "" {
				x.labels["instance_id"] = id
			}
		}
		all = append(all, s...)
	}
	return all, nil
}

// createdOnServer is the cluster's count of booking requests answered 201, by the domain counter
// and by the HTTP counter.
func createdOnServer(samples []promSample) (bookings, http201 float64) {
	return promSum(samples, "scheduler_bookings_total", map[string]string{"result": "created"}),
		promSum(samples, "scheduler_http_requests_total", map[string]string{"route": "/api/v1/appointments", "method": "POST", "status": "201"})
}

// checkMetrics compares the cluster's 201 counters over the run with the run's rows in Postgres:
// every row is one 201 (a replay answers 200), so with no other bookers the deltas are exact.
func checkMetrics(ctx context.Context, m *matrix, conn *pgx.Conn, before []promSample, backends []string, runID string) error {
	after, err := clusterScrape(ctx, backends...)
	if err != nil {
		m.fail("Q6", "metrics", "scrape after the run: %v", err)
		return nil
	}
	b0, h0 := createdOnServer(before)
	b1, h1 := createdOnServer(after)
	var rows int
	if err := conn.QueryRow(ctx, `SELECT count(*)::int FROM appointments WHERE idempotency_key LIKE $1`, runID+":%").Scan(&rows); err != nil {
		return err
	}
	db, dh := int(b1-b0), int(h1-h0)
	m.check("Q6", "metrics", db == rows && dh == rows,
		"Σ scheduler_bookings_total{result=created} Δ=%d and Σ http 201 on POST /appointments Δ=%d across %d instance(s) vs %d rows in Postgres (exact when nothing else books during the run)",
		db, dh, len(backends), rows)
	ids := map[string]bool{}
	for _, s := range after {
		if id := s.labels["instance_id"]; id != "" {
			ids[id] = true
		}
	}
	m.check("Q6", "metrics", len(ids) == len(backends), "every backend exports metrics labelled by instance_id (%d distinct)", len(ids))
	m.metric("serverCreated", db)
	return nil
}

// waitOutboxDrained waits until every outbox row of the run's appointments is published (none
// pending, processing, or failed), up to timeout.
func waitOutboxDrained(ctx context.Context, conn *pgx.Conn, runID string, timeout time.Duration) (total, left, failed int, err error) {
	deadline := time.Now().Add(timeout)
	for {
		err = conn.QueryRow(ctx, `
			SELECT count(*)::int, count(*) FILTER (WHERE o.status <> `+outboxPublishedSQL+`)::int, count(*) FILTER (WHERE o.status = `+outboxFailedSQL+`)::int
			FROM outbox_events o
			WHERE o.aggregate_id IN (SELECT id FROM appointments WHERE idempotency_key LIKE $1)`, runID+":%").Scan(&total, &left, &failed)
		if err != nil || left == 0 || failed > 0 || time.Now().After(deadline) {
			return total, left, failed, err
		}
		select {
		case <-ctx.Done():
			return total, left, failed, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// runEvents lists every appointment event of the run as "id|version" (each change bumps the
// version once, so the pair names one event).
func runEvents(ctx context.Context, conn *pgx.Conn, runID string) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `
		SELECT o.aggregate_id::text || '|' || (o.payload->>'version')
		FROM outbox_events o
		WHERE o.aggregate_type = `+sqlCode(domain.AggregateAppointment)+`
		  AND o.aggregate_id IN (SELECT id FROM appointments WHERE idempotency_key LIKE $1)`, runID+":%")
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out, nil
}

// recordRow is the persisted record as Postgres holds it.
type recordRow struct {
	customer, vehicle, bay, tech uuid.UUID
	status                       domain.AppointmentStatus
	start, end                   time.Time
}

// checkRecords reads every given appointment back through the API, alternating backends, and
// compares it with its row: the confirmed record (customer, vehicle, technician, bay, window,
// status) is persisted and every instance serves the same one.
func checkRecords(ctx context.Context, conn *pgx.Conn, apis []*apiClient, ids []uuid.UUID, limit int) (checked int, diffs []string, err error) {
	sort.Slice(ids, func(i, j int) bool { return ids[i].String() < ids[j].String() })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	rows, err := conn.Query(ctx, `
		SELECT id, customer_id, vehicle_id, service_bay_id, technician_id, status, start_at, end_at
		FROM appointments WHERE id = ANY($1)`, ids)
	if err != nil {
		return 0, nil, err
	}
	db := map[uuid.UUID]recordRow{}
	_, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct{}, error) {
		var id uuid.UUID
		var r recordRow
		var status int16
		err := row.Scan(&id, &r.customer, &r.vehicle, &r.bay, &r.tech, &status, &r.start, &r.end)
		r.status = domain.AppointmentStatus(status)
		db[id] = r
		return struct{}{}, err
	})
	if err != nil {
		return 0, nil, err
	}
	for i, id := range ids {
		r, ok := db[id]
		if !ok {
			diffs = append(diffs, id.String()+": not in Postgres")
			continue
		}
		a, err := getJSON[domain.Appointment](ctx, apis[i%len(apis)], "GET", "/api/v1/appointments/"+id.String(), nil)
		if err != nil {
			diffs = append(diffs, fmt.Sprintf("%s: read back: %v", id, err))
			continue
		}
		checked++
		if a.CustomerID != r.customer || a.VehicleID != r.vehicle || a.ServiceBayID != r.bay || a.TechnicianID != r.tech ||
			a.Status != r.status || !a.StartAt.Equal(r.start) || !a.EndAt.Equal(r.end) ||
			a.ServiceBayName == "" || a.TechnicianName == "" || a.CustomerName == "" {
			diffs = append(diffs, fmt.Sprintf("%s: API %+v vs row %+v", id, a, r))
		}
		if len(diffs) >= 5 {
			break
		}
	}
	return checked, diffs, nil
}
