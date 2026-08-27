package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in   string
		want time.Time
	}{
		{"now", now},
		{"", now},
		{"-6h", now.Add(-6 * time.Hour)},
		{"6h", now.Add(-6 * time.Hour)}, // a bare duration reads as "ago"
		{"-90m", now.Add(-90 * time.Minute)},
		{"-7d", now.Add(-7 * 24 * time.Hour)},        // days: Go's parser cannot
		{"7d", now.Add(-7 * 24 * time.Hour)},         // bare reads as "ago"
		{"-1w", now.Add(-7 * 24 * time.Hour)},        // weeks
		{"-1d12h", now.Add(-36 * time.Hour)},         // compound
		{"-500ms", now.Add(-500 * time.Millisecond)}, // sub-second falls back
		{"2026-08-26T06:00:00Z", time.Date(2026, 8, 26, 6, 0, 0, 0, time.UTC)},
	}
	for _, tc := range tests {
		got, err := parseTime(tc.in, now)
		if err != nil {
			t.Errorf("parseTime(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("parseTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := parseTime("yesterday", now); err == nil {
		t.Error(`parseTime("yesterday") should fail`)
	}
}

func TestParseWindowRejectsInverted(t *testing.T) {
	if _, _, err := parseWindow("now", "-1h"); err == nil {
		t.Error("start after end should be rejected")
	}
	if _, _, err := parseWindow("-1h", "now"); err != nil {
		t.Errorf("a normal window should parse: %v", err)
	}
}

func TestResolveColumn(t *testing.T) {
	for _, in := range []string{"service", "Service", "ServiceName", "servicename"} {
		c, err := resolveColumn(in)
		if err != nil || c.SQL != "ServiceName" {
			t.Errorf("resolveColumn(%q) = (%+v, %v), want ServiceName", in, c, err)
		}
	}
	if _, err := resolveColumn("Duration"); err == nil {
		t.Error("a non-whitelisted column must be rejected (injection guard)")
	}
}

func TestParseMatch(t *testing.T) {
	m, err := parseMatch("service=agentloop")
	if err != nil {
		t.Fatal(err)
	}
	if m.Col.SQL != "ServiceName" || m.Value != "agentloop" {
		t.Errorf("got %+v", m)
	}
	// A value containing '=' keeps everything after the first '=' verbatim; it
	// is bound as a parameter, so an operation like "GET /a=b" is fine.
	m, err = parseMatch("name=GET /a=b")
	if err != nil || m.Value != "GET /a=b" {
		t.Errorf("value after first = must be kept whole: %+v, %v", m, err)
	}
	if _, err := parseMatch("nokey"); err == nil {
		t.Error("--match without = must fail")
	}
	if _, err := parseMatch("Duration=5"); err == nil {
		t.Error("--match on a non-whitelisted column must fail")
	}
}

func TestHumanDuration(t *testing.T) {
	tests := []struct {
		ns   float64
		want string
	}{
		{500, "500ns"},
		{1500, "1.5µs"},
		{2_000_000, "2.0ms"},
		{2_500_000_000, "2.50s"},
		{90_000_000_000, "1m30s"},
		{5_400_000_000_000, "1.5h"},
		{172_800_000_000_000, "2.0d"},
	}
	for _, tc := range tests {
		if got := humanDuration(tc.ns); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.ns, got, tc.want)
		}
	}
}

func TestHumanCount(t *testing.T) {
	tests := []struct {
		n    uint64
		want string
	}{
		{0, "0"}, {42, "42"}, {999, "999"}, {1000, "1,000"},
		{285044, "285,044"}, {1000000, "1,000,000"},
	}
	for _, tc := range tests {
		if got := humanCount(tc.n); got != tc.want {
			t.Errorf("humanCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestPctGuardsZero(t *testing.T) {
	if got := pct(1, 0); got != 0 {
		t.Errorf("pct(1,0) = %v, want 0 (no NaN in a report cell)", got)
	}
	if got := pct(1, 4); got != 25 {
		t.Errorf("pct(1,4) = %v, want 25", got)
	}
}

func TestMdEscape(t *testing.T) {
	// A pipe would end the table cell; a newline would end the row. Both come
	// from arbitrary application strings and must be neutralized.
	if got := mdEscape("a|b\nc"); got != "a\\|b c" {
		t.Errorf("mdEscape = %q", got)
	}
}

// renderReport is the whole user-facing contract, so pin down the pieces an
// agent relies on: the summary, the INFLIGHT normalization, a totals row that
// sums to 100%, and the self-time caveat.
func TestRenderReport(t *testing.T) {
	start := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(100 * time.Second) // round window makes INFLIGHT exact
	col, _ := resolveColumn("service")
	totals := Totals{Spans: 300, Traces: 100, Services: 2, Errors: 3}
	groups := []GroupRow{
		{Name: "agentloop", Calls: 200, Errors: 3, CumNs: 200e9, SelfNs: 150e9},
		{Name: "dagger", Calls: 100, Errors: 0, CumNs: 100e9, SelfNs: 50e9},
	}
	ops := []OpRow{
		{Service: "agentloop", Op: "POST /q", Calls: 200, Errors: 3,
			CumNs: 200e9, SelfNs: 150e9, AvgNs: 1e9, P95Ns: 3e9, P99Ns: 5e9},
	}
	errOps := []ErrRow{{Service: "agentloop", Op: "POST /q", Calls: 200, Errors: 3}}

	var b strings.Builder
	renderReport(&b, options{table: "otel_traces", top: 15}, col, start, end, totals, groups, ops, errOps, nil)
	out := b.String()

	// grand self = 200e9 ns = 200 s over a 100 s window => 2.000 in flight.
	for _, want := range []string{
		"# otelhousereport",
		"**Spans:** 300 in 100 traces across 2 service(s)",
		"**In flight:** 2.000 spans",
		"## Where time goes — by service",
		"| agentloop | 1.500 | 75.0 | 200 | 3 |", // 150s/100s=1.5, 150/200=75%
		"| **total** | 2.000 | 100.0 | 300 | 3 |",
		"## Hottest operations (by self-time)",
		"## Errors",
		"Self-time is a span's own duration",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "INCOMPLETE") {
		t.Error("a clean report must not carry an INCOMPLETE section")
	}
}

// A failed section must be announced, never silently dropped.
func TestRenderReportMarksIncomplete(t *testing.T) {
	start := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	col, _ := resolveColumn("service")
	var b strings.Builder
	renderReport(&b, options{table: "otel_traces", top: 15}, col, start, end,
		Totals{Spans: 10}, nil, nil, nil, []string{"hot operations: context deadline exceeded"})
	out := b.String()
	if !strings.Contains(out, "INCOMPLETE") || !strings.Contains(out, "deadline exceeded") {
		t.Errorf("incomplete report must name the failure:\n%s", out)
	}
}

func TestRenderReportEscapesPipesInNames(t *testing.T) {
	start := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	col, _ := resolveColumn("service")
	ops := []OpRow{{Service: "svc", Op: "a|b", Calls: 1}}
	var b strings.Builder
	renderReport(&b, options{table: "otel_traces", top: 15}, col, start, end,
		Totals{Spans: 1}, []GroupRow{{Name: "svc", Calls: 1, SelfNs: 1}}, ops, nil, nil)
	if strings.Contains(b.String(), "| a|b |") {
		t.Error("an unescaped pipe in an operation name breaks the table")
	}
}

// --top=0 asks for a summary. The top-N sections must be omitted, NOT rendered
// empty with a "did not complete" note that reads as a failure.
func TestRenderReportTopZeroOmitsTables(t *testing.T) {
	start := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	col, _ := resolveColumn("service")
	var b strings.Builder
	renderReport(&b, options{table: "otel_traces", top: 0}, col, start, end,
		Totals{Spans: 10, Errors: 2}, []GroupRow{{Name: "svc", Calls: 10, Errors: 2, SelfNs: 1e9}}, nil, nil, nil)
	out := b.String()
	if strings.Contains(out, "## Hottest operations") || strings.Contains(out, "## Errors") {
		t.Errorf("--top=0 must omit the top-N sections:\n%s", out)
	}
	if strings.Contains(out, "did not complete") {
		t.Errorf("--top=0 must not claim a query failed:\n%s", out)
	}
	if !strings.Contains(out, "## Where time goes") {
		t.Errorf("--top=0 must still render the breakdown:\n%s", out)
	}
}
