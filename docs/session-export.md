---
title: Session Export
description: Content-free session summary exports for scripting and analytics
---

Run `agentsview export sessions` when your reporting scripts need session
metadata, token and cost totals, pricing, and project identity without
transcript text. You can export one JSON document or NDJSON with one JSON object
per line.

Artifact export and import exclude labels and launcher parent links, which
belong to the local archive. Transcript parents and spawn edges stay part of the
artifact.

## Commands

```bash
agentsview export sessions --format json
agentsview export sessions --format ndjson
```

The command reads your local SQLite archive directly. You do not need a running
daemon, and the command does not stream source transcript files.

## Output shapes

With `--format json`, the command writes one document:

```json
{
  "schema_version": 7,
  "archive_id": "00000000-0000-4000-8000-000000000002",
  "database_id": "00000000-0000-4000-8000-000000000001",
  "cursor": {
    "next": "opaque-cursor-or-empty"
  },
  "pricing": {
    "source": "fetched",
    "table_version": "2026-07-03T12:00:00Z",
    "latest_row_updated_at": "2026-07-03T12:00:00Z",
    "custom_override_count": 0,
    "effective_row_count": 2432,
    "digest": "sha256:8d815a1737bce68fa1a19ba977bf33c8c8efcc74deb954fcf62ce80e46e75f2c",
    "cost_source": "mixed",
    "fallback": {
      "used": false,
      "models": []
    },
    "models": {
      "fixture-model-computed": {
        "cost_source": "computed",
        "resolutions": [
          {
            "priced_model": "fixture-model-computed",
            "matched_pattern": "fixture-model-computed",
            "input_cost_per_mtok": {"microdollars": 2000000},
            "output_cost_per_mtok": {"microdollars": 8000000},
            "cache_write_cost_per_mtok": {"microdollars": 3000000},
            "cache_write_1h_cost_per_mtok": {"microdollars": 0},
            "cache_read_cost_per_mtok": {"microdollars": 500000},
            "cost_source": "computed",
            "bands": null,
            "application": {
              "base_request_count": 1,
              "aggregate_row_count": 0,
              "bands": null
            }
          }
        ]
      }
    }
  },
  "projects": {
    "pl1:sha256:333e5f19bc8ed34f56fa89e51a9307bbc972d173498993ed02e564d32162196f": {
      "display_label": "remote-project",
      "resolution": "resolved",
      "identity": {
        "key": "p1:sha256:eb8c8bb90c27de41cdfb780f4c756cc4c3b9faf4f7c785c9f6afa7e160c2112c",
        "kind": "git_remote",
        "normalized_remote": "github.com/acme/remote-project",
        "repository_key": "repo1:sha256:8a7da005b67fa8300b6072fd3a38629dc4505097258f7fb4398bf4cfd670df10"
      }
    }
  },
  "sessions": [
    {
      "id": "remote-current",
      "transcript_revision": "1",
      "local_modified_at": "2026-07-03T12:00:00.123Z",
      "project": {
        "project_key": "pl1:sha256:333e5f19bc8ed34f56fa89e51a9307bbc972d173498993ed02e564d32162196f",
        "display_label": "remote-project",
        "resolution": "resolved",
        "identity": {
          "key": "p1:sha256:eb8c8bb90c27de41cdfb780f4c756cc4c3b9faf4f7c785c9f6afa7e160c2112c",
          "kind": "git_remote",
          "normalized_remote": "github.com/acme/remote-project",
          "root_key": "r1:sha256:a12c16b92869149223e0340660e2d6b9b86e1550ac7587002aaca79d423ef365",
          "repository_key": "repo1:sha256:8a7da005b67fa8300b6072fd3a38629dc4505097258f7fb4398bf4cfd670df10"
        },
        "worktree": {
          "relationship": "linked_worktree",
          "worktree_key": "wt1:sha256:ba30beee2646122e214e08a531f5869264a78b33b857eb6b17cd0b57d0be4785",
          "repository_key": "repo1:sha256:8a7da005b67fa8300b6072fd3a38629dc4505097258f7fb4398bf4cfd670df10"
        },
        "checkout": {
          "state": "branch",
          "branch": "feature/golden"
        }
      },
      "agent": "codex",
      "started_at": "2026-07-03T10:00:00Z",
      "ended_at": "2026-07-03T10:30:00Z",
      "last_activity_at": "2026-07-03T10:30:00Z",
      "duration_seconds": 1800,
      "message_count": 4,
      "user_message_count": 2,
      "assistant_message_count": 2,
      "turn_count": 2,
      "classification": "interactive",
      "is_automated": false,
      "model_usage": {
        "models": ["fixture-model-computed"],
        "input_tokens": 1200,
        "output_tokens": 240,
        "cache_creation_input_tokens": 80,
        "cache_read_input_tokens": 400,
        "reasoning_tokens": 0,
        "cost": {"microdollars": 4760},
        "has_cost": true,
        "by_model": {
          "fixture-model-computed": {
            "model": "fixture-model-computed",
            "input_tokens": 1200,
            "output_tokens": 240,
            "cache_creation_input_tokens": 80,
            "cache_read_input_tokens": 400,
            "reasoning_tokens": 0,
            "cost": {"microdollars": 4760},
            "has_cost": true,
            "cost_source": "computed"
          }
        }
      },
      "parent_session_id": null,
      "relationship_type": "root",
      "total_output_tokens": 240,
      "peak_context_tokens": 0,
      "has_total_output_tokens": true,
      "has_peak_context_tokens": false
    }
  ]
}
```

With `--format ndjson`, the first line is a metadata object with
`"type": "meta"`, and each later line is a session row without a `type` field.

```json
{"type":"meta","schema_version":7,"archive_id":"00000000-0000-4000-8000-000000000002","database_id":"00000000-0000-4000-8000-000000000001","cursor":{"next":"..."},"pricing":{},"projects":{}}
{"id":"path-current","agent":"claude","model_usage":{"models":["fixture-model-reported"],"input_tokens":300,"output_tokens":60,"cost":{"microdollars":12500},"has_cost":true}}
```

These examples are abbreviated. For complete checked outputs, see
`testdata/golden/session_export_v7.json` and
`testdata/golden/session_export_v7.ndjson`.

### Codex Cloud usage

A Codex Cloud session can have no token figures at all, so `model_usage` can be
`null`. Codex Cloud sessions can also include `plan_usage` and
`capture_coverage`:

- `model_usage` is `null` when AgentsView has no token counts for the chat,
  either when it never listened in while the chat ran or when it listened in but
  received no token update. Treat that usage as unknown rather than zero tokens
  or zero cost, and rely on OpenAI's plan usage report instead. Sessions from
  other agents that recorded no usage still export zeros.
- `plan_usage` contains OpenAI's newest plan usage report for the chat, the
  outcome of a later check, or both. See the
  [`plan_usage` fields](/docs/session-api/#agentsview-session-usage) for the
  full list of fields. An object with only an outcome, such as
  `account_unavailable`, has no figures. When AgentsView has stored neither, it
  omits `plan_usage`.
- `capture_coverage` reports `last_capture_at`, the time AgentsView last
  received a token update for the chat, and `uncaptured_turns`, the count of
  finished turns that live capture did not fully see. When AgentsView never
  listened in on the chat, it omits `capture_coverage`.

### Archive and database identity

Every export includes two identifiers from the same read snapshot as the
exported rows, even in empty results and on the NDJSON metadata line:

- `archive_id` identifies the logical archive and stays the same across a full
  resync. Together, `(archive_id, session.id)` identify a session across rebuilt
  database generations.
- `database_id` identifies the physical database generation and changes on a
  full resync. Pagination cursors stay tied to that generation.

If the archive identity is missing, the read-only export returns an error rather
than creating one.

If `database_id` changes while `archive_id` stays the same, the archive was
rebuilt rather than filled with brand-new sessions. Restart pagination and
reconcile the new snapshot. Neither ID acts as an ordered revision, and a cursor
is not a durable change-feed checkpoint. These IDs do not tell you which of two
delayed exports is newer, and they do not turn absence from a filtered page into
a deletion signal. Copied archives keep their logical identity. If your
application ingests independent copies, define your own source scope instead of
assuming `archive_id` identifies a single device or writer.

### Revision evidence

In both JSON and NDJSON, schema v6 adds two fields to each session row:

- `transcript_revision` is the archive's per-session counter, formatted as a
  decimal string to keep full integer precision. Compare it numerically rather
  than lexicographically. Within one archive's writer lineage, an unchanged
  transcript keeps its counter across a full resync, while a changed transcript
  increments the previous counter once. That comparison checks persisted
  messages, tool calls, tool-result events, and message token usage, without
  exporting any of that content.
- `local_modified_at` is the stored local modification timestamp, or `null` when
  it is unavailable. It is neither session activity time nor measured working
  time. Transcript writes and standalone usage-event writes can update it while
  `last_activity_at` stays the same. It is a wall-clock signal, and equal
  values, clock shifts, and rebuilds prevent treating it as a monotonic
  revision. Labels and launcher parent links update only the mirror sync marker.

When you store a materialized session receipt, keep a tuple of independent
version evidence: **transcript revision, project identity (or a digest of the
complete exported `project` reference), and pricing timestamp plus digest**.
Keep `local_modified_at` as well to track local metadata and standalone
usage-event changes. Project evidence can change without a transcript edit.
Pricing evidence stays at the page level, where `pricing.latest_row_updated_at`
is the latest timestamp among the effective pricing rows and `pricing.digest`
identifies their contents. Editing another pricing row might not raise that
maximum, and removing the newest row can lower it. Keep `pricing.digest` even
when timestamps match or are unavailable.

Do not treat this tuple as a lexicographically ordered receipt version. A larger
transcript counter orders transcript changes, not concurrent project or pricing
changes, and equal counters do not prove equal usage totals. Standalone
`usage_events` and session metadata sit outside transcript equality, and digests
check equality rather than chronology. Do not accept or reject a full receipt
from `transcript_revision` alone, and do not compare counters across
independently modified archive copies.

The command reads all of these fields with their session rows in the existing
read transaction, and `--all` uses one transaction across all pages. Ordinary
cursor pages still run in separate read transactions with the existing
activity-based cursor checks rather than a revision-pinned historical snapshot.
These fields do not change cursor semantics or add a modified-since filter,
durable checkpoint, or tombstones. A future incremental export must account for
every export-affecting writer, project and pricing changes, cutoff ties, and
journal resets before it can promise complete change discovery. `--active-since`
filters by activity time and does not catch every correction to older sessions.

## Content boundary

Session summary exports leave out transcript text, first-message text, thinking
text, tool input, tool output, and raw transcript bytes. They can include
session metadata such as sanitized project labels, branch names, model names,
token totals, and cost totals, and they never emit raw working-directory paths.
Because no message text leaves the archive, this command has no secret masking
and no `--reveal` flag. Exports that return message text, such as
[conversation export](conversation-export.md#secrets-are-masked-by-default),
mask detected secrets by default.

## Project evidence

Each session row includes the project identity snapshot captured for that
session. `resolution` is `resolved`, `unknown`, or `ambiguous`, and `identity`
appears only when `resolution` is `resolved`. `display_label` is presentation
metadata and stays empty when no safe label is available. AgentsView never falls
back to `display_label` as an identity.

The nested `worktree.relationship` field is `main_worktree`, `linked_worktree`,
`not_a_worktree`, or `unknown`, while `checkout.state` is `branch`, `detached`,
or `unknown`. A detached checkout can still belong to a main or linked worktree,
so treat checkout state and worktree relationship as independent facts.

Remote-backed identities stay stable across checkout directory renames,
worktrees, branches, and machines. Machine-root identities are archive-scoped
and intentionally do not provide cross-machine continuity. Historical sessions
with no captured evidence stay `unknown` instead of guessing an identity from a
directory basename.

The report-level `projects` catalog uses the same identities, except that
remote-backed catalog entries omit the session-specific `root_key`. For key
prefixes, normalization, shared-store scope, and privacy guarantees, see
[Project Identity](/docs/token-usage/#project-identity).

## Filters and limits

`--limit` defaults to `db.MaxSessionLimit`, currently 500, which is also the
maximum accepted value.

By default, the export returns non-deleted root sessions and leaves out one-shot
and automated sessions. To widen that set, pass any combination of
`--include-one-shot`, `--include-automated`, and `--include-children`. Default
exclusions can undercount token and cost totals when excluded sessions carry
usage.

With `--all --format json`, the command writes one JSON document containing all
eligible sessions and an empty `cursor.next`. With `--all --format ndjson`, it
writes one metadata line with an empty `cursor.next`, followed by all eligible
session rows. It never concatenates multiple JSON documents.

## Cursor contract

Pagination uses keyset cursors pinned to a stable watermark. On the first page,
the command sets the watermark to the highest eligible `last_activity_at` at the
time it creates the cursor. Later pages include only sessions with
`last_activity_at` at or below that watermark, ordered by
`last_activity_at DESC, id ASC`.

Any session inserted after the first page with activity newer than the watermark
stays out of the run until you start a fresh export. The cursor also stores a
digest of the full watermarked set and the keyset prefix you have already read.
If a later request sees that rows moved into or out of either digest, including
an unemitted row whose activity moved above the watermark, the command returns a
cursor-reset error so you can restart from a fresh first page. Rows inserted
under the watermark mid-pagination also trigger a reset instead of slipping into
the in-flight run.

Each page reads its session rows, usage, pricing, project snapshots, worktree
observations, and archive metadata inside a single SQLite read transaction.
Cursor fingerprints track membership and order across separate manual requests,
not every evidence field across the whole run. An evidence-only update between
manual requests does not trigger a reset, and each page you receive is still internally
snapshot-consistent. `--all` keeps one read transaction open across every
internal page, so the combined artifact shares one pricing, identity, and usage
snapshot.

Because `--all` is explicitly unbounded, holding that read snapshot can delay
WAL checkpoint reclamation while concurrent sync writes continue. Canceling the
command aborts active queries and rolls back the transaction. For
latency-sensitive polling, use ordinary bounded pages, and reserve `--all` for
complete offline artifacts.

Each cursor embeds the archive `database_id`, filter parameters, ordering,
watermark, last keyset position, snapshot digest, prefix digest, and limit.
Combining `--cursor` with any query-affecting flag is an error. You can pair
`--cursor` only with `--format`, `--json`, `--limit`, and `--wait`. You cannot
pair it with `--project`, `--exclude-project`, `--machine`, `--git-branch`,
`--agent`, date filters, `--active-since`, message-count filters,
`--include-one-shot`, `--include-automated`, `--include-children`, `--outcome`,
`--health-grade`, `--min-tool-failures`, `--has-secret`, or `--all`.

If a cursor belongs to another archive or requires a reset for any other reason,
stdout stays empty, stderr receives one JSON object, and the command exits with
code 4:

```json
{"error":"cursor_reset","message":"session export cursor is no longer valid; restart the export","database_id":"00000000-0000-4000-8000-000000000001"}
```

Other usage and validation errors follow the standard CLI error path and exit
with code 1.

## Daemonless schema eligibility

The command normally keeps the archive read-only. If opening read-only returns a
typed missing-schema error for `archive_metadata`, `background_migrations`,
`cursor_usage_events`, `cursor_account_polls`, `project_identity_observations`,
or `session_project_identity_snapshots`, the command can create those tables
when they are missing, or add an explicitly supported column to an existing
`project_identity_observations`, `session_project_identity_snapshots`, or
`cursor_usage_events` table, in one transaction before reopening read-only. An
existing table with any other missing column is not eligible. The command does
not initialize unrelated tables, run data repairs, or reclassify sessions. Any
other missing core schema still requires the normal writable daemon migration or
a full resync.

When an older archive lacks per-session identity snapshots, the writable daemon
reconstructs them in a resumable background migration. That reconstruction is
best-effort. It combines stored session metadata with the repository and Git
configuration visible when the migration runs, so a repository that moved or
changed remotes may not reproduce its original checkout context. Existing stored
snapshots remain authoritative. To check migration progress, run
`agentsview export status`, which reports pending, running, failed, and
completed states. When a migration fails, the status output includes its cause
and recovery commands, even if the daemon could not save the failure. To start
the local daemon if needed and wait for this work before exporting, run
`agentsview export sessions --wait`.

## Versioning

`agentsview export sessions --format json|ndjson` has its own schema version.
Current builds report version 7, and there is no flag to request an earlier
output version.

- **Version 1.** Shipped in 0.37.1. Releases 0.38.0 and 0.38.1 introduced the
  current privacy-bounded project, repository, worktree, and checkout evidence
  shape but mistakenly continued to report version 1. Do not treat those two
  transitional releases as v1-compatible.
- **Version 2.** Corrected the project-evidence marker.
- **Version 3.** Introduced exact microdollar money objects.
- **Version 4.** Added explicit reported-to-priced-model resolutions with
  complete request-pricing bands and application counts.
- **Version 5.** Selected complete Claude snapshots before generic deduplication
  across pagination and session filters, kept earliest-session attribution, and
  included the maximum observed web-search count in pricing.
- **Version 6.** Applied provider-specific billing identity to computed usage
  and preserved reported cost rows and custom pricing overrides. Do not compare
  v5 and v6 costs as the same billing semantics. Publishing `archive_id` is an
  additive v6 extension that leaves existing session IDs, database IDs, usage,
  pricing, ordering, and cursor semantics unchanged.
- **Version 7.** Allows `model_usage` to be `null` for a Codex Cloud session
  whose token counts are unknown. Update any v6 consumer that expects an object
  there.

Additive fields do not require a version bump. A bump is required for row
semantic changes, field type changes, sort order changes, cursor semantics
changes, required-field meaning changes, field removal, pricing digest
canonicalization changes, project key derivation changes, remote normalization
changes, path fallback normalization changes, and new closed-enum values.

Have your consumer require the `schema_version` it expects and ignore unknown
additive fields.

These closed enums were established in version 2 and remain unchanged in
version 7:

- Project `resolution`: `resolved`, `unknown`, `ambiguous`
- Session `classification`: `interactive`, `automated`
- `cost_source`: `computed`, `reported`, `mixed`

See [Token Usage & Costs](/docs/token-usage/#pricing-provenance) for the shared
pricing provenance contract and
[Token Usage & Costs](/docs/token-usage/#project-identity) for the shared
project identity contract.
