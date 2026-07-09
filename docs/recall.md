---
title: Session Recall
description: Reviewed, provenance-linked knowledge distilled from your sessions and recalled by keyword query
---

AgentsView can store **recall entries** — reviewed facts, decisions, procedures,
and warnings distilled from your sessions — in the same local SQLite archive as
the sessions they came from. Each entry carries provenance back to the messages
that support it, so an entry is never a floating claim: it always traces to
specific evidence in a specific session. This is separate from
[semantic search](/semantic-search/), which indexes raw message content; recall
entries are curated, reviewed statements you (or a review step) decided are
worth keeping.

Today, getting entries into the archive is a review-then-import workflow: you or
a tool produce a JSONL file of reviewed candidates, and
`agentsview recall import` writes the ones that pass its trust gates. Once
imported, entries are recalled with keyword queries — `recall query` and
`recall brief` — the same way you'd search session content.

!!! note "SQLite only"

    Recall requires the local SQLite archive. [PostgreSQL sync](/pg-sync/) and the
    [DuckDB mirror](/duckdb/) are read-only mirrors with no recall tables; every
    `agentsview recall` command and every `/api/v1/recall/...` endpoint against a
    PG- or DuckDB-backed server returns a "not available" error.

## What a recall entry is

A recall entry is one row with:

- **A type and a scope** — see [Types and scopes](#types-and-scopes) below.
- **A title and body** — the reviewed statement itself.
- **Optional trigger, confidence, and uncertainty** — when the entry applies,
  how confident the source was, and any caveat text.
- **Context** — the project, working directory, git branch, and agent the entry
  was learned under, used to scope recall to relevant work.
- **Source fields** — `source_session_id` (required), plus optional
  `source_episode_id`, `source_run_id`, `extractor_method`, and `model`,
  recording where the entry came from.
- **Trust flags** — `transferable` and `provenance_ok` (see
  [Trust gates](#trust-gates-and-labels)).
- **Evidence** — one or more
  `(session_id, message_start_ordinal, message_end_ordinal)` ranges, each
  optionally naming a `tool_use_id` and carrying a snippet, that a reader can
  open to verify the claim against the original conversation.
- **Lifecycle fields** — `status` (`accepted` or `archived`),
  `supersedes_entry_id`, and `superseded_by_entry_id` (see
  [Supersede, don't delete](#supersede-dont-delete)).

## Supersede, don't delete

Recall entries are never overwritten or deleted in place. When new evidence
updates or replaces an existing entry, the replacement is inserted with
`supersedes_entry_id` set to the old entry's id; the old entry is archived
(`status = archived`) and gets `superseded_by_entry_id` set to point forward to
the new one. Both rows stay in the archive, so you can always see what an entry
used to say and why it changed.

`agentsview recall stats` and `recall query --summary` classify every entry into
one lifecycle bucket:

| Bucket                   | Meaning                                                     |
| ------------------------ | ----------------------------------------------------------- |
| `active`                 | Current, not superseding or superseded by anything          |
| `replacement`            | Supersedes an older entry                                   |
| `superseded`             | Archived or superseded by a newer entry                     |
| `replacement_superseded` | Itself a replacement, and has since been superseded in turn |

Default recall (`recall list`, `recall query`, `recall brief`) only returns
`accepted` entries unless you pass `--status` explicitly, so a superseded entry
does not show up alongside its replacement by default.

## Types and scopes

Seven entry types (`internal/recall/types.go`):

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

Every `agentsview recall` subcommand accepts `--server <url>` to query a remote
daemon instead of the local archive, plus the usual `--format human|json` /
`--json` flags.

### `recall list`

```bash
agentsview recall list --project myapp --type warning
agentsview recall list --current-cwd --trusted-only --json
```

Lists accepted entries. Filters: `--query`, `--project`, `--cwd`,
`--git-branch`, `--agent`, `--type`, `--scope`, `--status`,
`--extractor-method`, `--source-session-id`, `--source-episode-id`,
`--source-run-id`, `--supersedes-entry-id`, `--superseded-by-entry-id`,
`--trusted-only` (requires both `transferable` and `provenance_ok`), and
`--limit`. `--current-cwd`, `--current-git-branch`, and `--current-worktree`
fill `--cwd`/`--git-branch` from the shell's current directory and git state
instead of passing them explicitly; `--current-worktree` sets both together and
cannot be combined with the other current-\* or explicit flags.

### `recall get`

```bash
agentsview recall get rec-abc123 --evidence
```

Fetches one entry by id. `--evidence` prints each evidence range's snippet
alongside the entry.

### `recall query`

```bash
agentsview recall query "flaky test retries" --project myapp --scores --evidence
agentsview recall query "database pooling" --context --summary
```

Same filter flags as `list`, plus:

- `--context` assembles the matched entries into one packed text block sized to
  fit a prompt (`--context-max-bytes` bounds it).
- `--scores` prints the ranking score breakdown (keyword/evidence overlap,
  identifier, phrase, entity, temporal, and confidence boosts) per entry.
- `--evidence` prints evidence snippets per entry.
- `--summary` prints an aggregate breakdown of the result set (by type, scope,
  status, project, agent, cwd, git branch, match reason, extractor, model,
  source run/session/episode, transferability, provenance audit, evidence
  presence, and lifecycle bucket).

### `recall stats`

```bash
agentsview recall stats --project myapp
```

Same filter flags as `list`. Prints the same aggregate breakdown `--summary`
does, over up to `--limit` entries (defaults to the server's max recall page
size).

### `recall brief`

```bash
agentsview recall brief "add retry logic to the sync client" --project myapp
```

Runs a query restricted to `--trusted-only` by default and prints the packed
context block plus the list of entry ids it drew from — meant as a quick task
briefing pulled from prior reviewed work. Accepts the same query flags as
`recall query` (`--context` itself is hidden since brief always requests it),
plus `--scores`, `--evidence`, and `--summary`.

### `recall extract`

```bash
agentsview recall extract --session <session-id> --dry-run
```

Splits a session's messages into the chunks a recall-extraction step would
analyze, and prints them — `--chunk-max-chars` controls the chunk size. This is
preview-only: `--dry-run` is currently required, and omitting it fails with an
error, because model-backed extraction that turns these chunks into recall
candidates is not wired up yet. Producing entries today means writing or
generating a JSONL file yourself and running `recall import` (below).

### `recall import`

```bash
agentsview recall import accepted-recall.jsonl --dry-run
agentsview recall import accepted-recall.jsonl --yes
```

Imports a JSONL file of reviewed recall candidates (format below). Always run
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
  accidentally writing a batch of imported entries into the archive you use
  day to day.
- `--require-existing-sessions` (default `true`) rejects any entry whose
  `source_session_id` or evidence ranges are not already present in the target
  archive, so an imported entry's provenance is always checkable against real
  session data.
- `--allow-placeholder-sessions` relaxes that: missing source sessions are
  created as placeholder session rows (marked `machine = "recall-import"`) so
  the entry can still be imported without its original evidence being present
  in this archive.

## HTTP API

| Method | Path                          | Description                                                |
| ------ | ----------------------------- | ---------------------------------------------------------- |
| GET    | `/api/v1/recall/entries`      | List accepted entries; same query filters as `recall list` |
| GET    | `/api/v1/recall/entries/{id}` | Get one entry by id                                        |
| POST   | `/api/v1/recall/query`        | Query entries; same filters as `recall query`, JSON body   |
| POST   | `/api/v1/recall/import`       | Import a JSONL body of reviewed recall candidates          |

`GET /api/v1/recall/entries` accepts the filters above as query parameters (`q`,
`project`, `cwd`, `git_branch`, `agent`, `type`, `scope`, `status`,
`extractor_method`, `source_session_id`, `source_episode_id`, `source_run_id`,
`supersedes_entry_id`, `superseded_by_entry_id`, `trusted_only`, `limit`).
`POST /api/v1/recall/query` takes the same fields as a JSON body, plus
`include_context` and `context_max_bytes` to get back a packed context block.
`POST /api/v1/recall/import` takes the JSONL body directly plus `dry_run`,
`allow_production_import`, `require_existing_sessions`, and
`allow_placeholder_sessions` query parameters, mirroring the CLI's import flags.
Against a PostgreSQL- or DuckDB-backed server, all four endpoints return HTTP
`501 Not Implemented`.

## JSONL import format

Each line of the import file is one JSON object describing a reviewed candidate
entry:

```json
{"candidate_id":"m1","type":"debugging_method","scope":"repository","title":"Check cwd before file reads","body":"Verify cwd before retrying failed reads.","trigger":"file read failure","confidence":0.92,"uncertainty":"low","project":"agentsview","cwd":"/repo/agentsview","git_branch":"main","agent":"codex","session_id":"s1","episode_id":"ep1","run_id":"run1","extractor_method":"single","model":"fake-model","label":"correct","transferable":true,"provenance_ok":true,"evidence":{"ordinal_start":3,"ordinal_end":7,"tool_use_ids":["toolu_1"],"snippets":["Verify cwd before retrying"]}}
```

Fields:

- `candidate_id` — becomes the entry's id; required, no control characters.
- `type`, `scope` — must be one of the values listed in
  [Types and scopes](#types-and-scopes) above.
- `title`, `body` — required.
- `trigger`, `confidence` (0–1), `uncertainty` — optional.
- `project`, `cwd`, `git_branch`, `agent` — context fields.
- `session_id` — required; the source session this entry came from.
- `episode_id`, `run_id`, `extractor_method`, `model` — optional provenance
  detail about how the candidate was produced.
- `label` — the reviewer's disposition; see
  [Trust gates](#trust-gates-and-labels).
- `supersedes_entry_id` — optional; set this to replace an existing entry (see
  [Supersede, don't delete](#supersede-dont-delete)) instead of inserting a
  fresh one.
- `transferable`, `provenance_ok` — booleans; see below.
- `evidence.ordinal_start`, `evidence.ordinal_end` — required message-ordinal
  range in the source session backing this entry.
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
it names a `supersedes_entry_id` that doesn't exist. Duplicate `candidate_id`s —
either already present in the archive or repeated earlier in the same file — are
skipped as `duplicate`.

## Resync preservation

A [resync](/quickstart/) rebuilds the SQLite archive from scratch when parser
logic changes. Recall entries are not transcript data, so they aren't re-derived
during a resync — instead, the resync copies every entry (and its evidence) from
the old database into the new one before the atomic swap, the same way it
preserves display names, stars, and pinned messages. If the copy step fails, the
resync aborts and the old database stays in place rather than completing without
your entries. An entry whose source session no longer exists on disk still
survives the copy, since the entry itself — not the raw session file — is what
resync is protecting.

## Limitations

- **No automated extraction yet.** `recall extract` only previews chunking;
  turning session content into recall candidates automatically, and recalling
  entries by meaning rather than keyword, are both future work.
- **SQLite only.** PostgreSQL sync and the DuckDB mirror have no recall tables;
  every recall CLI command and HTTP endpoint against a PG- or DuckDB-backed
  server returns a "not available" error (HTTP 501).
- **No web UI.** Recall entries are CLI/HTTP-only in this release; the web UI
  does not list, search, or import them.
