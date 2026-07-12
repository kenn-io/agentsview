---
title: Session Export
description: Content-free session summary exports for scripting and analytics
---

`agentsview export sessions` emits versioned, content-free session summaries
from the local archive. It is intended for scripts, BI pipelines, and archive
handoffs that need session metadata, usage totals, pricing provenance, and
project identity without transcript text.

## Commands

```bash
agentsview export sessions --format json
agentsview export sessions --format ndjson
```

The command reads the local SQLite archive directly. It does not require a
running daemon and does not stream source transcript files.

## Output Shapes

JSON output is one document:

```json
{
  "schema_version": 1,
  "database_id": "00000000-0000-4000-8000-000000000001",
  "cursor": {
    "next": "opaque-cursor-or-empty"
  },
  "pricing": {
    "source": "fetched",
    "table_version": "2026-07-03T12:00:00Z",
    "latest_row_updated_at": "2026-07-03T12:00:00Z",
    "custom_override_count": 0,
    "effective_row_count": 2428,
    "digest": "sha256:8d815a1737bce68fa1a19ba977bf33c8c8efcc74deb954fcf62ce80e46e75f2c",
    "cost_source": "mixed",
    "fallback": {
      "used": false,
      "models": []
    },
    "models": {
      "fixture-model-computed": {
        "matched_pattern": "fixture-model-computed",
        "input_cost_per_mtok": 2,
        "output_cost_per_mtok": 8,
        "cache_write_cost_per_mtok": 3,
        "cache_read_cost_per_mtok": 0.5,
        "cost_source": "computed"
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
        "cost_usd": 0.00476,
        "has_cost": true,
        "by_model": {
          "fixture-model-computed": {
            "model": "fixture-model-computed",
            "input_tokens": 1200,
            "output_tokens": 240,
            "cache_creation_input_tokens": 80,
            "cache_read_input_tokens": 400,
            "reasoning_tokens": 0,
            "cost_usd": 0.00476,
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

NDJSON output writes a metadata object as the first line, then writes session
rows on subsequent lines. The metadata line has `"type": "meta"`; session rows
do not add a `type` discriminator.

```json
{"type":"meta","schema_version":1,"database_id":"00000000-0000-4000-8000-000000000001","cursor":{"next":"..."},"pricing":{},"projects":{}}
{"id":"path-current","project":{"project_key":"pl1:sha256:...","display_label":"path-project","resolution":"resolved","identity":{"key":"p1:sha256:...","kind":"machine_root","root_key":"r1:sha256:...","repository_key":"repo1:sha256:..."},"worktree":{"relationship":"main_worktree","worktree_key":"wt1:sha256:...","repository_key":"repo1:sha256:..."},"checkout":{"state":"unknown"}},"agent":"claude","started_at":"2026-07-03T11:00:00Z","ended_at":"2026-07-03T11:10:00Z","last_activity_at":"2026-07-03T11:10:00Z","duration_seconds":600,"message_count":4,"user_message_count":2,"assistant_message_count":2,"turn_count":2,"classification":"interactive","is_automated":false,"model_usage":{"models":["fixture-model-reported"],"input_tokens":300,"output_tokens":60,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"reasoning_tokens":0,"cost_usd":0.0125,"has_cost":true,"by_model":{"fixture-model-reported":{"model":"fixture-model-reported","input_tokens":300,"output_tokens":60,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"reasoning_tokens":0,"cost_usd":0.0125,"has_cost":true,"cost_source":"reported"}}},"parent_session_id":null,"relationship_type":"root","total_output_tokens":0,"peak_context_tokens":0,"has_total_output_tokens":false,"has_peak_context_tokens":false}
```

These snippets are abbreviated illustrations. Complete checked JSON and NDJSON
outputs live in `testdata/golden/session_export_v1.json` and
`testdata/golden/session_export_v1.ndjson`.

## Content Boundary

Session summary export does not include message content. V1 intentionally omits
transcript text, first-message text, thinking text, tool input, tool output, and
raw transcript bytes. It may include metadata that came from sessions, such as
sanitized project labels, branch names, model names, token totals, and cost
totals. It does not emit raw working-directory paths.

## Project Evidence

Each session row carries the project identity snapshot captured for that
session. `resolution` is `resolved`, `unknown`, or `ambiguous`; `identity`
appears only for resolved projects. `display_label` is presentation metadata and
is empty when no safe label is available. It is never used as an identity
fallback.

The nested `worktree.relationship` value is `main_worktree`, `linked_worktree`,
`not_a_worktree`, or `unknown`. `checkout.state` is `branch`, `detached`, or
`unknown`. A detached checkout can still belong to a main or linked worktree, so
consumers should treat checkout state and worktree relationship as independent
facts.

Remote-backed identities remain stable across checkout directory renames,
worktrees, branches, and machines. Machine-root identities are archive-scoped
and intentionally do not provide cross-machine continuity. Historical sessions
without captured evidence remain unknown rather than inferring identity from a
directory basename.

The report-level `projects` catalog uses the same identities, but remote-backed
catalog entries omit the session-specific `root_key`. See
[Project Identity](/token-usage/#project-identity) for key prefixes,
normalization, shared-store scope, and privacy guarantees.

## Filters And Limits

`--limit` defaults to `db.MaxSessionLimit`, currently 500. The maximum accepted
value is also `db.MaxSessionLimit`, currently 500.

By default, export returns non-deleted root sessions, excludes one-shot
sessions, and excludes automated sessions. Use `--include-one-shot`,
`--include-automated`, and `--include-children` to widen that default. Default
exclusions can undercount token and cost totals when excluded sessions carry
usage.

When `--all --format json` is used, the command emits one JSON document with all
eligible sessions and an empty `cursor.next`. When `--all --format ndjson` is
used, the command emits one metadata line with an empty `cursor.next`, then all
eligible session rows. It does not concatenate multiple JSON documents.

## Cursor Contract

Pagination uses keyset cursors over a stable watermark. The first page mints a
watermark equal to the maximum eligible `last_activity_at` at cursor creation.
Later pages include only sessions whose `last_activity_at` is at or below that
watermark. The order is `last_activity_at DESC, id ASC`.

Sessions inserted after the first page with activity newer than the watermark
are excluded until a fresh export starts. The cursor also records a digest of
the full watermarked set and the already-emitted keyset prefix. If a later
request sees that rows have moved into or out of either digest, including an
unemitted row whose activity moved above the watermark, the command returns the
cursor-reset error so the caller can restart from a fresh first page. Rows
inserted mid-pagination under the watermark reset the cursor rather than being
quietly mixed into the in-flight run.

Every page reads its session rows, usage, pricing, project snapshots, worktree
observations, and archive metadata from one SQLite read transaction. Cursor
fingerprints cover membership and order across separate manual requests, not
every evidence field for that multi-request run. An evidence-only update between
manual requests does not force a reset; each returned page is nevertheless
internally snapshot-consistent. `--all` holds one read transaction across every
internal page, so the combined artifact has one pricing, identity, and usage
snapshot.

Because `--all` is explicitly unbounded, that read snapshot can delay WAL
checkpoint reclamation while concurrent sync writes continue. Cancellation
aborts the active queries and rolls back the transaction. Use ordinary bounded
pages for latency-sensitive polling; reserve `--all` for complete offline
artifacts.

The cursor embeds the archive `database_id`, filter parameters, ordering,
watermark, last keyset position, snapshot digest, prefix digest, and limit.
Passing `--cursor` with any query-affecting flag is an error. Only `--format`,
`--json`, and `--limit` may be used with `--cursor`; filters such as
`--project`, `--exclude-project`, `--machine`, `--git-branch`, `--agent`, date
filters, `--active-since`, message-count filters, `--include-one-shot`,
`--include-automated`, `--include-children`, `--outcome`, `--health-grade`,
`--min-tool-failures`, `--has-secret`, and `--all` may not.

If a cursor belongs to another archive or otherwise requires reset, stdout is
empty, stderr contains one JSON object, and the process exits with code 4:

```json
{"error":"cursor_reset","message":"session export cursor is no longer valid; restart the export","database_id":"00000000-0000-4000-8000-000000000001"}
```

Other usage and validation errors use the normal CLI error path and exit code 1.

## Daemonless Schema Eligibility

The command normally keeps the archive read-only. If read-only open reports a
typed missing-schema error for `archive_metadata`,
`project_identity_observations`, or `session_project_identity_snapshots`, the
command may create a missing one of those tables, or add an explicitly supported
column to an existing observation table, in one transaction and then reopen
read-only. Existing metadata or snapshot tables with missing columns are not
eligible. It does not initialize unrelated tables, run data repairs, or
reclassify sessions. Other missing core schema still requires the normal
writable daemon migration or full resync.

When an older archive lacks per-session identity snapshots, the writable daemon
reconstructs them in a resumable background migration. This reconstruction is
best-effort: it combines stored session metadata with the repository and Git
configuration visible when the migration runs, so a repository that moved or
changed remotes may not reproduce its original checkout context. Existing stored
snapshots remain authoritative. `agentsview export status` reports pending,
running, failed, and completed state; failed status includes the last recorded
error.

## Versioning

`agentsview export sessions --format json|ndjson` is its own versioned surface.
This unshipped v1 shape is canonical and has no pre-v1 compatibility adapter.
Additive fields do not require a bump, but row semantic changes, field type
changes, sort order changes, cursor semantics changes, required-field meaning
changes, field removal, pricing digest canonicalization changes, project key
derivation changes, remote normalization changes, path fallback normalization
changes, and new closed-enum values require a bump.

Consumers should require the expected `schema_version` and ignore unknown
additive fields.

Closed v1 enums in this surface include project `resolution` (`resolved`,
`unknown`, `ambiguous`), session `classification` (`interactive`, `automated`),
and `cost_source` (`computed`, `reported`, `mixed`).

See [Token Usage & Costs](/token-usage/#pricing-provenance) for the shared
pricing provenance contract and
[Token Usage & Costs](/token-usage/#project-identity) for the shared project
identity contract.
