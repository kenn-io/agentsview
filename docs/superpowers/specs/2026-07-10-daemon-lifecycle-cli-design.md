# Daemon Lifecycle CLI Design

## Summary

AgentsView currently exposes managed background-server lifecycle operations
through `agentsview serve --background`, `agentsview serve status`, and
`agentsview serve stop`. This works, but it makes foreground serving and daemon
management share one command surface. Projects such as roborev use the clearer
`daemon start|stop|restart` convention.

Add a top-level `agentsview daemon` command as the canonical interface for the
writable SQLite daemon. Keep `serve` as the foreground-server command and
preserve the existing `serve` lifecycle forms for compatibility. The new daemon
commands are config-driven, idempotent, and deliberately exclude read-only
`pg serve` and `duckdb serve` processes.

## Goals

- Provide the familiar `daemon start|stop|restart|status` command family.
- Make lifecycle operations safe to repeat.
- Load daemon serve settings from normal configuration rather than lifecycle
  command flags.
- Reuse the existing launch serialization, readiness probing, runtime records,
  compatibility checks, and PID-safe termination behavior.
- Preserve existing scripts and documented `serve` lifecycle commands.
- Make the writable SQLite daemon the precise scope of the new command family.

## Non-goals

- Changing bare `agentsview serve` from a foreground operation.
- Adding `agentsview serve start`, whose foreground/background meaning would be
  ambiguous.
- Managing read-only `agentsview pg serve` or `agentsview duckdb serve`
  processes through the new daemon command.
- Adding service-manager integration such as launchd or systemd.
- Removing or hiding existing compatibility commands in this change.
- Changing daemon idle-timeout behavior.

## Command Surface

Add `daemon` to the root command's core command group.

| Command                     | Behavior                                                                                                                                                                             |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `agentsview daemon start`   | Start the writable SQLite daemon in the background. Succeed and report the existing daemon when it is already running.                                                               |
| `agentsview daemon status`  | Report the writable daemon's state, address, PID, version, and uptime. Exit successfully when state inspection succeeds; output distinguishes running, starting, and stopped states. |
| `agentsview daemon stop`    | Stop confirmed writable daemon processes. Succeed and report that no daemon is running when already stopped.                                                                         |
| `agentsview daemon restart` | Stop confirmed writable daemon processes and start a new one from current configuration. If stopped, start it and report that it was not previously running.                         |

The daemon subcommands accept no serve-specific flags. They load the normal
effective AgentsView configuration: defaults, `config.toml`, and supported
environment configuration such as `AGENTSVIEW_DATA_DIR`. Network, browser,
authentication, proxy, TLS, sync, and idle-timeout behavior therefore comes from
configuration rather than transient daemon command flags.

Two existing one-off modes cannot be expressed by `daemon start` and remain on
the `serve --background` compatibility path:

- `--no-sync` is runtime-only (`NoSync` is not a `config.toml` key).
- A persistent non-loopback `host` in `config.toml` requires
  `require_auth = true`, while an explicit `--host` flag can request a one-off
  unauthenticated non-loopback bind.

This config-only rule also applies when `daemon start` automatically replaces an
older daemon. It must not inherit runtime-only options from the old record,
including `NoSync`. Both start and restart launch from the newly loaded
configuration.

## Lifecycle Scope

The new command family manages only writable SQLite daemon runtime records.
Read-only `pg serve` and `duckdb serve` runtime records are ignored:

- `daemon status` reports no daemon when only read-only servers exist.
- `daemon stop` leaves read-only servers running.
- `daemon restart` leaves read-only servers running while restarting or starting
  the writable daemon.

If an invalid state contains multiple confirmed writable daemon records, stop
and restart operations stop all confirmed writable processes before starting one
replacement. This restores the single-writer invariant without affecting
read-only servers.

In that invalid multi-writer state, `daemon status` lists every live writable
daemon and warns that the single-writer invariant is violated. It must not
silently select the first runtime record.

## Compatibility Interfaces

The existing `serve` behavior remains visible and documented:

- Bare `agentsview serve` continues to run the server in the foreground.
- `agentsview serve --background` keeps its current flags and managed launch
  behavior. It remains the compatibility path for scripts that need transient
  serve flag overrides.
- `agentsview serve status` and `agentsview serve stop` keep their existing
  broader legacy scope, including read-only server records.
- Add `agentsview serve restart` as a new compatibility-shaped spelling for the
  writable, config-driven `agentsview daemon restart` operation. Help and
  documentation must call out that its writable-only scope intentionally
  differs from the broader legacy `serve stop`; it is not equivalent to
  running `serve stop` followed by a start.

New help text, documentation, and recovery guidance should prefer
`agentsview daemon ...`. Internal launch paths that need to preserve transient
runtime options, such as update recovery, may continue using the existing
background-launch machinery rather than forcing the config-only daemon command
through an unsuitable abstraction.

## Implementation Structure

Add a `newDaemonCommand` Cobra constructor with `start`, `status`, `stop`, and
`restart` children. Keep `newServeCommand` responsible for foreground serving
and its compatibility children.

Extract or add narrowly scoped lifecycle helpers around the existing machinery:

- Discover writable runtime records separately from all server records.
- During writable discovery, compare stored and live process create times with a
  tri-state result: match, proven mismatch, or unknown. Remove a runtime
  record only for a proven mismatch where both values are available and valid.
  Preserve missing or malformed recorded values and unavailable OS lookups as
  unknown for explicit identity handling.
- Render writable-only status without changing legacy `serve status` output.
- Stop only confirmed writable records for daemon operations.
- Launch a background child with synthesized `serve` arguments and no
  serve-specific CLI overrides.
- Orchestrate stop and restart while holding the existing background launch lock
  across their complete state transitions.

The background child remains a normal `agentsview serve` process. This avoids a
second server startup path and preserves existing runtime publication,
readiness, logging, platform-specific detachment, and graceful-shutdown logic.

`daemon start` and `daemon restart` must use the newly loaded configuration,
including when start automatically replaces an older daemon. They must not adopt
transient options recorded by the old runtime, such as an earlier `--no-sync`
flag. This differs intentionally from compatibility and update paths that need
to preserve an existing invocation's runtime behavior.

## Operation Flow

### Start

1. Resolve and create the configured data directory.
1. Acquire the existing background launch lock before configuration writes.
1. Load normal configuration and ensure required generated values such as the
   authentication token.
1. If a compatible writable daemon is running, report it and exit zero.
1. Apply the existing version and data-compatibility replacement policy when an
   older daemon requires replacement, but do not adopt runtime-only launch
   options from the replaced record.
1. Spawn the detached `serve` child, wait for its runtime record or early exit,
   and report its URL, PID, and log path.

If another start owns the launch lock, wait using the existing bounded startup
probe. Exit zero if that operation publishes a writable daemon; otherwise return
a clear startup-in-progress or startup-failed error. If the start lock remains
held beyond normal startup, include startup-snapshot PID/log details and the
same manual verification and termination guidance defined for stop; never launch
a possible second writer.

Normal foreground `serve` startup acquires the same background launch lock
during its handoff from process discovery through runtime publication.
Background children rely on their parent launcher holding that lock. The
separate daemon start lock remains authoritative for startup-in-progress state,
including the fallback case where runtime publication fails.

### Stop

1. Resolve the data directory and acquire the background launch lock for the
   complete discovery-and-stop operation. If another lifecycle operation owns
   the lock, return a retryable contention error rather than reporting success
   without guaranteeing the stopped state.
1. Load configuration so runtime discovery uses the correct data directory and
   authentication token.
1. Check the daemon start lock before examining runtime records. If it is held,
   return a retryable startup-in-progress error even when live writable
   records also exist; do not signal any process.
1. Select only live writable runtime records.
1. Remove records whose stored process create time proves that the live PID has
   been reused. If no writable records remain, report that the daemon was not
   running and exit zero.
1. Confirm every target's identity using the existing health/start-time checks
   before signalling any of them. If any live writable target cannot be
   confirmed, return nonzero without stopping the confirmed subset.
1. Gracefully stop confirmed targets, escalating with the existing timeout when
   necessary, and clean up managed Caddy children owned by those targets.

A startup that has not published a runtime record is not safe to signal.
`daemon stop` returns a retryable error asking the user to wait for startup to
finish. Its error also explains that runtime-record publication may have failed
if the state persists, and includes the PID and log path from the startup
snapshot when available. In that persistent fallback state, instruct the user to
verify and terminate the process manually before retrying. When a remaining live
record cannot be confirmed, report its PID and runtime-record path and give the
same manual verification and termination guidance. Do not suggest another daemon
lifecycle command that would refuse the same target.

### Restart

1. Resolve the data directory and acquire the background launch lock for the
   entire operation.
1. Load and validate current configuration.
1. Return a retryable error if the daemon start lock remains held after
   acquiring the launch lock.
1. Check database compatibility before stopping any running daemon.
1. Discover and safely stop every confirmed writable runtime record while
   leaving read-only records untouched.
1. Start one detached `serve` child from the newly loaded configuration and wait
   for readiness using the normal background-start path.
1. Report `restarted` when a daemon was stopped, or `started (was not running)`
   when no writable daemon existed.

If any live writable target cannot be confirmed, restart returns an error
without signalling it or starting a possible second writer. If the new process
fails after the old daemon has stopped, return nonzero and include the log path;
do not claim rollback or restoration.

The same persistent-start-lock recovery guidance used by stop applies when
restart finds the start lock held after it acquires the launch lock. Restart
must not infer process identity from a startup snapshot or signal that PID
automatically.

## Errors and Exit Behavior

- Repeated start, stop, and restart operations are successful when the requested
  final state already exists or can be reached safely.
- `daemon status` exits zero for successfully inspected running, starting,
  stopped, incompatible, and invalid multi-writer states. Configuration and
  runtime-store I/O failures return nonzero.
- Invalid configuration and incompatible database versions fail before a restart
  stops the existing daemon.
- Concurrent lifecycle activity is serialized by the launch lock. Operations
  that cannot safely wait return a concise retryable error. Stop and restart
  also check the distinct daemon start lock unconditionally before acting on
  runtime records.
- Stale or PID-reused records never authorize a signal. Only a valid stored
  create time and a valid, unequal live create time prove PID reuse and permit
  record cleanup; missing, malformed, or unavailable values remain
  unconfirmed. `daemon stop` and `daemon restart` return nonzero if a live
  writable target cannot be confirmed, because neither command can guarantee
  its requested final state.
- Child startup failure includes the background log location.
- Error messages and recovery hints prefer canonical `daemon` commands while
  compatibility commands remain accepted.

## Documentation

Update the README and CLI reference to:

- Present `daemon start|stop|restart|status` as the normal managed-background
  workflow.
- Describe `serve` as the foreground command.
- Label `serve --background` and the `serve` lifecycle subcommands as
  compatibility interfaces, without deprecating or hiding them.
- Explain the writable-only `serve restart` scope and its intentional asymmetry
  with broad legacy `serve stop`.
- Explain that daemon lifecycle settings come from `config.toml` and that
  read-only PG/DuckDB servers are outside the daemon command's scope.
- Document `--no-sync` and a one-off unauthenticated non-loopback bind as cases
  that still require `serve --background`.

Update user-facing restart and stop hints where the writable daemon is intended.
Retain backend-specific or compatibility wording where a broader server scope is
intentional.

## Testing

Add unit tests using testify and the repository's existing lifecycle test
helpers. Cover:

- Root help and Cobra registration for all four daemon subcommands.
- Rejection of positional arguments and serve-specific flags on daemon
  subcommands.
- `start` when stopped, running, and concurrently starting.
- `start` automatically replacing an older `NoSync` daemon without inheriting
  that runtime-only option.
- `start` when only read-only runtime records exist, which must launch a
  writable daemon rather than report that one is already running.
- `status` for running, starting, stopped, read-only-only, and incompatible
  writable daemon states.
- `status` reporting every writable runtime in an invalid multi-writer state.
- Idempotent `stop` and writable-only filtering with mixed writable/read-only
  runtime records.
- `stop` refusing before signalling confirmed records when the daemon start lock
  is also held.
- `stop` and `restart` refusing a persistent start-lock fallback and printing
  startup-snapshot PID/log recovery details without signalling the process.
- `restart` when running and stopped, including its distinct result messages.
- Restart loading current configuration without inheriting old runtime-only
  options.
- Database/config validation before stop.
- Launch-lock contention for stop and across the complete restart gap.
- Refusal to signal an unconfirmed or PID-reused runtime target, including
  proven-mismatch cleanup; preservation of missing, malformed, and
  OS-unavailable create times; legacy-record recovery output; and all-target
  prevalidation before stopping any writable process.
- New-child startup failure after stop, including the reported log path.
- Existing `serve --background`, `serve status`, and `serve stop` behavior.
- `serve restart` delegation to writable, config-driven restart behavior.
- `status` returning nonzero for configuration or runtime-store I/O failures
  while returning zero for every successfully inspected daemon state.

Run focused `cmd/agentsview` tests during development, followed by the normal Go
formatting, vetting, and practical repository test commands required by
`AGENTS.md` before committing implementation changes.
