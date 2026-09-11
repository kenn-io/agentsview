---
title: Reporting Export
description: Canonical hourly activity and usage exports for reporting integrations
---

`agentsview export hour`, `day`, and `digest` expose a versioned, canonical
reporting contract from the local SQLite archive. The commands are intended for
durable reporting integrations that need exact UTC-hour correction and
content-derived change detection.

## Commands

```bash
agentsview export hour --schema-version 3 2026-07-28-13
agentsview export day --schema-version 3 2026-07-28
agentsview export digest --schema-version 3 --from 2026-06-28 --to 2026-07-27
```

All dates and hours are UTC and must use the exact zero-padded forms shown
above. An hour is available only after it has closed. A past date contains 24
hours; the current UTC date contains only closed hours and has no day digest.
Digest ranges are inclusive, require both bounds, and may contain at most 31
dates.

Version 3 is the default and the only supported version. Hour, day, and digest
commands accept `--schema-version 3`; versions 1 and 2 are no longer available.
Any other value is rejected before the archive is opened or output is written.
Integrations pinned to an older version must update to version 3 and refresh
their saved digests.

Each successful command writes exactly one canonical JSON document followed by
one newline. Diagnostics go to stderr. A malformed or future period, an open
hour, a reversed or oversized digest range, or an unavailable archive produces a
non-zero exit.

## Hour and day documents

The v3 hour shape is:

```json
{
  "schema_version": 3,
  "period": "2026-07-28-13",
  "digest": "sha256:...",
  "has_data": true,
  "activity": {
    "totals": {},
    "peak": {},
    "interactive_peak": {},
    "subagent_peak": {},
    "automated_peak": {},
    "buckets": [],
    "by_model": [],
    "by_agent": [],
    "by_project": []
  },
  "usage": {
    "totals": {},
    "by_model": [],
    "by_agent": [],
    "by_project": []
  }
}
```

Activity always contains exactly twelve consecutive five-minute buckets.
Activity separates interactive sessions, subagents, and automated sessions.
Subagents take precedence over automation, so an automated child belongs only to
the subagent category. Totals and all model, agent, and project breakdowns carry
separate minutes and costs for each category. Combined minutes, cost, tokens,
and peak concurrency still include all three categories.

`interactive_peak`, `subagent_peak`, and `automated_peak` contain each
category's independent maximum and its earliest timestamp. Each bucket also
includes `max_interactive_agents`, `max_subagent_agents`, and
`max_automated_agents`. These maxima may occur at different times. The
`interactive_at_peak`, `subagent_at_peak`, and `automated_at_peak` fields
instead describe the composition at that bucket's combined peak.

Activity totals include additive first-seen counts for sessions, interactive,
subagent, and automated sessions, untimed sessions, projects, and models.
`new_interactive_sessions + new_subagent_sessions + new_automated_sessions`
equals `new_sessions`; untimed sessions are counted separately and may belong to
any category. A session's first-seen hour is the start of its earliest effective
activity interval after the five-minute gap cap and export-range clipping.
Without such an interval, the fallback order is an in-range session usage event,
activity event, `started_at`, then `created_at`; fallback-only sessions are
untimed.

Every activity total and model, agent, or project breakdown serializes
`agent_minutes` as the exact floating-point sum of
`automated_agent_minutes + interactive_agent_minutes + subagent_agent_minutes`.
Before serialization, the exporter validates the original total and all three
components as finite and non-negative. Given
`derived = automated + interactive + subagent`, the accepted difference is:

```text
abs(original - derived) <= 1e-9 * max(1, abs(original), abs(derived))
```

The boundary is inclusive; a larger difference is rejected.

Usage totals carry input, output, cache-creation, and cache-read tokens plus
cost. Reporting collects activity-eligible sessions, usage-eligible sessions,
and standalone observations independently through one read transaction.
Session-linked and standalone rows are merged before ordering, deduplication,
cost allocation, or aggregation. The resulting complete usage stream feeds
totals and all `by_model`, `by_agent`, and `by_project` breakdowns.

Usage-only sessions load the minimal metadata needed for agent and project
attribution without becoming activity sessions. Standalone observations have no
session or project, so they contribute to usage totals, `by_model`, and
`by_agent` without receiving fabricated `by_project` attribution. Only rows
attached to activity-eligible sessions feed activity minutes, buckets, peaks,
breakdowns, first-seen state, or an activity `new_*` counter. Activity token and
cost totals can therefore differ from usage totals. Money is encoded as integer
microdollars. Project breakdowns include both a safe display label and a stable
archive-scoped `project_key`.

A day document contains its UTC `date`, `complete` and `has_data` flags, its
ordered closed-hour documents, and a `digest` only when all 24 hours are
present. `agentsview export hour H` is constructed by the same day reader and
emits byte-for-byte the canonical hour element contained by `export day D`.

## Quiet hours

A quiet hour means that the archive has no activity or usage observation for
that period. It deliberately does not mean that an agent was observed idle for
60 minutes. A quiet hour therefore has:

- `has_data: false`;
- `idle_minutes: 0`;
- twelve zero-valued five-minute buckets;
- empty activity and usage breakdown arrays; and
- zero first-seen counters.

A completed date with 24 quiet hours still has a stable, non-empty day digest.
The separate `has_data` flag distinguishes content presence from document
identity.

## Snapshot and digest guarantees

Every day is assembled from one SQLite read transaction. Sessions, messages,
usage rows, pricing, and project identity therefore describe one coherent
archive snapshot even when a sync writes concurrently. Usage deduplication and
authoritative session-cost allocation happen once on the merged usage stream
across the day before rows are partitioned by hour.

Usage selects the greatest output-token snapshot for each Claude message/request
identity before generic deduplication, retains the earliest session for
attribution, and carries the maximum observed web-search count into pricing.
Usage rows are ordered by occurrence time, session ID ascending, and
`COALESCE(message_ordinal, -1)` ascending. A standalone row retains its empty
session ID and therefore sorts before a session-linked row at the same time.
Source and the remaining semantic fields provide deterministic tie-breakers only
after that shared ordering prefix.

Arrays use stable contract ordering. Canonical JSON preserves declared JSON
field names and encodes money exactly. Reporting exports use a project-specific
canonical format and do not claim RFC 8785 or JSON Canonicalization Scheme
compliance. Its byte rules are:

- Go JSON field names, `omitempty`, and custom marshalers are applied first;
- object keys sort lexicographically by UTF-16 code units;
- array order is preserved;
- output contains no insignificant whitespace;
- strings use Go JSON escaping with HTML escaping disabled: `<`, `>`, and `&`
  remain literal, defined short control escapes such as `\b` and `\f` are
  used, other control characters use `\u00xx`, and U+2028/U+2029 are escaped;
- integers use minimal base-10 form and preserve their complete value, including
  values above `2^53`;
- negative zero is encoded as `0`;
- finite floating-point values use the shortest round-trippable decimal form,
  with plain notation from magnitude `1e-6` through values below `1e21` and
  normalized exponent notation outside that range; and
- non-finite numbers are rejected.

Digests are derived as follows:

1. An hour digest is SHA-256 over the canonical hour content with the derived
   `digest` field omitted.
1. A completed-day digest is SHA-256 over the canonical ordered array of its 24
   hour digest strings.
1. An incomplete current date has ordered hour digests but no day digest.

`export digest` returns only those identities and presence flags:

```json
{
  "schema_version": 3,
  "from": "2026-07-27",
  "to": "2026-07-28",
  "days": [
    {
      "date": "2026-07-27",
      "complete": true,
      "has_data": false,
      "day_digest": "sha256:...",
      "hour_digests": ["sha256:..."]
    }
  ]
}
```

Consumers can screen completed dates by `day_digest`, then compare the 24
ordered `hour_digests` and fetch only changed hours.

### Canonical vectors

These vectors contain the complete canonical UTF-8 text with no trailing
newline. SHA-256 is computed over exactly those bytes.

```text
Input:     {"\ue000":"bmp","\ud800\udc00":"astral"}
Canonical: {"𐀀":"astral","":"bmp"}
SHA-256:   5e72745dd500f8b8d997ef851679707b89099da29d2aca4b93dfd85810ebaa20

Input:     {"text":"<>&\b\f\u2028\u2029"}
Canonical: {"text":"<>&\b\f\u2028\u2029"}
SHA-256:   654cd6bbd6c7311e46686b6cbf6dbfc9f092258e669b2d0ce2f286a5e81dd2bb

Input:     {"n":9007199254740993}
Canonical: {"n":9007199254740993}
SHA-256:   4ac8309cc76123ef6c5325ef925fc873e9b5856ec4f844ef1462f9303960378a

Input:     {"small":0.0000001,"plain":0.000001,"negative_zero":-0.0,"large":1e21}
Canonical: {"large":1e+21,"negative_zero":0,"plain":0.000001,"small":1e-7}
SHA-256:   940f129aabf5afc6800add24fbf597727e9dc6316f6ae10adbc78a3362b1c483
```

## Versioning

Version 3 separates subagents from interactive and automated sessions and adds
independent concurrency peaks for each category. It retains the complete Claude
snapshot selection and web-search charging introduced in version 2. Versions 1
and 2 are no longer emitted.

Integrations should request and require `schema_version: 3`, reject unknown
fields, and verify the canonical content digest before accepting an hour. The
new fields change hour and day digests, including quiet hours; refresh
previously saved digests when updating. Adding, renaming, or removing a field,
changing a type or accounting rule, or changing canonicalization requires a new
schema version.

## Local SQLite scope

Reporting export intentionally uses the existing read-only local SQLite export
path. It can run while the writable daemon owns the archive, and it does not
require that daemon to be running. It does not switch to PostgreSQL or DuckDB:
the local archive is the canonical source for the device's reporting payload,
dedup order, pricing view, and archive-scoped project keys.

Costs are estimates computed from the pricing catalog visible to the read
transaction. Reporting exports do not pin a closed hour to the catalog revision
that was current when the hour closed, so a later catalog change can change cost
and therefore the hour digest without changing token facts. Integrations should
treat such a digest change as an ordinary source correction.

When the archive has no stored model-pricing rows, the read-only reporting
command applies the embedded fallback catalog and configured custom prices in
memory. It does not write to the archive or replace a non-empty stored catalog.

The sum of a completed export day's usage fields reconciles with the Usage view
for the same UTC date and filters. Usage-session selection follows usage
timestamps independently of the session activity window.

The checked-in fixtures under `cmd/agentsview/testdata/reporting/` freeze v3
bytes and their SHA-256 manifest for portable contract tests.
