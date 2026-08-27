package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

// Report is the fully-assembled report plus whether it is trustworthy. The
// boolean is the whole point of separating this from the rendering: a report
// can be produced and still be incomplete (a section query failed), and the
// caller must be able to exit non-zero without re-deriving that from the text.
type Report struct {
	Markdown   string
	Incomplete bool
}

// options carries the resolved CLI configuration into report building.
type options struct {
	dsn     string
	table   string
	from    string
	to      string
	by      string
	match   string
	top     int
	out     string
	timeout time.Duration
}

// buildReport runs the queries and renders the Markdown. It is deliberately
// tolerant of *section* failures and intolerant of *foundational* ones: if the
// totals query fails there is nothing to report and it errors out; if a single
// section fails the report is still emitted, but marked incomplete so the run
// exits non-zero and the failure is printed next to the numbers it invalidates.
func buildReport(ctx context.Context, s *Store, o options, start, end time.Time) (Report, error) {
	col, err := resolveColumn(o.by)
	if err != nil {
		return Report{}, err
	}
	if err := ensureTable(ctx, s, o.table); err != nil {
		return Report{}, err
	}
	var match *Match
	if o.match != "" {
		m, err := parseMatch(o.match)
		if err != nil {
			return Report{}, err
		}
		match = m
	}

	totals, err := s.GetTotals(ctx, start, end, match)
	if err != nil {
		return Report{}, err
	}
	if totals.Spans == 0 {
		// An empty answer gets the same scepticism as a failed one: say whether
		// the table is missing, the table is empty, or the window simply misses
		// the data — never assert the most convenient reading.
		return Report{}, explainEmpty(ctx, s, o.table, start, end, match)
	}

	var failures []string
	note := func(err error) { failures = append(failures, err.Error()) }

	groups, err := s.GroupBy(ctx, col, start, end, match)
	if err != nil {
		note(err)
	}
	ops, err := s.HotOps(ctx, start, end, o.top, match)
	if err != nil {
		note(err)
	}
	var errOps []ErrRow
	if totals.Errors > 0 {
		if errOps, err = s.ErrorOps(ctx, start, end, o.top, match); err != nil {
			note(err)
		}
	}

	var b strings.Builder
	renderReport(&b, o, col, start, end, totals, groups, ops, errOps, failures)
	return Report{Markdown: b.String(), Incomplete: len(failures) > 0}, nil
}

// renderReport writes the Markdown. It is a pure function of already-fetched
// data so it can be unit-tested without a database.
func renderReport(w io.Writer, o options, col Column, start, end time.Time,
	t Totals, groups []GroupRow, ops []OpRow, errOps []ErrRow, failures []string) {

	windowSecs := end.Sub(start).Seconds()
	var grandSelf, grandCum float64
	for _, g := range groups {
		grandSelf += g.SelfNs
		grandCum += g.CumNs
	}

	fmt.Fprintf(w, "# otelhousereport\n\n")
	fmt.Fprintf(w, "- **Source:** ClickHouse table `%s`\n", o.table)
	fmt.Fprintf(w, "- **Window:** `%s` .. `%s` (%s)\n",
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339),
		end.Sub(start).Round(time.Second))
	fmt.Fprintf(w, "- **Spans:** %s in %s traces across %d service(s)\n",
		humanCount(t.Spans), humanCount(t.Traces), t.Services)
	fmt.Fprintf(w, "- **Errors:** %s (%.1f%% of spans)\n",
		humanCount(t.Errors), pct(float64(t.Errors), float64(t.Spans)))
	if len(groups) > 0 && windowSecs > 0 {
		fmt.Fprintf(w, "- **In flight:** %.3f spans on average (self-time ÷ wall-time)\n",
			grandSelf/1e9/windowSecs)
	}
	if o.match != "" {
		fmt.Fprintf(w, "- **Filter:** `%s`\n", mdEscape(o.match))
	}
	fmt.Fprintln(w)

	// Section 1: where time goes.
	fmt.Fprintf(w, "## Where time goes — by %s\n\n", col.Flag)
	if len(groups) > 0 {
		fmt.Fprintf(w, "`INFLIGHT` is self-time ÷ wall-time — the average number of these spans running at once. `%%TIME` is the share of total self-time.\n\n")
		headers := []string{strings.ToUpper(col.Flag), "INFLIGHT", "%TIME", "CALLS", "ERRORS"}
		var rows [][]string
		for _, g := range groups {
			rows = append(rows, []string{
				mdEscape(orEmpty(g.Name)),
				fmt.Sprintf("%.3f", g.SelfNs/1e9/windowSecs),
				fmt.Sprintf("%.1f", pct(g.SelfNs, grandSelf)),
				humanCount(g.Calls),
				humanCount(g.Errors),
			})
		}
		rows = append(rows, []string{
			"**total**",
			fmt.Sprintf("%.3f", grandSelf/1e9/windowSecs),
			"100.0", humanCount(t.Spans), humanCount(t.Errors),
		})
		mdTable(w, headers, "lrrrr", rows)
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "_No breakdown: this section's query did not complete._\n\n")
	}

	// Section 2: hottest operations.
	fmt.Fprintf(w, "## Hottest operations (by self-time)\n\n")
	if len(ops) > 0 {
		headers := []string{"SERVICE", "OPERATION", "CALLS", "SELF", "AVG", "P95", "P99", "ERR%"}
		var rows [][]string
		for _, op := range ops {
			rows = append(rows, []string{
				mdEscape(orEmpty(op.Service)),
				mdEscape(orEmpty(op.Op)),
				humanCount(op.Calls),
				humanDuration(op.SelfNs),
				humanDuration(op.AvgNs),
				humanDuration(op.P95Ns),
				humanDuration(op.P99Ns),
				fmt.Sprintf("%.1f", pct(float64(op.Errors), float64(op.Calls))),
			})
		}
		mdTable(w, headers, "llrrrrrr", rows)
		fmt.Fprintln(w)
	} else {
		fmt.Fprintf(w, "_No operations: this section's query did not complete._\n\n")
	}

	// Section 3: errors (only when there are any).
	if t.Errors > 0 {
		fmt.Fprintf(w, "## Errors\n\n")
		if len(errOps) > 0 {
			headers := []string{"SERVICE", "OPERATION", "CALLS", "ERRORS", "ERR%"}
			var rows [][]string
			for _, e := range errOps {
				rows = append(rows, []string{
					mdEscape(orEmpty(e.Service)),
					mdEscape(orEmpty(e.Op)),
					humanCount(e.Calls),
					humanCount(e.Errors),
					fmt.Sprintf("%.1f", pct(float64(e.Errors), float64(e.Calls))),
				})
			}
			mdTable(w, headers, "llrrr", rows)
			fmt.Fprintln(w)
		} else {
			fmt.Fprintf(w, "_%s error span(s) in the window, but the error breakdown did not complete._\n\n",
				humanCount(t.Errors))
		}
	}

	// The honesty footer: partial results are never presented as complete.
	if len(failures) > 0 {
		fmt.Fprintf(w, "## ⚠️ INCOMPLETE\n\n")
		fmt.Fprintf(w, "%d section quer(ies) failed. The numbers above EXCLUDE whatever they would have contributed and are therefore wrong:\n\n", len(failures))
		for _, f := range failures {
			fmt.Fprintf(w, "- %s\n", mdEscape(f))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "---\n")
	fmt.Fprintf(w, "_Self-time is a span's own duration minus its children's, floored at zero; overlapping child spans make it approximate. Cumulative time (a parent plus its children) is intentionally not the ranking key because nested spans double-count it._\n")
}

// orEmpty makes an empty string visible in a table so a blank service name
// reads as "(none)" rather than an empty cell that looks like a rendering bug.
func orEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// parseMatch validates a --match of the form column=value. The column is
// whitelisted (so it is injection-safe as an identifier) and the value is
// returned to be passed as a bound parameter, never concatenated.
func parseMatch(m string) (*Match, error) {
	k, v, ok := strings.Cut(m, "=")
	if !ok {
		return nil, fmt.Errorf("--match %q must be column=value, e.g. service=agentloop", m)
	}
	col, err := resolveColumn(strings.TrimSpace(k))
	if err != nil {
		return nil, fmt.Errorf("--match: %w", err)
	}
	return &Match{Col: col, Value: v}, nil
}

// ensureTable fails early, with the list of tables that do exist, when the
// configured table is absent. Without it the first aggregation fails with a raw
// "Unknown table expression" from ClickHouse, which buries the one thing the
// user needs — the right --table value — under SQL they did not write.
func ensureTable(ctx context.Context, s *Store, table string) error {
	tabs, err := s.Tables(ctx)
	if err != nil {
		// The lookup itself failed; let the real query surface the true error
		// rather than masking a server problem as a missing table.
		return nil
	}
	for _, t := range tabs {
		if t == table {
			return nil
		}
	}
	present := "(no otel_* tables in this database)"
	if len(tabs) > 0 {
		present = strings.Join(tabs, ", ")
	}
	return fmt.Errorf("table %q not found; otel tables present: %s — set --table", table, present)
}

// explainEmpty turns an empty window into a specific conclusion — the table is
// missing, the table is empty, or the window misses the data — rather than the
// convenient "no data" that reads as an idle cluster.
func explainEmpty(ctx context.Context, s *Store, table string, start, end time.Time, match *Match) error {
	win := fmt.Sprintf("`%s` .. `%s`", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))

	if tabs, err := s.Tables(ctx); err == nil {
		found := false
		for _, t := range tabs {
			if t == table {
				found = true
				break
			}
		}
		if !found {
			present := "(none)"
			if len(tabs) > 0 {
				present = strings.Join(tabs, ", ")
			}
			return fmt.Errorf("table %q not found in this database; otel tables present: %s — set --table", table, present)
		}
	}

	if match != nil {
		// With a filter applied, "empty" most likely means the filter matched
		// nothing, not that the window is empty. Say so instead of blaming the
		// window.
		return fmt.Errorf("no spans in %s matching --match %s=%q; drop --match or check the value with `otelhousereport %ss`",
			win, match.Col.Flag, match.Value, match.Col.Flag)
	}

	mn, mx, n, err := s.FullRange(ctx)
	if err != nil {
		return fmt.Errorf("no spans in %s, and the cross-check failed too, so this is more likely a server problem than an empty window — retry before believing it: %w", win, err)
	}
	if n == 0 {
		return fmt.Errorf("table %q exists but is empty: nothing has ever been written to it", table)
	}
	return fmt.Errorf("no spans in %s; %s holds %s spans from `%s` .. `%s` — widen --from/--to to cover that",
		win, table, humanCount(n), mn.UTC().Format(time.RFC3339), mx.UTC().Format(time.RFC3339))
}
