# otelhousereport

A CLI that turns the OpenTelemetry **traces** in ClickHouse into a **Markdown
report** — built to be read by an agent, not just a human.

It is the trace-side sibling of
[`parcareport`](https://github.com/guettli/parcareport). `parcareport` turns a
Parca profiling server into a cross-cluster *CPU* bottleneck report;
`otelhousereport` turns a ClickHouse full of spans into a *"where did latency
and errors go"* report over a time window, normalized so a 1-hour window and a
24-hour window are directly comparable.

The traces are the ones the OpenTelemetry Collector's
[`clickhouseexporter`](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/exporter/clickhouseexporter)
writes: the `otel_traces` table. This tool only ever **reads** it.

```console
$ export CLICKHOUSE_DSN='clickhouse://ro:***@ch:9000/otel'
$ otelhousereport --from=-24h
```

```markdown
# otelhousereport

- **Source:** ClickHouse table `otel_traces`
- **Window:** `2026-08-26T17:56:57Z` .. `2026-08-27T17:56:57Z` (24h0m0s)
- **Spans:** 46,456 in 8,570 traces across 4 service(s)
- **Errors:** 500 (1.1% of spans)
- **In flight:** 0.592 spans on average (self-time ÷ wall-time)

## Where time goes — by service

| SERVICE                       | INFLIGHT | %TIME |  CALLS | ERRORS |
| :---------------------------- | -------: | ----: | -----: | -----: |
| unknown_service:dagger-engine |    0.372 |  62.9 | 14,548 |    173 |
| agentloop                     |    0.219 |  37.0 | 30,932 |    323 |
| **total**                     |    0.592 | 100.0 | 46,456 |    500 |

## Hottest operations (by self-time)

| SERVICE   | OPERATION       | CALLS | SELF  | AVG    | P95    | P99    | ERR% |
| :-------- | :-------------- | ----: | ----: | -----: | -----: | -----: | ---: |
| …engine   | resume withExec |   508 | 4.3h  | 30.75s | 5m02s  | 5m03s  |  3.5 |
| agentloop | POST /graphql   | 6,244 | 1.8h  | 1.05s  | 1.70s  | 2.36s  |  0.0 |
| agentloop | tick            | 2,447 | 17m11s| 6.82s  | 15.15s | 20.38s | 12.8 |
```

## INFLIGHT, and why not raw span counts

`INFLIGHT` is **self-time ÷ wall-time**: the average number of spans of a kind
running at once over the window. `0.5` means that, on average across the whole
window, half a span of that kind was in flight.

This is the point of the tool, and it is the same idea as `parcareport`'s
`CORES`. Raw span counts are not comparable between a 1-hour window and a
24-hour one, and they conflate a handful of very slow spans with a flood of fast
ones. Self-time-per-wall-time is an absolute rate: "agentloop keeps 0.22 spans
busy" means the same thing whatever window you pick, and you can watch it move
across a change.

### Self-time, and the double-counting it avoids

Spans **nest**: a parent span's duration already includes its children's. So you
cannot sum `Duration` across spans to learn where time went — a request that
calls three services would be counted four times.

`otelhousereport` ranks by **self-time** instead: a span's own duration minus
the time its child spans covered. Summed across a group, self-time adds up to
real elapsed work with no double counting, which is why the `%TIME` column sums
to 100.

Self-time is an **approximation**, and the tool says so in its own footer.
Children can overlap each other, or a child recorded on a different host can
carry enough clock skew to run "longer" than its parent; the tool floors each
span's self-time at zero so neither corrupts the sum, but a deeply concurrent
service will have its self-time slightly understated. Judge the ranking, not the
third decimal.

## Honest about empty and partial results

Borrowed wholesale from `parcareport`, because the failure modes are the same.

**An empty answer is not the same as no problem.** A window that misses the data
looks identical to an idle cluster looks identical to a wrong `--table`. Rather
than assert the convenient reading, the tool cross-checks and names the reason:

```
no spans in 2035-01-01T00:00:00Z .. 2035-01-02T00:00:00Z; otel_traces holds
442,876 spans from 2026-08-18T15:43:53Z .. 2026-08-27T17:57:10Z — widen
--from/--to to cover that

table "otel_nope" not found; otel tables present: otel_logs, otel_traces — set --table

no spans in <window> matching --match service="doesnotexist"; drop --match or
check the value with `otelhousereport services`
```

**A partial report is never presented as complete.** The report is several
independent queries; any one can fail on a slow window or a restarting server.
If a section fails, its numbers would silently drop out and the totals would
still look whole. So a failed section is called out in an `⚠️ INCOMPLETE` block
**inside the report**, next to the numbers it invalidates, and the command
**exits non-zero** — so an agent that checks the status does not mistake an
incomplete report for a clean one.

## Install

```sh
go install github.com/guettli/otelhousereport@latest
```

## Usage

```
otelhousereport [report] [flags]     the report (default)
otelhousereport services [flags]     list services seen in the window
otelhousereport operations [flags]   list operations seen in the window
otelhousereport tables [flags]       list the otel_* tables present
```

| Flag | Default | Meaning |
|---|---|---|
| `--dsn` | `$CLICKHOUSE_DSN` | ClickHouse DSN, e.g. `clickhouse://ro:***@ch:9000/otel` |
| `--from` | `-1h` | window start: RFC3339, or relative (`-6h`, `-90m`, `-7d`, `-1w`; compound like `1d6h`) |
| `--to` | `now` | window end |
| `--by` | `service` | breakdown column: `service`, `name`, `kind`, `status` |
| `--match` | | filter to one value, e.g. `service=agentloop` |
| `--top` | `15` | rows in the operation and error tables (`0` = summary only: header + breakdown) |
| `--out` | | write to this file instead of stdout |
| `--timeout` | `25s` | per-query timeout (see the note below) |

```sh
otelhousereport --from=-24h                       # last day, by service
otelhousereport --by=name --top=30                # hottest operations, wider
otelhousereport --match=service=agentloop --by=name  # drill into one service
otelhousereport --from=-7d --out=report.md        # write a file for an agent
otelhousereport services --from=-24h              # what services exist
```

### For agents

The whole report is one self-contained Markdown document with stable section
headings (`## Where time goes`, `## Hottest operations`, `## Errors`). Point an
agent at `otelhousereport --from=-6h` (or `--out=report.md`) to get a compact,
parseable answer to *"where is time and where are the errors right now"* before
it goes digging in individual traces.

## Notes

- **`--timeout` also caps ClickHouse's `max_execution_time`.** The clickhouse-go
  driver turns the per-query deadline into that server setting, and a read-only
  user profile commonly constrains it to `<= 30s`. The default is `25s` for that
  reason; raise it only if the server allows a larger cap, or a wide-window query
  will be rejected outright rather than merely slow.
- **Start narrow.** The self-time breakdown is a windowed self-join of the
  traces table; a wide window over a busy table is real work for the server.
- **Read-only by design.** The tool issues no DDL, no writes, and never
  interpolates a user-supplied *value* into SQL — times, `--match` values and
  the like are always bound as native `{name:Type}` parameters. The only
  identifiers that reach the SQL by name (the table and the group-by column) are
  whitelisted.

## License

Apache-2.0
