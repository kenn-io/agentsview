# Source Attribution Normalization Implementation Plan

> Continue the accepted immutable-attribution design in
> `docs/superpowers/specs/2026-07-31-immutable-session-source-attribution-design.md`.

**Goal:** Resolve a session's immutable machine attribution once, before any
project or ownership consumer runs, and keep warm DB-backed reconciliation
set-based.

## 1. Pin the regressions

- Add a write-path test where a stored session under machine A is reparsed from
  relabeled machine B while its checkout is unavailable. The durable project
  identity must still be recovered under A.
- Strengthen the relabel reconciliation test so an admitted A source paired
  with a configured B candidate retains deletion proof under A and is later
  tombstoned.
- Add a warm DB-backed test showing that one discovery page performs one
  attribution lookup regardless of the number of fresh sources, including a
  shared source containing sessions from multiple machines.

## 2. Normalize pending writes before consumers

- Add a bounded SQLite query that loads stored session machines by stable,
  prefixed session ID.
- Normalize every pending write to its stored machine when one exists, or keep
  its configured machine for first ingestion.
- Run normalization before unavailable-project recovery, worktree mapping,
  baseline admission, and persistence in both batch and single-session paths.
- Remove the late `prepareSessionWrite` lookup and the `persistedMachine` side
  channel.

## 3. Make source attribution set-based

- Add a bounded SQLite query returning every distinct active
  `(machine, agent, file_path)` attribution for a page of discovered sources.
- Replace the per-source `storedSourceMachine` lookup with one query per
  discovery page, retaining all machines represented inside shared sources.
- Make baseline replacement iterate the union of candidate and admitted
  machines so a stored A admission cannot be dropped merely because the
  configured candidate says B.

## 4. Verify and deliver

- Run the focused DB and sync tests, then `go fmt ./...`, `go vet ./...`, and
  the repository's relevant broader checks.
- Review the final diff, scrub public text for private data, and create one
  focused follow-up commit. Do not push.
