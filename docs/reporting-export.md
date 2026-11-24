---
title: Reporting Export
description: Canonical hourly activity and usage exports for reporting integrations
---

Use `agentsview export hour`, `day`, and `digest` to build reports from your
local SQLite archive. Each export names its UTC period and includes a content
digest, a checksum that changes whenever the exported data changes. Your
integration can compare these digests to spot corrections and replace saved
hours.

## Commands

```bash
agentsview export hour --schema-version 3 2026-07-28-13
agentsview export day --schema-version 3 2026-07-28
agentsview export digest --schema-version 3 --from 2026-06-28 --to 2026-07-27
```

Pass all dates and hours in UTC using the exact zero-padded formats shown above.
An hour becomes available only after it closes. A past date has all 24 hours,
while the current UTC date includes only closed hours and has no day digest.
Digest ranges are inclusive, require both `--from` and `--to`, and accept at
most 31 dates.

By default, `hour`, `day`, and `digest` use schema version 3, and you can also
pass `--schema-version 5` for joint cells or `--schema-version 7 --by-machine`
for Cursor account usage. Schema 6 also supports account usage. Versions 1, 2, and 4 are no longer available. Any
unsupported schema version fails before the command opens the archive or writes
output. If your integration is pinned to version 1 or 2, update it to version 3;
if it is pinned to version 4, update it to version 5. In either case, refresh
your saved digests.

When a command succeeds, it writes exactly one canonical JSON document followed by one
newline to stdout, and sends diagnostics to stderr. A malformed or future
period, an open hour, a reversed or oversized digest range, or an unavailable
archive exits with a non-zero code.

## Report an archive that contains several machines

If several archives might hold copies of the same sessions, pass
`--schema-version 5 --by-machine` to `hour`, `day`, or `digest`:

```bash
agentsview export day --schema-version 5 --by-machine 2026-07-28
```

With `--by-machine`, each hour adds `local_machine_id` and a `machines` object.
The keys in `machines` are originating machine identities, and each value is a
complete schema 5 hour without nested machine metadata. `local_machine_id` comes
from the archive's recorded installation rather than its display label, and any
recorded machine aliases resolve to their canonical identities. If your archive
has not recorded its installation identity yet, run `agentsview source sync`
first.

Every machine snapshot uses the same read transaction, bucket resolution,
project selection, pricing, and usage-survivor selection as the combined hour.
The local machine stays present even during quiet hours, so a downstream report
can replace an earlier nonempty copy. Both machine membership and each machine's
content contribute to the hour digest, so `day` and `digest` catch machine-only
changes as well.

Usage with no session machine, including standalone Cursor billing, appears
under `archive:<archive_id>`. That key identifies the archive that supplied the
data, not whether two different archives' account-wide billing is distinct. Do
not treat it as a machine identity or merge it into a named machine.

Schema 6 reports matched Cursor usage under `cursor-account:<user_id>` and sets
`polled_at` to the completion time of the unfiltered event fetch. An unfiltered
sync lists the team roster and fetches usage for each member, which adds
requests for each roster page and member. A member's account receives only the
events returned by that member's fetch for a fully closed hour. Filtered syncs
do not establish account coverage, and events without a match from an unfiltered
sync remain under archive billing. Each account-hour holds only the rows from
that archive's newest poll for the user and UTC hour, while rows from an older
poll stay under archive billing. If a completed poll is empty, it replaces a
previously covered account-hour with zero usage. Project-key exports omit
account parts.

Events already in your archive stay under archive billing until an unfiltered
sync matches them. The first export of an older archive adds empty account
columns in place, and archives with account stamps require data version 185 or
newer. To keep machine IDs distinct from account keys, schema 6 hashes any
machine ID starting with `cursor-account:` or `machine:`, while schema 5 and the
change stream keep the original IDs. If your reports mix schemas 5 and 6, those
machines have different IDs and can be counted twice. Use the same schema
version across reporters.

Schema 7 reports Cursor billing events in each hour's `cursor_events` list and
account poll times in `cursor_polls`. The `key` fingerprints stored event
fields. Events also carry the model, token counts, cost in microdollars, account
ID, poll time, and `at`, the event time in UTC RFC3339Nano. When available,
`conversation_id` identifies the conversation that generated the event. A poll
is listed when it is newer than every event for that account and hour. The
newest poll is the maximum of the event poll times and listed polls, including
completed empty polls. Both lists are sorted by key and conversation ID or by
account and omitted when empty. Scoped exports omit them. Cursor events remain
in the whole-hour total but are omitted from machine parts. Schema 7 has no
`cursor-account:*` parts, so consumers reconcile the ledger before summing.
Other standalone usage stays in the archive part.

To combine schema 7 reports for an hour, union events by `(key, conversation_id)`.
When any event with a given key has a conversation ID, drop copies with that key
and an empty ID. Otherwise union the empty-ID events by key. Choose the event
copy with the newest `polled_at`, breaking ties by the lowest source device ID.
Credit its account only when its stamp matches the newest poll for that account
across all sources. Otherwise count it under the archive of the lowest-ID device
holding the event. Consumers must implement this reconciliation to count shared
events once. Schemas 5 and 6 lack the event identities needed for it. Use one
schema version across reporters. Schema 7 uses the same machine ID hashing as
schema 6.

Events without a conversation ID and older rows that have not been fetched
again retain fingerprint-only ambiguity. Each archive stores one row per
fingerprint, so two events with the same fingerprint and different conversation
IDs in one archive still count as one. A later fetch fills a missing conversation
ID without replacing one already stored.

Your downstream integration can pick one snapshot per originating machine and
UTC hour before summing totals. Dropping an entire overlapping archive would
also drop activity from machines that have no other reporter. Exports run
without `--by-machine` keep their existing output unchanged.

## Discover the reporting range

Before choosing dates for a historical import, run `agentsview export range`. It
reads your local SQLite archive directly and writes one canonical JSON document:

```json
{
  "schema_version": 1,
  "earliest_date": "2026-06-01",
  "closed_through": "2026-07-29T14:00:00Z"
}
```

`earliest_date` gives you a conservative UTC starting date for dated activity or
usage in closed hours. It covers subagents and forks, usage-only sessions,
standalone Cursor usage, message and usage timestamps older than session
metadata, terminal tool executions, and session start or creation fallbacks for
untimed activity. Deleted sessions, ineligible usage, invalid timestamps, and
timestamps in open or future hours do not set this bound. Because discovery
skips full activity aggregation and usage deduplication, `earliest_date` can
fall earlier than the first nonempty report.

`closed_through` is the exclusive UTC hour cutoff. In the example above, hours
before 14:00 are closed and the 14:00 hour is still open. If an archive has no
dated reporting evidence before that cutoff, including an empty archive, the
command returns `"earliest_date": null` alongside the cutoff.

Range schema 1 is independent of the hour, day, and digest schemas. The command
exports no projects, session lists, or conversation text, and accepts no report
scope or schema options. It does not migrate the archive. If an archive needs an
upgrade, open it first with the matching writable version.

Treat these bounds as availability hints, not a completeness guarantee or an
import checkpoint. Late imports, corrections, and deletions can still alter
older reports. Keep using date and hour digests to catch those changes,
splitting digest reads into ranges of at most 31 dates.

## Hour and day documents

A version 3 hour document has this structure:

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

Version 3 always includes twelve consecutive five-minute activity buckets, while
version 5 declares the bucket duration explicitly and allows other resolutions.
Activity splits sessions into interactive sessions, subagents, and automated
sessions. Subagents take precedence over automation, so an automated child
belongs only to the subagent category. Totals and every model, agent, and
project breakdown report separate minutes and costs for each category, while
combined minutes, cost, tokens, and peak concurrency include all three.

`interactive_peak`, `subagent_peak`, and `automated_peak` record each category's
independent maximum and its earliest timestamp. Each bucket also reports
`max_interactive_agents`, `max_subagent_agents`, and `max_automated_agents`,
which can happen at different times. By contrast, `interactive_at_peak`,
`subagent_at_peak`, and `automated_at_peak` show the breakdown at that bucket's
combined peak.

Activity totals also report additive first-seen counts for sessions, interactive
sessions, subagent sessions, automated sessions, untimed sessions, projects, and
models. `new_interactive_sessions + new_subagent_sessions +
new_automated_sessions` equals `new_sessions`, while untimed sessions are
counted separately and can belong to any category. A session's first-seen hour
is the start of its earliest effective activity interval after applying the
five-minute gap cap and export-range clipping. When a session has no such
interval, AgentsView falls back to an in-range session usage event, an activity
event, `started_at`, or `created_at`, in that order. Sessions placed by fallback
are counted as untimed.

In every activity total and every model, agent, or project breakdown,
`agent_minutes` is serialized as the exact floating-point sum of
`automated_agent_minutes + interactive_agent_minutes + subagent_agent_minutes`.
Before writing the output, the exporter verifies that the original total and all
three components are finite and non-negative. Given
`derived = automated + interactive + subagent`, the accepted difference is:

```text
abs(original - derived) <= 1e-9 * max(1, abs(original), abs(derived))
```

This boundary is inclusive, and any larger difference is rejected.

Usage totals include input, output, cache-creation, and cache-read tokens along
with cost. In one read transaction, reporting gathers activity-eligible
sessions, usage-eligible sessions, and standalone observations independently,
then merges session-linked and standalone rows before ordering, deduplication,
cost allocation, and aggregation. That complete usage stream drives usage totals
and all `by_model`, `by_agent`, and `by_project` breakdowns.

Usage-only sessions load only the metadata needed for agent and project
attribution and do not become activity sessions. Because standalone observations
have no session or project, they count toward usage totals, `by_model`, and
`by_agent`, and never get fabricated `by_project` attribution. Only rows tied to
activity-eligible sessions contribute to activity minutes, buckets, peaks,
breakdowns, first-seen state, or an activity `new_*` counter. For that reason,
activity token and cost totals can differ from usage totals. All money values
use integer microdollars, and project breakdowns carry both a safe display label
and a stable archive-scoped `project_key`.

A day document contains its UTC `date`, `complete` and `has_data` flags, its
ordered closed-hour documents, and a `digest` only when all 24 hours are
present. `agentsview export hour H` uses the same day reader and outputs
byte-for-byte the same canonical hour element you get inside `export day D`.

## Joint bucket cells (version 5)

Version 5 keeps version 3's accounting rules and adds a `joint` object to every
hour. Separate project, model, and agent breakdowns cannot answer a combined
query like "model A on project B". Joint cells preserve those combinations
without exporting session IDs, titles, messages, or tool content.

```sh
agentsview export day --schema-version 5 2026-07-28
agentsview export day --schema-version 5 --bucket 1m 2026-07-28
agentsview export hour --schema-version 5 --project-key <project-key> 2026-07-28-13
agentsview export digest --schema-version 5 --project-key <project-key> \
  --from 2026-07-01 --to 2026-07-28
```

To filter by more than one project, repeat `--project-key`. Take those keys from
the project breakdowns or `joint.projects` of an unscoped export. A session
export's project map uses label keys, which do not select these groups. Passing
no keys exports the whole archive, including unattributed usage. Passing an
explicit key selects only that project, and an unknown key produces an empty
replacement rather than an error. An empty key is invalid, and version 3 rejects
project selection.

Use `--bucket` to set the duration for both activity buckets and joint cells.
You can pass any positive whole-minute duration that divides one hour evenly,
such as `1m`, `2m`, `5m`, `15m`, or `1h`. Five minutes is the default rather
than a fixed contract limit. Version 3 rejects explicit bucket selection and
keeps its original bytes, and any invalid duration fails before the command
opens the archive.

Every version 5 hour, day, and digest document includes `bucket_seconds`, even
when empty, and every hour in a day uses that same duration. Bucket starts align
to the UTC hour. Activity always emits `3600 / bucket_seconds` buckets, while
joint cells stay sparse. The five-minute inactivity gap cap is independent of
bucket size, and one-minute exports still follow the same activity and
accounting rules.

Your selected scope applies to the **whole hour**, including existing totals,
breakdowns, and per-device bucket maxima. Before applying scope, the exporter
picks canonical usage survivors and allocates authoritative costs. A duplicate
observation dropped during that step does not turn into a new charge even when
its winning observation belongs to another project. Scoping filters export
output rather than enforcing access control; callers still own permission and
destination rules.

`joint.project_keys` lists the sorted, unique requested keys, or `[]` for the
whole archive. `joint.cells` is a sparse array with these fields:

| Field                         | Meaning                                                                                         |
| ----------------------------- | ----------------------------------------------------------------------------------------------- |
| `bucket_start`                | UTC start of a half-open bucket lasting `bucket_seconds`                                        |
| `project`, `project_key`      | Safe display label and canonical archive-scoped key; an empty key is unattributed               |
| `agent`, `model`              | Producer agent and model; `unknown` when absent                                                 |
| `automation`                  | `interactive`, `subagent`, `automated`, or `unknown` without session classification             |
| `agent_minutes`, `max_agents` | Sum of inferred activity durations and simultaneous peak within this cell                       |
| `usage`                       | Input, output, cache-creation and cache-read tokens, plus cost in integer microdollars          |
| `pricing`                     | `computed_cost`, `reported_cost`, `allocated_cost` in integer microdollars, and `unpriced_rows` |

### Project identity evidence

For Codex-format sessions, sync keeps the repository URL recorded in session
metadata. That URL can identify a session even after its checkout is removed.
After you upgrade to data version 138, the next sync reparses readable Codex,
TraeX, and Augure Code sources to recover URLs that earlier versions omitted. A
recovered URL replaces a stored identity that had no remote or was ambiguous,
without overriding a remote that sync already found in the checkout. Sessions
whose sources are unavailable keep their stored evidence. Because the archive
stores the recovered URL separately from any preserved checkout identity, later
appends reuse it after a restart. Full archive rebuilds keep recovered URLs too.
Repaired sessions clear obsolete ambiguity from the project catalog while
leaving ambiguity backed by another session in place.

`joint.projects` maps each nonempty cell project key to a safe `display_label`,
a `resolution`, and an `identity` when resolved. The exporter reads it in the
same SQLite transaction as activity and usage, and includes it in the hour
digest. It never contains session IDs or raw filesystem paths. Empty hours carry
an empty catalog, and standalone usage has no project identity.

A display label is **not a repository**, and different repositories can share
the same label. Each cell key pairs a label with its contributors' saved
evidence, as explained in [Project contributions](#project-contributions). A
`resolved` entry means every session contributing activity or canonical usage
under that key in the hour has a matching source-label snapshot and the same
resolved project identity. Missing snapshots or mismatched labels yield
`unknown`, and ambiguous snapshots yield `ambiguous`, with no `identity` in
either case. A known repository never lends its identity to another session that
lacks evidence. Resolution applies to the whole project key for that hour, even
across different models or buckets.

Identities use the existing project identity fields: `key`, `kind`,
`repository_key`, and, for `git_remote`, a credential-free `normalized_remote`.
Machine-root identities keep opaque scoped keys and do not prove that a
consumer's configured filesystem path matches. The exporter includes `root_key`
only when every contributing session agrees on that root. Different worktrees of
the same repository share the same identity and `repository_key`, with no
`root_key`. Consumers handle their own permission checks by matching verified
identity evidence against policy, never by matching a display label. Do not
authorize detailed cells when a catalog is missing, an entry is unresolved, or
an identity cannot establish the requested permission.

Identity-only changes update the hour and day digests, so normal correction
screening picks them up. During a day export, the exporter loads session
snapshots once in bounded batches and builds each hour's catalog from its actual
contributors, without querying the archive per hour or inspecting live
checkouts.

### Project contributions

Before building project breakdowns and joint cells, version 5 splits activity
and usage by each contributing session's saved identity. That separation keeps
repositories with the same project name distinct and lets you select only
activity with a verified identity. `new_projects` still counts project names
rather than groups, and sessions without a project stay out of `by_project`.

For example, two sessions named `api` can produce two entries: ten minutes tied
to a verified repository and five minutes with missing evidence. If you select
the verified entry, you get only its ten minutes and its usage. The unknown
entry neither hides those minutes nor inherits that repository.

Each project key combines the archive-scoped label key, the resolution, and,
when resolved, the identity key. Treat the full key as an opaque selector.
Different resolved repositories, as well as unknown or ambiguous contributions,
get separate keys even when their display labels match. A missing snapshot, or
one whose label no longer matches the session, stays `unknown`. Sessions with
the same label and identity share a group, while differently named checkouts
keep separate labels that consumers can group by verified repository identity.

`activity.by_project`, `usage.by_project`, and `joint` all use the same keys,
and `--project-key` filters across all three along with totals and concurrency.
Usage deduplication and cost allocation still run before selection. Display
labels stay labels and never establish identity, and standalone usage remains
unattributed. Exports never include session IDs or raw checkout paths, never
inspect live checkouts, and never modify the archive.

Version 5 keys come from the archive's saved evidence. When a sync, resync, or
upgrade changes a session's saved identity, its contributions move to a new key,
and a saved selector for the old key matches less data or none at all. When that
happens, read the current keys from `joint.projects` and refresh your saved
selectors and digests.

### Cell measurements

The `automation` field uses the same disjoint categories as activity totals,
labeling subagents as `subagent` even when their automation flag is set. Because
activity and usage share this classification, subagents stay separate from other
sessions that share the same project, agent, model, and bucket. Standalone usage
keeps `unknown`.

Known cost is split across the three pricing fields, and their sum equals the
cell's usage cost. `allocated_cost` represents an authoritative total
apportioned by the existing accounting rules rather than separately measured
per-message spend. `unpriced_rows` counts canonical usage observations that lack
complete pricing. Known fees still add to cost when token prices are unknown;
that cost is incomplete, not free usage. Activity-only cells report zero usage,
while usage without an activity interval adds tokens and cost without inventing
minutes. Usage lands in the bucket of its observation timestamp instead of
spreading across an activity interval.

Activity follows the same gap cap, model attribution, clipping, and overlap
removal as the Activity report. Agent-minutes are not measured human working
time. A report boundary inside a cell has only the declared bucket precision. Do
not prorate that cell and claim an exact instant-level result.

### Concurrency and corrections

Bucket resolution is part of an hour's content identity rather than a new
publication period. Changing `--bucket` replaces the entire cell set and all
activity buckets for that hour and scope, including quiet hours. Remove the old
resolution instead of adding both sets together, and screen digests using the
same `--bucket` value as your exports. Finer buckets do not require more
frequent uploads. You can roll buckets up into aligned multiples of the stored
duration, but you cannot recover finer detail from coarse buckets or invent
missing precision. To combine different resolutions, align them to a common
coarser grain.

A model switch can produce two cells for one session in the same bucket. Summing
their maxima can overstate even a single device's peak. For selected cells in a
bucket, compute an upper bound across devices like this:

```text
sum over devices of min(device bucket max_agents,
                       sum of that device's selected cell max_agents)
```

For a window-level bound, take the maximum bucket bound. Use
`activity.buckets[].max_agents` from the same permitted export scope as the
companion device maximum. This gives an upper bound, not exact selected
concurrency or exact cross-device concurrency.

Cells sort by bucket, project key, agent, model, and automation. Their full
contents and the requested project scope both enter the hour digest, including
for quiet hours. When an hour changes, replace its entire previous cell set in
the same scope. Removed cells count as retractions, and an empty cell set
retracts all previous cells. Never append a replacement as extra usage or
combine overlapping export scopes as independent sources.

Closing an hour does not freeze its contents. Old parser fixes, project changes,
pricing changes, and deletions can still alter a closed hour. When an hour's
digest changes, re-export and replace it. Version 5 adds no history checkpoint
or deletion journal. Digest screening still computes full day exports before
returning digests, and a single-hour export still reads its full day. Joint
export adds aggregation and output proportional to the populated cells rather
than acting as a source-side incremental optimization.

## Quiet hours

A quiet hour means your archive recorded no activity or usage observations for
that period. It deliberately does not mean an agent was observed idle for 60
minutes. A quiet hour therefore contains:

- `has_data: false`
- `idle_minutes: 0`
- Zero-valued activity buckets covering the full hour, twelve at the default
  five-minute resolution
- Empty activity and usage breakdown arrays
- Zero first-seen counters

A completed date with 24 quiet hours still produces a stable, non-empty day
digest. Use `has_data` to distinguish whether content is present from the
document's identity.

## Snapshot and digest guarantees

The exporter builds each day inside one SQLite read transaction, so sessions,
messages, usage rows, pricing, and project identity reflect a single coherent
snapshot even while a sync writes concurrently. Usage deduplication and
authoritative session-cost allocation run once across the day's merged usage
stream before rows split into hours. All dates in one `digest` call share that
read transaction, while survivor selection and session-cost allocation still run
independently for each date.

Before generic deduplication, usage picks the snapshot with the most output
tokens for each Claude message or request identity, keeps the earliest session
for attribution, and carries the highest observed web-search count into pricing.
Usage rows sort by occurrence time, then session ID ascending, then
`COALESCE(message_ordinal, -1)` ascending. A standalone row keeps its empty
session ID, so it sorts ahead of a session-linked row at the same timestamp.
Source and the remaining semantic fields act as deterministic tie-breakers only
after that shared ordering prefix.

Arrays follow stable contract ordering. Canonical JSON keeps declared JSON field
names and encodes money exactly. Reporting exports use a project-specific
canonical format and do not claim RFC 8785 or JSON Canonicalization Scheme
compliance. The byte rules are:

- Go JSON field names, `omitempty`, and custom marshalers apply first.
- Object keys sort lexicographically by UTF-16 code units.
- Array order stays as emitted.
- Output contains no insignificant whitespace.
- Strings use Go JSON escaping with HTML escaping disabled: `<`, `>`, and `&`
  stay literal, defined short control escapes such as `\b` and `\f` are used,
  other control characters use `\u00xx`, and U+2028 and U+2029 stay literal.
- Integers use minimal base-10 form and keep their full value, including values
  above `2^53`.
- Negative zero encodes as `0`.
- Finite floating-point values use the shortest round-trippable decimal form,
  with plain notation from magnitude `1e-6` through values below `1e21` and
  normalized exponent notation outside that range.
- Non-finite numbers are rejected.

The exporter computes digests like this:

1. An hour digest is SHA-256 over the canonical hour document with the derived
   `digest` field omitted.
1. A completed-day digest is SHA-256 over the canonical ordered array of its 24
   hour digest strings.
1. An incomplete current date includes ordered hour digests and no day digest.

`export digest` returns only those digests and presence flags:

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

You can screen completed dates by `day_digest`, then compare the 24 ordered
`hour_digests` and fetch only the hours that changed.

### Canonical vectors

Each vector below shows the complete canonical UTF-8 text with no trailing
newline. SHA-256 is computed over those exact bytes.

```text
Input:     {"\ue000":"bmp","\ud800\udc00":"astral"}
Canonical: {"𐀀":"astral","":"bmp"}
SHA-256:   5e72745dd500f8b8d997ef851679707b89099da29d2aca4b93dfd85810ebaa20

Input:     {"text":"<>&\b\f\u2028\u2029"}
Canonical: {"text":"<>&\b\f  "}
SHA-256:   e5de86e813b24e83a074160c5e30141ab85fb400e09f0e10a1594ce2cfedeccf

Input:     {"n":9007199254740993}
Canonical: {"n":9007199254740993}
SHA-256:   4ac8309cc76123ef6c5325ef925fc873e9b5856ec4f844ef1462f9303960378a

Input:     {"small":0.0000001,"plain":0.000001,"negative_zero":-0.0,"large":1e21}
Canonical: {"large":1e+21,"negative_zero":0,"plain":0.000001,"small":1e-7}
SHA-256:   940f129aabf5afc6800add24fbf597727e9dc6316f6ae10adbc78a3362b1c483
```

## Versioning

Version 3 separates subagents from interactive and automated sessions and adds
independent concurrency peaks for each category. It also keeps the complete
Claude snapshot selection and web-search charging introduced in version 2.
Versions 1 and 2 are no longer emitted.

Version 5 adds scoped joint cells and selectable bucket durations, and splits
project contributions by saved repository evidence. Version 3 remains the
default.

Version 5 replaces version 4, which is no longer emitted. Version 4 grouped
every contribution under one project label, so a session with missing or
different evidence hid a known repository's activity. Version 4 label keys do
not select version 5 groups. If you migrate an integration from version 4, fetch
new project keys and refresh your saved digests.

Have your integration request and require its expected `schema_version`, reject
unknown fields, and verify the canonical content digest before accepting an
hour. New fields change hour and day digests, including for quiet hours. Refresh
previously saved digests when you update. Adding, renaming, or removing a field,
changing a type or accounting rule, or changing canonicalization all require a
new schema version.

## Content boundary

Hour, day, digest, and range exports contain counts, durations, token totals,
costs, and model, agent, and project labels. Because they include no message
text, thinking text, or tool input and output, they have no secret masking and
no `--reveal` flag. Exports that return message text, such as
[conversation export](conversation-export.md#secrets-are-masked-by-default),
mask detected secrets by default.

## Local SQLite scope

Reporting export uses the read-only local SQLite export path by design. You can
run it while the writable daemon owns the archive, or with no daemon running at
all. It does not switch to PostgreSQL or ClickHouse. The local archive is the
canonical source for the device's reporting payload, deduplication order,
pricing view, and archive-scoped project keys.

Costs use reported charges when available and catalog estimates otherwise.
Estimates come from the pricing catalog visible at export time, not the catalog
revision from when an hour closed. A later pricing update can therefore change
an hour's estimated cost and digest without changing token counts. Treat that as
an ordinary correction and replace the saved hour.

If the archive has no stored model-pricing rows, the read-only reporting command
applies the embedded fallback catalog and configured custom prices in memory. It
never writes to the archive or replaces a non-empty stored catalog.

For the same UTC date and filters, the sum of a completed export day's usage
fields reconciles with the **Usage** view. Usage-session selection follows usage
timestamps independently of the session activity window.

Checked-in fixtures under `cmd/agentsview/testdata/reporting/` freeze version 3
bytes and their SHA-256 manifest for portable contract tests.
