package domain

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// The shell and Python checks cannot import Go, so scripts/lib/codes.py mirrors the codes; it must
// stay identical to the definitions here.
func TestScriptCodesMirrorDomain(t *testing.T) {
	raw, err := os.ReadFile("../../../scripts/lib/codes.py")
	if err != nil {
		t.Fatal(err)
	}
	// Each table is `NAME = {"key": code, ...}` (one or several lines).
	tables := map[string]map[string]int16{}
	for _, m := range regexp.MustCompile(`(?s)\n([A-Z_]+) = \{(.*?)\}`).FindAllStringSubmatch(string(raw), -1) {
		entries := map[string]int16{}
		for _, e := range regexp.MustCompile(`"([^"]+)": (\d+)`).FindAllStringSubmatch(m[2], -1) {
			n, err := strconv.ParseInt(e[2], 10, 16)
			if err != nil {
				t.Fatalf("codes.py %s[%q]: %v", m[1], e[1], err)
			}
			entries[e[1]] = int16(n)
		}
		tables[m[1]] = entries
	}
	check := func(table string, want map[string]int16) {
		t.Helper()
		got := tables[table]
		if len(got) != len(want) {
			t.Errorf("codes.py %s has %d codes, domain %d", table, len(got), len(want))
		}
		for name, code := range want {
			if got[name] != code {
				t.Errorf("codes.py %s[%q] = %d, domain %d", table, name, got[name], code)
			}
		}
	}
	check("EVENT", names(eventTypeNames))
	check("OUTBOX_STATUS", names(outboxStatusNames))
	check("NOTIFICATION_STATUS", names(notificationStatusNames))
	check("NOTIFICATION_KIND", names(notificationKindNames))
}

func names(table []string) map[string]int16 {
	out := map[string]int16{}
	for code, name := range table {
		if name != "" {
			out[name] = int16(code)
		}
	}
	return out
}
