# Design: Antigravity Token Usage Extraction & Sync

This document details how we extract, parse, and sync model token usage
(including input, output, and reasoning tokens) for Antigravity (IDE) and
Antigravity CLI sessions.

## Background

Antigravity CLI and IDE sessions store conversation steps in SQLite `.db`
databases. Currently, the parser extracts timestamps and message roles/contents
but ignores token usage. To enable token usage tracking and cost calculations,
we need to decode LLM generation metadata stored in the database's
`gen_metadata` table.

## Requirements

1. **Token Extraction:** Retrieve input (context) tokens, output (candidate)
   tokens, and reasoning tokens from `gen_metadata` for each step.
1. **Model Identification:** Extract the LLM model name (e.g.,
   `Gemini 3.5 Flash (Medium)`) from the generation metadata.
1. **Session Rollup:** Aggregate token usage at the session level to calculate
   peak context size and total output tokens.
1. **Analytics Integration:** Emit usage events so daily stats and cost charts
   correctly reflect Antigravity usage.
1. **Robust Fallbacks:** Handle older database versions that lack the
   `gen_metadata` table gracefully without failing the sync.

## Proposed Changes

### 1. Protobuf Metadata Parser (`internal/parser/antigravity.go`)

We will implement two new recursive protobuf field walkers in
`internal/parser/antigravity.go`:

- `extractTokenUsage(data []byte) (input, output, reasoning int, ok bool)`:
  Locates the token usage block by looking for a nested message where `Field 1`
  is a model-kind varint in `[1000, 5000)`. Within that block:

  - `Field 5` = Context/Input tokens
  - `Field 2` = Output/Candidate tokens
  - `Field 3` = Reasoning tokens

  Persisted `OutputTokens` (message fields, usage events, session totals) is
  `field2 + field3`: cost paths price `OutputTokens` only, so reasoning folds
  into the billable output — matching the Gemini parser's thoughts handling —
  while `ReasoningTokens` is kept separately as a breakdown.

  **False-positive guard:** the heuristic (`field1 ∈ [1000, 5000)`) is broad
  enough to match other unrelated nested messages — for example, a
  nanosecond-latency counter in a scheduler event whose `field1` happens to fall
  in range. To reject these, `extractTokenUsage` requires `field5` (input) to be
  present as a varint — a real generation always consumes input context, while
  observed false-positive blocks lack the field — and requires `field2`,
  `field5`, and (when present) `field3` to be varints `≤ maxPlausibleTokens`
  (2,000,000). `field3` stays optional because zero reasoning tokens is
  legitimate and proto3 omits zero values from the wire, but a present `field3`
  with a non-varint wire type rejects the block. Blocks failing any check are
  skipped and the walk continues to find a legitimate block.

- `extractModelName(data []byte) string`: Recursively locates the model string
  from `Field 21` (or `Field 19`).

We will update `loadAntigravityStepsWithRawCount` to:

- Query `SELECT idx, data FROM gen_metadata`.
- Decode and map the metadata to the corresponding steps.
- Set `ContextTokens`, `OutputTokens`, `HasContextTokens`, `HasOutputTokens`,
  and `Model` on `ParsedMessage` instances.
- Append `ParsedUsageEvent` structs to populate the daily cost analytics. Usage
  extraction is independent of step decoding: a step the heuristic cannot render
  into a message (later rescued by the CLI trajectory sidecar transcript) still
  contributes its usage event, with token fields and model attached to the
  message only when the step decoded. Both parsers return usage events even for
  zero-message parses, so persisted sessions never carry event-derived token
  totals without the matching usage rows.

### 2. Caller Signatures & Rollups

- Update `ParseAntigravitySession` in `internal/parser/antigravity.go`:

  ```go
  func ParseAntigravitySession(
      path, project, machine string,
  ) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, error)
  ```

  Call `accumulateMessageTokenUsage(sess, messages)` to compute session totals
  before returning.

- Update `ParseAntigravityCLISessionWithStatus` in
  `internal/parser/antigravity_cli.go`:

  ```go
  func ParseAntigravityCLISessionWithStatus(
      path, project, machine string,
  ) (*ParsedSession, []ParsedMessage, []ParsedUsageEvent, ParseStatus, error)
  ```

  Call `accumulateMessageTokenUsage(sess, messages)` to compute session totals
  before returning, then `applyUsageEventTokenTotals(sess, usageEvents)`. The
  usage-event set is a superset of per-message token metadata (every
  token-bearing message derives from a gen row that also emits an event, and
  undecodable steps emit events with no message), so when events exist the
  session totals are recomputed from the full event set. This covers transcripts
  that dropped steps — sidecar wins, undecodable rows — without double counting.
  Both `ParseAntigravitySession` and `ParseAntigravityCLISessionWithStatus`
  apply it.

- Update sync orchestration in `internal/sync/engine.go` to pass the returned
  usage events into the final `ParseResult`.

### 3. WAL-Aware Skip Checks for IDE Sessions

IDE conversation `.db` files run in WAL mode: live updates land in `<id>.db-wal`
without changing the main file's size or mtime, so skip checks keyed on the main
file alone never reparse an active session. `AntigravityFileInfo` combines the
`.db` with its `-wal`/`-shm` siblings, the `annotations/<id>.pbtxt` sidecar, and
the `brain/<id>` artifacts (`*.md` plus `.metadata.json`) the parse renders as
messages, mirroring `AntigravityCLIFileInfo` — which includes the same brain set
for both `.db` and `.pb` sessions, since the CLI parser renders brain artifacts
too. The sync engine uses it for `processFile` skip checks,
`discoveredFileMtime` cutoff filtering, and watcher `SourceMtime` polling, and
the parser persists the same composite as the session's file fingerprint. The
file-watcher classifier also maps `annotations/<id>.pbtxt` and `brain/<id>/*`
events to `conversations/<id>.db` (when that DB exists) so sidecar-only updates
trigger the watcher path rather than waiting for periodic sync. The fingerprint
is computed after the parse's own read-only open, which can itself touch the
`-shm` sidecar; statting afterwards keeps the persisted value identical to what
the next sync observes, so unchanged sessions still skip.

## Testing Plan

1. **Unit Tests:** Add unit tests to `internal/parser/antigravity_test.go`
   verifying:
   - Protobuf extraction parses input, output, and reasoning tokens accurately.
   - `gen_metadata` querying degrades gracefully when the table does not exist.
   - Session-level totals are rolled up correctly.
1. **Integration Checks:** Run local tests with `make test` to ensure all
   existing and new tests pass.
