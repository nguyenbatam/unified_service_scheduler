package booking

import (
	"strings"
	"testing"
)

// Make and model are bounded in characters, as the web form counts them, not in UTF-8 bytes.
func TestValidVehicleName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain", "Corolla", true},
		{"empty", "", false},
		{"60 ASCII", strings.Repeat("a", MaxVehicleName), true},
		{"61 ASCII", strings.Repeat("a", MaxVehicleName+1), false},
		{"60 accented (120 bytes)", strings.Repeat("Š", MaxVehicleName), true},
		{"60 CJK (180 bytes)", strings.Repeat("車", MaxVehicleName), true},
		{"61 accented", strings.Repeat("Š", MaxVehicleName+1), false},
		{"invalid UTF-8", "Sk\xffoda", false},
		{"NUL", "Sk\x00oda", false},
	}
	for _, tc := range cases {
		if got := validVehicleName(tc.in); got != tc.want {
			t.Errorf("%s: validVehicleName = %v, want %v", tc.name, got, tc.want)
		}
	}
}
