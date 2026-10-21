# Docbank Snapshot Reader Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development or superpowers:executing-plans to
> implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for
> tracking.

**Goal:** Let an embedding application read selected backup files without
opening or restoring a Docbank vault.

**Architecture:** `BackupRepository` wraps the
[Kit snapshot reader](2026-10-16-kit-snapshot-reader.md). Public arguments and
results use Docbank types; the wrapper supplies Kit's pack extension and
preserves integrity, lease, cancellation and close errors.

**Tech Stack:** Go, Kit backup reader, Docbank embedded API, testify.

**Spec:** [Archive source design](../specs/2026-10-15-archive-source-design.md).
The [AgentsView plan](2026-10-16-archive-source.md) consumes this API.

## Global constraints

- Baseline inspected: Docbank v0.15.2. Recheck intervening changes first.
- Use a tagged Kit release containing K1/K2 before this PR becomes mergeable.
- No live vault, catalog restore, placement changes, new compression settings,
  or access to Kit's on-disk filenames from the wrapper.
- An exact snapshot is required; no force option or automatic latest snapshot.
- Keep `errors.Is(err, docbank.ErrBackupRepositoryLocked)` working.
- A reader owns one lease window. Metadata lookup survives close; copies do not.
  Partial output on error is the caller's responsibility.
- Keep application-specific names and private rehearsal data out of this public
  repository. Publishing a release requires explicit authorization.

## Review focus

- A host extra over 4 GiB must stream through the public API (D1/rehearsal).
- Lookup after close must work without opening any repository files (D1).
- A missing member must not fall through to the repository-wide index (D1).
- Reopening after backup/prune must use fresh Kit placement state (D1).
- Failed open/copy/close must preserve actionable errors and release custody
  even when no Vault object was ever created (D1).

## File structure

Paths are relative to the Docbank checkout.

| Path                                   | Responsibility                               |
| -------------------------------------- | -------------------------------------------- |
| `backup_reader.go` (new)               | Public wrapper and Docbank metadata types    |
| `backup_reader_external_test.go` (new) | External-package contract tests              |
| `backup_external_test.go`              | Existing production backup fixture patterns  |
| `go.mod`, `go.sum`                     | Tagged Kit reader dependency                 |
| `docs/embedding.md`                    | Selective backup reads and lifecycle example |

### Task D1: Expose and prove the embedded reader

**Interfaces:** Consume K1's `backup.SnapshotReader`. Produce:

```go
type BackupReaderInfo struct {
    RepositoryID, SnapshotID string
    MinReaderVersion int
}
type BackupObject struct {
    SHA256 string
    Bytes int64
}
func (r *BackupRepository) OpenSnapshot(ctx context.Context, snapshotID string) (*BackupReader, error)
func (r *BackupReader) Info() BackupReaderInfo
func (r *BackupReader) Object(sha256 string) (BackupObject, error)
func (r *BackupReader) Extra(name string) (BackupObject, error)
func (r *BackupReader) CopyObject(ctx context.Context, sha256 string, dst io.Writer) (int64, error)
func (r *BackupReader) CopyExtra(ctx context.Context, name string, dst io.Writer) (int64, error)
func (r *BackupReader) Close() error
```

- [ ] Write `TestBackupReaderWithoutVault` in package `docbank_test`. Use
  `Vault.CreateBackup` with a host extra and a `64<<20 + 1` byte logical
  object, close the vault and make its root inaccessible, then open only the
  backup repository. Copy both inputs and compare independent hashes and
  sizes. Read metadata after close; a later copy must fail.

    ```go
    assert.Equal(t, snapshot.ID, reader.Info().SnapshotID)
    require.NoError(t, copyErr)
    assert.Equal(t, expectedDigest, copiedDigest)
    require.NoError(t, reader.Close())
    _, err = reader.CopyObject(ctx, objectHash, io.Discard)
    require.Error(t, err)
    ```

- [ ] Add external-package cases for malformed hashes, nonmember hashes, missing
  extras, incompatible snapshots, damaged chunks, cancelled/writer failures,
  and real backup contention. Require the lock sentinel through `errors.Is`,
  followed by a successful retry after the backup finishes. Verify no restored
  catalog/content directory appears. Reuse K2 for cache internals; do not
  duplicate its implementation tests in Docbank.

- [ ] Run `go test -tags fts5 -run '^TestBackupReader' .` and observe the
  missing API. Pin the tagged Kit release, add the thin wrapper and checked
  hash conversion, and obtain the pack extension from `backupapp.App`. Return
  copied bytes and errors unchanged except for useful context.

- [ ] Add an embedding example that opens a repository, selects an explicit
  snapshot, reports an extra's size, copies to private temporary storage,
  closes the reader, and only then consumes the file. Explain lease writes,
  lock contention, partial-output disposal and the absence of a restore.

- [ ] Run
  `go test -tags fts5 -run 'TestBackupReader|TestEmbeddedBackup|TestRepositoryRestore' .`
  and `go test -tags fts5 ./internal/backupapp/...`. Run repository
  formatting, vet and required lint checks. Commit and prepare the normal
  review/release handoff. AgentsView pins the resulting tagged Docbank
  release.

Use A6's saved application database, retained original over 4 GiB and generated
host extra over 4 GiB for the large public-reader checks. Keep the real data
private, stream to temporary files and independently hash them; do not check a
multi-GiB fixture into either repository.
