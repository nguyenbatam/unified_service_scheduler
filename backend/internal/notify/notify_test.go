package notify

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

func TestMask(t *testing.T) {
	for in, want := range map[string]string{
		"amelia.clarke@example.com": "a***@example.com",
		"07700 900111":              "***11",
		"7":                         "***",
	} {
		if got := Mask(in); got != want {
			t.Errorf("Mask(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMessageChannels(t *testing.T) {
	m := message{kind: domain.KindConfirmation, appointmentID: uuid.New(), start: time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC),
		dealer: "Riverside", vehicle: "2022 BMW 3 Series", service: "Oil Change", customer: "Amelia Clarke"}
	rows := m.rows("a@example.com", "0770", "evt")
	if len(rows) != 1 || rows[0].Channel != domain.ChannelEmail || rows[0].DedupeKey != "evt|email" ||
		!strings.Contains(rows[0].Body, "Hello Amelia") || !strings.Contains(rows[0].Body, "Mon 19 Oct 2026 at 09:30") {
		t.Fatalf("confirmation rows = %+v", rows)
	}
	m.kind = domain.KindReminder2h
	if rows := m.rows("a@example.com", "0770", "r"); rows[0].Channel != domain.ChannelSMS || rows[0].Recipient != "0770" {
		t.Fatalf("reminder prefers sms: %+v", rows)
	}
	if rows := m.rows("a@example.com", " ", "r"); rows[0].Channel != domain.ChannelEmail {
		t.Fatalf("reminder falls back to email: %+v", rows)
	}
	if rows := m.rows("", "", "r"); rows != nil {
		t.Fatalf("no contact, no row: %+v", rows)
	}
}

func TestWebhookSender(t *testing.T) {
	var got webhookBody
	var key string
	status := http.StatusAccepted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Idempotency-Key")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
	}))
	defer srv.Close()

	n := store.Notification{
		ID: uuid.New(), AppointmentID: uuid.New(), Kind: domain.KindConfirmation, Channel: domain.ChannelEmail,
		Recipient: "a@example.com", Subject: "s", Body: "b",
	}
	w := NewWebhookSender(srv.URL, 50)
	if err := w.Send(t.Context(), n); err != nil {
		t.Fatal(err)
	}
	if key != n.ID.String() || got.Recipient != "a@example.com" || got.Kind != "confirmation" || got.Channel != "email" {
		t.Fatalf("key=%q body=%+v", key, got)
	}
	status = http.StatusBadGateway
	if err := w.Send(t.Context(), n); err == nil {
		t.Fatal("non-2xx must be an error (retried)")
	}
}

// Send errors are logged and stored in notifications.last_error: they name the webhook by scheme
// and host, never its path, query, or userinfo (which may carry a token).
func TestWebhookSenderErrorsRedactURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // connection refused: the transport error path
	n := store.Notification{ID: uuid.New(), AppointmentID: uuid.New(), Kind: domain.KindConfirmation, Channel: domain.ChannelEmail, Recipient: "a@example.com"}
	for name, base := range map[string]string{"status": srv.URL, "transport": closed.URL} {
		target := strings.Replace(base, "http://", "http://user:pa55word@", 1) + "/hooks/s3cret-path?token=s3cret-query"
		err := NewWebhookSender(target, 2).Send(t.Context(), n)
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		for _, secret := range []string{"pa55word", "s3cret-path", "s3cret-query"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: error leaks %q: %v", name, secret, err)
			}
		}
		if !strings.Contains(err.Error(), strings.TrimPrefix(base, "http://")) {
			t.Fatalf("%s: error %q does not name the host", name, err)
		}
	}
	if err := (WebhookSender{URL: "http://h/%zz?token=s3cret", Client: http.DefaultClient}).Send(t.Context(), n); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("invalid URL: %v", err)
	}
}

// A whole batch is sent at once: the transport must keep that many idle connections to the gateway
// (http.DefaultTransport keeps 2 per host) and bound each stage of a request.
func TestWebhookSenderTransport(t *testing.T) {
	tr, ok := NewWebhookSender("http://gateway.invalid", DefaultConfig().BatchSize).Client.Transport.(*http.Transport)
	if !ok || tr == http.DefaultTransport {
		t.Fatalf("transport = %T, want a dedicated *http.Transport", tr)
	}
	if tr.MaxIdleConnsPerHost < DefaultConfig().BatchSize || tr.MaxIdleConns < tr.MaxIdleConnsPerHost {
		t.Fatalf("idle conns per host %d (total %d), want >= batch size %d", tr.MaxIdleConnsPerHost, tr.MaxIdleConns, DefaultConfig().BatchSize)
	}
	if tr.TLSHandshakeTimeout <= 0 || tr.ResponseHeaderTimeout <= 0 || tr.IdleConnTimeout <= 0 {
		t.Fatalf("transport timeouts unset: %+v", tr)
	}
}

func TestConfigValidate(t *testing.T) {
	lease := 30 * time.Second // worker.DefaultMaintenanceConfig().Lease
	if err := DefaultConfig().Validate(lease); err != nil {
		t.Fatalf("defaults must fit the default lease: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"no batch":            func(c *Config) { c.BatchSize = 0 },
		"no poll interval":    func(c *Config) { c.PollInterval = 0 },
		"no attempts":         func(c *Config) { c.MaxAttempts = 0 },
		"no time to send":     func(c *Config) { c.SendTimeout = claimTimeout + staleCheckTimeout },
		"outlives the lease":  func(c *Config) { c.SendTimeout = lease - bookkeepingTimeout },
		"bookkeeping at edge": func(c *Config) { c.SendTimeout = lease - bookkeepingTimeout - leaseMargin + time.Millisecond },
	} {
		cfg := DefaultConfig()
		mutate(&cfg)
		if cfg.Validate(lease) == nil {
			t.Errorf("%s: %+v must be refused", name, cfg)
		}
	}
	edge := DefaultConfig()
	edge.SendTimeout = lease - bookkeepingTimeout - leaseMargin
	if err := edge.Validate(lease); err != nil {
		t.Fatalf("send + bookkeeping + margin == lease fits: %v", err)
	}
}

func TestConfigDrainTimeout(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.DrainTimeout(); got != cfg.SendTimeout+bookkeepingTimeout {
		t.Fatalf("drain timeout = %s", got)
	}
}

// A name of only whitespace used to panic (strings.Fields(name)[0]) and kill the consumer.
func TestTextBlankCustomerName(t *testing.T) {
	for _, name := range []string{"", "  ", "\t\n", "　"} {
		m := message{kind: domain.KindConfirmation, start: time.Date(2026, 10, 19, 9, 30, 0, 0, time.UTC), customer: name}
		if _, body := m.text(); !strings.HasPrefix(body, "Hello, ") {
			t.Errorf("customer %q: body = %q, want the plain greeting", name, body)
		}
	}
	m := message{kind: domain.KindConfirmation, customer: "  Amelia  Clarke "}
	if _, body := m.text(); !strings.HasPrefix(body, "Hello Amelia, ") {
		t.Errorf("body = %q, want the first name", body)
	}
}

// Zones are loaded once per name; an unknown one falls back to UTC.
func TestLocationCache(t *testing.T) {
	s := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), DefaultConfig())
	london := s.location("Europe/London")
	if london.String() != "Europe/London" || s.location("Europe/London") != london {
		t.Fatalf("location = %v, want one cached Europe/London", london)
	}
	if got := s.location("Not/AZone"); got != time.UTC {
		t.Fatalf("unknown zone = %v, want UTC", got)
	}
}
