# Reporting Export Schema v1 Completion Design

**Goal:** Complete reporting schema v1 with comprehensive usage collection,
layout-independent canonical output, internally consistent minute totals,
explicit CLI version selection, and portable synthetic fixtures.

## Scope

Schema version 1 remains the only supported reporting schema. Existing commands
continue to default to version 1, and callers may select it explicitly.
Unsupported versions fail before the database is opened or output is written.

Test data, fixtures, documentation, and examples use conspicuously synthetic
identities. Fixture machines use names such as `fixture-machine`, repository
examples use `github.com/acme/example`, and any required domains use
`example.invalid`.

## Independent input collection

The exporter collects three input sets through one read transaction:

1. activity-eligible sessions whose activity window overlaps the reporting
   range;
1. usage-eligible sessions selected independently by in-range usage facts; and
1. standalone usage events selected by their occurrence time.

Usage-only sessions load only the session metadata needed for agent and project
attribution. They do not become activity sessions. Standalone events have no
session or project, and the exporter does not fabricate either value.

Raw session-linked and standalone rows are merged into `allUsage` before any
ordering, deduplication, cost allocation, or aggregation. The merged rows are
globally ordered by stable semantic fields, deduplicated once, and assigned
costs once for the complete reporting range.

`allUsage` feeds the complete usage projection:

- totals;
- `by_model`;
- `by_agent`; and
- `by_project`.

Standalone rows naturally cannot contribute to `by_project`. The exporter
derives `activityUsage` by filtering `allUsage` to session IDs in the
activity-eligible set. Only `activityUsage` may feed activity calculations,
first-seen state, or `new_*` counters. Usage-only sessions and standalone events
never change activity minutes, activity breakdowns, first-seen state, or any
`new_*` counter.

## Global semantic ordering

Equivalent stored facts must produce identical canonical bytes and digests
regardless of insertion order, physical database layout, or SQL parameter
chunking.

After all chunks have been read, the exporter globally orders:

- sessions and candidate session IDs by session ID;
- activity events by session ID, ordinal, timestamp, role, model, and other
  persisted semantic tie fields; and
- normalized usage rows first by occurrence time, session ID ascending, and
  `COALESCE(message_ordinal, -1)` ascending, matching the ordering prefix used
  by daily usage, and then by source, deduplication identity, model, token
  counts, cost attributes, agent, and project.

Ordering never uses row IDs or other storage-layout identifiers. Per-chunk SQL
ordering is not treated as global ordering.

Normalized usage rows retain their source and message ordinal until after the
single merged survivor pass. An absent session ID remains the empty string and
therefore sorts before a non-empty session ID; standalone rows do not receive a
fabricated session ID to alter that order. The fields after the daily-usage
ordering prefix are deterministic semantic tie-breakers only. They do not
replace or precede the first-seen ordering contract.

## Derived minute totals

Every split-bearing activity total obeys this invariant:

```text
agent_minutes = automated_agent_minutes + interactive_agent_minutes
```

Before replacing an original total with the derived value, the exporter
validates the original total and both components. Each value must be finite and
non-negative. Given:

```text
derived = automated_agent_minutes + interactive_agent_minutes
scale = max(1, abs(original), abs(derived))
tolerance = 1e-9 * scale
```

the input is valid when:

```text
abs(original - derived) <= tolerance
```

The boundary is inclusive. A difference greater than the tolerance is rejected.
After validation, the exporter serializes `agent_minutes` as the exact
floating-point result of adding the two serialized components. This applies to
activity totals and every model, agent, and project activity breakdown. Buckets
without automated and interactive components are outside this invariant.

## CLI schema selection

The `export hour`, `export day`, and `export digest` commands each accept:

```text
--schema-version 1
```

Omitting the option preserves the version 1 default. Any other value is rejected
before invoking the database opener and before writing output. A small
command-construction seam allows tests to prove that unsupported versions do not
open the database.

## Read-only pricing bootstrap

After opening the archive, reporting commands check whether `model_pricing`
contains any non-metadata rows. If it is empty, the exporter installs the
embedded fallback catalog plus configured custom pricing as an in-memory
effective-pricing overlay. This is the same empty-catalog behavior used by the
session exporter: it does not write to the read-only archive and does not
override a stored catalog.

If the pricing check or fallback setup fails, the reporting database is closed
before the command returns the error. Hour, day, and digest commands all use
this prepared database path.

## Canonical output and fixtures

Version 1 retains the documented canonical JSON algorithm:

- object keys sort lexicographically by UTF-16 code units;
- arrays preserve their defined semantic order;
- output contains no insignificant whitespace;
- strings use the documented Go JSON escaping behavior with HTML escaping
  disabled;
- integers use minimal base-10 form without losing precision;
- finite floating-point values use the documented shortest round-trippable form;
- negative zero serializes as `0`;
- non-finite values are rejected; and
- an hour digest is SHA-256 over the canonical hour object with its `digest`
  field omitted.

The hour, day, and digest fixtures and their SHA-256 manifest are regenerated
from exclusively synthetic source data after behavior is final. Documentation
describes the three input sets, complete usage projection, activity filtering,
minute invariant, schema selection, and canonical byte rules.

## Validation

Focused regressions cover:

- an activity session, a usage-only session, and standalone usage rows merged
  before deduplication, with the complete usage projection asserted and no
  activity or first-seen contamination;
- a duplicate spanning session-linked and standalone inputs, proving that the
  merged stream is deduplicated once and that the survivor matches the
  daily-usage timestamp, ascending session-ID, and ordinal ordering prefix;
- equal-prefix usage rows with different sources and semantic values, proving
  deterministic trailing tie-breakers without changing the daily-usage ordering
  prefix;
- more than one SQL parameter chunk of equivalent data inserted in different
  orders, asserting exact canonical bytes, exact digests, and at least one
  literal expected aggregate total;
- explicit global activity-event ordering across chunk boundaries;
- valid minute totals, exact tolerance-boundary acceptance, just-over-boundary
  rejection, non-finite rejection, and negative-value rejection;
- default and explicit version 1 output for hour, day, and digest commands;
- unsupported schema rejection with an uncalled database opener and empty
  output; and
- a read-only archive with an empty pricing catalog and usage for a known
  embedded model, asserting nonzero exported cost and embedded fallback
  provenance.

Before each commit, inspect the complete staged diff and all changed generated
fixtures. Verify that they contain only conspicuously synthetic identities and
no credentials, identifying values, environment-derived paths, or unrelated
repository content.
