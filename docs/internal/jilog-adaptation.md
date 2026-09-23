---
last_edited: 2026-09-23
---

# jilog adaptation provenance

AgentsView's Friction Log adapts the session review from
[jilog](https://github.com/Joi/jilog) at commit `9e8e094` (workspace version
0.8.1). jilog's detectors, thresholds, digest format, and tests are the
behavioral reference. This page records the source, the changes, and the MIT
notice. Source paths below are relative to that jilog commit.

PR 1 provides pure detection, signal, formatting, and JSON packages. PR 2 adds
the pure archived-row adapter, pattern detection, and session review.
Persistence, digest rendering, scheduling, NanoClaw integration, and Kata filing
remain planned for later PRs.

## Kept in PR 1

- The correction, error, workaround, deferral, and pattern signal model
  (`crates/jilog-review/src/signal.rs:6-219`) maps to `friction.Signal`.
- Coding and chat correction detection keeps the ten chat marker patterns
  (`crates/jilog-review/src/detectors.rs:89-176`).
- Error detection keeps the `{"error","success"}` envelope rules, message
  precedence, and expected-noise rules for `mode` denials and content-free
  `bash` failures (`detectors.rs:205-488`).
- Workaround and deferral detection keeps the eight and nine patterns and
  labels, respectively (`detectors.rs:31-78,504-601`).
- P0 alerts require at least three distinct root sessions per tool
  (`detectors.rs:81,609-633`). Operational diagnostics remain excluded by
  subject kind, without a list of tool names.
- `iteration_runaway` requires 150 tool calls without an intervening user
  message; sub-agents are exempt (`health.rs:220-278`).
- Issue title construction (`tracker.rs:59-90`), `python_repr`, rune truncation,
  truncation with a marker (`util.rs:69-113`), display sanitization
  (`digest.rs:147-155`), and USD formatting (`digest.rs:1120-1127`) have pure
  Go equivalents. A source-specific title exception was dropped.
- `internal/serdejson` reproduces the relevant serde_json 1.x output: sorted
  keys, no HTML escaping, and float layout. This keeps JSON serialization
  byte-stable for future digests within the documented parity limits.

## Planned replacements

- Database tables replace the processed-sessions file and retry sidecar. A
  catch-up job will build each completed local day once, replacing the nightly
  run.
- A direct usage-rollup query will replace the `agentsview usage daily`
  shell-out for archive spend.
- A native Kata HTTP client will replace the Kata CLI tracker. Filing remains
  off until a Kata hub is configured.
- Generic NanoClaw persona and channel resolution, trust filtering, and
  message-envelope cleanup will retain the public NanoClaw behavior. They will
  be inactive until a NanoClaw data directory is configured.

## Kept in PR 2

- AgentsView parser and archive rows replace jilog's transcript readers.
  `BuildSessionInput` prepares detector input from those rows.
- Existing retry, runaway-loop, edit-churn, mid-task-compaction, and
  context-pressure signals replace jilog's `stuck_loop` and
  `compaction_storm` as pattern kinds.
- Session parent relationships replace jilog's 16-zero sub-agent ID prefix.

## Dropped

- `resume_storm`, whose only source is Amplifier `session:resume` events.
- The GitHub tracker, synthetic IDs from the none tracker, file-path helpers
  (`contract_tilde`, `expand_tilde`), and `run_with_timeout`.
- Collectors and rules tied to a particular worker setup: private diagnostic
  collectors, seats inferred from fixed pool-profile paths, a worker-specific
  title exception, and a P0 exclusion list of worker tool names. A configured
  source can provide generic diagnostics and seats in later PRs.
- Migration or matching of existing `[jilog/…]` issues. Friction Log starts
  with its own issue history.

## Additions

- `frustration` and `interruption` kinds use AgentsView's existing
  frustration markers and interrupted-turn rows. They do not change a jilog
  kind.
- Archived tool calls supply error and pattern findings. jilog's AgentsView
  reader cannot produce those findings from its session rows.
- Detection and digests are planned to be on by default, as running jilog
  makes them. P0 filing awaits the archive and Kata integration.

## Deliberate differences

- Titles use `[friction/<kind>]`, labels use `friction`, and the planned digest
  heading is `# Friction Log — <date>`.
- The adapter removes system, compact-boundary, and tool-result rows
  from every correction stream, extending jilog's NanoClaw rule to all
  sessions.
- The adapter drops thinking blocks and tool renderings from stored
  assistant content, matching jilog's text-block-only extraction.
- The adapter synthesizes error envelopes from tool rows and maps the
  `Bash` tool category to `bash` for the noise allowlist.

## Session input adapter

agentsview does not port jilog's file readers. `friction.BuildSessionInput`
maps archived rows into the message stream jilog's detectors read:

- System rows, compact-boundary rows and `tool_result` fallback rows are
  dropped from the stream. This applies jilog's NanoClaw rule to every
  session (D10) and closes jilog's Claude Code reader gap, which kept
  `isMeta` and compact-summary lines.
- Assistant text drops inline `[Thinking]` blocks and tool-call renderings,
  reproducing jilog's text-blocks-only extraction (D11).
- Every tool call becomes a `tool` message whose envelope is
  `{"error","success"}`, with success from `signals.IsFailure` (D12). The
  noise allowlist keys on `bash` for any Bash-category call.
- Pattern kinds reuse `internal/signals`: `retry_loop`, `runaway_loop`,
  `edit_churn`, `mid_task_compaction` and `context_pressure`.
  `iteration_runaway` is ported. `resume_storm` is dropped because no
  agentsview source records resumes (D14).

Architecture-forced deltas from jilog:

- jilog's `stuck_loop` fires at 4 identical calls. `retry_loop` fires at 3,
  because it is the same predicate as the Quality page's retry count.
- Compaction storms (3 compactions within 10 minutes) are replaced by
  mid-task compactions, the agentsview signal. The evidence range spans all
  compact boundaries in the session.
- A user message that mixes text and tool results keeps its text, because
  the echo part was removed at parse. jilog would skip it.

Additions beyond jilog:

- Frustration markers (`signals.IsFrustrationMarker`) and user interruptions
  (rows the Claude parser tags `interrupted`) are friction kinds of their
  own. They run after jilog's five kinds.
- `SeatFromPath` uses only the patterns its caller provides. Wiring those
  patterns to `[friction] seat_patterns` configuration is planned for a later
  PR. jilog's built-in pool-directory conventions are not carried over.

## Parity notes

- Go's RE2 `\b` is ASCII-only; Rust's word boundary is Unicode-aware. A
  corrective marker beside a non-ASCII letter (`wrongé`) matches here but not
  in jilog. `TestChatCorrectionWordBoundary` pins this accepted difference.
- RE2 `\d` is ASCII-only. A timeout sentence with fullwidth digits is
  reported as an error here and suppressed by jilog.
  `TestBareTimeoutDigitClass` pins the difference.
- serde_json 1.0.149 formats floats with zmij, not ryu.
  `internal/serdejson` ports zmij's layout. serde_json's default parser can be
  one unit in the last place off for some long literals, such as
  `12345678901234567.0`; Go parses them exactly, so those values print
  differently.
- `friction.ParseUSD` accepts plain decimals only. rust_decimal also accepts
  exponents and underscores, which AgentsView does not produce.
- An empty tool name becomes `unknown`; jilog does this only for a missing
  name.

## Digest golden deltas

The digest renderer is planned for PR 4. That PR will replace this note with
every difference between AgentsView's golden digest and jilog's
`tests/golden/learning-digest.md`.

## License

The adapted code is distributed under the MIT License:

Copyright (c) 2026 Joichi Ito

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
