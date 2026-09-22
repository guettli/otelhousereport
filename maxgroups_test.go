package main

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// foldRemainder is the fold behind the --max-groups cap. A high-cardinality
// --by must not return one row per distinct value; the tail folds into a single
// "(other)" row whose numbers are the true totals minus the shown rows, so the
// breakdown stays complete even though it is bounded.
func TestFoldRemainderKeepsTheTotalsComplete(t *testing.T) {
	// Six groups, self-time 60/50/40/30/20/10 (= 210 total), cap at 3.
	all := []GroupRow{
		{Name: "a", Calls: 6, Errors: 1, SelfNs: 60, CumNs: 60},
		{Name: "b", Calls: 5, Errors: 1, SelfNs: 50, CumNs: 50},
		{Name: "c", Calls: 4, Errors: 0, SelfNs: 40, CumNs: 40},
		{Name: "d", Calls: 3, Errors: 0, SelfNs: 30, CumNs: 30},
		{Name: "e", Calls: 2, Errors: 0, SelfNs: 20, CumNs: 20},
		{Name: "f", Calls: 1, Errors: 1, SelfNs: 10, CumNs: 10},
	}
	// Grand totals as the ungrouped query would report them.
	totals := Totals{Spans: 21, Errors: 4}
	rows, more := foldRemainder(all, 210, 210, totals, 6, 3)

	if len(rows) != 4 {
		t.Fatalf("want 3 shown + 1 (other) = 4 rows, got %d", len(rows))
	}
	if more != 3 {
		t.Errorf("want 3 folded groups (6 - cap 3), got %d", more)
	}
	other := rows[3]
	if other.Name != "(other)" {
		t.Fatalf("last row must be (other), got %q", other.Name)
	}
	// (other) = grand - shown: self 210-150=60, calls 21-15=6, errors 4-2=2.
	if other.SelfNs != 60 || other.Calls != 6 || other.Errors != 2 {
		t.Errorf("(other) = self %.0f calls %d errors %d, want 60/6/2", other.SelfNs, other.Calls, other.Errors)
	}
	// The whole point: shown self + (other) self == grand, so %TIME sums to 100.
	var sum float64
	for _, r := range rows {
		sum += r.SelfNs
	}
	if sum != 210 {
		t.Errorf("rows self-time sums to %.0f, want the grand total 210", sum)
	}
}

// Below the cap nothing is folded and nothing is fetched — the common,
// low-cardinality report is untouched.
func TestFoldRemainderNoOpUnderCap(t *testing.T) {
	all := []GroupRow{{Name: "a", SelfNs: 10}, {Name: "b", SelfNs: 5}}
	rows, more := foldRemainder(all, 15, 15, Totals{Spans: 3}, 2, 50)
	if len(rows) != 2 || more != 0 {
		t.Errorf("under the cap the rows pass through unchanged: got %d rows, more=%d", len(rows), more)
	}
	if rows[len(rows)-1].Name == "(other)" {
		t.Error("no (other) row should be added under the cap")
	}
	// maxGroups=0 means no cap, even with many groups.
	rows, more = foldRemainder(all, 15, 15, Totals{Spans: 3}, 2, 0)
	if len(rows) != 2 || more != 0 {
		t.Errorf("--max-groups=0 disables the cap: got %d rows, more=%d", len(rows), more)
	}
}

// The grand totals come from a second query over the same window; a scrape at
// the window edge can make them differ from the shown rows by a hair. (other)
// must floor at zero rather than go negative or underflow.
func TestFoldRemainderFloorsAtZero(t *testing.T) {
	all := []GroupRow{
		{Name: "a", Calls: 10, Errors: 5, SelfNs: 100, CumNs: 100},
		{Name: "b", Calls: 10, Errors: 5, SelfNs: 100, CumNs: 100},
		{Name: "c", Calls: 10, Errors: 5, SelfNs: 100, CumNs: 100},
	}
	// Cap 2, so the shown rows sum to 200 self / 20 calls / 10 errors — but the
	// grand totals come from a second query at a different instant and come back
	// SMALLER (window-edge skew). The remainder is negative before flooring;
	// three rows are needed because showing only the two under the cap is what
	// makes the shown sum able to exceed the grand total at all.
	rows, _ := foldRemainder(all, 150, 150, Totals{Spans: 15, Errors: 8}, 4, 2)
	other := rows[len(rows)-1]
	if other.Name != "(other)" {
		t.Fatalf("last row must be (other), got %q", other.Name)
	}
	if other.SelfNs != 0 || other.CumNs != 0 {
		t.Errorf("(other) must floor at zero, got self=%.0f cum=%.0f", other.SelfNs, other.CumNs)
	}
	// subU floors the unsigned counts too — the failure mode is a huge uint64,
	// so assert the exact floored value rather than a magnitude guard.
	if other.Calls != 0 || other.Errors != 0 {
		t.Errorf("(other) counts must floor at zero (no uint underflow), got calls=%d errors=%d", other.Calls, other.Errors)
	}
}

// renderReport must show the (other) row and a note naming the cap, and the
// %TIME column (including (other)) must still sum to 100.
func TestRenderReportShowsOtherAndCapNote(t *testing.T) {
	start := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	col, _ := resolveColumn("span:http.url")
	groups := []GroupRow{
		{Name: "/a", Calls: 6, SelfNs: 60e9, CumNs: 60e9},
		{Name: "/b", Calls: 5, SelfNs: 50e9, CumNs: 50e9},
		{Name: "(other)", Calls: 10, SelfNs: 100e9, CumNs: 100e9},
	}
	var b strings.Builder
	renderReport(&b, options{table: "otel_traces", top: 15, maxGroups: 2}, col, start, end,
		Totals{Spans: 21}, groups, nil, nil, nil, nil, 1200)
	out := b.String()

	if !strings.Contains(out, "(other)") {
		t.Errorf("the (other) row must render:\n%s", out)
	}
	if !strings.Contains(out, "top 2") || !strings.Contains(out, "--max-groups") {
		t.Errorf("a note must name the cap and how to raise it:\n%s", out)
	}
	if !strings.Contains(out, "1,200") {
		t.Errorf("the note should say how many values were folded (humanized):\n%s", out)
	}
	// %TIME across the data rows (excluding the **total** row) must sum to 100.
	sumPct := sumTimeColumn(t, out)
	if sumPct < 99.9 || sumPct > 100.1 {
		t.Errorf("%%TIME column sums to %.1f, want 100 (the (other) row is what makes it whole):\n%s", sumPct, out)
	}
}

// sumTimeColumn adds the %TIME column of the "Where time goes" table, skipping
// the header, the alignment row, and the bold **total** row.
func sumTimeColumn(t *testing.T, md string) float64 {
	t.Helper()
	var sum float64
	inTable := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(line, "| ") && strings.Contains(line, "INFLIGHT") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") { // table ended
			break
		}
		if strings.Contains(line, ":--") || strings.Contains(line, "**total**") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(cells[2]), 64) // %TIME is column 3
		if err == nil {
			sum += v
		}
	}
	return sum
}
