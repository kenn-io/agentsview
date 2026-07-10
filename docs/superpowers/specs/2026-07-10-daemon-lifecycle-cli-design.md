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

| Command                     | Behavior                                                                                                                                                     |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `agentsview daemon start`   | Start the writable SQLite daemon in the background. Succeed and report the existing daemon when it is already running.                                       |
| `agentsview daemon status`  | Report the writable daemon's state, address, PID, version, and uptime. Always exit successfully; output distinguishes running, starting, and stopped states. |
| `agentsview daemon stop`    | Stop confirmed writable daemon processes. Succeed and report that no daemon is running when already stopped.                                                 |
| `agentsview daemon restart` | Stop confirmed writable daemon processes and start a new one from current configuration. If stopped, start it and report that it was not previously running. |

The daemon subcommands accept no serve-specific flags. They load the normal
effective AgentsView configuration: defaults, `config.toml`, and supported
environment configuration such as `AGENTSVIEW_DATA_DIR`. Network, browser,
authentication, proxy, TLS, sync, and idle-timeout behavior therefore comes from
configuration rather than transient daemon command flags.

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

## Compatibility Interfaces

The existing `serve` behavior remains visible and documented:

- Bare `agentsview serve` continues to run the server in the foreground.
- `agentsview serve --background` keeps its current flags and managed launch
  behavior. It remains the compatibility path for scripts that need transient
  serve flag overrides.
- `agentsview serve status` and `agentsview serve stop` keep their existing
  broader legacy scope, including read-only server records.
- Add `agentsview serve restart` as a compatibility spelling for the writable,
  config-driven `agentsview daemon restart` operation.

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
- Render writable-only status without changing legacy `serve status` output.
- Stop only confirmed writable records for daemon operations.
- Launch a background child with synthesized `serve` arguments and no
  serve-specific CLI overrides.
- Orchestrate restart while holding the existing background launch lock across
  the complete stop/start gap.

The background child remains a normal `agentsview serve` process. This avoids a
second server startup path and preserves existing runtime publication,
readiness, logging, platform-specific detachment, and graceful-shutdown logic.

`daemon restart` must use the newly loaded configuration. It must not adopt
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
   older daemon requires replacement.
1. Spawn the detached `serve` child, wait for its runtime record or early exit,
   and report its URL, PID, and log path.

If another start owns the launch lock, wait using the existing bounded startup
probe. Exit zero if that operation publishes a writable daemon; otherwise return
a clear startup-in-progress or startup-failed error.

### Stop

1. Load configuration so runtime discovery uses the correct data directory and
   authentication token.
1. Select only live writable runtime records.
1. If none exist and no writable startup is active, report that the daemon was
   not running and exit zero.
1. Confirm each target's identity using the existing health/start-time checks
   before signalling it.
1. Gracefully stop confirmed targets, escalating with the existing timeout when
   necessary, and clean up managed Caddy children owned by those targets.

A startup that has not published a runtime record is not safe to signal.
`daemon stop` returns a retryable error asking the user to wait for startup to
finish.

### Restart

1. Resolve the data directory and acquire the background launch lock for the
   entire operation.
1. Load and validate current configuration.
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

## Errors and Exit Behavior

- Repeated start, stop, and restart operations are successful when the requested
  final state already exists or can be reached safely.
- `daemon status` always exits zero; its human-readable output conveys state.
- Invalid configuration and incompatible database versions fail before a restart
  stops the existing daemon.
- Concurrent lifecycle activity is serialized by the launch lock. Operations
  that cannot safely wait return a concise retryable error.
- Stale or PID-reused records never authorize a signal.
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
- Explain that daemon lifecycle settings come from `config.toml` and that
  read-only PG/DuckDB servers are outside the daemon command's scope.

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
- `status` for running, starting, stopped, read-only-only, and incompatible
  writable daemon states.
- Idempotent `stop` and writable-only filtering with mixed writable/read-only
  runtime records.
- `restart` when running and stopped, including its distinct result messages.
- Restart loading current configuration without inheriting old runtime-only
  options.
- Database/config validation before stop.
- Launch-lock contention across the complete restart gap.
- Refusal to signal an unconfirmed or PID-reused runtime target.
- New-child startup failure after stop, including the reported log path.
- Existing `serve --background`, `serve status`, and `serve stop` behavior.
- `serve restart` delegation to writable, config-driven restart behavior.

Run focused `cmd/agentsview` tests during development, followed by the normal Go
formatting, vetting, and practical repository test commands required by
`AGENTS.md` before committing implementation changes.
