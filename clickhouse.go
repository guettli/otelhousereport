package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Store is a read-only view of the OpenTelemetry traces the Collector's
// clickhouseexporter wrote. It never issues DDL or writes: this tool measures
// what is there, it does not manage the schema.
//
// Every query is bounded by a per-call timeout so one slow aggregation over a
// wide window fails visibly instead of stalling the whole run — the same
// discipline parcareport applies to its merge queries.
type Store struct {
	conn    driver.Conn
	table   string
	timeout time.Duration
}

// identRe guards the two identifiers that cannot be passed as bound query
// parameters — the table name and the group-by column — against SQL injection.
// Everything a user supplies as a *value* (times, the --match value, service
// names) is bound with clickhouse.Named and never concatenated; only these two
// identifiers reach the SQL by name, so they must match a strict pattern.
var identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Open dials ClickHouse from a standard clickhouse-go DSN and pings it, so a
// bad DSN or an unreachable server fails here with a clear message rather than
// on the first query.
func Open(ctx context.Context, dsn, table string, timeout time.Duration) (*Store, error) {
	if !identRe.MatchString(table) {
		return nil, fmt.Errorf("invalid --table %q: must be a bare table identifier", table)
	}
	opts, err := clickhouse.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse CLICKHOUSE_DSN: %w", err)
	}
	conn, err := clickhouse.Open(opts)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := conn.Ping(pctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping clickhouse (check DSN host/credentials): %w", err)
	}
	return &Store{conn: conn, table: table, timeout: timeout}, nil
}

func (s *Store) Close() error { return s.conn.Close() }

// call bounds one query with the store's per-query timeout.
func (s *Store) call(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, s.timeout)
}

// Column is a whitelisted, injection-safe reference to a traces column that a
// user may group or filter by. The map is the whitelist: a name not in it is
// rejected before it can reach the SQL.
type Column struct{ Flag, SQL string }

var columns = map[string]Column{
	"service": {"service", "ServiceName"},
	"name":    {"name", "SpanName"},
	"kind":    {"kind", "SpanKind"},
	"status":  {"status", "StatusCode"},
}

// resolveColumn maps a user-facing flag value to a real column, accepting both
// the short alias ("service") and the ClickHouse column name ("ServiceName").
func resolveColumn(name string) (Column, error) {
	key := strings.ToLower(name)
	if c, ok := columns[key]; ok {
		return c, nil
	}
	for _, c := range columns {
		if strings.EqualFold(c.SQL, name) {
			return c, nil
		}
	}
	want := make([]string, 0, len(columns))
	for _, c := range columns {
		want = append(want, c.Flag)
	}
	return Column{}, fmt.Errorf("unknown column %q; group/filter by one of: %s", name, strings.Join(want, ", "))
}

// Totals is the top-of-report summary of the window.
type Totals struct {
	Spans, Traces, Services, Errors uint64
	Min, Max                        time.Time
}

// GroupRow is one row of the "where time goes" breakdown.
type GroupRow struct {
	Name          string
	Calls, Errors uint64
	CumNs, SelfNs float64
}

// OpRow is one operation in the hot-operations table.
type OpRow struct {
	Service, Op         string
	Calls, Errors       uint64
	CumNs, SelfNs       float64
	AvgNs, P95Ns, P99Ns float64
}

// ErrRow is one operation in the error table.
type ErrRow struct {
	Service, Op   string
	Calls, Errors uint64
}

// filter is the shared WHERE fragment (time window plus an optional --match),
// with the bound arguments that go with it. Building the args alongside the SQL
// keeps the two in lockstep and the values out of the SQL text.
func (s *Store) filter(start, end time.Time, match *Match) (string, []any) {
	args := []any{clickhouse.Named("start", start), clickhouse.Named("end", end)}
	where := "s.Timestamp >= {start:DateTime64(9)} AND s.Timestamp < {end:DateTime64(9)}"
	if match != nil {
		where += fmt.Sprintf(" AND s.%s = {mval:String}", match.Col.SQL)
		args = append(args, clickhouse.Named("mval", match.Value))
	}
	return where, args
}

// Match is a validated column=value equality filter.
type Match struct {
	Col   Column
	Value string
}

// childrenCTE sums each parent's direct child durations within the window, so
// the outer query can subtract them and get self time. It is windowed but
// deliberately NOT filtered by --match: a matched parent's children must all be
// counted regardless of whether the child itself matches, or self time would be
// overstated.
func (s *Store) childrenCTE() string {
	return fmt.Sprintf(`WITH children AS (
  SELECT TraceId, ParentSpanId AS SpanId, sum(Duration) AS child_dur
  FROM %s
  WHERE Timestamp >= {start:DateTime64(9)} AND Timestamp < {end:DateTime64(9)}
    AND ParentSpanId != '' AND ParentSpanId != '0000000000000000'
  GROUP BY TraceId, ParentSpanId
)`, s.table)
}

// selfExpr is self time in nanoseconds: a span's own duration minus the time
// its children covered. greatest(...,0) floors it at zero because overlapping
// children (or clock skew between a parent and a child recorded on a different
// host) can otherwise push a single span's self time negative, which is
// meaningless and would corrupt the sum.
const selfExpr = `sum(greatest(toInt64(s.Duration) - toInt64(c.child_dur), 0))`

// join is the LEFT JOIN onto the children CTE, pinned to join_use_nulls=0 so an
// unmatched parent (a leaf span) gets child_dur=0 rather than NULL. Without the
// setting a server configured for NULL-filling would turn the subtraction into
// NULL and drop every leaf span from the totals.
const joinChildren = "LEFT JOIN children c ON s.TraceId = c.TraceId AND s.SpanId = c.SpanId"
const settings = "SETTINGS join_use_nulls = 0"

func (s *Store) GetTotals(ctx context.Context, start, end time.Time, match *Match) (Totals, error) {
	where, args := s.filter(start, end, match)
	q := fmt.Sprintf(`SELECT count() AS spans, uniqExact(s.TraceId) AS traces,
       uniqExact(s.ServiceName) AS services, countIf(s.StatusCode = 'Error') AS errors,
       min(s.Timestamp) AS mn, max(s.Timestamp) AS mx
FROM %s s WHERE %s`, s.table, where)
	cctx, cancel := s.call(ctx)
	defer cancel()
	var t Totals
	row := s.conn.QueryRow(cctx, q, args...)
	if err := row.Scan(&t.Spans, &t.Traces, &t.Services, &t.Errors, &t.Min, &t.Max); err != nil {
		return Totals{}, fmt.Errorf("totals: %w", err)
	}
	return t, nil
}

func (s *Store) GroupBy(ctx context.Context, col Column, start, end time.Time, match *Match) ([]GroupRow, error) {
	where, args := s.filter(start, end, match)
	q := fmt.Sprintf(`%s
SELECT s.%s AS g, count() AS calls, countIf(s.StatusCode = 'Error') AS errors,
       toFloat64(sum(s.Duration)) AS cum_ns, toFloat64(%s) AS self_ns
FROM %s s %s
WHERE %s
GROUP BY g ORDER BY self_ns DESC %s`,
		s.childrenCTE(), col.SQL, selfExpr, s.table, joinChildren, where, settings)
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("group by %s: %w", col.Flag, err)
	}
	defer rows.Close()
	var out []GroupRow
	for rows.Next() {
		var r GroupRow
		if err := rows.Scan(&r.Name, &r.Calls, &r.Errors, &r.CumNs, &r.SelfNs); err != nil {
			return nil, fmt.Errorf("group by %s: scan: %w", col.Flag, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) HotOps(ctx context.Context, start, end time.Time, top int, match *Match) ([]OpRow, error) {
	where, args := s.filter(start, end, match)
	q := fmt.Sprintf(`%s
SELECT s.ServiceName AS svc, s.SpanName AS op, count() AS calls,
       countIf(s.StatusCode = 'Error') AS errors,
       toFloat64(sum(s.Duration)) AS cum_ns, toFloat64(%s) AS self_ns,
       avg(s.Duration) AS avg_ns, quantile(0.95)(s.Duration) AS p95_ns,
       quantile(0.99)(s.Duration) AS p99_ns
FROM %s s %s
WHERE %s
GROUP BY svc, op ORDER BY self_ns DESC LIMIT %d %s`,
		s.childrenCTE(), selfExpr, s.table, joinChildren, where, top, settings)
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("hot operations: %w", err)
	}
	defer rows.Close()
	var out []OpRow
	for rows.Next() {
		var r OpRow
		if err := rows.Scan(&r.Service, &r.Op, &r.Calls, &r.Errors, &r.CumNs, &r.SelfNs,
			&r.AvgNs, &r.P95Ns, &r.P99Ns); err != nil {
			return nil, fmt.Errorf("hot operations: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ErrorOps(ctx context.Context, start, end time.Time, top int, match *Match) ([]ErrRow, error) {
	where, args := s.filter(start, end, match)
	q := fmt.Sprintf(`SELECT s.ServiceName AS svc, s.SpanName AS op,
       count() AS calls, countIf(s.StatusCode = 'Error') AS errors
FROM %s s WHERE %s
GROUP BY svc, op HAVING errors > 0 ORDER BY errors DESC LIMIT %d`,
		s.table, where, top)
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("error operations: %w", err)
	}
	defer rows.Close()
	var out []ErrRow
	for rows.Next() {
		var r ErrRow
		if err := rows.Scan(&r.Service, &r.Op, &r.Calls, &r.Errors); err != nil {
			return nil, fmt.Errorf("error operations: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NameCount is a (name, count) pair for the discovery subcommands.
type NameCount struct {
	A, B  string
	Count uint64
}

func (s *Store) Services(ctx context.Context, start, end time.Time) ([]NameCount, error) {
	q := fmt.Sprintf(`SELECT s.ServiceName AS n, count() AS c FROM %s s
WHERE s.Timestamp >= {start:DateTime64(9)} AND s.Timestamp < {end:DateTime64(9)}
GROUP BY n ORDER BY c DESC`, s.table)
	return s.nameCounts(ctx, q, false, start, end)
}

func (s *Store) Operations(ctx context.Context, start, end time.Time, top int) ([]NameCount, error) {
	q := fmt.Sprintf(`SELECT s.ServiceName AS a, s.SpanName AS b, count() AS c FROM %s s
WHERE s.Timestamp >= {start:DateTime64(9)} AND s.Timestamp < {end:DateTime64(9)}
GROUP BY a, b ORDER BY c DESC LIMIT %d`, s.table, top)
	return s.nameCounts(ctx, q, true, start, end)
}

func (s *Store) nameCounts(ctx context.Context, q string, pair bool, start, end time.Time) ([]NameCount, error) {
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q,
		clickhouse.Named("start", start), clickhouse.Named("end", end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var r NameCount
		if pair {
			if err := rows.Scan(&r.A, &r.B, &r.Count); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&r.A, &r.Count); err != nil {
				return nil, err
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Tables lists the otel_* tables present, so `otelhousereport tables` can tell
// the user what signals this ClickHouse actually holds (traces always; logs and
// metrics only if the Collector was configured to export them).
func (s *Store) Tables(ctx context.Context) ([]string, error) {
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx,
		"SELECT name FROM system.tables WHERE database = currentDatabase() AND name LIKE 'otel_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FullRange returns the first and last span timestamps across the whole table,
// ignoring the window. It runs only on the empty-window path, to tell the user
// where the data actually is instead of asserting the cluster is idle.
func (s *Store) FullRange(ctx context.Context) (time.Time, time.Time, uint64, error) {
	cctx, cancel := s.call(ctx)
	defer cancel()
	var mn, mx time.Time
	var n uint64
	err := s.conn.QueryRow(cctx,
		fmt.Sprintf("SELECT min(Timestamp), max(Timestamp), count() FROM %s", s.table)).Scan(&mn, &mx, &n)
	if err != nil {
		return time.Time{}, time.Time{}, 0, err
	}
	return mn, mx, n, nil
}

// errTableMissing is returned by explainEmpty when the configured table does
// not exist, which is a configuration error rather than an empty window.
var errTableMissing = errors.New("table not found")
