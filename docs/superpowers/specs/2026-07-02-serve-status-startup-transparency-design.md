# serve status startup transparency

Date: 2026-07-02 Status: approved

## Problem

While the daemon is starting (often auto-started in the background by another
command), `agentsview serve status` prints only `agentsview is starting up.`
Startup can take minutes (full resync), and the rich progress the daemon emits
goes to `<dataDir>/serve.log`, which nothing points the user at. The start lock
is an anonymous flock: no pid, phase, or timestamp is readable by other
processes.

## Scope

`serve status` output during startup only. No changes to commands that block
waiting for an auto-started daemon, no machine-readable API, no frontend work.
CLI-process-local, so storage-backend parity does not apply.

## Design

### State file and lifecycle

- Path: `<dataDir>/startup-state.json`.
- Fields: `pid`, `started_at`, `phase` (short human string:
  `"opening database"`, `"full resync"`, `"initial sync"`,
  `"starting HTTP server"`), `detail` (current formatted progress line, e.g.
  `claude-code: 12340/38209 sessions (32%) · 1.2M messages`), `log_path` (set
  only when running as a background child, since only then does output go to
  serve.log), and `updated_at`.
- Written atomically (temp file + rename) by the serve process:
    - initial write right after `MarkDaemonStarting`;
    - phase writes at the existing transition points in `main.go` (DB open, resync
      vs. sync branch, HTTP start);
    - detail updates teed from the existing progress callbacks, throttled to at
      most once per second.
- Deleted in `UnmarkDaemonStarting` (success path and deferred cleanup), so the
  file lives exactly as long as the start lock.

### serve status rendering

In `runServeStatus`, the `IsDaemonStarting` branch keeps its current first line
(`agentsview is starting up.`), then appends indented fields matching the
`serveStatusLines` style:

```
agentsview is starting up.
  pid:      48151
  elapsed:  1m12s
  phase:    full resync: claude-code: 12340/38209 sessions (32%)
  log:      ~/.agentsview/serve.log
```

- `elapsed` is now minus `started_at`.
- `phase` appends `detail` when present.
- `log` is printed only when `log_path` is set.
- The file is read only inside the `IsDaemonStarting` branch — the held lock is
  the trust gate, so staleness is a non-issue.

### Error handling

- State-file write failures during startup are logged and otherwise ignored;
  transparency must never break startup.
- Missing, unreadable, or corrupt JSON on the read side (legacy daemon version,
  mid-write race) falls back to today's bare message; never an error.
- A `pid` that no longer maps to a live process still prints what we have — the
  held lock is authoritative.
- A stale file with no lock held (crash leftover) is ignored by status; the next
  successful startup overwrites it and the next clean shutdown removes it.

### Testing

- State write/read round-trip, atomic overwrite, throttling.
- Rendering with and without `detail` and `log_path`.
- Corrupt-file fallback to the bare message.
- Extend the existing "starting up" lifecycle test to assert the enriched
  output.
- Table-driven, testify, `t.TempDir()`.
