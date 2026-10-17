package integration_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

// Extend's body is a delta, so the API requires If-Match: a client retrying after a lost response
// gets 412 instead of extending twice, and one that sends no version (or "*") is told so (428)
// before anything changes.
func TestHTTPExtendRequiresIfMatch(t *testing.T) {
	t.Parallel()
	_, st, svc, ctx := openTestDB(t)
	today := londonDay(t, 0)
	job, err := bookAt(t, svc, seed.DealerRiver, newVehicle(t, svc), seed.ServiceOil, londonOn(t, today, 9, 0))
	if err != nil {
		t.Fatal(err)
	}
	clock := serviceOn(t, st, today, 9, 0)
	if job, err = clock.CheckIn(ctx, staffDispatcher, job.ID, 0); err != nil {
		t.Fatal(err)
	}
	admin := login(t, newAPI(st, clock), seed.DemoAdminEmail, seed.DemoAdminPassword)
	path := "/api/v1/appointments/" + job.ID.String() + "/extend"
	body := map[string]any{"minutes": 30}
	etag := func(v int) map[string]string { return map[string]string{"If-Match": `"` + strconv.Itoa(v) + `"`} }

	admin.expect(admin.do(http.MethodPost, path, body, nil), http.StatusPreconditionRequired, nil)
	admin.expect(admin.do(http.MethodPost, path, body, map[string]string{"If-Match": "*"}), http.StatusPreconditionRequired, nil)
	if got := mustGet(t, svc, job.ID); !got.EndAt.Equal(job.EndAt) {
		t.Fatalf("end moved to %s without a version", got.EndAt)
	}

	var extended domain.Appointment
	admin.expect(admin.do(http.MethodPost, path, body, etag(job.Version)), http.StatusOK, &extended)
	if want := londonOn(t, today, 10, 0); !extended.EndAt.Equal(want) {
		t.Fatalf("end = %s, want %s", extended.EndAt, want)
	}
	// The same request retried (its response lost) is now stale: nothing is added twice.
	admin.expect(admin.do(http.MethodPost, path, body, etag(job.Version)), http.StatusPreconditionFailed, nil)
	if got := mustGet(t, svc, job.ID); !got.EndAt.Equal(extended.EndAt) {
		t.Fatalf("retry moved the end again to %s", got.EndAt)
	}
}
