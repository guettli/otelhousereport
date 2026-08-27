// Command otelhousereport turns the OpenTelemetry traces stored in ClickHouse
// (the Collector's clickhouseexporter schema) into a Markdown report an agent
// can read to decide where to look next.
//
// It is the trace-side sibling of parcareport: parcareport turns a Parca
// profiling server into a cross-cluster CPU-bottleneck report; this turns a
// ClickHouse full of spans into a "where did latency and errors go" report,
// normalized so a 1-hour window and a 24-hour window are comparable.
//
// The design borrows parcareport's discipline: an absolute, window-normalized
// unit (spans in flight, not raw span counts); a strict separation of an empty
// answer from a failed one; and partial results that announce themselves rather
// than looking complete.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "otelhousereport: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var o options
	fs := flag.NewFlagSet("otelhousereport", flag.ContinueOnError)
	fs.StringVar(&o.dsn, "dsn", os.Getenv("CLICKHOUSE_DSN"), "ClickHouse DSN (default: $CLICKHOUSE_DSN)")
	fs.StringVar(&o.table, "table", "otel_traces", "traces table name")
	fs.StringVar(&o.from, "from", "-1h", "window start: RFC3339, or relative like -6h, -90m, -7d, -1w")
	fs.StringVar(&o.to, "to", "now", "window end: RFC3339, or 'now'")
	fs.StringVar(&o.by, "by", "service", "column for the breakdown: service, name, kind, status")
	fs.StringVar(&o.match, "match", "", `filter to one value, e.g. service=agentloop`)
	fs.IntVar(&o.top, "top", 15, "rows in the operation and error tables (0 = omit them; summary only)")
	fs.StringVar(&o.out, "out", "", "write the report to this file (default: stdout)")
	fs.DurationVar(&o.timeout, "timeout", 25*time.Second, "per-query timeout; also caps ClickHouse max_execution_time, so keep it under any server-side cap (a read-only profile often caps it at 30s)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), usage)
		fs.PrintDefaults()
	}

	// A subcommand, if present, comes before the flags: `otelhousereport
	// services --from=-6h`. Peel it off before flag parsing, which otherwise
	// stops at the first non-flag argument and silently ignores the flags.
	var sub string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if o.dsn == "" {
		return errors.New("no ClickHouse DSN: set $CLICKHOUSE_DSN or pass --dsn (e.g. clickhouse://user:pass@host:9000/otel)")
	}
	if o.top < 0 {
		return fmt.Errorf("--top must be >= 0, got %d", o.top)
	}

	start, end, err := parseWindow(o.from, o.to)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	s, err := Open(ctx, o.dsn, o.table, o.timeout)
	if err != nil {
		return err
	}
	defer s.Close()

	switch sub {
	case "", "report":
		rep, err := buildReport(ctx, s, o, start, end)
		if err != nil {
			return err
		}
		if err := writeOutput(o.out, rep.Markdown); err != nil {
			return err
		}
		if rep.Incomplete {
			// The report was written, but it is not trustworthy. Exit non-zero
			// so a script or an agent that checks the status does not mistake
			// an incomplete report for a clean one.
			return errors.New("report is incomplete (see the ⚠️ INCOMPLETE section); exiting non-zero")
		}
		return nil
	case "services":
		return runServices(ctx, s, o, start, end)
	case "operations":
		return runOperations(ctx, s, o, start, end)
	case "tables":
		return runTables(ctx, s, o)
	default:
		return fmt.Errorf("unknown command %q (want: report, services, operations, tables)", sub)
	}
}

func runServices(ctx context.Context, s *Store, o options, start, end time.Time) error {
	if err := ensureTable(ctx, s, o.table); err != nil {
		return err
	}
	rows, err := s.Services(ctx, start, end)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Services (`%s` .. `%s`)\n\n", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	if len(rows) == 0 {
		return explainEmpty(ctx, s, o.table, start, end, nil)
	}
	var tr [][]string
	for _, r := range rows {
		tr = append(tr, []string{mdEscape(orEmpty(r.A)), humanCount(r.Count)})
	}
	mdTable(&b, []string{"SERVICE", "SPANS"}, "lr", tr)
	return writeOutput(o.out, b.String())
}

func runOperations(ctx context.Context, s *Store, o options, start, end time.Time) error {
	if err := ensureTable(ctx, s, o.table); err != nil {
		return err
	}
	top := o.top
	if top == 0 {
		top = 100 // "0 rows" makes no sense for a listing; use a sane default
	}
	rows, err := s.Operations(ctx, start, end, top)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Operations (`%s` .. `%s`, top %d)\n\n", start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339), top)
	if len(rows) == 0 {
		return explainEmpty(ctx, s, o.table, start, end, nil)
	}
	var tr [][]string
	for _, r := range rows {
		tr = append(tr, []string{mdEscape(orEmpty(r.A)), mdEscape(orEmpty(r.B)), humanCount(r.Count)})
	}
	mdTable(&b, []string{"SERVICE", "OPERATION", "SPANS"}, "llr", tr)
	return writeOutput(o.out, b.String())
}

func runTables(ctx context.Context, s *Store, o options) error {
	tabs, err := s.Tables(ctx)
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# OpenTelemetry tables\n\n")
	if len(tabs) == 0 {
		fmt.Fprintf(&b, "_No `otel_*` tables in this database._\n")
	}
	for _, t := range tabs {
		fmt.Fprintf(&b, "- `%s`\n", t)
	}
	return writeOutput(o.out, b.String())
}

// writeOutput sends the report to a file or stdout. A file write is atomic
// enough for this: it truncates and writes in one call, so a reader never sees
// a half-written report from a previous run.
func writeOutput(path, content string) error {
	if path == "" {
		fmt.Print(content)
		return nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// parseWindow accepts RFC3339 or a relative offset from now ("-6h"), the same
// grammar parcareport uses, and rejects an inverted window early.
func parseWindow(from, to string) (time.Time, time.Time, error) {
	now := time.Now()
	end, err := parseTime(to, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--to: %w", err)
	}
	start, err := parseTime(from, now)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--from: %w", err)
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("--from (%s) must be before --to (%s)",
			start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	return start, end, nil
}

func parseTime(s string, now time.Time) (time.Time, error) {
	if s == "" || s == "now" {
		return now, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := parseSignedDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither RFC3339 nor a duration like -6h, -90m or -7d", s)
	}
	if d > 0 {
		d = -d // "6h" reads as "6h ago", same as "-6h"
	}
	return now.Add(d), nil
}

var relUnitRe = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([wdhms])`)

// parseSignedDuration extends Go's time.ParseDuration with the units a human
// reaching for a reporting window actually types: days and weeks. Go's parser
// stops at hours, so "-7d" — which this tool's own README advertises — would
// otherwise be rejected. Compound windows like "1d6h" work too.
//
// Sub-second units (ms, µs, ns) fall through to time.ParseDuration, so nothing
// the standard parser accepted is lost.
func parseSignedDuration(s string) (time.Duration, error) {
	neg := false
	switch {
	case strings.HasPrefix(s, "-"):
		neg, s = true, s[1:]
	case strings.HasPrefix(s, "+"):
		s = s[1:]
	}
	if d, ok := parseWDHMS(s); ok {
		if neg {
			d = -d
		}
		return d, nil
	}
	full := s
	if neg {
		full = "-" + s
	}
	return time.ParseDuration(full)
}

// parseWDHMS sums a sequence of week/day/hour/minute/second components. It
// returns false — rather than a partial result — if the whole string is not
// made of those components, so the caller can fall back to the standard parser
// cleanly. "500ms" fails here (the trailing "s" has no number) and falls back,
// which is the intended handoff.
func parseWDHMS(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var total time.Duration
	for s != "" {
		m := relUnitRe.FindStringSubmatch(s)
		if m == nil {
			return 0, false
		}
		val, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			return 0, false
		}
		var unit time.Duration
		switch m[2] {
		case "w":
			unit = 7 * 24 * time.Hour
		case "d":
			unit = 24 * time.Hour
		case "h":
			unit = time.Hour
		case "m":
			unit = time.Minute
		case "s":
			unit = time.Second
		}
		total += time.Duration(val * float64(unit))
		s = s[len(m[0]):]
	}
	return total, true
}

const usage = `otelhousereport - a Markdown report of where OpenTelemetry traces spend time

Reads the Collector's clickhouseexporter traces out of ClickHouse and writes a
Markdown report: where time goes by service, the hottest operations, and the
error hot spots. Built to be read by an agent, not just a human.

Usage:
  otelhousereport [report] [flags]   the report (default)
  otelhousereport services [flags]   list services seen in the window
  otelhousereport operations [flags] list operations seen in the window
  otelhousereport tables [flags]     list the otel_* tables present

INFLIGHT is self-time / wall-time: the average number of spans of a kind running
at once. It is comparable across windows of different length, unlike raw counts.

Flags:
`
