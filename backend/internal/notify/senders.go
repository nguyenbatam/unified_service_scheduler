package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// LogSender records messages in the service log with the recipient masked (no provider
// configured: local stack and tests). Message bodies are not logged.
type LogSender struct {
	Log *slog.Logger
}

func (l LogSender) Send(_ context.Context, n store.Notification) error {
	l.Log.Info("notification", "id", n.ID, "kind", n.Kind, "channel", n.Channel,
		"recipient", Mask(n.Recipient), "appointment", n.AppointmentID, "subject", n.Subject)
	return nil
}

// Mask keeps the first character and the domain / last two digits: a***@example.com, ***11.
func Mask(recipient string) string {
	if at := strings.IndexByte(recipient, '@'); at > 0 {
		return recipient[:1] + "***" + recipient[at:]
	}
	if len(recipient) > 2 {
		return "***" + recipient[len(recipient)-2:]
	}
	return "***"
}

// WebhookSender POSTs each message as JSON to a delivery service (an SMS/email gateway, a CRM).
// The notification id is sent as Idempotency-Key so a retried delivery is not sent twice.
type WebhookSender struct {
	URL    string
	Client *http.Client
}

// NewWebhookSender posts to url with a client of its own. Flush sends a whole batch at once, so the
// transport keeps up to maxConns idle connections to the gateway (http.DefaultTransport keeps 2
// per host: the rest of each batch would open, and then drop, a fresh connection every time).
// Each send is bounded by the caller's context; the timeouts here bound the stages of a request.
func NewWebhookSender(url string, maxConns int) WebhookSender {
	maxConns = max(maxConns, 2)
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxConns,
		MaxIdleConnsPerHost:   maxConns,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return WebhookSender{URL: url, Client: &http.Client{Transport: transport, Timeout: 15 * time.Second}}
}

type webhookBody struct {
	ID            string `json:"id"`
	AppointmentID string `json:"appointmentId"`
	Kind          string `json:"kind"`    // by name: the receiver is a gateway without our code tables
	Channel       string `json:"channel"` // email | sms
	Recipient     string `json:"recipient"`
	Subject       string `json:"subject"`
	Body          string `json:"body"`
}

func (w WebhookSender) Send(ctx context.Context, n store.Notification) error {
	raw, err := json.Marshal(webhookBody{
		ID: n.ID.String(), AppointmentID: n.AppointmentID.String(), Kind: n.Kind.Name(), Channel: n.Channel.Name(),
		Recipient: n.Recipient, Subject: n.Subject, Body: n.Body,
	})
	if err != nil {
		return err
	}
	// Errors are logged and saved in notifications.last_error, and the URL may carry a token in
	// its path, query, or userinfo: they name the endpoint by scheme and host only.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("webhook %s: invalid URL", redactURL(w.URL))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", n.ID.String())
	resp, err := w.Client.Do(req)
	if err != nil {
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return fmt.Errorf("webhook %s: %w", redactURL(w.URL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s answered %d", redactURL(w.URL), resp.StatusCode)
	}
	return nil
}

// redactURL is raw reduced to scheme://host[:port]: no userinfo, path, query, or fragment.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unparsable URL)"
	}
	return u.Scheme + "://" + u.Host
}
