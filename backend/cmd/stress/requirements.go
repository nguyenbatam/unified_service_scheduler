package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

// The run ends with a requirements matrix: every requirement of the brief (Keyloop Scenario A) and
// every "build for the future" quality it names, each with the evidence the run gathered and a
// verdict. It is printed whatever the outcome (a failed run shows what it proved before failing)
// and written as JSON with -report-json.

// requirement is one row of the matrix.
type requirement struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// The rows, in report order. R* are the brief's core requirements, Q* the qualities it asks the
// design to consider (scalability, performance, reliability, maintainability via the optimisation
// it chose, observability).
var requirements = []requirement{
	{"R1", "Resource-constrained booking: a vehicle + service type(s) + dealership at a desired time"},
	{"R2a", "Bay AND qualified technician free for the ENTIRE duration (never double-booked, even under concurrency)"},
	{"R2b", "Real-time availability check: what is offered is bookable, a refusal means truly full, every instance agrees"},
	{"R3", "Confirmed appointment record persisted (customer, vehicle, technician, bay)"},
	{"Q1", "Concurrency & idempotency: exact winners per contended slot, one appointment per Idempotency-Key"},
	{"Q2", "Scalability: stateless replicas behind the gateway all serve writes and agree"},
	{"Q3", "Performance: booking / availability latency under load within budget"},
	{"Q4", "Reliability: events published (outbox drained), live board fan-out complete, reads stay up"},
	{"Q5", "Optimisation: resources used well (best-fit technician, quick lane kept, utilisation)"},
	{"Q6", "Observability: server metrics agree with what clients saw"},
	{"Q7", "Many users: concurrent customers book their own cars; nobody sees or books another's"},
}

type verdict string

const (
	verdictPass verdict = "PASS"
	verdictFail verdict = "FAIL"
	verdictSkip verdict = "SKIP"
	// verdictInfo is a fact the run reports (a ratio, a distribution) that proves nothing on its
	// own: it never makes a row PASS.
	verdictInfo verdict = "INFO"
)

// evidence is one fact gathered for a requirement.
type evidence struct {
	Verdict verdict `json:"verdict"`
	Source  string  `json:"source"` // phase or check that produced it
	Detail  string  `json:"detail"`
}

// matrix collects evidence per requirement (safe for concurrent use).
type matrix struct {
	mu        sync.Mutex
	started   time.Time
	rows      map[string][]evidence
	headlines map[string]string // one number per requirement for the compact summary
	metrics   map[string]any    // headline numbers for the JSON report
}

func newMatrix() *matrix {
	return &matrix{started: time.Now(), rows: map[string][]evidence{}, headlines: map[string]string{}, metrics: map[string]any{}}
}

func (m *matrix) add(id string, v verdict, source, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id] = append(m.rows[id], evidence{Verdict: v, Source: source, Detail: fmt.Sprintf(format, args...)})
}

func (m *matrix) pass(id, source, format string, args ...any) {
	m.add(id, verdictPass, source, format, args...)
}

func (m *matrix) fail(id, source, format string, args ...any) {
	m.add(id, verdictFail, source, format, args...)
}

func (m *matrix) skip(id, source, format string, args ...any) {
	m.add(id, verdictSkip, source, format, args...)
}

// info records a fact: reported, never a reason to PASS.
func (m *matrix) info(id, source, format string, args ...any) {
	m.add(id, verdictInfo, source, format, args...)
}

// infoUnless records a fact that should hold by construction: INFO while ok, FAIL if it ever
// does not (then a bound, or the pass that vouches for it, is wrong).
func (m *matrix) infoUnless(id, source string, ok bool, format string, args ...any) {
	if ok {
		m.info(id, source, format, args...)
	} else {
		m.fail(id, source, format, args...)
	}
}

// headline sets the one number the compact summary shows for requirement id.
func (m *matrix) headline(id, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.headlines[id] = fmt.Sprintf(format, args...)
}

// addHeadline appends to requirement id's headline ("; " between), so several phases can each
// add their number.
func (m *matrix) addHeadline(id, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.headlines[id]; h != "" {
		m.headlines[id] = h + "; " + fmt.Sprintf(format, args...)
		return
	}
	m.headlines[id] = fmt.Sprintf(format, args...)
}

// prependHeadline puts a number in front of requirement id's headline ("; " between).
func (m *matrix) prependHeadline(id, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h := m.headlines[id]; h != "" {
		m.headlines[id] = fmt.Sprintf(format, args...) + "; " + h
		return
	}
	m.headlines[id] = fmt.Sprintf(format, args...)
}

// check records pass when ok, else fail, with the same detail.
func (m *matrix) check(id, source string, ok bool, format string, args ...any) bool {
	if ok {
		m.pass(id, source, format, args...)
	} else {
		m.fail(id, source, format, args...)
	}
	return ok
}

// metric records a headline number for the JSON report.
func (m *matrix) metric(name string, v any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.metrics[name] = v
}

// rowVerdict is FAIL if any evidence failed, PASS if any passed, else SKIP (nothing ran, or only
// skips and facts).
func rowVerdict(ev []evidence) verdict {
	out := verdictSkip
	for _, e := range ev {
		switch e.Verdict {
		case verdictFail:
			return verdictFail
		case verdictPass:
			out = verdictPass
		}
	}
	return out
}

// validRequirement reports whether id names a row of the matrix.
func validRequirement(id string) bool {
	return slices.ContainsFunc(requirements, func(r requirement) bool { return r.ID == id })
}

// essentialEvidence are the named checks "-require all" asks for on top of every row: a row
// passes on any one piece of evidence, so these would otherwise SKIP unseen behind the rest
// (Q4/ws: the fan-out check, skipped with -ws-clients 0; Q4/ws order: the per-appointment order
// of the live board, skipped unless -mixed-ops changes appointments mid-run; Q5/beats first fit
// and Q5/repair: the quality day, which alone shows the engine's optimisation pays — the fill
// day only shows it is no worse than first fit; Q5/fit gain: the fit days, the same comparison on
// random demand sized to what the shop holds).
var essentialEvidence = []string{"Q4/ws", "Q4/ws order", "Q5/beats first fit", "Q5/repair", "Q5/fit gain"}

// parseRequire expands and validates a -require list: "all" (every row plus essentialEvidence),
// a row id ("R2b": the row must PASS), or a row id and a source ("Q4/ws order": that named
// evidence must have run and passed).
func parseRequire(items []string) ([]string, error) {
	var out []string
	for _, item := range items {
		if item == "all" {
			for _, r := range requirements {
				out = append(out, r.ID)
			}
			out = append(out, essentialEvidence...)
			continue
		}
		id, source, named := strings.Cut(item, "/")
		if !validRequirement(id) || (named && strings.TrimSpace(source) == "") {
			return nil, fmt.Errorf("-require: unknown requirement %q (want all, ids of the matrix: R1, R2a, R2b, R3, Q1 … Q7, or id/source such as Q4/ws order)", item)
		}
		out = append(out, item)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// enforceRequired turns every required item (-require) that gathered no PASS into a failure: a
// row that is SKIP (nothing ran, or every piece of evidence was skipped) proves nothing, and a run
// asked to prove it must not exit 0; likewise a named piece of evidence ("Q4/ws order") that was
// skipped or never ran. It returns the items it failed.
func (m *matrix) enforceRequired(required []string) []string {
	var unmet []string
	for _, item := range required {
		id, source, named := strings.Cut(item, "/")
		m.mu.Lock()
		ev := m.rows[id]
		if named {
			ev = slices.DeleteFunc(slices.Clone(ev), func(e evidence) bool { return e.Source != source })
		}
		v := rowVerdict(ev)
		m.mu.Unlock()
		if v != verdictSkip {
			continue
		}
		unmet = append(unmet, item)
		if named {
			m.fail(id, "require", "required (-require / STRESS_REQUIRE %q) but the %q check was skipped or never ran", item, source)
		} else {
			m.fail(id, "require", "required (-require / STRESS_REQUIRE) but no evidence passed: every check of it was skipped or none ran")
		}
	}
	return unmet
}

// failed lists the requirements whose verdict is FAIL.
func (m *matrix) failed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range requirements {
		if rowVerdict(m.rows[r.ID]) == verdictFail {
			out = append(out, r.ID)
		}
	}
	return out
}

// rowSummary is one requirement's compact line: verdict, counts, its headline, and the names of
// the checks that were skipped (each could hide behind the row's other evidence).
type rowSummary struct {
	id, title        string
	verdict          verdict
	pass, fail, info int
	skipped          []string
	headline         string
}

func (m *matrix) summaries() []rowSummary {
	var out []rowSummary
	for _, r := range requirements {
		ev := m.rows[r.ID]
		s := rowSummary{id: r.ID, title: r.Title, verdict: rowVerdict(ev), headline: m.headlines[r.ID]}
		for _, e := range ev {
			switch e.Verdict {
			case verdictPass:
				s.pass++
			case verdictFail:
				s.fail++
			case verdictInfo:
				s.info++
			case verdictSkip:
				if !slices.Contains(s.skipped, e.Source) {
					s.skipped = append(s.skipped, e.Source)
				}
			}
		}
		out = append(out, s)
	}
	return out
}

func (s rowSummary) counts() string {
	out := fmt.Sprintf("%d pass, %d fail, %d info", s.pass, s.fail, s.info)
	if len(s.skipped) > 0 {
		out += ", skipped: " + strings.Join(s.skipped, "; ")
	}
	return out
}

// print writes the matrix: first one line per requirement (verdict, headline, counts, the
// skipped checks), then one block per requirement with each piece of evidence.
func (m *matrix) print() {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Println()
	fmt.Println("════════ requirements (Keyloop Scenario A) ════════")
	for _, s := range m.summaries() {
		head := ""
		if s.headline != "" {
			head = s.headline + " — "
		}
		fmt.Printf("[%s] %-4s %s%s\n", s.verdict, s.id, head, s.counts())
	}
	fmt.Println("──────── evidence ────────")
	for _, r := range requirements {
		ev := m.rows[r.ID]
		fmt.Printf("[%s] %-4s %s\n", rowVerdict(ev), r.ID, r.Title)
		for _, e := range ev {
			fmt.Printf("        %-4s %-12s %s\n", e.Verdict, e.Source, e.Detail)
		}
		if len(ev) == 0 {
			fmt.Println("        (no phase of this run gathered evidence)")
		}
	}
	fmt.Println("═══════════════════════════════════════════════════════════")
}

// appendMarkdown appends the compact matrix as a Markdown table to path ("" = skip), e.g. the CI
// job summary ($GITHUB_STEP_SUMMARY).
func (m *matrix) appendMarkdown(path, title string, runErr error) error {
	if path == "" {
		return nil
	}
	m.mu.Lock()
	var b strings.Builder
	fmt.Fprintf(&b, "\n### %s\n\n| | Requirement | Headline | Evidence |\n|---|---|---|---|\n", title)
	for _, s := range m.summaries() {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", s.verdict, s.id, mdCell(s.headline), mdCell(s.counts()))
	}
	m.mu.Unlock()
	if runErr != nil {
		fmt.Fprintf(&b, "\nRun error: `%s`\n", mdCell(runErr.Error()))
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G302/G304: a report path the caller chose
	if err != nil {
		return err
	}
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// mdCell keeps text inside one Markdown table cell.
func mdCell(s string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "`", "'").Replace(s)
}

// jsonReport is what -report-json writes.
type jsonReport struct {
	StartedAt    time.Time             `json:"startedAt"`
	DurationSecs float64               `json:"durationSeconds"`
	Passed       bool                  `json:"passed"`
	Requirements []jsonRequirement     `json:"requirements"`
	Metrics      map[string]any        `json:"metrics"`
	Error        string                `json:"error,omitempty"`
	Evidence     map[string][]evidence `json:"-"`
}

type jsonRequirement struct {
	requirement
	Verdict  verdict    `json:"verdict"`
	Evidence []evidence `json:"evidence"`
}

// writeJSON writes the report to path ("" = skip).
func (m *matrix) writeJSON(path string, runErr error) error {
	if path == "" {
		return nil
	}
	m.mu.Lock()
	rep := jsonReport{StartedAt: m.started, DurationSecs: time.Since(m.started).Seconds(), Metrics: m.metrics}
	passed := runErr == nil
	for _, r := range requirements {
		ev := slices.Clone(m.rows[r.ID])
		v := rowVerdict(ev)
		passed = passed && v != verdictFail
		rep.Requirements = append(rep.Requirements, jsonRequirement{requirement: r, Verdict: v, Evidence: ev})
	}
	m.mu.Unlock()
	rep.Passed = passed
	if runErr != nil {
		rep.Error = runErr.Error()
	}
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644) //nolint:gosec // G306: a report, not a secret
}

// pct formats part/whole as a percentage ("-" for an empty whole).
func pct(part, whole int) string {
	if whole == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(part)/float64(whole))
}
