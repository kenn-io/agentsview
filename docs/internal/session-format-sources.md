# Session Format Source Inventory

This inventory records the best reproducible evidence currently available for
the session formats consumed by Agentsview. It is a maintainer research aid, not
a compatibility guarantee. Source links are pinned; documentation links are
moving first-party pages and include the date checked.

Evidence classes:

- `source`: public producer, persistence, schema, or migration source.
- `documentation`: first-party format documentation without suitable public
  producer source.
- `no-public-source`: no usable public source or authoritative format
  documentation was found after the searches recorded in the entry.

Usage notes distinguish values persisted by the provider from costs Agentsview
computes later with its pricing catalog. A compatible upstream implementation,
independent parser, or recorded fixture is useful evidence for a format, but is
called out when it is not the product's own producer source.

## Pricing Catalog Evidence

Agentsview uses Pydantic GenAI Prices v2 as its historical and conditional
pricing source. The embedded snapshot pins upstream
[`data.json`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/new_data/v2/data.json)
and its generated
[`JSON Schema`](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/new_data/v2/data.schema.json)
at commit `83a49e8b386176a1e28e9d9aedeea5e2b4abc586`. Agentsview preserves the
complete upstream JSON in its embedded artifact and refreshed singleton row. It
compiles the provider and model match rules, ordered start-date and UTC
time-window conditions, token prices, and whole-request tier thresholds at
runtime. The upstream v2 schema permits generic JSON numbers for prices;
Agentsview rejects negative scalar, tier base, and tier prices while accepting
zero. Reverified 2026-08-26 against the pinned data, schema, typed Python
source, and parser boundary.

For a usage event with a valid timestamp, pricing precedence is a user custom
rate resolved from the reported or caller-supplied canonical model, a matching
Pydantic conditional rate at that timestamp, then the existing flat catalog. An
exact custom rate for the reported model takes precedence over canonicalization.
Events without a valid timestamp skip Pydantic because choosing its oldest
conditional record would invent a historical date. Pydantic currently carries
both GPT-5.6 Luna price periods but not Grok 4.6, so the flat fallback remains
necessary.

Agentsview's flat fallback prices come from LiteLLM's
[`model_prices_and_context_window.json`](https://github.com/BerriAI/litellm/blob/418c7c6012d7c39a9d4a28c72cabe1995595ad2b/model_prices_and_context_window.json)
at pinned commit `418c7c6012d7c39a9d4a28c72cabe1995595ad2b`. LiteLLM's
[`cost_per_token` implementation](https://github.com/BerriAI/litellm/blob/418c7c6012d7c39a9d4a28c72cabe1995595ad2b/litellm/litellm_core_utils/llm_cost_calc/utils.py)
shows that these catalog fields are request-pricing thresholds rather than
model-name conventions. Reverified 2026-08-21 against the pinned catalog and
cost implementation.

Agentsview recognizes the anchored standard field shape
`input_cost_per_token_above_<N>[k]_tokens`, including the published 200K and
272K bands, and reads output, cache-creation, 1h cache-creation, and cache-read
companions with the same suffix. The base
`cache_creation_input_token_cost_above_1hr` key is the 1-hour-TTL cache-write
rate (2x input for Anthropic models), not a request-pricing band; its own banded
variants such as `cache_creation_input_token_cost_above_1hr_above_200k_tokens`
follow the companion-suffix shape. Reverified 2026-08-25 against the pinned
catalog: `claude-fable-5` publishes `cache_creation_input_token_cost` 1.25e-05
and `cache_creation_input_token_cost_above_1hr` 2e-05 per token.

A band applies only when whole-request input is strictly greater than its
threshold; when several bands exist, the highest eligible threshold wins.
Additional suffixes for Batch, Flex, Priority, regional, or other service tiers
are deliberately excluded because stored usage does not identify those variants.

Claude and Codex session artifacts provide normalized input, output,
cache-creation, and cache-read token categories, but they do not supply this
pricing metadata. Agentsview therefore uses their request boundaries and token
counts with the catalog bands; it does not infer thresholds from provider or
model names.

Unless an entry states otherwise, entries were last verified on 2026-07-19. A
pinned revision is a reproducible research snapshot, not a claim that it
produced every historical artifact that Agentsview accepts. Where an entry
covers several generations, its **Format** and **Agentsview** fields identify
that boundary; the parser, its colocated tests, and `internal/parser/testdata`
remain the implementation evidence for observed legacy or closed-source
artifacts. Add a producer release or format-version range when one can be tied
confidently to an artifact.

An evidence class names the strongest public authority in an entry, not every
claim in that section. Source links may prove a current producer or migration
while an explicitly labeled limitation remains based on observed files. Generic
standards or documentation that only proves an export exists do not establish
the complete persisted schema.

For `no-public-source` entries, the repeatable search used the first-party
pages, organizations, or pinned public repositories named in the entry, plus
repository and code searches for `<display name> session format`,
`<display name> persistence`, and `<display name> token usage cost`, including
likely JSONL and SQLite names. Reverify an entry during provider-release
investigations, when a new artifact generation appears, for parser or
usage-accounting bug reports, and during periodic inventory review. Record newly
discovered exact URLs, releases, and queries in the provider entry. If a
repository or document disappears, retain its original URL and commit hash and
add an archived or maintained mirror without replacing the original identity.

## Claude Code (`claude`)

- **Format:** Project-scoped JSONL transcripts, including subagent JSONL, with
  `user`, `assistant`, `system`, and progress records.
- **Evidence:** `no-public-source`.
- **Upstream:** The public
  [Claude Code repository](https://github.com/anthropics/claude-code) at
  `015170d3fd84fb57ef4685a64b673fadd0690dc1` and the
  [Claude Code documentation](https://docs.anthropic.com/en/docs/claude-code)
  were checked 2026-07-19. The repository does not publish the CLI persistence
  implementation or an authoritative transcript schema. As independent
  corroboration, clone `https://github.com/getagentseal/codeburn.git` at
  `3472885629c41725b40c19c0780ecce148b067bf` and inspect its
  [Claude format notes](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/docs/providers/claude.md)
  and
  [parser](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/src/providers/claude.ts);
  these are consumer observations, not Anthropic authority.
- **Usage and cost:** Assistant messages persist input, output, cache-creation,
  and cache-read tokens. Model IDs are present. No authoritative persisted USD
  cost field is consumed; Agentsview prices the tokens from its catalog.
  Claude Code also persists Anthropic's nested cache-write TTL breakdown
  verbatim: `message.usage.cache_creation` carries `ephemeral_1h_input_tokens`
  and `ephemeral_5m_input_tokens`, whose sum matches the flat
  `cache_creation_input_tokens` counter. Verified 2026-08-25 against Claude
  Code 2.1.231 transcripts with 1h prompt caching:
  `claude -p --output-format json` `total_cost_usd` matches hand math only
  when the `ephemeral_1h_input_tokens` subset bills at the catalog's 1h
  cache-write rate (2x input), so Agentsview prices that subset at
  `cache_creation_input_token_cost_above_1hr` and the remainder at the 5m
  rate. The flat counter stays authoritative: the nested subset is clamped to
  it, and an absent or malformed breakdown falls back to the 5m rate for the
  whole write total (issue #1452).
- **Server tool use (web search):** Anthropic bills server-side web search at
  $10 per 1,000 requests on top of tokens and reports the count in
  `message.usage.server_tool_use.web_search_requests`, which Claude Code
  persists verbatim inside the stored usage object. Verified 2026-07-30
  against two local web-search sessions: when the search is driven by the
  **CLI's** `WebSearch` tool, every assistant record carries
  `server_tool_use: {"web_search_requests": 0, "web_fetch_requests": 0}` — the
  search itself runs in an out-of-band side call that is **not written to the
  transcript at all**. The only surviving evidence is the tool-result record's
  `toolUseResult` object (`{query, results, durationSeconds, searchCount}`),
  whose `searchCount` matched the wire-billed `web_search_requests` (1 == 1)
  in both sessions. Agentsview therefore credits the assistant message that
  issued the `WebSearch` `tool_use` with its linked result's `searchCount`,
  and uses the message's own counter instead whenever that counter is nonzero
  (which is what sessions driving the API directly report), so a search is
  never counted twice. **Known undercount:** the side call's own token usage —
  tens of thousands of input tokens on `claude-haiku-4-5` per search — is not
  persisted anywhere in the transcript and is not recoverable, so it is
  neither recorded nor estimated. `web_fetch_requests` is recorded when
  present but is not priced. Data version 82 reparses existing Claude archives
  so persisted side-call counts receive the flat fee.
- **Subagent attribution:** Task-tool subagents are written to their own
  transcripts under `<project>/<parent-session-id>/subagents/`, named
  `agent-<id>.jsonl` (nested one more level under `workflows/<workflow-id>/`
  for workflow tools) with an `agent-<id>.meta.json` sidecar. Every record in
  those files carries `isSidechain: true` and the **parent's** `sessionId`,
  and assistant records carry the same full per-message `usage` object as a
  root transcript, so subagent spend is real billed spend that the parent's
  own file does not record. Agentsview ingests each file as its own session
  (id = filename stem) linked to the parent with
  `relationship_type = 'subagent'`, and attributes it to the parent at
  presentation time only — `session usage` and `?subagents=true` combine them,
  while daily and activity aggregates keep counting the child sessions
  directly. Fork branches created inside a subagent subtree remain delegated
  usage and are included in that presentation-time total; root-level forks are
  still traversed only to discover nested subagents. Verified 2026-07-30
  against wire-captured billing for three local Claude Code sessions: parents
  with subagents under-reported cost by 45-77% before the presentation-time
  rollup. Reverified 2026-07-30 with Claude Code 2.1.220: a streaming tool
  turn can persist several assistant records with one
  `(message.id, requestId)` pair while `usage.output_tokens` grows from an
  early partial count to the final billed count (observed examples included
  `5` then `631` and `6` then `798`). Usage reporting therefore keeps the
  greatest output-token snapshot for each message/request identity across the
  included sessions, attributes it to the earliest transcript, and then
  applies cross-session replay deduplication. Session-owned dimensions and
  display metadata also come from that earliest transcript. Numeric-string
  token values remain accepted as compatibility input and are normalized
  before snapshot comparison on every backend. The SQLite and PostgreSQL read
  the exact top-level token path and nested server-tool path even in supported
  malformed legacy JSON. Reverified 2026-08-06 with end-truncated objects
  containing earlier nested decoy keys; PostgreSQL and its
  Cockroach-compatible helper repair the truncated object before extracting
  the requested path, while irreparable input contributes no counter rather
  than an ambiguously scoped value. Equal snapshots are selected
  deterministically by timestamp, session id, and message ordinal; equivalent
  RFC3339 spellings use the semantic tie-breakers rather than raw timestamp
  text. Reverified 2026-08-04 against the cross-backend stored-usage fixtures.
  Replaying the three captured sessions after this correction matched all
  transcript-visible output; each full-wire total remained 15 output tokens
  higher because Claude Code's separate session-title request is not
  persisted. Reverified 2026-08-06 that session-summary export loads matching
  snapshots across excluded sessions and pagination before applying the same
  snapshot, attribution, web-search, and generic deduplication rules. These
  accounting semantics are exposed by usage, activity, and session-summary
  schema version 5 and reporting schema version 2; reporting version 1 retains
  its frozen first-seen, token-only semantics. Reverified 2026-08-06 that
  DuckDB records a web-search-only flat fee as computed pricing provenance, so
  combining it with a provider-reported cost is labeled `mixed` like the
  SQLite archive. Reverified 2026-08-22 that activity buckets carry the
  selected Claude input-token snapshots identically across SQLite, PostgreSQL,
  and DuckDB.
- **Agentsview:** `internal/parser/claude.go` and
  `internal/parser/claude_provider.go`; local observations and fixtures are
  the implementation evidence for fields not documented upstream. Reverified
  2026-07-22 against local CLI transcripts: `type=attachment` records with
  `attachment.type=queued_command` are written mid-stream, in file order
  between consecutive `assistant` records that share one `message.id`, so a
  queued command can fall inside a streaming run that straddles an incremental
  sync boundary. Reverified 2026-07-23 against the transcript shape reported
  in [#1238](https://github.com/kenn-io/agentsview/issues/1238): Claude Code
  for VS Code writes standalone `user` records wrapped in `ide_opened_file` or
  `ide_selection` tags for editor context rather than operator prompts.
  Reverified 2026-07-24 against local CLI transcripts: current transcripts
  carry two top-level launch/prompt-provenance keys that the parser now
  captures — `sessionKind` (session-level, e.g. `"bg"`; present on
  background/headless sessions and absent on interactive ones) and
  `promptSource` (per user turn, e.g. `"typed"`, `"queued"`, `"system"`,
  `"sdk"`). Neither key is documented upstream or covered by the codeburn
  notes; the evidence remains local observation under `no-public-source`.
  Reverified 2026-07-31 against the transcript shape reported in
  [#1265](https://github.com/kenn-io/agentsview/pull/1265): the extension can
  also prepend one `ide_opened_file`/`ide_selection` wrapper directly onto a
  real operator prompt inside a single `user` record (envelope first, prompt
  text after the closing tag, one shared `uuid` for the whole record); the
  parser splits these into a hidden system-metadata message plus the visible
  prompt. Reverified 2026-08-09 against controlled
  `--resume <session> --fork-session` reproductions and inspection of the
  Claude Code 2.1.226 bundle
  ([#1370](https://github.com/kenn-io/agentsview/issues/1370)): the background
  handoff (left-arrow picker, Ctrl+B, `/background`) spawns
  `claude --resume <transcript> --fork-session` with
  `CLAUDE_CODE_SESSION_KIND=bg`. The forked process re-persists the entire
  prior message chain into a new transcript in the same project directory;
  replayed chain entries are byte-identical to the originals (same `uuid`,
  `parentUuid`, `timestamp`, `requestId`, `message.id`, and usage) except for
  a rewritten `sessionId` and, when spawned by the background launcher, an
  injected `sessionKind:"bg"` on every chain entry. The new transcript carries
  no pointer back to the original session (no Codex-style `forked_from_id`),
  so `internal/parser/claude_lineage.go` establishes lineage from sibling
  content overlap anchored on the asymmetric `bg` stamp. Reverified 2026-08-09
  by fork-resuming a transcript containing a uuid-less `queued_command`
  attachment: only uuid-bearing chain entries are replayed into the fork;
  uuid-less records (queued commands, queue-operations, ai-title, mode) never
  appear in the replay region, so every uuid-less line in a fork transcript is
  the fork's own. Reverified 2026-08-09 by forking a fully bg-marked transcript
  with a plain non-bg `--fork-session` process: every replayed line in the new
  transcript carries no `sessionKind` — the writer re-stamps the current
  process's kind on each persisted line, overwriting the copied value, so the
  bg marker reflects the forking process and cannot be inherited through
  replayed entries. Evidence remains `no-public-source`. Reverified 2026-08-16
  with Claude Code 2.1.233 using a controlled `claude -p --session-id <uuid>`
  probe under an isolated `CLAUDE_CONFIG_DIR`. Before the deliberately bounded
  probe was terminated during its API retry, Claude had created the exact UUID
  transcript under `projects/<sanitized-cwd>/`. A working directory containing
  spaces, `.`, `_`, `@`, and separators confirmed that the producer preserves
  ASCII letters, digits, and `-` and replaces every other character with `-`.
  The transcript existed before process exit, so an interrupted wrapper can
  retain exact recovery evidence. One-shot capture copies the exact root and
  bounded subagent tree after an unchanged-file interval, requires every
  persisted child reference to have a captured transcript, and includes every
  captured subagent file even when interruption prevented its link record from
  being flushed. Parser termination remains separate assurance; an interrupted
  transcript can still contain usable token records. Because an unparseable
  middle record may hide usage, one-shot capture marks assurance partial when
  any included session reports parser-malformed lines. One-shot correlation
  reserves the provider root, encoded working directory, and explicit UUID
  across processes until finalization ends. The final usage read keeps SQLite
  ordering, snapshot selection, deduplication, pricing, and token projection
  under the same bounded finalization context. Reverified 2026-08-20 that a
  child-start failure is persisted as a terminal capture state: later
  `capture report` retries do not discover or attribute a matching transcript
  for an execution that never started. Reverified 2026-08-21 that recovery
  refuses to seal when the wrapper did not durably record execution completion,
  even if the transcript is temporarily quiescent and receives more usage
  later. Reverified 2026-08-21 that exact token projection compares canonical
  output and context coverage separately for every included session, while
  crediting a deduplicated snapshot to each source session that contained its
  equivalent row. It also requires the materialized breakdown length to match
  its recorded count. A larger context row from another session therefore
  cannot hide missing delegated input or cache usage. Reverified 2026-08-22
  that an incomplete category breakdown, malformed included transcript, or
  unfinished included session withholds computed or mixed cost;
  provider-reported cost remains authoritative. Reverified 2026-08-27 that
  raw-capture membership mirrors persisted tool output resolution: it includes
  regular files at any depth in the session's `tool-results/` directory and,
  for subagents, the enclosing parent session's `tool-results/` directory.
  These immutable companions are captured with the appendable transcript so a
  reconstructed tree preserves the parser's physical inputs.

## OpenClaude (`openclaude`)

- **Format:** OpenClaude JSONL with Claude-compatible message content and usage
  objects.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/Gitlawb/openclaude.git` at
  `1ddb7d68399a2cd5028d4c5f487676f941879eae`. The pinned
  [session JSONL writer](https://github.com/Gitlawb/openclaude/blob/1ddb7d68399a2cd5028d4c5f487676f941879eae/src/utils/sessionStorage.ts)
  records the session. The
  [project-directory resolver](https://github.com/Gitlawb/openclaude/blob/1ddb7d68399a2cd5028d4c5f487676f941879eae/src/utils/envUtils.ts)
  maps its project directory. The
  [assistant message type](https://github.com/Gitlawb/openclaude/blob/1ddb7d68399a2cd5028d4c5f487676f941879eae/src/types/message.ts)
  defines assistant content.

- **Usage and cost:** Claude-style input, output, cache-creation, and cache-read
  tokens are persisted in each assistant message's API usage object.
  Agentsview derives money from its pricing catalog; no provider-reported cost
  is consumed.

- **Agentsview:** `internal/parser/openclaude.go` plus the shared Claude parsing
  code in `internal/parser/claude.go`; the producer writes the same
  project-scoped JSONL family that the parser consumes.

- **Project-directory layout reverified 2026-07-23:** the pinned session writer
  creates project directories itself with `mkdir(recursive: true)` under
  `<config home>/projects/<sanitized cwd>` and never creates symlinks.
  Symlinked project directories are therefore a user-side arrangement, and
  streaming discovery follows them only to match the legacy `Discover` walk
  (`isDirOrSymlink`), not because the producer emits them.

## Cowork (`cowork`)

- **Format:** A workspace metadata JSON file plus nested Claude-compatible
  project and subagent JSONL transcripts.
- **Evidence:** `no-public-source`.
- **Upstream:** Anthropic's moving
  [Cowork documentation](https://support.anthropic.com/en/collections/14464166-cowork)
  and the public Claude Code repository were checked 2026-07-19. They
  explain the product but do not publish a Cowork disk schema, so the local
  layout and transcript fields remain implementation evidence.
- **Usage and cost:** Nested assistant records carry Claude-style input, output,
  cache-creation, and cache-read tokens with model IDs. Agentsview
  catalog-prices them; no persisted USD total is consumed.
- **Agentsview:** `internal/parser/cowork.go`,
  `internal/parser/cowork_paths.go`, and `internal/parser/cowork_provider.go`.

## Codex (`codex`)

- **Format:** Rollout JSONL files, with a separate JSONL session index used by
  older releases for discovery and metadata. Current releases no longer write
  `session_index.jsonl`; thread titles live in `thread_history_*.sqlite`
  databases that agentsview does not read, so an absent index is the normal
  state, not a rename signal (reverified 2026-08-13 against a live `~/.codex`
  with no `session_index.jsonl` and a populated `thread_history_1.sqlite`).
  The TUI also maintains an append-oriented `history.jsonl` whose records
  contain `session_id`, Unix-seconds `ts`, and submitted prompt `text`;
  configured size enforcement can rewrite a retained tail in place. Agentsview
  consumes only the first two fields as a live-activity hint.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/openai/codex.git` at
  `406dc9239492aff6d295cca5eebe2a548548d42f`; see the pinned
  [rollout recorder](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/rollout/src/recorder.rs)
  and
  [protocol types](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/protocol/src/protocol.rs).
  The pinned
  [message-history implementation](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/message-history/src/lib.rs)
  defines the `session_id`/`ts`/`text` schema, append behavior, file
  location, and the no-write path for `HistoryPersistence::None`. The
  [TUI input-submission path](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/tui/src/chatwidget/input_submission.rs)
  emits accepted submitted text to the
  [TUI history append route](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/tui/src/app/thread_routing.rs).
  The
  [configuration schema](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/core/config.schema.json)
  defines `save-all` (the default) and `none`.
- **Usage and cost:** `token_count` records include total and last usage with
  input, cached input, cache-write input, output, reasoning output, and total
  tokens. Agentsview currently consumes input, cached input, and output only:
  it subtracts cached input from upstream's inclusive input total, maps cached
  input to cache-read, and ignores cache-write and reasoning-output fields.
  Catalog pricing therefore covers only the normalized fields the parser
  emits.
- **Agentsview:** `internal/parser/codex.go` and
  `internal/parser/codex_provider.go`; usage is taken from the last-turn
  counters rather than repeatedly counting cumulative totals. Fork and
  subagent rollouts can begin with a re-stamped copy of the parent's
  transcript, including its `token_count` records. Agentsview follows the
  explicit parent id, compares the ordered `turn_context.turn_id` sequence as
  opaque identifiers, and discards the leading turns also present in the
  parent. UUID versions and identifier bytes carry no chronological meaning;
  the first turn id absent from the parent begins child-owned usage. Missing
  parents fail open, and child-only subagent transcripts are left unchanged.
  S3 imports list the child's configured Codex root for its explicitly named
  parent and materialize only that one parent beside the child. When the
  parent is not yet available or has no turns, the child remains visible but
  is stored below the current data version so a later unchanged-object sync
  retries and corrects the overcount. Reverified 2026-08-13 against the
  materialized-S3 parser-to-SQLite path: the first missing-parent pass kept
  replayed content as retryable, and the next pass fetched only the named
  parent and replaced it with child-owned messages and usage. An appended
  `session_meta` after an incremental-sync offset forces an authoritative
  replacement of that derived session, because the metadata can be the copied
  parent record that activates replay filtering. The original parent session
  remains valid and is not reparsed. Reverified 2026-08-12 against locally
  observed multi-agent rollouts that replayed differently shaped opaque turn
  ids before the first child-owned turn, and against the pinned format
  sources; the pinned TUI is the evidenced `history.jsonl` producer. No
  `append_entry` producer call exists under the pinned `app-server` or `exec`
  trees, so this evidence does not establish IDE, desktop, or `codex exec`
  activity-hint coverage. Locally observed Codex app builds can write the same
  schema, but that is observational evidence rather than a public compatibility
  guarantee. A missing `session_index.jsonl` is verified as normal absence;
  read or scan failures remain unverified and cannot earn persisted freshness
  trust, so a transient failure cannot pin a stale stored title. Agentsview
  derives the hint path as `<configured-sessions-root>/../history.jsonl`; a
  custom sessions root without that sibling, or `HistoryPersistence::None`,
  degrades to ordinary watcher behavior, degraded-coverage polling when
  applicable, and the daily archive audit. Restart bootstrap reads at most the
  newest 4 MiB and accepts records from the preceding 24 hours. If a daemon
  restarts during a longer autonomous run whose last prompt falls outside
  those bounds, the rollout relies on those fallbacks until its next prompt.
  Reverified 2026-08-16 with Codex CLI 0.147.0: `codex exec --json` emitted a
  `thread.started` record carrying one UUID, followed by turn and item records
  and a terminal usage record, while its dated rollout began with a
  `session_meta.id` equal to that UUID and ended with `task_complete`. One-shot
  capture therefore accepts only this structured mode, tees its bytes without
  interpreting formatted stderr, and validates the ID against filenames and
  `session_meta` inside the wrapper-start local and UTC days, each plus or
  minus one day. It copies and ingests that exact rollout first, then uses
  parsed `spawn_agent` links and their message timestamps to repeat the same
  bounded day-shard lookup around each child's spawn time. Final accounting
  uses only the provider-shaped copies in the capture directory. Malformed
  JSONL records are counted on both root and delegated sessions so one-shot
  capture marks otherwise usable accounting as partial instead of silently
  treating the transcript as complete. Reverified 2026-08-20 that this
  includes an unterminated invalid final record after `task_complete`;
  ordinary live parsing still defers that tail while its writer can complete
  it. This bounded lookup is deliberately separate from the provider's general
  full-archive UUID discovery.

## TraeX (`traex`)

- **Format:** Codex-compatible rollout JSONL under a dated `YYYY/MM/DD` tree,
  written by TRAE CLI 2.0, plus the flat `archived_sessions/` directory that
  `traex archive <id>` moves a rollout into. The sibling `history.jsonl`
  carries the same `session_id`/Unix-seconds `ts`/prompt `text` records, and
  agentsview consumes it as the same live-activity hint. No
  `session_index.jsonl` sidecar is produced, so titles come from the rollout
  head alone.
- **Evidence:** `no-public-source`.
- **Upstream:** TRAE CLI 2.0 ships only as a closed-source binary; the observed
  builds report themselves as `traecli 0.200.x`. Trae's first-party
  [product site](https://www.trae.ai/) and the official
  `https://github.com/Trae-AI/Trae.git` repository were searched 2026-08-04
  and publish neither the producer nor a session schema. The equivalence to
  Codex rests on locally observed rollouts whose `session_meta`, `event_msg`,
  `response_item`, and `token_count` records are field-for-field the Codex
  shape -- including `source.subagent.thread_spawn.parent_thread_id` and an
  `originator` of `codex-tui` -- which identifies it as a fork of the
  evidenced codex-rs recorder rather than an independent format. A
  de-identified rollout is retained as a fixture.
- **Usage and cost:** `token_count` records carry the Codex fields, so
  normalization and catalog pricing follow the Codex entry above exactly,
  including the same cache-write and reasoning-output omissions.
- **Agentsview:** `internal/parser/traex.go` relabels the shared Codex parser
  (`internal/parser/codex.go`, `internal/parser/codex_provider.go`) onto the
  `traex:` ID namespace, and `internal/sync` gates the format-shaped branches
  on `isCodexFormatAgent`. The `session_index.jsonl` and S3 branches stay
  Codex-only because TraeX writes no index file and has no archive layout.

## GitHub Copilot CLI (`copilot`)

- **Format:** Flat session JSONL or a session directory containing
  `events.jsonl`.
- **Evidence:** `documentation`.
- **Upstream:** The public
  [Copilot CLI repository](https://github.com/github/copilot-cli) at
  `fd24cea5cb11da4e630485ff2d9269318b8c2a4e` and
  [Copilot CLI session-data documentation](https://docs.github.com/en/copilot/concepts/agents/copilot-cli/chronicle)
  were checked 2026-07-19. GitHub documents complete per-session files under
  `~/.copilot/session-state/` and the derived `~/.copilot/session-store.db`,
  including reindex behavior, but not the event or database schema. The
  [configuration-directory reference](https://docs.github.com/en/copilot/reference/copilot-cli-reference/cli-config-dir-reference)
  further identifies `events.jsonl` and workspace artifacts. No
  producer-side serializer is public. For independent legacy CLI and sibling
  Copilot-store observations, clone
  `https://github.com/getagentseal/codeburn.git` at
  `3472885629c41725b40c19c0780ecce148b067bf` and inspect its
  [Copilot format notes](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/docs/providers/copilot.md)
  and
  [parser](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/src/providers/copilot.ts).
- **Usage and cost:** Shutdown metrics can persist input, output, cache-read,
  cache-write, and reasoning tokens. Copilot accounting is credit-oriented;
  Agentsview does not treat credits as USD and does not infer a monetary cost.
- **Agentsview:** `internal/parser/copilot.go` and
  `internal/parser/copilot_provider.go`. Reverified 2026-07-28 against local
  Copilot CLI 1.0.76-0 transcripts: `tool.execution_start` and
  `tool.execution_complete` carry the same `data.toolCallId` and independent
  RFC3339 `timestamp` values, providing an exact execution interval even when
  the next user message arrives after a long resumed-session idle gap.

## Gemini CLI (`gemini`)

- **Format:** Project chat recordings written as JSONL, with older JSON
  recordings also accepted.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/google-gemini/gemini-cli.git` at
  `acae7124bdd849e554eaa5e090199a0cf08cd782`; see
  [chatRecordingService.ts](https://github.com/google-gemini/gemini-cli/blob/acae7124bdd849e554eaa5e090199a0cf08cd782/packages/core/src/services/chatRecordingService.ts)
  and
  [session management](https://github.com/google-gemini/gemini-cli/blob/acae7124bdd849e554eaa5e090199a0cf08cd782/docs/cli/session-management.md).
- **Usage and cost:** Message usage stores input, output, cached, thoughts,
  tool, and total tokens derived from Gemini API usage metadata. Some records
  are cumulative or streamed, so Agentsview normalizes deltas. Model IDs are
  available; monetary cost is catalog-derived.
- **Agentsview:** `internal/parser/gemini.go` and
  `internal/parser/gemini_provider.go`; both JSON and JSONL generations remain
  supported.

## Gemini Apps (`gemini-apps`)

- **Format:** Google Takeout `My Activity` HTML containing Gemini Apps activity
  cells. Each compatible `Prompted` record is imported as one one-turn session
  with exactly one user message containing the complete visible plain text;
  HTML presentation does not infer speaker roles or generate Markdown. Canvas,
  feedback, and unknown record kinds are counted as skipped. Explicitly
  identified cells from other Takeout products are ignored. The current parser
  supports the observed English rendering for Gemini Apps cells and reports
  declared non-English or otherwise unsupported localized Gemini candidates
  before emitting sessions. Inline code remains inline text, while
  preformatted text preserves authored spaces, tabs, newlines, and backticks
  as data. Session IDs use the canonical UTC timestamp plus a zero-based
  occurrence index among admitted `Prompted` records sharing that timestamp.
  Records with other timestamps can be inserted or reordered without changing
  existing IDs; order remains a tie-breaker only for exact timestamp
  collisions.
- **Evidence:** `no-public-source`.
- **Upstream:** Google's Takeout documentation and public format references were
  searched 2026-08-01. Google does not publish a versioned Gemini Apps
  activity HTML schema, so markup, labels, timestamp zones, and future record
  kinds remain observed compatibility evidence from sanitized exports. No
  translated label or timestamp vocabulary is claimed. Timestamp compatibility
  includes the existing named zones and complete `GMT±H`, `GMT±HH`,
  `GMT±H:MM`, and `GMT±HH:MM` forms, with omitted minutes treated as zero;
  unsupported localized formats and malformed zone tokens return an explicit
  compatibility error.
- **Usage and cost:** Takeout activity records expose no authoritative token,
  cache, reasoning, credit, or monetary-cost fields to Agentsview.
- **Agentsview:** `internal/parser/gemini_apps_takeout.go` and
  `internal/importer/gemini_apps.go`; the CLI-only import path does not affect
  Gemini CLI discovery or parsing.

## Grok Build (`grok`)

- **Format:** Workspace-scoped session directories containing `summary.json`, a
  derived `chat_history.jsonl` model-message cache, and an authoritative
  `updates.jsonl` stream of timestamped ACP and xAI session notifications.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/xai-org/grok-build.git` at
  `d71f6e0c1f5acc5469e503e192fe14824e6f8c90`. The
  [session guide](https://github.com/xai-org/grok-build/blob/d71f6e0c1f5acc5469e503e192fe14824e6f8c90/crates/codegen/xai-grok-pager/docs/user-guide/17-sessions.md)
  identifies `updates.jsonl` as the authoritative conversation log. The
  [storage reducer](https://github.com/xai-org/grok-build/blob/d71f6e0c1f5acc5469e503e192fe14824e6f8c90/crates/codegen/xai-grok-shell/src/session/storage/mod.rs)
  rebuilds `chat_history.jsonl` from that stream, while the
  [JSONL adapter](https://github.com/xai-org/grok-build/blob/d71f6e0c1f5acc5469e503e192fe14824e6f8c90/crates/codegen/xai-grok-shell/src/session/storage/jsonl/mod.rs)
  wraps each persisted update in a Unix-second timestamp envelope. The
  [conversation types](https://github.com/xai-org/grok-build/blob/d71f6e0c1f5acc5469e503e192fe14824e6f8c90/crates/codegen/xai-grok-sampling-types/src/conversation.rs)
  confirm that the derived chat rows themselves carry no message timestamps.
  Agentsview maps timestamped `tool_call` and terminal `tool_call_update`
  records to the existing tool-result event model, so Activity can use tool
  completion time without adding derived transcript messages.
- **Usage and cost:** Durable `turn_completed` updates may carry per-model
  input, output, cache-read, cache-creation, and reasoning tokens plus
  optional `costUsdTicks` (10^10 ticks per USD), as defined by the
  [notification schema](https://github.com/xai-org/grok-build/blob/d71f6e0c1f5acc5469e503e192fe14824e6f8c90/crates/codegen/xai-grok-shell/src/extensions/notification.rs).
  Agentsview emits one usage event per prompt and model, subtracts cache
  reads from the full input count, and uses reported cost ticks when present.
- **Automation:** The first-party
  [headless guide](https://github.com/xai-org/grok-build/blob/d92c5b0b8582fda358de1f97446aa74af44a464f/crates/codegen/xai-grok-pager/docs/user-guide/14-headless-mode.md)
  defines prompt flags as non-interactive invocation. The producer
  propagates that startup mode into
  [`PromptContext.is_non_interactive`](https://github.com/xai-org/grok-build/blob/d92c5b0b8582fda358de1f97446aa74af44a464f/crates/codegen/xai-grok-shell/src/session/acp_session_impl/spawn.rs#L936-L944),
  whose
  [schema](https://github.com/xai-org/grok-build/blob/d92c5b0b8582fda358de1f97446aa74af44a464f/crates/codegen/xai-grok-agent/src/prompt/context.rs#L145-L150)
  identifies headless, SDK, stdio, and generic ACP execution. Its
  [default implementation](https://github.com/xai-org/grok-build/blob/d92c5b0b8582fda358de1f97446aa74af44a464f/crates/codegen/xai-grok-agent/src/prompt/context.rs#L177-L199)
  defaults false for interactive or older contexts. The
  [persistence implementation](https://github.com/xai-org/grok-build/blob/d92c5b0b8582fda358de1f97446aa74af44a464f/crates/codegen/xai-grok-shell/src/session/acp_session.rs#L1384-L1404)
  writes that context to the same session directory as
  `prompt_context.json`, and the
  [spawn call](https://github.com/xai-org/grok-build/blob/d92c5b0b8582fda358de1f97446aa74af44a464f/crates/codegen/xai-grok-shell/src/session/acp_session_impl/spawn.rs#L1049-L1055)
  supplies it. Agentsview treats only an explicit true value in a valid,
  session-associated file as durable automation evidence; file presence, a
  missing field, or a missing file does not classify a session as automated.
- **Agentsview:** `internal/parser/grok.go`, `internal/parser/grok_provider.go`,
  colocated tests, and the sanitized upstream-generated fixtures in
  `internal/parser/testdata/grok-build`.

## MiMo Code (`mimocode`)

- **Format:** OpenCode-compatible SQLite or legacy `storage/session`,
  `storage/message`, and `storage/part` JSON stores.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/XiaomiMiMo/MiMo-Code.git` at
  `f24ce4eb7341bfba6bb608436c1d27a843508adf`; see the SQLite
  [session/message/part tables](https://github.com/XiaomiMiMo/MiMo-Code/blob/f24ce4eb7341bfba6bb608436c1d27a843508adf/packages/opencode/src/session/session.sql.ts),
  persisted
  [message usage shape](https://github.com/XiaomiMiMo/MiMo-Code/blob/f24ce4eb7341bfba6bb608436c1d27a843508adf/packages/opencode/src/session/message.ts),
  and
  [usage normalization and cost calculation](https://github.com/XiaomiMiMo/MiMo-Code/blob/f24ce4eb7341bfba6bb608436c1d27a843508adf/packages/opencode/src/session/session.ts).
- **Usage and cost:** Assistant message data persists input, output, reasoning,
  cache-read, cache-write, model, and a calculated currency cost. Agentsview
  reads the token/model fields but deliberately ignores the stored cost and
  catalog-prices the normalized usage.
- **Agentsview:** `internal/parser/mimocode.go` delegates to
  `internal/parser/opencode.go`; compare MiMo's pinned schema with OpenCode
  whenever their shared parser changes.

## OpenCode (`opencode`)

- **Format:** Current SQLite-backed session/message/part records and the legacy
  JSON storage tree.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/anomalyco/opencode.git` at
  `67caf894e0843ee370e72839e8265e483233479b`; see
  [message-v2.ts](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/opencode/src/session/message-v2.ts)
  and
  [session.ts](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/opencode/src/session/session.ts).
- **Usage and cost:** Assistant messages persist input, output, cache-read, and
  cache-write tokens, plus model/provider identity. Agentsview computes price
  from those tokens rather than consuming a persisted USD total.
- **Working directory:** SQLite sessions store a per-session `directory` and a
  `project_id`. The synthetic `global` project uses `worktree=/`. Agentsview
  prefers a concrete `session.directory` over `project.worktree` when
  resolving cwd/project (verified against live `opencode.db` rows under
  `project_id=global` on 2026-07-23; see #1236).
- **Invalid tool calls:** Model calls to unknown or malformed tools are recorded
  as a synthetic `invalid` tool part whose `execute` succeeds
  (`packages/opencode/src/tool/invalid.ts`, registered in
  `packages/opencode/src/tool/registry.ts` at the pinned commit), so
  `state.status` is `completed` with the error text in the output. Agentsview
  attaches an errored result event to `tool:"invalid"` parts so tool health
  counts them as failures (verified 2026-07-24; see #1254).
- **Bash exit codes:** The `bash` tool declares a structured output of
  `{exit, truncated, timeout}` and returns the child process exit code as
  `exit`
  ([bash.ts](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/core/src/tool/bash.ts)
  at the pinned commit). That structured output is persisted as the tool
  part's `state.metadata`, so `state.metadata.exit` is the authoritative
  failure signal. The tool's own output text carries no `exit status N`
  marker, and the shell is `COMSPEC`/`cmd.exe` on Windows, so text-pattern
  matching alone misses these failures on every platform. Agentsview treats a
  non-zero `state.metadata.exit` on a `bash` tool part as a failure and
  attaches an errored result event. Only `bash` parts record `exit`; other
  tools omit the key. Verified 2026-07-24 against a live `opencode.db` where
  all 24 bash parts with `exit` in `{1, 127, 128}` had output text without an
  `exit status` marker, and the 81 successful parts recorded `exit=0`. Known
  gaps: a command that legitimately exits non-zero (`grep` with no match)
  counts as a failure, matching the existing `exit status N` heuristic, and a
  timed-out command records `timeout: true` with no `exit` key, so it is not
  detected here. See #1256.
- **Change detection (SQLite layout):** every session in a root shares one
  physical `opencode.db`, so the container's own size and mtime move whenever
  any single session is written and cannot discriminate between sessions.
  Agentsview instead builds a per-session composite from
  `session.time_updated`, `project.time_updated`, `MAX(message.time_updated)`,
  and `MAX(part.time_updated)` (`openCodeCompositeMtimeExpr`), and omits the
  container size from the per-session fingerprint. Verified 2026-07-27 against
  an isolated clone of a production container (13.5 GB, 5,981 sessions, 104k
  messages, 508k parts): 432,779 of 508,400 parts (86%) carry
  `time_updated != time_created`, so in-place child edits do move the signal;
  437 sessions have `MAX(part.time_updated) > session.time_updated`, so the
  session row alone is insufficient; and no project's `time_updated` falls
  within 5s of its newest session, so folding `project` in tracks genuine
  worktree/metadata changes rather than ordinary session activity. The child
  scans cost ~0.6s warm on that container because `part.data` lives in SQLite
  overflow pages, so scanning `(session_id, time_updated)` does not read
  transcript bytes. A MAX over timestamps cannot see a deletion: on that
  container 5,758 of 5,981 sessions (96%) carry a session or project timestamp
  at or above every child, so removing a message or part leaves the max
  untouched. The fingerprint hash therefore carries a per-session digest of
  the watermark plus the child row counts, and freshness compares it
  (`FingerprintHashRequiredForFreshness`). An earlier revision of this entry
  claimed a revert stays detectable because it lowers the max; that is wrong for
  the 96% above, and the row counts are what actually cover deletions. Known
  gap: a write that leaves the watermark, the message count and the part count
  all unchanged is not attributed to any session, which requires an in-place
  edit that does not stamp `time_updated`. Containers whose schema lacks the
  child `time_updated` columns (older OpenCode, Kilo, MiMoCode, ICodeMate)
  fall back to the session-only mtime plus the container size and emit an
  empty digest, preserving prior behavior. Watcher events do not pay the child
  scan at all: changed-path classification lists sessions through a bounded
  session-row watermark (`MAX(session.time_updated, project.time_updated)`,
  `ForEachOpenCodeSessionWatermarkMeta`, ordered by session id), compares it
  per session and like-for-like against the stored session/project metadata
  watermark recovered from the persisted child digest
  (`OpenCodeChildDigestMetadataWatermarkNS`; rows without a parseable digest
  fall back to the stored composite), merged in ascending virtual-path order
  against a paged stored-freshness cursor
  (`ListVirtualContainerMemberFreshnessPage` through
  `storedMemberFreshnessPager` and `changedWatermarkSources`), and drops
  covered sessions during the stream — only the changed batch is ever
  materialized, peak memory per event is one stored page plus that batch, and
  the surviving sources resolve the full composite and digest through the
  indexed per-session lookup. The merge trusts stored authority only while a
  container capture taken before the listing still matches a recapture
  afterwards; a stale capture re-lists unfiltered and leaves the decision to
  the per-file gates. The comparison must be like-for-like: the stored
  composite can be dominated by a newer child timestamp, and comparing the
  session-row watermark against it would hide a metadata update (title,
  directory, worktree rename) whose stamp lands below that child maximum. A
  session or project row that advances past its own stored metadata watermark
  is always a candidate, wherever other sessions' watermarks or its own child
  timestamps sit. Periodic full passes and streamed reconciliation passes over
  a container whose captured state still matches the last fully verified pass
  also list the watermark form (`SQLiteContainerUnchangedSinceTrust`): every
  member gate-skips before fingerprinting, so the child identity scan would be
  archive-sized work nothing reads; any write breaks that trust and the next
  pass carries the complete digest again. Watermark-only skips additionally
  require the pass's container capture to still be valid
  (`sqliteContainerPassCaptureValid`) — a container that changes between
  listing and the recapture check resolves full per-session digests instead,
  so a concurrent child-only write cannot hide beneath an unchanged metadata
  watermark. The trade is explicit: any child-only write that leaves the
  session and project rows untouched — wherever its timestamps land relative
  to the stored composite — is invisible to a watcher pass and is reconciled
  by the next full-discovery pass over the now-untrusted container, whose
  digest still catches it; on the production container above, 96% of sessions
  carry a session/project timestamp at or above every child, and actively
  watched sessions bypass this entirely via the per-session composite poll.
  Per-event work is bounded by the changed batch plus one O(session-count)
  scan of small fixed-width rows (the session table and the paged
  stored-member reads); that floor is irreducible without a watermark index,
  which OpenCode's schema does not have and which is not agentsview's to add —
  but only the changed batch and one stored page are ever held in memory.
- **Agentsview:** `internal/parser/opencode.go`,
  `internal/parser/opencode_provider.go`, and
  `internal/parser/opencode_storage_state.go`; legacy and database layouts are
  both intentional compatibility targets. File-backed snapshots retain the
  session JSON byte length in parsed session metadata so persisted `file_size`
  matches the provider fingerprint and an unchanged cold-start sync stops
  after fingerprinting instead of parsing the storage tree again. Reverified
  2026-08-19 that legacy project-metadata events use the indexed sessions
  whose cwd depends on that project. A malformed session path keeps the
  directory fallback active only until that exact path is successfully
  re-indexed or deleted.

## Kilo Code (`kilo`)

- **Format:** Kilo's current session store and OpenCode-compatible legacy
  session/message/part data.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/Kilo-Org/kilocode.git` at
  `938919ab72e3977d1512e0363417270e3337c7b1`; see
  [session.ts](https://github.com/Kilo-Org/kilocode/blob/938919ab72e3977d1512e0363417270e3337c7b1/packages/core/src/session.ts)
  and
  [message.ts](https://github.com/Kilo-Org/kilocode/blob/938919ab72e3977d1512e0363417270e3337c7b1/packages/core/src/session/message.ts).
- **Usage and cost:** Compatible message data includes input, output,
  cache-read, and cache-write tokens with model identity. The parser does not
  consume a Kilo-reported currency total; Agentsview catalog-prices tokens.
- **Agentsview:** `internal/parser/kilo.go` uses the OpenCode family parser.
  Kilo migrations mean the pinned current source must be compared with legacy
  fixtures when changing compatibility.

## Kilo (legacy) (`kilo-legacy`)

- **Format:** Pre-OpenCode Kilo VSCode extension (`kilocode.kilo-code`) task
  directories under VSCode `globalStorage`. Each session holds
  `task_metadata.json` (files-in-context only), the Claude-shaped
  `api_conversation_history.json`, and the Cline-shaped `ui_messages.json`.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/Kilo-Org/kilocode.git` at
  `938919ab72e3977d1512e0363417270e3337c7b1`. The pinned
  [task persistence](https://github.com/Kilo-Org/kilocode/blob/938919ab72e3977d1512e0363417270e3337c7b1/src/core/task-persistence/TaskHistoryStore.ts)
  and
  [UI message reader](https://github.com/Kilo-Org/kilocode/blob/938919ab72e3977d1512e0363417270e3337c7b1/src/core/task-persistence/taskMessages.ts)
  own the Cline-shaped transcript. The extension was superseded by the
  OpenCode-based rebuild (public beta 2026-03-10, GA 2026-04-02); new sessions
  stopped appearing around 2026-03-21.
- **Usage and cost:** `ui_messages.json` carries per-request `api_req_started`
  metadata with input, output, cache-read, and cache-write tokens, explicit
  USD cost, and `usageMissing` flag. `task_metadata.json` does not carry the
  RooCode-style ID/token/cost wiring; token and cost totals are derived from
  the transcript itself.
- **Agentsview:** `internal/parser/kilo_legacy.go` and
  `internal/parser/kilo_legacy_provider.go`; the parser borrows RooCode's
  Cline message handling (tool-call pairing, reasoning, compact boundaries,
  error linking). New sessions stopped after the OpenCode migration.

## Roo Code (`roocode`)

- **Format:** One task directory per session with `history_item.json` metadata
  and a `ui_messages.json` array of UI transcript records.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/RooCodeInc/Roo-Code.git` at
  `b867ec9145750d0ae1ff7f02d35406e9bf2a0b16`. The pinned
  [per-task history store](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/task-persistence/TaskHistoryStore.ts)
  persists `history_item.json`, while the
  [UI message reader and writer](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/task-persistence/taskMessages.ts)
  owns `ui_messages.json`. The
  [history-item construction](https://github.com/RooCodeInc/Roo-Code/blob/b867ec9145750d0ae1ff7f02d35406e9bf2a0b16/src/core/task-persistence/taskMetadata.ts)
  derives the persisted usage totals.
- **Usage and cost:** `history_item.json` persists cumulative input, output,
  cache-write, and cache-read tokens plus `totalCost` and optional API profile
  identity. Agentsview consumes the reported cost, including explicit zero,
  instead of replacing it with catalog pricing.
- **Agentsview:** `internal/parser/roocode.go` and
  `internal/parser/roocode_provider.go`; observed older Roo/Cline message
  variants remain covered by the parser's colocated fixtures.

## OpenHands (`openhands`)

- **Format:** A CLI conversation directory containing `base_state.json` and one
  JSON file per event under `events/`; Agentsview also fingerprints optional
  legacy `TASKS.json` files.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/OpenHands/software-agent-sdk.git` at
  `4fe565663af2b4f1130a6e0dac7566b002bfe9b4`. Inspect the
  [persistence constants](https://github.com/OpenHands/software-agent-sdk/blob/4fe565663af2b4f1130a6e0dac7566b002bfe9b4/openhands-sdk/openhands/sdk/conversation/persistence_const.py)
  for filenames, the
  [base-state writer](https://github.com/OpenHands/software-agent-sdk/blob/4fe565663af2b4f1130a6e0dac7566b002bfe9b4/openhands-sdk/openhands/sdk/conversation/state.py)
  for event-log attachment, and the
  [metrics model](https://github.com/OpenHands/software-agent-sdk/blob/4fe565663af2b4f1130a6e0dac7566b002bfe9b4/openhands-sdk/openhands/sdk/llm/utils/metrics.py)
  for persisted usage and cost fields. The public CLI clone
  `https://github.com/OpenHands/OpenHands-CLI.git` at
  `2df8a2835d3f1bd2f2eadf5a7a2e1ad0dfb0d271` supplies the matching
  [conversation store](https://github.com/OpenHands/OpenHands-CLI/blob/2df8a2835d3f1bd2f2eadf5a7a2e1ad0dfb0d271/openhands_cli/conversations/store/local.py).
- **Usage and cost:** `base_state.json` persists per-model prompt, completion,
  cache-read, cache-write, reasoning, context-window, per-call and accumulated
  token data, plus per-call and accumulated cost. Agentsview currently reads
  transcript events only and exposes none of those persisted metrics; that is
  a parser limitation.
- **Agentsview:** `internal/parser/openhands.go` and
  `internal/parser/openhands_provider.go`; `TASKS.json` is legacy supplemental
  state rather than a requirement of the pinned current producer.

## Cursor (`cursor`)

- **Format:** Legacy text and newer JSONL transcripts under per-project
  `agent-transcripts` directories.
- **Evidence:** `documentation`.
- **Upstream:** Cursor's first-party
  [history documentation](https://docs.cursor.com/en/agent/chat/history)
  confirms local chat persistence and the separate SQLite history index.
  Cursor support on the official forum documents the
  [`~/.cursor/projects/<project>/agent-transcripts` layout](https://forum.cursor.com/t/chat-history-gone-after-pc-restart-agent-transcripts-files-emptied-how-to-recover/158251/5)
  and identifies `state.vscdb` as metadata. Cursor's public GitHub
  organization was also searched 2026-07-19; no transcript schema or producer
  source was found.
- **Usage and cost:** The consumed text/JSONL transcripts have no reliable
  per-message token, cache, reasoning, credit, or monetary-cost fields.
- **Agentsview:** `internal/parser/cursor.go`,
  `internal/parser/cursor_paths.go`, and `internal/parser/cursor_provider.go`;
  workspace identity uses a filesystem-backed unique-match resolver, while
  role and attribution boundaries are reconstructed from Markdown.

## Amp (`amp`)

- **Format:** One JSON thread document per session.
- **Evidence:** `no-public-source`.
- **Upstream:** The first-party [Amp manual](https://ampcode.com/manual), its
  [appendix](https://ampcode.com/manual/appendix), the
  [CLI guide](https://github.com/sourcegraph/amp-examples-and-guides/blob/main/guides/cli/README.md),
  and public Sourcegraph/Amp repositories were searched 2026-07-19 and again
  2026-08-08; no session-file producer or authoritative disk schema was found.
  `amp threads export`, which produces the complete thread documents, is
  itself undocumented. `amp threads raw` is permission-gated (HTTP 403) for
  non-maintainer accounts.
- **Usage and cost:** Complete thread documents carry a per-inference `usage`
  object on assistant messages: `inputTokens`, `outputTokens`,
  `cacheCreationInputTokens`, `cacheReadInputTokens`, `totalInputTokens`,
  `maxInputTokens`, and optionally `model`, `timestamp`, and `thinkingBudget`.
  Amp persists no USD or credit field per thread; `amp threads usage` reports
  Amp credits only and returns `$0` for customer-managed provider keys, so
  Agentsview computes cost from tokens. `totalInputTokens` equals the sum of
  the three input buckets across every record observed (172/172, four threads,
  three model families), and the parser asserts nothing beyond that shape.
- **Field availability:** `model` and `timestamp` are absent from older threads
  — one observed thread carries 67 usage records with no `model` on any of
  them, and no thread-level fallback (`agentMode` is null and no
  `debug.lastInferenceUsage` key exists in exports). Those records are
  recorded as tokens without cost rather than attributed to a guessed model.
  Only the six token and window counters were present in every observed
  record. Usage reporting requires a model, so those rows are stored and
  visible per session but excluded from daily totals and model breakdowns.
  `timestamp` is likewise absent on older threads; those messages fall back to
  the session start for date bucketing.
- **Subagent usage:** Not represented. Threads that invoke `oracle`,
  `librarian`, or subagents record only main-thread inference; tool results
  carry no nested usage and no child thread ID, and subagent threads do not
  appear in `amp threads list`. Those tokens are therefore not counted.
- **Independent parser:** `illegalstudio/lazyagent` (`internal/amp/process.go`)
  reads the same `model`, `inputTokens`, `outputTokens`,
  `cacheCreationInputTokens`, and `cacheReadInputTokens` fields, corroborating
  the field names from outside this project. It is not Amp's own producer
  source.
- **Agentsview:** `internal/parser/amp.go` and
  `internal/parser/amp_provider.go`.

## VS Code Copilot (`vscode-copilot`)

- **Format:** VS Code `chatSessions/<uuid>.json` snapshots and JSONL operation
  logs containing serialized chat requests and responses.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/microsoft/vscode.git` at
  `693614c9f239b49f6d13d55da7f1a851d5b82c36`; see
  [chatModel.ts](https://github.com/microsoft/vscode/blob/693614c9f239b49f6d13d55da7f1a851d5b82c36/src/vs/workbench/contrib/chat/common/model/chatModel.ts)
  and
  [chatSessionStore.ts](https://github.com/microsoft/vscode/blob/693614c9f239b49f6d13d55da7f1a851d5b82c36/src/vs/workbench/contrib/chat/common/model/chatSessionStore.ts).
- **Usage and cost:** Request metadata can persist prompt and output tokens plus
  the resolved model, but has no cache split or provider-reported USD cost in
  the consumed shape. Copilot credits are not treated as currency.
- **Agentsview:** `internal/parser/vscode_copilot.go` and
  `internal/parser/vscode_copilot_provider.go`; both compact snapshots and
  operation logs are supported. Reverified 2026-08-12 against the VS Code
  1.132 JSONL artifact from
  [#1351](https://github.com/kenn-io/agentsview/issues/1351): completed tools
  can persist object-valued `isConfirmed`, terminal commands under
  `toolSpecificData.commandLine.original`, and ordered `inlineReference`
  response items. Agentsview consumes the final response array, which also
  preserves display order, rather than the duplicate tool calls under
  `result.metadata.toolCallRounds`.

## Windsurf (`windsurf`)

- **Format:** VS Code-compatible workspace `state.vscdb` rows whose keys and
  values encode Windsurf tabs and conversation bubbles.
- **Evidence:** `no-public-source`.
- **Upstream:** The first-party
  [Windsurf documentation](https://docs.windsurf.com/) and public Codeium
  repositories were searched 2026-07-19; no producer source or authoritative
  workspace-state schema was found. For a reproducible independent reader,
  clone `https://github.com/veverke/chatwizard.git` at
  `d5d4eebb610da04cdd656be83016973281d82eff`; its pinned
  [workspace discovery](https://github.com/veverke/chatwizard/blob/d5d4eebb610da04cdd656be83016973281d82eff/src/readers/windsurfWorkspace.ts)
  and
  [`cascade.sessionData` parser](https://github.com/veverke/chatwizard/blob/d5d4eebb610da04cdd656be83016973281d82eff/src/parsers/windsurf.ts)
  document the cross-platform `state.vscdb` locations and a directly
  observed key/value shape. This is consumer evidence, not Windsurf authority.
- **Usage and cost:** The consumed state exposes no reliable token, cache,
  reasoning, or USD fields. Windsurf credit accounting is not converted to
  monetary cost.
- **Agentsview:** `internal/parser/windsurf_provider.go` and the shared VS
  Code-state helpers; database keys are reverse-engineered implementation
  evidence.

## Trae (`trae`)

- **Format:** VS Code-compatible workspace and global `state.vscdb` files with a
  JSON session list stored under the `memento/icube-ai-agent-storage`
  `ItemTable` key.
- **Evidence:** `no-public-source`.
- **Upstream:** Trae's first-party [product site](https://www.trae.ai/) and the
  official `https://github.com/Trae-AI/Trae.git` repository at
  `d9386061fd45805f00fd74e09f35566deb4d5a79` were searched 2026-07-21. The
  repository contains product notices rather than the desktop producer, and
  neither source publishes the `state.vscdb` key or an authoritative session
  schema.
- **Usage and cost:** The consumed session list provides optional model identity
  but no token, cache, reasoning, credit, or USD fields. Agentsview leaves
  usage and cost unavailable rather than estimating them.
- **Agentsview:** `internal/parser/trae_provider.go`; the storage key and JSON
  shape are based on observed local databases and controlled fixtures.
  Reverified 2026-08-19 with a controlled two-session database reduced to one
  session and then zero sessions, plus a deleted container: complete parses
  retain emitted members and mark removed members source-missing, missing
  whole containers preserve archived members, and unsupported encrypted
  layouts remain non-authoritative for archive reconciliation.

## Visual Studio Copilot (`visualstudio-copilot`)

- **Format:** OpenTelemetry JSONL spans exported by Visual Studio's GitHub
  Copilot integration.
- **Evidence:** `no-public-source`.
- **Upstream:** GitHub's
  [Copilot usage metrics documentation](https://docs.github.com/en/copilot/reference/copilot-usage-metrics)
  and the OpenTelemetry
  [generative-AI semantic conventions](https://opentelemetry.io/docs/specs/semconv/gen-ai/)
  were checked 2026-07-19. They are supplemental usage-semantics references;
  Visual Studio's emitting implementation, persisted exporter format, and
  on-disk configuration are not public.
- **Usage and cost:** Spans persist `gen_ai.usage.input_tokens` and
  `gen_ai.usage.output_tokens`, with model attributes when emitted. Cache and
  reasoning splits are absent in the consumed data. Copilot credits are not
  USD; Agentsview does not synthesize a currency value from them.
- **Agentsview:** `internal/parser/visualstudio_copilot.go`,
  `internal/parser/visualstudio_copilot_provider.go`, and
  `docs/internal/visual-studio-copilot-traces.md`.

## Pi (`pi`)

- **Format:** A tree-structured JSONL log with a session header and entries
  connected by `id` and `parentId`.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/earendil-works/pi.git` at
  `f1c587dde39025c75d7397bc14532d8fa5c001d9`; see the pinned
  [session format](https://github.com/earendil-works/pi/blob/f1c587dde39025c75d7397bc14532d8fa5c001d9/packages/coding-agent/docs/session-format.md)
  and
  [session manager](https://github.com/earendil-works/pi/blob/f1c587dde39025c75d7397bc14532d8fa5c001d9/packages/coding-agent/src/core/session-manager.ts).
- **Usage and cost:** Assistant messages persist input and output tokens plus
  cache-read and cache-write/creation values in nested or historical flat
  shapes. Model IDs are present. Agentsview catalog-prices the tokens. Data
  version 81 reparses existing Pi-family archives after adding the flat
  `cacheWrite` spelling.
- **Agentsview:** `internal/parser/pi.go` and `internal/parser/pi_provider.go`;
  alternate branches remain in the file but only the active ancestry is a
  conversation.

## Prime Agent (`prime-agent`)

- **Format:** Pi-family, tree-structured JSONL with current saved sessions
  stored directly under a flat session directory.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/PrimeIntellect-ai/prime-agent.git` at
  `0e0d23391bcd879f1aea70dbda4d07dda7970b34`; see the pinned

    [session format](https://github.com/PrimeIntellect-ai/prime-agent/blob/0e0d23391bcd879f1aea70dbda4d07dda7970b34/packages/coding-agent/docs/session-format.md),

    [session types and persistence](https://github.com/PrimeIntellect-ai/prime-agent/blob/0e0d23391bcd879f1aea70dbda4d07dda7970b34/packages/coding-agent/src/core/session-manager.ts),
    and
    [configuration paths](https://github.com/PrimeIntellect-ai/prime-agent/blob/0e0d23391bcd879f1aea70dbda4d07dda7970b34/packages/coding-agent/src/config.ts).

- **Usage and cost:** Assistant messages persist input, output, cache-read, and
  cache-write tokens with model IDs. `child_usage_attributed` entries replace
  a target assistant message's usage with the persisted aggregate that
  includes RLM child work. Agentsview consumes that aggregate and
  catalog-prices the normalized tokens rather than trusting the producer's
  persisted cost object. Attribution is applied in file order: records for an
  unknown target are ignored, and the last aggregate for a known target wins.
  Prime Agent full parses replace stored messages because a later append can
  retroactively update an earlier assistant message.

- **Agentsview:** Prime Agent is registered through the Pi-family parser in
  `internal/parser/pi.go` and `internal/parser/pi_provider.go`, with its own
  flat-root discovery, `prime-agent:` session identity, and `parentSession`
  path handling. Reverified 2026-08-06 against a live Prime Agent v0.7.0
  session: the flat transcript filename UUID can differ from the session
  header UUID, so canonical ID lookup falls back to scanning session headers.
  Stored file and fingerprint hints remain advisory during identity-based
  lookup: Agentsview accepts one only when its header identity matches the
  requested Prime Agent session. Parent lineage accepts both POSIX and Windows
  separators and resolves the referenced sibling transcript's header UUID from
  the configured session root before trying the original persisted path; when
  neither file is available, the filename UUID remains the explicit fallback.
  Upstream defines `parentSession` for sessions derived through `/fork`,
  `/clone`, or `newSession({ parentSession })`, so Agentsview records that
  lineage as a fork. The same artifact persisted OpenAI Codex model identity
  and per-message input and output usage in the documented Pi-family fields.
  Support targets v0.7.0's current flat layout; that producer migrates older
  per-project sessions before normal session listing.

## Oh My Pi (`omp`)

- **Format:** Pi-family JSONL with Oh My Pi session entry and persistence
  extensions.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/can1357/oh-my-pi.git` at
  `39c95e5e29b1c8b082059f57421ce445c3dffdd4`; see
  [session-entries.ts](https://github.com/can1357/oh-my-pi/blob/39c95e5e29b1c8b082059f57421ce445c3dffdd4/packages/coding-agent/src/session/session-entries.ts),

    [session-persistence.ts](https://github.com/can1357/oh-my-pi/blob/39c95e5e29b1c8b082059f57421ce445c3dffdd4/packages/coding-agent/src/session/session-persistence.ts),
    and
    [usage.ts](https://github.com/can1357/oh-my-pi/blob/39c95e5e29b1c8b082059f57421ce445c3dffdd4/packages/ai/src/usage.ts).

- **Usage and cost:** Pi-family usage persists input, output, cache-read, and
  cache-write tokens with a model. Agentsview derives monetary cost from the
  catalog; provider reporting notes are not treated as exact persisted USD.

- **Agentsview:** Oh My Pi is registered through the Pi-family provider in
  `internal/parser/pi.go` and `internal/parser/pi_provider.go`.

## Qwen Code (`qwen`)

- **Format:** Gemini-derived project chat-record JSONL.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/QwenLM/qwen-code.git` at
  `076427650d363ce9e9a0962f389361b474c170dc`; see
  [chatRecordingService.ts](https://github.com/QwenLM/qwen-code/blob/076427650d363ce9e9a0962f389361b474c170dc/packages/core/src/services/chatRecordingService.ts)
  and
  [tokenUsageService.ts](https://github.com/QwenLM/qwen-code/blob/076427650d363ce9e9a0962f389361b474c170dc/packages/core/src/services/tokenUsageService.ts).
- **Usage and cost:** `usageMetadata` supplies prompt, candidate/output,
  cached-content, thoughts, and total tokens. Streaming records may repeat
  cumulative values, so Agentsview aggregates carefully. Price is
  catalog-derived.
- **Agentsview:** `internal/parser/qwen.go` and
  `internal/parser/qwen_provider.go`.

## Command Code (`commandcode`)

- **Format:** Session JSONL accompanied by a `.meta.json` sidecar.
- **Evidence:** `no-public-source`.
- **Upstream:** Command Code's first-party product site, documentation surfaces,
  and public GitHub repositories were checked 2026-07-19. Clone the official
  `https://github.com/CommandCodeAI/command-code.git` repository at
  `a774fe8cbe71697d115d4660de299c9c1b286cea`; it contains product and issue
  material only, not the CLI implementation. No authoritative persistence
  source or disk schema was public.
- **Usage and cost:** The consumed records provide transcript and metadata but
  no token, cache, reasoning, credit, or USD accounting to Agentsview.
- **Agentsview:** `internal/parser/commandcode.go` and
  `internal/parser/commandcode_provider.go`.

## DeepSeek TUI (`deepseek-tui`)

- **Format:** Per-session JSON documents, excluding transient latest-session and
  offline-queue artifacts.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/Hmbown/DeepSeek-TUI.git` at
  `7e845f3bf409d2eb06a2f4764c0b332b4190b0c3`; the project is now branded
  CodeWhale. See the
  [saved-session schema and atomic writer](https://github.com/Hmbown/DeepSeek-TUI/blob/7e845f3bf409d2eb06a2f4764c0b332b4190b0c3/crates/tui/src/session_manager.rs)
  and
  [message/content-block schema](https://github.com/Hmbown/DeepSeek-TUI/blob/7e845f3bf409d2eb06a2f4764c0b332b4190b0c3/crates/tui/src/models.rs).
- **Usage and cost:** Session metadata persists aggregate `total_tokens`, model
  and provider identity, plus separate parent-session and subagent USD/CNY
  cost snapshots and displayed high-water marks. It does not persist a
  dependable input/output/cache/reasoning token split. Agentsview currently
  emits no usage event from this metadata; that is a parser limitation.
- **Agentsview:** `internal/parser/deepseek_tui.go` and
  `internal/parser/deepseek_tui_provider.go`; both `.codewhale` and legacy
  `.deepseek` roots are intentional.

## DeepSeek Harness (`deepseek-harness`)

- **Format:** Version `0` session JSONL under
  `<sessions-root>/<project>/<encoded-session-id>/session.jsonl`, or the
  default checksummed multi-frame zstd encoding at `session.jsonl.zstd`. The
  immutable header records session identity, cwd, creation time, seed lineage,
  delegation origin, and agent preset. Event rows carry a contiguous `seq`;
  runs of assistant deltas may use the `text-chunks`, `reasoning-chunks`, and
  `tool-call-chunks` packed storage rows. Session IDs are arbitrary non-empty
  strings and are injectively encoded before use as a directory name. A
  sessions root belongs to one physical encoding; the upstream backend rejects
  an opposite-suffix artifact rather than providing mixed-root fallback or
  migration.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/deepseek-ai/deepseek-harness.git` at
  `47f943859bef60e4160492346772ded9b24f765a`. See the pinned
  [JSONL layout, header, and scanner](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/session/session-persistence-jsonl/src/format.ts),

    [multi-frame zstd backend](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/session/session-persistence-jsonl/src/index.ts),

    [packed chunk codec](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/session/src/chunk-rows.ts),

    [session event and seed schema](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/session/src/types.ts),

    [turn and step production order](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/agent-loop/src/agent.ts#L245-L292),

    [turn and step invariants](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/core/session/src/invariant.ts),

    [agent preset reconstruction](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/preset/agent-presets/src/session.ts),

    [compaction model-call facts](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/compaction/compaction/src/types.ts),
    and the
    [message/content schema](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/llm/llm/src/message.ts)
    plus the
    [usage schema](https://github.com/deepseek-ai/deepseek-harness/blob/47f943859bef60e4160492346772ded9b24f765a/packages/llm/llm/src/types.ts).

- **Usage and cost:** Each model response and summarizing compaction can persist
  disjoint input, output, cache-read, cache-write, and reasoning token counts.
  Assistant provenance, request headers, and compaction summaries carry model
  IDs. Harness persists no USD amount; Agentsview prices recognized models
  from its catalog.

- **Agentsview:** `internal/parser/deepseek_harness.go`,
  `internal/parser/deepseek_harness_format.go`, and
  `internal/parser/deepseek_harness_provider.go`. Only events at or after a
  child's `seedLength` contribute transcript rows and usage, while the full
  log remains available to validate event and turn/step structure and fold the
  latest title and agent preset. Surface replacements are excluded from the
  human transcript, and a chunk-only live response is positioned from its
  first assistant chunk and reconstructed until a final assistant message
  replaces it on the next authoritative parse. Agentsview reversibly escapes
  `%` and the reserved remote-host separator `~` in canonical session IDs.
  Explicit raw-ID lookups remain literal; canonical escaping is decoded only
  when lookup starts from a full session ID. Per-response usage events are the
  sole analytics rows, while messages retain explicit context/output token
  fields without duplicating the raw Harness usage blob into `token_usage`.
  Plain and zstd artifacts in one session directory are treated as one logical
  source and rejected while both exist; a change maps directly to the
  surviving sibling once that conflict is removed. The optional Harness SQLite
  persistence backend is not supported.

## OpenClaw (`openclaw`)

- **Format:** Per-agent session JSONL managed by the OpenClaw session store.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/openclaw/openclaw.git` at
  `40d31f34813c2a01284b097c0d0d785fbb173400`; see
  [session-store.ts](https://github.com/openclaw/openclaw/blob/40d31f34813c2a01284b097c0d0d785fbb173400/src/agents/command/session-store.ts)
  and
  [usage-accumulator.ts](https://github.com/openclaw/openclaw/blob/40d31f34813c2a01284b097c0d0d785fbb173400/src/agents/embedded-agent-runner/usage-accumulator.ts).
- **Usage and cost:** Messages persist input, output, cache-read, cache-write,
  model identity, and sometimes `usage.cost.total`. Agentsview intentionally
  ignores the reported cost and catalog-prices normalized token fields to keep
  pricing attribution consistent.
- **Agentsview:** `internal/parser/openclaw.go`.

## QClaw (`qclaw`)

- **Format:** OpenClaw-compatible agent session JSONL with QClaw-specific root
  discovery.
- **Evidence:** `no-public-source`.
- **Upstream:** QClaw's product pages and public repository search were checked
  2026-07-19. Tencent's first-party
  [launch description](https://www.tencent.com/tencent-launches-qclaw-globally-lowering-barriers-to-ai-agent-deployment/)
  confirms that QClaw is built on OpenClaw, but publishes neither the exact
  embedded OpenClaw revision nor its wrapper's persistence changes. The public
  OpenClaw producer source pinned in the `openclaw` entry therefore describes
  the compatible format family, not the exact QClaw build.
- **Usage and cost:** Compatible records can contain input, output, cache-read,
  cache-write, model, and reported total cost. As for OpenClaw, Agentsview
  ignores the reported monetary field and catalog-prices tokens.
- **Agentsview:** `internal/parser/qclaw.go` delegates message decoding to
  `internal/parser/openclaw.go`.

## Kimi CLI (`kimi`)

- **Format:** Session directories containing `wire.jsonl`, with both current and
  legacy wire layouts.

- **Working directory:** Native Kimi Code `config.update` records can carry the
  provider-emitted absolute `cwd`; Agentsview preserves the last non-empty
  value for sync filtering. The exact issue artifact is covered by
  `internal/parser/testdata/kimi-config-update-cwd.jsonl`.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/MoonshotAI/kimi-cli.git` at
  `4a550effdfcb29a25a5d325bf935296cc50cd417`; see
  [session.py](https://github.com/MoonshotAI/kimi-cli/blob/4a550effdfcb29a25a5d325bf935296cc50cd417/src/kimi_cli/session.py),

    [wire-mode.md](https://github.com/MoonshotAI/kimi-cli/blob/4a550effdfcb29a25a5d325bf935296cc50cd417/docs/en/customization/wire-mode.md),
    and the
    [Kimi provider usage mapping](https://github.com/MoonshotAI/kimi-cli/blob/4a550effdfcb29a25a5d325bf935296cc50cd417/packages/kosong/src/kosong/chat_provider/kimi.py).

- **Usage and cost:** Native usage distinguishes uncached/other input, output,
  cache read, and cache creation. The aggregate fallback exposes only output
  and is therefore a lower bound. Agentsview catalog-prices usage with a
  model.

- **Agentsview:** `internal/parser/kimi.go` and
  `internal/parser/kimi_provider.go`.

## Kimi Work (`kimi-work`)

- **Format:** Kimi Desktop's `daimon` runtime stores each user conversation as a
  Kimi Code kernel session with a `wire.jsonl` transcript. Current files use
  `<workspace>/conv-*/agents/<agent>/wire.jsonl`; the legacy
  `<workspace>/conv-*/wire.jsonl` layout is also accepted. Sibling `ctitle-*`,
  `sklsum-*`, and `dvlt-*` runtime sessions are internal work and are
  excluded.
- **Evidence:** `no-public-source`.
- **Upstream:** Kimi's first-party
  [Kimi Work overview](https://www.kimi.com/en-cn/help/kimi-work/overview) was
  checked 2026-07-27. It confirms that Kimi Work is the local agent in the
  macOS and Windows desktop client and uses Kimi Code as its agent kernel, but
  it does not publish the desktop persistence schema or pin the embedded
  kernel revision. The pinned Kimi CLI producer sources in the `kimi` entry
  establish the shared wire-format family, while observed Kimi Work artifacts
  establish the desktop directory wrapper and auxiliary-session prefixes.
- **Usage and cost:** The shared wire records expose input, output, cache-read,
  and cache-creation token counts. Kimi Work can report the internal model
  aliases `daimon-kimi-code`, `daimon-kimi-messages`, `k2d6-agent`, and
  `k3-agent`; Agentsview catalog-prices those tokens. The explicit
  `k2d6-agent` alias resolves to K2.6. The date-ambiguous `daimon-*` aliases
  resolve to K2.6 before the 2026-07-19 UTC cutoff and K3 at or after it. When
  a transcript omits model metadata, Agentsview uses the date-ambiguous
  `daimon-kimi-code` alias so the same timestamp rule applies instead of
  assuming one model era. No authoritative persisted USD cost is consumed.
- **Event ordering reverified 2026-08-03:** observed protocol-1.4 transcripts
  can persist `tool.call`, then `tool.result`, then `step.end` for one model
  step. The following `usage.record` repeats the same native usage values.
  Agentsview keeps the assistant tool-call message as the pending usage target
  across the user-role tool result, attaches the trailing `step.end` usage,
  and treats `usage.record` only as a fallback so the step is counted once.
- **Agentsview:** `internal/parser/kimi_work_provider.go` constrains discovery
  to user conversations, delegates wire decoding to `internal/parser/kimi.go`,
  and rewrites the provider identity and aggregate usage-event keys to
  `kimi-work`.

## Claude.ai Export (`claude-ai`)

- **Format:** The `conversations.json` artifact from a Claude.ai data export.
- **Evidence:** `documentation`.
- **Upstream:** Anthropic's first-party
  [data export instructions](https://support.anthropic.com/en/articles/9450526-how-can-i-export-my-claude-ai-data)
  were checked 2026-07-19. They establish the export artifact but do not
  publish its complete JSON schema.
- **Usage and cost:** The export contains conversation content and timestamps,
  not authoritative token, cache, reasoning, credit, or USD accounting.
- **Agentsview:** `internal/parser/claude_ai.go`; this is an import format, not
  a live application session store.

## ChatGPT Export (`chatgpt`)

- **Format:** `conversations.json` and numbered `conversations-*.json` export
  artifacts containing a conversation DAG and message mapping.
- **Evidence:** `documentation`.
- **Upstream:** OpenAI's first-party
  [ChatGPT data export instructions](https://help.openai.com/en/articles/7260999-how-do-i-export-my-chatgpt-history-and-data)
  were checked 2026-07-19. The help page does not publish a versioned JSON
  schema.
- **Usage and cost:** Export messages may include `model_slug`, but the artifact
  does not provide authoritative token, cache, reasoning, credit, or cost
  data.
- **Agentsview:** `internal/parser/chatgpt.go`; graph ancestry is flattened for
  display and the importer does not claim billing completeness.

## Kiro CLI (`kiro`)

- **Format:** Legacy JSONL plus companion metadata JSON, and newer SQLite
  session databases.

    The issue-reported current layout uses
    `~/.kiro/sessions/<workspace>/sess_<id>/messages.jsonl` or the direct
    `sess_<id>/messages.jsonl` form, with optional `session.json`. Agentsview
    admits only these exact producer-relative shapes: one workspace segment, no
    `.history` or `snapshots` workspace, a valid `sess_<id>` directory, and no
    nested session directory. It preserves the literal `sess_<id>` identity and
    maps user, assistant, tool-call, and tool-result envelope fields; unknown
    and malformed records are ignored. This observed layout has no pinned
    producer schema source. For duplicate IDs, SQLite outranks current JSONL,
    current outranks legacy JSONL, configured root order breaks ties within a
    class, and recency then canonical path provide deterministic ties.

- **Evidence:** `documentation`.

- **Upstream:** Kiro's first-party [license page](https://kiro.dev/license/) and
  [conversation-persistence documentation](https://kiro.dev/docs/cli/chat/#conversation-persistence)
  were checked 2026-07-19: current Kiro CLI is proprietary. The
  documentation confirms automatic per-directory database persistence,
  resume-by-ID, and manual JSON save/load, but does not publish either
  database generation's schema. The open-source predecessor can be cloned from
  `https://github.com/aws/amazon-q-developer-cli.git` at
  `15cc8f3cd18c4272925ce1c7053268eedff1ea0a`, but its pinned
  [conversation migration](https://github.com/aws/amazon-q-developer-cli/blob/15cc8f3cd18c4272925ce1c7053268eedff1ea0a/crates/chat-cli/src/database/sqlite_migrations/007_conversations_table.sql)
  does not establish either Kiro generation. Useful independent format
  evidence can be cloned from `https://github.com/ingo-eichhorst/Irrlicht.git`
  at `12375a273a289c131a45b4fd3eb1ad6483b4e9d4`; see its pinned
  [Kiro JSONL parser](https://github.com/ingo-eichhorst/Irrlicht/blob/12375a273a289c131a45b4fd3eb1ad6483b4e9d4/core/adapters/inbound/agents/kirocli/parser.go),

    [sidecar metrics reader](https://github.com/ingo-eichhorst/Irrlicht/blob/12375a273a289c131a45b4fd3eb1ad6483b4e9d4/core/adapters/inbound/agents/kirocli/sidecar_metrics.go),
    and recorded
    [token-accounting assessment](https://github.com/ingo-eichhorst/Irrlicht/blob/12375a273a289c131a45b4fd3eb1ad6483b4e9d4/replaydata/agents/kiro-cli/scenarios/5-1_token-accounting/metadata.json).
    These are consumer observations, not Kiro producer authority, and they do
    not cover the newer `conversations_v2` writer.

- **Usage and cost:** JSONL events contain no model, token, cache, credit, or
  USD fields. The companion state can contain model/window metadata, context
  percentage, and per-turn credit metering; the recorded Kiro 2.5.1 evidence
  found input/output counters present but zero and no cache split. Agentsview
  currently consumes none of those sidecar usage fields, so it emits no Kiro
  usage or cost metrics.

- **Agentsview:** `internal/parser/kiro.go`, `internal/parser/kiro_sqlite.go`,
  and `internal/parser/kiro_provider.go`; both generations must remain
  discoverable.

## Kiro IDE (`kiro-ide`)

- **Format:** Historical `.chat` files and newer workspace-session JSON data.
- **Evidence:** `no-public-source`.
- **Upstream:** Kiro's first-party [license page](https://kiro.dev/license/),
  [documentation](https://kiro.dev/docs/), and the public
  [kirodotdev/Kiro repository](https://github.com/kirodotdev/Kiro/tree/e8daa058590dd58efb14f6d41ddb3ba1a26cfba3)
  were checked 2026-07-19. The IDE is proprietary, and the public repository
  contains community and issue infrastructure rather than the IDE persistence
  serializer or a versioned disk schema.
- **Usage and cost:** Model metadata may be present, but the consumed format
  exposes no authoritative token, cache, reasoning, credit, or monetary cost.
- **Agentsview:** `internal/parser/kiro_ide.go` and
  `internal/parser/kiro_ide_provider.go`.

## Cortex (`cortex`)

- **Format:** A session JSON document with an optional `.history.jsonl`
  companion.
- **Evidence:** `documentation`.
- **Upstream:** Snowflake's first-party
  [CoCo session-replay guide](https://www.snowflake.com/en/developers/guides/create-shareable-coco-session-replays-with-cortex-replay/)
  was checked 2026-07-19 and documents automatic JSON transcript storage at
  `~/.snowflake/cortex/conversations/<session-id>.json`. It links an
  independent open-source reader: clone
  `https://github.com/dataprofessor/cortex-replay.git` at
  `d61d46a7acbe55b3367f695a04e56eca24871320` and inspect the pinned
  [session parser](https://github.com/dataprofessor/cortex-replay/blob/d61d46a7acbe55b3367f695a04e56eca24871320/src/parser.mjs).
  Snowflake does not publish the producer or a versioned schema, and the
  independent reader does not cover the newer split-history generation.
- **Usage and cost:** The consumed files expose transcript content but no token,
  cache, reasoning, credit, or USD accounting.
- **Agentsview:** `internal/parser/cortex.go` and
  `internal/parser/cortex_provider.go`.

## Hermes Agent (`hermes`)

- **Format:** `state.db` for indexed state and usage, with JSONL/JSON session
  transcripts retained for compatibility.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/NousResearch/hermes-agent.git` at
  `299e409f15aa5615a8a64be488580be92cda351e`; see
  [hermes_state.py](https://github.com/NousResearch/hermes-agent/blob/299e409f15aa5615a8a64be488580be92cda351e/hermes_state.py)
  and
  [usage_pricing.py](https://github.com/NousResearch/hermes-agent/blob/299e409f15aa5615a8a64be488580be92cda351e/agent/usage_pricing.py).
- **Usage and cost:** State records distinguish input, output, cache-read,
  cache-write, and reasoning tokens and can retain estimated or actual cost
  with status/source metadata. Agentsview uses provider-reported cost when it
  is meaningfully identified; otherwise it falls back to catalog pricing.
- **Agentsview:** `internal/parser/hermes.go` and
  `internal/parser/hermes_provider.go`; database and file generations are both
  recognized.

## Forge (`forge`)

- **Format:** A `.forge.db` SQLite database containing conversations, context
  messages, and usage records.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/tailcallhq/forgecode.git` at
  `c5698103bce973d1c569ae905bca6f34ba85c1d0`; see
  [conversation_record.rs](https://github.com/tailcallhq/forgecode/blob/c5698103bce973d1c569ae905bca6f34ba85c1d0/crates/forge_repo/src/conversation/conversation_record.rs)
  and the pinned
  [conversation migration](https://github.com/tailcallhq/forgecode/blob/c5698103bce973d1c569ae905bca6f34ba85c1d0/crates/forge_repo/src/database/migrations/2025-09-12-065405_create_conversations_table/up.sql).
- **Usage and cost:** Usage records distinguish actual prompt, completion, and
  cached tokens. Although Forge domain data can discuss cost, Agentsview does
  not consume a direct persisted currency total from this store and instead
  catalog-prices normalized tokens.
- **Agentsview:** `internal/parser/forge.go`.

## Devin CLI (`devin`)

- **Format:** `cli/sessions.db` for session metadata plus transcript JSON
  artifacts. The `sessions.created_at`, `sessions.last_activity_at`, and
  `message_nodes.created_at` columns are Unix epoch seconds (not
  milliseconds). Verified against a live Devin CLI database 2026-07-31, and
  reverified independently against CLI 3000.3.22 the same day. Because the
  unit is observed rather than documented, the parser rejects values outside
  the nanosecond-representable epoch-second range instead of converting them,
  so a future unit change surfaces as missing timestamps rather than as a
  silently overflowed far-future mtime that would wedge resync.
- **Evidence:** `no-public-source`.
- **Upstream:** Cognition's first-party
  [Devin documentation](https://docs.devin.ai/) and public repositories were
  searched 2026-07-19; no CLI database schema or transcript serializer was
  published. The transcript generation follows the public Agent Trajectory
  Interchange Format: clone `https://github.com/harbor-framework/harbor.git`
  at `071281b3d931aafd6a5375fa7d5933e23054d784` and see the pinned
  [ATIF specification](https://github.com/harbor-framework/harbor/blob/071281b3d931aafd6a5375fa7d5933e23054d784/rfcs/0001-trajectory-format.md).
  Devin-specific field aliases and the SQLite enrichment store are
  independently documented by `https://github.com/getagentseal/codeburn.git`
  at `3472885629c41725b40c19c0780ecce148b067bf` in its
  [Devin format notes](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/docs/providers/devin.md).
  The pinned
  [Devin parser](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/src/providers/devin.ts)
  makes the observed aliases reproducible. Neither project is Cognition's
  producer source.
- **Usage and cost:** Message or aggregate metrics can persist prompt,
  completion, and cached tokens. The parser handles multiple observed field
  names; no authoritative provider-reported USD value is consumed, so pricing
  is catalog-derived when model attribution is possible. Transcript JSON is
  written only by an explicit session export, so most sessions have none; for
  those the parser reads per-assistant-message counters from the
  `message_nodes` fallback at `chat_message -> metadata.metrics`
  (`input_tokens`, `output_tokens`, `cache_read_tokens`,
  `cache_creation_tokens`, any of which may be JSON null). `message_nodes` is
  a forest, so totals are summed only along the main chain
  (`sessions.main_chain_id` walked up via `parent_node_id`); summing every row
  double-counts retries and edits. Older databases that predate
  `sessions.main_chain_id` keep that field invalid and fall back to all
  message nodes in creation order. Verified against a live Devin CLI database.
  Each message-node request is attributed to the concrete model at
  `metadata.generation_model` (falling back to the session-level
  `sessions.model` alias), because the session column is often empty or a
  coarse alias. Devin reports the base model with a reasoning-effort or speed
  tier appended (`-thinking`, `-high`, `-medium`, `-low`, `-xhigh`, `-max`,
  and `-*-fast` combinations); those tiers do not change per-token price, so
  the shared pricing resolver strips them to the base model as a last-resort
  match (see `EffortTierBaseModel` in `internal/pricing/normalize.go`). Truly
  opaque names (`adaptive`, `compactor`, `MODEL_PRIVATE_*`, Devin codenames
  such as `claude-5-fable-*`) have no catalog entry and remain unpriced.
- **Agentsview:** `internal/parser/devin.go` and
  `internal/parser/devin_provider.go`; metric aliases are implementation
  evidence because the upstream schema is unavailable.

## Piebald (`piebald`)

- **Format:** An `app.db` SQLite database containing chats, projects, and
  messages.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/Piebald-AI/splitrail.git` at
  `e2f195906dc7bf80d0faf16281cf9544e6413d01`; its first-party
  [Piebald analyzer](https://github.com/Piebald-AI/splitrail/blob/e2f195906dc7bf80d0faf16281cf9544e6413d01/src/analyzers/piebald.rs)
  defines the database location, `chats`/`projects`/`messages` joins, token
  columns, service-tier joins, and normalization. This is a read-only
  first-party schema consumer rather than the application serializer, but it
  is maintained by the product company and directly targets the current store.
- **Usage and cost:** Messages can persist input, output, reasoning, cache-read,
  cache-write, model, and service-tier data. The official analyzer derives
  price from those fields; it does not read a persisted provider USD total.
  Agentsview likewise normalizes the counters and catalog-prices the result.
- **Agentsview:** `internal/parser/piebald.go`.

## Warp (`warp`)

- **Format:** A `warp.sqlite` database whose conversation records include
  transcript metadata and aggregate usage counters.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/warpdotdev/warp.git` at
  `69ce3728acae0b01c2f457b65a90c144664686aa`; see the pinned
  [agent conversation migration](https://github.com/warpdotdev/warp/blob/69ce3728acae0b01c2f457b65a90c144664686aa/crates/persistence/migrations/2025-06-09-013710_create_agent_conversations_table/up.sql),

    [persistence writer](https://github.com/warpdotdev/warp/blob/69ce3728acae0b01c2f457b65a90c144664686aa/app/src/persistence/agent.rs),
    and
    [conversation usage types](https://github.com/warpdotdev/warp/blob/69ce3728acae0b01c2f457b65a90c144664686aa/crates/persistence/src/model.rs).

- **Usage and cost:** The consumed metadata has aggregate `warp_tokens` and
  `byok_tokens` by model and category, plus custom-endpoint tokens and credit
  fields upstream. Agentsview consumes only the Warp/BYOK aggregates; they are
  not attributable per-request billing tokens, cache splits, or reasoning, so
  it reports them as session metrics and does not derive USD from them.

- **Agentsview:** `internal/parser/warp.go` and `internal/parser/warp_paths.go`.

## Positron (`positron`)

- **Format:** VS Code-derived `chatSessions` JSON snapshots or JSONL operation
  logs in Positron workspace storage.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/posit-dev/positron.git` at
  `61345078cc1833b740fda2b1fe1aabc8472d2249`; see
  [chatModel.ts](https://github.com/posit-dev/positron/blob/61345078cc1833b740fda2b1fe1aabc8472d2249/src/vs/workbench/contrib/chat/common/model/chatModel.ts)
  and
  [chatSessionStore.ts](https://github.com/posit-dev/positron/blob/61345078cc1833b740fda2b1fe1aabc8472d2249/src/vs/workbench/contrib/chat/common/model/chatSessionStore.ts).
- **Usage and cost:** The underlying VS Code shape can carry prompt/output
  metadata and model identity, but the Positron provider currently exposes no
  usage events. Cache, reasoning, and monetary cost are therefore absent from
  Agentsview analytics for this provider.
- **Agentsview:** `internal/parser/positron_provider.go` and the shared decoding
  in `internal/parser/vscode_copilot.go`; the lack of usage export is a parser
  limitation, not proof that upstream never records metadata.

## Posit Assistant (`posit-assistant`)

- **Format:** Workspace conversation directories containing `conversation.json`,
  `lm-messages.jsonl`, `ui-messages.jsonl`, and an optional
  `usage-events.jsonl` sidecar of auxiliary per-request usage.
- **Evidence:** `no-public-source`.
- **Upstream:** Posit's product documentation and the
  [posit-dev GitHub organization](https://github.com/posit-dev) were searched
  2026-07-19. Clone the public Positron repository
  `https://github.com/posit-dev/positron.git` at
  `61345078cc1833b740fda2b1fe1aabc8472d2249`; its current tree includes an
  older
  [Copilot conversation store](https://github.com/posit-dev/positron/blob/61345078cc1833b740fda2b1fe1aabc8472d2249/extensions/copilot/src/extension/conversationStore/node/conversationStore.ts),
  but contains no producer for `.posit/assistant/workspaces`,
  `conversation.json`, or `lm-messages.jsonl`. Demo and feedback repositories
  were also public, but no matching producer or authoritative
  persisted-session schema was found.
- **Usage and cost:** Language-model messages can persist input, output,
  cache-read, and cache-write tokens with model identity. Observed GLM, Gemma,
  and Kimi records use the Anthropic-shaped `cacheWriteTokens` field for the
  uncached prompt remainder while leaving `inputTokens` at zero; these model
  families do not expose a separately billed cache-write category in the
  pricing catalog. Claude records retain real cache-write semantics.
  Agentsview catalog-prices these values; no provider-reported USD total is
  consumed. Auxiliary usage that never appears in the transcript —
  cache-keepalive pings and classifier calls — is appended to
  `usage-events.jsonl` as
  `{"type":"usage","kind":"keepalive"|"classifier", "timestamp":…,"anchorMessageId":…,"providerId":…,"modelId":…, "inputTokens":…,"outputTokens":…,"totalTokens":…,"cacheReadTokens":…, "cacheWriteTokens":…}`
  lines; subagent conversations carry their own sidecar. Observed on real
  idle sessions: repeated keepalive pings whose spend is invisible in
  `lm-messages.jsonl`.
- **Agentsview:** `internal/parser/posit_assistant_provider.go`; current schema
  details are based on observed files and fixtures. Reverified 2026-08-22
  against the samples reported in
  [#1466](https://github.com/kenn-io/agentsview/issues/1466): the parser folds
  the persisted cache-write remainder into uncached input only for recognized
  GLM, Gemma, and Kimi model families. Missing and unrecognized model
  identities preserve Posit Assistant's original buckets, and full context
  remains the sum of the persisted input, cache-read, and cache-write fields.
  Data version 91 reparses existing Posit Assistant archives through the
  normal non-destructive resync path. `usage-events.jsonl` lines are ingested
  as request-scoped usage events (`posit-assistant-` + kind) with the same
  model-family token normalization; the sidecar participates in the composite
  fingerprint and changed-path classification so keepalive appends on
  otherwise idle sessions trigger resync. Data version 92 reparses existing
  archives to pick up sidecar spend. Conversations with valid sidecar usage
  are retained even when they have no renderable transcript messages, and
  newer sidecar timestamps extend the session end time.

## Z Code (`zcode`)

- **Format:** A `db.sqlite` database, including a `model_usage` table.
- **Evidence:** `no-public-source`.
- **Upstream:** Z Code's first-party product pages, documentation, and public
  GitHub organization surfaces were searched 2026-07-19. Its
  [usage documentation](https://zcode.z.ai/en/docs/usage-stats) confirms that
  the application reads local ZCode session records and presents token,
  session, message, and model totals, but does not publish the database
  schema. For a reproducible independent schema observation, clone
  `https://github.com/getagentseal/codeburn.git` at
  `3472885629c41725b40c19c0780ecce148b067bf` and inspect the pinned
  [ZCode format notes](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/docs/providers/zcode.md)
  and
  [parser](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/src/providers/zcode.ts).
  No producer migration or source was found.
- **Usage and cost:** `model_usage` rows persist input, output, reasoning,
  cache-creation, cache-read, computed total, and model data. Agentsview emits
  usage events and derives monetary price from its catalog rather than a
  provider-reported USD value.
- **Agentsview:** `internal/parser/zcode.go`; table and column semantics remain
  reverse-engineered implementation evidence.

## Goose (`goose`)

- **Format:** A shared SQLite `sessions.db`. Schema version 15 stores session
  metadata in `sessions`, ordered role messages with tagged JSON content in
  `messages`, and request-scoped token and cost records in `usage_ledger`.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/aaif-goose/goose.git` at
  `5ab0e6df34e69444f6f2016de40717a9f54bf816`; see the pinned

    [session manager](https://github.com/aaif-goose/goose/blob/5ab0e6df34e69444f6f2016de40717a9f54bf816/crates/goose/src/session/session_manager.rs),

    [message model](https://github.com/aaif-goose/goose/blob/5ab0e6df34e69444f6f2016de40717a9f54bf816/crates/goose-provider-types/src/conversation/message.rs),

    [tool-result serialization](https://github.com/aaif-goose/goose/blob/5ab0e6df34e69444f6f2016de40717a9f54bf816/crates/goose-provider-types/src/conversation/tool_result_serde.rs),
    and
    [path resolution](https://github.com/aaif-goose/goose/blob/5ab0e6df34e69444f6f2016de40717a9f54bf816/crates/goose/src/config/paths.rs).

    `Paths::data_dir()` uses etcetera 0.11 `choose_app_strategy` (XDG on macOS and
    Linux; the Windows strategy appends a `data` subfolder under
    `%APPDATA%\Block\goose\`), and `GOOSE_PATH_ROOT` overrides it with
    `<root>/data`. The first-party
    [session-management guide](https://goose-docs.ai/docs/guides/sessions/session-management/)
    and an isolated observed schema-version-15 database were also checked
    2026-08-03.

- **Usage and cost:** `usage_ledger` rows provide model, input, output,
  cache-read, cache-write, compaction, cost, and cost-source data without a
  stable message ordinal. Agentsview emits them as `goose-request`
  request-scoped usage events so aggregate reporting reads the ledger token
  columns without attaching rows to arbitrary messages, and preserves reported
  or estimated costs. Older schemas without the ledger fall back to the
  session's accumulated counters and cost.

- **Agentsview:** `internal/parser/goose.go` and
  `internal/parser/goose_provider.go`; the provider uses per-session content
  fingerprints and bounded SQLite row cursors for watcher events, while a
  periodic full reconciliation covers metadata-only edits and row deletes.

## Zed (`zed`)

- **Format:** `threads/threads.db`, whose thread payload is JSON or zstd-
  compressed JSON depending on generation.
- **Compatibility:** Agentsview accepts the legacy five-column `threads` table
  (`id`, `summary`, `updated_at`, `data_type`, and `data`). Modern lineage and
  metadata columns are optional; a present `parent_id` continues to exclude
  child threads.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/zed-industries/zed.git` at
  `f14fea9bf3c93797d5161f7440ed418655bc6c57`; see
  [thread_store.rs](https://github.com/zed-industries/zed/blob/f14fea9bf3c93797d5161f7440ed418655bc6c57/crates/agent/src/thread_store.rs)
  and
  [thread.rs](https://github.com/zed-industries/zed/blob/f14fea9bf3c93797d5161f7440ed418655bc6c57/crates/agent/src/thread.rs).
- **Usage and cost:** Thread metadata can persist aggregate input and output
  token usage with model identity. It does not provide per-message cache or
  reasoning splits in the consumed shape. Agentsview emits one aggregate usage
  event and catalog-prices it.
- **Agentsview:** `internal/parser/zed.go`, `internal/parser/zed_helpers.go`,
  and `internal/parser/zed_provider.go`.

## Antigravity IDE (`antigravity`)

- **Format:** Per-session SQLite databases, optionally supplemented by
  trajectory JSON sidecars.
- **Evidence:** `no-public-source`.
- **Upstream:** Google's first-party Antigravity product and documentation
  surfaces and public repositories were searched 2026-07-19; no application
  database schema or protobuf definition for `gen_metadata` was published. For
  an independent implementation that queries Antigravity's local
  language-server RPC and documents the protobuf-derived token fields, clone
  `https://github.com/getagentseal/codeburn.git` at
  `3472885629c41725b40c19c0780ecce148b067bf` and inspect its
  [Antigravity format notes](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/docs/providers/antigravity.md)
  and
  [parser](https://github.com/getagentseal/codeburn/blob/3472885629c41725b40c19c0780ecce148b067bf/src/providers/antigravity.ts).
- **Usage and cost:** Heuristically decoded generation metadata or sidecars
  provide uncached input, output (including thinking), cache-read, and model
  data. There is no separate reliable reasoning counter or reported USD cost;
  Agentsview catalog-prices tokens. Decode failures are surfaced explicitly.
- **Agentsview:** `internal/parser/antigravity.go`,
  `internal/parser/antigravity_proto.go`, and
  `internal/parser/antigravity_provider.go`; field decoding is deliberately
  marked as reverse engineering.

## Antigravity CLI (`antigravity-cli`)

- **Format:** Newer per-session SQLite databases or older encrypted protobuf
  files, with trajectory/history/brain sidecars when present.
- **Evidence:** `no-public-source`.
- **Upstream:** Google's Antigravity product documentation and public
  repositories were searched 2026-07-19; no CLI persistence source, encryption
  specification, or authoritative protobuf schema was found. The independent
  CodeBurn evidence pinned in the `antigravity` entry also covers CLI
  discovery, live RPC metadata, and the shorter capture window, but not the
  encrypted producer format. Reverified 2026-08-20 against Google's official
  [Antigravity CLI 1.1.16 macOS ARM64 release](https://github.com/google-antigravity/antigravity-cli/releases/tag/1.1.16),
  asset SHA-256
  `a902e8b24fdc53504e7daf658e18f7ba47ebb9cfcf058aa6c01e37c5e610d389`. Its Go
  binary embeds `FileDescriptorProto` records for
  `third_party/jetski/cortex_pb/cortex.proto` and
  `third_party/jetski/codeium_common_pb/codeium_common.proto`. These compiled
  descriptors name the fields consumed below, but the persistence writer
  remains closed source, so the evidence class remains `no-public-source`.
- **Usage and cost:** SQLite `gen_metadata` and trajectory sidecars can carry
  input, output, thinking-output, cache-read, and model fields; output already
  includes thinking. In CLI 1.1.5 SQLite,
  `CortexStepGeneratorMetadata.step_indices` (field 2) contains packed step
  indices and `ChatModelMetadata.response_model` (field 19) can contain only
  the base model slug. The matching `ExecutorMetadata.last_step_idx` range
  carries the effort-qualified model at
  `cascade_config.planner_config.model_name` (fields 10, 1, and 28).
  `ChatModelMetadata.model_display_name` (field 21) remains the complete label
  when present. Agentsview avoids double counting and catalog-prices usage. No
  provider USD cost is consumed.
- **Agentsview:** `internal/parser/antigravity_cli.go`,
  `internal/parser/antigravity_crypto.go`, and
  `internal/parser/antigravity_cli_provider.go`.

## iFlow CLI (`iflow`)

- **Format:** Claude-like JSONL with UUID/parent UUID links and streaming
  message records.
- **Evidence:** `no-public-source`.
- **Upstream:** The public
  [iFlow CLI repository](https://github.com/iflow-ai/iflow-cli) at
  `4642808afbc6580ac117d930f6c64ac0d84955c7` and its first-party documentation
  were checked 2026-07-19. The repository publishes documentation and release
  material but no usable session persistence implementation or schema. As
  independent compatible-format evidence, clone
  `https://github.com/chenhg5/tape.git` at
  `c40d46d16a32295da63221629293a000b0675df2` and inspect its pinned
  [iFlow source adapter](https://github.com/chenhg5/tape/blob/c40d46d16a32295da63221629293a000b0675df2/internal/source/iflow/iflow.go),
  which records discovery paths and delegates the observed wire shape to its
  Gemini-family parser.
- **Usage and cost:** Although records may resemble Claude streaming events,
  Agentsview does not expose token, cache, reasoning, credit, or USD
  accounting for iFlow.
- **Agentsview:** `internal/parser/iflow.go` and
  `internal/parser/iflow_provider.go`; field interpretation is based on
  observed files rather than upstream authority.

## ICodeMate (`icodemate`)

- **Format:** Two storage families under one agent, matched by on-disk layout:
  the VSCode-extension OpenCode-compatible SQLite or legacy
  session/message/part storage, and the terminal CLI Claude-format projects
  JSONL (`<projectsRoot>/<project>/<session>.jsonl`).
- **Evidence:** `no-public-source`.
- **Upstream:** ICodeMate's first-party product pages, documentation, and public
  GitHub repository search were checked 2026-07-19 without finding producer
  source or an authoritative disk schema. The OpenCode source pinned in the
  `opencode` entry is compatible-family evidence for the VSCode path only; the
  terminal CLI path's Claude-format transcript schema (type/user/assistant
  records carrying uuid, parentUuid, sessionId, cwd, gitBranch, timestamp, and
  message.usage token fields) is compatible-family evidence from the `claude`
  entry.
- **Usage and cost:** Compatible messages can persist input, output, cache-read,
  cache-write, and model identity. Agentsview catalog-prices these values and
  consumes no product-reported USD total.
- **Agentsview:** `internal/parser/icodemate.go` delegates to
  `internal/parser/opencode.go` for the VSCode OpenCode path;
  `internal/parser/icodemate_cli.go` parses the Claude-format CLI projects
  transcripts, and `internal/parser/icodemate_provider.go` fans the configured
  roots out to whichever layout each root owns. Product-specific divergence is
  a known limitation. Reverified 2026-08-22 with controlled compatible-format
  fixtures: the CLI parser uses the Claude UUID/parent UUID graph, coalesces
  repeated assistant message snapshots by message ID, and resolves local and
  S3-materialized persisted tool-result sidecars before extracting content.
  Local sidecar writes map back to their owning transcript and participate in
  its content-based, extraction-root-independent freshness fingerprint. Remote
  imports resolve persisted paths against their extracted sidecars before
  parsing. CLI project attribution uses transcript cwd and branch metadata to
  resolve repository subdirectories and managed worktrees. Transcript identity
  is the filename session ID across configured roots, so duplicate or moved
  copies reconcile as one `icodemate:` session. Branch reconciliation follows
  that identity across prior and current source paths and archive rebuilds. S3
  subagent refreshes retain archived parent provider and machine metadata
  instead of importing ICodeMate children as Claude. S3 discovery also
  preserves transcript-only size and mtime separately from composite sidecar
  freshness. A trailing partial local or S3 JSONL record stays incomplete and
  retryable without replacing archived branch content. Polling includes local
  and S3 sidecar mtimes, shortened CLI transcripts replace archived messages,
  and duplicate ranking uses transcript metadata rather than sidecar volume.
  Complete CLI parses reconcile the transcript's current branch membership,
  and source freshness is recorded only after every emitted branch commits;
  unchanged S3 transcripts use that persisted all-branch state to skip object
  downloads.

## WorkBuddy (`workbuddy`)

- **Format:** Session JSONL with provider-specific raw usage embedded under
  message provider data.
- **Evidence:** `no-public-source`.
- **Upstream:** WorkBuddy's first-party product site, documentation, and public
  repositories were searched 2026-07-19; no authoritative persistence producer
  or versioned schema was found. For reproducible independent format and
  accounting evidence, clone `https://github.com/mm7894215/TokenTracker.git`
  at `eaf6048b07729f3ae1224def6011ea22f80cd035` and inspect its pinned
  [WorkBuddy reader](https://github.com/mm7894215/TokenTracker/blob/eaf6048b07729f3ae1224def6011ea22f80cd035/src/lib/rollout.js),
  which documents the recursive JSONL layout, raw usage variants, cache and
  reasoning normalization, model fallback, and newer `workbuddy.db` aggregate
  fallback. These are consumer observations, not Tencent authority.
- **Usage and cost:** Usage may contain input, output, cache, and reasoning
  counters. Upstream prompt totals include cache, so Agentsview subtracts
  cache to obtain uncached input and keeps reasoning separate. Monetary cost
  is catalog-derived.
- **Agentsview:** `internal/parser/workbuddy.go` and
  `internal/parser/workbuddy_provider.go`; counter semantics are
  implementation evidence.

## Zencoder (`zencoder`)

- **Format:** Per-session JSONL transcripts.
- **Evidence:** `no-public-source`.
- **Upstream:** The first-party
  [Zencoder documentation](https://docs.zencoder.ai/) and public repositories
  were searched 2026-07-19. Zencoder publishes an organization-level
  [Analytics API](https://docs.zencoder.ai/features/analytics-api), but it
  does not document the local JSONL transcript or its fields. No local
  transcript serializer or authoritative schema was found.
- **Usage and cost:** The consumed JSONL exposes no reliable token, cache,
  reasoning, credit, or monetary-cost fields to Agentsview.
- **Agentsview:** `internal/parser/zencoder.go` and
  `internal/parser/zencoder_provider.go`.

## gptme (`gptme`)

- **Format:** Conversation `conversation.jsonl` files containing typed message
  records and metadata.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/gptme/gptme.git` at
  `a1d8ca21dd662e04970ff36c8c3e9b342f989605`; see
  [conversations.py](https://github.com/gptme/gptme/blob/a1d8ca21dd662e04970ff36c8c3e9b342f989605/gptme/logmanager/conversations.py)
  and
  [message.py](https://github.com/gptme/gptme/blob/a1d8ca21dd662e04970ff36c8c3e9b342f989605/gptme/message.py).
- **Usage and cost:** Assistant metadata can persist input, output, cache-read,
  and cache-creation tokens with model data. Agentsview catalog-prices the
  normalized usage and consumes no authoritative persisted USD total.
- **Agentsview:** `internal/parser/gptme.go` and
  `internal/parser/gptme_provider.go`.

## Qoder (`qoder`)

- **Format:** Project JSONL transcripts, `-session.json` metadata, and related
  subagent artifacts.
- **Evidence:** `no-public-source`.
- **Upstream:** The first-party [Qoder documentation](https://docs.qoder.com/)
  and public repositories were searched 2026-07-19; no producer-side session
  serializer or authoritative local schema was found. The official scoped npm
  package currently names a GitHub repository that is not publicly clonable.
  For independent reproducible evidence, clone
  `https://github.com/chenhg5/tape.git` at
  `c40d46d16a32295da63221629293a000b0675df2` and inspect its pinned
  [Qoder source adapter](https://github.com/chenhg5/tape/blob/c40d46d16a32295da63221629293a000b0675df2/internal/source/qoder/qoder.go),
  which documents the transcript/metadata pair and shared Qwen `ChatRecord`
  shape. Agentsview issue
  [#1405](https://github.com/kenn-io/agentsview/issues/1405), checked
  2026-08-28, reports Qoder CLI CN 1.1.21 storing the same project-scoped JSONL
  family under `~/.qoder-cn/projects/<project-slug>/`, including
  `<session-id>.jsonl`. This is a user-reported local observation, not
  producer-side evidence, and does not establish storage behavior for all
  Qoder CN releases or platforms.
- **Usage and cost:** The consumed files provide transcript and model/session
  metadata but no authoritative token, cache, reasoning, credit, or USD events
  to Agentsview.
- **Agentsview:** `internal/parser/qoder.go` and
  `internal/parser/qoder_provider.go`.

## QwenPaw (`qwenpaw`)

- **Format:** Workspace `sessions/<name>.json` documents whose
  `agent.memory.content` holds message/content-block pairs.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/agentscope-ai/QwenPaw.git` at
  `a15a69fca73e67c17dc47326e933eaa259fa0d8d`; see the context
  [serializer](https://github.com/agentscope-ai/QwenPaw/blob/a15a69fca73e67c17dc47326e933eaa259fa0d8d/src/qwenpaw/agents/context/scroll/serialize.py)
  and
  [history implementation](https://github.com/agentscope-ai/QwenPaw/blob/a15a69fca73e67c17dc47326e933eaa259fa0d8d/src/qwenpaw/agents/context/scroll/history.py).
- **Usage and cost:** The consumed session memory contains messages and content
  blocks but no per-message billing usage. QwenPaw has separate token-usage
  services, but Agentsview does not join that accounting store to session
  files; cache, reasoning totals, and USD cost are therefore absent.
- **Agentsview:** `internal/parser/qwenpaw.go` and
  `internal/parser/qwenpaw_provider.go`.

## Shelley (`shelley`)

- **Format:** A `shelley.db` SQLite database containing conversations, messages,
  and JSON usage data.
- **Evidence:** `documentation`.
- **Upstream:** The first-party
  [Shelley launch and storage documentation](https://blog.exe.dev/shelley) was
  checked 2026-07-19 and identifies the SQLite store at
  `~/.config/shelley/shelley.db`. No public migration, table schema, or
  producer source was found, so column-level details remain observed evidence.
- **Usage and cost:** `usage_data` can persist input, cache-creation,
  cache-read, output, model, and exact `cost_usd`. Agentsview intentionally
  ignores `cost_usd` while emitting token usage, avoiding mixed/double cost
  attribution and using catalog pricing instead.
- **Agentsview:** `internal/parser/shelley.go` and
  `internal/parser/shelley_provider.go`; schema and cost-field behavior are
  observed implementation evidence.

## Mistral Vibe (`vibe`)

- **Format:** A session directory containing `messages.jsonl` and `meta.json`.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/mistralai/mistral-vibe.git` at
  `0685654a40a4035966891289065379a751a7e617`; see
  [session_logger.py](https://github.com/mistralai/mistral-vibe/blob/0685654a40a4035966891289065379a751a7e617/vibe/core/session/session_logger.py)
  and
  [history_manager.py](https://github.com/mistralai/mistral-vibe/blob/0685654a40a4035966891289065379a751a7e617/vibe/cli/history_manager.py).
- **Usage and cost:** Metadata stores aggregate session prompt/completion and
  context/last-turn/total statistics, without per-message cache or cost data.
  Agentsview emits one aggregate usage event and catalog-prices it when model
  identity is available.
- **Project identity:** Metadata records `session_id`, `git_branch`, and
  `environment.working_directory`. Agentsview recovers those independent
  fields even when another optional metadata field is malformed, so a partial
  parse cannot replace repository classification with generic fallbacks.
- **Agentsview:** `internal/parser/vibe.go` and
  `internal/parser/vibe_provider.go`.

## Aider (`aider`)

- **Format:** Repository-local `.aider.chat.history.md`; multiple runs can be
  reconstructed from one Markdown history.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/Aider-AI/aider.git` at
  `5dc9490bb35f9729ef2c95d00a19ccd30c26339c`; see
  [history.py](https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/history.py)
  and the first-party
  [usage documentation](https://github.com/Aider-AI/aider/blob/5dc9490bb35f9729ef2c95d00a19ccd30c26339c/aider/website/docs/usage.md).
- **Usage and cost:** The Markdown transcript does not persist authoritative
  per-message tokens, cache, reasoning, credits, or USD cost. Aider may
  display runtime cost elsewhere, but Agentsview does not infer it from this
  history.
- **Agentsview:** `internal/parser/aider.go` and
  `internal/parser/aider_provider.go`; roles and run boundaries are
  reconstructed from Markdown.

## Poolside Agent CLI (`poolside`)

- **Format:** Single NDJSON trajectory file per session under
  `<root>/trajectories/trajectory-<type>_<uuid>.ndjson`. Events include
  `session.start`, `session.input`, `assistant_message.start/end`,
  `tool_call.parsed`, `tool_call.result`, `thought.start/end`, and
  `tool_call.inference.start/end`.
- **Evidence:** `documentation`.
- **Upstream:** The public
  [pool release repository](https://github.com/poolsideai/pool) (README,
  changelog, and third-party notices only; no source code) and the
  [Poolside Agent CLI documentation](https://docs.poolside.ai/cli/pool) were
  checked 2026-07-23. Upstream confirms sessions are saved automatically and
  that per-session trajectory files exist (`pool config` prints the trajectory
  directory; `pool history trajectories` browses them), but publishes neither
  the on-disk paths nor the NDJSON event schema. The event format was
  characterized from real trajectory files.
- **Usage and cost:** Per-inference token counts (`input_tokens`,
  `output_tokens`, `cache_read_input_tokens`, `cache_write_input_tokens`) are
  persisted in `tool_call.inference.end` events. The model is recorded in
  `tool_call.inference.start` and paired by `step_id`. No authoritative USD
  cost is persisted; Agentsview computes cost from its pricing catalog.
- **Agentsview:** `internal/parser/poolside.go` and
  `internal/parser/poolside_provider.go`; single-file provider with NDJSON
  line-by-line parsing.

## Reasonix (`reasonix`)

- **Format:** Session JSONL plus `.jsonl.meta` sidecars across live, archive,
  project, and subagent roots.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/esengine/DeepSeek-Reasonix.git` at
  `2301e24827bf62c7584f34c4f541c432dd4f6e0b`; see
  [session.go](https://github.com/esengine/DeepSeek-Reasonix/blob/2301e24827bf62c7584f34c4f541c432dd4f6e0b/internal/agent/session.go)
  and
  [session content](https://github.com/esengine/DeepSeek-Reasonix/blob/2301e24827bf62c7584f34c4f541c432dd4f6e0b/internal/agent/session_content.go).
- **Usage and cost:** The consumed session records do not currently yield
  authoritative per-message token, cache, reasoning, credit, or monetary-cost
  events to Agentsview.
- **Agentsview:** `internal/parser/reasonix.go` and
  `internal/parser/reasonix_provider.go`; discovery spans multiple roots and
  uses metadata sidecars for identity.

## Omnigent (`omnigent`)

- **Format:** A shared SQLite `chat.db` containing conversations and ordered
  conversation items, with session metadata and usage stored alongside each
  conversation.
- **Evidence:** `source`.
- **Upstream:** The first-party
  [database documentation](https://omnigent.ai/docs/deploy/database)
  identifies SQLite `chat.db` as the local persistence store and was checked
  2026-07-27. Clone `https://github.com/omnigent-ai/omnigent.git` at
  `61fd72350ea4c4aba776fbc01c40774079d352e8`. The pinned
  [conversation schema](https://github.com/omnigent-ai/omnigent/blob/61fd72350ea4c4aba776fbc01c40774079d352e8/omnigent/db/db_models.py),
  and
  [store decoding](https://github.com/omnigent-ai/omnigent/blob/61fd72350ea4c4aba776fbc01c40774079d352e8/omnigent/stores/conversation_store/sqlalchemy_store.py)
  describe persistence. The current schema indexes conversation changes by
  `(workspace_id, archived, updated_at, id)` rather than a bare `updated_at`
  index. `session_usage` lives on `omnigent_conversation_metadata`, and both
  `set_session_usage` and `increment_session_usage` update that metadata row
  without changing `conversations.updated_at`. The metadata table has runner
  and project lookup indexes but no modification timestamp or change index.
  Consequently, the immediate filesystem-event sync can defer a metadata-only
  edit. The next scheduled reconciliation pass, an explicit resync, or an
  archive audit reparses the whole changed container and is not limited to a
  bounded candidate set, so it picks up the edit regardless of how long ago it
  was made. The pinned
  [message entity](https://github.com/omnigent-ai/omnigent/blob/61fd72350ea4c4aba776fbc01c40774079d352e8/omnigent/entities/conversation.py)
  and
  [deterministic benchmark seeder](https://github.com/omnigent-ai/omnigent/blob/61fd72350ea4c4aba776fbc01c40774079d352e8/dev/benchmarks/omnigent/seed.py)
  were inspected. The seeder runs the Alembic lineage to head before
  inserting model-backed rows. Agentsview supports two schema generations
  observed at that lineage head: the split text-ID generation (session
  metadata in `omnigent_conversation_metadata`, model overrides in a separate
  `agent_configuration` table) and the current split binary-UUID generation
  (16-byte `BLOB` ids, `session_overrides` JSON on `conversations`). The
  earlier single-table generation, where session metadata columns (including
  `kind`) lived directly on `conversations` with no separate metadata table,
  predates that split and is detected-unsupported: Agentsview fails closed
  with a nonfatal `ErrOmnigentUnsupportedSchema`, skips the container, and
  preserves any archive rows already synced from it.
- **Regeneration:** From that checkout, run
  `uv run dev/benchmarks/omnigent/seed.py --database-uri sqlite:////absolute/temp/path/chat.db --sessions 3 --items-per-session 4 --projects 1 --filed-fraction 1`,
  then set `OMNIGENT_SOURCE_DB` to the generated file for the opt-in parser
  test. Never use a live Omnigent data directory.
- **Usage and cost:** Session usage can contain input and output tokens,
  per-model breakdowns, and an optional authoritative USD total. An absent
  cost remains unset so Agentsview can use catalog pricing.
- **Agentsview:** `internal/parser/omnigent.go` and
  `internal/parser/omnigent_provider.go`; fixtures under
  `internal/parser/testdata/omnigent/` provide observed event-shape evidence.

## Codebuff (`codebuff`)

- **Format:** Per-session JSON files under
  `<root>/<project>/chats/<timestamp>/`. Each session directory contains
  `chat-messages.json` (JSON array of user/ai/error message objects with text,
  tool, agent, mode-divider, plan, ask-user, and image blocks),
  `run-state.json` (agent type, context token count, credits used, cwd, and
  skill catalog), and optional `chat-meta.json` (message count, first prompt,
  and messages size). Freebuff sessions share the same layout and are
  distinguished by the `agentType` field containing `"free"`.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/CodebuffAI/codebuff.git` at
  `b285b562b9ef3a3f35272ed32718eeb74dd86283`; see
  [chat.ts](https://github.com/CodebuffAI/codebuff/blob/b285b562b9ef3a3f35272ed32718eeb74dd86283/cli/src/types/chat.ts)
  for the `ChatMessage` and `ContentBlock` type definitions that define the
  on-disk format, and
  [session-state.ts](https://github.com/CodebuffAI/codebuff/blob/b285b562b9ef3a3f35272ed32718eeb74dd86283/common/src/types/session-state.ts)
  for the `AgentState` type that defines `contextTokenCount` and
  `creditsUsed`. Freebuff shares the same layout and is distinguished by the
  `agentType` field in `run-state.json`.
- **Usage and cost:** The `contextTokenCount` field in `run-state.json` provides
  context window token counts (updated per API step). The `creditsUsed` and
  `directCreditsUsed` fields provide session-level billing totals (1 credit =
  $0.01). The `agentType` field records the agent template name (e.g.
  `base2-deepseek`, `base2-free-mimo`), which encodes the model family but is
  not the actual LLM model -- the real model is selected server-side and can
  change mid-session; mid-session model switches are not detectable from the
  on-disk format. Per-message token breakdown (input/output/cache) is not
  available; only context window size and billing credits are persisted.
  Freebuff (free tier) has no credits -- it is ad-supported with daily session
  limits.
- **Agentsview:** `internal/parser/codebuff.go` and
  `internal/parser/codebuff_provider.go`; single-file provider with JSON array
  parsing.
