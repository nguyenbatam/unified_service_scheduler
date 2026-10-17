package integration_test

import (
	"net/http"
	"testing"

	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/domain"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/seed"
)

func TestHTTPDealershipCalendar(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	handler := newAPI(st, svc)
	admin := login(t, handler, seed.DemoAdminEmail, seed.DemoAdminPassword)
	dispatcher := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	base := "/api/v1/admin/dealerships/" + seed.DealerNorth.String()
	tomorrow := londonDay(t, 1).String()

	dispatcher.expect(dispatcher.do(http.MethodGet, base+"/calendar", nil, nil), http.StatusForbidden, nil)

	var cal domain.DealershipCalendar
	rec := admin.do(http.MethodGet, base+"/calendar", nil, nil)
	admin.expect(rec, http.StatusOK, &cal)
	etag := rec.Header().Get("ETag")
	if etag == "" || len(cal.Weekly) != 0 || len(cal.Closures) != 0 {
		t.Fatalf("empty calendar = %+v etag %q", cal, etag)
	}

	// Override two weekdays that hold no booking: the seed books its demo jobs today (or tomorrow
	// past 08:00), and closing that weekday would rightly be refused as stranding them (409).
	short, closed := freeWeekdays(t)
	rec = admin.do(http.MethodPut, base+"/hours", map[string]any{"weekly": []map[string]any{
		{"weekday": short, "closed": false, "openTime": "08:00", "closeTime": "12:00"},
		{"weekday": closed, "closed": true},
	}}, map[string]string{"If-Match": etag})
	admin.expect(rec, http.StatusOK, &cal)
	if len(cal.Weekly) != 2 || cal.Weekly[0].Weekday != short || cal.Weekly[1].Weekday != closed || !cal.Weekly[1].Closed {
		t.Fatalf("weekly = %+v", cal.Weekly)
	}
	// The old ETag is stale now that the dealership version moved.
	admin.expect(admin.do(http.MethodPut, base+"/hours", map[string]any{"weekly": []any{}},
		map[string]string{"If-Match": etag}), http.StatusPreconditionFailed, nil)
	admin.expect(admin.do(http.MethodPut, base+"/hours", map[string]any{}, nil), http.StatusBadRequest, nil)
	admin.expect(admin.do(http.MethodPut, base+"/hours", map[string]any{"weekly": []map[string]any{
		{"weekday": 9, "closed": true},
	}}, nil), http.StatusBadRequest, nil)

	admin.expect(admin.do(http.MethodPost, base+"/closures", map[string]any{"date": tomorrow, "reason": "Stock take"}, nil),
		http.StatusCreated, &cal)
	if len(cal.Closures) != 1 || cal.Closures[0].Reason != "Stock take" {
		t.Fatalf("closures = %+v", cal.Closures)
	}
	admin.expect(admin.do(http.MethodPost, base+"/closures", map[string]any{"date": tomorrow}, nil), http.StatusConflict, nil)
	admin.expect(admin.do(http.MethodPost, base+"/closures", map[string]any{"date": "tomorrow"}, nil), http.StatusBadRequest, nil)

	var avail domain.Availability
	admin.expect(admin.do(http.MethodGet, "/api/v1/availability?dealershipId="+seed.DealerNorth.String()+
		"&vehicleId="+seed.VehicleA.String()+"&serviceTypeId="+seed.ServiceOil.String()+"&date="+tomorrow, nil, nil),
		http.StatusOK, &avail)
	if !avail.Closed || avail.ClosedReason != "Stock take" || len(avail.Slots) != 0 {
		t.Fatalf("availability = %+v", avail)
	}

	admin.expect(admin.do(http.MethodDelete, base+"/closures/"+tomorrow, nil, nil), http.StatusOK, &cal)
	if len(cal.Closures) != 0 {
		t.Fatalf("closures after delete = %+v", cal.Closures)
	}
	admin.expect(admin.do(http.MethodDelete, base+"/closures/"+tomorrow, nil, nil), http.StatusNotFound, nil)
}

// freeWeekdays returns two ISO weekdays, in order, that are neither the suite's today nor
// tomorrow: no seed booking (today, or tomorrow when seeded late) and no closure the test adds for
// tomorrow falls on them, whatever day the suite runs.
func freeWeekdays(t *testing.T) (first, second int16) {
	t.Helper()
	busy := map[int16]bool{londonDay(t, 0).ISOWeekday(): true, londonDay(t, 1).ISOWeekday(): true}
	var free []int16
	for wd := int16(1); wd <= 7; wd++ {
		if !busy[wd] {
			free = append(free, wd)
		}
	}
	return free[0], free[1]
}

func TestHTTPReinstateIsStaffOnly(t *testing.T) {
	t.Parallel()
	st, svc, _ := openTestStore(t)
	handler := newAPI(st, svc)
	customer := login(t, handler, seed.DemoCustomerEmail, seed.DemoCustomerPassword)
	dispatcher := login(t, handler, seed.DemoDispatcherEmail, seed.DemoDispatcherPassword)
	appt, err := bookAt(t, svc, seed.DealerNorth, seed.VehicleA, seed.ServiceOil, londonOn(t, londonDay(t, 1), 8, 0))
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/appointments/" + appt.ID.String() + "/reinstate"
	customer.expect(customer.do(http.MethodPost, path, nil, nil), http.StatusForbidden, nil)
	// Not a no-show: nothing to reinstate.
	dispatcher.expect(dispatcher.do(http.MethodPost, path, nil, nil), http.StatusConflict, nil)

	// The demo dispatcher works at Northside: Riverside work does not exist for them.
	river, err := bookAt(t, svc, seed.DealerRiver, seed.VehicleB, seed.ServiceOil, londonOn(t, londonDay(t, 1), 9, 0))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.expect(dispatcher.do(http.MethodPost, "/api/v1/appointments/"+river.ID.String()+"/check-in", nil, nil), http.StatusNotFound, nil)
	dispatcher.expect(dispatcher.do(http.MethodGet, "/api/v1/schedule?dealershipId="+seed.DealerRiver.String()+"&date="+londonOn(t, londonDay(t, 1), 8, 0).Format("2006-01-02"), nil, nil), http.StatusForbidden, nil)
}
