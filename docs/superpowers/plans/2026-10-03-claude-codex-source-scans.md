# Claude and Codex source scans

> Use superpowers:executing-plans for this implementation.

**Goal:** Give Claude and Codex a shared, bounded metadata backstop alongside
existing native watchers, without adding a scanner database.

**Architecture:** Providers declare physical file globs on watch roots. The
existing Watcher owns scans, cancellation, dispatch, retries, and successful
archive acknowledgements. Native backends remain unchanged. Other providers
retain their existing polling behavior.

**Tech stack:** Go standard library, existing fsnotify/FSEvents and SQLite.

**Spec:** The narrower first implementation of
[central source watching](../specs/2026-10-01-central-source-watcher-design.md).

## Constraints and decisions

- Limit directory reads to 256 entries. Run scans outside the native event loop.
- Keep at most 65,536 signatures and 8 MiB of retained names. Store directory
  prefixes once. At capacity, uncached files are sent through archive freshness
  checks each pass; correctness does not depend on retaining all signatures.
- Schedule coverage 30 seconds after a pass completes. Keep the existing
  serialized callback. Scanner pages bypass the native-event dispatch floor.
- Commit observed signatures only after successful downstream work. Missing
  roots defer work; complete scans can request provider-owned reconciliation
  for disappearance. Scan absence alone never authorizes deletion.
- Keep existing authoritative native-loss recovery. Do not migrate other
  providers, archive formats, raw capture, or activity-hint interpretation.
- Benchmarks support the direction, not an end-to-end superiority claim.

## Implementation

- [x] Add behavior tests for filtered scans, retry acknowledgement, append,
  replacement, disappearance, missing roots, cache bounds, and cancellation.
- [x] Add the shared scanner and integrate its lifecycle with Watcher.
- [x] Declare Claude transcripts and Codex rollouts/title indexes. Route their
  coverage to scans rather than duplicate authoritative polling.
- [x] Exercise actual Claude/Codex parsers and SQLite through watcher batches;
  measure a 50,000-file unchanged pass and retained heap.
- [x] Run focused race tests, the Go suite, format/vet/lint, platform builds,
  and a fresh review. Update documentation, commit, and push the draft PR.

## Review focus

Changes during initial discovery must remain visible on the next pass. Failed
or partial scans must not delete archive rows. Callback failure must preserve
pending observations. Cache saturation must retain bounded memory. Scanning
and acknowledgement waits must not block native event collection or shutdown.

## Execution decisions

The first complete metadata pass requests provider reconciliation before its
seeded signatures can suppress dispatch. Companion directories reconcile only
available configured transcript scopes, never their containing directory.
Missing sibling scopes do not disable title-index coverage for available ones.
Saturated scopes reconcile every complete pass to catch uncached deletions.
Rejected scanner acknowledgement tokens leave retries, while native lifecycle
tokens remain pending. Claude project symlinks follow the existing discovery
contract. Other providers keep probe-gated polling.

## Verification limits

Focused parser/archive and watcher race tests pass. Linux vet, application
build, and full lint pass. The Go suite passes with `umask 077` and the existing
IPv6 artifact-exchange test excluded. That test also fails against the
pre-change tree through a read-only Go overlay. Capture tests fail with the
host's default group-writable temporary directories and pass with private
permissions. Windows scanner/provider packages compile and targeted Windows
lint passes; linking the whole application with the local cross compiler fails
on DuckDB C++ runtime symbols. Native Mac and Windows execution remains open.
