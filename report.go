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
	dsn       string
	table     string
	from      string
	to        string
	by        string
	match     []string
	top       int
	maxGroups int
	logs      bool
	exactSelf bool
	out       string
	timeout   time.Duration
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
	matches, err := parseMatches(o.match)
	if err != nil {
		return Report{}, err
	}
	s.exactSelf = o.exactSelf

	totals, err := s.GetTotals(ctx, start, end, matches)
	if err != nil {
		return Report{}, err
	}
	if totals.Spans == 0 {
		// An empty answer gets the same scepticism as a failed one: say whether
		// the table is missing, the table is empty, or the window simply misses
		// the data — never assert the most convenient reading.
		return Report{}, explainEmpty(ctx, s, o.table, start, end, matches)
	}

	var failures []string
	note := func(err error) { failures = append(failures, err.Error()) }

	// Cap the breakdown. A high-cardinality --by (an id-like attribute) would
	// otherwise return one row per distinct value. Fetch one more than the cap
	// so a full page is distinguishable from a truncated one; the tail is
	// folded into an honest "(other)" row below, never dropped.
	limit := 0
	if o.maxGroups > 0 {
		limit = o.maxGroups + 1
	}
	groups, err := s.GroupBy(ctx, col, start, end, matches, limit)
	if err != nil {
		note(err)
	}
	// moreGroups > 0 means the breakdown was capped: some groups are summarised
	// in the "(other)" row rather than listed. renderReport uses it for the note.
	var moreGroups uint64
	if o.maxGroups > 0 && len(groups) > o.maxGroups {
		// The tail exists. Get the true grand totals (self, cum, group count)
		// across ALL groups so the "(other)" row and %TIME stay exact, then
		// trim to the cap and synthesise the remainder. This second query runs
		// ONLY here, on the high-cardinality path -- the common report never
		// pays for it.
		grandSelf, grandCum, groupCount, gerr := s.GroupGrandTotals(ctx, col, start, end, matches)
		if gerr != nil {
			// Can't reconcile the remainder, so don't fake it: keep the top
			// rows, drop the extra probe row, and note the run is incomplete
			// rather than render an "(other)" with invented numbers.
			note(gerr)
			groups = groups[:o.maxGroups]
		} else {
			groups, moreGroups = foldRemainder(groups, grandSelf, grandCum, totals, groupCount, o.maxGroups)
		}
	}
	// --top=0 asks for a summary: the header and the breakdown only. Skip the
	// two top-N tables entirely rather than running LIMIT 0 queries and then
	// rendering an empty table that reads as a failure.
	var ops []OpRow
	var errOps []ErrRow
	if o.top > 0 {
		if ops, err = s.HotOps(ctx, start, end, o.top, matches); err != nil {
			note(err)
		}
		if totals.Errors > 0 {
			if errOps, err = s.ErrorOps(ctx, start, end, o.top, matches); err != nil {
				note(err)
			}
		}
	}

	// Correlated error logs are opt-in (a second join) and only meaningful when
	// there are errors and otel_logs actually exists.
	var logs []LogRow
	if o.logs && totals.Errors > 0 && s.HasTable(ctx, logsTable) {
		limit := o.top
		if limit <= 0 {
			limit = 15
		}
		if logs, err = s.ErrorLogs(ctx, start, end, limit, matches); err != nil {
			note(err)
		}
	}

	var b strings.Builder
	renderReport(&b, o, col, start, end, totals, groups, ops, errOps, logs, failures, moreGroups)
	return Report{Markdown: b.String(), Incomplete: len(failures) > 0}, nil
}

// renderReport writes the Markdown. It is a pure function of already-fetched
// data so it can be unit-tested without a database.
func renderReport(w io.Writer, o options, col Column, start, end time.Time,
	t Totals, groups []GroupRow, ops []OpRow, errOps []ErrRow, logs []LogRow, failures []string, moreGroups uint64) {

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
	// When a selection (usually a --match onto a parent operation) spends almost
	// all its time inside child spans that the filter excluded, self-time — and
	// therefore INFLIGHT — collapses toward zero and the headline reads as idle
	// for something the reader picked precisely because it is slow. Say so, and
	// point at where the real number is.
	if len(groups) > 0 && grandCum > 0 && grandSelf < 0.2*grandCum {
		where := "raise --top to see per-operation latency"
		if o.top != 0 {
			where = "read the AVG/P95/P99 latency columns below"
		}
		fmt.Fprintf(w, "- **Mostly in children:** self-time is only %.0f%% of the %s cumulative span time in this selection, so INFLIGHT understates it — %s.\n",
			pct(grandSelf, grandCum), humanDuration(grandCum), where)
	}
	if len(o.match) > 0 {
		fmt.Fprintf(w, "- **Filter:** `%s`\n", mdEscape(strings.Join(o.match, "` `")))
	}
	fmt.Fprintln(w)

	// Section 1: where time goes.
	fmt.Fprintf(w, "## Where time goes — by %s\n\n", col.Flag)
	if len(groups) > 0 {
		fmt.Fprintf(w, "`INFLIGHT` is self-time ÷ wall-time — the average number of these spans running at once. `%%TIME` is the share of total self-time.\n\n")
		if moreGroups > 0 {
			fmt.Fprintf(w, "Showing the top %d by self-time; the remaining %s %s are summed into **(other)**, so the total is still complete. Raise `--max-groups` to list more.\n\n",
				o.maxGroups, humanCount(moreGroups), plural(moreGroups, col.Flag+" value", col.Flag+" values"))
		}
		headers := []string{col.Header, "INFLIGHT", "%TIME", "CALLS", "ERRORS"}
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

	// Section 2: hottest operations. Omitted entirely under --top=0, which asks
	// for a summary (header + breakdown) rather than the top-N tables.
	if o.top != 0 {
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
	}

	// Section 3: errors (only when there are any, and not under --top=0).
	if o.top != 0 && t.Errors > 0 {
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

	// Section 4: correlated error logs (opt-in via --logs).
	if o.logs && t.Errors > 0 {
		fmt.Fprintf(w, "## Error logs\n\n")
		if len(logs) > 0 {
			fmt.Fprintf(w, "Recurring log lines on error spans, highest severity first (from `otel_logs`, joined on trace + span id).\n\n")
			headers := []string{"SERVICE", "OPERATION", "SEV", "COUNT", "MESSAGE"}
			var rows [][]string
			for _, l := range logs {
				rows = append(rows, []string{
					mdEscape(orEmpty(l.Service)),
					mdEscape(orEmpty(l.Op)),
					mdEscape(l.Severity),
					humanCount(l.Count),
					mdEscape(l.Body),
				})
			}
			mdTable(w, headers, "lllrl", rows)
			fmt.Fprintln(w)
		} else {
			fmt.Fprintf(w, "_No log lines in `otel_logs` correlate to the error spans in this window (or the table is absent)._\n\n")
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
	if o.exactSelf {
		fmt.Fprintf(w, "_Self-time is exact: each parent's child coverage is the **union** of its child spans' intervals (`--exact-self-time`), so concurrent children are counted once. Cumulative time (a parent plus its children) is intentionally not the ranking key because nested spans double-count it._\n")
	} else {
		fmt.Fprintf(w, "_Self-time is a span's own duration minus its children's, floored at zero; overlapping child spans make it approximate (pass `--exact-self-time` for the interval-union version). Cumulative time (a parent plus its children) is intentionally not the ranking key because nested spans double-count it._\n")
	}
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
// whitelisted or an attribute reference (so it is injection-safe) and the value
// is returned to be passed as a bound parameter, never concatenated.
func parseMatch(m string) (Match, error) {
	k, v, ok := strings.Cut(m, "=")
	if !ok {
		return Match{}, fmt.Errorf("--match %q must be column=value, e.g. service=agentloop or span:http.request.method=POST", m)
	}
	col, err := resolveColumn(strings.TrimSpace(k))
	if err != nil {
		return Match{}, fmt.Errorf("--match: %w", err)
	}
	return Match{Col: col, Value: v}, nil
}

// parseMatches turns the repeated --match flags into validated Matches. They
// are ANDed together, so `--match service=agentloop --match span:x=y` narrows.
func parseMatches(ms []string) ([]Match, error) {
	var out []Match
	for _, m := range ms {
		parsed, err := parseMatch(m)
		if err != nil {
			return nil, err
		}
		out = append(out, parsed)
	}
	return out, nil
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
func explainEmpty(ctx context.Context, s *Store, table string, start, end time.Time, matches []Match) error {
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

	if len(matches) > 0 {
		// With a filter applied, "empty" most likely means the filter matched
		// nothing, not that the window is empty. Say so instead of blaming the
		// window.
		parts := make([]string, len(matches))
		for i, m := range matches {
			parts[i] = m.Col.Flag + "=" + m.Value
		}
		return fmt.Errorf("no spans in %s matching --match %s; drop a --match or check values with `otelhousereport services` / `operations`",
			win, strings.Join(parts, " "))
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

// foldRemainder trims a capped breakdown to its top rows and appends a single
// "(other)" row for everything below the cap, so the table stays bounded while
// the totals stay complete. Pure — the grand totals it needs are fetched by the
// caller — so the fold math (and its %TIME-sums-to-100 property) is testable
// without a database.
//
// grandSelf/grandCum are the true totals across ALL groups; t.Spans/t.Errors
// the true span/error counts; groupCount the number of distinct groups. The
// "(other)" row is the difference between those and the shown rows, floored so
// two queries disagreeing at the window edge cannot make it negative.
func foldRemainder(groups []GroupRow, grandSelf, grandCum float64, t Totals, groupCount uint64, maxGroups int) ([]GroupRow, uint64) {
	if maxGroups <= 0 || len(groups) <= maxGroups {
		return groups, 0
	}
	shown := groups[:maxGroups]
	var sumSelf, sumCum float64
	var sumCalls, sumErrors uint64
	for _, g := range shown {
		sumSelf += g.SelfNs
		sumCum += g.CumNs
		sumCalls += g.Calls
		sumErrors += g.Errors
	}
	other := GroupRow{
		Name:   "(other)",
		Calls:  subU(t.Spans, sumCalls),
		Errors: subU(t.Errors, sumErrors),
		CumNs:  nonNeg(grandCum - sumCum),
		SelfNs: nonNeg(grandSelf - sumSelf),
	}
	var more uint64
	if groupCount > uint64(maxGroups) {
		more = groupCount - uint64(maxGroups)
	}
	return append(shown, other), more
}

// subU subtracts without underflowing an unsigned total, in case an independent
// totals query and the group sums disagree at the edges of the window.
func subU(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// nonNeg floors a float at zero: the grand totals and the shown-group sums come
// from two queries over the same window and can differ by a hair, which must
// not make "(other)" negative.
func nonNeg(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

// plural picks the singular or plural noun for n.
func plural(n uint64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
