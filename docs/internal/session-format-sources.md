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

- **Performance fixture check (2026-09-04):** Rechecked the pinned Codeburn
  format notes below for project-scoped JSONL. `cmd/perfsim` uses the shared
  Claude fixture builder to emit user/assistant pairs with message/request
  identities and usage, then checks actual parsed counts. These synthetic
  records are a measured subset, not an authoritative or exhaustive schema.

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
  replayed entries. Evidence remains `no-public-source`. Reverified 2026-09-10
  against local parser fixtures: equal-length replay candidates with the same
  background flag leave lineage unresolved when their complete UUID sets
  differ. Identical sets elect the smallest stem, retaining one copy across
  three background transcripts. An interactive original still wins a tie with
  a background sibling. Reverified 2026-09-11 against the local lineage
  fixtures: replacing the background marker or root UUID in a same-size
  transcript updates the parsed lineage after its file-change timestamp
  advances, even when its modification time is restored. Reverified 2026-08-16
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
  refuses to seal when the wrapper did not durably record execution
  completion, even if the transcript is temporarily quiescent and receives
  more usage later. Reverified 2026-08-21 that exact token projection compares
  canonical output and context coverage separately for every included session,
  while crediting a deduplicated snapshot to each source session that
  contained its equivalent row. It also requires the materialized breakdown
  length to match its recorded count. A larger context row from another
  session therefore cannot hide missing delegated input or cache usage.
  Reverified 2026-08-22 that an incomplete category breakdown, malformed
  included transcript, or unfinished included session withholds computed or
  mixed cost; provider-reported cost remains authoritative. Reverified
  2026-08-27 that raw-capture membership mirrors persisted tool output
  resolution: it includes regular files at any depth in the session's
  `tool-results/` directory and, for subagents, the enclosing parent session's
  `tool-results/` directory. These immutable companions are captured with the
  appendable transcript so a reconstructed tree preserves the parser's
  physical inputs. Reverified 2026-09-10 that hosted tool parsing derives
  skill names from recorded paths without consulting worker-local `SKILL.md`
  frontmatter or the local parse cache; local parsing retains frontmatter
  lookup. `TestHostedSkillInferenceKeepsNamesLexical` covers this boundary.

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

- **Tool-result image check (2026-09-08):** Reverified the pinned
  [output payload types and array tests](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/protocol/src/models.rs).
  `function_call_output.output` accepts a string or a content-item array.
  `FunctionCallOutputContentItem::InputImage` serializes as `input_image` with
  a string `image_url`; upstream's MCP conversion tests cover inline
  `data:image/png;base64,...` URLs alongside `input_text` blocks. The
  archive's drop policy projects supported inline images in these stored
  arrays, including staged full imports and late results. Claude's text-block
  decoding is unchanged; this evidence does not establish Claude emitting
  Codex image blocks. Reverified archive summary handling on 2026-09-09
  against `summarizeToolCallFromStateTx`: multiple named results get agent
  labels; an anonymous result is appended without a label. The image
  projection handles both forms, including JSON arrays with internal blank
  lines. These summary labels are added by Agentsview, not by the provider.

- **Performance fixture check (2026-09-04):** Rechecked the pinned rollout
  recorder below for session metadata and rollout-item persistence.
  `cmd/perfsim` generates dated rollouts with session metadata, turn context,
  response items and token-count events through the shared fixture builder.
  Its integration test checks parsed messages and aggregate output tokens.

- **Format:** Rollout JSONL files, with a separate JSONL session index used by
  older releases for discovery and metadata. Current releases no longer write
  `session_index.jsonl`; thread titles live in `thread_history_*.sqlite`
  databases that agentsview does not read, so an absent index is the normal
  state, not a rename signal (reverified 2026-08-13 against a live `~/.codex`
  with no `session_index.jsonl` and a populated `thread_history_1.sqlite`).
  The TUI also maintains an append-oriented `history.jsonl` whose records
  contain `session_id`, Unix-seconds `ts`, and submitted prompt `text`;
  configured size enforcement can rewrite a retained tail in place. Agentsview
  consumes only the first two fields as a live-activity hint. Subagent
  rollouts carry a structural `source.subagent` marker and a top-level
  `parent_thread_id`; that pair defines the parent edge. `thread_source` is a
  legacy fallback, and `session_id` identifies the root or tree rather than
  the parent.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/openai/codex.git` at
  `406dc9239492aff6d295cca5eebe2a548548d42f`; see the pinned
  [rollout recorder](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/rollout/src/recorder.rs)
  and
  [protocol types](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/protocol/src/protocol.rs).
  The current producer source at commit
  `a44454656459437fc8e2ffa9eca0646537b1fdfd` keeps the subagent source marker
  and parent while serializing guardian reviews as `guardian_review`; see
  [codex_delegate.rs](https://github.com/openai/codex/blob/a44454656459437fc8e2ffa9eca0646537b1fdfd/codex-rs/core/src/codex_delegate.rs#L76-L110).
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
  emits. Codex Luna Reserve turns persist `turn_context.payload.model` as
  `gpt-reserve`. That reported name is stored unchanged; pricing resolves it
  to the `gpt-5.6-luna` catalog row (Luna list rates, not an OpenAI invoice).
  An exact `[custom_model_pricing."gpt-reserve"]` row still wins. Reverified
  2026-09-06 against OpenAI's Luna Reserve help article
    <https://help.openai.com/en/articles/20001499-luna-reserve-in-codex-and-chatgpt-work>
    and Codex `turn_context` model seeding in `internal/parser/codex.go`. With
    the `amazon-bedrock` provider, Codex reports `openai.gpt-5.4`,
    `openai.gpt-5.6-luna`, `openai.gpt-5.6-terra`, and `openai.gpt-6-astra`.
    Reverified 2026-09-10 against Codex's
    [provider model IDs](https://github.com/openai/codex/blob/713caa89f389acd9cbcd77016edbb607273826af/codex-rs/model-provider-info/src/lib.rs#L45-L53)
    and
    [Bedrock catalog](https://github.com/openai/codex/blob/713caa89f389acd9cbcd77016edbb607273826af/codex-rs/model-provider/src/amazon_bedrock/catalog.rs).
    Agentsview retains these reported names and maps them to the corresponding
    `bedrock_mantle/openai.gpt-*` pricing names. Only the full reported IDs
    match these aliases; provider- and region-qualified catalog keys pass
    through unchanged. Exact custom pricing still wins. For timestamped usage,
    GenAI Prices' `aws/openai.*` entries take precedence over flat LiteLLM rows
    when available, including Luna and Terra prices before the 2026-07-30
    reduction. Usage without a valid timestamp uses the flat catalog. The
    embedded GenAI document uses the pinned
    [AWS price history](https://github.com/pydantic/genai-prices/blob/83a49e8b386176a1e28e9d9aedeea5e2b4abc586/prices/providers/aws.yml).
    The pinned LiteLLM snapshot predates Astra. A temporary supplemental
    `bedrock_mantle/openai.gpt-6-astra` row uses $11 input, $55 output,
    $13.75 cache write, and $1.10 cache read per million tokens; above 272k
    input tokens these become $22, $82.50, $27.50, and $2.20. Reverified against
    [LiteLLM's Bedrock row](https://github.com/BerriAI/litellm/blob/fbed17d567a62b14b8fc7d9ef13c5cd61a8d1ae0/model_prices_and_context_window.json).
    Remove that supplemental row when the shared snapshot includes it.

- **Agentsview:** `internal/parser/codex.go` and
  `internal/parser/codex_provider.go`; usage is taken from the last-turn
  counters rather than repeatedly counting cumulative totals. Fork and
  subagent rollouts can begin with a re-stamped copy of the parent's
  transcript, including its `token_count` records. Agentsview follows the
  explicit parent id, compares the ordered `turn_context.turn_id` sequence as
  opaque identifiers, and discards the leading turns also present in the
  parent. UUID versions and identifier bytes carry no chronological meaning;
  the first turn id absent from the parent begins child-owned usage. Missing
  parents fail open, and child-only subagent transcripts are left unchanged. A
  local corpus measured 2026-09-07 contained 2,044 Codex JSONL files, with
  1,565 carrying `source.subagent` and none carrying `guardian_review`; the
  published producer source supplies the guardian format evidence. Legacy
  `session_index.jsonl` files from aliased homes also travel through remote
  archive export and import. Reverified on 2026-09-07 with
  `TestRemoteCodexAliasTitleSurvivesArchiveImport`, which checks the imported
  title while another provider retains its own metadata configuration.
  Reverified hosted replay on 2026-09-10 with
  `TestProviderParserHostedCodexAliasHomeMetadata`: captured primary and alias
  indexes merge by their original modification times, with later configured
  homes winning ties. Missing indexes preserve the remaining aliases' logical
  paths and precedence, including generations without a primary index.
  `TestProviderParserHostedCodexSkillNameStaysLexical` also verifies that hosted
  skill inference ignores worker-local frontmatter and cached local names. A
  bare `SKILL.md` reference without a lexical skill name remains unnamed in
  hosted replay. Metadata paths are resolved at configuration load and belong
  to provider instances; imports do not change process-wide configuration. S3
  imports list the child's configured Codex root for its explicitly named
  parent and materialize only that one parent beside the child. When the
  parent is not yet available, the child remains visible but is stored below
  the current data version so a later unchanged-object sync retries and
  corrects the overcount. Reverified 2026-08-13 against the materialized-S3
  parser-to-SQLite path: the first missing-parent pass kept replayed content
  as retryable, and the next pass fetched only the named parent and replaced
  it with child-owned messages and usage. A readable parent with no turns
  resolves as current, whether or not the child carries copied parent metadata.
  Reverified 2026-09-06 against the provider parse path with both metadata
  shapes when integrating the single-pass retry gate with the turnless-parent
  fix from #1578. Reverified 2026-09-10 that raw capture also preserves an
  orphaned child's full transcript when its named parent is unavailable,
  matching local parsing. When available, the explicitly named parent travels
  with the captured fork so hosted parsing applies the local replay boundary.
  Reverified on 2026-09-10 with
  `TestProviderParserHostedParseMatchesLocalCodexForkLineage`: parents in
  other configured homes, archives, and custom roots also travel with the
  child. Capture keeps local root precedence when roots contain differing
  parent copies; hosted replay discovers the selected external parent in its
  own flat root. An appended `session_meta` after an incremental-sync offset
  forces an authoritative replacement of that derived session, because the
  metadata can be the copied parent record that activates replay filtering.
  The original parent session remains valid and is not reparsed. Reverified
  2026-08-12 against locally observed multi-agent rollouts that replayed
  differently shaped opaque turn ids before the first child-owned turn, and
  against the pinned format sources; the pinned TUI is the evidenced
  `history.jsonl` producer. No `append_entry` producer call exists under the
  pinned `app-server` or `exec` trees, so this evidence does not establish
  IDE, desktop, or `codex exec` activity-hint coverage. Locally observed Codex
  app builds can write the same schema, but that is observational evidence
  rather than a public compatibility guarantee. A missing
  `session_index.jsonl` is verified as normal absence; read or scan failures
  remain unverified and cannot earn persisted freshness trust, so a transient
  failure cannot pin a stale stored title. Agentsview derives the hint path as
  `<configured-sessions-root>/../history.jsonl`; a custom sessions root
  without that sibling, or `HistoryPersistence::None`, degrades to ordinary
  watcher behavior, degraded-coverage polling when applicable, and the daily
  archive audit. Restart bootstrap reads at most the newest 4 MiB and accepts
  records from the preceding 24 hours. If a daemon restarts during a longer
  autonomous run whose last prompt falls outside those bounds, the rollout
  relies on those fallbacks until its next prompt. Reverified 2026-08-16 with
  Codex CLI 0.147.0: `codex exec --json` emitted a `thread.started` record
  carrying one UUID, followed by turn and item records and a terminal usage
  record, while its dated rollout began with a `session_meta.id` equal to that
  UUID and ended with `task_complete`. One-shot capture therefore accepts only
  this structured mode, tees its bytes without interpreting formatted stderr,
  and validates the ID against filenames and `session_meta` inside the
  wrapper-start local and UTC days, each plus or minus one day. It copies and
  ingests that exact rollout first, then uses parsed `spawn_agent` links and
  their message timestamps to repeat the same bounded day-shard lookup around
  each child's spawn time. Final accounting uses only the provider-shaped
  copies in the capture directory. Malformed JSONL records are counted on both
  root and delegated sessions so one-shot capture marks otherwise usable
  accounting as partial instead of silently treating the transcript as complete.
  Reverified 2026-08-20 that this includes an unterminated invalid final
  record after `task_complete`; ordinary live parsing still defers that tail
  while its writer can complete it. This bounded lookup is deliberately
  separate from the provider's general full-archive UUID discovery. Hosted raw
  discovery and event-driven capture preserve each physical transcript under
  its configured root; duplicate ranking remains limited to normalized
  discovery. Reverified 2026-08-29 with live and archived copies sharing one
  UUID.

- **HTTP import verification (2026-09-07):**
  `TestRemoteCodexAliasTitleSurvivesArchiveImport` also checks that unrelated
  indexes cannot override explicit or empty metadata associations.
  `TestHTTPMirrorCodexIndexRemoval` exercises persisted mirror deletion,
  truncation, home removal, and journal replay. Remaining indexes supply the
  title; absence of all titles preserves the stored name.

- **Import-path parity reverified 2026-09-07:** staged and collecting imports
  retain the same malformed-line count for an immutable transcript ending in
  an incomplete JSON record. A mutable transcript may still have a partial
  record in flight. The streaming entry point now receives the sync context,
  so cancellation reaches the parser while it emits staged events. These are
  Agentsview integration checks; they do not change the producer format.

- **Archive import integration reverified 2026-09-08:** collecting and staged
  Codex imports apply the archive content policy before storing result
  payloads or computing signals. Restricted archives omit resumable hash
  state, which can contain trailing source bytes. This is an Agentsview
  storage change; the pinned producer format is unchanged.

- **Pending calls reverified 2026-09-07:** the pinned upstream
  [task abort path](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/core/src/tasks/mod.rs#L846-L915)
  cancels work, allows a short grace period, and emits `turn_aborted`; it
  does not establish that every pending call has received its last output. The
  [response types](https://github.com/openai/codex/blob/406dc9239492aff6d295cca5eebe2a548548d42f/codex-rs/protocol/src/models.rs#L809-L857)
  use opaque string call IDs. Agentsview retains unresolved metadata across
  aborts. Synthetic repeated-ID fixtures preserve its existing latest-call
  attachment behavior; the schema alone does not establish that the producer
  emits repeated IDs.

- **Archive projection (2026-09-04):** Rechecked the pre-version-100
  `flushPendingAgentResultsContext` in `internal/parser/codex.go`: it emitted
  unpaired agent results as unmarked user rows. Archive copies under
  `transcripts` discard those legacy user rows, including indistinguishable
  prompts; version-100 rows use the tool-result marker. Reverified 2026-09-05
  against the parser and archive classification paths: orphan notifications
  retain their result text in full archives but do not count as user prompts
  or supply the first-user text used for automation classification.

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
- **Archive projection (2026-09-04):** Rechecked `relabelCodexResultAsTraeX` in
  `internal/parser/traex.go`: the shared Codex parser produces the same
  unmarked legacy notification rows. Transcript-only archive copies apply the
  same pre-version-100 user-row removal as Codex.

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

- **Usage and cost:** Assistant messages can persist model identity and output
  tokens. Shutdown metrics can persist input, output, cache-read, cache-write,
  and reasoning totals. When both shutdown and store usage are absent,
  Agentsview records only known per-message output tokens; it does not infer
  input, cache, reasoning, or credit totals. Known-model tokens use catalog
  estimates; the existing shutdown reported-cost treatment is unchanged.

- **Agentsview:** `internal/parser/copilot.go` and
  `internal/parser/copilot_provider.go`. Reverified 2026-07-28 against local
  Copilot CLI 1.0.76-0 transcripts: `tool.execution_start` and
  `tool.execution_complete` carry the same `data.toolCallId` and independent
  RFC3339 `timestamp` values, providing an exact execution interval even when
  the next user message arrives after a long resumed-session idle gap.
  Reverified 2026-09-04 against current local transcripts: an
  `assistant.message` can carry `data.model` and `data.outputTokens` when no
  usable `session.shutdown` metrics are present.

- **Store evidence:** Reverified 2026-09-10 against the published Copilot CLI
  1.0.83
  [native package](https://registry.npmjs.org/@github/copilot-darwin-arm64/-/copilot-darwin-arm64-1.0.83.tgz)
  (SHA1 `8b8f67a38e893b61e6cef9c011a6fe22d8fdcd4d`). Native tracking writes
  one usage row per model call, including multiple calls in one turn. Store
  rows therefore use request pricing without a message ordinal. The native
  emitter supplies ISO timestamps. A controlled SQLite writer lock rejected an
  earlier usage insert; a later insert succeeded, and flush did not replay the
  failed insert. A maximum row timestamp is not proof of complete transcript
  coverage. The package's embedded schema and tracking handoff confirm the
  queried token fields and explicit event timestamp.

- **Optional store schema:** Reverified 2026-09-10 against the published
  [Copilot CLI 1.0.60 package](https://registry.npmjs.org/@github/copilot/-/copilot-1.0.60.tgz)
  (SHA1 `8c3a6ae7f9b98986092a9adcb6a2e4d42d915ae3`). Its `app.js`
  initializes session-store schema version 4 without `assistant_usage_events`.
  Running that initialization SQL in isolated SQLite reproduced the missing
  table. A store without the required usage schema leaves transcript and
  shutdown usage available. Missing or incomplete usage schemas are cached as
  empty usage for the current SQLite state. Reverified 2026-09-10 with
  isolated syncs of 8 and 800 sessions: metadata-only writes do not reparse
  transcripts, unchanged states reuse the cached result while the store is
  locked, and a completed schema imports new usage for only the affected
  session.

- **Store refresh:** Store database and WAL writes participate in incremental
  sync cutoff filtering without changing session activity timestamps. Parent
  directory timestamps and available file change times also retain deleted or
  replaced stores whose mtimes are old. Non-missing stat errors retain the
  source for worker verification; operational read failures preserve the
  archived result and remain retryable. Reverified 2026-09-10 with isolated
  sync tests for store-only writes in both journal modes, deletion,
  replacement with an older mtime, database/WAL stat errors, and a lock
  followed by a retry without another store write.

- **Store marker capture:** Reverified 2026-09-10 with isolated SQLite writes
  and connection closes. WAL cleanup racing a marker read can leave usage
  readable while the marker capture fails. Failed captures remain retryable;
  only a confirmed missing database uses the absent-store fingerprint. Parser
  regressions cover failed database/WAL captures, recovery, and an absent
  store.

- **Store accounting:** For sessions starting June 1, 2026 or later, observed
  store tokens replace shutdown token estimates. The latest shutdown reported
  cost remains authoritative; store tokens use catalog estimates, not inferred
  invoice prices. Overlapping transcript output contributes only its positive
  per-model excess over store output. This is a lower bound: extra store-only
  calls can mask missing output, and missing input or request bands cannot be
  recovered. A recovered row replaces that excess without adding it twice.

- **Store refresh:** The native usage insert leaves `sessions.updated_at`
  unchanged. Its `(session_id, id)` usage index supports per-session
  maximum-ID lookups. Agentsview hashes changed sessions' usage rows once per
  store update and reuses unchanged transcript hashes across provider and
  engine restarts. Session timestamps still come from the transcript. Startup
  rebuilds store hashes; runtime refresh follows appends and latest-row
  removal. Historical edits below an unchanged maximum ID wait for a new
  engine or CLI sync.

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

## Open Code Review (`opencodereview`)

- **Format:** One JSONL session file per review under an encoded project
  directory below `.opencodereview/sessions`. Records include review metadata,
  request and response history, tool results, resume lineage, and a terminal
  run manifest.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/alibaba/open-code-review.git` at
  `966f976e24b09e2a3691d3919c9833adcd03642f`; see
  [session writer](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/session/persist.go)
  and
  [session history](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/session/history.go).
  The public issue sample was also preserved as a sanitized fixture in the
  AgentsView parser tests.
- **Reverified (2026-09-10):** Checked the pinned writer and history code plus
  [tool execution](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/llmloop/loop.go)
  and
  [compression](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/llmloop/compression.go),
  together with the
  [comment schema](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/model/review.go).
  `task_done` is a control signal and writes no tool result. `session_end`
  closes the file and is its last physical record. Main and plan requests
  append conversation messages; compression rewrites the initial user prompt
  and removes older messages. AgentsView tracks each stream's previous message
  count and tail digest, emits new user turns, and excludes auxiliary task
  prompts. The tail digest preserves grace prompts appended immediately after
  compression. Successful `task_done` calls reset request history for
  subsequent
  [review rounds](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/agent/agent.go)
  on the same stream. Review checkpoints retain the complete comment objects
  and reuse source ID.
- **Usage and cost:** Response records persist prompt, completion, cache-read,
  and cache-write token counts plus model identity. The
  [OpenAI usage resolver](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/llm/usage_resolver.go)
  treats cached tokens as part of the prompt total, and the
  [native Anthropic adapter](https://github.com/alibaba/open-code-review/blob/966f976e24b09e2a3691d3919c9833adcd03642f/internal/llm/client.go)
  adds cache reads and writes to its prompt total. AgentsView subtracts both
  cache counts for uncached input pricing and uses the prompt total for
  context size. The raw resolver's Anthropic-style fallback can instead leave
  cache counts outside the prompt total. JSONL usage records omit that
  distinction, so those fallback or proxy records may undercount. AgentsView
  does not consume a producer-reported monetary total.
- **Agentsview:** `internal/parser/opencodereview.go` and
  `internal/parser/opencodereview_provider.go`.

## OpenCode (`opencode`)

**Projection detail check (2026-09-12):** Rechecked the pinned
[beta read tool](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/tool/plugin/read.ts#L169),
[tool content schema](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/schema/src/tool.ts#L73),
and
[message updater](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/session/message-updater.ts#L322).
The read tool puts image and PDF bytes in base64 data URIs, alongside their MIME
type and filename. Tool success copies that content into the projection; tool
errors may also retain content. Agentsview stores file-bearing results as
ordered JSON blocks. Inline images use `input_image`/`image_url` so the existing
image keep/drop policy owns their only payload copy. PDFs and other files keep
their `file` records, including the full URI, MIME type, and optional name, in
raw result JSON. Remote and filesystem URIs remain references and are not
fetched. Text-only results retain their existing plain-text format.

`testdata/opencode_v2/tool_files.json` is a synthetic fixture shaped from these
producer sources, with a valid one-pixel PNG and a one-page PDF; it is not a
captured CLI conversation. Parser tests cover successful and failed results.
Normal sync tests use the captured beta database schema and check archived
payloads, image keep/drop behavior, unchanged PDF/text files and references, and
an unchanged second sync. Data version 108 makes existing imports eligible to
recover omitted file payloads. This adds retention, not a PDF previewer.

**V2 projection check (2026-09-08):** Cloned upstream at
`dff8fbc149fb7492e4f07b713ac31ea70d9a541c` and checked the
[SQL schema](https://github.com/anomalyco/opencode/blob/dff8fbc149fb7492e4f07b713ac31ea70d9a541c/packages/core/src/session/sql.ts),
[message schema](https://github.com/anomalyco/opencode/blob/dff8fbc149fb7492e4f07b713ac31ea70d9a541c/packages/schema/src/session-message.ts),
[projector](https://github.com/anomalyco/opencode/blob/dff8fbc149fb7492e4f07b713ac31ea70d9a541c/packages/core/src/session/projector.ts),
and
[message updater](https://github.com/anomalyco/opencode/blob/dff8fbc149fb7492e4f07b713ac31ea70d9a541c/packages/core/src/session/message-updater.ts).
The event log is `event`. The `session_message` table stores complete projected
messages, with `id`, `session_id`, `type`, `seq`, `time_created`,
`time_updated`, and JSON `data`. Inserts retain their initial event sequence;
later text, tool, and step events update the same row. Read rows in `seq` order
without replaying the event log. Revert commits delete rows after the chosen
sequence.

Metadata still lives in `session`. Neither this checkout nor a fresh database
created by the installed OpenCode 1.18.25 has the `session_v2` table described
in issue #1642. The installed CLI's `run` command writes v1 messages, while its
`POST /api/session` and `/api/session/:id/prompt` APIs write v2 projections in
the same database. Both paths were exercised in an isolated scratch project with
a three-line text file. The captured v2 rows are retained in
`internal/parser/testdata/opencode_v2/messages.json`, with temporary paths
replaced by `/workspace/project-a`.

**Released beta check (2026-09-09):** Downloaded the macOS ARM64 asset from
[OpenCode beta 19381](https://github.com/anomalyco/opencode-beta/releases/tag/v0.0.0-beta-19381).
Its bundled CLI reports `opencode2 v0.0.0-beta-19381` and describes itself as
"OpenCode 2.0 preview". The separate `anomalyco/opencode-beta` release channel
carries newer v2 builds than npm's `beta` tag. The exact `2.0.0-beta.7` label in
the issue remains unverified.

Four prompts ran with `run --standalone --model opencode/big-pickle` against
fresh scratch data: a text reply, reading a three-line file, a follow-up
recalling the second word, and a shell command exiting with code 7. The database
contains `session_v2` and `session_message`, with no `session`, `message`, or
`part` tables. Metadata uses `time_created`, `time_updated`, and terminal
`time_idle`; projections retain the `type`, `seq`, and JSON `data` shape above.
The three sessions, ten messages, four user prompts, two tool calls, and one
failed tool were imported through the Agentsview provider. Sanitized producer
tables, indexes, and rows are retained in
`internal/parser/testdata/opencode_v2/beta.sql`.

The beta branch at `d461154a8d2b24c4ad24a89b589069cf08ab168c` corroborates the
[session tables](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/session/sql.ts)
and
[message types](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/schema/src/session-message.ts).
This is a source cross-check, not a verified release-to-commit mapping. For IDs
in `session_v2`, metadata and freshness queries aggregate only projections,
without requiring v1 child tables. Empty beta sessions remain empty. The
simulator uses this beta schema, including its published indexes.

**Upgrade and mixed-history audit (2026-09-09):** The beta's
[v1 migration](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/database/v1-migration.bun.ts)
copies each session's metadata and projections in one transaction and retains
old tables. Discovery therefore includes legacy-only IDs until copied, prefers
v2 for duplicate IDs, and reads OpenCode's `kv` migration cursor to exclude
retained legacy copies after migration. This matters for deletion: the beta's
[delete projector](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/session/projector.ts#L541)
deletes only v2 metadata. A real scratch upgrade started with a 1.18.25 CLI
prompt and continued the same session in beta 19381. Both replies retained the
requested phrase. The parser imported four messages and two prompts without
duplicating the retained v1 rows. Deleting that session through the beta API on
a scratch database copy returned HTTP 204 and left one legacy session with two
messages, zero v2 sessions, and a completed migration marker. Those retained
rows must not become a new source session.

A separate 1.18.25 reproduction reused a CLI-created session through its v2
prompt API. The HTTP 200 request left four v1 messages and appended two
projections. For these earlier databases, the parser merges unmatched v1 rows by
creation time while preserving projection `seq` order. A matching message ID
takes its projected representation. The real session imports all six messages
and both prompts.

The beta's
[idle projector](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/session/projector.ts#L402)
advances `time_idle` while explicitly retaining `time_updated`. The parser
includes terminal idle time in `EndedAt`, passive metadata watermarks, and
active fingerprints. A regression check advances idle time alone and observes
both freshness paths change.

The beta's `read` result uses `state.content`. Its command tool is named
`shell`; the observed exit code 7 is in `state.metadata.exit`. The
[shell plugin](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/core/src/tool/plugin/shell.ts)
confirms that metadata. Streaming tool input has status `streaming` and is not
yet a complete JSON object. Standalone shell messages use `shellID` and an
`output` object containing the text, cursor, size, and truncation flag.

V2 user text and ordered assistant content carry text, reasoning, and tools.
Model identity is `model.id`; tokens retain the `input`, `output`, and
`cache.{read,write}` shape. A completed assistant step with usage but no visible
text still contributes usage. System, synthetic, skill, and compaction rows are
system messages and do not count as user prompts. Completed compactions mark
context boundaries; running or failed compactions do not. Earlier previews
without compaction status also mark a boundary. Shell rows retain their commands
and completed output. Agent/model switch rows are omitted.

The beta prompt API also produced an attachment-only prompt, followed by a
completed manual compaction. Stored files use base64 `data`, as specified by the
[prompt schema](https://github.com/anomalyco/opencode/blob/d461154a8d2b24c4ad24a89b589069cf08ab168c/packages/schema/src/prompt.ts).
The sanitized producer rows are retained in
`internal/parser/testdata/opencode_v2/attachment_compaction.json`. The real
session imports four messages, one prompt, the three-line attachment text, and a
compaction boundary. Inline UTF-8 `text/*` attachments retain decoded text.
Binary and URI-only files retain a named attachment placeholder; the parser does
not dereference file or network locations.

Tool IDs and names are `content[].id` and `content[].name`. Results use
`state.content` text items; the captured `read` tool instead returns a UTF-8
file in `state.structured.content`. Failed tools use `state.status = error`, and
completed bash calls also expose nonzero exits in `state.structured.exit`. The
latter is verified against the upstream
[bash tool](https://github.com/anomalyco/opencode/blob/dff8fbc149fb7492e4f07b713ac31ea70d9a541c/packages/core/src/tool/bash.ts).

V2 change detection includes projection timestamps, row counts, and ordered
`id/seq/time_updated` identities. Earlier previews add these to the v1 composite
described below; current beta databases combine them with session/project
metadata. Per-session queries use the producer's `session_id` indexes.
Watermark-only discovery still reads session/project metadata; child-only
changes follow the existing five-minute full-digest reconciliation policy.
`cmd/perfsim` accepts `--source-format opencode-v2` to exercise projection
inserts, finalization, child-only updates, archive parsing, and usage queries.
This simulator models the persisted projector output; it does not implement
OpenCode's event engine. Data version 104 makes existing imports and skipped
sources eligible for parsing again without requiring a producer write.

**Performance fixture check (2026-09-04):** Rechecked the pinned commit's
[session tables](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/core/src/session/sql.ts),
[project tables](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/core/src/project/sql.ts),
[timestamp defaults](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/core/src/database/schema.sql.ts),
and
[event projector](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/core/src/session/projector.ts).
The projector inserts/upserts JSON message and part data; updates stamp
`time_updated` through the shared column definition. `cmd/perfsim` preserves the
four provider-consumed tables and their indexes in a synthetic WAL database,
writes usage-bearing messages, and edits text parts without advancing session
metadata. Parsed message, usage, and edited-content assertions verify the
fixture. This subset omits the newer `session_message` projection and does not
establish support for that layout. A successfully resolved virtual member is no
longer treated as a missing filesystem path during changed-path preparation;
missing members still follow normal source-missing reconciliation. The retained
recovery workload also closes the producer writer and sends WAL-sidecar events.
A vanished sidecar beside a captured existing database does not imply missing
sessions; an absent database still follows source-missing reconciliation. Member
deletions inside a surviving OpenCode-family container are detected by the next
successful authoritative root/container reconciliation, not promised by a
sidecar event. The retained integration scenario checks that eventual state and
preservation of archived messages for OpenCode, Kilo, MiMoCode, and Icodemate.

- **Format:** Current SQLite-backed session/message/part records and the legacy
  JSON storage tree.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/anomalyco/opencode.git` at
  `67caf894e0843ee370e72839e8265e483233479b`; see
  [message-v2.ts](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/opencode/src/session/message-v2.ts)
  and
  [session.ts](https://github.com/anomalyco/opencode/blob/67caf894e0843ee370e72839e8265e483233479b/packages/opencode/src/session/session.ts).
  Channel database naming was reverified 2026-08-27 against `database.ts`.
- **Usage and cost:** Assistant messages persist input, output, cache-read, and
  cache-write tokens, plus model/provider identity. Agentsview computes price
  from those tokens rather than consuming a persisted USD total. Reverified
  2026-09-07: the pinned `message-v2.ts` hydrates message data independently
  of its part rows. Agentsview retains model identity even when no recognized
  token fields are available; usage-only resync guards check model loss before
  checking token counts.
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
  a container whose last complete digest verification is less than five
  minutes old may list the watermark form
  (`SQLiteContainerListsWatermarkOnly`): every member gate-skips before
  fingerprinting, so the child identity scan would be archive-sized work
  nothing reads. A changed container may continue using this form during that
  bounded interval; after the interval, the next pass carries the complete
  digest again. Watermark-only skips additionally require the pass's container
  capture to still be valid (`sqliteContainerPassCaptureValid`) — a container
  that changes between listing and the recapture check resolves full per-session
  digests instead, so a concurrent child-only write cannot hide beneath an
  unchanged metadata watermark. The trade is explicit: any child-only write that
  leaves the session and project rows untouched — wherever its timestamps land
  relative to the stored composite — is invisible to watermark-only discovery
  and is reconciled by the next full digest pass, at most five minutes after
  the last successful digest verification. That due pass bypasses the
  container-level trusted-state skip so every session reaches the
  authoritative digest comparison; when the platform cannot provide stable
  file identity, the policy fails closed to this full digest form rather than
  authorizing a stale verification timestamp; on the production container
  above, 96% of sessions carry a session/project timestamp at or above every
  child, and actively watched sessions bypass this entirely via the
  per-session composite poll. Per-event work is bounded by the changed batch
  plus one O(session-count) scan of small fixed-width rows (the session table
  and the paged stored-member reads); that floor is irreducible without a
  watermark index, which OpenCode's schema does not have and which is not
  agentsview's to add — but only the changed batch and one stored page are
  ever held in memory.
- **Archive message identity:** Reverified 2026-09-05 against `message-v2.ts` at
  the pinned revision: hydrated messages expose the persisted message row ID
  as `info.id`. Agentsview's pre-version-100 parsers did not retain that ID in
  `source_uuid`. Usage archive comparisons match those stored rows by ordinal
  and role until a complete source rewrite records the ID; rows that already
  have an ID require an exact identity match.
- **Ollama Cloud model tags and cost (2026-09-10):** An assistant message's
  `data.modelID` is the model key from the user's OpenCode provider config,
  recorded verbatim. A custom `@ai-sdk/openai-compatible` provider that points
  at a local Ollama server therefore yields Ollama tags such as
  `kimi-k2.7-code:cloud`, and `data.providerID` is that config's own key
  rather than a catalog provider, so the parser does not retain it. OpenCode
  writes `cost: 0` for such models because it has no rate for them, and the
  parser never imports OpenCode cost, so every such turn is priced by
  agentsview. Ollama's [pricing page](https://ollama.com/pricing) bills cloud
  models per million tokens; on the check date it listed `kimi-k2.7-code` at
  $0.95 input, $0.19 cached input, and $4.00 output, equal to the `moonshotai`
  row in the embedded GenAI Prices document. `PricingResolver.ResolveAt`
  therefore retries the untagged name (`pricing.OllamaCloudBaseModel`) only
  after every exact, custom, historical, and canonical attempt on the tagged
  name fails. The pinned LiteLLM snapshot lists `ollama/*-cloud` rows at $0;
  those exact rows still win, and local tags such as `:27b-mlx` or `:latest`
  stay unpriced.
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
- **Project discovery reverified 2026-09-08:** The pinned SDK's
  [terminal observation](https://github.com/OpenHands/software-agent-sdk/blob/4fe565663af2b4f1130a6e0dac7566b002bfe9b4/openhands-tools/openhands/tools/terminal/definition.py)
  carries `metadata.working_dir`. Remote imports derive project names from
  recorded paths without inspecting those directories on the receiving
  machine; local sessions retain filesystem-based Git project discovery.
- **Archive projection (2026-09-04):** Rechecked `formatOpenHandsAction` in
  `internal/parser/openhands.go`: terminal and custom-tool headers embed the
  event summary, which is not stored separately in tool-call rows.
  Transcript-only archive copies discard the tail from a summary-bearing
  header, retaining the preceding prose and tool label. Appended thinking is
  also discarded because the summary boundaries cannot be recovered.

## Cursor (`cursor`)

- **Format:** Legacy text and newer JSONL transcripts under per-project
  `agent-transcripts` directories in three layouts: flat (`<id>.ext`), nested
  (`<id>/<id>.ext`), and subagent (`<parent>/subagents/<id>.ext`), where a
  delegated subagent's transcript sits one level below the session that
  spawned it. Cursor CLI also writes a per-session SQLite store at
  `~/.cursor/chats/<workspace-hash>/<agent-id>/store.db`, with `blobs` and
  `meta` tables. Metadata key `0` is hex-encoded UTF-8 JSON carrying `agentId`
  and `latestRootBlobId`. The selected root's protobuf field-8 turn index
  supplies assistant reasoning and producer epoch-millisecond timestamps;
  field-1 system/context blobs are not transcript messages.
- **Turn timestamps:** Cursor user messages can carry a leading metadata tag in
  the form
  `<timestamp>Weekday, Mon D, YYYY, H:MM AM|PM (UTC±H[:MM])</timestamp>`
  immediately before `<user_query>`. Agentsview parses the explicit offset and
  stores a UTC instant with zero seconds and nanoseconds. The source has
  minute precision, so the encoded `:00` does not provide second precision.
- **Session bounds:** Agentsview uses the earliest and latest usable tagged user
  turns. A transcript with no usable tag retains the file modification time
  for both bounds. The latest user turn gives a lower bound for assistant
  completion because the Cursor format supplies no assistant completion time.
- **Evidence:** `documentation`.
- **Upstream:** Cursor's first-party
  [history documentation](https://docs.cursor.com/en/agent/chat/history)
  confirms local chat persistence and the separate SQLite history index.
  Cursor support on the official forum documents the
  [`~/.cursor/projects/<project>/agent-transcripts` layout](https://forum.cursor.com/t/chat-history-gone-after-pc-restart-agent-transcripts-files-emptied-how-to-recover/158251/5)
  and identifies `state.vscdb` as metadata **for this CLI producer**. That
  characterization does not extend to Cursor IDE (the GUI editor, see below):
  for the GUI, `state.vscdb` is the only transcript store, not metadata beside
  one. Cursor's public GitHub organization was also searched 2026-07-19; no
  transcript schema or producer source was found. CLI store evidence comes
  from a structural capture of one Windows install on 2026-09-07, which
  recorded the store shape without private prompts, paths, IDs, or
  encryption-key values. A directory measure found one store among 18
  transcript IDs, one overlapping ID, and no store-only sessions. No published
  `.proto` or store schema was found; field numbers remain provisional for
  that captured producer version. Its main database was a 4096-byte header
  while schema and rows lived in `store.db-wal`, so the reader must keep the
  WAL attached.
- **Legacy result evidence (rechecked 2026-09-09):**
  [Issue #1627](https://github.com/kenn-io/agentsview/issues/1627), based on
  read-only inspection of live transcripts on 2026-09-04, reports discarded
  legacy `[Tool result]` blocks. It supplies no raw legacy excerpt or producer
  version. The parser retains its existing assumption that result bodies are
  indented: a nonempty line at column zero ends the body and becomes assistant
  prose. Transcript role delimiters also start at column zero; indented
  `user:` and `assistant:` lines remain in the result body. The issue and
  linked first-party material do not establish that indentation contract; the
  regression inputs are synthetic, not captured producer fixtures. A redacted
  legacy excerpt is still needed to verify it. The first-party
  [JSONL discussion](https://forum.cursor.com/t/accessing-the-full-agent-transcript-in-cursor/157311)
  reports missing tool outputs in JSONL, which does not establish legacy
  text boundaries. The history documentation link above now redirects to the
  docs landing page and supplies no legacy format details. The archive
  regression also exercises incremental S3 sync from data version 101 with an
  unchanged object, verifying recovered output and a skipped fetch on the next
  pass. This uses synthetic input, not additional format evidence.
- **Subagent evidence:** The subagent layout comes from direct inspection of a
  live local `~/.cursor/projects` tree (macOS, 2026-09-04): a delegating
  session records a `Subagent` `tool_use` block whose input carries
  `description`, `prompt`, `subagent_type`, and `run_in_background`; every
  observed `tool_use` block has `"id": null` and no file contains a
  `tool_result` block, so directory placement is the only parent link. In 835
  of 933 checked children the opening `<user_query>` equals a parent
  `Subagent` `prompt` byte for byte, which corroborates that placement. Every
  user message opens with a `<timestamp>` tag at minute resolution with a UTC
  offset, and every file ends with a `{"type":"turn_ended","status":...}`
  record. Two independent readers derive subagent parentage from the same path
  shape:
  [agent-sessions](https://github.com/jazzyalex/agent-sessions/blob/7a3f1f402ed9a2cf6c47aa04c1ed87a0c35fa391/AgentSessions/Services/CursorSessionParser.swift)
  and
  [cc_transcript_viewer](https://github.com/tim-hua-01/cc_transcript_viewer/blob/4157ac3575c4c0e9d742048f494be7ee10c589fc/cursor_parser.py).
- **Usage and cost:** The consumed text/JSONL transcripts have no reliable
  per-message token, cache, reasoning, credit, or monetary-cost fields. The
  captured store adds no priced usage fields consumed by agentsview.
- **Agentsview:** `internal/parser/cursor.go`,
  `internal/parser/cursor_paths.go`, and `internal/parser/cursor_provider.go`;
  workspace identity uses a filesystem-backed unique-match resolver, while
  role and attribution boundaries are reconstructed from Markdown. A
  subagent's parent link is derived from its path at parse time.
  `internal/parser/cursor_store.go` enriches the matching transcript UUID by
  following only the selected root's turn tree in one deferred read-only
  transaction (`mode=ro`), without mutating SQL. Discovery and archive identity
  remain the transcript source (`cursor:<agentId>`). The chats directory is
  local provider metadata, automatically associated only with a resolved
  `.cursor/projects` root, with case-insensitive matching on Windows. Custom
  roots such as `.cursor/archive` remain transcript-only. The chats directory
  stays outside remote, SSH, and S3 transfer targets. Store-only discovery,
  native tool-call/result joining, encrypted blob payloads, and cross-version
  field-number stability remain unsupported; the capture contained no tool
  result and the reader ignores `blobEncryptionKey`. Unknown reachable blobs
  do not discard decoded siblings. Missing required tables or columns,
  unsupported metadata, missing roots or turn indexes, and stores with no
  decodable turns leave the transcript usable, with a warning. Store open/read
  errors remain retryable source errors without replacing archived content.
  Store fingerprint failures prevent freshness skips, and temporary
  path-access failures retain cached stores and newly observed store candidates
  for retry. A failed chats scan warns and preserves cached store locations;
  otherwise sync uses transcripts alone. The next discovery pass retries the
  scan, and store changes invalidate the composite fingerprint. Reverified
  2026-09-09 with synthetic parser and SQLite archive fixtures covering
  initial import and subsequent transcript updates when store enrichment is
  unavailable, missing store tables or columns, and recovery after access
  failures for cached or newly observed stores. A custom root fixture keeps
  reasoning from sibling live stores out of archived transcripts.
- **Duplicate transcripts:** Reverified 2026-09-10 with local parser and sync
  fixtures: a child filename can appear under multiple parents. Changed-path
  and stored-path lookup compare that filename across the project, using the
  same selection rules as discovery. This is an Agentsview policy, not a
  producer guarantee that child IDs are unique. A synthetic fixture also
  verifies that a symlinked nested transcript cannot hide a regular child
  transcript with the same filename in either discovery mode. Broken nested
  links still abort streaming scans as file-access errors, as verified by the
  archive reconciliation fixture. The official support post linked above still
  documents the `agent-transcripts` location; the history documentation link
  now redirects to the Agent overview.

## Cursor IDE (`cursor-ide`)

- **Format:** A shared VS Code-style global-state SQLite database
  (`state.vscdb`), whose `cursorDiskKV` key-value table holds one JSON blob
  per key: `composerData:<uuid>` is one chat session, and
  `bubbleId:<composerId>:<bubbleUuid>` is one turn of that session. This is a
  distinct product and store from Cursor Agent (the CLI, above): the GUI
  writes no `agent-transcripts` files at all.
- **Evidence:** `no-public-source`.
- **Upstream:** Cursor's public GitHub organization and first-party docs were
  searched 2026-08-25; no `cursorDiskKV`, `composerData`, or `bubbleId` schema
  was found. This is consistent with two independent local-history tools
  hitting the same wall (see agentsview issue #1515): a VS Code local-history
  extension states it does not decode Cursor's DB-only chat blobs, and `ctx`
  (`https://github.com/ctxrs/ctx`, checked at `8c6d670`) reads only
  `agent-transcripts` and contains none of those three strings. Evidence here
  is instead direct inspection of a live local `state.vscdb` (macOS, 86MB,
  `PRAGMA quick_check` = `ok`): `composerData` documents observed at `_v: 16`
  and bubble documents at `_v: 3`. `composerData.createdAt` and
  `.lastUpdatedAt` are epoch milliseconds; bubble `.createdAt` is a separate
  ISO-8601 string encoding. `composerData.fullConversationHeadersOnly` is the
  session's turn order (`bubbleId` + `type`: `1` user, `2` assistant); the
  sibling `conversationMap` field, structurally an alternative inline-message
  store, was observed empty (`{}`) on every real conversation inspected,
  including multi-hundred-KB ones, so it is not a usable source at this schema
  version. An assistant bubble's tool call is inline on that bubble's
  `toolFormerData` (name, `rawArgs`, `result`), unlike Claude's separate
  call/result blocks. `workspaceIdentifier.uri.fsPath` and
  `trackedGitRepos[].{repoPath,branches[].branchName}` give cwd and git
  branch. The issue's reporter additionally documents that a Cursor version
  update (3.16.29) has shrunk or wiped some users' `cursorDiskKV` rows, so the
  parser tolerates a `fullConversationHeadersOnly` entry whose `bubbleId` row
  is missing rather than failing the whole session.
- **NULL values:**
  [Issue #1676](https://github.com/kenn-io/agentsview/issues/1676) (reported
  2026-09-08; rechecked 2026-09-10) records 64 SQL NULL values among 2,189
  `cursorDiskKV` rows: 2 `composerData:` rows and 62 `bubbleId:` rows. What
  created those NULLs remains unknown. The synthetic fixture
  `internal/parser/testdata/cursor-ide-null-values.sql` reproduces both row
  types. Tests also cover zero-length BLOBs as synthetic boundary cases; the
  report establishes SQL NULLs only. Agentsview treats either zero-length
  value as absent: a composer yields no session result, preserving any
  archived transcript in the recoverable source-missing state; a bubble
  becomes a gap and marks the transcript truncated. Nonempty malformed JSON
  still errors.
- **Usage and cost:** No per-message or per-session token, cache, reasoning,
  credit, or monetary-cost fields were observed in `composerData` or bubble
  documents. Agentsview emits no usage events for this agent; cost is
  unpriced.
- **Agentsview:** `internal/parser/cursor_ide.go` and
  `internal/parser/cursor_ide_provider.go`, built on the shared
  `multiSessionContainerSourceSet` framework (see Zed, below). Fingerprinting
  never hashes the full database (86MB+ locally, 500MB+ reported in the wild):
  a container-level fingerprint uses whole-file size, composite mtime, and a
  SQLite transaction-state hash over just the database and WAL headers, so a
  rewrite that leaves size and mtime unchanged still misses the skip cache; a
  member-level fingerprint digests every parse input of that one composer (the
  raw `composerData` document plus its bubble rows' keys and values), so an
  equal-length bubble rewrite, a header reorder, or a rename that leaves
  `lastUpdatedAt` untouched still reads as changed. A chat deleted inside
  Cursor IDE is retired through stored-source-hint tombstones on `state.vscdb`
  change events and through complete-container ownership reconciliation.

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
  [session manager](https://github.com/earendil-works/pi/blob/f1c587dde39025c75d7397bc14532d8fa5c001d9/packages/coding-agent/src/core/session-manager.ts),
  plus the
  [skill documentation](https://github.com/earendil-works/pi/blob/f1c587dde39025c75d7397bc14532d8fa5c001d9/packages/coding-agent/docs/skills.md)
  and
  [skill loader](https://github.com/earendil-works/pi/blob/f1c587dde39025c75d7397bc14532d8fa5c001d9/packages/coding-agent/src/core/skills.ts).
- **Usage and cost:** Assistant messages persist input and output tokens plus
  cache-read and cache-write/creation values in nested or historical flat
  shapes. Model IDs are present. Agentsview catalog-prices the tokens. Data
  version 81 reparses existing Pi-family archives after adding the flat
  `cacheWrite` spelling.
- **Directory configuration:** Reverified 2026-09-09 against
  [config.ts](https://github.com/earendil-works/pi/blob/acaa253cc8e3f159e6100b6f3874861b1f0bfc99/packages/coding-agent/src/config.ts)
  and
  [main.ts](https://github.com/earendil-works/pi/blob/acaa253cc8e3f159e6100b6f3874861b1f0bfc99/packages/coding-agent/src/main.ts).
  `PI_CODING_AGENT_DIR` replaces `~/.pi/agent`, with default sessions under
  `sessions/`. `PI_CODING_AGENT_SESSION_DIR` supplies the session directory
  directly and takes precedence over that default. Both accept tilde paths.
  The pinned
  [session manager](https://github.com/earendil-works/pi/blob/acaa253cc8e3f159e6100b6f3874861b1f0bfc99/packages/coding-agent/src/core/session-manager.ts)
  writes files directly into an explicit session directory; default sessions
  live one encoded project directory below the session root. Agentsview
  discovers both layouts.
- **Agentsview:** `internal/parser/pi.go` and `internal/parser/pi_provider.go`;
  alternate branches remain in the file but only the active ancestry is a
  conversation. Reverified 2026-09-03 against 156 local Pi transcripts: the
  parser attributed 1,316 `read` calls whose `path` or `file_path` named a
  concrete `SKILL.md`, while shell commands that only mentioned the filename
  without reading it stayed unattributed.

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
    Skill attribution follows the pinned
    [skill protocol](https://github.com/can1357/oh-my-pi/blob/39c95e5e29b1c8b082059f57421ce445c3dffdd4/packages/coding-agent/src/internal-urls/skill-protocol.ts)
    and
    [shell URL resolver](https://github.com/can1357/oh-my-pi/blob/39c95e5e29b1c8b082059f57421ce445c3dffdd4/packages/coding-agent/src/tools/bash-skill-urls.ts).

- **Usage and cost:** Pi-family usage persists input, output, cache-read, and
  cache-write tokens with a model. Agentsview derives monetary cost from the
  catalog; provider reporting notes are not treated as exact persisted USD.

- **Agentsview:** Oh My Pi is registered through the Pi-family provider in
  `internal/parser/pi.go` and `internal/parser/pi_provider.go`. Reverified
  2026-09-03 that OMP exposes on-demand skill content through `read` calls
  with a `skill://<name>` path, percent-decodes namespaced skill names, and
  accepts a relative resource path after the name. Agentsview attributes only
  the URI in the Pi-family read path and decodes the name before storing it.

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

- **Format:** Agentsview reads version `0` session JSONL under
  `<sessions-root>/<project>/<encoded-session-id>/session.jsonl`, or the
  default checksummed multi-frame zstd encoding at `session.jsonl.zstd`. The
  immutable header records session identity, cwd, creation time, seed lineage,
  delegation origin, and agent preset. Event rows carry a contiguous `seq`;
  runs of assistant deltas may use the `text-chunks`, `reasoning-chunks`, and
  `tool-call-chunks` packed storage rows. Session IDs are arbitrary non-empty
  strings and are injectively encoded before use as a directory name. A
  sessions root belongs to one physical encoding; the upstream backend rejects
  an opposite-suffix artifact rather than providing mixed-root fallback or
  migration. `sourceEventSeqs` uses non-negative safe integers and inclusive
  `[start, end]` ranges, mixed entry by entry: `[[138, 144]]` represents seven
  sequences. This is intentional storage compression, introduced in upstream
  commit
  [df76bc6](https://github.com/deepseek-ai/deepseek-harness/commit/df76bc695b4bdff093369ab22a506cd37ca087c1).

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/deepseek-ai/deepseek-harness.git` at
  `56c4c3e47c195ff5edbfe3d307bdef81f3de348b` (reverified 2026-09-11). The
  [frozen version-0 codec](https://github.com/deepseek-ai/deepseek-harness/blob/56c4c3e47c195ff5edbfe3d307bdef81f3de348b/packages/session/session-format-v0-to-v1/src/codec.ts),

    [released event inventory](https://github.com/deepseek-ai/deepseek-harness/blob/56c4c3e47c195ff5edbfe3d307bdef81f3de348b/packages/session/session-format-v0-to-v1/src/dispositions.ts),
    and
    [version-0 envelope validation](https://github.com/deepseek-ai/deepseek-harness/blob/56c4c3e47c195ff5edbfe3d307bdef81f3de348b/packages/session/session-format-v0-to-v1/src/validation.ts)
    define the generation this parser reads. The codec accepts mixed
    provenance ranges, bounds their expanded count by the owning event sequence,
    and requires strictly increasing sequences when ranges occur. Agentsview
    validates those rules without allocating an unused expanded list. Version 0
    retains `assistant/chunk` and `tool/code-dispatch*`, allows assistant
    provenance, and uses `start`/`end` for surface replacements.

    The original format evidence remains pinned at
    `47f943859bef60e4160492346772ded9b24f765a` for the
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
  persistence backend is not supported. The updated inventory accepts all
  released version-0 event names, including `model/selection`, delivery
  tracking, subagent model policy, and team events. These metadata events do
  not add transcript rows; model-selection reasoning effort is not imported.

- **Later formats:** The reverified upstream
  [session schema](https://github.com/deepseek-ai/deepseek-harness/blob/56c4c3e47c195ff5edbfe3d307bdef81f3de348b/packages/core/session/src/types.ts)
  is version `3`. Agentsview still rejects versions `1` through `3` and does
  not run upstream's migrations. The current
  [generated event inventory](https://github.com/deepseek-ai/deepseek-harness/blob/56c4c3e47c195ff5edbfe3d307bdef81f3de348b/packages/core/session/src/known-event-types.ts)
  includes later events such as `assistant/attempt`, `system/message`, and
  `tool/ptc-dispatch*`; these do not belong to the frozen version-0 inventory.
  Version-3 system messages and `startSeq`/`endSeq` surface replacements need
  a separate format update. Accepting compressed provenance and version-0
  metadata does not add support for later session formats.

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

- **Archive projection (2026-09-04):** Rechecked `formatKimiToolUse` in
  `internal/parser/kimi.go`: its Glob header embeds the raw pattern. Copied
  transcript-only rows now use that formatter with path-only inputs to remove
  arguments while retaining tool labels and file paths. The shared decoder
  gives Kimi Work the same rendering and copy behavior.

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
  was checked 2026-09-06 and documents automatic JSON transcript storage at
  `~/.snowflake/cortex/conversations/<session-id>.json`. It links an
  independent open-source reader: clone
  `https://github.com/dataprofessor/cortex-replay.git` at
  `d61d46a7acbe55b3367f695a04e56eca24871320` and inspect the pinned
  [session parser](https://github.com/dataprofessor/cortex-replay/blob/d61d46a7acbe55b3367f695a04e56eca24871320/src/parser.mjs).
  Rechecked the pinned reader on 2026-09-06: it supports the split-history
  companion and treats user rows containing only tool results and internal
  text as tool responses, not new prompts. This is consumer-side evidence;
  Snowflake does not publish the producer or a versioned schema.
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
- **Timestamp check (2026-09-10):** Reverified the pinned `hermes_state.py`:
  `end_session` writes `ended_at` from `time.time()` only when closing a
  session. `append_message` and `_insert_message_rows` persist message times
  independently and accept explicit timestamps. Agentsview keeps the latest
  transcript or recorded end time and advances it when a state message is
  newer than both that value and `started_at`. The aggregate usage event still
  uses the recorded `ended_at`, falling back to `started_at`.
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
- **Agentsview:** `internal/parser/forge.go`. Reverified 2026-09-10 with
  isolated SQLite fixtures: hosted WAL snapshots use immutable reads in
  read-only materializations; live reads retain uncheckpointed WAL rows.
  Producer schema and usage semantics are unchanged.

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
- **Agentsview:** `internal/parser/piebald.go`. Reverified 2026-09-10 with
  isolated SQLite fixtures: hosted WAL snapshots use immutable reads in
  read-only materializations; live reads retain uncheckpointed WAL rows.
  Producer schema and usage semantics are unchanged.

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
  Reverified 2026-09-10 with isolated SQLite fixtures: hosted WAL snapshots
  use immutable reads in read-only materializations; live reads retain
  uncheckpointed WAL rows. Producer schema and usage semantics are unchanged.

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
  `lm-messages.jsonl`. Assistant messages additionally carry a billing
  provider identity at `providerOptions.providerMetadata.positai.providerId`,
  and sidecar lines carry the same identity as their top-level `providerId`.
  Observed values are `positai` for requests billed through the managed Posit
  AI service and `anthropic` for bring-your-own-provider requests; both can
  appear within one session. These serialized values are implementation
  evidence from observed session artifacts, not published schema. Separately,
  Posit's public [FAQ](https://docs.posit.co/posit-ai/user/faq/) states that
  managed credit usage "is calculated at a 10% premium over model provider
  rates" (checked 2026-08-29); the FAQ substantiates the premium but does not
  define the `providerId` field. Agentsview applies the 11/10 billing
  adjustment only to rows whose provider ID is exactly `positai`. Empty and
  other values, including `anthropic`, price at base catalog rates, custom
  pricing overrides are never adjusted, and explicit reported costs stay
  authoritative.
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
  newer sidecar timestamps extend the session end time. Data version 95 reparses
  existing archives so message and usage-event rows persist the row-level
  provider identity that drives the billing adjustment.

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
  reverse-engineered implementation evidence. Rechecked 2026-09-10 against
  isolated SQLite fixtures: stable WAL snapshots parse read-only, live WAL
  rows remain visible, and a damaged usage-table page preserves session
  discovery while parsing reports the per-session error. Cancellation still
  aborts usage-mtime lookup. Hosted tool skill inference uses recorded path
  names without reading worker-local frontmatter, as checked by
  `TestHostedSkillInferenceKeepsNamesLexical`. Captured WAL fixtures also pass
  through capture, object storage, PostgreSQL job leases, and the hosted
  worker to its projection boundary in
  `TestRawCapturedSourcesReachHostedWorker`. No producer schema change is
  inferred.

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
  Rechecked 2026-09-10: the pinned session manager enables WAL, and a SQLite
  backup retains its WAL header. Stable snapshots use `immutable=1` so the
  read-only materialized directory needs no WAL/SHM writes. Live reads retain
  `immutable=0`; isolated provider fixtures verify both paths. Reverified
  2026-09-10 with `TestHostedSkillInferenceKeepsNamesLexical` that hosted tool
  parsing derives skill names from recorded paths without reading worker-local
  frontmatter; local parsing retains frontmatter lookup.

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
  encrypted producer format. Reverified 2026-09-02 against Google's official
  [Antigravity CLI 1.1.24 macOS ARM64 release](https://github.com/google-antigravity/antigravity-cli/releases/tag/1.1.24),
  asset SHA-256
  `189af288ed9527f567ab3a53b35a6da2fc0c3812c6245f266c75a2a3604bdec3`. Its Go
  binary embeds `FileDescriptorProto` records for
  `third_party/jetski/cortex_pb/cortex.proto` and
  `third_party/jetski/codeium_common_pb/codeium_common.proto`. These compiled
  descriptors name the fields consumed below, but the persistence writer
  remains closed source, so the evidence class remains `no-public-source`.
- **Usage and cost:** SQLite `gen_metadata` and trajectory sidecars can carry
  input, output, thinking-output, cache-read, and model fields; output already
  includes thinking. In CLI 1.1.5 SQLite,
  `CortexStepGeneratorMetadata.step_indices` (field 2) contains packed step
  indices and `ChatModelMetadata.response_model` (field 19) can contain the
  base model slug, and in CLI 1.1.24 can contain an experimental serving
  variant such as `gemini-3.7-flash-exp-b`. The matching
  `ExecutorMetadata.last_step_idx` range carries the effort-qualified model at
  `cascade_config.planner_config.model_name` (fields 10, 1, and 28).
  Agentsview normalizes observed serving canary suffixes (such as `-exp-b`)
  against the covering executor model, while preserving distinct product
  models ending in generic `-exp` (such as `gemini-2.0-flash-exp`).
  `ChatModelMetadata.model_display_name` (field 21) remains the complete label
  when present. Agentsview avoids double counting and catalog-prices usage. No
  provider USD cost is consumed.
- **Agentsview:** `internal/parser/antigravity_cli.go`,
  `internal/parser/antigravity_crypto.go`, and
  `internal/parser/antigravity_cli_provider.go`. The CLI `history.jsonl`
  `workspace` value and the current CLI's `cache/last_conversations.json`
  workspace-to-conversation mapping are authoritative session CWD sources when
  the workspace is an absolute path. Agentsview prefers the first valid
  current-cache folder for an exact conversation ID, retains the strict
  prompt/time fallback for older untagged history rows, and leaves CWD empty
  when the value is missing or relative. The exact absolute workspace remains
  the CWD. The project label is derived from the path text without probing the
  recorded folder when filesystem discovery is disabled.

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
- **Archive projection (2026-09-04):** Rechecked the pre-version-100
  `internal/parser/zencoder.go`: system blocks inside tool results became
  unmarked, system-flagged user rows. Transcript-only archive copies discard
  these legacy system-flagged rows, including indistinguishable notices;
  version-100 rows use the tool-result marker.

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

## Evener (`evener`)

- **Format:** newline-framed semantic transcript v2; header followed by entries
  containing a sequence number and semantic turn. Optional metadata is a
  sibling `<session-id>.meta.json`.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/prime-radiant-inc/evener.git`,
  producer revision `da7c06396c9848abfae362dcffce3861a6a0c95a`, checked
  2026-09-05 (tool-result types reverified 2026-09-08), includes structured
  model-switch facts from PR #889. Earlier v2 records need not contain those
  facts. See
  [transcript.go](https://github.com/prime-radiant-inc/evener/blob/da7c06396c9848abfae362dcffce3861a6a0c95a/agent/transcript/transcript.go),
  [turn schema][evener-source-2],
  [message and usage types][evener-source-3], [metadata][evener-source-4], and
  [fork writer][evener-source-5].
- **Usage and cost:** assistant turns persist uncached input and output plus
  optional cache reads, 5-minute cache writes, 1-hour cache writes, and
  reasoning counts. Reasoning is part of output, not an additional output
  total. Metadata running totals and API logs are not added to these per-turn
  facts. Catalog pricing is computed by Agentsview, not supplied by the
  transcript.
- **Forks:** the producer copies complete turns before the 1-based divergence
  index; verify that prefix against the parent before suppressing replayed
  child history. Missing parents retain child history, like Codex.
- **Model switches:** structured values identify configured provider/model
  transitions, not automatic fallbacks or response aliases. Per-response
  identities take precedence; do not parse display prose for billing facts.
- **Agentsview:** `internal/parser/evener.go` and `evener_provider.go`. Fixtures
  are synthetic and cover semantic content, usage, metadata and fork behavior.
  Capture discovery uses bounded directory batches and the raw-audit progress
  contract. Remote imports verify content hashes rather than trusting copied
  filesystem timestamps. SSH discovery honors an absolute `XDG_STATE_HOME`
  when `EVENER_DIR` is unset and transfers only transcript/metadata pairs,
  excluding API logs, credentials, and symlinked descendants. These transport
  selections do not change the producer format above. Remote Evener imports
  derive project names from the recorded path without probing that directory
  on the receiving machine. The shared remote-import engine applies this
  policy during parsing and project metadata preservation. This can use a
  subdirectory name instead of the Git repository name; local discovery is
  unchanged. Tool-result bodies are stored through the existing category
  filter, without an unfiltered copy in message text; result lengths remain
  available. SSH roots remain file-scoped when invalid filename encodings are
  skipped. Reverified 2026-09-10 with
  `TestProviderParserHostedEvenerMatchesLocal` that captures from both home
  and directly configured sessions roots replay with the local session
  identity and messages. Capture plans retain the sessions directory in their
  logical entry paths so hosted discovery sees the provider's layout, and
  include the immediate parent transcript and metadata needed to filter copied
  fork history. Missing or invalid parent evidence preserves the child's
  history, matching local parsing. The pinned transcript writer appends framed
  entries and resumes at EOF after trimming a partial tail (see the pinned
  [transcript writer][evener-source-1]).
  `TestCapturerEvenerLineageParentGrowthReusesFork` verifies incremental
  parent capture with unchanged fork-object reuse; metadata remains
  replaceable.

## Tau (`tau`)

- **Format:** Tau stores one JSONL transcript per session below
  `<root>/<project>/`, beside a metadata-only `index.jsonl`. Entries use an
  `id` and `parent_id` tree, and the latest `leaf.entry_id` selects the active
  path. Outer timestamps use fractional Unix seconds; nested message
  timestamps use Unix milliseconds.
- **Evidence:** `source`.
- **Upstream:** Tau source is pinned to
  [`93bfc761b43e0a5a646b0e5ac808b3a15918e74d`](https://github.com/huggingface/tau/tree/93bfc761b43e0a5a646b0e5ac808b3a15918e74d).
  Clone `https://github.com/huggingface/tau.git` at that revision. The issue
  transcript attachment has 33 records and SHA256
  `f0d95655c08002655c7e727afe7249bebd8077c58ec017d76a703d785dddb3bd`. The
  index attachment has SHA256
  `069e87052a6fb448f2588669ec6cd31c2456e7a8f7bdee15083d9308197cd3d1`.
  `internal/parser/testdata/tau/issue-session.jsonl` is a sanitized derivative
  of the transcript. It changes paths and names but retains IDs, parents,
  timestamps, models, tools, and usage values. A private Tau 0.4.1 capture
  used during PR validation exercises an active-leaf branch and compaction.
  The public fixture and the committed branch and compaction tests remain
  separate from that private capture. The entry models are in
  [`entries.py`](https://github.com/huggingface/tau/blob/93bfc761b43e0a5a646b0e5ac808b3a15918e74d/src/tau_agent/session/entries.py),
  message models are in
  [`messages.py`](https://github.com/huggingface/tau/blob/93bfc761b43e0a5a646b0e5ac808b3a15918e74d/src/tau_agent/messages.py),
  ancestry is in
  [`tree.py`](https://github.com/huggingface/tau/blob/93bfc761b43e0a5a646b0e5ac808b3a15918e74d/src/tau_agent/session/tree.py),
  persistence is in
  [`storage.py`](https://github.com/huggingface/tau/blob/93bfc761b43e0a5a646b0e5ac808b3a15918e74d/src/tau_agent/session/storage.py),
  paths are in
  [`paths.py`](https://github.com/huggingface/tau/blob/93bfc761b43e0a5a646b0e5ac808b3a15918e74d/src/tau_coding/paths.py),
  and the session manager is in
  [`session_manager.py`](https://github.com/huggingface/tau/blob/93bfc761b43e0a5a646b0e5ac808b3a15918e74d/src/tau_coding/session_manager.py).
- **Usage and cost:** AgentsView maps only `input`, `output`, `cacheRead`, and
  `cacheWrite` to its existing per-message token fields. Tau's `cacheWrite`
  already contains total cache creation, so AgentsView counts it once and
  ignores `cacheWrite1h`, `reasoning`, and the stored cost object. Catalog
  pricing supplies cost when a model has a rate. The private live capture also
  contained `reasoning`, `totalTokens`, and cost objects, which AgentsView
  ignores. A producer-derived test covers 25 total cache-write tokens and a
  10-token one-hour subset.
- **Agentsview:** `internal/parser/tau.go` and `internal/parser/tau_provider.go`
  read each transcript once, exclude the exact `index.jsonl` basename, use the
  filename for ordinary session identity, and encode the project directory
  plus a stable hash of the canonical configured root into `default.jsonl`
  session IDs. They replay the selected ancestry and return a zero-message
  result for an explicit empty leaf. A missing parent detaches the selected
  path. Messages with roles `bashExecution` (user-run shell commands),
  `custom`, `branchSummary`, and `compactionSummary` are skipped; separate
  `branch_summary` and `compaction` entries are rendered. These roles and the
  native `default-<project-directory>` ID were reverified against the pinned
  message models and session manager on 2026-09-08. The root hash keeps
  default sessions from distinct configured roots separate; it does not
  distinguish machines with identical root paths. No legacy Tau v1 conversion,
  native transfer, or index metadata synchronization is included.

[evener-source-1]: https://github.com/prime-radiant-inc/evener/blob/da7c06396c9848abfae362dcffce3861a6a0c95a/agent/transcript/transcript.go
[evener-source-2]: https://github.com/prime-radiant-inc/evener/blob/da7c06396c9848abfae362dcffce3861a6a0c95a/agent/schema/turn.go
[evener-source-3]: https://github.com/prime-radiant-inc/evener/blob/da7c06396c9848abfae362dcffce3861a6a0c95a/llm/types.go
[evener-source-4]: https://github.com/prime-radiant-inc/evener/blob/da7c06396c9848abfae362dcffce3861a6a0c95a/agent/schema/snapshot.go
[evener-source-5]: https://github.com/prime-radiant-inc/evener/blob/da7c06396c9848abfae362dcffce3861a6a0c95a/agent/fork.go
