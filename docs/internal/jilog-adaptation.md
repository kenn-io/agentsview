---
last_edited: 2026-09-24
---

# jilog adaptation provenance

AgentsView's Friction Log adapts the session review from
[jilog](https://github.com/Joi/jilog) at commit `9e8e094` (workspace version
0.8.1). jilog's detectors, thresholds, digest format, and tests are the
behavioral reference. This page records the source, the changes, and the MIT
notice. Source paths below are relative to that jilog commit.

PR 1 provides pure detection, signal, formatting, and JSON packages. PR 12
adds the pure event ledger model and file codec. The archive adapter,
persisted review, digest rendering, scheduling, NanoClaw integration, and
Kata filing are planned for later PRs. Descriptions of those
parts below record the approved mapping; they do not describe PR 1 behavior.

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

- AgentsView parsers and its archive replace jilog's transcript readers. An
  adapter will build detector input from archived sessions in PR 2.
- Existing retry, runaway-loop, edit-churn, mid-task-compaction, and
  context-pressure signals replace jilog's `stuck_loop` and
  `compaction_storm`. The review will expose them as pattern kinds.
- Session parent relationships replace jilog's 16-zero sub-agent ID prefix.
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

## Planned additions

- `frustration` and `interruption` kinds will use AgentsView's existing
  frustration markers and interrupted-turn rows. They do not change a jilog
  kind.
- Archived tool calls will supply error, P0, and pattern findings. jilog's
  AgentsView reader cannot produce those findings from its session rows.
- Detection and digests will be on by default, as running jilog makes them.

## Deliberate differences

- Titles use `[friction/<kind>]`, labels use `friction`, and the planned digest
  heading is `# Friction Log — <date>`.
- The planned adapter removes system, compact-boundary, and tool-result rows
  from every correction stream, extending jilog's NanoClaw rule to all
  sessions.
- The planned adapter drops thinking blocks and tool renderings from stored
  assistant content, matching jilog's text-block-only extraction.
- The planned adapter synthesizes error envelopes from tool rows and maps the
  `Bash` tool category to `bash` for the noise allowlist.

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
- The `ledger-spool` writer and ingester and `commands/spool.rs` emit, ingest,
  and status flows. Publication uses no-clobber hard links; bad incoming files
  remain available for inspection and retry.

Storage and import (PR 13):

- Database rows are the authority instead of per-zone SQLite index files.
  Segment files remain the import, export, and interop format.
- No-clobber publication, directory listing, and import keep jilog's identity
  check, checksum verification, skip-and-retry behavior, and separate listing
  errors. `ledger rebuild-index` reprojects stored segments in one transaction.
- Each projected event stores its exact serialized bytes alongside class and
  tier in serde form. Verify checkpoints live in the archive per zone and
  source.
- The archive replaces `fleet_store_path` and the separate mirror-zone index
  refresh. `spool_authority` marks the ingest host. The configured ledger
  source or installation-ID source replaces jilog's hostname source.

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
