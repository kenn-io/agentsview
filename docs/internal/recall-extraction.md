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
points. Runs additionally split at ordinal discontinuities: ingest filtering can
drop rows after ordinals are assigned, and evidence provenance requires gap-free
transcript ranges, so a unit spanning a missing row could never commit. Skipped
system and empty rows still occupy their ordinals and keep a run contiguous.
Derivation is deterministic — resume cursors index into the unit list and entry
identity embeds the unit index, so replaying a session after a restart or
upgrade must yield the same units in the same order.

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
scan. The atomic replace path re-stamps inside the same transaction. Stamps
written before this in-write revocation existed can cover less than the current
transcript (an append whose deferred rescan failed kept its stamp), so the rules
algorithm version was bumped alongside it: pre-existing stamps read as stale by
value — including rows arriving later from machines running older binaries,
which a one-time local migration could not catch — and every archive must rescan
before becoming eligible.

The eligibility check is also bound to the transcript actually sent: the manager
re-reads the session row (full column set — the standard list omits
`local_modified_at`) after loading its messages, compares the two reads (message
count, transcript revision, scan version, leak count, ended-at, last local
write), and re-runs the eligibility predicates against the second read —
trashing or flagging a session automated flips fields the comparison does not
watch. An unstable bracket skips the session silently: a concurrent write bumped
`local_modified_at`, so the next pass retries against a settled view, and a
newly ineligible session must simply not be extracted.

Before distillation the manager also re-scans the transcript content it is about
to send, independently of the stored scan stamp (an archive from an older binary
can carry a clean stamp over content the scanner never saw). The re-scan runs
over exactly the model-visible contents concatenated in transcript order (the
same rows the segmenter keeps — system messages and unsupported roles dropped,
each trimmed — so an intervening system message or boundary whitespace cannot
break a token in the scan that the endpoint still receives contiguously), not
per message: a secret whose structure spans messages — a private-key block split
across adjacent messages, or across separate units the endpoint receives and can
correlate — matches no per-message scan, and scanning the formatted unit texts
would let the interposed formatting push a straddling key under the scanner's
payload-purity gate. Both a newline-joined and a separator-free aggregate are
scanned: the first keeps multi-line structure, the second reconstructs a
single-token credential split mid-token across messages that the newline would
otherwise break. A match on either fails the session closed.

The bracket does not end when distillation starts. Eligibility is re-validated —
snapshot comparison, eligibility predicates, and a fresh secret-findings read (a
scan under an unchanged rules version can land candidate findings without
touching any snapshot field) — before every model call and again before
persisting each call's output. A concurrent write stops the pass silently as
above. Losing eligibility mid-extraction fails closed: the session's generated
entries are deleted and its progress row is reopened at cursor zero as a
retryable failure, so nothing extracted under the lost eligibility persists and
a session that becomes eligible again re-extracts from scratch.

The enforcement point is the unit commit itself. Each distilled unit's entries
are persisted through a single transaction that re-verifies the session snapshot
(`ended_at` included: a bare row update can reopen or re-date a session without
touching any other guarded field), the eligibility predicates, and the absence
of secret findings before inserting the entries and advancing the cursor from
exactly the position the unit was derived at — a same-digest reopen resets the
cursor after deleting entries, so a merely-monotonic advance would let a stale
worker skip units that no longer have output — the pre-call recheck only saves a
wasted model call, since a write can land between any out-of-band check and the
insert. A guard failure persists nothing; the caller classifies it by re-reading
the session: eligibility lost means discard, a changed snapshot means a
concurrent write landed and the next pass silently retries against a settled
view, and an *unchanged* snapshot means the refusal is deterministic (for
example an evidence range the transcript cannot verify) — that is recorded as a
failure so the backoff applies instead of paying for the same doomed model call
on every pass.

Eligibility loss *after* extraction is reconciled on every scheduled pass —
incremental ones included, because with the backstop disabled no full pass ever
runs and privacy retraction must not be schedulable away. Reconciliation runs
before any model work (so an endpoint-scoped abort against a persistently broken
endpoint cannot defer retraction) and again after the extraction loop (so
eligibility lost while units were at the model is retracted in the same pass).
Sessions since trashed, flagged automated, or carrying secret findings or leaks
get their `unreviewed_auto` entries deleted across *all* registered generations
(a retired generation keeps serving until the next activation, so retraction is
generation-independent) and their progress rows removed, so an excluded
session's corpus stops serving, a lingering pending or partial row cannot block
activation forever, and a session that becomes eligible again is rediscovered
and re-extracted from scratch. Both deletes are set-based, so retraction cannot
be blocked by SQLite's host-parameter limit however many sessions match.
Retraction does not depend on extraction being enabled: a generation activated
while `[recall.extract]` was on keeps serving after it is turned off, so the
daemon runs a reconcile-only scheduler in that case (skipped entirely when no
generation exists), driven by the same session-mutation notifications, startup
pass, and periodic ticks — a session trashed, flagged automated, or found to
carry secrets stops serving its entries whether or not the model-backed loop
runs. Stale or missing scan versions deliberately do not qualify — they are
transient (every transcript write clears the stamp until rescan), and deleting
on them would rebuild the corpus on every sync. The walk is bounded by its own
watermark, which advances on every completed unlimited scan pass; a fresh
manager reconciles unbounded. A mid-extraction discard is likewise atomic: the
entry delete and the guarded cursor reset commit together, so a resume can never
skip units whose entries no longer exist.

A stable bracket whose loaded message count differs from the row's is a
different case — the sync loop writes the session row before the transcript, so
the row can durably claim more or fewer messages than are stored, and no future
write may ever re-surface the session. That mismatch never reaches the model,
but it is recorded as a retryable *failure* (visible in status, re-offered after
the backoff) instead of skipped: a silent skip would let the discovery
watermarks advance past the session's writes and exclude it forever. The rule
holds for completed rows too — it is applied before the done short-circuit,
because a same-digest revisit would otherwise preserve `done` and settle the
coverage stamp, claiming the inconsistent state as covered. The failure
transition owns any stamp movement: a row already carrying the digest is failed
or discarded directly with no opening upsert (whose same-digest rule would
advance the stamp in its own committed transaction — a crash before the reopen
would then leave invalid coverage stamped current and permanently unselectable),
and a missing or digest-changed row is created as pending, which a crash leaves
retryable. The failure mark demotes such a row (`Reopen`) and resets its cursor
to zero: its completed-units claim was judged against the inconsistent session,
and the strictly monotonic cursor could otherwise never reach `done` again.

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
leaves the row resumable instead of burning the backoff — which is also why a
same-digest upsert on a failed row preserves `updated_at`: the retry's opening
upsert must not reset the backoff clock a cancelled retry would then wait out
again. A same-digest upsert completes a zero-unit row (`done`) whatever state it
held: the extraction loop runs zero iterations for it, so no cursor advance
would ever promote a reopened zero-unit failure otherwise.

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

Evidence provenance is bound at commit time through the recall evidence-window
APIs: inside the unit-commit transaction, each cited ordinal range is rebuilt as
a host-authorized window and its selection metadata — the host-derived content
digest and stable endpoint source UUIDs — is stamped onto the evidence rows
before `provenance_ok` entries are inserted. The evidence reconciler re-verifies
exactly that digest on later transcript writes and resyncs, so untouched
evidence survives coordinate shifts instead of being revoked for a missing
digest.

Entries also copy session context — project, cwd, git branch, agent — at insert
time, and a metadata-only session update keeps the unit digest unchanged. A
same-digest revisit therefore synchronizes those fields on the session's
generated entries, so the corpus stops matching Recall filters for the old
context without any model calls. Human-touched entries are left as they were,
mirroring the delete path.

The same revisit also rebinds evidence. Evidence digests cover every row in
their range — system and empty messages included — while the units digest covers
only distilled text, so an ignored-row edit lets the reconciler revoke
provenance on an entry whose extraction output never changed, and that works in
both orders (revisit-then-reconcile would strand the entry just the same). The
rebind re-derives each range through the verifying window against the current
transcript, re-stamps digests and endpoint UUIDs, and restores revoked
provenance.

Context sync, evidence rebind, and the coverage stamp land in one transaction
guarded by the same session-snapshot and digest checks as the unit commit: a
transcript write racing the revisit rolls the whole refresh back (silently
retried next pass), because a partial refresh could bind stale entries to the
new transcript, mark them provenance-verified — permanently pacifying the
reconciler — and settle the stamp over the unseen write.

A digest change deletes the previous derivation's machine entries inside the
same transaction that resets the progress row, so no failure between the two can
leave a `done` row claiming coverage for entries that are gone — a window
incremental passes, which never revisit done rows, would not repair.

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
  surfaces instead. The recovery is bounded: one unit's recursive splitting
  may make at most a fixed number of model calls, so a single oversized
  message — user content is never packed, so its length is unbounded — cannot
  fan out into unbounded calls or accumulate every leaf's entries in memory;
  exceeding the budget fails the session closed.

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
  of every completed progress row. A done session with no recorded local write
  (a legacy row predating the column) revisits only while its coverage stamp
  is empty: every write path records a local write, so a stamped legacy row
  cannot have changed, and pre-stamp archives re-open once and settle. The
  full watermark ratchets to the pass *start* with no quiet-period lag — a
  write landing during pass N carries a `local_modified_at` at or after N's
  start, so pass N+1 still sees it. Full passes bound their discovery by the
  same watermark incremental passes use. Unbounded reconciliation is the
  fresh-manager path: a daemon restart or a manual `recall extract run` starts
  with zero watermarks and scans everything once;
- when the backstop is disabled, a catchup ticker (paced by the quiet period,
  never faster than once a minute) runs incremental passes instead: sessions
  become eligible only after the quiet period, long after the last sync-driven
  debounce fired, so sync signals alone cannot guarantee eventual extraction;
- every daemon lifetime begins with one *full* pass (the debounce timer starts
  armed and the full carry starts set): a detached daemon self-reaps after its
  idle timeout (20 m default), which is shorter than the backstop interval, so
  work deferred past a daemon's exit — a session whose quiet period elapsed
  with no daemon running, retraction for a session trashed in between, a
  completed session whose transcript grew — must not depend on sync activity
  or a full tick arriving before the next lifetime idles out too. The full
  revisit stays bounded: only done sessions written to since their coverage
  stamp reload. Passes hold an idle-tracker work lease, so a pass in flight is
  never cut off by the reaper, and the pending startup pass holds one too
  until the lifetime's first pass has started — an idle timeout configured
  shorter than the startup debounce would otherwise reap the daemon before
  extraction ever ran, every lifetime. Other pending future work deliberately
  does not pin the daemon alive — the startup pass of the next lifetime owns
  it;
- the server's trash, restore, delete, and secret-scan routes signal the
  scheduler directly (they change extraction eligibility and no sync activity
  follows them), so retraction and newly clean sessions ride a pass one
  debounce later instead of waiting for the backstop. Scans run directly by
  the `secrets scan` CLI against an unlocked archive cannot signal the daemon;
  their eligibility changes ride the next lifetime's startup pass or tick;
- passes drop instead of queueing when one is already running, and a dropped
  backstop carries into the next debounced pass.

Concurrency is one pass at a time, one session at a time, one unit per model
call. Each response is bounded locally as well as by the transport size cap: the
client refuses responses exceeding fixed limits on entries per call and on
title, body, and entity lengths, and the requested JSON schema declares the same
bounds (`maxItems`/`maxLength`), so a compliant constrained-decoding server
never produces a refused response while a non-compliant one cannot balloon the
archive or hold its write lock through oversized inserts.

## Activation

A building generation auto-activates when everything currently eligible is done,
no eligible session is pending or partial, and the generation has produced at
least one entry. Raw pending/partial counts are not consulted: a row whose
session turned transiently ineligible (reopened, scan stamp lost) is skipped by
candidate selection and left alone by reconciliation, so no pass can ever finish
it — refusing on it would stall activation until the session happens to settle.
The activation transaction clears such rows with their staged output, and
rediscovery re-extracts once the session settles. The backlog check counts
completed sessions whose transcripts changed since their unit snapshot: their
corpus is stale, and activating over them would promote a generation that does
not cover what it claims. Failed sessions do not block activation — they retry
later and top the corpus up — unless a failed partial row holds staged entries
and its session was written after its coverage stamp (a content change, or a
remap to another project, cwd, or branch): those entries would promote context
only the retry's refresh repairs, so the activation transaction refuses them
like stale done coverage. An entryless generation never activates, manually or
automatically: activation retires the previously active generation, and
replacing a working corpus with an empty one must not happen silently. All of
these checks are advisory racing against concurrent writes, so the activation
transaction re-verifies them before switching generations: it aborts if any
still-eligible session is pending or partial (a row whose session has since
turned ineligible can never finish, and an explicit activation runs no
retraction pass to clear it first), if any completed, still-eligible session has
transcript writes past its coverage stamp or a scan stamp outside the current
rules versions, if any eligible session has no progress row at all (a
single-session run, or a session ending after the caller's checks, is uncovered
work no progress-based gate can see), or if promotion would leave zero servable
(accepted, provenance-verified) entries — a blocked activation changes nothing
and the next pass retries after re-extraction. Promotion also re-verifies full
eligibility inside the same transaction, and clears what fails it: any session
no longer fully eligible — trashed, flagged automated, carrying findings,
reopened, back inside the quiet period, or awaiting rescan after a write — has
its staged entries and progress rows deleted before the rest are promoted.
Skipping instead of deleting would strand the output: an archived entry under a
surviving progress row is never promoted or rediscovered once the generation is
active, and deferring hard-ineligible rows to the scheduled retraction pass
loses the race against a session restored before that pass runs. A cleared
session is rediscovered and re-extracted from scratch when it settles or
returns.

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
