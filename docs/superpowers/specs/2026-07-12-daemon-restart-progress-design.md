# Daemon Restart Progress Design

## Context

`agentsview daemon restart` stops the current writable daemon, launches its
replacement, and waits up to five seconds for the child to publish a healthy
writable runtime. The command produces no startup output during that wait. A
healthy startup that needs longer for database work or initial sync is then
reported as a fatal error even though the child remains alive and continues
starting.

The child already publishes structured startup state while it owns the startup
lock. The snapshot includes its PID, start time, current phase, phase detail,
and log path. Phases include opening the database, full resync, initial sync,
and starting the HTTP server. `daemon status` renders this information, but the
restart command does not consume it.

## Goals

- Keep canonical restart attached until the replacement is ready or a real
  failure or cancellation occurs.
- Show useful, bounded progress throughout database and sync work.
- Report success only after the replacement publishes a healthy writable
  runtime.
- Preserve the child after terminal cancellation, matching the existing rule for
  a slow startup that continues in the background.
- Keep `daemon start`, automatic startup, and compatibility background-launch
  timeout behavior unchanged.

## Command Experience

After spawning the replacement, restart prints the child PID and log path. It
then renders startup-state changes until readiness:

```text
Starting agentsview (pid 86832)...
  log: /path/to/data/serve.log
  opening database (2s)
  initial sync: claude: 120/450 sessions (27%) (6s)
  starting HTTP server (18s)
agentsview restarted at http://127.0.0.1:8080 (pid 86832)
```

The renderer prints a line when the phase or detail changes. It suppresses
duplicate snapshots. If neither value changes for a bounded interval, it emits
an elapsed-time heartbeat for the current phase so the terminal does not look
stalled. Progress remains line-oriented rather than rewriting terminal lines, so
redirected output and basic terminals retain an intelligible history.

The initial launch line is emitted only after process creation succeeds. The
existing stop output remains first, so the complete operation visibly moves from
stopped, to starting, to ready.

## Architecture

The background readiness wait gains an optional progress observer and accepts
the caller's context. On each readiness probe tick, the wait reads the trusted
startup snapshot and sends it to the observer. The observer owns presentation,
deduplication, and heartbeat timing; readiness discovery continues to own
runtime validation and child-exit detection.

`backgroundLaunchPolicy` selects the waiting behavior. Canonical restart passes
its command context, enables progress reporting, and requests an attached wait
without a fixed readiness deadline. Existing callers retain their current
timeouts and do not receive progress unless they opt in. This avoids duplicating
runtime discovery in the command layer and keeps structured startup state,
rather than log text, as the progress contract.

The command-layer dependency seam carries context and progress configuration so
tests can exercise the public CLI output without launching a real daemon.

## Completion and Error Handling

- A healthy writable runtime completes the wait and produces the existing
  restarted URL and PID result.
- Child exit before readiness fails immediately and retains the serve-log
  pointer.
- Failure to create the child reports the launch error without printing a
  misleading starting line.
- Cancellation stops the CLI wait and returns a clear message that the child
  continues running, including its PID, log path, and the `daemon status`
  follow-up command. Cancellation does not signal or reap the daemon child.
- A temporarily unreadable or absent startup snapshot does not fail restart;
  readiness polling continues and the next valid snapshot can resume progress.
- Runtime publication and compatibility checks remain authoritative. Progress
  output never constitutes readiness evidence.

An attached restart intentionally has no elapsed-time deadline while the child
is alive. A user can cancel an unexpectedly long wait and inspect or manage the
continuing child separately.

## Testing

Command tests use concrete startup snapshots and literal expected output to
prove that restart prints the launch identity, phase transitions, detailed
progress, quiet-phase heartbeats, and the final ready result. They also assert
that repeated snapshots do not duplicate output.

Separate tests cover child exit, missing snapshots, and context cancellation.
The cancellation test proves that no stop operation targets the replacement and
that the returned message identifies the continuing child and status command.
Existing slow-start tests for canonical start and compatibility background
launch continue to protect their timeout behavior.

## Non-goals

- Streaming progress for `daemon start` or implicit automatic startup.
- Parsing or tailing `serve.log` for progress.
- Changing startup phases, sync accounting, runtime-record publication, or
  daemon status formatting.
- Adding terminal spinners, colors, cursor movement, or a machine-readable
  progress format.
