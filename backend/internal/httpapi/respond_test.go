package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

func TestParseIfMatch(t *testing.T) {
	tests := []struct {
		header  string
		want    int
		wantErr bool
	}{
		{header: "", want: 0},
		{header: "   ", want: 0},
		{header: "*", want: 0},
		{header: `"3"`, want: 3},
		{header: ` "42" `, want: 42},
		{header: `"3",`, want: 3}, // one-element list (empty list elements are allowed)
		{header: `"3`, wantErr: true},
		{header: `3"`, wantErr: true},
		{header: `3`, wantErr: true},
		{header: `W/"3"`, wantErr: true},
		{header: `""`, wantErr: true},
		{header: `"0"`, wantErr: true},
		{header: `"-1"`, wantErr: true},
		{header: `"03"`, wantErr: true},
		{header: `"abc"`, wantErr: true},
		{header: `"3", "4"`, wantErr: true},
		{header: `"3", *`, wantErr: true},
		{header: `"99999999999"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.header, func(t *testing.T) {
			got, err := parseIfMatch(tt.header)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFailMasksInternalDomainErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{name: "internal", err: domain.NewError(domain.CodeInternal, "pq: relation \"secret_table\" does not exist"), wantStatus: 500, wantMessage: "internal error"},
		{name: "unmapped code", err: domain.NewError("weird", "stack trace here"), wantStatus: 500, wantMessage: "internal error"},
		{name: "raw error", err: errors.New("dial tcp 10.0.0.5:5432"), wantStatus: 500, wantMessage: "internal error"},
		{name: "client error passes through", err: domain.NewError(domain.CodeConflict, "slot taken"), wantStatus: 409, wantMessage: "slot taken"},
	}
	s := &Server{log: slog.New(slog.DiscardHandler)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/x", nil)
			s.fail(c, tt.err)
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tt.wantStatus || body.Error.Message != tt.wantMessage {
				t.Fatalf("got %d %q, want %d %q", rec.Code, body.Error.Message, tt.wantStatus, tt.wantMessage)
			}
			if tt.wantStatus == 500 && body.Error.Code != domain.CodeInternal {
				t.Fatalf("code = %q, want internal", body.Error.Code)
			}
		})
	}
}
