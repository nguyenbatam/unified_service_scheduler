package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// apiClient talks to one base URL, signed in as auth's user (nil: anonymous). Clients of one user
// share its authSession (authclient.go): one cookie jar, one refresh at a time.
type apiClient struct {
	client *http.Client
	base   string
	auth   *authSession
}

// maxClientInFlight caps the requests one load generator keeps in flight: the bench drops a due
// request beyond it (and counts it), and every client keeps that many idle connections per host,
// so a burst that fell back to idle never has to re-dial (TIME_WAIT churn and connect latency
// would show up as server latency).
const maxClientInFlight = 4096

// clientIdleTimeout closes an idle connection before the gateway does (nginx keepalive_timeout
// 65 s, deploy/gateway/nginx-main.conf): a request written on a connection nginx is closing at
// that moment fails with a reset, which a run would count as a transport error.
const clientIdleTimeout = 55 * time.Second

func newAPI(base string) *apiClient {
	return &apiClient{
		base: base,
		client: &http.Client{
			// Whole-request cap; ResponseHeaderTimeout catches a backend that accepted the request
			// but never answers (the request is then retried with the same Idempotency-Key). It sits
			// above the backend's REQUEST_TIMEOUT (20 s), so the backend's own 503 busy reaches the
			// client rather than a client-side timeout, and below the gateway's proxy timeout (35 s).
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxIdleConns:          maxClientInFlight,
				MaxIdleConnsPerHost:   maxClientInFlight,
				IdleConnTimeout:       clientIdleTimeout,
				ResponseHeaderTimeout: 25 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
		},
	}
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

// do sends one JSON request. path may be absolute (another backend) or relative to the base URL.
// A signed-in client sends a current access token (refreshed first when due); a 401 renews the
// token once and, when the request is safe to resend (retrySafeAfter401), sends it once more. A
// session that cannot be renewed fails the request with errClientAuth.
func (a *apiClient) do(ctx context.Context, method, path string, body any, headers map[string]string) (response, error) {
	resp, used, err := a.send(ctx, method, path, body, headers, "")
	if err != nil || resp.code != http.StatusUnauthorized || used == "" {
		return resp, err
	}
	fresh, err := a.auth.renew(ctx, used)
	if err != nil {
		return response{}, err
	}
	if !retrySafeAfter401(method, headers) {
		a.auth.refused401.Add(1)
		return resp, nil
	}
	a.auth.retried401.Add(1)
	resp, _, err = a.send(ctx, method, path, body, headers, fresh)
	return resp, err
}

// send sends one request with token (or, when empty, the session's current one) and returns the
// token it used ("" when anonymous).
func (a *apiClient) send(ctx context.Context, method, path string, body any, headers map[string]string, token string) (response, string, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return response{}, "", err
		}
		rdr = bytes.NewReader(b)
	}
	url := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		url = a.base + path
	}
	if token == "" && a.auth != nil {
		var err error
		if token, err = a.auth.accessToken(ctx); err != nil {
			return response{}, "", err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return response{}, "", err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return response{}, token, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return response{code: resp.StatusCode, header: resp.Header}, token, err
	}
	return response{code: resp.StatusCode, header: resp.Header, body: raw}, token, nil
}

// asUser is a client for base acting as a's user (the same session: one jar, one refresh).
func (a *apiClient) asUser(base string) *apiClient {
	c := newAPI(base)
	c.auth = a.auth
	return c
}

// indefinite reports whether an answer leaves the outcome unknown: a transport error, or 429
// (throttled), 502/504 (the gateway lost the backend) or 503 (busy: the request deadline).
func indefinite(code int, err error) bool {
	if err != nil {
		return true
	}
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// probeAttempts is how many times sendUntilDefinite sends a probe before calling it busy.
const probeAttempts = 3

// sendUntilDefinite sends one request up to attempts times while the answer is indefinite,
// waiting Retry-After (capped at retryCap) or a doubling backoff from retryBase in between. Only
// use it for requests that are safe to resend (reads, or writes carrying an Idempotency-Key or
// If-Match). definite is false when every attempt stayed indefinite; resp is then the last one.
func sendUntilDefinite(ctx context.Context, a *apiClient, method, path string, body any, headers map[string]string, attempts int) (resp response, definite bool) {
	backoff := retryBase
	for attempt := 1; ; attempt++ {
		r, err := a.do(ctx, method, path, body, headers)
		if !indefinite(r.code, err) {
			return r, true
		}
		if err != nil {
			r.code = 0
		}
		// A lost session is not an answer: resending cannot help (the run is aborting).
		if attempt >= attempts || ctx.Err() != nil || errors.Is(err, errClientAuth) {
			return r, false
		}
		wait := retryAfter(r.header)
		if wait <= 0 {
			wait, backoff = backoff, backoff*2
		}
		select {
		case <-ctx.Done():
		case <-time.After(min(wait, retryCap)):
		}
	}
}

func getJSON[T any](ctx context.Context, a *apiClient, method, path string, body any) (T, error) {
	return getJSONWith[T](ctx, a, method, path, body, nil)
}

// getJSONWith is getJSON with extra request headers.
func getJSONWith[T any](ctx context.Context, a *apiClient, method, path string, body any, headers map[string]string) (T, error) {
	var zero T
	resp, err := a.do(ctx, method, path, body, headers)
	if err != nil {
		return zero, err
	}
	if resp.code < 200 || resp.code >= 300 {
		return zero, fmt.Errorf("%s %s -> %d %s", method, path, resp.code, truncate(string(resp.body), 160))
	}
	var out T
	if err := json.Unmarshal(resp.body, &out); err != nil {
		return zero, fmt.Errorf("decode %s: %w", path, err)
	}
	return out, nil
}

// maxListLimit is the API maximum page size of the lists the stress client reads in full
// (dealerships, service types, vehicles, technicians); /customers allows only 100.
const maxListLimit = 200

// getAll reads every page of a list endpoint: path (which may carry a query) with ?limit=limit —
// the listing's own maximum, or 0 for the server's default page size — following X-Next-Cursor
// until the last page. Every list is paged, so a plain GET would silently see only the first page.
func getAll[T any](ctx context.Context, a *apiClient, path string, limit int, headers map[string]string) ([]T, error) {
	first := path
	if limit > 0 {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		first = fmt.Sprintf("%s%slimit=%d", path, sep, limit)
	}
	var out []T
	for page, next := 0, first; ; page++ {
		resp, err := a.do(ctx, http.MethodGet, next, nil, headers)
		if err != nil {
			return nil, err
		}
		if resp.code != http.StatusOK {
			return nil, fmt.Errorf("GET %s -> %d %s", next, resp.code, truncate(string(resp.body), 160))
		}
		var items []T
		if err := json.Unmarshal(resp.body, &items); err != nil {
			return nil, fmt.Errorf("decode %s: %w", next, err)
		}
		out = append(out, items...)
		cursor := resp.header.Get("X-Next-Cursor")
		if cursor == "" {
			return out, nil
		}
		if page >= 10_000 {
			return nil, fmt.Errorf("GET %s: no last page after %d pages", path, page)
		}
		sep := "?"
		if strings.Contains(first, "?") {
			sep = "&"
		}
		next = first + sep + "cursor=" + url.QueryEscape(cursor)
	}
}

// login signs in as email with a session of its own (a fresh cookie jar): every client that acts
// as this user afterwards shares it (asUser). onLost is called once if the session is ever lost
// for good (the run aborts with that error); nil ignores it.
func (a *apiClient) login(ctx context.Context, email, password string, onLost func(error)) error {
	sess := newPasswordSession(a.base, email, password, a.client.Transport, onLost)
	if err := sess.login(ctx); err != nil {
		return err
	}
	a.auth = sess
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
