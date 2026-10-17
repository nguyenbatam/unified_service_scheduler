package httpapi

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/booking"
	"github.com/nguyenbatam/unified_service_scheduler/backend/internal/store"
)

// A page token round-trips only within the listing and filters it was made for: a token replayed
// against another listing (or mode, e.g. upcoming vs past), other filters, or tampered with is
// refused instead of seeking into a different order.
func TestPageTokenIsBoundToItsScope(t *testing.T) {
	t.Parallel()
	want := store.AppointmentCursor{StartAt: time.Date(2030, 1, 7, 9, 0, 0, 123, time.UTC), ID: uuid.New()}
	customer := uuid.New()
	upcoming := newPageScope("appointments:upcoming", idFilter(customer), "", "")
	token := upcoming.encode(appointmentCursorKeys(want)...)

	keys, err := upcoming.decode(token, 2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseAppointmentCursor(keys)
	if err != nil || !got.StartAt.Equal(want.StartAt) || got.ID != want.ID {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, want)
	}
	if keys, err := upcoming.decode("", 2); keys != nil || err != nil {
		t.Fatalf(`decode("") = %v, %v; want the first page`, keys, err)
	}

	for name, scope := range map[string]pageScope{
		"other mode":     newPageScope("appointments:past", idFilter(customer), "", ""),
		"no mode":        newPageScope("appointments:", idFilter(customer), "", ""),
		"other filter":   newPageScope("appointments:upcoming", idFilter(uuid.New()), "", ""),
		"filter moved":   newPageScope("appointments:upcoming", "", idFilter(customer), ""),
		"other listing":  newPageScope("vehicles", idFilter(customer)),
		"unfiltered too": newPageScope("appointments:upcoming"),
	} {
		if _, err := scope.decode(token, 2); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
	for name, bad := range map[string]string{
		"not base64":   "not-base64!",
		"not JSON":     base64.RawURLEncoding.EncodeToString([]byte("2030-01-07T09:00:00Z|" + want.ID.String())),
		"too few keys": upcoming.encode(cursorTime(want.StartAt)),
		"too long":     strings.Repeat("A", maxCursorLength+1),
		"old version":  base64.RawURLEncoding.EncodeToString([]byte(`["0","appointments:upcoming","` + upcoming.filter + `","x","y"]`)),
	} {
		if _, err := upcoming.decode(bad, 2); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
	keys, err = upcoming.decode(upcoming.encode("yesterday", "not-a-uuid"), 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseAppointmentCursor(keys); err == nil {
		t.Error("malformed keyset values accepted")
	}
}

// The longest keyset (a vehicle's owner, make, model at the booking limits' maximum lengths) still
// fits maxCursorLength — and decodes — in every kind of character, including the worst case: those
// JSON escapes to 6 bytes (a control character, U+2028). Lengths come from the booking limits, so
// raising one past the bound fails here. HTML characters are not escaped (one byte each).
func TestLongestPageTokenFits(t *testing.T) {
	t.Parallel()
	scope := newPageScope("vehicles", strings.Repeat("😀", booking.MaxVehicleSearch))
	size := map[string]int{}
	for _, r := range []string{"a", "<", "&", "😀", "\u2028", "\x01", "\x1f"} {
		c := store.VehicleCursor{
			Owner: strings.Repeat(r, booking.MaxCustomerName),
			Make:  strings.Repeat(r, booking.MaxVehicleName),
			Model: strings.Repeat(r, booking.MaxVehicleName),
			ID:    uuid.New(),
		}
		token := scope.encode(vehicleCursorKeys(c)...)
		size[r] = len(token)
		if len(token) > maxCursorLength {
			t.Fatalf("%q token of %d characters exceeds maxCursorLength %d", r, len(token), maxCursorLength)
		}
		keys, err := scope.decode(token, 4)
		if err != nil {
			t.Fatalf("%q: %v", r, err)
		}
		if got, err := parseVehicleCursor(keys); err != nil || got != c {
			t.Fatalf("%q round trip = %+v, %v; want %+v", r, got, err, c)
		}
	}
	if size["<"] != size["a"] || size["&"] != size["a"] {
		t.Errorf("HTML characters are escaped: %d / %d bytes vs %d", size["<"], size["&"], size["a"])
	}
	if size["\x01"] < size["😀"] || size["\u2028"] < size["😀"] {
		t.Errorf("the 6-byte escapes are not the worst case: %v", size)
	}
}
