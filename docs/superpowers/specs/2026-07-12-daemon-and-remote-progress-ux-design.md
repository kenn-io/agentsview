# Daemon Startup and Remote Progress UX Design

## Context

Canonical `agentsview daemon start` currently waits five seconds for a spawned
server to publish its writable runtime. A healthy startup that spends longer
than five seconds in its initial sync is then reported as a fatal error even
though the identified child continues to run. The existing 90-second
automatic-start readiness window is a better fit for canonical start while
preserving the rule that it must not claim the daemon is ready before its health
endpoint is available.

While the daemon is starting, `agentsview daemon status` reads
`startup-state.json`. That snapshot contains the PID, elapsed time, phase,
detail, and log path, but not the child binary version, so status cannot show
the version until the writable runtime has been published and probed.

During a multi-source full rebuild, contributor progress is labelled for the
current HTTP host after the coordinator has added all prior-source counts. For
example, a local pass of 40,000 sessions followed by a remote corpus of 60,000
sessions is displayed as `Processing sessions from <host>: .../100000`. The
aggregate is valid for coordinator accounting but misleading under a
host-specific label.

## Goals

- Let normal canonical daemon starts wait long enough for healthy initial syncs
  without weakening readiness claims.
- Show the starting child binary's version before runtime publication.
- Make HTTP contributor progress counters match the host named in the label.
- Preserve lifecycle safety, aggregate rebuild statistics, atomic swap behavior,
  and compatibility with startup snapshots from older binaries.

## Design

### Canonical daemon readiness window

`backgroundLaunchPolicy` will carry the readiness timeout selected by its
caller. Canonical `daemon start` will pass the existing
`backgroundAutoStartReadyTimeout` value of 90 seconds. The compatibility
`serve --background` path will retain its five-second window.

The background launcher will use the policy timeout when it is positive and fall
back to `backgroundServeReadyTimeout` otherwise. This keeps existing callers and
tests with a zero-value policy unchanged.

If the child publishes a healthy writable runtime within 90 seconds, the
canonical command reports the normal ready result. If the child exits, the
command still fails immediately. If it remains alive but unready after 90
seconds, the existing slow-start error and status guidance remain unchanged; the
command must not print `running` or `restarted` without readiness evidence.

### Version during startup

`startupState` will gain an optional `version` field. The startup-state writer
will initialize it from the child process's build version before its first phase
write. Starting-status rendering will show `version:` when the field is present.

The field is additive and uses `omitempty`. Older startup snapshots remain
readable and simply omit the version line. New CLIs reading an old daemon's
snapshot therefore retain today's output rather than inventing a version. Once
runtime publication succeeds, the existing health-probe version remains
authoritative for running-daemon status.

### Host-specific contributor progress

The rebuild coordinator will stop adding prior-source session and message counts
to each contributor's progress events. Contributor engines already produce
counts scoped to their own discovery and processing pass, and the remote
progress transformer labels those events with the contributor host. The CLI will
therefore render, for example:

```text
Processing sessions from remote-host: 12000/60000 sessions (20%)
```

The phase counter may reset when the rebuild moves from local work to a remote
host; the changed label makes that phase transition explicit. Aggregate final
`SyncStats`, failure ratios, contributor ordering, database writes, and swap
decisions continue to use merged statistics and are unchanged.

## Error Handling and Compatibility

- A longer wait changes only how long canonical commands observe the existing
  child; it does not suppress child-exit, lock, configuration, compatibility,
  or health errors.
- Startup version publication is best-effort like the rest of the startup
  snapshot. Snapshot write failures remain warnings and never block startup.
- Host-only progress changes presentation events only. It does not change
  manifest counts, discovered sessions, imported IDs, skip caches, or archive
  contents.

## Testing

- Command tests assert canonical start requests the 90-second readiness window
  while compatibility launches retain five seconds.
- Startup-state round-trip and rendering tests cover the version line and an
  older snapshot without the field.
- Contributor progress tests assert local and remote phases report their own
  session and message totals, while final merged statistics still equal the
  sum of all sources.
- Existing lifecycle failure tests continue to prove that an exited or
  persistently unready child is never reported as ready.

## Non-goals

- Streaming startup progress directly in the blocking start command.
- Waiting indefinitely for daemon readiness.
- Changing the HTTP manifest, parser discovery rules, or the number of remote
  sessions ingested.
- Addressing the separate per-source rebuild abort-safety review finding; that
  requires its own correctness change and regression coverage.
