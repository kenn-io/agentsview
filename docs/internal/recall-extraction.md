# Recall Extraction

This document records the design contracts of automatic recall extraction: the
subsystem that distills ended sessions into recall entries with a local model.
The implementation lives in `internal/recall/extract` (segmenter, client,
manager), `internal/db/recall_extract.go` (generation and progress storage),
`internal/config/recall.go` (the `[recall.extract]` section), and
`cmd/agentsview/recall_extract.go` plus `extract_scheduler.go` (CLI and daemon
wiring).

## Generations and fingerprints

An extraction *generation* is one distillation configuration's corpus. Its
fingerprint is a SHA-256 over everything that changes model output:

- the extraction protocol version (bumped when client recovery semantics change
  in output-affecting ways);
- the model identity — model name plus an optional *deployment* label for setups
  where two deployments serve different weights under one name. The server
  endpoint is deliberately excluded: moving the same deployment to a new
  address must not orphan the corpus;
- the segmenter name and parameters;
- a digest of each prompt by role; and
- the request shape (temperature, max_tokens, extra body).

Changing any of these builds a new generation rather than mixing outputs. At
most one generation is *active* at a time; the lifecycle is
`building → active → retired`, enforced in one transaction by
`ActivateExtractGeneration`.

## Segmentation

`TurnsV1` derives units at user/assistant turn granularity: each non-system user
message becomes one *intent* unit, and each run of assistant messages between
user messages is packed into *action* units of at most `max_window_chars` code
points. Derivation is deterministic — resume cursors index into the unit list
and entry identity embeds the unit index, so replaying a session after a restart
or upgrade must yield the same units in the same order.

## Privacy boundary

Eligibility is enforced in SQL by `ExtractCandidates` and re-checked in Go for
explicit single-session runs, so no path can feed an excluded session to the
model. A session is eligible only when all of the following hold:

- it has ended, and ended before the configured quiet period cutoff (so a
  session that resumes shortly after ending is not extracted mid-way);
- it is not automated, not trashed, and has messages;
- it has zero secret findings **and** its last secret scan was performed under a
  rules version the current binary considers fresh
  (`secrets.ActiveRulesVersions()`). An unscanned session — leak count zero
  but no recorded scan version — is excluded. The candidates query refuses to
  run without scan versions at all: the boundary fails closed.

None of these predicates are configurable. The quiet period is a scheduling
knob; the privacy predicates are not.

## Progress and resume

`recall_extract_progress` records one row per (session, generation):
`unit_cursor` counts completed units, `units_total` the derived unit count,
`content_digest` a SHA-256 over the derived unit list, and a state machine
`pending → partial → done | failed`. Mutations use optimistic concurrency —
cursor advances and failure marks carry the digest and expected cursor, and a
mismatch means another writer reset or took over the session; the caller
re-reads instead of overwriting. Cursor advances are strictly monotonic and can
never resurrect a failed or done row.

A failed session waits out `failure_backoff` before the scan offers it again, so
one poisoned transcript cannot monopolize passes. Cancellation (daemon shutdown)
leaves the row resumable instead of burning the backoff.

## Content-change reconciliation

Hashing the *units* rather than the raw messages means digest stability tracks
exactly what the model would see. When a session's transcript changes after
extraction — most commonly an assistant run that grew and re-packed into an
existing unit — the digest differs, and the manager rebuilds that session's
generated corpus: it deletes the session's entries under this generation whose
review state is still `unreviewed_auto`, resets progress to cursor zero, and
re-extracts. Entries a human has touched are preserved.

Entry ids are positional (`sha256` of generation fingerprint, session id, unit
index, entry index), so within one digest a replayed unit dedupes to zero new
rows; across digests the delete step prevents stale entries from lingering or
blocking their replacements.

## Model client recovery

Each unit is sent as one chat-completion call with strict client-side schema
validation of the response (servers may ignore `json_schema`). The recovery
ladder:

- transient failures (network errors, 408/429/5xx, malformed 200s) retry with
  exponential backoff, honoring `Retry-After`;
- permanent request rejections fail fast;
- context overflow (input too large) and persistent truncation (response capped)
  are typed errors meaning *split this unit*: the manager halves the text
  recursively down to a floor (`max_window_chars / 8`, capped at 2000 code
  points), below which splitting would only destroy context and the error
  surfaces instead.

There is no "retry with a compact prompt" path: a capped response silently loses
entries, so truncation always splits or fails.

## Scheduling

The daemon scheduler mirrors the embedding scheduler's shape:

- sync completions are debounced (30 s) into *incremental* passes, which scan
  for new eligible sessions and retryable failures;
- a backstop ticker (`backstop_interval`, default 1 h) runs *full* passes, which
  additionally revisit done sessions — but only those written to since
  extraction finished (`local_modified_at > progress.updated_at`), so full
  passes do not reload the whole archive;
- passes drop instead of queueing when one is already running, and a dropped
  backstop carries into the next debounced pass.

Concurrency is one pass at a time, one session at a time, one unit per model
call.

## Activation

A building generation auto-activates when everything currently eligible is done,
nothing is pending or partial, and the generation has produced at least one
entry. Failed sessions do not block activation — they retry later and top the
corpus up. An entryless generation never activates, manually or automatically:
activation retires the previously active generation, and replacing a working
corpus with an empty one must not happen silently.

## Entry mapping

Extracted entries are stored `unreviewed_auto` / `accepted`, scoped to the
project, with: title and body from the model (entity lists folded into the body
as an `Entities:` line), an empty trigger, session context (project, cwd, git
branch, agent), the source session id, the generation fingerprint as
`source_run_id`, the segmenter name as `extractor_method`, and one evidence row
spanning the unit's message-ordinal range. Generated entries remain outside
trusted Recall until a separate promotion decision.

## CLI and ownership

`recall extract` subcommands: `run` (one pass; `--session` bypasses the quiet
period but never the privacy filters; `--full` revisits changed done sessions),
`status`, `activate`, `retire <fingerprint> [--force]`, `doctor` (prints the
resolved configuration and fingerprint, then makes one tiny probe call), and
`preview` (the former `--dry-run` chunk preview; the legacy flags still work as
a silent fallback).

Manual write commands refuse while a daemon owns the archive — a daemon with
`[recall.extract]` enabled runs passes itself, and there is no extraction HTTP
seam yet. They also reject `--server` rather than silently operating on the
local archive while the user targets a remote daemon.
