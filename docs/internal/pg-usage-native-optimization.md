# PostgreSQL usage benchmark decision

PostgreSQL usage currently uses the live query methods in
`internal/postgres/usage.go`. This benchmark-first slice records the evidence
needed before changing that query shape. It measures the five public usage
methods, their supported windows and breakdown modes, and the cold push, delta
push, and catalog probe paths against the pinned PostgreSQL 16 compose
service.

The benchmark reuses the SQLite/PostgreSQL parity fixture, including survivor,
reported-cost, rounding, timestamp, and activity-only cases. Each read case
compares complete SQLite and PostgreSQL results for all five usage methods
before timing. `eligible_usage_input_rows` counts eligible fixture message and
usage-event inputs outside timing, not PostgreSQL execution-plan rows, while
`bytes_token_usage` measures token-usage bytes from those inputs. `ns/op`,
`B/op`, and `allocs/op` come from the Go benchmark runner.

The read workload is configured as 500 sessions with 200 messages per session,
ten projects, three agents, four priced models, and four date buckets, for
100,000 bulk messages. The correctness and standard refresh benchmarks retain
the smaller parity fixture so their exact push and result assertions stay
deterministic. `BenchmarkPGUsageRefreshLarge` deliberately loads the
repository-scale corpus to measure cold-load and multi-message delta behavior
separately.

This is evidence only. It does not choose a facts table, aggregate, trigger,
refresh schedule, or materialized view. Go-owned pricing, per-survivor
microdollar rounding, Cockroach-compatible SQL, SQLite parity, and read-only
PostgreSQL serving remain the authority. Run the opt-in target with:

```text
make bench-pg-usage
```
