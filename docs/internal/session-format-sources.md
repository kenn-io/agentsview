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

Grok is temporarily excluded at the user's request because a separate
format-alignment change owns that provider. Once that alignment lands, add a
Grok section and remove the explicit registry exception in the coverage test.

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
- **Agentsview:** `internal/parser/claude.go` and
  `internal/parser/claude_provider.go`; local observations and fixtures are
  the implementation evidence for fields not documented upstream. Reverified
  2026-07-22 against local CLI transcripts: `type=attachment` records with
  `attachment.type=queued_command` are written mid-stream, in file order
  between consecutive `assistant` records that share one `message.id`, so a
  queued command can fall inside a streaming run that straddles an incremental
  sync boundary.

## OpenClaude (`openclaude`)

- **Format:** OpenClaude JSONL with Claude-compatible message content and usage
  objects.

- **Evidence:** `source`.

- **Upstream:** Clone `https://github.com/Gitlawb/openclaude.git` at
  `1ddb7d68399a2cd5028d4c5f487676f941879eae`; see the pinned
  [session JSONL writer](https://github.com/Gitlawb/openclaude/blob/1ddb7d68399a2cd5028d4c5f487676f941879eae/src/utils/sessionStorage.ts),

    [project-directory resolver](https://github.com/Gitlawb/openclaude/blob/1ddb7d68399a2cd5028d4c5f487676f941879eae/src/utils/envUtils.ts),
    and
    [assistant message type](https://github.com/Gitlawb/openclaude/blob/1ddb7d68399a2cd5028d4c5f487676f941879eae/src/types/message.ts).

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

- **Format:** Rollout JSONL files, with a separate JSONL session index used for
  discovery and metadata.
- **Evidence:** `source`.
- **Upstream:** Clone `https://github.com/openai/codex.git` at
  `3e2f79727a4e8ddfc8e3acb838d496b121094b9e`; see the pinned
  [rollout recorder](https://github.com/openai/codex/blob/3e2f79727a4e8ddfc8e3acb838d496b121094b9e/codex-rs/rollout/src/recorder.rs)
  and
  [protocol types](https://github.com/openai/codex/blob/3e2f79727a4e8ddfc8e3acb838d496b121094b9e/codex-rs/protocol/src/protocol.rs).
- **Usage and cost:** `token_count` records include total and last usage with
  input, cached input, cache-write input, output, reasoning output, and total
  tokens. Agentsview currently consumes input, cached input, and output only:
  it subtracts cached input from upstream's inclusive input total, maps cached
  input to cache-read, and ignores cache-write and reasoning-output fields.
  Catalog pricing therefore covers only the normalized fields the parser
  emits.
- **Agentsview:** `internal/parser/codex.go` and
  `internal/parser/codex_provider.go`; usage is taken from the last-turn
  counters rather than repeatedly counting cumulative totals.

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
  `internal/parser/copilot_provider.go`.

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
- **Agentsview:** `internal/parser/opencode.go`,
  `internal/parser/opencode_provider.go`, and
  `internal/parser/opencode_storage_state.go`; legacy and database layouts are
  both intentional compatibility targets.

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
  metadata with input, output, cache-read, and cache-write tokens, explicit USD
  cost, and `usageMissing` flag. `task_metadata.json` does not carry the
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
  role and attribution boundaries are reconstructed from Markdown.

## Amp (`amp`)

- **Format:** One JSON thread document per session.
- **Evidence:** `no-public-source`.
- **Upstream:** The first-party [Amp manual](https://ampcode.com/manual) and
  public Sourcegraph/Amp repositories were searched 2026-07-19; no
  session-file producer or authoritative disk schema was found.
- **Usage and cost:** The consumed thread documents do not expose token, cache,
  reasoning, credit, or USD fields to Agentsview.
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
  operation logs are supported.

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
  shapes. Model IDs are present. Agentsview catalog-prices the tokens.
- **Agentsview:** `internal/parser/pi.go` and `internal/parser/pi_provider.go`;
  alternate branches remain in the file but only the active ancestry is a
  conversation.

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
  artifacts.
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
  is catalog-derived when model attribution is possible.
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
  `lm-messages.jsonl`, and `ui-messages.jsonl`.
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
  cache-read, and cache-write tokens with model identity. Agentsview
  catalog-prices these values; no provider-reported USD total is consumed.
- **Agentsview:** `internal/parser/posit_assistant_provider.go`; current schema
  details are based on observed files and fixtures.

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

## Zed (`zed`)

- **Format:** `threads/threads.db`, whose thread payload is JSON or zstd-
  compressed JSON depending on generation.
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
  encrypted producer format.
- **Usage and cost:** Sidecar generator metadata can carry input, output,
  thinking-output, cache-read, and model fields; output already includes
  thinking. Agentsview avoids double counting and catalog-prices usage. No
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

- **Format:** OpenCode-compatible SQLite or legacy session/message/part storage.
- **Evidence:** `no-public-source`.
- **Upstream:** ICodeMate's first-party product pages, documentation, and public
  GitHub repository search were checked 2026-07-19 without finding producer
  source or an authoritative disk schema. The OpenCode source pinned in the
  `opencode` entry is compatible-family evidence only.
- **Usage and cost:** Compatible messages can persist input, output, cache-read,
  cache-write, and model identity. Agentsview catalog-prices these values and
  consumes no product-reported USD total.
- **Agentsview:** `internal/parser/icodemate.go` delegates to
  `internal/parser/opencode.go`; product-specific divergence is a known
  limitation.

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
  shape.
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
