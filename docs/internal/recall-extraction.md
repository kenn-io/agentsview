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
- its last secret scan was the current **full** scan (`secrets.RulesVersion()`).
  The definite-only inline sync scan does not qualify: it never looks for
  candidate-confidence secrets, so a session it cleared may still carry them.
  An unscanned session — leak count zero but no recorded scan version — is
  excluded, and the candidates query refuses to run without scan versions at
  all: the boundary fails closed. In practice this means sessions need
  `secrets scan --backfill` before they become eligible;
- it has zero recorded secret findings **of any confidence**. The leak count
  only counts definite findings; a candidate finding (a JWT, a high-entropy
  blob) is exactly the material that must not reach the model either, so the
  `secret_findings` table is consulted directly.

None of these predicates are configurable. The quiet period is a scheduling
knob; the privacy predicates are not.

Scan freshness is bound to the transcript at the storage layer: every transcript
mutation (append, replace, diff, tool-call relink) advances
`transcript_revision` and clears `secrets_rules_version` in the same
transaction, so a session whose transcript changed after its last scan is
ineligible until a rescan re-stamps it — the inline sync rescan restores only
the definite-only version, so extraction stays excluded until the next full
scan. The atomic replace path re-stamps inside the same transaction.

The eligibility check is also bound to the transcript actually sent: the manager
re-reads the session row (full column set — the standard list omits
`local_modified_at`) after loading its messages, compares the two reads (message
count, transcript revision, scan version, leak count, ended-at, last local
write), and re-runs the eligibility predicates against the second read —
trashing or flagging a session automated flips fields the comparison does not
watch. An unstable bracket skips the session silently: a concurrent write bumped
`local_modified_at`, so the next pass retries against a settled view, and a
newly ineligible session must simply not be extracted.

A stable bracket whose loaded message count differs from the row's is a
different case — the sync loop writes the session row before the transcript, so
the row can durably claim more or fewer messages than are stored, and no future
write may ever re-surface the session. That mismatch never reaches the model,
but it is recorded as a retryable *failure* (visible in status, re-offered after
the backoff) instead of skipped: a silent skip would let the discovery
watermarks advance past the session's writes and exclude it forever. The rule
holds for completed rows too — it is applied before the done short-circuit,
because a same-digest revisit would otherwise preserve `done` and settle the
coverage stamp, claiming the inconsistent state as covered. The failure mark
demotes such a row (`ReopenDone`) and resets its cursor to zero: its
completed-units claim was judged against the inconsistent session, and the
strictly monotonic cursor could otherwise never reach `done` again.

## Progress and resume

`recall_extract_progress` records one row per (session, generation):
`unit_cursor` counts completed units, `units_total` the derived unit count,
`content_digest` a SHA-256 over the derived unit list, `content_stamped_at` the
caller's transcript-read cutoff — captured *before* the messages were read, so a
write landing during derivation still compares as after the stamp, and taken on
every stable upsert including same-digest revisits, so a revisit settles the
stamp forward instead of leaving old metadata writes re-opening the session on
every full pass — and a state machine `pending → partial → done | failed`.
Mutations use optimistic concurrency — cursor advances and failure marks carry
the digest and expected cursor, and a mismatch means another writer reset or
took over the session; the caller re-reads instead of overwriting. Cursor
advances are strictly monotonic and can never resurrect a failed or done row.

A failed session waits out `failure_backoff` before the scan offers it again, so
one poisoned transcript cannot monopolize passes. Cancellation (daemon shutdown)
leaves the row resumable instead of burning the backoff.

A full resync carries progress rows into the rebuilt database with
`content_stamped_at` intact — an empty stamp reads as "changed since coverage"
for every completed session, so losing it would reload the whole archive's
transcripts on the next full pass. Archives written before the column existed
copy it as empty; those rows re-open once and settle on their first revisit.

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

Entries also copy session context — project, cwd, git branch, agent — at insert
time, and a metadata-only session update keeps the unit digest unchanged. A
same-digest revisit therefore synchronizes those fields on the session's
generated entries before settling the coverage stamp, so the corpus stops
matching Recall filters for the old context without any model calls.
Human-touched entries are left as they were, mirroring the delete path.

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
  for new eligible sessions and retryable failures. Their discovery is
  watermarked: only sessions written since the last completed unlimited pass
  (lagged by the quiet period, since a session becomes eligible that long
  after its final write) are examined for new work, so steady-state passes
  scale with recent activity rather than the archive. Sessions already in
  progress — pending, partial, retryable failed, revisitable done — are always
  offered through the progress-state index regardless of the watermark, and a
  session with no recorded local write stays discoverable. Explicit and
  limited passes never advance the watermark (they leave eligible sessions
  behind that a bounded discovery would then never see), and the advance is a
  ratchet — it only moves forward;
- a backstop ticker (`backstop_interval`, default 1 h) runs *full* passes, which
  additionally revisit done sessions — but only those written to since their
  unit snapshot was derived
  (`local_modified_at >= progress.content_stamped_at`; the stamp, not
  `updated_at`, because a write that lands mid-extraction predates the row's
  last touch but postdates what the model saw), so full passes do not reload the
  whole archive's transcripts. The revisit drives from the sessions side of
  the join so the planner can bound it with the `local_modified_at` index, and
  a second watermark bounds it the way the discovery watermark bounds
  discovery: only done sessions written since the last completed unlimited
  full pass are rechecked, so the hourly backstop walks recent writes instead
  of every completed progress row. The full watermark ratchets to the pass
  *start* with no quiet-period lag — a write landing during pass N carries a
  `local_modified_at` at or after N's start, so pass N+1 still sees it. Full
  passes bound their discovery by the same watermark incremental passes use.
  Unbounded reconciliation is the fresh-manager path: a daemon restart or a
  manual `recall extract run` starts with zero watermarks and scans everything
  once;
- when the backstop is disabled, a catchup ticker (paced by the quiet period,
  never faster than once a minute) runs incremental passes instead: sessions
  become eligible only after the quiet period, long after the last sync-driven
  debounce fired, so sync signals alone cannot guarantee eventual extraction;
- passes drop instead of queueing when one is already running, and a dropped
  backstop carries into the next debounced pass.

Concurrency is one pass at a time, one session at a time, one unit per model
call.

## Activation

A building generation auto-activates when everything currently eligible is done,
nothing is pending or partial, and the generation has produced at least one
entry. The backlog check counts completed sessions whose transcripts changed
since their unit snapshot: their corpus is stale, and activating over them would
promote a generation that does not cover what it claims. Failed sessions do not
block activation — they retry later and top the corpus up. An entryless
generation never activates, manually or automatically: activation retires the
previously active generation, and replacing a working corpus with an empty one
must not happen silently.

Generation state controls serving. While a generation is building, its entries
are staged with the `archived` status so an unfinished corpus never serves;
activation promotes them to `accepted` and archives the retired generation's
still-automatic entries in the same transaction, so the served corpus switches
atomically. Retiring a generation likewise archives its still-automatic entries.
Entries a human has touched (any review state other than `unreviewed_auto`) are
never moved by these flips.

## Entry mapping

Extracted entries are stored `unreviewed_auto` — `accepted` under an active
generation, staged `archived` under a building one — scoped to the project,
with: title and body from the model (entity lists folded into the body as an
`Entities:` line), an empty trigger, session context (project, cwd, git branch,
agent), the source session id, the generation fingerprint as `source_run_id`,
the segmenter name as `extractor_method`, and one evidence row spanning the
unit's message-ordinal range. Generated entries remain outside trusted Recall
until a separate promotion decision.

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
local archive while the user targets a remote daemon, and they hold the offline
writer lock for their lifetime, so a multi-step extraction pass can never
overlap another direct writer or a resync swapping the database underneath it.
