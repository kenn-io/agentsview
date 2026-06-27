# Daemon Version Conflict Handling Design

## Problem

`agentsview serve` currently handles existing writable daemons differently
depending on the entry point. Background serve and daemon-backed CLI auto-start
paths already use the established upgrade policy:

- newer non-dev release binaries may replace older writable daemons;
- dev builds do not replace automatically;
- downgrades do not replace automatically;
- daemons with newer API or data versions are not replaced automatically.

Foreground `agentsview serve` bypasses that policy. It opens the SQLite archive
through `mustOpenWriteDB`, reaches `rejectLiveWritableDaemonBeforeDirectWrite`,
and refuses with a generic `agentsview serve stop` suggestion. This makes normal
upgrades and dev-binary testing harder than necessary, and `serve status` can
incorrectly describe an incompatible but responding daemon as a process that is
not responding to health checks.

## Goals

- Make foreground `agentsview serve` use the same trusted replacement policy as
  background serve and daemon-backed CLI auto-start.
- Add a first-class manual override for intentional replacement, especially for
  dev builds.
- Make version conflicts obvious in `serve status` and startup errors.
- Keep SQLite single-writer protection intact.
- Preserve read-only daemon behavior; `pg serve` or other read-only daemons must
  not be stopped by local writable serve replacement.

## Non-Goals

- Do not change daemon auto-start behavior for unrelated commands.
- Do not add automatic replacement for dev builds, downgrades, or forward
  API/data versions.
- Do not attempt a zero-downtime handoff. The existing stop-then-start tradeoff
  is acceptable because the old daemon owns the write lock.
- Do not change PostgreSQL, DuckDB, or read-only serving semantics.

## User-Facing Behavior

`agentsview serve`:

- If no writable daemon owns the archive, start normally.
- If a compatible writable daemon is already running on the same version or a
  newer version, report that daemon clearly and exit without starting another
  writer.
- If an older writable daemon is running and the current binary is a newer
  non-dev release, stop the old daemon, print a replacement message, then
  start the new foreground server.
- If the current binary is `dev`, older, not semver-newer, or the daemon has a
  newer API/data version, refuse by default and explain the conflict.
- If `--replace` is passed, stop the confirmed writable daemon and proceed even
  for dev builds, downgrades, and forward API/data mismatches.

`agentsview serve --background`:

- Continue to auto-replace safe older release daemons using the existing policy.
- Accept the same `--replace` flag for explicit replacement outside the safe
  automatic policy.
- Print the replacement before reporting the new background daemon URL/log path.

`agentsview serve status`:

- Prefer compatible daemons as it does today.
- If a live daemon responds but is incompatible, report it as running and
  incompatible rather than "not responding".
- Include URL, PID, daemon version, current binary version, API/data versions
  when relevant, and suggested commands.

Example refusal shape:

```text
agentsview daemon version conflict:
  daemon:  http://127.0.0.1:8080 (pid 12345, version 0.23.0, api 1, data 7)
  binary:  dev (api 1, data 7)

This binary will not replace that daemon automatically.
Run `agentsview serve --replace` to replace it, or run
`agentsview serve stop` and then `agentsview serve`.
```

Example automatic replacement message:

```text
Replacing agentsview daemon at http://127.0.0.1:8080
  stopped: pid 12345, version 0.23.0
  starting: version 0.24.0
```

## Design

Introduce a small shared replacement decision layer in the command package,
backed by the existing policy helpers:

- `shouldUpgradeDaemonRuntime`
- `shouldUpgradeIncompatibleDaemonRuntime`
- `stopDaemonRuntimeForUpgrade`

The new layer should classify a live writable daemon into one of these actions:

- use/report existing daemon;
- replace automatically;
- replace because `--replace` was passed;
- refuse with an explanatory conflict.

The classifier should carry the daemon runtime, compatibility error, action, and
human-readable reason so foreground serve, background serve, and status can
render consistent messages without duplicating policy logic.

Foreground `serve` should run this replacement check after config validation and
auth-token generation, but before opening the write DB. This minimizes the
stop-then-fail window while still stopping late enough to avoid racing the
existing daemon's write lock.

Foreground replacement should use the current invocation's resolved config and
flags for the new server. Replacing a daemon is permission to stop the old
writer; it is not a request to inherit the old daemon's host, port, auth, or
sync settings.

Background `serve` should replace its current scattered compatible and
incompatible checks with the shared classifier. Its existing launch lock remains
the concurrency boundary for stop/start replacement. It should keep the current
background behavior that preserves the replaced daemon's `no_sync` setting when
the replacement command did not explicitly enable sync, but otherwise use the
new invocation's config and flags.

`serve status` should first look for a compatible daemon with
`FindDaemonRuntime`. If none is found, it should call
`FindIncompatibleDaemonRuntime` before falling back to generic live-record or
startup-lock messages.

## Safety

Replacement must only target writable daemons. Read-only daemon records are
reported by status but never stopped by local writable `serve`.

Stopping remains guarded by `stopTargetConfirmed`, which verifies either a
successful daemon ping or an exact recorded process create-time match before
signalling a PID.

`--replace` is explicit but not blind. It skips the automatic version policy,
but it does not skip identity confirmation or writable/read-only checks.

If the old daemon stops and the new server fails during startup, no automatic
rollback is attempted. This is the same operational tradeoff already present in
the update flow.

## Testing

Add focused Go tests for:

- foreground `serve` auto-replaces an older release daemon before opening the
  write DB;
- foreground `serve` refuses to auto-replace from a dev binary and suggests
  `--replace`;
- foreground `serve --replace` replaces a confirmed writable daemon even when
  the automatic policy would refuse;
- foreground `serve` does not stop read-only daemons;
- `serve --background --replace` follows the same replacement decision while
  retaining the background launch lock behavior;
- `serve status` reports incompatible responding daemons as incompatible rather
  than not responding;
- status/refusal output includes daemon version, binary version, PID, URL, and
  API/data mismatch details when available.

Where tests stub process stopping or server startup, they should assert the
replacement decision and output shape without starting a real long-running
server unless an existing lifecycle test helper already covers that path.
