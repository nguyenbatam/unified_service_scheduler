package integration_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/auth"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/httpapi"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/realtime"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// testSecret signs every test API's tokens.
const testSecret = "test-secret"

// testAPI is one API instance over a test's store and service. It is an http.Handler, and embeds
// the server for calls such as InvalidateUser.
type testAPI struct {
	*httpapi.Server
	handler http.Handler
	auth    *auth.Service
	st      *store.Store
	svc     *booking.Service
}

func (a *testAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

// newAPI serves st and svc as instance "test": its own auth.Service, no WebSocket hub.
func newAPI(st *store.Store, svc *booking.Service, opts ...httpapi.Option) *testAPI {
	return serveAPI(st, svc, auth.New(testSecret), nil, "test", opts...)
}

// newAPIWithHub is newAPI with a WebSocket hub (httpapi.New installs its session validator).
func newAPIWithHub(st *store.Store, svc *booking.Service, hub *realtime.Hub, opts ...httpapi.Option) *testAPI {
	return serveAPI(st, svc, auth.New(testSecret), hub, "test", opts...)
}

// peer is another instance over the same database and service sharing a's signing key, so a
// token from one works on the other (as behind the gateway).
func (a *testAPI) peer(instance string, opts ...httpapi.Option) *testAPI {
	return serveAPI(a.st, a.svc, a.auth, nil, instance, opts...)
}

func serveAPI(st *store.Store, svc *booking.Service, authSvc *auth.Service, hub *realtime.Hub, instance string, opts ...httpapi.Option) *testAPI {
	srv := httpapi.New(svc, st, authSvc, hub, slog.Default(), instance, opts...)
	return &testAPI{Server: srv, handler: srv.Handler(), auth: authSvc, st: st, svc: svc}
}

// apiClient sends JSON requests to one handler, with a bearer token when it has one.
type apiClient struct {
	t       *testing.T
	handler http.Handler
	token   string
}

// on is the same session against another instance.
func (c apiClient) on(h http.Handler) apiClient {
	c.handler = h
	return c
}

func (c apiClient) do(method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatalf("encode %s %s body: %v", method, path, err)
		}
	}
	req := httptest.NewRequestWithContext(c.t.Context(), method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

// expect fails unless rec has status, then decodes its body into out (when not nil).
func (c apiClient) expect(rec *httptest.ResponseRecorder, status int, out any) {
	c.t.Helper()
	if rec.Code != status {
		c.t.Fatalf("status=%d want %d body=%s", rec.Code, status, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			c.t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
	}
}

func login(t *testing.T, handler http.Handler, email, password string) apiClient {
	t.Helper()
	c := apiClient{t: t, handler: handler}
	var session struct {
		Token string `json:"token"`
	}
	c.expect(c.do(http.MethodPost, "/api/v1/auth/login", map[string]string{"email": email, "password": password}, nil), http.StatusOK, &session)
	if session.Token == "" {
		t.Fatal("login returned an empty token")
	}
	c.token = session.Token
	return c
}
