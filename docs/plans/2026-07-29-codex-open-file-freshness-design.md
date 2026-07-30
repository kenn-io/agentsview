# Codex Open-File Freshness Design

Status: approved for implementation planning

Date: 2026-07-29

## Problem

On macOS, Codex can keep a rollout JSONL file descriptor open for the lifetime
of a session. The native recursive FSEvents backend does not necessarily deliver
file-write events for repeated appends while that descriptor remains open. It
delivers an event when the writer closes the file, which can be hours or days
later.

Agentsview currently treats native watch coverage as complete for Codex and does
not include Codex in periodic provider reconciliation. A long-running rollout
can therefore remain stale in SQLite even while the source file keeps growing.
The stale archive row causes several visible correctness failures:

- Activity omits intervals and sessions that occurred after the last observed
  append.
- Daily usage can remain flat and then jump when the rollout eventually closes.
- Session `last_active`, message count, and file metadata lag behind the source.

This is a synchronization defect. Activity and usage queries are correctly
reporting the stale data available to them.

## Requirements

The design must:

- reflect an actively growing Codex rollout in Agentsview within two minutes
  under healthy local operating conditions;
- cover sessions resumed after days of inactivity, not only sessions that were
  recent when Agentsview started;
- keep Activity, session freshness, and daily usage current through the same
  source synchronization path;
- retain the existing filesystem watcher as the low-latency fast path;
- keep recurring work independent of the total number of archived sessions;
- bound retained paths, queued work, hint bytes, and retry state;
- avoid a periodic walk or reconciliation of the complete Codex archive;
- preserve deletion, tombstone, duplicate-source, and persistent-archive
  behavior;
- require no SQLite schema change and no PostgreSQL query change; and
- work on every supported platform even though the observed notification gap is
  macOS-specific.

The two-minute objective assumes a functioning local filesystem and available
sync worker capacity. I/O failures and pre-existing queue saturation continue to
use the existing retry and degraded-coverage behavior.

## Considered Approaches

### Full Codex reconciliation every two minutes

This would eventually find every changed rollout and would naturally include
cold resumes. It is rejected because every pass would enumerate the complete
archive. CPU, I/O, and temporary state would scale with historical session count
even when only one session changed.

### Dynamic native watches for individual rollout files

Codex activity could install a file-level kqueue watch on macOS, with equivalent
platform-specific implementations elsewhere. This provides low notification
latency but consumes an operating-system watch or file descriptor per active
rollout, adds platform-specific lifecycle code, and still needs an independent
way to discover an old session when it resumes.

### Provider activity hints plus bounded hot-source polling

Codex appends submitted prompts to one `history.jsonl` file under its data root.
Each record contains the session ID. This compact append stream reveals both new
activity and cold resumes without inspecting the rollout archive.

After observing a session ID, Agentsview can resolve its already-indexed rollout
path and temporarily poll only that path's metadata. A sync is scheduled only
when the path changes.

This is the selected approach. It is portable, adds no per-file operating system
resources, and makes recurring work proportional to configured hint files plus
the recent live working set.

## Provider Contract

Add an activity-hint capability to `parser.SourceCapabilities`. Its zero value
is unsupported. Only providers that explicitly advertise support participate in
activity-hint scheduling.

The accompanying provider interface supplies:

- a bounded list of activity-hint files for the configured roots; and
- a decoder that extracts stable raw session IDs from appended hint records.

The scheduler owns cursors, byte limits, polling cadence, path lookup, and
hot-set retention. Providers own only format knowledge and configured-path
derivation.

Codex opts into the capability and exposes the `history.jsonl` beside its
configured `sessions` directory. The decoder reads only `session_id`. Prompt
text is discarded immediately and must never be logged or retained by the
scheduler.

The Codex entry in `docs/internal/session-format-sources.md` must be reverified
and updated with producer evidence for this activity-hint record.

## Scheduling Design

### Activity-hint polling

One scheduler instance polls provider-declared hint files every 30 seconds. Each
cursor contains only:

- cleaned path;
- file identity;
- consumed byte offset; and
- a bounded partial-line buffer.

An unchanged hint file costs one metadata lookup per interval. When the file
grows, the scheduler reads only appended bytes. Reads, decoded records, partial
lines, and queued session IDs use explicit entry and byte limits.

On startup, the scheduler reads a bounded tail window and accepts recent
timestamped records before establishing the end cursor. This closes the race
between initial source synchronization and cursor initialization without reading
the complete historical hint log.

If a hint file is replaced, truncated, or changes identity, the scheduler
discards its old cursor and bootstraps from a bounded tail of the replacement.
Malformed or oversized lines are skipped and reported through rate-limited
diagnostics without exposing their contents.

### Session-path resolution

For each Codex session ID, the scheduler constructs the canonical full ID and
uses the indexed session row to obtain `file_path`. It must not call a fallback
that walks the Codex archive to locate the UUID.

When the row or path is not yet available, the ID enters a small bounded retry
set. This covers ordering between rollout creation, the normal watcher callback,
and the first history record. Retries query only the indexed row. They expire
after a short fixed window; ordinary source creation and startup discovery
remain responsible for ingesting previously unknown files.

### Hot-source set

A resolved hint immediately:

1. adds or refreshes the rollout in the hot-source set;
1. checks its current metadata; and
1. queues an exact-path sync when it differs from the last observed metadata.

Every 30 seconds, the scheduler stats the hot sources and queues only changed
paths. Paths are deduplicated before entering the existing serialized sync
callback. The normal sync engine performs fingerprinting, incremental parsing,
database writes, and session-change emission, so Activity and Usage need no
special update path.

Hot entries retain:

- agent and full session ID;
- source path;
- file identity, size, and modification time;
- last hint time; and
- last observed growth time.

Entries refresh whenever the source changes. Quiescent entries expire after a
long fixed inactivity window; a later user prompt reintroduces them through the
history hint. The initial implementation will use a 24-hour inactivity window to
cover extended agent runs while avoiding permanent per-session polling.

The set is limited to 8,192 entries and 2 MiB of retained path bytes. On
overflow, the least recently hinted quiescent entries are removed first.
Overflow is rate-limited in logs and never promotes the work to a complete
provider reconciliation.

## Resource Model

For `H` configured activity-hint files and `A` hot sources, an unchanged poll
performs:

```text
H + A metadata lookups
0 archive directory walks
0 rollout reads
0 database writes
```

When `C` hot sources changed, only those `C` paths enter synchronization.
Archive cardinality does not appear in the recurring-work equation.

The default bounds retain at most:

- one cursor per provider-declared hint file;
- 8,192 hot-source entries;
- 2 MiB of hot-source path strings;
- the existing bounded watcher batch; and
- bounded hint and retry buffers.

No rollout file stays open between polls, and no operating-system watch is
allocated per hot source.

## Failure and Lifecycle Behavior

- Missing hint files remain cheap metadata probes and become active when
  created.
- Permission and transient I/O errors are logged with existing rate-limiting
  conventions and retried on a later interval.
- A missing hot rollout is removed from the hot set. Its deletion is still
  handled by the authoritative watcher/reconciliation path, so the poller
  cannot independently tombstone a session.
- A rollout rename or duplicate-source promotion is resolved by the normal sync
  engine. A later hint refreshes the database-selected canonical path.
- Hint polling stops with the daemon context and does not outlive the watcher or
  database.
- Activity-hint failure never triggers full synchronization or broadens a
  reconciliation scope.
- SQLite remains the local system of record. PostgreSQL and DuckDB receive the
  updated session through their existing downstream synchronization paths.

## Verification

Tests must assert observable behavior rather than implementation strings.

### Provider and hint-reader tests

- Codex alone advertises the new capability initially.
- Zero-value and unrelated provider capabilities remain unsupported.
- Configured Codex roots resolve to the correct hint paths.
- Appended records yield deduplicated session IDs.
- Partial, malformed, oversized, truncated, and rotated hint files remain
  bounded and recover on later records.
- Prompt text is neither returned nor logged.

### Scheduler tests

- A cold-resumed session already present in the archive becomes hot from a
  history record without an archive walk.
- The first hint schedules a changed rollout immediately.
- Continued rollout appends schedule later exact-path syncs even when no
  additional prompt is submitted and the writer remains open.
- Unchanged hot files cause metadata checks but no sync calls.
- Missing indexed rows use only the bounded retry set.
- Expiration, deduplication, overflow, and cancellation release retained state.

### Cardinality tests

Run the same hint and hot-source event against small and large synthetic
archives. Assert equal counts for:

- hint files inspected;
- hint bytes read;
- session-path lookups;
- rollout metadata checks;
- exact paths submitted to sync; and
- archive directories traversed, which must remain zero.

The same tests must preserve existing deletion, tombstone, and
persistent-archive outcomes by proving the poller does not claim authoritative
ownership.

### Integration and Darwin regression tests

- Keep a rollout descriptor open, append several records over time, and prove
  that the normal Darwin recursive notification can remain absent before
  close.
- Append a cold-resume history record and then message, tool, and token records
  to that still-open rollout.
- Advance the scheduler clock and verify within two minutes that:
    - session `last_active`, message count, and file metadata advance;
    - the appropriate Activity interval includes the session; and
    - daily usage includes the new token event and does not move backward.

Run the relevant package tests, the short suite, `go fmt ./...`, and
`go vet ./...` before committing implementation changes.

## Expected User-Visible Result

On a healthy local daemon, a prompt submitted to a new or long-idle Codex
session is reflected in Agentsview within two minutes. Subsequent agent,
tool-call, and token activity in the still-open rollout continues to advance
Activity, session freshness, and daily Usage without waiting for Codex to close
the file.
