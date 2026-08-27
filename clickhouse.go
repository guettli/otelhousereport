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
	// exactSelf switches the children CTE from summing child durations (fast,
	// slightly under-counts self time when children overlap) to merging child
	// intervals into their union (exact, heavier). Set per run by buildReport.
	exactSelf bool
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

// maxExecRe pulls the cap out of ClickHouse's max_execution_time rejection so
// the tool can name the exact number to pass.
var maxExecRe = regexp.MustCompile(`max_execution_time shouldn't be greater than (\d+)`)

// translateCHError rewrites the one ClickHouse error a user is most likely to
// hit and least likely to understand. clickhouse-go turns the per-query timeout
// into the server-side max_execution_time setting; a read-only profile commonly
// caps that at 30s, and the raw rejection ("code: 452 … shouldn't be greater
// than 30") names neither --timeout nor what to do. This says both.
func translateCHError(err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	if m := maxExecRe.FindStringSubmatch(err.Error()); m != nil {
		return fmt.Errorf("the ClickHouse server caps max_execution_time at %ss, "+
			"but --timeout is %s and the driver sends it as that setting; "+
			"lower --timeout to <= %ss: %w", m[1], timeout, m[1], err)
	}
	return err
}

// qerr wraps a query error with what was being done and the translation above,
// so every call site gets the friendly max_execution_time message for free.
func (s *Store) qerr(what string, err error) error {
	return fmt.Errorf("%s: %w", what, translateCHError(err, s.timeout))
}

// call bounds one query with the store's per-query timeout.
func (s *Store) call(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, s.timeout)
}

// Column is an injection-safe reference to something to group or filter by:
// either a whitelisted traces column, or a key inside the ResourceAttributes /
// SpanAttributes map. For a map key the key itself is bound as a query
// parameter, so arbitrary attribute names — dots, slashes, whatever the app
// emitted — are safe.
type Column struct {
	Flag   string // user token, for titles and messages: "service", "res:tenant"
	Header string // table-header text
	col    string // fixed column name (whitelisted), e.g. "ServiceName"; "" for a map key
	mapCol string // "ResourceAttributes" | "SpanAttributes" for a map key; "" for a fixed column
	key    string // the attribute key (bound, never concatenated)
}

// exprAndArgs renders the column as an s.-qualified SQL expression plus any
// bound args it needs. paramName must be unique within a single query.
func (c Column) exprAndArgs(paramName string) (string, []any) {
	if c.mapCol != "" {
		return fmt.Sprintf("s.%s[{%s:String}]", c.mapCol, paramName),
			[]any{clickhouse.Named(paramName, c.key)}
	}
	return "s." + c.col, nil
}

var fixedColumns = map[string]struct{ Header, SQL string }{
	"service": {"SERVICE", "ServiceName"},
	"name":    {"NAME", "SpanName"},
	"kind":    {"KIND", "SpanKind"},
	"status":  {"STATUS", "StatusCode"},
}

// resolveColumn maps a user token to a Column. It accepts the short alias
// ("service"), the ClickHouse column name ("ServiceName"), and an attribute
// reference ("res:<key>" / "resource:<key>" for ResourceAttributes,
// "span:<key>" / "attr:<key>" for SpanAttributes).
func resolveColumn(name string) (Column, error) {
	if mapCol, key, ok := attrRef(name); ok {
		if key == "" {
			return Column{}, fmt.Errorf("empty attribute key in %q (want e.g. res:tenant or span:http.request.method)", name)
		}
		return Column{Flag: name, Header: strings.ToUpper(key), mapCol: mapCol, key: key}, nil
	}
	key := strings.ToLower(name)
	if c, ok := fixedColumns[key]; ok {
		return Column{Flag: key, Header: c.Header, col: c.SQL}, nil
	}
	for k, c := range fixedColumns {
		if strings.EqualFold(c.SQL, name) {
			return Column{Flag: k, Header: c.Header, col: c.SQL}, nil
		}
	}
	return Column{}, fmt.Errorf("unknown column %q; use service, name, kind, status, or an attribute like res:<key> / span:<key>", name)
}

// attrRef splits an attribute reference into its map column and key.
func attrRef(name string) (mapCol, key string, ok bool) {
	for _, p := range []struct{ prefix, mapCol string }{
		{"resource:", "ResourceAttributes"},
		{"res:", "ResourceAttributes"},
		{"span:", "SpanAttributes"},
		{"attr:", "SpanAttributes"},
	} {
		if strings.HasPrefix(name, p.prefix) {
			return p.mapCol, name[len(p.prefix):], true
		}
	}
	return "", "", false
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

// filter is the shared WHERE fragment (time window plus zero or more --match
// equalities), with the bound arguments that go with it. Building the args
// alongside the SQL keeps the two in lockstep and every user value — including
// attribute keys — out of the SQL text. Each match gets uniquely-named params
// so multiple --match flags cannot collide.
func (s *Store) filter(start, end time.Time, matches []Match) (string, []any) {
	args := []any{clickhouse.Named("start", start), clickhouse.Named("end", end)}
	where := "s.Timestamp >= {start:DateTime64(9)} AND s.Timestamp < {end:DateTime64(9)}"
	for i, m := range matches {
		expr, cargs := m.Col.exprAndArgs(fmt.Sprintf("mk%d", i))
		vp := fmt.Sprintf("mv%d", i)
		where += fmt.Sprintf(" AND %s = {%s:String}", expr, vp)
		args = append(args, cargs...)
		args = append(args, clickhouse.Named(vp, m.Value))
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
	childDur := "sum(Duration) AS child_dur"
	if s.exactSelf {
		// Union of each parent's child intervals, not the sum of their
		// durations. arrayFold sweeps the intervals sorted by start, carrying
		// (curEnd, covered) and adding only the part of each interval beyond
		// curEnd — so overlapping children are counted once. This is the exact
		// child coverage; subtracting it gives self time with no under-count.
		childDur = `arrayFold(
      (acc, iv) -> (greatest(acc.1, iv.2), acc.2 + greatest(toInt64(0), iv.2 - greatest(iv.1, acc.1))),
      arraySort(x -> x.1, groupArray((
        toInt64(toUnixTimestamp64Nano(Timestamp)),
        toInt64(toUnixTimestamp64Nano(Timestamp)) + toInt64(Duration)))),
      (toInt64(0), toInt64(0))
    ).2 AS child_dur`
	}
	return fmt.Sprintf(`WITH children AS (
  SELECT TraceId, ParentSpanId AS SpanId, %s
  FROM %s
  WHERE Timestamp >= {start:DateTime64(9)} AND Timestamp < {end:DateTime64(9)}
    AND ParentSpanId != '' AND ParentSpanId != '0000000000000000'
  GROUP BY TraceId, ParentSpanId
)`, childDur, s.table)
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

func (s *Store) GetTotals(ctx context.Context, start, end time.Time, matches []Match) (Totals, error) {
	where, args := s.filter(start, end, matches)
	q := fmt.Sprintf(`SELECT count() AS spans, uniqExact(s.TraceId) AS traces,
       uniqExact(s.ServiceName) AS services, countIf(s.StatusCode = 'Error') AS errors,
       min(s.Timestamp) AS mn, max(s.Timestamp) AS mx
FROM %s s WHERE %s`, s.table, where)
	cctx, cancel := s.call(ctx)
	defer cancel()
	var t Totals
	row := s.conn.QueryRow(cctx, q, args...)
	if err := row.Scan(&t.Spans, &t.Traces, &t.Services, &t.Errors, &t.Min, &t.Max); err != nil {
		return Totals{}, fmt.Errorf("totals: %w", translateCHError(err, s.timeout))
	}
	return t, nil
}

func (s *Store) GroupBy(ctx context.Context, col Column, start, end time.Time, matches []Match) ([]GroupRow, error) {
	gexpr, gargs := col.exprAndArgs("bykey")
	where, args := s.filter(start, end, matches)
	args = append(args, gargs...)
	q := fmt.Sprintf(`%s
SELECT %s AS g, count() AS calls, countIf(s.StatusCode = 'Error') AS errors,
       toFloat64(sum(s.Duration)) AS cum_ns, toFloat64(%s) AS self_ns
FROM %s s %s
WHERE %s
GROUP BY g ORDER BY self_ns DESC %s`,
		s.childrenCTE(), gexpr, selfExpr, s.table, joinChildren, where, settings)
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("group by %s: %w", col.Flag, translateCHError(err, s.timeout))
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

func (s *Store) HotOps(ctx context.Context, start, end time.Time, top int, matches []Match) ([]OpRow, error) {
	where, args := s.filter(start, end, matches)
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
		return nil, fmt.Errorf("hot operations: %w", translateCHError(err, s.timeout))
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

func (s *Store) ErrorOps(ctx context.Context, start, end time.Time, top int, matches []Match) ([]ErrRow, error) {
	where, args := s.filter(start, end, matches)
	q := fmt.Sprintf(`SELECT s.ServiceName AS svc, s.SpanName AS op,
       count() AS calls, countIf(s.StatusCode = 'Error') AS errors
FROM %s s WHERE %s
GROUP BY svc, op HAVING errors > 0 ORDER BY errors DESC LIMIT %d`,
		s.table, where, top)
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("error operations: %w", translateCHError(err, s.timeout))
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

// logsTable is the stock clickhouseexporter logs table. Unlike the traces
// table it is not configurable: --logs is a convenience correlation, and a
// deployment that renamed its logs table can still get everything else.
const logsTable = "otel_logs"

// LogRow is one recurring log line correlated to an error span.
type LogRow struct {
	Service, Op, Severity, Body string
	Count                       uint64
}

// ErrorLogs answers "why did it error" by joining otel_logs to the error spans
// in the window (on TraceId+SpanId) and returning the recurring log lines,
// highest severity first. It is opt-in (--logs) because it is a second join and
// most reports do not need it.
//
// Ordering by severity, not count, is deliberate: a single ERROR line that
// names the failure is worth more than hundreds of routine WARNs emitted inside
// the same span, and count-first ordering would bury it.
func (s *Store) ErrorLogs(ctx context.Context, start, end time.Time, top int, matches []Match) ([]LogRow, error) {
	where, args := s.filter(start, end, matches)
	q := fmt.Sprintf(`SELECT sp.ServiceName AS svc, sp.SpanName AS op,
       l.SeverityText AS sev, substring(l.Body, 1, 200) AS body,
       count() AS c, max(l.SeverityNumber) AS sevnum
FROM %s l
INNER JOIN (
  SELECT s.TraceId AS TraceId, s.SpanId AS SpanId, s.ServiceName AS ServiceName, s.SpanName AS SpanName
  FROM %s s WHERE %s AND s.StatusCode = 'Error'
) sp ON l.TraceId = sp.TraceId AND l.SpanId = sp.SpanId
WHERE l.Timestamp >= {start:DateTime64(9)} AND l.Timestamp < {end:DateTime64(9)}
GROUP BY svc, op, sev, body
ORDER BY sevnum DESC, c DESC LIMIT %d`,
		logsTable, s.table, where, top)
	cctx, cancel := s.call(ctx)
	defer cancel()
	rows, err := s.conn.Query(cctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("error logs: %w", translateCHError(err, s.timeout))
	}
	defer rows.Close()
	var out []LogRow
	for rows.Next() {
		var r LogRow
		var sevnum uint8
		if err := rows.Scan(&r.Service, &r.Op, &r.Severity, &r.Body, &r.Count, &sevnum); err != nil {
			return nil, fmt.Errorf("error logs: scan: %w", err)
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

// HasTable reports whether a table exists in the current database. It is used
// to skip the --logs correlation cleanly when otel_logs is absent, rather than
// letting the join fail and clutter the report with an INCOMPLETE note.
func (s *Store) HasTable(ctx context.Context, name string) bool {
	tabs, err := s.Tables(ctx)
	if err != nil {
		return false
	}
	for _, t := range tabs {
		if t == name {
			return true
		}
	}
	return false
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
