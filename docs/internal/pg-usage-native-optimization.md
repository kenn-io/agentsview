# PostgreSQL usage benchmark decision

PostgreSQL usage currently uses the live query methods in
`internal/postgres/usage.go`. This benchmark-first slice records the evidence
needed before changing that query shape. It measures the five public usage
methods, their supported windows and breakdown modes, and the cold push, delta
push, and catalog maintenance paths against the pinned PostgreSQL 16 compose
service.

The benchmark reuses the SQLite/PostgreSQL parity fixture, including survivor,
reported-cost, rounding, timestamp, and activity-only cases. Exact non-empty
results are asserted before timing. `rows_scanned` and `bytes_token_usage` are
reported as fixture workload metrics, while `ns/op`, `B/op`, and `allocs/op`
come from the Go benchmark runner.

This is evidence only. It does not choose a facts table, aggregate, trigger,
refresh schedule, or materialized view. Go-owned pricing, per-survivor
microdollar rounding, Cockroach-compatible SQL, SQLite parity, and read-only
PostgreSQL serving remain the authority. Run the opt-in target with:

```text
make bench-pg-usage
```
