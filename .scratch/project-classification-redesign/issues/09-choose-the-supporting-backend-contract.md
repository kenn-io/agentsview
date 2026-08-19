# Choose the supporting backend contract

Type: grilling

Status: open

Blocked by: 05, 06, 07, 08

## Question

Given the approved workflows, what backend capabilities and API boundaries are
needed for archive-wide folder discovery, atomic correction, impact review, true
reversal, read-only presentation, and multi-machine scope? Prefer the smallest
contract that makes the chosen UX truthful. Produce a capability matrix for
SQLite, PostgreSQL/CockroachDB, and DuckDB, with identical semantics or an
explicit read-only fallback for each workflow. Include authentication and origin
checks for new endpoints, performance budgets and degradation behavior, and the
recovery boundary for interrupted writes.
