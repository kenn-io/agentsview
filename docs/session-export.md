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
    "table_version": "litellm-398a0b15378c",
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
    "remote-project": {
      "resolution": "resolved",
      "identity": {
        "key": "sha256:97879729c8ab311e9d4b28941e3a04830b28c527f00af53f2270212eccdbbd39",
        "key_source": "git_remote",
        "normalized_remote": "github.com/acme/remote-project"
      }
    }
  },
  "sessions": [
    {
      "id": "remote-current",
      "project": "remote-project",
      "machine": "golden-host",
      "agent": "codex",
      "cwd": "/fixtures/remote-project/worktrees/feature/app",
      "git_branch": "feature/golden",
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
      "worktree": {
        "name": "feature",
        "root_path": "/fixtures/remote-project"
      },
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
{"id":"path-current","project":"path-project","machine":"golden-host","agent":"claude","cwd":"/fixtures/path-project/pkg","started_at":"2026-07-03T11:00:00Z","ended_at":"2026-07-03T11:10:00Z","last_activity_at":"2026-07-03T11:10:00Z","duration_seconds":600,"message_count":4,"user_message_count":2,"assistant_message_count":2,"turn_count":2,"classification":"interactive","is_automated":false,"model_usage":{"models":["fixture-model-reported"],"input_tokens":300,"output_tokens":60,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"reasoning_tokens":0,"cost_usd":0.0125,"has_cost":true,"by_model":{"fixture-model-reported":{"model":"fixture-model-reported","input_tokens":300,"output_tokens":60,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"reasoning_tokens":0,"cost_usd":0.0125,"has_cost":true,"cost_source":"reported"}}},"parent_session_id":null,"relationship_type":"root","worktree":null,"total_output_tokens":0,"peak_context_tokens":0,"has_total_output_tokens":false,"has_peak_context_tokens":false}
```

## Content Boundary

Session summary export does not include message content. V1 intentionally omits
transcript text, first-message text, thinking text, tool input, tool output, and
raw transcript bytes. It may include metadata that came from sessions, such as
project labels, working directories, branch names, model names, token totals,
and cost totals.

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

## Versioning

`agentsview export sessions --format json|ndjson` is its own versioned surface.
Absent `schema_version` means legacy pre-v1 output. Additive fields do not
require a bump, but row semantic changes, field type changes, sort order
changes, cursor semantics changes, required-field meaning changes, field
removal, pricing digest canonicalization changes, project key derivation
changes, remote normalization changes, path fallback normalization changes, and
new closed-enum values require a bump.

This v1 JSON/NDJSON contract is intended to remain backward compatible, but it
may see some instability while the new export surface settles. Consumers should
pin `schema_version`, ignore unknown additive fields, and treat missing
`schema_version` as legacy output.

Closed v1 enums in this surface include project `resolution` (`resolved`,
`unknown`, `ambiguous`), session `classification` (`interactive`, `automated`),
and `cost_source` (`computed`, `reported`, `mixed`).

See [Token Usage & Costs](/token-usage/#pricing-provenance) for the shared
pricing provenance contract and
[Token Usage & Costs](/token-usage/#project-identity) for the shared project
identity contract.
