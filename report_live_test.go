package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLive runs the whole pipeline against a real ClickHouse. It is skipped
// unless CLICKHOUSE_DSN is set, so `go test ./...` stays hermetic in CI while a
// developer (or an agent) with a DSN can prove the queries actually run against
// the live clickhouseexporter schema.
//
//	CLICKHOUSE_DSN='clickhouse://user:pass@host:9000/otel' go test -run TestLive -v
func TestLive(t *testing.T) {
	dsn := os.Getenv("CLICKHOUSE_DSN")
	if dsn == "" {
		t.Skip("set CLICKHOUSE_DSN to run the live ClickHouse test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 25s, not 60s: clickhouse-go turns the per-query timeout into ClickHouse's
	// max_execution_time, which a read-only profile commonly caps at 30s.
	s, err := Open(ctx, dsn, "otel_traces", 25*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// A wide window so the test does not depend on there being traffic in the
	// last hour.
	end := time.Now()
	start := end.Add(-30 * 24 * time.Hour)

	o := options{table: "otel_traces", by: "service", top: 10}
	rep, err := buildReport(ctx, s, o, start, end)
	if err != nil {
		t.Fatalf("buildReport: %v", err)
	}
	if !strings.HasPrefix(rep.Markdown, "# otelhousereport") {
		t.Errorf("report does not start with the title:\n%s", first(rep.Markdown, 200))
	}
	if !strings.Contains(rep.Markdown, "## Where time goes") {
		t.Errorf("report missing the breakdown section:\n%s", rep.Markdown)
	}
	t.Logf("live report (%d bytes):\n%s", len(rep.Markdown), rep.Markdown)

	if _, err := s.Services(ctx, start, end); err != nil {
		t.Errorf("services: %v", err)
	}
	if _, err := s.Tables(ctx); err != nil {
		t.Errorf("tables: %v", err)
	}
}

func first(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
