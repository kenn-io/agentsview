---
last_edited: 2026-09-24
---

# jilog adaptation provenance

AgentsView's Friction Log adapts the session review from
[jilog](https://github.com/Joi/jilog) at commit `9e8e094` (workspace version
0.8.1). jilog's detectors, thresholds, digest format, and tests are the
behavioral reference. This page records the source, the changes, and the MIT
notice. Source paths below are relative to that jilog commit.

PRs 1–5 provide detection, the archived-row adapter, persisted findings,
digest rendering, and scheduled review. PRs 12–15 provide the ledger model,
archive storage, replication, and query surfaces. NanoClaw integration and
Kata filing are separate roadmap branches. Descriptions below record the
approved mapping where code has not joined this branch yet.

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
- Observed-spend role and model names are sanitized in Markdown so a backtick
  or control character cannot break a line. JSON keeps the original map keys.

## Digest goldens

`internal/friction/testdata/golden/friction-log.md` is jilog's
`crates/jilog-review/tests/golden/learning-digest.md` (at `9e8e094`), and
`summary.json` is `crates/jilog/tests/golden/review-nightly.json`. The fixture
builders port `tests/golden_digest.rs` and `digest_report()` in
`crates/jilog/src/commands/review.rs`. The only Markdown differences are the D8
heading, scrubbed fixture strings, the two D36 frontmatter keys and the two D36
sections, which are empty here. Every jilog line keeps its bytes. The JSON
differences are the `kata` backend, the `digest_path` meaning, `schema_version`
3 and the two D36 count keys:

```diff
 patterns: 1
+frustrations: 0
+interruptions: 0
 ---
-# Learning Digest — 2026-09-16
+# Friction Log — 2026-09-16
+- `842c45ce-77b2-4d72-b995-f2a10466eb40` — 'do calendar re-auth' (recurred in sessions totaling $4.20)
+- `helper@general` `seat:seat-02` `chat-1` — 'no, use the gh cli'
 - `seat:seat-03` `ee58d934-1049-4da0-b5b3-9a00f50efcc7` kind=`stuck_loop`: `bash` x6 identical arguments 01:35-01:54

+## Frustration
+
+_No frustration detected._
+
+## Interruptions
+
+_No interruptions detected._
+
+- `helper@general`: 1 corrections, …
```

```diff
-      "backend": "github",
+      "backend": "kata",
-  "digest_path": "/tmp/learning-digest-2026-05-10.md",
+  "digest_path": "friction:2026-05-10",
+  "frustrations": 0,
+  "interruptions": 0,
+    "helper@general": {
+      "channel": "general",
+      "persona": "helper",
-  "schema_version": 2,
+  "schema_version": 3,
```

`friction-log-extra-kinds.md` has no jilog counterpart. It pins the D36 sections
with content: a frustration line with both annotations, a persona frustration
line, and interruptions counted per session.

The pattern line keeps jilog's `stuck_loop` kind. The renderer prints the kind
verbatim, and agentsview's `retry_loop` evidence has the same shape. Ported unit
tests scrub personal, channel and machine names the same way, and replace the
fixture time zone with another UTC+06 zone. The `+` lines above show the
scrubbed fixture strings; jilog's originals are not reproduced here.

Zone resolution differs from jilog `zone.rs`. `AGENTSVIEW_FRICTION_TZ` and
`[friction] timezone` replace `JILOG_TZ` and the config key. After them,
`timeutil.LocalLocation()` covers jilog's `TZ` and system-zone steps and falls
back to the process zone instead of UTC. An empty configured timezone means
unset rather than an error.

## Review orchestration (roadmap PR 5)

Ported from jilog `9e8e094` `crates/jilog-review/src/digest.rs:208-706`
(`run_review`), `reader.rs:233-270` (processed sessions) and
`archive_spend.rs:83-109`.

- **Added:** `frustration` and `interruption` kinds from agentsview's own
  detectors flow through digests and recurrence (not counted in the persona
  line); generic diagnostic subjects (`subject_kind = diagnostic`) replace
  jilog's worker records and are excluded from P0 by kind, not by tool name.
- **Kept:** detector order and run order within each kind, dimension
  stamping, persona rollup including sessions without signals, spend
  accumulation with `(root)` role keys, P0 alerts over the digest's errors,
  archive-spend window and summarizing, empty digest on the first build of a
  date, dry run writes nothing.
- **Replaced:** the processed-sessions file is `friction_digest_sessions`;
  "nightly" is an hourly catch-up over complete local dates; a date is built
  once (jilog's same-date preservation rule becomes build-once plus explicit
  rebuild); subjects are dated by last activity instead of the run day;
  archive spend reads native daily usage instead of shelling out; per-session
  cost is archive microdollars (scale 6), so a jilog `$1.50` renders
  `$1.500000`; sub-agent spend is keyed `subagent`.
- **Dropped:** reader discovery windows and the retry sidecar (the filing
  outbox replaces it in the Kata PRs); jilog's private worker collectors and
  pool-seat conventions (seats come only from user `seat_patterns`).

## Event ledger

`internal/ledger` adapts jilog's `crates/ledger-core` (`event.rs`,
`segment.rs`, `store.rs`), `crates/ledger-spool/src/lib.rs`
(`valid_source_name`) and `crates/jilog/src/commands/query.rs` at the same
pinned commit. The codec reproduces jilog's segment bytes; the float
verification limit is described below.

Kept:

- The event model with its ten classes and three tiers, field order, and
  `null` for absent optional fields.
- CRC-32 (IEEE) over the compact serde serialization of the events array,
  "sealed" meaning a non-zero checksum, and deep content comparison of
  duplicate identities.
- The pretty segment-file format and the `{source}-{seq:06}.json` name,
  parsed at the last `-`.
- The source-name rule `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`.
- Incremental verification with a per-source watermark, remembered failures
  and remembered gaps.
- The `jilog query` `--since` grammar, class names, subsystem globs, and the
  text and JSON output formats.

Storage and import (PR 13):

- Database rows are the authority instead of per-zone SQLite index files.
  Segment files remain the import, export, and interop format.
- No-clobber publication, directory listing, and import keep jilog's identity
  check, checksum verification, skip-and-retry behavior, and separate listing
  errors. `ledger rebuild-index` reprojects stored segments in one transaction.
- Each projected event stores its exact serialized bytes alongside class and
  tier in serde form. Verify checkpoints live in the archive per zone and
  source.

Replication (PR 14) replaces jilog's file-synced spool (`spool emit` on each
host, `spool ingest` on one authority) with a phase of `agentsview pg push`.
The producer verifies each segment again before sending. The hub skips an
identical duplicate identity and refuses different content without overwriting
it. Per-zone `replicate` and `replicate_confidential` keep selected segments
local.

Planned in the query PR:

- Query filtering will move into SQL, so a filtered query can find older
  matches beyond the newest `5 x limit` events.

Added:

- Deterministic UUIDv5 event IDs (namespace
  `v5(URL, "agentsview:ledger:event-id")`, name `source NUL key`) for
  producers that re-emit the same fact.
- A bound of 1000 listed gaps per source; jilog enumerates every missing
  sequence number.

Dropped:

- The README-only `review` and `learning` classes, which the code never had.

Byte-compatibility fixtures are generated by jilog's own Rust code; see
`internal/ledger/testdata/README.md`. serde_json 1.0.149 formats floats with
an explicit exponent sign (`1e+16`) and uses decimal notation for magnitudes
from `1e-5` up to `1e16`; `internal/serdejson` matches it. jilog builds
serde_json without `float_roundtrip`, so jilog's own `verify()` rejects some
segments holding floats that it wrote correctly; AgentsView verifies them
(`fixture-lossy-000001.json`). Payloads that a jilog host must verify should
use integers and strings.

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
