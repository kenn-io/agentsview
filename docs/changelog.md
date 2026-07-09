---
title: Changelog
description: Release history for AgentsView
---

## 0.37.4
<small>2026-07-09</small>

**New features**

- Add **incremental HTTP remote sync** backed by a persistent per-host mirror.
  After the initial archive download, the collector compares a remote manifest
  with its mirror and requests only changed files when fewer than half of the
  manifest files need fetching. Deleted remote files are removed from the
  mirror without deleting sessions already stored in the local archive.

**Improvements**

- Include **GPT-5.6 model pricing** for the base alias and the Sol, Terra, and
  Luna variants in the embedded fallback catalog, so fresh installs and offline
  usage reports can estimate their costs without a network fetch.
- Explain **default `session list` exclusions** on stderr when one-shot or
  automated sessions are hidden, including the number in each category and the
  flags that include them. Structured stdout remains unchanged for scripts.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for incremental HTTP remote
  sync, GPT-5.6 fallback pricing, and release documentation.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for making
  the `session list` default exclusions visible.

---

## 0.37.3
<small>2026-07-09</small>

**Bug fixes**

- Restore **optimized SQLite builds for Linux release binaries and Docker
  images** by preserving Go's default `-O2 -g` cgo flags when overriding
  `CGO_CFLAGS`, improving full-text search and query performance in those
  artifacts.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for restoring optimized
  release and Docker SQLite builds.

---

## 0.37.2
<small>2026-07-08</small>

**Improvements**

- Speed up **periodic syncs for unchanged OpenCode-family SQLite containers**.
  Once a full pass has verified every session in a shared OpenCode-format
  SQLite database, later idle passes can skip the container before per-session
  fingerprinting by comparing SQLite write markers for the database and WAL.
- Reduce **heap retention after large sync backfills** by periodically
  returning memory to the operating system while archived signal and secret
  recomputations walk large message and tool-result payloads.

**Bug fixes**

- Show **OpenCode tool-call skill names** in tool analytics by extracting the
  dedicated `skill` tool input and applying the existing `SKILL.md` inference
  heuristics to OpenCode read and shell calls.
- Shut down **watcher-triggered syncs** cleanly by threading the server run
  context through changed-path syncs, prioritizing watcher stop signals, and
  making unwatched-directory polling exit on cancellation.
- Fix **desktop app icon spacing** by insetting the shared icon artwork and
  regenerating the checked-in PNG, ICNS, and ICO assets.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for
  speeding up unchanged OpenCode-family SQLite container syncs.
- Thanks to [Rod Boev](https://github.com/rodboev) for OpenCode skill-name
  extraction and large-backfill memory reductions.
- Thanks to [Wes McKinney](https://github.com/wesm) for watcher shutdown fixes,
  desktop icon spacing, and release documentation.

---

## 0.37.1
<small>2026-07-08</small>

**New features**

- Add **semantic and hybrid search** backed by an opt-in local embeddings
  index. AgentsView now embeds user-message units and grouped assistant runs,
  supports `agentsview embeddings build|list|activate|retire`, adds
  `session search --semantic` and `--hybrid`, and returns conversation-unit
  citation ranges with lineage metadata for sidechain and subagent hits.
- Add **content-free session summary export** with a v1 JSON/NDJSON contract.
  `agentsview export sessions` emits session metadata, usage totals, pricing
  provenance, project identity, stable cursors, and reset errors without
  transcript text.
- Add **single-session insight analysis** from the session header. Session
  analysis uses the existing insight-generation pipeline with
  `agent_analysis` plus `session_id`, builds a prompt from that session's
  messages, timing, and usage, and stores the result with the other insights.
- Add **Posit Assistant, Windsurf workspace chat, Qoder, and ZCode session
  support**. These providers cover Posit Assistant workspace conversations,
  Windsurf `workspaceStorage` chat state, Qoder project transcripts and
  sidecars, and ZCode's local SQLite session database.
- Add **Korean (`ko`) localization** and register it alongside English,
  Simplified Chinese, and Traditional Chinese.
- Render **Mermaid fenced code blocks** in markdown messages while keeping the
  source readable if the Mermaid runtime cannot load.
- Show **session context and detailed token breakdowns** in usage views,
  including session-level output-token and peak-context details.
- Add **copy buttons for tool blocks**, with separate affordances for tool
  input and output content.
- Add **worktree layout mappings** in Settings. Worktree mappings now support
  both explicit path-prefix mappings and the `repo_dot_worktrees` layout for
  paths like `<prefix>/<repo>.worktrees/<branch>/...`.
- Add **`sync_include_cwd_prefixes`** to `config.toml` so local sync can ingest
  only sessions whose working directory falls under an allowed path prefix.

**Improvements**

- Render **CLI session search results as an aligned table** for the default
  no-context human output, with terminal-width-aware truncation.
- Reduce **push sync churn** by ignoring volatile stat fields when deciding
  PostgreSQL push candidates and by lowering DuckDB write amplification.
- Keep **unchanged OpenCode-family container sessions** out of sync updates, so
  container rows that did not change no longer churn during sync.

**Bug fixes**

- Parse **Codex custom tool calls** correctly so custom tool invocations are
  preserved as tool blocks instead of falling through malformed paths.
- Apply **`config.toml` port settings** to the active server config before
  startup, so configured ports affect the runtime server just like CLI flags.
- Preserve **calendar range picker selections** after range changes in the
  frontend.
- Apply **agent exclusions consistently in usage filters**, including Usage API
  and frontend paths that previously missed the exclusion set.
- Include **subagent sessions in activity report cost totals**, matching the
  session rows that contribute to the report.
- Map **OhMyPi `parentSession` headers** to `parent_session_id`, preserving
  parent-child lineage for OMP transcripts.
- Skip **local git discovery for sessions from other machines**, avoiding
  host-local repository probes for synced foreign-machine sessions.
- Fix **Linux release builds** by compiling the sqlite-vec cgo bindings
  against the SQLite header bundled with `mattn/go-sqlite3`. This is why the
  release ships as 0.37.1: the 0.37.0 tag never produced binaries.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for semantic search,
  session summary export, activity cost fixes, foreign-machine git-discovery
  safeguards, the Linux release-build fix, and release documentation.
- Thanks to [Rod Boev](https://github.com/rodboev) for Windsurf workspace chat,
  Qoder and ZCode support, Mermaid rendering, usage context and token
  breakdowns, worktree layout mappings, tool-block copy buttons,
  single-session insight analysis, and the Codex custom tool-call fix.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for aligned CLI
  session search output and OMP parent-session lineage mapping.
- Thanks to [Elliot Murphy](https://github.com/statik) for Posit Assistant
  session support.
- Thanks to [Rob Schilder](https://github.com/RobSchilderr) for
  `sync_include_cwd_prefixes` ingestion filtering.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for PostgreSQL and
  DuckDB push-churn reductions, OpenCode-family sync churn fixes, and calendar
  range picker preservation.
- Thanks to [Mr Koala](https://github.com/Mr-Koala) for applying configured
  port settings to the active runtime config.
- Thanks to [Prateek Rungta](https://github.com/prateek) for consistent usage
  agent-exclusion filtering.
- Thanks to [Leuconoe](https://github.com/Leuconoe) for the Korean (`ko`)
  localization.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for
  recurring sync and database performance benchmark gates.

---

## 0.36.1
<small>2026-07-03</small>

**New features**

- Add **Devin CLI session support**. AgentsView now discovers Devin CLI roots,
  reads local session data from the `cli/` subtree, parses Devin messages and
  tool activity, includes Devin in supported-agent metadata, and documents the
  safe root to share when filing bug reports.
- Extend **`parse-diff` diagnostics** for sessions last written through the
  incremental-append path. These rows are now classified as
  `incremental_skew`, excluded from `--fail-on-change`, and accompanied by
  guidance to run a full resync for a clean parser-drift baseline.
- Report **Antigravity `gen_metadata` without usage anomalies** in sync
  summaries, so operators can spot Antigravity records that contain generation
  metadata but did not produce normalized usage totals.

**Improvements**

- Show clearer **startup and resync state** while AgentsView initializes.
  Background startup now publishes the daemon PID, elapsed time, current phase,
  progress detail, and log path for `agentsview serve status`, while full
  resyncs print durable phase and completion lines.
- Improve **remote sync configuration and error reporting**. HTTP remote sync
  now validates configured hosts more directly, keeps ad hoc HTTP remotes
  unsupported, and maps common failures such as token rejection, missing remote
  endpoints, connection refusal, DNS failures, and timeouts to actionable
  messages.
- Refresh **frontend controls and dialogs** by migrating the Svelte UI to the
  shared `@kenn-io/kit-ui` components, making filters, range pickers, dialogs,
  settings controls, copy buttons, refresh controls, and related interaction
  states more consistent across pages.

**Bug fixes**

- Discover **OhMyPi sessions with a leading title slot**, matching the current
  OMP/Pi-style transcript shape instead of skipping those files.
- Reparse **in-place Claude rewrites** even when the source file size and mtime
  are unchanged, so same-length edits no longer leave stale stored messages.
- Fix **data-version resync completion** so preserved orphaned sessions and
  sessions copied through an aborted-resync fallback still receive the
  backfills they need instead of being treated as fully rewritten.
- Scope **PostgreSQL session-alias backfill markers per push target**, so one
  PostgreSQL destination cannot incorrectly satisfy or block the required full
  alias backfill for another target.

**Acknowledgements**

- Thanks to [Aaron Florey](https://github.com/aaronflorey) for Devin CLI
  session discovery and parsing.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for `parse-diff`
  incremental-skew diagnostics, OMP leading-title discovery, Antigravity
  `gen_metadata` anomaly reporting, and the same-size same-mtime Claude rewrite
  fix.
- Thanks to [Wes McKinney](https://github.com/wesm) for startup transparency,
  resync consistency fixes, and remote sync configuration and error reporting.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  frontend migration to shared UI controls and dialogs.
- Thanks to [Rod Boev](https://github.com/rodboev) for scoping PostgreSQL
  session-alias backfill markers by target.

---

## 0.36.0
<small>2026-07-02</small>

**New features**

- Add **pairwise usage cost comparisons** to the Usage page and usage API. The
  new comparison panel can compare any two project or model slices in the
  current date/filter window and reports backend-computed cost, session count,
  total tokens, cost per session, tokens per session, absolute deltas, and
  percent deltas. The REST surface is
  `GET /api/v1/usage/pairwise-comparison`.
- Show **parser malformed-line badges** on session detail pages when a parser
  preserved a session but skipped malformed source lines. The badge uses the
  persisted `parser_malformed_lines` count already produced by parser/sync
  validation, so users can spot partially recovered transcripts without opening
  CLI sync logs.
- Add **Traditional Chinese (`zh-TW`) localization** across the Svelte frontend
  and register it as a supported locale alongside English and Simplified
  Chinese.
- Support **forking Claude sessions from a selected message**. Claude message
  headers now expose a fork action that renders the transcript through the
  selected ordinal into a temporary prompt and launches `claude` from the
  session working directory, or returns a copyable command when launch is not
  available.
- Add **branch metadata and filter support** for session queries. The new
  `GET /api/v1/branches` endpoint returns distinct `(project, branch)` pairs
  with opaque filter tokens, and session, search, analytics, activity, and usage
  endpoints accept those tokens as `git_branch`. Project-scoped tokens keep
  same-named branches in different repositories distinct and preserve empty
  branch values.
- Flag **Antigravity sessions decoded from unrecognized schemas**. Antigravity
  IDE and CLI parsers now fingerprint SQLite schemas into `source_version`;
  unknown fingerprints get an `agy-schema:<prefix>` marker, session details
  expose `decode_confidence: "low"`, sync summaries count the affected
  sessions, `doctor sync` reports them, and the UI shows an **Unverified
  schema** badge.

**Improvements**

- Improve **Cursor attribution in stats** so `agentsview stats` and the stats
  service read the host-local Cursor attribution database with parity between
  direct and daemon-backed execution. Cursor attribution remains machine-local
  and reports unsupported project filters explicitly.
- Extend **`parse-diff` coverage** to DB-backed Warp, Forge, and Piebald
  sessions by allowing provider-authoritative sources, not only file-backed
  parsers.
- Reduce **daemon sync CPU usage while sessions are actively streaming** by
  avoiding repeated skip-check work on hot files that are still being written.
- Improve **unsupported usage reporting** from agent capabilities. Agents now
  advertise whether they lack per-message token data and whether their costs are
  denominated in AI credits (surfaced as "Copilot AI Credits" for Copilot
  agents) as separate capabilities, so Copilot-family filters keep
  Copilot-specific wording while other no-token agents get generic guidance.
- Publish a **`stable` Docker image tag** on tagged releases. `stable` always
  points at the most recent tagged release, while `latest` continues to track
  `main` and exact version tags remain immutable.

**Bug fixes**

- Fix **DuckDB push schema setup through Quack** so remote Quack-backed pushes
  initialize and validate the DuckDB schema correctly.
- Fix **Visual Studio 2026 Copilot session parsing** for the current trace
  layout while preserving the existing Visual Studio Copilot agent identity and
  discovery paths.
- Surface **unsupported Copilot usage filters** correctly in the Usage page and
  API instead of showing an empty report without the Copilot no-token-data note.
- Backfill **worktree mappings** for sessions that have empty working-directory
  rows but can be matched from sibling sessions under the same mapping.
- Prevent **OpenCode WAL watcher feedback loops** by reading OpenCode-family
  SQLite databases through read-only file URIs, ignoring transient `-shm`
  events, and only treating main database or data-bearing WAL changes as
  meaningful.
- Remove **outdated Copilot billing wording** from CLI usage output. The note
  now says Copilot records do not include token or cost data AgentsView can
  total, rather than referring to old billing terminology.

**Acknowledgements**

- Thanks to [Rod Boev](https://github.com/rodboev) for pairwise usage
  comparisons, Cursor attribution parity, Claude message-point session forking,
  worktree-mapping backfills, unsupported usage capability metadata, usage
  filter fixes, Visual Studio 2026 Copilot session parsing, and the `stable`
  Docker image tag.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for parser
  malformed-line badges, Antigravity schema-confidence reporting,
  provider-backed `parse-diff` coverage, the Copilot CLI wording fix, and MCP
  schema documentation.
- Thanks to [Linus](https://github.com/Playgrand-by-linus) for the Traditional
  Chinese (`zh-TW`) localization.
- Thanks to [Prateek Rungta](https://github.com/prateek) for the project-scoped
  branch filter foundation.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for the DuckDB/Quack
  schema setup fix.
- Thanks to [Trent Nelson](https://github.com/tpn) for preventing OpenCode WAL
  watcher feedback loops.
- Thanks to [Wes McKinney](https://github.com/wesm) for daemon sync CPU
  reduction, Windows test-suite fixture reuse, and release documentation.

---

## 0.35.2
<small>2026-06-30</small>

**Bug fixes**

- Fix **PostgreSQL push sync** so skipped ownership conflicts no longer block
  the session-alias backfill marker.

**Acknowledgements**

- Thanks to [leejuhanKr](https://github.com/leejuhanKr) for fixing PostgreSQL
  push sync behavior around skipped ownership conflicts.

---

## 0.35.1
<small>2026-06-30</small>

**New features**

- Add **OpenClaude session support** so AgentsView can discover, import, and
  display OpenClaude JSONL sessions alongside Claude Code and other local
  agents.
- Add **Copilot CLI usage guidance** for records that include aggregate usage
  metadata but do not include per-message token counts.

**Improvements**

- Reuse the configured **DuckDB mirror path** for Quack sync so `duckdb push`,
  `duckdb status`, `duckdb serve`, and `duckdb quack serve` stay pointed at the
  same mirror by default.
- Improve **DuckDB and Quack sync behavior and coverage**.

**Bug fixes**

- Require **daemon transport** for CLI read commands that depend on
  daemon-backed data.
- Surface **desktop backend startup failures** clearly instead of hiding the
  underlying error.
- Validate **desktop updater signatures** before applying updates.

**Acknowledgements**

- Thanks to [Rod Boev](https://github.com/rodboev) for OpenClaude session
  support.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for Copilot CLI usage
  guidance when usage records do not include per-message token counts.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for reusing the
  configured DuckDB path for Quack sync and improving DuckDB/Quack coverage.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  daemon-transport requirement on CLI read commands.
- Thanks to [Wes McKinney](https://github.com/wesm) for desktop backend error
  surfacing, desktop updater signature validation, and release documentation.

---

## 0.35.0
<small>2026-06-29</small>

**New features**

- Add **HTTP daemon remote sync** so configured remote hosts can sync through the
  local daemon instead of requiring each CLI invocation to perform the work
  directly.
- Add **PostgreSQL serve support for curation and insights**. `agentsview pg
  serve` can now back starred, trashed, renamed, and insight workflows from the
  shared PostgreSQL store.
- Add a read-only **`agentsview mcp` server** for MCP-capable assistants to
  search sessions, inspect message windows, and summarize usage from the
  AgentsView archive. See [MCP Server](/mcp/).
- Add **Recent Edits**, a top-level feed for reviewing file edits across
  sessions and jumping back to the exact message that made each change. See
  [Recent Edits](/recent-edits/).
- Add **batch session selection and deletion** in the sidebar.
- Add **S3-compatible object storage discovery** for Claude and Codex session
  roots, so a central AgentsView instance can sync raw session files from S3,
  MinIO, R2, OSS, and similar stores. See
  [S3-Compatible Session Sources](/configuration/#s3-compatible-session-sources).
- Add **Chinese localization** plus language settings across the frontend.
- Add **UI text-size scaling** and **high-contrast** appearance mode.
- Add an **Analytics dashboard model filter** that scopes dashboard panels to
  selected models. See [Model Filter](/usage/#model-filter).
- Add **export and publish actions for generated insights**.
- Add **IcodeMate** agent support.
- Add **Cursor admin usage ingestion** to the Usage board.
- Add **Kimi cost estimation** from aggregate token usage.
- Add **per-remote sync intervals** for configured remote hosts.
- Add **named PostgreSQL push targets** so separate destinations can keep their
  own connection and watermark state.
- Add **offline LiteLLM pricing fallback data** for usage and cost reporting.

**Improvements**

- Make **local CLI and desktop workflows daemon-first**, aligning commands with
  the long-running local service used by the desktop app.
- Standardize **`--format` and `--json` output flags** across commands.
- Surface **parser anomaly signals** in sync summaries so malformed or
  suspicious parser output is easier to notice.
- Stream **remote sync progress through the daemon** with per-phase elapsed
  time.
- Improve **daemon replacement handling** for `agentsview serve`.
- Preserve **Pi message tree lineage**.
- Preserve **XML-style prompt tags** in rendered markdown.
- Show **summary-mode Antigravity CLI sessions** and record Antigravity
  producing-version metadata.
- Enrich **tool summaries** and add a **skim layout** for session reading.
- Add an **in-page help affordance** for Insights.
- Add **right-side axis labels** to the concurrency timeline.
- Require **opt-in Aider discovery** to avoid scanning large or sensitive
  directory trees unexpectedly.
- Add `substrings` and `exact_matches` to **automated session detection**
  configuration.
- Publish **raw markdown route companions** for docs pages.
- Deprecate **Amp support documentation** now that current Amp releases may keep
  complete threads server-side.

**Bug fixes**

- Include **Claude.ai export attachments** during import.
- Honor **`CLAUDE_CONFIG_DIR`** for Claude session discovery.
- Support **explicit WSL paths** in desktop environment settings.
- Persist **desktop sidecar logs** for desktop builds.
- Use **native webview zoom** in the desktop app, fixing Windows zoom rendering.
- Repair the **AppImage DirIcon** after bundling.
- Fix **remote daemon progress reporting** so each sync phase stays visible with
  elapsed time.
- Fix a **resync discovery performance regression** and correct its
  mis-attributed phase timing.
- Detect **Codex title-only renames** during full sync.
- Repair **persisted Codex goal-context rows**.
- Treat Codex **`/goal` continuation context** as system content.
- Fix **Gemini per-turn context token** calculation.
- Fix **Gemini insight generation** by no longer forcing a sandboxed Gemini CLI
  run.
- Count **subagent sessions** in analytics totals.
- Clamp **analytics top-session active duration** by idle gaps.
- Scope **filtered PostgreSQL push watermarks** correctly.
- Reset **push watermarks** when PostgreSQL targets change.
- Fail **blocked PostgreSQL pushes** and surface push errors.
- Accept duration syntax for **`usage daily --since` and `--until`**.
- Deduplicate **replayed continued-session usage rows**.
- Suppress the **Local reporter timezone sentinel** in stats.
- Fall back to `AGENTSVIEW_GITHUB_TOKEN` and the GitHub CLI auth token for local
  **Gist publishing**.
- Install the binary with an **atomic rename** in `make install`.
- Skip compatible PostgreSQL push schema DDL where it is not needed.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for HTTP daemon remote sync,
  daemon-first CLI and desktop workflows, Recent Edits, UI text-size scaling and
  high-contrast mode, remote sync progress fixes, daemon replacement handling,
  raw markdown docs routes, Amp documentation deprecation, PostgreSQL push
  watermark fixes, and installer hardening.
- Thanks to [Rod Boev](https://github.com/rodboev) for PostgreSQL serve curation
  and insight persistence, generated insight export and publishing, Cursor admin
  usage ingestion, per-remote sync intervals, named PostgreSQL push targets,
  offline LiteLLM pricing fallback data, Pi lineage preservation, Insights help,
  desktop fixes, Claude import/config fixes, and usage/stat fixes.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the MCP server,
  parser anomaly signals, Codex rename and `/goal` handling fixes, Antigravity
  summary-mode sessions, blocked PostgreSQL push error handling, and parser
  validation work.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the
  provider source-set migration work, frontend control chrome guard, localized
  reading-control pluralization, and analytics active-duration correction.
- Thanks to [icatw](https://github.com/icatw) for Chinese localization and
  frontend localization coverage.
- Thanks to [Prateek Rungta](https://github.com/prateek) for standardized CLI
  output flags and duration syntax for `usage daily`.
- Thanks to [DanielMao](https://github.com/DanielMao1) for S3-compatible session
  discovery.
- Thanks to [huaiyuWangh](https://github.com/huaiyuWangh) for batch sidebar
  selection and deletion.
- Thanks to Leonidas Lux for IcodeMate agent support.
- Thanks to [Junt184](https://github.com/Junt184) for Kimi cost estimation.
- Thanks to [Jesse Robbins](https://github.com/jesserobbins) for counting
  subagent sessions in analytics totals.
- Thanks to Trent Nelson for repairing persisted Codex goal-context rows.
- Thanks to [Martin Wimpress](https://github.com/flexiondotorg) for compatible
  PostgreSQL push schema handling.

---

## 0.34.5
<small>2026-06-23</small>

**Bug fixes**

- Keep **PostgreSQL session reads** opt-in instead of enabling them
  unexpectedly.

**Documentation**

- Refresh **command and session API docs** for current behavior.

---

## 0.34.4
<small>2026-06-22</small>

**New features**

- Reject **exported archives from newer AgentsView versions** so
  incompatible imports fail safely.

**Improvements**

- Refresh **daily usage totals** correctly while reducing repeated
  pricing lookup work.
- Group **GitHub Actions Renovate updates** in CI dependency
  automation.

**Bug fixes**

- Avoid **macOS Photos permission prompts** while scanning Aider
  sessions.
- Avoid **macOS Music permission prompts** while scanning Aider
  sessions.
- Reparse **stale Roborev CI projects** so updated session data
  appears correctly.

---

## 0.34.3
<small>2026-06-22</small>

**Improvements**

- Make **full resync rebuilds** more efficient, reducing sync cost for
  large archives.
- Update documentation for the latest **command, usage, and session API**
  changes.

**Bug fixes**

- Recognize **embedded security review prompts** correctly in stored
  messages.

---

## 0.34.2
<small>2026-06-22</small>

**New features**

- Show **detailed resync progress** while full resyncs are running,
  so the UI can report the current phase instead of only showing a
  generic syncing state.
- Add a **resume-focused session table** to
  `agentsview session list`, including `--resume` and `--active`
  modes for quickly finding recently active sessions.

**Improvements**

- Add **native session links in the sidebar** when an agent exposes
  a supported resume target.
- Redesign the **supported agents docs grid** as uniform clickable
  chips.

**Bug fixes**

- Handle **search terms with operator characters** without returning
  server errors.
- Parse **native Kimi Code records** correctly.
- Improve **incremental JSONL resume reliability**.
- Make **large-session deletes safer** for full-text search cleanup.
- Prevent **Ctrl+K command palette input** from reselecting text on
  every keystroke.
- Avoid **DuckDB tool-call attachment failures** from invalid
  negative call indexes.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for detailed
  resync progress, native Kimi Code record parsing, and the supported
  agents docs grid redesign.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the
  resume-focused session table and search operator-character fix.
- Thanks to [Rod Boev](https://github.com/rodboev) for native sidebar
  session links, incremental JSONL resume reliability, safer
  large-session full-text cleanup, and the Ctrl+K palette input fix.
- Thanks to [MirzaSamadAhmedBaig](https://github.com/Mirza-Samad-Ahmed-Baig)
  for the DuckDB negative call-index fix.

---

## 0.34.1
<small>2026-06-21</small>

**New features**

- Add the **AgentsView documentation site**, including quickstart,
  commands, configuration, usage, stats, token usage, API, remote
  access, and PostgreSQL sync docs.
- Add **documentation build and deployment support**, including
  validation, asset hydration, screenshot handling, and Vercel
  deployment configuration.

**Improvements**

- Clarify **forwarded development access** instructions for public
  origins, tunnels, WSL2, Codespaces, Coder, and reverse proxy setups.
- Move **internal-only documentation** into an internal docs area so
  it stays available without publishing as user documentation.
- Add automated checks for **docs sources, built site output,
  redirects, and docs assets**.

**Bug fixes**

- Avoid scanning **protected home directories** during Aider session
  discovery.

---

## 0.34.0
<small>2026-06-20</small>

**New features**

- Add the **Activity** reporting dashboard. The new top-level page
  reports active time, idle time, peak concurrency, agent-minutes,
  cost, output tokens, projects, models, machine filters,
  automation filters, clickable concurrency buckets, session rows,
  project/model/agent breakdowns, and range-scoped Activity
  Insights. See [Activity](/activity/).
- Add **session quality scoring and insight patterns**. Session
  intelligence now has a richer scoring basis for recurring quality
  patterns, and the Activity page can generate a daily or range
  activity insight for the currently resolved range.
- Add a **Top Skills** analytics panel that ranks skill-backed tool
  calls by call count, sessions, recency, agent mix, project mix,
  and weekly trend. Skill names come from explicit tool metadata and
  are inferred from `SKILL.md` reads for agents (such as Codex and
  Cursor) that do not record them. See [Top Skills](/usage/#top-skills).
- Add new session sources: **OhMyPi**, **Reasonix**, **aider**,
  **Mistral Vibe**, **Visual Studio Copilot**, **Shelley**,
  **QwenPaw**, **MiMoCode**, **Claude Cowork** sessions from Claude
  Desktop local-agent mode, **Kilo**, **DeepSeek TUI**, and
  **gptme**. Each source has a matching environment variable and
  `config.toml` directory-array key. See
  [Session Discovery](/configuration/#session-discovery).
- Add **background `serve` mode**. `agentsview serve --background`
  starts the web UI as a managed local service, writes logs to
  `~/.agentsview/serve.log`, and records enough runtime metadata for
  `agentsview serve status` and `agentsview serve stop` to inspect or
  stop the daemon later. See [CLI Reference](/commands/#background-mode).
- Route `agentsview session` **read commands through PostgreSQL**.
  When `AGENTSVIEW_PG_URL` or `[pg].url` is configured, read-only
  session commands use the shared PostgreSQL store by default; pass
  `--pg` to require that path explicitly. Mutating commands continue
  to use the local archive.
- Target an **explicit session server** from the `agentsview session`
  CLI with `--server <url>`, authenticated by `AGENTSVIEW_SERVER_TOKEN`
  or `--server-token-file`; a discovered local daemon's token is never
  sent to an explicit URL. See [Session API](/session-api/).
- Add **session list sorting** to CLI and HTTP surfaces with
  `agentsview session list --sort`, `--reverse`, `order_by`,
  `descending`, and per-key direction suffixes such as
  `messages:desc,started:asc`. See
  [Session API](/session-api/#agentsview-session-list).
- Add `agentsview doctor sync`, a **sync diagnostics** report for
  checking database readability, data-version counts, agent roots,
  and recent sync-debug lines. See [CLI Reference](/commands/#agentsview-doctor-sync).
- Add `agentsview parse-diff`, a parser QA report that re-parses
  source files from the current archive and reports differences from
  the stored SQLite rows without rewriting sessions. See
  [CLI Reference](/commands/#agentsview-parse-diff).
- Surface **Copilot AI credits** in usage reporting when
  Copilot-family sessions have priced usage.
- Add **Copy source file path** to the session export menu when the
  active session has a backing source file.

**Improvements**

- Use one shared **range picker** and keep date ranges synchronized
  across the Analytics dashboard, Usage page, and Activity page.
- Improve **SQLite dashboard performance** by reducing heavy analytics
  refetches during sync, adding more deliberate freshness handling,
  improving large sidebar index loading, adding usage-query indexes,
  compressing API responses, and bounding WAL checkpoints.
- Batch **PostgreSQL push comparison reads** for faster sync.
- Preserve **source machine metadata** during PostgreSQL push instead
  of replacing it with the pushing machine.
- Persist **Codex and OpenCode working directories** so Git worktree
  mappings can classify more sessions by their real project root.
- Streamline the **Analytics and Usage refresh control**: a single
  refresh button paired with a relative "Updated…" timestamp, where a
  manual refresh also resets the automatic refresh timer.
- Show **agent-provided display names in the dashboard "Top sessions"**
  list instead of the first message, matching the session sidebar.
- Extract **token usage and cost from VS Code Copilot** sessions when
  their persisted request metadata includes `promptTokens`,
  `outputTokens`, and resolved model names. See
  [VS Code Copilot Token Metrics](/token-usage/#vs-code-copilot-token-metrics).
- Improve **Antigravity** parsing: IDE role detection now recognizes
  newer step types, model-name extraction filters noisy prose and URLs,
  token extraction is more complete, and tool-call metadata is richer.
- Import **Codex renamed session titles** from `session_index.jsonl`
  and re-parse **Codex sessions when token counts arrive late**, so
  sidebar titles and usage rows catch up without a manual workaround.
- Promote **orphan subagent sessions** into sidebar roots instead of
  hiding them behind missing parents.
- Fill in **working directory metadata for Pi** sessions when the Pi
  transcript header provides it.
- Refresh frontend glyphs with **lucide icons**.
- Strengthen **selected session styling** in the sidebar so the active
  row remains visible across themes and agent colors.
- Improve **Activity refresh controls**, filter controls, selected
  sidebar rows, and read-only UI states.
- Advertise the WSL **`eth0` URL** when `agentsview serve --host
  0.0.0.0` runs inside WSL and no explicit `--public-url` was supplied.
- Migrate frontend tooling to **Vite+** while preserving the existing
  Svelte app behavior.

**Bug fixes**

- Prevent **PostgreSQL push collisions** for same-ID sessions from
  different machines.
- Deduplicate **duplicate Claude sync sources** so the same transcript
  is not indexed from overlapping roots.
- Support the **Claude companion session layout**, including subagent
  parent inference from companion directories and externalized tool
  result content.
- Support the newer **`.kimi-code` session layout** and add
  **`.kimi_openclaw`** to OpenClaw's default discovery paths.
- Parse **OpenClaw and QClaw `toolCall`/`toolResult` blocks**, whose
  camelCase names the shared content extractor previously dropped, so
  tool calls and tool-only assistant turns no longer vanish from those
  transcripts.
- Render Cursor **`ApplyPatch` tool calls** as patch/diff content in
  the message viewer and markdown/export paths.
- Fix **nested fenced code block** parsing.
- Pin CSP resource origins to the configured **public origin**, fixing
  deployments behind a trusted public URL or proxy.
- Classify **single-turn automation from session transcripts**, not
  just the stored first-message preview, so automated review sessions
  are filtered correctly even when their preview is a generated title.
- Base **skill analytics** on message timestamps.
- Route **command palette session picks** correctly from non-session
  pages.
- Skip **local-only frontend calls in read-only mode**, including
  PostgreSQL-backed `pg serve` deployments.
- Tolerate **NULL message timestamps** in velocity analytics.
- Keep **usage chart date labels** within bounds.
- Show **hour-of-week heatmap** rows starting on Sunday.
- Use **`CREATE_NO_WINDOW`** for the Windows background daemon.
- Update **DOMPurify** for security.

**Acknowledgements**

- Thanks to [Wes McKinney](https://github.com/wesm) for the Activity dashboard, SQLite dashboard performance, sync diagnostics, explicit session-server targets, Claude companion session linking, Cursor `ApplyPatch` rendering, nested fence parsing, the WSL `eth0` URL, copy-source-path support, and assorted CLI and frontend fixes.
- Thanks to [Nitin Gupta](https://github.com/g-nitin) for background `serve` mode.
- Thanks to [Luiz Ferraz](https://github.com/Fryuni) for OhMyPi support.
- Thanks to [Frank Zhu](https://github.com/gkld) for QwenPaw support.
- Thanks to [Rod Boev](https://github.com/rodboev) for Reasonix support, Codex renamed titles, PostgreSQL source-machine preservation, PostgreSQL push batching, Copilot AI credits, orphan subagent promotion, and MiMoCode support.
- Thanks to [SyedaAnshrahGillani](https://github.com/SyedaAnshrahGillani) for Antigravity role and model extraction improvements.
- Thanks to [Justin Cauchon](https://github.com/Cauchon) for Claude Cowork indexing.
- Thanks to [stephan379](https://github.com/stephan379) for VS Code Copilot token and cost extraction.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for `parse-diff` reporting, Shelley support, session sorting, the NULL timestamp velocity fix, OpenClaw/QClaw tool-call parsing, and parser registry coverage.
- Thanks to [matt wilkie](https://github.com/maphew) for Kilo support.
- Thanks to [yuna](https://github.com/yunasora) for DeepSeek TUI support.
- Thanks to [Matt Van Horn](https://github.com/mvanhorn) for PostgreSQL-backed CLI reads.
- Thanks to [潦草学者](https://github.com/liaocaoxuezhe) for Kimi `.kimi-code` and `.kimi_openclaw` support.
- Thanks to [Douglas Creager](https://github.com/dcreager) for Pi working-directory metadata and dashboard display names.
- Thanks to [Bob](https://github.com/TimeToBuildBob) for the gptme parser.
- Thanks to [Trần Quốc Việt](https://github.com/charlieviettq) for the Top Skills analytics panel and `SKILL.md` skill-name inference.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for session quality scoring and insight patterns, the Vite+ migration, and the Codex late-token-count fix.
- Thanks to [vikram bhandoh](https://github.com/vikram) for Antigravity token extraction and richer tool-call metadata.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for synchronized date ranges, the shared range picker, lucide icon migration, transcript-based automation classification, refresh and filter polish, read-only UI fixes, and usage chart label fixes.
- Thanks to [liyi0x0](https://github.com/liyi0x0) for the Windows background daemon window fix.
- Thanks to [Jesse Vincent](https://github.com/obra) for duplicate Claude sync-source deduplication and unwatched-root polling improvements.
- Thanks to [Berend de Boer](https://github.com/berenddeboer) for Codex and OpenCode working-directory persistence.
- Thanks to [KBS](https://github.com/youdie006) for aider support.
- Thanks to [Amjad Saadeh](https://github.com/amjadsaadeh) for Mistral Vibe support.
- Thanks to [mtucker-virtra](https://github.com/mtucker-virtra) for Visual Studio Copilot support.

---

## 0.33.1
<small>2026-06-12</small>

**New features**

- Show **user-assigned Pi session names**. The Pi parser now
  extracts names assigned with Pi's `/name` command, so those
  sessions appear in the sidebar under their given name
  alongside other agent-provided session names.

**Bug fixes**

- Stop **double-counting forked Codex sessions**. `codex fork`
  copies the parent's rollout history — session metadata,
  turns, messages, and token counts — into the top of the new
  file, and the parser counted that replayed prefix as the
  fork's own activity, billing the shared history in both the
  parent and the fork; the replayed parent metadata could also
  overwrite the fork's session ID, storing the fork under the
  parent's identity. Forked sessions now skip the replayed
  history and keep their own identity.
- Render **tool call groups** reliably when message identifiers
  repeat. Sessions read through the
  [PostgreSQL backend](/pg-sync/)'s `pg serve` leave message
  IDs unset (its messages table keys on session and ordinal),
  so a tool call group holding more than one message produced
  duplicate render keys and tore down the message panel. Groups
  now key messages by their ordinal, which is unique within a
  session on every backend.

**Acknowledgements**

- Thanks to [Douglas Creager](https://github.com/dcreager) for the Pi session name extraction.
- Thanks to [nyxst4ck](https://github.com/nyxst4ck) for the forked Codex session fix.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the tool call group rendering fix.

---

## 0.33.0
<small>2026-06-12</small>

**New features**

- Add a **DuckDB mirror backend** with Quack remote support.
  `agentsview duckdb push` mirrors the local SQLite archive into
  `~/.agentsview/sessions.duckdb`, `agentsview duckdb serve`
  runs the read-only web UI from that mirror, and
  `agentsview duckdb quack serve` exposes the mirror over
  DuckDB's Quack remote protocol so another machine can serve
  from it. SQLite remains the source of truth for ingestion —
  the mirror is one-way, like [PostgreSQL sync](/pg-sync/).
  Configuration lives in a `[duckdb]` section of `config.toml`.
  See [DuckDB Mirror](/duckdb/) for the full setup, including
  the Quack token and loopback safety defaults. The DuckDB
  driver is linked into every binary except `windows/arm64`,
  where the upstream bindings ship no prebuilt library —
  `agentsview duckdb` subcommands report a clear error there
  while everything SQLite-backed keeps working.
- Add **Command Code** session support. Sessions are discovered
  under `~/.commandcode/projects/` as one JSONL file per
  session, with an optional `.meta.json` sidecar for metadata;
  structured tool calls, tool results, and thinking blocks are
  parsed from the message content. Configure custom paths with
  `COMMANDCODE_PROJECTS_DIR` or `commandcode_project_dirs`. See
  [Session Discovery](/configuration/#session-discovery).
- Add **Zed** AI assistant session support. AgentsView reads
  Zed's `threads/threads.db` SQLite database from the
  platform data directory (`~/Library/Application Support/Zed`
  on macOS, `~/.local/share/zed` on Linux,
  `~/AppData/Local/Zed` on Windows), extracting messages, model
  names, and per-request token usage, so Zed sessions also
  contribute to [usage and cost reports](/token-usage/).
  Configure custom paths with `ZED_DIR` or `zed_dirs`.
- Allow publishing sessions as **secret GitHub gists**. The
  publish button in the session header is now a dropdown with
  **Publish public Gist** and **Publish secret Gist** entries,
  backed by an optional `secret` parameter on the publish
  endpoint. Note that secret gists are unlisted, not
  access-controlled — anyone with the URL can read them. The
  `p` shortcut still publishes publicly. See
  [Publish to Gist](/usage/#publish-to-gist).
- Ship **native Windows ARM64 builds**: release archives now
  include a native `windows_arm64` binary (built with the
  aarch64 llvm-mingw toolchain, with full CGO/FTS5 support) and
  PyPI gains a `win_arm64` wheel. The PowerShell installer
  prefers the native build automatically. See
  [Quick Start](/quickstart/#install).
- Add **configurable remote hosts** for
  [`agentsview sync`](/commands/#agentsview-sync). Declare a
  fleet of machines as `[[remote_hosts]]` entries in
  `config.toml` (each with `host` and optional `user` / `port`)
  and a bare `agentsview sync` runs the local sync, then syncs
  every configured host over SSH in order. Failed hosts are
  reported and skipped rather than aborting the run;
  `sync --host X` continues to ignore the configured list.
- Show the **session cost** in the session header. Next to the
  token summary, the header now displays the estimated cost of
  the open session (`<$0.01` floor, two decimals up to $100,
  whole dollars above), computed from the same per-session
  usage endpoint that powers `agentsview session usage`. The
  badge is hidden when the session has no token data or its
  models have no pricing.
- Add **syntax highlighting** for fenced code blocks, powered
  by Shiki. Twelve common languages are bundled (JavaScript,
  TypeScript, Python, Bash, JSON, YAML, Markdown, HTML, CSS,
  Rust, Go, SQL); unlabeled or unknown languages render as
  plain text. Highlighting is skipped for blocks over 50 KB or
  800 lines to keep large sessions fast, and the highlighter
  loads lazily in code-split chunks so it costs nothing until
  the first code fence renders. See
  [Code Blocks](/usage/#code-blocks).

**Improvements**

- Show **agent-provided session names** in the sidebar. Several
  agents record a session title (Claude Code's `/rename`,
  Claude.ai and ChatGPT conversation names, Forge, Hermes,
  Kiro, Piebald, Cortex Code, and Command Code's `.meta.json`
  titles); the session list now shows these titles instead of
  the first-message preview. Manual in-app renames always win
  and are never overwritten by agent-provided names. The
  release bumps the parser data version, so existing sessions
  backfill their names on the next resync.
- Leave **session labels** untruncated. Sidebar labels were
  hard-truncated at 50 characters; the full label is now placed
  in the DOM and CSS ellipsis clipping adapts to the actual
  sidebar width instead of a fixed character count.
- Extract **Antigravity CLI token usage**. The parser now reads
  token counts from trajectory `gen_metadata` blobs and from
  sidecar `generatorMetadata` arrays, with precedence rules
  that prevent double-counting, so Antigravity CLI sessions
  contribute to the [Usage dashboard](/token-usage/#usage-dashboard)
  and `agentsview usage` after the 0.33.0 resync.
- Prefer **agy-reader trajectory sidecars** when they are at
  least as complete as the raw Antigravity CLI source: for
  SQLite sessions the richer source wins by step count, and for
  encrypted `.pb` sessions the sidecar is used whenever
  present.
- Infer the **Antigravity CLI project** when `history.jsonl`
  rows lack a conversation ID, by matching the conversation's
  first user prompt against history entries — previously those
  sessions grouped under `unknown`.
- Discover **nested Claude workflow subagents**. Subagent
  transcripts written under nested `subagents/` paths (as
  produced by workflow orchestration) are now found by a
  recursive walk instead of a flat directory listing, so they
  appear in the [sub-agent tree](/usage/#sub-agent-tree).
- Keep the app usable when the **PostgreSQL sync backend is
  degraded**. A 5xx from the backend no longer forces a full
  page reload (which could loop while the database was down) —
  the UI stays interactive with a compact status-bar warning,
  and recovers once a real data read succeeds. True network
  failures still use the reload-based recovery path.
- Improve **PostgreSQL and CockroachDB analytics performance**:
  usage-window queries push their filters down into the source
  tables, tool-call category aggregation moved from Go into
  SQL, and model-pricing writes are batched and skipped when
  unchanged. Aggregation semantics are unchanged across SQLite,
  PostgreSQL, and CockroachDB.
- Ship fallback pricing for **`claude-fable-5`** ($10 input,
  $50 output, $12.50 cache creation, $1 cache read per million
  tokens), so Fable 5 sessions are priced before the LiteLLM
  catalog catches up. See
  [Pricing Source](/token-usage/#pricing-source).
- Fall back to the **system browser on Linux** when the desktop
  WebView cannot render. On GPU/driver/compositor combinations
  where WebKitGTK aborts with an EGL error and leaves a blank
  window, the desktop app now detects the dead WebView, opens
  the UI in your default browser, and explains what happened —
  the backend keeps running either way.
- Add **anonymous daemon telemetry**. The server now sends a
  daily liveness ping (version, commit, OS, architecture, and a
  random per-machine install ID — no session data, paths,
  hostnames, or prompts). Disable it with
  `AGENTSVIEW_TELEMETRY_ENABLED=0`. See
  [Privacy and Telemetry](/configuration/#privacy-and-telemetry).

**Bug fixes**

- Include **non-Claude agents** in the peak context token
  distribution. The stats bucketing was gated to Claude
  sessions even though other parsers (Hermes, Kimi, Forge, Zed)
  record peak context tokens; the gate now keys on whether the
  data exists.
- Create the PostgreSQL **content trigram index** with
  `fastupdate = off`. With the GIN default, continuous ingest
  buffered inserts into a pending list that only VACUUM merges,
  bloating the index by orders of magnitude on large archives.
  New indexes are created with `fastupdate = off` and existing
  ones are altered on startup; run
  `REINDEX INDEX idx_messages_content_trgm;` once to reclaim
  space already consumed.
- Fix the **Windows PowerShell 5.x installer**, which aborted
  with "maximum redirection count exceeded" while resolving the
  latest release; the installer now follows the redirect with a
  HEAD request that works on both PowerShell 5.x and 7+. On
  Windows ARM64 it also probes for the native `arm64` asset and
  falls back to `amd64` under emulation only when no native
  build exists.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the DuckDB mirror backend, secret gist publishing, nested workflow subagent discovery, untruncated session labels, and daemon telemetry.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the Antigravity CLI sidecar token usage, trajectory-sidecar preference, project inference, and parser fuzz hardening.
- Thanks to [vikram bhandoh](https://github.com/vikram) for Antigravity CLI token usage extraction.
- Thanks to [Aaron Florey](https://github.com/aaronflorey) for Command Code session support.
- Thanks to [ArBing Xie](https://github.com/arbing) for Zed session support.
- Thanks to [Josix](https://github.com/josix) for Shiki syntax highlighting.
- Thanks to [chrislee3408](https://github.com/chrislee3408) for the session cost display in the session header.
- Thanks to [Gordon Woodhull](https://github.com/gordonwoodhull) for agent-provided session names in the sidebar.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for degraded-backend resilience and the CockroachDB analytics performance work.
- Thanks to [Martin Wimpress](https://github.com/flexiondotorg) for the trigram index fix.
- Thanks to [matt wilkie](https://github.com/maphew) for the Linux system-browser fallback.
- Thanks to [nyxst4ck](https://github.com/nyxst4ck) for the peak context token distribution fix.

---

## 0.32.1
<small>2026-06-05</small>

**New features**

- Show the **full content of tool calls** in the message
  viewer. Long tool parameters — Bash commands and
  heredocs, large `Write` payloads — were truncated at
  200 characters with no way to reach the rest, even
  though the complete text was already stored. The
  collapsed [tool block](/usage/#tool-blocks) now previews
  the first 20 lines with a *Show more* / *Show less*
  toggle, Bash calls render their `description` and
  `command` fields in full, content is capped at 200 lines
  to keep the DOM light, and a search match inside a
  collapsed block expands it automatically.

**Improvements**

- Skip **`/usage` probe sessions** from session lists and
  usage reporting. Content-free probes whose only real
  turn is the `/usage` command — such as CodexBar's
  ClaudeProbe, which runs `claude /usage` to read usage
  stats — are now dropped during parsing, while sessions
  that also contain a genuine prompt are kept.
- Recognize **Codex code-review sessions** as
  [automated activity](/configuration/#automated-session-detection).
  Codex re-emits the initial prompt verbatim when it
  continues a task across turns, which pushed the
  user-message count past the single-turn gate and left
  review sessions (`"You are a code reviewer."` and
  `"You are a security code reviewer."`) unflagged. The
  parser now drops a verbatim replay of the first prompt
  until a distinct user turn appears, so these sessions
  classify as automated and stay out of session lists and
  analytics by default. Both this and the `/usage` skip
  ship with a `dataVersion` bump, so existing archives
  correct themselves on the next resync.

**Bug fixes**

- Resolve **dotted model names** to the dashed
  pricing-catalog keys so usage costs compute correctly.
  Some agents report model ids with dots (such as
  OpenCode's `claude-opus-4.7`) while the LiteLLM table
  keys them by the dashed API string (`claude-opus-4-7`),
  so [`agentsview usage daily`](/token-usage/#agentsview-usage-daily)
  priced those sessions at `$0.00`. Every cost lookup now
  falls back to the dot-to-dash form.
- Speed up **sync project resolution** by avoiding
  unnecessary `git` subprocesses. Resolving a session's
  project now prefers a cheap filesystem `.git` walk for
  normal repos and linked worktrees instead of shelling
  out to `git` per session, keeping the previous git-based
  lookup as a fallback for rare live-repo layouts. On a
  ~35.7k-session archive this brings cold-sync time back
  from about 4m31s to about 1m14s.

**Acknowledgements**

- Thanks to [Nat Torkington](https://github.com/njt) for showing the full content of tool calls with a show-more toggle.
- Thanks to [Daniel Grenner](https://github.com/dgrenner) for resolving dotted model names to dashed pricing keys.

---

## 0.32.0
<small>2026-06-03</small>

**New features**

- Add support for newer **Antigravity CLI SQLite sessions**
  stored as `~/.gemini/antigravity-cli/conversations/<uuid>.db`.
  AgentsView discovers the SQLite files directly, prefers them
  over same-ID encrypted `.pb` files, parses the trajectory steps
  through the shared Antigravity SQLite/protobuf path, and includes
  `.db-wal` / `.db-shm` metadata in change detection so live
  updates resync reliably. The existing encrypted `.pb`,
  `agy-reader` sidecar, and plaintext summary flows remain in
  place for older Antigravity CLI sessions. See
  [Session Discovery](/configuration/#session-discovery).
- Add **Copilot CLI token usage tracking**. The parser now reads
  assistant output tokens from `assistant.message` events and
  model-level input, output, cache-read, cache-write, and
  reasoning totals from `session.shutdown` `modelMetrics`.
  Claude model IDs reported with dotted versions are normalized to
  the pricing-catalog form, so Copilot CLI rows now contribute to
  the [Usage dashboard](/token-usage/#usage-dashboard),
  `agentsview usage`, and per-session usage reports after the
  0.32.0 resync.
- Add `GET /api/v1/sessions/{id}/usage`, a per-session usage
  REST endpoint that returns the same stable JSON fields as
  [`agentsview session usage --format json`](/session-api/#agentsview-session-usage),
  including cost status, model lists, and unpriced models. The
  endpoint works under both local SQLite-backed `agentsview serve`
  and read-only [`agentsview pg serve`](/pg-sync/#agentsview-pg-serve).
- Add an automatic PostgreSQL push daemon. Run
  `agentsview pg push --watch` for a foreground watcher that
  performs an initial catch-up push, coalesces file changes with
  `--debounce`, pushes at a periodic `--interval` floor, and
  reconnects after transient PostgreSQL failures. The new
  `agentsview pg service` command installs and manages that
  watcher as a per-user launchd service on macOS or
  `systemd --user` service on Linux. See
  [PostgreSQL Sync](/pg-sync/#automatic-push-watcher).
- Ship fallback pricing for `claude-opus-4-7` at the current
  Opus 4.6 / 4.8 tier (`$5` input, `$25` output,
  `$6.25` cache creation, `$0.50` cache read per million tokens),
  so offline reports and fresh installs no longer leave Opus 4.7
  rows unpriced before the LiteLLM catalog is available locally.

**Improvements**

- Replace the custom daemon state-file and PID-lock code with the
  shared `go.kenn.io/kit` daemon runtime. CLI commands now use
  kit runtime records and start locks for daemon discovery, and
  the server exposes a shared `/api/ping` liveness endpoint.
- Switch git repository discovery and author lookup paths onto
  `go.kenn.io/kit` helpers, thread caller contexts through git
  outcome collection, and pin the Go toolchain consistently in
  source and Docker builds.
- Improve the PostgreSQL push/watch path so the watcher uses the
  same `[pg]` config, project filters, classifier wiring, and
  result-content blocking rules as one-shot `pg push` and normal
  `serve`.
- Replace custom frontend UI glyphs with shared lucide Svelte
  icons for more consistent controls, with focused icon export
  tests to guard the mappings.
- Improve forwarded-host rejection handling. When AgentsView is
  reached through SSH port forwarding, a reverse proxy, or a
  remote dev environment with an untrusted `Host`, the server now
  returns a descriptive `403` body and logs a breadcrumb; the
  frontend Settings load surfaces the same actionable
  `--public-url` hint instead of a generic forbidden error. See
  [Remote Access](/remote-access/#forwarded-dev-environments).
- Update `agy-reader` install documentation to the current module
  root command: `go install github.com/mjacobs/agy-reader@latest`.

**Bug fixes**

- Stop benign `tar` warnings from aborting SSH remote sync. The
  local extractor now uses Go's `archive/tar`, skips only known
  self-referential hardlinks, and still fails on malformed,
  truncated, or path-escaping archives. Remote `tar` non-zero
  exits are tolerated only when every stderr line matches known
  "file changed/removed while reading" warnings.
- Rename the private frontend npm package from the old working
  name to `agentsview-frontend`, matching the current project
  identity and avoiding package-name conflicts.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for self-explaining forwarded-host 403 responses, Antigravity CLI SQLite session support, and updated `agy-reader` install guidance.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for the lucide icon migration, shared kit daemon runtime, kit git-helper migration, and Go toolchain pinning.
- Thanks to [Max Feinberg](https://github.com/mxfeinberg) for the PostgreSQL push watcher and macOS/Linux service management.
- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for Copilot CLI token usage tracking.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for the per-session usage REST endpoint.
- Thanks to [Charley Wu](https://github.com/akunzai) for catching the broken `agy-reader` install command path.

---

## 0.31.1
<small>2026-05-28</small>

**Bug fixes**

- Add fallback pricing for **Claude Opus 4.8** so cost
  estimates stay accurate before the model reaches the
  LiteLLM catalog. Opus 4.8 launched at the same rates as
  Opus 4.6 / 4.7 ($5 input / $25 output per million tokens,
  $6.25 / $0.50 cache create / read), but with no catalog
  entry yet the exact-match cost lookup priced its messages
  at $0 in `agentsview usage`. AgentsView now seeds a
  `claude-opus-4-8` fallback row at the current Opus tier;
  the background LiteLLM refresh takes over automatically
  once the upstream catalog adds the model.

---

## 0.31.0
<small>2026-05-28</small>

**New features**

- Parse **decrypted Antigravity CLI trajectory sidecars**
  for richer Antigravity session imports. When a
  `<uuid>.trajectory.json` file sits next to the existing
  AES-encrypted `<uuid>.pb` transcript (under
  `~/.gemini/antigravity-cli/conversations/` or `implicit/`),
  AgentsView uses it as the source of truth for messages,
  tool calls, and tool results, falling back to the
  `history.jsonl` + `brain/` artifact path when no sidecar
  is present. The sidecars are produced out-of-process by
  [agy-reader](https://github.com/mjacobs/agy-reader), which
  performs the decryption and writes plain JSON — so this
  path needs no `ANTIGRAVITY_KEY`. AgentsView treats the
  sidecar as untrusted input: unknown step types are skipped
  and reads are size-capped. The in-process `ANTIGRAVITY_KEY`
  decrypt path is preserved as a fallback. See
  [Session Discovery](/configuration/#session-discovery).
- Expand **[secret scanning](/session-api/#secret-scanning)**
  with six more well-anchored vendor rules: OpenAI
  (`sk-proj-`, `sk-svcacct-`, `sk-admin-`), GitLab personal
  access tokens (`glpat-…`), npm tokens (`npm_…`), PyPI API
  tokens (`pypi-…` macaroons), Hugging Face tokens (`hf_…`),
  and SendGrid keys (`SG.…`). The CLI summary now splits
  counts into definite vs. candidate findings —
  `N findings (X definite, Y candidate)` — and points you at
  [`agentsview secrets list --confidence all`](/commands/#agentsview-secrets)
  when candidate-tier matches exist. JSON consumers get the
  new count fields automatically.

**Improvements**

- Give the right-column
  [Session Vital Signs](/usage/#session-vital-signs) panel a
  sticky **Analysis** title bar with an explicit close
  button, and make the header toggle read *Show / Hide
  session analysis* based on the panel's current state.
- Add hover and title hints plus accessible labels across
  the compact title bar, session breadcrumb, sidebar group
  headers, and modal controls, improving the experience for
  assistive-technology users.
- Document the project's current security posture — threat
  model, trust boundaries, outbound channels, data-at-rest
  sensitivity, and open questions — in a new
  [`SECURITY.md`](https://github.com/kenn-io/agentsview/blob/main/SECURITY.md).
- Update frontend dependencies and the Docker / CI GitHub
  Actions.

**Bug fixes**

- Treat only `401 Unauthorized` from a settings load as a
  token-auth challenge. A `403 Forbidden` response now
  surfaces as a settings error instead of triggering a
  misleading auth-token prompt.
- Use the GitHub release **redirect endpoint** in the
  installers and the in-binary updater (`install.sh`,
  `install.ps1`, and `agentsview update`), reading the
  `releases/latest` 302 target instead of calling
  `api.github.com`. Users behind shared NAT, VPN, or CGNAT
  no longer exhaust the unauthenticated API's 60-request
  hourly quota during install or update.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for the SECURITY.md describing the project security posture and parsing decrypted Antigravity CLI trajectory sidecars.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for header accessibility hints and labels.

---

## 0.30.1
<small>2026-05-25</small>

**Bug fixes**

- Refresh pricing from LiteLLM when
  [`agentsview session usage`](/session-api/#agentsview-session-usage)
  hits an unpriced model. The command previously reported
  `Cost: n/a (unpriced: <model>)` for a newly released model
  even when LiteLLM already had the rate — running
  `agentsview serve` once was enough to fix it, because the
  server's background refresh populated the catalog, while
  the CLI path only seeded the embedded fallback. The CLI now
  triggers a one-shot LiteLLM fetch when it encounters an
  unpriced model and no daemon is running, then re-queries.
  A one-hour cooldown keeps repeated invocations — or
  persistent fetch failures when offline — from pinging
  LiteLLM on every call.

---

## 0.30.0
<small>2026-05-24</small>

**New features**

- Add full **session content search** across stored
  transcripts — substring, RE2 regex, or FTS5 tokenized
  modes — over message bodies, tool inputs, and tool result
  content. Exposed as `agentsview session search <pattern>`
  with the standard session filters (`--project`, `--agent`,
  `--date-from`, etc.) plus `--regex`, `--fts`, and
  `--in messages,tool_input,tool_result`, and over HTTP at
  `GET /api/v1/search/content`. Snippets are returned with
  ~60 characters of context on each side, snapped to rune
  boundaries, and any value that matches a secret-scanner
  rule is masked unless the caller passes `--reveal` from
  localhost.
- Add **secret scanning** for session content. A new
  `internal/secrets` rule pack flags definite vendor
  formats (AWS access keys, `sk-ant-…`, `ghp_…`,
  `github_pat_…`, `xoxb/xoxa/xoxp/xoxr/xoxs`, Stripe
  `sk_live_…` / `rk_live_…`, Google `AIza…` keys, PEM
  private-key blocks) plus an opt-in candidate tier
  (basic-auth URLs, JWTs, high-entropy assignments).
  Definite findings are scored inline during sync; the new
  [`agentsview secrets`](/commands/#agentsview-secrets)
  command group lists them (redacted by default) and
  re-scans the archive for the candidate tier on demand.
  Sessions carry a `secret_leak_count` summary that surfaces
  in [`session get`](/session-api/#agentsview-session-get)
  and a new
  [`session list --has-secret`](/session-api/#agentsview-session-list)
  filter; the same shape is mirrored in PostgreSQL sync.
- Add [`agentsview session usage <id>`](/session-api/#agentsview-session-usage),
  a per-session token-and-cost report. Prints output and
  peak-context totals plus a `~$X.XX` model-pricing
  estimate, with `--format json` for scripting. Reuses the
  same LiteLLM pricing table and `custom_model_pricing`
  overrides as `agentsview usage daily`, and runs an
  on-demand sync for the target session when no daemon is
  running. Replaces the older `token-use` subcommand, which
  remains as a deprecated alias.
- Add support for **Google Antigravity** IDE and CLI
  sessions ([Antigravity](https://antigravity.google),
  Google's agentic IDE). The IDE parser opens per-session
  SQLite databases under `~/.gemini/antigravity/conversations/`
  and decodes the protobuf step payloads; the CLI parser
  reads `~/.gemini/antigravity-cli/` and surfaces the
  plaintext `brain/` artifacts and `history.jsonl`. Setting
  `ANTIGRAVITY_KEY` to the base64-encoded AES key
  additionally decrypts the `.pb` transcripts, mirroring the
  strategy used by the upstream
  [`antigravity_decryptor`](https://github.com/arashz/antigravity_decryptor)
  Python tool. Override the directories with `ANTIGRAVITY_DIR`
  / `antigravity_dirs` and `ANTIGRAVITY_CLI_DIR` /
  `antigravity_cli_dirs`.
- Add support for **[Qwen Code](https://github.com/QwenLM/qwen-code)**
  (`~/.qwen/projects/`, override with `QWEN_PROJECTS_DIR`
  or `qwen_project_dirs`), **QClaw**
  (`~/.qclaw/assets/static/agents/`, override with `QCLAW_DIR` or
  `qclaw_dirs`), and **WorkBuddy**
  (`~/.workbuddy/projects/`, override with
  `WORKBUDDY_PROJECTS_DIR` or `workbuddy_project_dirs`).
  WorkBuddy includes inline subagent linking via
  `<sessionId>:subagent:<subagentId>` IDs.
- Add support for the **current Kiro CLI SQLite store** at
  `~/.local/share/kiro-cli/data.sqlite3`. The Kiro CLI
  parser now reads both the legacy
  `~/.kiro/sessions/cli/` JSONL layout and the SQLite
  `conversations_v2` table; when the same conversation
  exists in both, the SQLite record wins.
- Add **PostgreSQL curation metadata** to `pg push`.
  Starred sessions and pinned messages now sync to two new
  tables (`starred_sessions`, `pinned_messages`) so curation
  state is shared across machines through the same push.
  Pinned messages are reconciled by `source_uuid`, so pins
  survive message-ordinal shifts after a re-parse.
- Add [configurable insight agent binaries](/insights/#configuring-agent-binaries).
  Set `[agent.<name>] binary = "..."` in
  `~/.agentsview/config.toml` to point each insight-generation
  agent (Claude, Codex, Copilot, Gemini, Kiro) at a specific
  executable. When unset, AgentsView falls through to the
  default `PATH` lookup.

**Improvements**

- Add a [follow latest message toggle](/usage/#follow-latest-message)
  to the session header. While active, the message list
  auto-scrolls to the newest message as updates stream in;
  clicking a specific message or scrolling up cancels the
  follow. The preference is persisted in localStorage.
- Honor the [Normal and Focused transcript modes](/usage/#focused-transcript-mode)
  in the standalone HTML export. The exported file now ships
  with a Normal / Focused radio toggle in the document
  header, so a recipient who only wants the user-and-final
  view can flip into Focused mode without re-running the
  export.
- Add **copy buttons to code blocks** in session messages,
  matching the per-message copy action. The button appears
  on hover/focus over the code block and copies the raw
  code (no fences).
- Import richer **Hermes** state-database metadata —
  parent-continuation chains, titles, per-message token
  usage, and inline cost — and classify Hermes compaction
  handoffs as compact-boundary system messages. Hermes now
  contributes to `agentsview usage` totals.
- Normalize **Gemini** per-message token usage into the
  same `input_tokens` / `output_tokens` /
  `cache_creation_input_tokens` / `cache_read_input_tokens`
  shape used by Claude and Codex, with Gemini thoughts
  counted toward output tokens and cached prefix toward
  cache reads. Gemini sessions now produce real cost rows
  in `agentsview usage` instead of showing up only in the
  models-used list.
- Note Homebrew Cask in the README install section:
  `brew install --cask agentsview` is the recommended
  desktop-app install on macOS.
- Rename the upstream repository to
  [`github.com/kenn-io/agentsview`](https://github.com/kenn-io/agentsview),
  publish the container image at
  `ghcr.io/kenn-io/agentsview`, and adopt the
  `go.kenn.io/agentsview` vanity Go module path. All
  references in the docs site have been updated to match.

**Bug fixes**

- Load the sidebar from a new skinny session-index endpoint
  so large refreshes no longer trip a render storm against
  the full session payload — the index returns lightweight
  rows and the visible rows hydrate on demand.
- Widen the server's CSP `connect-src` to allow `http:`,
  `https:`, `ws:`, and `wss:` origins so the desktop app's
  **Connect to Remote Server** action can reach external
  AgentsView instances. Other CSP directives stay pinned
  to `'self'`.
- Skip slash-command first messages (`/login`, `/plan`,
  `/clear`, …) when computing the sidebar preview text,
  instead of the previous allowlist that only covered
  `/clear` and `/effort`. The slash-prefix heuristic
  matches any `/<word>` followed by end-of-string or
  whitespace, so absolute file paths in user messages
  still surface normally.
- Fix a Windows desktop sidecar lock during in-place
  updates — the bundled Go server is now stopped only
  immediately before the Tauri updater swaps the binary,
  so the download phase no longer races against the
  running `agentsview.exe`.
- Harden temporary-directory cleanup in tests against
  open SQLite handles on Windows by forcing a GC and
  retrying `RemoveAll` with exponential backoff. CI-only
  change.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for fixing a Windows desktop update sidecar lock, supporting configured insight agent binaries, importing Hermes state-database metadata and usage, the copy action on session code blocks, and the go.kenn.io vanity module path.
- Thanks to [Trent Nelson](https://github.com/tpn) for clarifying insight generation in read-only mode and Normal/Focused transcript modes in HTML exports.
- Thanks to [Matej Sychra](https://github.com/suculent) for Qwen Code agent support.
- Thanks to [Muescha](https://github.com/muescha) for the Homebrew Cask install note in the README.
- Thanks to [Bob N](https://github.com/danxtshake) for reading the current Kiro CLI SQLite session store.
- Thanks to [Christopher Swingley](https://github.com/cswingle) for skipping any slash command when computing the session preview.
- Thanks to [Evgenii Terekhov](https://github.com/nergal-perm) for normalizing Gemini token usage for cost tracking.
- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for supporting Google Antigravity IDE and CLI sessions.
- Thanks to [ArBing Xie](https://github.com/arbing) for QClaw agent support and WorkBuddy agent support.
- Thanks to [lisiyuan656](https://github.com/lisiyuan656) for PostgreSQL curation metadata sync.

---

## 0.29.0
<small>2026-05-10</small>

**New features**

- Add session support for [Piebald](https://piebald.ai), a
  database-backed coding agent. AgentsView discovers Piebald's
  `app.db` SQLite file under `~/.local/share/piebald/` (override
  with `PIEBALD_DIR` or `piebald_dirs`), opens it read-only, and
  parses each chat on demand into the standard
  user/assistant/tool model. Reasoning parts are surfaced as
  `[Thinking]…[/Thinking]` blocks, tool calls are mapped onto
  AgentsView's taxonomy, and Piebald's `sub_agent_chat_id`
  links parent tool calls to the spawned child chat so subagent
  expansion works inline. Per-chat incremental sync only
  re-parses rows whose `updated_at` changed.
- Add user-configurable
  [worktree project mappings](/configuration/#worktree-project-mappings).
  AgentsView normally infers a session's project from its `cwd`,
  which doesn't recognize custom worktree layouts like
  `~/code/{project}.worktrees/feat/<branch>/` — those sessions
  end up grouped under the branch name instead of the parent
  repo. The new **Worktree Project Mappings** section in
  Settings lets you register a path-prefix → project mapping
  per machine, applied to new sessions as they sync and, via an
  **Apply** button, retroactively to already-imported sessions.
  Mappings live in a `worktree_project_mappings` SQLite table
  scoped to the host machine, so mappings created on one machine
  never leak into another's view of synced sessions.

**Improvements**

- Add Piebald to the
  [supported-agents reference table](/configuration/#session-discovery)
  in the README and to the docs.
- Fix sync progress accounting for database-backed agents
  (Piebald, OpenCode, Warp, Forge): the terminal `PhaseDone`
  callback was overwriting the cumulative `MessagesIndexed`
  count with the file-only total, so the post-sync log line
  under-reported indexed messages.
- Validate Piebald fork session IDs (`piebald:<chat>-<row>`).
  Stale or typoed fork IDs that resolved to a real base chat
  but not a real fork row previously returned mtimes and
  succeeded silently; they now require an exact `Session.ID`
  match in the parsed results.
- Drop the hard-coded agent list from `agentsview --help`. The
  list went stale every time a new agent was added; the
  README's supported-agents table and the per-agent env vars
  below it stay authoritative.
- Update GitHub Actions, Go, and frontend dependencies.

**Bug fixes**

- Discover and import Codex sessions from
  `~/.codex/archived_sessions/` in addition to
  `~/.codex/sessions/`, supporting both the dated live-session
  layout and the flat archived layout. Sessions are deduplicated
  by canonical session ID, with live paths preferred when a
  session exists in both locations.
- Link Claude `Task` and `Agent` tool calls to their child
  subagent sessions when result-side `toolUseResult.agentId` is
  the only signal available (queue/progress mapping absent).
  The parser also preserves sibling subagent tool calls when
  Claude emits additive same-`message.id` assistant chunks, so
  parallel subagents under one parent message all link inline.
  Existing Claude archives need a full resync to pick up the
  populated `subagent_session_id` on historical rows.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for documenting Forge in the supported-agents table.
- Thanks to [Kevin Roberts](https://github.com/basekevin) for adding Piebald support.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for importing archived Codex sessions and adding configurable worktree project mappings.

---

## 0.28.0
<small>2026-05-06</small>

**New features**

- Add session support for [Forge](https://forgecode.dev), a
  database-backed coding agent. AgentsView discovers
  `.forge.db` SQLite files under `~/.forge` (override with
  `FORGE_DIR` or `forge_dirs`), opens them read-only, and
  parses each conversation on demand into the standard
  user/assistant/tool model. Reasoning traces are surfaced as
  `[Thinking]…[/Thinking]` blocks, and Forge's nine
  agent-specific tool names (`fs_search`, `patch`,
  `multi_patch`, `undo`, `remove`, `fetch`, `todo_write`,
  `todo_read`, `parallel`) are mapped onto AgentsView's
  taxonomy. Per-conversation incremental sync only re-parses
  rows whose `updated_at` changed.

**Improvements**

- Preview common tool inputs in the collapsed
  [tool block](/usage/#tool-blocks) header so the most
  meaningful field shows without expanding. `TodoWrite`
  surfaces the in-progress todo (or last todo) with a `→`
  prefix; `TaskCreate` shows the subject; `TaskUpdate` shows
  `#<id> · <status> · <subject>`; `Skill` shows the skill
  name; `ToolSearch` shows the first line of the query; and
  `Task`/`Agent`/subagent calls show the description (falling
  back to the prompt). These previews take precedence over
  the first line of `content`, which for these tools is a
  generic header that hides the useful information.
- Link Codex `spawn_agent` tool calls to the spawned
  subagent's session, so the inline subagent expander in the
  message viewer works the same way for Codex sessions as it
  already did for Claude `Task` calls.
- Make git outcome metrics opt-in for
  [`agentsview stats`](/stats/). Outcome aggregation walks
  every session's working directory and shells out to `git`,
  which is slow on large repos and brittle when the cwd has
  moved or the repo is unavailable. Add
  `--include-git-outcomes` to opt into local commit, LOC, and
  files-changed totals, and `--include-github-outcomes` to
  additionally include GitHub PR counts via `gh` (this
  implies `--include-git-outcomes`). The `GHToken` is only
  resolved when GitHub outcomes are requested.

**Bug fixes**

- Link Codex App subagent spawn events to the corresponding
  child session. The Codex parser now consumes both
  `spawn_agent` outputs and
  `collab_agent_spawn_end.new_thread_id` events, recognizes
  `wait_agent` calls with `targets`, and accepts subagent
  notifications that identify children via `agent_path`
  alongside the legacy `agent_id`. Without this, related
  Codex App subagent sessions did not resolve in the
  transcript or in the sidebar's child list.

**Acknowledgements**

- Thanks to [Matthew Jacobs](https://github.com/mjacobs) for adding Forge agent support.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for linking Codex spawn calls to subagent sessions and linking Codex app subagents.
- Thanks to [Mike Baik](https://github.com/wbaik) for linking Claude subagent tool calls.

---

## 0.27.0
<small>2026-05-04</small>

**New features**

- Publish official Docker images. The
  [`ghcr.io/kenn-io/agentsview`](https://github.com/kenn-io/agentsview/pkgs/container/agentsview)
  multi-arch image (linux/amd64, linux/arm64) is built on tagged
  releases and pushes to main, and the repo ships a `Dockerfile`,
  `docker-entrypoint.sh`, and a `docker-compose.prod.yaml` example.
  The same image runs both `agentsview serve` (default) and
  `agentsview pg serve` (set `PG_SERVE=1`). See the
  [Quick Start](/quickstart/#docker) for usage.
- Add a [session status indicator](/usage/#session-status-indicator)
  that classifies each session by recency and termination state and
  surfaces six visual states (working, waiting, idle, stale, unclean,
  quiet) on the sidebar dot and the dashboard's Top Sessions table.
  Backed by a new `termination_status` column on the `sessions` table
  populated by the Claude Code and Codex parsers from per-message
  stop reasons. Sessions can now be filtered by status from the
  [filter dropdown](/usage/#session-filters) or via the new
  `?termination=` URL parameter.
- Use Copilot CLI's `workspace.yaml` `name` field as the session
  title when present, falling back to the first user message
  otherwise. Copilot itself writes a short generated or user-set
  name to that file, so sessions now show meaningful titles like
  *"Fix login authentication bug"* instead of the verbatim first
  prompt. Existing AgentsView display-name overrides still win.

**Improvements**

- Degrade large watch trees instead of failing server startup. When
  the recursive file watcher hits its 8192-directory budget or the
  OS inotify/file-descriptor limit (`EMFILE`/`ENOSPC`), the affected
  root falls back to the existing 15-minute periodic sync plus a new
  2-minute poll loop, and the server logs which roots are watched
  vs. polled. The HTTP listener now binds before the watcher is
  registered, so users hitting these limits no longer see startup
  abort with `socket: too many open files`.
- Sessions, Usage, and the Analytics dashboard now share a single
  filter store: machine, agent, project, min user-message, and
  one-shot/automated toggles applied in the sidebar carry across to
  the Usage page header and the dashboard panels. The Usage page
  reuses the same `SessionFilterControl` widget with searchable
  agent and machine multi-selects (the sidebar's **Status** pills
  are sidebar- and dashboard-only).
- Fix the README `agentsview stats` reference (the README previously
  showed the non-existent `agentsview session stats` form).

**Bug fixes**

- Return `409 Conflict` when uploading a session that is already
  excluded by config or sitting in the trash, instead of silently
  dropping the upload.
- Allow compaction-boundary summary cards to expand in the message
  viewer. Long or multi-line summaries get a "Show full summary"
  toggle; short summaries stay inline.
- Read the `model` and `usage` fields from OpenClaw assistant turns
  so `agentsview usage daily --agent openclaw` reports actual cost
  and token totals instead of `$0.00`.
- Synthesize `Message.ID` from the message ordinal in
  [`pg serve`](/pg-sync/) responses. PG's `messages` table has a
  composite primary key (`session_id`, `ordinal`) and no `id`
  column, which previously left every row with `id = 0` and broke
  Svelte's keyed `{#each}` rendering in the message viewer.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for fixing session and usage filter behavior and tests covering Codex assistant blockquotes.
- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for using the Copilot workspace.yaml name as the session title.
- Thanks to [zerone0x](https://github.com/zerone0x) for fixing the README stats command reference, allowing compaction-boundary summaries to expand, and returning 409 for excluded or trashed uploads.
- Thanks to [juno02139](https://github.com/juno02139) for extracting OpenClaw per-message token usage and model.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for adding session termination status detection and StatusDot UI.
- Thanks to [Aaron Florey](https://github.com/aaronflorey) for the Docker deployment workflow.

---

## 0.26.1
<small>2026-04-30</small>

**Improvements**

- Refresh the filter controls and active-filter displays on the
  Sessions, Usage, and Analytics views for clearer state and
  more consistent behavior across the three pages.
- Add launchd support for running the PostgreSQL helper service
  on macOS.
- Update frontend and Go dependencies.

**Bug fixes**

- Fix session filter behavior in the sidebar.
- Fix Usage page filter behavior and the selected-filter state.
- Keep timing API response arrays non-null.
- Disable the WebKit DMABUF renderer on Linux desktop builds to
  avoid rendering issues.

---

## 0.26.0
<small>2026-04-29</small>

**New features**

- Add a [Trends page](/usage/#trends) for ad-hoc term-frequency line
  charts over your session history. Plot up to 12 terms — each with
  optional pipe-separated variants like `cat|cats` — at day, week, or
  month granularity, and optionally normalize counts by message
  volume. Backed by both SQLite and PostgreSQL, so the page works
  under local `agentsview serve` and shared [`pg serve`](/pg-sync/)
  deployments.
- Replace the right-column activity minimap with a richer
  [Session Vital Signs](/usage/#session-vital-signs) panel covering
  total wall-clock, slowest call, per-category time spent, a
  per-category timeline, and a chronological calls list with parallel
  groups bracketed and sub-agents expandable inline. Each tool block
  in the conversation now also shows a duration badge, and each
  assistant message shows a per-turn summary line.
- Make the default date range on the Usage and Analytics dashboards
  *rolling* — pages left open across midnight roll the window forward
  at the next refresh tick, sync event, or manual refresh, instead of
  staying anchored to the day they loaded. Manual date edits or
  `?from=…&to=…` URL parameters pin the window; preset buttons (`7d`,
  `30d`, `90d`, `1y`) return to rolling mode.

**Improvements**

- Scale call duration bars in the Vital Signs Calls list relative to
  the longest call in scope rather than total session wall-clock, so
  call-vs-call comparison stays legible in long sessions.
- Lower the progressive-load threshold from 20,000 to 3,000 messages
  so large sessions render the message viewer faster — older pages
  lazy-load on scroll instead of all loading on session open.
- Reuse the shared `DateRangeSelector` component on the Usage page
  for consistent presets and behavior between Analytics and Usage.
- Faster full resync: batched SQLite writes with per-session
  savepoints, multi-row inserts for messages and tool calls, and a
  single FTS rebuild on the temp database before the atomic swap. On
  a 26.7k-session workload, full-resync wall-clock improved from
  about 1m17s to about 34s.
- Expand [Gemini ingestion](/configuration/#session-discovery) to
  discover and parse the newer streamed `session-*.jsonl` format
  alongside the legacy `.json` Gemini session files.
- Render Claude Code `!cmd` bash shortcut wrappers (`<bash-input>`,
  `<bash-stdout>`, `<bash-stderr>`) as code blocks in message bodies
  and as inline `<code>` in sidebar previews and breadcrumbs, instead
  of leaking the literal pseudo-HTML into the UI.

**Bug fixes**

- Recognize managed worktree project paths — Middleman GitHub
  worktrees and Codex App's `~/.codex/worktrees/<id>/<repo>` layout —
  as the owning repo, so sessions land under the right project
  instead of the worktree id.
- Sync OpenCode SQLite sessions when `opencode.db` and the per-file
  `storage/` layout coexist in the same root. SQLite virtual paths
  are used as a fallback for source lookup and single-session sync
  in those hybrid roots.
- Surface Claude Code `queued_command` attachments — the user
  prompts submitted while a tool call is still running — as regular
  user messages in the transcript instead of dropping them. The
  embedded `dataVersion` bumps from 19 to 20 so existing databases
  re-parse on next startup and recover any previously missed
  mid-flight messages.
- Run remote [SSH sync](/commands/#agentsview-sync) commands through
  `sh -c` so command parsing is independent of the remote login
  shell and embedded single quotes are escaped safely.
- Atomically swap the binary during self-update by staging the new
  binary at `dstPath + ".new"` (with the executable bit already set)
  and renaming it into place, so concurrent `agentsview` invocations
  during an update no longer hit a partially written or
  non-executable file.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for reusing the shared date-range selector on the Usage page and recognizing managed worktree project paths.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for the rolling default date range for the Usage and Analytics dashboards, gating the frontend with svelte-check and vitest, the session-duration UX, and rendering Claude Code bash shortcut wrappers as code blocks.
- Thanks to [Aaron Florey](https://github.com/aaronflorey) for syncing OpenCode SQLite sessions in hybrid roots and running remote SSH sync commands through a POSIX shell.
- Thanks to [Christopher Swingley](https://github.com/cswingle) for the Gemini JSONL import fix.

---

## 0.25.0
<small>2026-04-25</small>

**Improvements**

- Use `is_automated` consistently across [`agentsview stats`](/stats/)
  totals, archetypes, distributions, and agent portfolio metrics, so
  the stats report uses the stored automation classification instead
  of mixing in older user-message-count heuristics.
- Rename the data directory environment variable to
  `AGENTSVIEW_DATA_DIR`. The legacy `AGENT_VIEWER_DATA_DIR` name is
  still accepted as a fallback when the canonical variable is unset.
- Build and test against Go 1.26. Source builds now require Go
  1.26+ with CGO.

**Bug fixes**

- Filter synthetic Copilot skill messages from parsed conversations,
  so injected skill context no longer affects stored transcripts,
  first messages, or user-message counts.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for NilAway lint checks and updating the build to Go 1.26 and fixing an arm64 desktop test flake.
- Thanks to [Tim O'Guin](https://github.com/timoguin) for filtering synthetic Copilot skill messages.

---

## 0.24.0
<small>2026-04-23</small>

**New features**

- Add OpenCode storage-backed session support. AgentsView now
  auto-detects when an OpenCode root uses the per-file `storage/`
  layout (`storage/session`, `storage/message`, `storage/part`)
  and parses the JSON files directly. The legacy `opencode.db`
  SQLite backend is still read transparently when that's what's
  present — see [Session Discovery](/configuration/#session-discovery).
- Add [custom model pricing](/token-usage/#custom-model-pricing)
  via `[custom_model_pricing.<model>]` tables in
  `~/.agentsview/config.toml`, for models not in the LiteLLM
  catalog or when you want to override the catalog's rates.
- Add configurable [automation patterns](/configuration/#automated-session-detection)
  via `[automated] prefixes`, `substrings`, and `exact_matches` in
  `~/.agentsview/config.toml`, so single-turn sessions with
  first-message patterns unique to your own automation get filtered
  from session lists and analytics alongside the built-in roborev
  patterns.

**Improvements**

- Broaden built-in automated-session detection to cover Claude
  Code conversation-title generation, warmup pings, roborev
  review combiner prompts, and AgentsView's own changelog
  generator.
- Refresh the `is_automated` classification at startup, using a
  classifier hash that covers built-in patterns and configured
  `[automated]` patterns, so edits to automation rules and rows
  imported from other archives get re-labeled without a manual
  resync.
- Improve Claude first-message selection in the sidebar by
  skipping leading `/clear` and `/effort` command envelopes so
  the session preview shows the next real user message instead.

**Bug fixes**

- Narrow OpenCode's storage-mode file-watch root to the `storage/`
  subtree, reducing inotify pressure and spurious watch events on
  binaries, logs, and caches. SQLite mode continues to watch the
  `opencode.db` parent.
- Stop orphaned subagent/fork rows from surfacing as top-level
  groups in the sidebar when their parent session has been
  filtered or rotated off disk.
- Fix Codex parser handling so `codex exec` sessions interrupted
  mid-run (which emit a synthetic `<turn_aborted>` user message)
  are classified and counted correctly.

**Acknowledgements**

- Thanks to [Eran Sandler](https://github.com/erans) for adding OpenCode storage-backed session support.
- Thanks to [Antoine](https://github.com/anth2o) for custom model pricing via config.

---

## 0.23.5
<small>2026-04-20</small>

**Improvements**

- Coalesce rapid dashboard update events to cap refetch frequency and keep the UI smoother under heavy activity.

---

## 0.23.1
<small>2026-04-19</small>

**Bug fixes**

- Usage reporting excludes cached Codex tokens from `input_tokens`, making token totals more accurate.

---

## 0.23.0
<small>2026-04-19</small>

**New features**

- Add live dashboard refresh via Server-Sent Events so the Sessions and Usage views update without a full page reload when new sync data lands.
- Add [session intelligence](/session-intelligence/) with health signals, outcome classification, score/grade badges, a per-session signal panel, and aggregated dashboard health analytics.
- Add the [`agentsview session`](/session-api/) CLI as a programmatic surface for listing, inspecting, exporting, syncing, and watching session data.
- Add markdown export for sessions via `/api/v1/sessions/{id}/md`, including optional child-session depth controls.
- Add SSH remote sync to [`agentsview sync`](/commands/#agentsview-sync) so a local archive can pull session data from a remote machine over SSH.
- Add PostgreSQL-backed usage reporting so the Usage dashboard and related endpoints work under [`agentsview pg serve`](/pg-sync/).
- Add [`agentsview stats`](/stats/), an experimental window-scoped reporter for session, git, and outcome activity across the local archive.

**Improvements**

- Require the explicit `serve` subcommand to start the server; plain `agentsview` now shows help.
- Show session names in the Usage page's Top Sessions by Cost table.
- Preserve active filters when switching between the Sessions and Usage tabs.
- Link the Usage page's `Project | Model | Agent` group-by selectors so the chart and attribution panel stay in sync.
- Make incremental parsing more reliable by validating files with inode and device tracking in addition to size and mtime.
- Rewrite the project README as a clearer user-facing guide.

**Bug fixes**

- Surface the underlying login-shell probe failure reason for arm64 desktop flake cases instead of hiding the root cause.
- Only self-heal the frontend events store on transient failures, avoiding permanent retry loops against unsupported event streams.
- Accept raw session IDs in `agentsview token-use` in addition to canonical stored IDs.
- Resolve ephemeral `serve` port handling correctly.
- Parse Claude Code CLI JSON array output correctly in insight generation.
- Recognize the `"helpful assistant"` prefix as an automated session.
- Skip git-root walking for foreign-OS working directories.

**Acknowledgements**

- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for linking usage group-by selectors, migrating command tree to Cobra, adding markdown session export, resolving ephemeral serve port, showing session name in Top Sessions by Cost, and preserving filters when switching between the Sessions and Usage tabs.
- Thanks to [Jesse Robbins](https://github.com/jesserobbins) for correctly parsing Claude Code CLI JSON array output in insight generation.
- Thanks to [Trent Nelson](https://github.com/tpn) for PostgreSQL-backed usage reporting.
- Thanks to [Phillip Cloud](https://github.com/cpcloud) for live dashboard refresh via Server-Sent Events and self-healing the frontend events store only on transient failures.
- Thanks to [AO](https://github.com/andrewwowens) for inode/device tracking to validate incremental parses.

---

## 0.22.2
<small>2026-04-13</small>

**Bug fixes**

- Correct fallback pricing for Claude Opus 4.6 so usage cost estimates use the proper `$5` input and `$25` output rates.

---

## 0.22.1
<small>2026-04-13</small>

**Bug fixes**

- Keep Codex model detection consistent during incremental syncs so usage reporting stays accurate.

---

## 0.22.0
<small>2026-04-13</small>

**Improvements**

- Extend usage cost tracking to [OpenCode and Pi](/token-usage/#agent-coverage) sessions alongside Claude Code and Codex. Their token counts now flow into the [Usage dashboard](/token-usage/#usage-dashboard) and `agentsview usage` CLI reports.
- Refine the Usage dashboard summary cards and cost-over-time chart for clearer cost reporting.

**Bug fixes**

- Sync Codex `exec` sessions so execution activity appears reliably in AgentsView.
- Fix usage accounting so total cost data stays complete across supported providers.

---

## 0.21.0
<small>2026-04-13</small>

**New features**

- Add a [token usage dashboard](/token-usage/#usage-dashboard) to the web UI for exploring cost and token totals across your agent sessions. The new `/usage` page shows summary cards, a cost-over-time chart, a cost attribution treemap with project/model/agent toggles, top sessions by cost, and a cache efficiency panel. Filter by project, agent, model, and date range — filter state is written back to the URL so views are shareable and bookmarkable.
- Add backing API endpoints for the usage dashboard so scripts and external tools can fetch the same summary, time series, and attribution data the UI uses.

**Bug fixes**

- Prevent some Claude usage records from being double-counted in cost and token totals.
- Correct Opus 4.6 pricing in usage cost calculations.

---

## 0.20.0
<small>2026-04-12</small>

**New features**

- Add [`agentsview usage daily`](/token-usage/#agentsview-usage-daily) and [`agentsview usage statusline`](/token-usage/#agentsview-usage-statusline) commands that report token usage and estimated cost by day or for the current day, scoped to Claude Code and Codex sessions. Pricing is pulled from the LiteLLM catalog with an embedded fallback for offline use. See [Token Usage & Costs](/token-usage/) for the full write-up, including benchmarks against `ccusage`.
- Add [OpenHands CLI](/configuration/#session-discovery) session support. Local OpenHands conversations under `~/.openhands/conversations` are discovered, synced, and rendered alongside other agents. A new shallow-watch mode is used for agents that store each conversation in its own subdirectory, so file watchers don't exhaust inotify limits.
- Add [Positron Assistant](/configuration/#session-discovery) session support. Positron is a VS Code–based IDE; chat sessions under `workspaceStorage/*/chatSessions/` are parsed using the VSCode Copilot format. Built-in discovery covers macOS only in 0.20.0 — Linux and Windows users need to set `POSITRON_DIR` or `positron_dirs`.
- Filter pinned messages by the currently selected project. The [Pinned](/usage/#pinned-messages) page only shows pins from the active project, the count badge reflects the filtered total, and a dedicated empty state is shown when the filter yields no results.

**Improvements**

- Drop the Intel macOS desktop builds regression. Release artifacts now include `.dmg` files for both Apple Silicon (`aarch64`) and Intel (`x86_64`) Macs, a Linux `arm64` AppImage for PR artifact coverage, plus the existing Linux `x86_64` AppImage and Windows `.exe`.
- Stop enumerating agent names in CLI help text so the `--agent` flag and one-line descriptions don't go stale every time a new agent lands.

**Bug fixes**

- Auto-recover the macOS desktop app when WKWebView's content process is killed during sleep/wake. A Rust-side focus probe detects a dead WebView and force-reloads it; a JavaScript `visibilitychange` handler pings the backend and reloads if unreachable.
- Honor `pi_dirs`, `cursor_project_dirs`, and `amp_dirs` from `config.toml`. These arrays were previously dropped by the loader because the registry entries for Pi, Cursor, and Amp were missing `ConfigKey`.
- Claude Code streaming messages were being recorded multiple times by the parser, inflating historical input-token totals by roughly 2×. The parser now deduplicates them. After upgrading, the first `agentsview usage` invocation triggers a full resync so historical totals are recomputed with the fix.
- Codex sessions now capture the per-request `token_count` events embedded in `event_msg` entries, so Codex messages populate token usage (and therefore show up in [`agentsview usage daily`](/token-usage/)) where they previously reported zero.

**Acknowledgements**

- Thanks to [kinglau66](https://github.com/kinglau66) for fixing the stale synced-status label and showing the exact sync time on hover.
- Thanks to [Srujun Thanmay Gupta](https://github.com/srujun) for adding Hermes Agent session support.
- Thanks to [Mario Witte](https://github.com/mariow) for adding Warp agent support.
- Thanks to [nodivbyzero](https://github.com/nodivbyzero) for adding Positron Assistant session support.
- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for filtering pinned messages by the selected project.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for restoring the Intel macOS desktop build.
- Thanks to [Rajiv Shah](https://github.com/rajshah4) for adding OpenHands CLI session support and shallow watch mode.

---

## 0.19.0
<small>2026-04-08</small>

**New features**

- Add Warp agent session support, reading AI conversations from Warp's local SQLite database with tool call breakdowns and token usage.
- Add Hermes Agent session support across CLI, Discord, webhook, and cron platforms, with per-platform project grouping and skill prefix handling.
- Add Cortex Code session support for Snowflake's AI coding agent, parsing both embedded-history and split-file formats.
- Add Kiro CLI and Kiro IDE session support for Amazon's AI coding assistant, including unified diff rendering for edit actions.
- Add Cursor session history with resume (`cursor agent --resume`) and automatic workspace recovery via `--workspace`.
- Add the ability to resume Claude sessions in Claude Desktop via the session resume menu (macOS).

**Improvements**

- Add [project filtering](/pg-sync/#project-filtering) to `pg push` with `--projects`, `--exclude-projects`, and `--all-projects` flags plus config file equivalents. A new [`agentsview projects`](/commands/#agentsview-projects) command lists all projects with session counts.
- Add an opt-out for the update check via `disable_update_check` in config, `AGENTSVIEW_DISABLE_UPDATE_CHECK=1` environment variable, or `--no-update-check` CLI flag.

**Bug fixes**

- Fix the synced status label staying stale after a sync completes. The relative time now refreshes every 10 seconds, and hovering shows the exact sync timestamp.

**Acknowledgements**

- Thanks to [Jacob Struzik](https://github.com/jstruzik) for resuming Claude sessions in Claude Desktop.
- Thanks to [Christian Bush](https://github.com/cbb330) for Cursor session history with resume and workspace recovery.
- Thanks to [AO](https://github.com/andrewwowens) for adding Cortex Code session support.

---

## 0.18.0
<small>2026-04-02</small>

**New features**

- Import [Claude.ai and ChatGPT conversations](/chat-import/) into AgentsView. Upload the zip file export from either service via the UI or CLI to bring your full conversation history — including ChatGPT images — into your local database alongside your agent coding sessions.
- Show token usage metrics across sessions, analytics views, and exports. Session breadcrumbs display peak context and output tokens, the analytics dashboard includes output token summary cards and heatmap metrics, and CSV exports include token totals.

**Improvements**

- Detect automated roborev sessions and filter them out of session lists, counts, and analytics by default. An "Include automated" toggle in the session filter dropdown opts back in.

**Bug fixes**

- Keep pinned messages pinned after a session resync. Previously, re-syncing a session's messages could silently remove all pins due to cascade deletes; pins are now preserved by matching on message ordinal.
- Reduce WebView memory usage when rendering message content by rekeying content-parser caches on message ID instead of full message text and lowering cache capacity.

**Acknowledgements**

- Thanks to [Trent Nelson](https://github.com/tpn) for token-usage metrics across sessions, analytics, and exports.
- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for preserving pinned messages after a session resync.

---

## 0.17.1
<small>2026-03-29</small>

**Bug fixes**

- `pg serve` automatically applies pending PostgreSQL schema migrations on startup, reducing upgrade friction and startup failures.

**Acknowledgements**

- Thanks to [Thomas Maloney](https://github.com/tlmaloney) for auto-migrating the PostgreSQL schema on pg serve startup.

---

## 0.17.0
<small>2026-03-28</small>

**New features**

- Add a session activity minimap with click-to-navigate support for quickly jumping to activity hotspots within a session.
- Add a resizable session sidebar so you can drag the divider to adjust the layout to fit your workflow.
- Capture and display Codex subagent result events in session transcripts, with an expandable history of status updates per tool call.

**Improvements**

- Support Cursor's nested transcript directory layout for more reliable transcript discovery.

**Bug fixes**

- Restore Copilot as an insight-generation agent alongside Claude, Codex, and Gemini.
- Fix transcript strip pill spacing in WebKit browsers.

**Acknowledgements**

- Thanks to [Trent Nelson](https://github.com/tpn) for a resizable session sidebar and modeling Codex subagent result events.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for fixing transcript-strip pill spacing in WebKit.
- Thanks to [Christian Bush](https://github.com/cbb330) for supporting Cursor's nested transcript directory layout.

---

## 0.16.2
<small>2026-03-25</small>

**Bug fixes**

- Detect Claude conversation forks across the full reply subtree so branched threads display correctly.

---

## 0.16.1
<small>2026-03-25</small>

**Bug fixes**

- Fix Linux container builds by disabling VCS stamping in the Go build.

**Acknowledgements**

- Thanks to [Trent Nelson](https://github.com/tpn) for adding managed Caddy support to pg serve and adding multi-host machine filtering and labels.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for adding URL-based session linking.

---

## 0.16.0
<small>2026-03-24</small>

**New features**

- Add direct URL links for opening specific sessions, enabling bookmarks and sharing of session deep links.
- Add machine labels and multi-host filtering to separate sessions by source machine in shared deployments.
- Add focused transcript mode with a responsive header that strips intermediate tool calls and thinking blocks for easier reading.
- Add `Del` as a keyboard shortcut to delete or archive the selected session.
- Add Kimi (Moonshot AI) session support for importing and viewing Kimi conversations.
- Add PostgreSQL push sync and a read-only server mode (`pg push`, `pg serve`) for shared multi-user deployments backed by a central PostgreSQL database.

**Improvements**

- Group command palette search results by session, add a relevance/recency sort toggle, and match against session names in addition to message content.
- Show model information in the session UI, including Codex session models, displayed in message headers.
- Use OpenCode session titles as display names in the session list.
- Improve the mobile layout for smaller screens with a responsive sidebar and viewport-aware keyboard shortcuts.
- Add managed Caddy support to `pg serve` for simpler TLS-terminated shared deployments.
- Publish platform-specific wheels to PyPI for easier installation via `pip install agentsview`.
- Add standard macOS app menu actions for Hide and Hide Others.

**Bug fixes**

- Show user messages that begin with skill or command invocations (e.g. `[Skill: name]`) in transcripts instead of hiding them.
- Prevent Linux desktop app freezes with improved runtime CSP handling in the Tauri configuration.
- Restore desktop copy, paste, and cut keyboard shortcuts through the Edit menu.
- Fix pinned message navigation in very large sessions where the virtualizer could stop mid-scroll before reaching the target message.
- Surface insight generation errors directly in the UI with error status and message display instead of silently failing.

**Acknowledgements**

- Thanks to [Thomas Maloney](https://github.com/tlmaloney) for PostgreSQL push sync and read-only server mode and showing user messages that begin with skill or command invocations.
- Thanks to [CL Kao](https://github.com/clkao) for showing which model Codex sessions use.
- Thanks to [Adam Hjerpe](https://github.com/hjerpe) for using the OpenCode session title as the display name.
- Thanks to [Gary Ritchie](https://github.com/gtritchie) for Hide and Hide Others actions in the macOS app menu.
- Thanks to [0xjjjjjj](https://github.com/0xjjjjjj) for the mobile responsive layout.
- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for repairing pinned-message navigation in large sessions.
- Thanks to [Li Ye](https://github.com/liye71023326) for adding Kimi (Moonshot AI) agent support.
- Thanks to [Maxim Khailo](https://github.com/mempko) for fixing a Linux desktop app freeze with layered CSP and runtime port pinning.
- Thanks to [Trent Nelson](https://github.com/tpn) for a focused transcript mode with a responsive header.
- Thanks to [Stan Rosenberg](https://github.com/srosenberg) for a Del-key shortcut to delete or archive sessions.

---

## 0.15.0
<small>2026-03-17</small>

**New features**

- Add `agentsview token-use`, a CLI command that outputs machine-readable token usage data for scripts and automation.

**Improvements**

- Improve cross-platform token usage reporting on macOS, Linux, and Windows.
- Make token usage insights more consistent across application and system restarts.

**Bug fixes**

- Fix cases where token usage can be attributed to the wrong process after PID reuse or restarts.

---

## 0.14.0
<small>2026-03-15</small>

**New features**

- Add in-session search with `Cmd+F` / `Ctrl+F` and keyboard navigation between matches for finding text within a session's messages.
- Display token usage for each session, showing input and output token counts in the session detail header.
- Add Zencoder CLI session support for importing and viewing Zencoder conversations.
- Add GitHub Copilot CLI resume support in the session dropdown, letting you reopen Copilot CLI sessions directly.
- Ship Linux desktop builds as AppImages, adding Linux to the desktop app platform matrix alongside macOS `.dmg` and Windows `.exe`.

**Improvements**

- Improve insight agent detection for sandboxed sessions and GitHub Copilot, producing more accurate and relevant insights.
- Speed up session resync for larger histories, reducing the time to re-parse all session files.

**Bug fixes**

- Fix updater output so the verify step starts on a new line after download progress.

**Acknowledgements**

- Thanks to [Aleksei Sotov](https://github.com/asotov-zen) for adding Zencoder CLI agent support.
- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for adding Copilot CLI resume support to session dropdown and in-session search with keyboard navigation, plus a resync performance fix.
- Thanks to [CL Kao](https://github.com/clkao) for collecting and displaying per-session token usage.

---

## 0.13.0
<small>2026-03-12</small>

**New features**

- Add a Settings page for appearance, terminal, GitHub, agent directory, and remote access options — centralizing configuration that was previously spread across individual modals and config file edits.
- Add remote access with bearer token authentication, trusted public origins, and managed Caddy reverse proxy mode for secure multi-device access.
- Organize sub-agents and teams in a collapsible tree view in the sidebar, making complex multi-agent sessions easier to navigate.
- Update live sessions incrementally as new activity arrives, replacing full refreshes for faster real-time updates.
- Add desktop zoom controls for adjusting the interface scale.

**Improvements**

- Skip common dependency and build folders (e.g. `node_modules`, `__pycache__`, `.git`) during recursive watching to reduce noise and overhead.
- Show release information in a new About dialog accessible from the header.
- Shut down cleanly on `Ctrl+C` and stop auto-opening the browser on startup by default.

**Bug fixes**

- Preserve original project name casing in group headers.
- Resolve project names correctly for deleted nested worktrees.
- Fix navigation issues in the Windows desktop app.

**Acknowledgements**

- Thanks to [Trent Nelson](https://github.com/tpn) for adding trusted public origins and managed Caddy mode.
- Thanks to [Ben Sedat](https://github.com/bsedat) for excluding common dependency and build directories from the recursive watcher.
- Thanks to [TalesOfThales](https://github.com/userFRM) for a centralized settings page with remote-access authentication and a configurable API base URL, the sub-agent and team tree view with collapsible hierarchy, and preserving project-name casing in group headers.

---

## 0.12.0
<small>2026-03-10</small>

**New features**

- Add iFlow agent support for importing and viewing iFlow conversations, including JSONL session parsing and tool calls.
- Add a session resume menu with three actions: reopen a session, launch a terminal in its working directory, and open the directory in Finder/Explorer.
- Ship packaged desktop releases (`.dmg` for macOS, `.exe` for Windows) as the recommended install path, with built-in auto-update support.

**Improvements**

- Replace the single-select agent filter with a multi-select agent filter, allowing multiple agents to be selected at once.
- Improve cross-platform path handling for more reliable session discovery on Windows and WSL.

**Bug fixes**

- Fix Amp tool output rendering so tool results display correctly in expanded tool blocks.

**Acknowledgements**

- Thanks to [Michael Chapman](https://github.com/MCBoarder289) for surfacing Copilot reasoningText as thinking blocks.
- Thanks to [CL Kao](https://github.com/clkao) for category-based tool-metadata dispatch for cross-agent consistency.
- Thanks to [Song Li](https://github.com/boltomli) for iFlow agent support, the multi-select agent filter, and cross-platform path handling fixes.
- Thanks to [Luis Colunga](https://github.com/sinnet3000) for fixing missing Amp tool output in the UI.

---

## 0.11.0
<small>2026-03-08</small>

**New features**

- Add Pi agent support for importing and viewing Pi conversations, including JSONL session parsing, thinking blocks, and tool calls.
- Add an `agentsview sync` command to populate the database without starting the HTTP server. Supports a `--full` flag to force a complete resync.
- Add session renaming (double-click or right-click context menu), soft delete with trash and undo, and pinned messages with a gallery view.
- Persist starred sessions in SQLite so stars survive server restarts. Existing localStorage stars are automatically migrated on first load.

**Improvements**

- Exclude single-turn sessions (one or fewer user messages) by default in session lists and analytics views to reduce noise. A new "Include single-turn" toggle in the sidebar filter dropdown opts back in.
- Normalize tool metadata across agents so tool calls from Gemini, Pi, and other agents display with the same metadata tags as their Claude equivalents.

**Bug fixes**

- Align timestamps between message headers and grouped tool call headers so they sit at the same right edge in all layouts.
- Show Copilot `reasoningText` as thinking blocks instead of discarding it.
- Extract Gemini tool results more reliably and recognize additional Gemini tool names (`replace`, `run_shell_command`, `grep_search`, `glob`, `list_directory`).

**Acknowledgements**

- Thanks to [TalesOfThales](https://github.com/userFRM) for OpenClaw agent support, removing the redundant toggle-thinking button, persisting starred sessions in SQLite, and session management with rename, trash/undo, and pinned messages.
- Thanks to [CL Kao](https://github.com/clkao) for storing tool-result content with a category blocklist, the sync subcommand that populates data without serving, and extracting Gemini tool-result content and expanding tool-name coverage.
- Thanks to [Cesar Arze](https://github.com/carze) for adding Pi agent support.

---

## 0.10.0
<small>2026-03-04</small>

**New features**

- Add Amp, VS Code Copilot, and OpenClaw agent support, including session parsing, filtering, and agent-colored display.
- Add a Tauri-based desktop app wrapper that runs the web UI in a native window (in development, not yet released).
- Add session starring with `s` key toggle and a starred-only filter for quick access to important sessions.
- Add switchable message layouts — Default, Compact, and Stream — cycled with the `l` key.
- Add collapsible sidebar grouping by agent to organize sessions by tool.
- Add block-type filtering to toggle visibility of user messages, assistant responses, thinking blocks, tool calls, and code blocks.
- Add a copy button on message headers to copy message content to the clipboard.
- Add inline subagent transcript viewing for Task and Agent tool calls, loading the full subagent conversation on demand.

**Improvements**

- Enrich Codex tool call display with structured formatting for bash, write_stdin, and apply_patch calls.
- Store tool result content alongside tool calls, with configurable category blocklists to control database size.
- Use a non-destructive database resync flow that preserves existing session data instead of dropping the database.
- Simplify the message viewer header by removing the dedicated toggle-thinking button (use block-type filtering instead).

**Bug fixes**

- Return an empty array instead of `null` from the sessions API when no sessions match.
- Fix dashboard date handling in analytics and insights views for consistent date-range filtering.

**Acknowledgements**

- Thanks to [Luis Colunga](https://github.com/sinnet3000) for Amp agent support and centralizing the agent registry.
- Thanks to [Christoph Deil](https://github.com/cdeil) for returning an empty array instead of null from the sessions API and adding VSCode Copilot agent support.
- Thanks to [thellert](https://github.com/thellert) for subagent transcript viewing for Task and Agent tool calls.
- Thanks to [TalesOfThales](https://github.com/userFRM) for the per-message copy button, block-type filtering by content category, collapsible sidebar grouping by agent, switchable message layouts (default, compact, stream), and session starring with localStorage persistence.

---

## 0.9.0
<small>2026-02-27</small>

**New features**

- Add Cursor agent support, including parsing Cursor sessions and filtering them in the UI.

**Improvements**

- Show tool call parameters in expanded tool blocks for better execution visibility.
- Restrict CORS to `localhost` origin to improve local security defaults.

**Bug fixes**

- Fix `install.sh` version parsing so installs work correctly with minified release JSON.

**Acknowledgements**

- Thanks to [procrypt](https://github.com/procrypto) for restricting CORS to the localhost origin.

---

## 0.8.0
<small>2026-02-26</small>

**New features**

- Replace the project dropdown with a filterable typeahead to find and switch projects faster.

**Improvements**

- Improve resync reliability so session data stays in sync more consistently.
- Polish startup UX to make initial loading clearer and smoother.

**Bug fixes**

- Fix Gemini message parsing and display for thinking blocks.
- Fix text selection inside tool call blocks.

**Acknowledgements**

- Thanks to [CL Kao](https://github.com/clkao) for the session DAG view with fork detection and subagent linking.
- Thanks to [Marius van Niekerk](https://github.com/mariusvniekerk) for fixing text selection inside tool-call blocks and the filterable project typeahead.

---

## 0.7.0
<small>2026-02-25</small>

**New features**

- Add a session DAG view that detects forks and shows branch relationships.
- Add inline subagent linking so parent and subagent conversations are connected in context.
- Add an `All` option to the Analytics date range picker.
- Add a `Hide unknown` filter to reduce noise from unclassified items.

**Improvements**

- Improve Gemini project resolution for more accurate project attribution.
- Update Gemini model handling to support newer model variants.

**Bug fixes**

- Fix Analytics message counting so totals match actual message data.
- Fix agent color assignment for more consistent visual identification.

---

## 0.6.0
<small>2026-02-25</small>

**New features**

- Support multiple Claude project directories, including mixed Windows/WSL setups.
- Add session filters to quickly narrow the session list and analytics views.
- Add in-app resync so you can refresh data without restarting AgentsView.
- Support theme control via `postMessage` when embedding AgentsView in an iframe.

**Improvements**

- Condense watcher warnings to reduce noisy sync notifications.
- Add a polling fallback for unwatchable directories to keep sync running reliably.

**Bug fixes**

- Fix an app freeze when search returns no results.
- Fix Gemini project detection so sessions map to the correct project.

---

## 0.5.0
<small>2026-02-25</small>

**New features**

- Add Copilot CLI session support so Copilot CLI conversations appear in AgentsView.
- Add OpenCode session history support so OpenCode conversations can be synced and viewed.
- Show a copyable Session ID in the session detail header for quicker sharing and lookup.

**Improvements**

- Improve session source classification and sync coverage for newly supported tools.
- Follow symlinks during project discovery so linked workspaces are detected correctly.
- Improve interrupted-run recovery so valid sync results are preserved more reliably.

**Acknowledgements**

- Thanks to [Shreyas Karnik](https://github.com/shreyaskarnik) for the copyable Session ID in the session detail header.
- Thanks to [CL Kao](https://github.com/clkao) for following symlinks when discovering project directories.
- Thanks to [thellert](https://github.com/thellert) for theme control via postMessage for iframe embedding.
- Thanks to [Nat Torkington](https://github.com/njt) for multi-directory Claude project support (including mixed Windows/WSL), condensed watcher warnings, and a polling fallback for unwatchable directories.

---

## 0.4.1
<small>2026-02-24</small>

**Improvements**

- Speed up sync to reduce startup and refresh time.
- Improve startup logging to make initialization progress clearer.

**Bug fixes**

- Fix insights generation/store behavior to improve stability and accuracy.
- Restore `CLAUDE_NO_SOUND=1` for Claude subprocesses to keep background runs silent.

---

## 0.4.0
<small>2026-02-24</small>

**New features**

- Add AI-powered Insights with multi-agent generation for session analysis.
- Introduce an Insights page to view and manage generated insights.
- Add structured tool-call metadata for clearer, richer tool output display.

**Improvements**

- Normalize worktree project names so sessions stay grouped consistently across worktrees.
- Improve message and tool block presentation for better readability.

**Bug fixes**

- Fix insight generation authentication failures.
- Skip oversized JSONL lines during ingest so one bad line no longer fails the whole session.
- Embed the IANA timezone database to improve timezone support on Windows.

---

## 0.3.2
<small>2026-02-23</small>

**New features**

- Add session continuity via `sessionId` chaining, so resumed conversations stay linked as one ongoing session across sync/import.

**Improvements**

- Improve sidebar session list and item behavior to better represent continued sessions.
- Make session list state updates and virtualization smoother for larger histories.

**Bug fixes**

- Fix cases where continued conversation segments appear as separate sessions instead of a single chain.
- Fix Claude parsing/sync edge cases so session links persist reliably.

---

## 0.3.1
<small>2026-02-22</small>

**Bug fixes**

- Keep exported and gist HTML styling in sync with current app design tokens for consistent visuals.

---

## 0.3.0
<small>2026-02-22</small>

**New features**

- Add an Activity Timeline in Analytics to visualize session activity over time.

**Improvements**

- Improve the message viewer with clearer rendering for code blocks, tool calls, and thinking content.
- Refine the analytics experience for easier session exploration and readability.
- Improve sync performance to load and refresh data faster.
- Update project dependencies to improve compatibility and stability.
- Standardize session count formatting with explicit `en-US` locale handling for consistent display.

---

## 0.2.2
<small>2026-02-22</small>

**Improvements**

- Reduce Hour-of-Week heatmap cell size by 20% for a denser, easier-to-scan analytics view.

**Bug fixes**

- Fix available-port probing to bind the correct host, improving startup reliability.
- Ensure builds compile without frontend assets by providing `go:embed` stubs.

---

## 0.2.1
<small>2026-02-21</small>

**Improvements**

- Improve Codex output parsing so edge cases are handled more reliably.

**Bug fixes**

- Make installer exit on `SHA256SUMS` download failures to prevent incomplete or unverified installs.

---

## 0.2.0
<small>2026-02-21</small>

**New features**

- Render Codex function calls as informative tool blocks in session views and exports.

**Improvements**

- Make the activity calendar full-width and centered for a cleaner analytics layout.
- Increase heatmap cell size (about 20%) to improve readability.
- Combine day and hour activity charts into a more streamlined analytics view.
- Default analytics to a 1-year date range instead of 30 days.
- Match gist/export styling with the app's visual theme for consistent output.
- Filter out system-injected user messages from the message list display.

---

## 0.1.0
<small>2026-02-21</small>

**New features**

- Launch AgentsView with an integrated Go backend and Svelte frontend.
- Add analytics as the home screen with a persistent sidebar.
- Add rich dashboard views, including summary cards, activity timeline, project breakdown, top sessions, and session shape metrics.
- Add hour-of-week heatmap analytics with timezone display and interactive filtering.
- Add tool-aware analytics, including tool usage, thinking/tool counts, velocity metrics, and agent comparison.
- Add analytics permalinks, drill-down navigation, transitions, and CSV export.
- Add Gemini CLI session ingestion.
- Add tool-call ingestion and parsing (including Codex function calls) for deeper analytics.
- Add self-update support and cross-platform install scripts.
- Add frontend/backend version tracking with mismatch detection.

**Improvements**

- Improve dashboard usability with centralized active filter badges, project filtering, top sessions, and layout updates.
- Improve session pane performance and virtualized list stability for deep scrolling.
- Load sessions and selected session messages fully by default for smoother browsing.
- Improve search UX by scrolling to and highlighting the matched message.
- Keep sessions newest-first while showing messages in chronological order.
- Refresh branding with the AgentsView name, periscope logo, and favicon.
- Update assistant message colors to a warmer, higher-contrast palette.

**Bug fixes**

- Fix analytics drill-down behavior so heatmap and project clicks filter and navigate correctly.
- Fix `prune --max-messages` so it counts user messages correctly.
- Fix Claude parsing to skip system-injected user messages.
- Fix chart panels so they stay within dashboard grid bounds.
- Fix route and query handling to reject invalid routes and empty filters.
- Fix concurrent load/search race conditions to keep results consistent.
- Harden markdown sanitization to prevent XSS and preserve escaped content.
- Fix theme persistence when browser `localStorage` is unavailable.
- Fix session-loading edge cases, including short session IDs and empty Gemini directories.
- Fix Darwin release builds by targeting the macOS 15 runner.
