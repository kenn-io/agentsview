# Codex S3 Fork Replay Repair

## Goal

Keep Codex fork messages and usage correct when rollouts arrive through S3,
including when a child is imported before its parent is available.

## Scope

This repair covers the three findings approved during PR #1384 triage:

- resolve a fork's parent while parsing a materialized S3 child;
- keep a child retryable when its parent cannot yet be resolved; and
- make captured full-replay validation include explicit forks.

It does not add a general persisted parent-child dependency graph or restore
timestamp interpretation for opaque turn identifiers.

## Design

Before parsing a materialized Codex S3 child, read its explicit parent session
identifier from `session_meta`. Derive the parent object's URI from the child's
configured S3 root and Codex archive layout, then fetch only that named parent
into the same temporary Codex directory tree. The existing file-backed parent
turn resolver will then compare opaque `turn_context.turn_id` values without a
second parsing implementation.

If the parent object is missing, unreadable, or has no turn identifiers, parse
fail-open so child-owned data cannot be lost, but return the child with
`DataVersionNeedsRetry`. The sync layer must persist that retry marker rather
than treating the possibly inflated result as current. A later audit or source
sync will revisit the unchanged child and replace it once its parent can be
resolved. This is the narrow eventual-correction mechanism; no dependency index
is introduced. A non-empty parent is authoritative for the current parse. The
format has no marker that can distinguish a later replay turn omitted from a
parent snapshot from the child's first real turn, so tracking future parent
growth remains outside this repair.

Parent hydration must remain bounded to one named object per child and must use
the existing safe S3 path and temporary-file rules. A malformed parent ID or a
parent URI outside the child's configured Codex root fails open and leaves the
child retryable.

The captured full-replay test will select explicit fork children from the
rollout's `session_meta.forked_from_id`, matching the line-by-line replay test.
Production relationship classification remains unchanged.

## Verification

Behavioral tests will protect these contracts:

- the materialized S3 path fetches a named parent and excludes its replayed
  messages and token usage from the child;
- a missing parent stores a retryable child, and a later sync with the parent
  available replaces the inflated result with child-owned data only;
- captured full replay includes all explicit fork rollouts instead of filtering
  them through `RelationshipType`.

The focused parser, S3 sync, and database tests must pass, followed by
`go fmt ./...`, `go vet ./...`, and the repository's normal commit hooks.
