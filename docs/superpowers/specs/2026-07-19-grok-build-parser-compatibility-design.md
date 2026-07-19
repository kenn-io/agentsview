# Grok Build parser compatibility design

## Context

AgentsView's Grok provider reads sessions from the public Grok Build layout:

```text
~/.grok/sessions/{url-encoded-cwd}/{session-id}/
  summary.json
  chat_history.jsonl
  signals.json
```

The parser was originally based on observed files. Grok Build is now source
available, so the persistence types and compatibility behavior can be checked
against the implementation. This design uses xAI's `xai-org/grok-build`
repository at commit `7cfcb20d2b50b0d18801a6c0af2e401c0e060894` as the
reference.

The upstream source confirms that Grok Build still loads two transcript
generations, including mixed files:

- format 1 uses a `type` discriminator and `ConversationItem` rows;
- format 0 uses a `role` discriminator and `ChatRequestMessage` rows.

It also confirms compatibility shapes within format 1: reasoning may be a
standalone row, an older inline `assistant.reasoning` object, or an older
`assistant.raw_output` entry.

## Confirmed gaps

The current AgentsView parser does not fully match those contracts:

1. It dispatches only on `type`, so format 0 transcripts produce no messages.
1. It reads `session_summary` before `generated_title`, opposite Grok Build's
   `display_title()` precedence.
1. It handles standalone reasoning summaries but not standalone reasoning
   `content` arrays, inline reasoning, or `raw_output` reasoning.
1. It ignores standalone backend tool-call rows even though Grok Build retains
   them as assistant-visible context.
1. It treats tagged synthetic user items as ordinary human prompts.
1. It maps the current `contextTokensUsed` snapshot to peak context usage, even
   though the source does not define that field as a peak.
1. It ignores persisted fork-parent metadata and the source-workspace grouping
   hint used by worktree sessions.

## Scope

The change will make AgentsView match every local transcript shape that the
current Grok Build loader accepts. It will remain isolated to the Grok parser
and provider tests.

Nested `subagents/{id}` directories will not be added to discovery. Grok Build
stores those child sessions under their parent and hides subagent session kinds
from its normal session listing. AgentsView's existing two-level discovery
therefore matches the upstream user-visible boundary.

The change will not parse `updates.jsonl`, reconstruct timestamps that are not
persisted on chat rows, expose encrypted reasoning, or infer aggregate output
tokens that Grok Build does not store in `signals.json`.

## Transcript normalization

`parseGrokChatHistory` will determine each row's schema independently. A row
with a recognized `type` uses the format 1 path. Otherwise, a recognized `role`
uses the format 0 path. This mirrors upstream's per-line fallback and supports
mixed histories without relying solely on `summary.chat_format_version`.

Both paths normalize into the existing `ParsedMessage` model:

- system rows remain omitted as vendor context;
- user rows extract text blocks and existing `<user_query>` wrappers;
- assistant rows preserve text, model ID, reasoning, and tool calls;
- tool rows become tool-result carrier messages for the sync engine's existing
  tool-result pairing;
- malformed JSON increments `MalformedLines`, while unknown valid row types are
  ignored.

Tagged synthetic user rows will be omitted from human prompt counts and the
visible transcript. The `interjection` reason is the exception because Grok
Build documents it as user-authored steering entered during a running turn.

Reasoning will use the same association rule as Grok Build: consecutive
standalone reasoning rows are buffered and attached to the following assistant;
an intervening backend tool call does not clear the buffer, while a user or tool
result does. Inline assistant reasoning is more specific and takes precedence
over buffered reasoning. Plaintext summary/content blocks are retained;
encrypted-only payloads are not surfaced.

Standalone backend tool calls will become assistant-visible context messages.
Their content will use stable, compact descriptions derived from the persisted
tool kind and action, and their identifiers and inputs will be retained as
`ParsedToolCall` data where representable. This preserves search/code activity
without pretending that Grok executed a client-side tool result.

## Summary and signal normalization

Session names will follow Grok Build's display order: nonblank
`generated_title`, then `session_summary`, then legacy `summary`.

For worktree sessions, `info.cwd` remains the session `Cwd`, while
`source_workspace_dir` is preferred when deriving the project grouping. The
existing git-root, worktree-label, and encoded-directory fallbacks remain.

A nonblank `parent_session_id` will populate the Grok-prefixed parent ID and
mark the relationship as a fork. This reflects the meaning assigned to the field
by the upstream `Summary` type.

Legacy explicit peak-context and total-output fields remain supported. Current
`contextTokensUsed` will no longer set `PeakContextTokens`; it is a current
snapshot and can decrease after compaction, so treating it as a peak creates a
false aggregate. `userMessageCount` remains the summary-only fallback because it
is the only persisted count available when chat history is absent.

## Golden samples and tests

The compatibility suite will include a checked-in, sanitized golden session
generated with the pinned Grok Build source rather than transcribed by hand. A
temporary Rust generator compiled inside the upstream checkout will serialize
the actual `Summary`, `SessionSignals`, and format 1 `ConversationItem` types.
Grok Build's own `chat-history-downgrade` binary will convert the same format 1
history into format 0 rows. The golden fixture directory will record the
upstream commit, generator source, and exact generation commands so its
provenance can be audited or reproduced without making Rust part of AgentsView's
normal test toolchain.

The golden bytes are inputs, not expected results. Go tests will call the public
provider parse boundary and compare the resulting session and messages with
hand-authored literal expectations. This separation ensures the source-derived
sample catches schema drift without using AgentsView's parser to generate its
own answers.

Small, hand-written JSON/JSONL fixtures will remain appropriate for isolated
branches that would be obscured by a full session. Each behavior change will
follow red-green-refactor:

1. format 0 user, assistant, tool call, tool result, model, and reasoning;
1. mixed format 0/1 rows;
1. current and legacy reasoning placement;
1. backend tool-call preservation;
1. generated-title precedence;
1. synthetic-message filtering with interjection preservation;
1. fork and source-workspace metadata;
1. rejection of current-context snapshot as peak usage while retaining legacy
   explicit peak fields.

Focused parser tests will run after each minimal fix. Completion verification
will include the full parser package tests, `go fmt ./...`, and `go vet ./...`
before the required commit.
