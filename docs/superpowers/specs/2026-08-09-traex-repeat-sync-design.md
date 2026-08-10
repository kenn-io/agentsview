# TraeX Repeat-Sync Design

## Problem

TraeX uses the Codex rollout format and supports incremental JSONL parsing. On
an unchanged rollout, the incremental gate finds that the current file size
equals the stored offset. That branch currently exempts only Codex, so TraeX
requests a full replacement before the later fingerprint-based database
freshness checks can skip the source. Periodic syncs therefore parse and replace
an untouched TraeX session.

## Design

Treat the equal-size decline as a Codex-format behavior. Replace the literal
Codex check with `isCodexFormatAgent(agent)`. Codex and TraeX will then decline
incremental parsing without requesting replacement and continue to the shared
post-fingerprint database freshness checks.

Keep the remaining behavior unchanged. Codex alone continues to fold
`session_index.jsonl` into its effective metadata, while TraeX continues to use
transcript-only metadata. Other incremental providers retain the existing
equal-size replacement behavior.

## Regression Coverage

Use a real temporary TraeX rollout and SQLite archive. Perform the initial sync,
then create a fresh engine over the same archive so no in-memory skip state can
hide the incremental decision. Sync the untouched path again and assert the
observable sync result: zero sessions synchronized and one session skipped. The
test must fail when the equal-size branch uses the literal Codex check.

## Scope

This change does not alter provider capabilities, parsing, storage schemas,
session metadata, or Codex index handling. It only lets unchanged TraeX rollouts
reach the freshness logic that already validates their stored fingerprint and
data version.
