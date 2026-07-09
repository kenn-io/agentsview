---
title: Session Memories
description: Reviewed, provenance-linked facts distilled from your sessions and recalled by keyword query
---

AgentsView can store **memories** — reviewed facts, decisions, procedures, and
warnings distilled from your sessions — in the same local SQLite archive as the
sessions they came from. Each memory carries provenance back to the messages
that support it, so a memory is never a floating claim: it always traces to
specific evidence in a specific session. This is separate from
[semantic search](/semantic-search/), which indexes raw message content;
memories are curated, reviewed statements you (or a review step) decided are
worth keeping.

Today, getting memories into the archive is a review-then-import workflow: you
or a tool produce a JSONL file of reviewed candidates, and
`agentsview memory import` writes the ones that pass its trust gates. Once
imported, memories are recalled with keyword queries — `memory query` and
`memory brief` — the same way you'd search session content.

!!! note "SQLite only"

    Memories require the local SQLite archive. [PostgreSQL sync](/pg-sync/) and the
    [DuckDB mirror](/duckdb/) are read-only mirrors with no memory table; every
    `agentsview memory` command and every `/api/v1/memories` endpoint against a PG-
    or DuckDB-backed server returns a "not available" error.

## What a memory is

A memory is one row with:

- **A type and a scope** — see [Types and scopes](#types-and-scopes) below.
- **A title and body** — the reviewed statement itself.
- **Optional trigger, confidence, and uncertainty** — when the memory applies,
    how confident the source was, and any caveat text.
- **Context** — the project, working directory, git branch, and agent the memory
    was learned under, used to scope recall to relevant work.
- **Source fields** — `source_session_id` (required), plus optional
    `source_episode_id`, `source_run_id`, `extractor_method`, and `model`,
    recording where the memory came from.
- **Trust flags** — `transferable` and `provenance_ok` (see
    [Trust gates](#trust-gates-and-labels)).
- **Evidence** — one or more
    `(session_id, message_start_ordinal, message_end_ordinal)` ranges, each
    optionally naming a `tool_use_id` and carrying a snippet, that a reader can
    open to verify the claim against the original conversation.
- **Lifecycle fields** — `status` (`accepted` or `archived`),
    `supersedes_memory_id`, and `superseded_by_memory_id` (see
    [Supersede, don't delete](#supersede-dont-delete)).

## Supersede, don't delete

Memories are never overwritten or deleted in place. When new evidence updates or
replaces an existing memory, the replacement is inserted with
`supersedes_memory_id` set to the old memory's id; the old memory is archived
(`status = archived`) and gets `superseded_by_memory_id` set to point forward to
the new one. Both rows stay in the archive, so you can always see what a memory
used to say and why it changed.

`agentsview memory stats` and `memory query --summary` classify every memory
into one lifecycle bucket:

| Bucket                   | Meaning                                                     |
| ------------------------ | ----------------------------------------------------------- |
| `active`                 | Current, not superseding or superseded by anything          |
| `replacement`            | Supersedes an older memory                                  |
| `superseded`             | Archived or superseded by a newer memory                    |
| `replacement_superseded` | Itself a replacement, and has since been superseded in turn |

Default recall (`memory list`, `memory query`, `memory brief`) only returns
`accepted` memories unless you pass `--status` explicitly, so a superseded
memory does not show up alongside its replacement by default.

## Types and scopes

Seven memory types (`internal/memory/types.go`):

| Type               | Use for                                                           |
| ------------------ | ----------------------------------------------------------------- |
| `fact`             | A concrete, verifiable statement about the project or environment |
| `decision`         | A choice that was made and the reasoning behind it                |
| `procedure`        | A repeatable series of steps that worked                          |
| `debugging_method` | An approach that found or fixed a bug                             |
| `warning`          | A pitfall or failure mode to avoid                                |
| `preference`       | A stated preference about how work should be done                 |
| `open_question`    | Something left unresolved that a future session should revisit    |

Seven scopes, narrowest to broadest applicability:

| Scope        | Applies to                                        |
| ------------ | ------------------------------------------------- |
| `file`       | A single file                                     |
| `tool`       | Usage of one tool or command                      |
| `branch`     | One git branch                                    |
| `repository` | One git repository                                |
| `project`    | One AgentsView project (may span repos/worktrees) |
| `agent`      | Behavior specific to one coding agent             |
| `global`     | Applies everywhere, no narrower context           |

## CLI usage

Every `agentsview memory` subcommand accepts `--server <url>` to query a remote
daemon instead of the local archive, plus the usual `--format human|json` /
`--json` flags.

### `memory list`

```bash
agentsview memory list --project myapp --type warning
agentsview memory list --current-cwd --trusted-only --json
```

Lists accepted memories. Filters: `--query`, `--project`, `--cwd`,
`--git-branch`, `--agent`, `--type`, `--scope`, `--status`,
`--extractor-method`, `--source-session-id`, `--source-episode-id`,
`--source-run-id`, `--supersedes-memory-id`, `--superseded-by-memory-id`,
`--trusted-only` (requires both `transferable` and `provenance_ok`), and
`--limit`. `--current-cwd`, `--current-git-branch`, and `--current-worktree`
fill `--cwd`/`--git-branch` from the shell's current directory and git state
instead of passing them explicitly; `--current-worktree` sets both together and
cannot be combined with the other current-\* or explicit flags.

### `memory get`

```bash
agentsview memory get mem-abc123 --evidence
```

Fetches one memory by id. `--evidence` prints each evidence range's snippet
alongside the memory.

### `memory query`

```bash
agentsview memory query "flaky test retries" --project myapp --scores --evidence
agentsview memory query "database pooling" --context --summary
```

Same filter flags as `list`, plus:

- `--context` assembles the matched memories into one packed text block sized to
    fit a prompt (`--context-max-bytes` bounds it).
- `--scores` prints the ranking score breakdown (keyword/evidence overlap,
    identifier, phrase, entity, temporal, and confidence boosts) per memory.
- `--evidence` prints evidence snippets per memory.
- `--summary` prints an aggregate breakdown of the result set (by type, scope,
    status, project, agent, cwd, git branch, match reason, extractor, model,
    source run/session/episode, transferability, provenance audit, evidence
    presence, and lifecycle bucket).

### `memory stats`

```bash
agentsview memory stats --project myapp
```

Same filter flags as `list`. Prints the same aggregate breakdown `--summary`
does, over up to `--limit` memories (defaults to the server's max memory page
size).

### `memory brief`

```bash
agentsview memory brief "add retry logic to the sync client" --project myapp
```

Runs a query restricted to `--trusted-only` by default and prints the packed
context block plus the list of memory ids it drew from — meant as a quick task
briefing pulled from prior reviewed work. Accepts the same query flags as
`memory query` (`--context` itself is hidden since brief always requests it),
plus `--scores`, `--evidence`, and `--summary`.

### `memory extract`

```bash
agentsview memory extract --session <session-id> --dry-run
```

Splits a session's messages into the chunks a memory-extraction step would
analyze, and prints them — `--chunk-max-chars` controls the chunk size. This is
preview-only: `--dry-run` is currently required, and omitting it fails with an
error, because model-backed extraction that turns these chunks into memory
candidates is not wired up yet. Producing memories today means writing or
generating a JSONL file yourself and running `memory import` (below).

### `memory import`

```bash
agentsview memory import accepted-memories.jsonl --dry-run
agentsview memory import accepted-memories.jsonl --yes
```

Imports a JSONL file of reviewed memory candidates (format below). Always run
`--dry-run` first: it validates every line and reports what would be imported or
skipped, and why, without writing anything. A real import requires `--yes`.

Safety flags:

- `--allow-remote-import` is required together with `--server` and `--yes` to
    import into a remote daemon; without it, a remote `--dry-run` still works
    but a real import is refused.
- `--allow-production-import` is required to validate or import against the
    default local `AGENTSVIEW_DATA_DIR`/`sessions.db`; without it, import
    refuses to touch your primary archive and asks you to point
    `AGENTSVIEW_DATA_DIR` at an isolated directory instead. This guards against
    accidentally writing a batch of imported memories into the archive you use
    day to day.
- `--require-existing-sessions` (default `true`) rejects any memory whose
    `source_session_id` or evidence ranges are not already present in the target
    archive, so an imported memory's provenance is always checkable against real
    session data.
- `--allow-placeholder-sessions` relaxes that: missing source sessions are
    created as placeholder session rows (marked `machine = "memory-import"`) so
    the memory can still be imported without its original evidence being present
    in this archive.

## HTTP API

| Method | Path                      | Description                                                 |
| ------ | ------------------------- | ----------------------------------------------------------- |
| GET    | `/api/v1/memories`        | List accepted memories; same query filters as `memory list` |
| GET    | `/api/v1/memories/{id}`   | Get one memory by id                                        |
| POST   | `/api/v1/memories/query`  | Query memories; same filters as `memory query`, JSON body   |
| POST   | `/api/v1/memories/import` | Import a JSONL body of reviewed memory candidates           |

`GET /api/v1/memories` accepts the filters above as query parameters (`q`,
`project`, `cwd`, `git_branch`, `agent`, `type`, `scope`, `status`,
`extractor_method`, `source_session_id`, `source_episode_id`, `source_run_id`,
`supersedes_memory_id`, `superseded_by_memory_id`, `trusted_only`, `limit`).
`POST /api/v1/memories/query` takes the same fields as a JSON body, plus
`include_context` and `context_max_bytes` to get back a packed context block.
`POST /api/v1/memories/import` takes the JSONL body directly plus `dry_run`,
`allow_production_import`, `require_existing_sessions`, and
`allow_placeholder_sessions` query parameters, mirroring the CLI's import flags.
Against a PostgreSQL- or DuckDB-backed server, all four endpoints return HTTP
`501 Not Implemented`.

## JSONL import format

Each line of the import file is one JSON object describing a reviewed candidate
memory:

```json
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","trigger":"file read failure","confidence":0.92,"uncertainty":"low","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s1","episode_id":"ep1","run_id":"run1","extractor_method":"single","model":"fake-model","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"],"snippets":["Verify cwd before retrying"]}}
```

Fields:

- `candidate_id` — becomes the memory's id; required, no control characters.
- `type`, `scope` — must be one of the values listed in
    [Types and scopes](#types-and-scopes) above.
- `title`, `body` — required.
- `trigger`, `confidence` (0–1), `uncertainty` — optional.
- `project`, `cwd`, `git_branch`, `agent` — context fields.
- `session_id` — required; the source session this memory came from.
- `episode_id`, `run_id`, `extractor_method`, `model` — optional provenance
    detail about how the candidate was produced.
- `label` — the reviewer's disposition; see
    [Trust gates](#trust-gates-and-labels).
- `supersedes_memory_id` — optional; set this to replace an existing memory (see
    [Supersede, don't delete](#supersede-dont-delete)) instead of inserting a
    fresh one.
- `transferable`, `provenance_ok` — booleans; see below.
- `evidence.ordinal_start`, `evidence.ordinal_end` — required message-ordinal
    range in the source session backing this memory.
- `evidence.tool_use_ids` — optional; when present, one evidence row is created
    per tool use id instead of a single range-only row.
- `evidence.snippets` — optional text joined into the evidence snippet.

### Trust gates and labels

A candidate line is imported only if it clears every gate; otherwise it is
skipped with a reason and `--dry-run` reports which gate it failed:

1. `transferable` must be `true` — skipped as `not_transferable` otherwise.
1. `provenance_ok` must be `true` — skipped as `provenance_not_ok` otherwise.
1. `label` must be one of `correct`, `useful-but-uncertain`, or
    `genuine-tradeoff` — any other label (or none) is skipped as
    `label_not_keeper`.

A line that clears the gates is further rejected outright (the whole import
fails, not just that line) if `--require-existing-sessions` is set and its
`session_id` or evidence range/tool-use isn't found in the target archive, or if
it names a `supersedes_memory_id` that doesn't exist. Duplicate `candidate_id`s
— either already present in the archive or repeated earlier in the same file —
are skipped as `duplicate`.

## Resync preservation

A [resync](/quickstart/) rebuilds the SQLite archive from scratch when parser
logic changes. Memories are not transcript data, so they aren't re-derived
during a resync — instead, the resync copies every memory row (and its evidence)
from the old database into the new one before the atomic swap, the same way it
preserves display names, stars, and pinned messages. If the copy step fails, the
resync aborts and the old database stays in place rather than completing without
your memories. A memory whose source session no longer exists on disk still
survives the copy, since the memory row itself — not the raw session file — is
what resync is protecting.

## Limitations

- **No automated extraction yet.** `memory extract` only previews chunking;
    turning session content into memory candidates automatically, and recalling
    memories by meaning rather than keyword, are both future work.
- **SQLite only.** PostgreSQL sync and the DuckDB mirror have no memory table;
    every memory CLI command and HTTP endpoint against a PG- or DuckDB-backed
    server returns a "not available" error (HTTP 501).
- **No web UI.** Memories are CLI/HTTP-only in this release; the web UI does not
    list, search, or import them.
