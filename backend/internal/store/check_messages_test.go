package store

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
)

// A CHECK failure is a 400 with a readable message; the constraint name stays server-side.
func TestTranslateCheckViolation(t *testing.T) {
	tests := []struct {
		constraint string
		want       string
	}{
		{constraint: "dealerships_staging_window_minutes_check", want: "stagingWindowMinutes must be 15, 30, or 60"},
		{constraint: "dealership_hours_shape", want: "a weekday is either closed or open with closeTime after openTime"},
		{constraint: "dealerships_no_show_grace_minutes_check", want: "noShowGraceMinutes must be 5–240"},
		{constraint: "appointments_max_span", want: "an appointment cannot span more than 24 hours"},
		{constraint: "some_future_secret_check", want: "value is out of range"},
		{constraint: "", want: "value is out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.constraint, func(t *testing.T) {
			err := translate(&pgconn.PgError{Code: "23514", ConstraintName: tt.constraint})
			domainErr, ok := domain.AsError(err)
			if !ok || domainErr.Code != domain.CodeInvalid || domainErr.Message != tt.want {
				t.Fatalf("translate = %v (%+v), want invalid %q", err, domainErr, tt.want)
			}
			if tt.constraint != "" && strings.Contains(domainErr.Message, tt.constraint) {
				t.Fatalf("message %q leaks the constraint name", domainErr.Message)
			}
			if !strings.Contains(err.Error(), tt.constraint) {
				t.Fatalf("wrapping error %q lost the constraint name for the server log", err)
			}
		})
	}
}
