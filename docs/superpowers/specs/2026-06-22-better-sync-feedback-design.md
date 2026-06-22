# Better sync feedback design

## Goal

When a data-version upgrade triggers a full resync, users should see clear
progress for long-running stages in both `agentsview serve` output and the Tauri
status bar. The most important silent stage is rebuilding the FTS index, which
can make the app look frozen.

## Approach

Use the existing sync progress model and `/api/v1/sync/status` polling path. The
engine will keep the current sync progress in memory while a sync is running,
clear it when the sync finishes or aborts, and include it in the sync status
response. Manually triggered `/sync` and `/resync` streams will continue to send
the same progress frames, with richer phase/detail fields.

## Progress model

Extend `sync.Progress` with optional display-oriented fields:

- `detail`: short user-facing status text.
- `hint`: optional longer note for expensive stages.
- `resync`: true when the current run is a full database rebuild.

Keep existing counters (`sessions_total`, `sessions_done`, `messages_indexed`)
so existing clients remain compatible.

Add explicit phases for full-resync milestones:

- preparing the temporary database
- syncing sessions
- closing/copying metadata
- copying archived orphan sessions
- reclassifying sessions
- rebuilding the FTS search index
- swapping the rebuilt database into place

The FTS phase should set a hint such as "Rebuilding the search index may take a
while on large archives."

## Backend data flow

`Engine` will own `currentProgress` under its existing mutex. Sync entrypoints
wrap caller-provided progress callbacks so every progress update is both stored
and forwarded. `LastSyncStats` remains the last completed run; current progress
is separate and transient.

`/api/v1/sync/status` will add an optional `progress` field. If no sync is
running, the field is omitted or null.

## CLI behavior

`printSyncProgress` will render phase/detail text as it arrives. Session
progress stays compact, while long resync phases print enough text to explain
what is happening. The FTS rebuild phase must explicitly say it may take a
while.

## Frontend behavior

The sync store will hydrate `syncing` and `progress` from `/api/v1/sync/status`
when a server-side startup sync is running. The status bar will render `detail`
and append `hint` in the title. For existing progress without detail, it will
fall back to counter-based labels.

## Testing

Add tests for:

- Engine current-progress state is populated during sync and cleared when done.
- `ResyncAll` emits a rebuild-FTS progress event with the expected hint.
- `/api/v1/sync/status` includes current progress while a sync is running.
- The frontend sync store adopts status progress from polling.
- The status bar renders detailed sync progress and the FTS hint.
