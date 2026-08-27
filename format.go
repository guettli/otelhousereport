package main

import (
	"fmt"
	"io"
	"strings"
)

// humanDuration renders a nanosecond count the way a human reads a latency or a
// time budget, picking one unit rather than printing 83081100000000ns. It is
// used for both per-span latencies (µs..s) and window-wide totals (minutes,
// hours, days), so it spans a wide range on purpose.
func humanDuration(ns float64) string {
	switch {
	case ns < 1e3:
		return fmt.Sprintf("%.0fns", ns)
	case ns < 1e6:
		return fmt.Sprintf("%.1fµs", ns/1e3)
	case ns < 1e9:
		return fmt.Sprintf("%.1fms", ns/1e6)
	case ns < 60e9:
		return fmt.Sprintf("%.2fs", ns/1e9)
	case ns < 3600e9:
		s := ns / 1e9
		return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
	case ns < 86400e9:
		h := ns / 3600e9
		return fmt.Sprintf("%.1fh", h)
	default:
		return fmt.Sprintf("%.1fd", ns/86400e9)
	}
}

// humanCount groups thousands so a report reads "285,044" rather than 285044.
// Big call counts are the norm here, and the grouping is the difference between
// a number you can size up at a glance and one you have to count digits on.
func humanCount(n uint64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// pct is a percentage guarded against a zero denominator, so an empty group
// reports 0.0 instead of NaN — NaN in a Markdown cell is exactly the kind of
// glitch that makes an agent distrust the whole report.
func pct(part, whole float64) float64 {
	if whole == 0 {
		return 0
	}
	return part / whole * 100
}

// mdTable writes a GitHub-flavoured Markdown table. aligns is one byte per
// column: 'r' right-aligns (numbers), anything else left-aligns (text). The
// output is deliberately plain Markdown and not a pretty-printed ASCII table:
// the consumer is an LLM agent reading a .md file, and Markdown tables are what
// it parses most reliably.
func mdTable(w io.Writer, headers []string, aligns string, rows [][]string) {
	fmt.Fprintf(w, "| %s |\n", strings.Join(headers, " | "))
	seps := make([]string, len(headers))
	for i := range headers {
		if i < len(aligns) && aligns[i] == 'r' {
			seps[i] = "---:"
		} else {
			seps[i] = ":---"
		}
	}
	fmt.Fprintf(w, "| %s |\n", strings.Join(seps, " | "))
	for _, r := range rows {
		fmt.Fprintf(w, "| %s |\n", strings.Join(r, " | "))
	}
}

// mdEscape neutralizes the two characters that would break a Markdown table
// cell: a pipe (ends the cell) and a newline (ends the row). Span names and
// especially resource-attribute values are arbitrary strings from the traced
// application, so they cannot be trusted to be table-safe.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}
