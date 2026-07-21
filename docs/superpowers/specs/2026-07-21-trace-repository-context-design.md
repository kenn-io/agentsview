# Trace Repository Context Design

## Problem

The session analysis sidebar summarizes timing and tool activity, but it does
not identify the repository or working tree captured by the trace. Users can
therefore inspect a long-running session without being able to confirm which
checkout produced it.

## Approaches Considered

1. Pass the hydrated session to the analysis pane. This reuses metadata already
   loaded for the active transcript and keeps repository context separate from
   timing data.
1. Add repository fields to the timing endpoint. This would make the pane
   self-contained, but it would couple session identity to a timing-specific
   response and duplicate existing session-detail data.
1. Fetch the session detail again inside the analysis pane. This would avoid a
   new prop, but adds redundant network and loading state for data the app
   already owns.

The first approach is the smallest and clearest fit for the current frontend
architecture.

## Design

Add a compact repository-context block at the top of the existing Session
section, before the timing statistics. It contains two stacked label/value rows:

- **Repository** displays the session's persisted `project` label.
- **Worktree** displays the exact trace-recorded `cwd` value.

Values use the existing monospace data treatment. Long values truncate within
the narrow sidebar, while a native title tooltip exposes the complete value.
Hovering a row, or focusing within it by keyboard, reveals the shared copy
control for that value; touch devices keep the control visible. The block is
omitted until hydrated session metadata is available, so selecting an index-only
session does not flash placeholders that could be mistaken for recorded trace
data.

## Data Flow

`App.svelte` passes `sessions.activeSession` alongside the existing session ID.
`SessionVitals.svelte` uses the session ID for timing reads and the optional
session object for repository context. The frontend `Session` type gains the
already-present detail-response `cwd` field. No server, database, or timing API
changes are required.

## Localization and Accessibility

Add Repository and Worktree labels to every supported Paraglide catalog. The
repository label and path are technical identifiers and remain untranslated.
Each visible value has a matching `title` containing the complete text, so
truncation does not hide the recorded value from pointer users. Copy controls
have localized accessible names and copied-state feedback.

## Testing

Extend the `SessionVitals` component test to render a hydrated session and
assert the localized labels, repository label, worktree path, full-value titles,
and clipboard values. Existing component, localization, and frontend checks
cover the propagation and catalog synchronization.
