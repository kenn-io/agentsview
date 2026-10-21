# Kit Snapshot Reader Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development or superpowers:executing-plans to
> implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for
> tracking.

**Goal:** Copy selected files from a backup snapshot without restoring an
application database or content store.

**Architecture:** A short-lived reader owns a shared repository lease, validated
snapshot membership, recipes, and a bounded pack-reader cache. It exposes
verified copies, keeping Kit's layout and integrity checks inside Kit.

**Tech Stack:** Go, existing Kit backup/pack packages, testify.

**Spec:** [Archive source design](../specs/2026-10-15-archive-source-design.md).
This is the first of three component plans; the
[Docbank wrapper](2026-10-16-docbank-snapshot-reader.md) follows it.

## Global constraints

- Baseline inspected: Kit v0.34.1. Inspect the implementation checkout and its
  instructions before edits; recheck any intervening backup changes.
- No new backup format, encryption mode, restore operation, or codec.
- Require an exact snapshot ID. Never choose latest or force a lock.
- Membership comes from the selected snapshot, not `LoadBlobIndex` alone.
- Bound open pack readers at 64. Cache descriptors and metadata, not payloads.
- A copy succeeds only after terminal hash and length verification and a
  successful stream close. A failed copy may have written partial output;
  callers must discard it.
- Readers are sequential: one copy at a time, with close after copies finish. Do
  not introduce parallel read orchestration in this delivery.
- Commit tested tasks locally. Publishing a release is a separate explicit
  action; downstream PRs must use a tagged release.

## Review focus

- A blob present only in another snapshot must be refused before output (K1).
- A declared size that disagrees with a recipe or footer must fail (K1).
- Writer failure and cancellation must not turn into successful copies (K1).
- Closing after an error must release the lease and every descriptor (K2).
- Prune may relocate objects between readers; reopening must reload indexes and
  still read the pinned snapshot (K2).

## File structure

All paths here are relative to the Kit checkout.

| Path                                          | Responsibility                                                |
| --------------------------------------------- | ------------------------------------------------------------- |
| `backup/snapshot_reader.go` (new)             | Public reader, membership and copy contract                   |
| `backup/snapshot_reader_test.go` (new)        | Real snapshots, integrity and lease behavior                  |
| `backup/pack_readers.go` (new)                | Private bounded pack reader and footer reuse                  |
| `backup/stream.go`, `backup/object_recipe.go` | Shared verified reads through a supplied blob opener          |
| `backup/attachments.go`, `backup/extras.go`   | Reuse list/tree decoding and validation                       |
| `backup/verify.go`                            | Reuse its cache rules; migrate common code only when required |
| `backup/FORMAT.md`                            | Reader lease, membership, partial-output and close contracts  |

## Public contract

Add these types and methods in `backup/snapshot_reader.go`:

```go
type SnapshotInfo struct {
    RepositoryID, SnapshotID string
    MinReaderVersion int
}
type SnapshotObject struct {
    ID pack.BlobID
    Bytes int64
}
func (r *Repo) OpenSnapshot(ctx context.Context, snapshotID, packExtension string) (*SnapshotReader, error)
func (r *SnapshotReader) Info() SnapshotInfo
func (r *SnapshotReader) Object(id pack.BlobID) (SnapshotObject, error)
func (r *SnapshotReader) Extra(name string) (SnapshotObject, error)
func (r *SnapshotReader) CopyObject(ctx context.Context, id pack.BlobID, dst io.Writer) (int64, error)
func (r *SnapshotReader) CopyExtra(ctx context.Context, name string, dst io.Writer) (int64, error)
func (r *SnapshotReader) Close() error
```

The lookup methods return immutable, validated manifest metadata and may be used
after close. They do not prove that payload bytes remain available. Copy methods
require an open reader. This lets a caller close the bootstrap lease before
inspecting a copied SQLite database, then validate its references against the
retained membership metadata. Each later read opens a fresh reader.

Accept logical objects named by attachment lists or extras. Recipe records,
their chunks, and portable catalog metadata are not independently readable
unless also explicitly listed as logical objects. Require consistent sizes for
repeated hashes and consistent entries for repeated extra paths. Lookup misses
wrap `fs.ErrNotExist`; lease contention preserves `ErrRepoLocked`.

### Task K1: Verified selected-object copies

**Files:** `backup/snapshot_reader.go`, `backup/snapshot_reader_test.go`,
`backup/stream.go`, `backup/object_recipe.go`, and existing list/tree decoders.

**Interfaces:** Produce the public contract above. Consume `LoadManifest`,
`LoadListRefs`, `loadObjectRecipes`, and the existing verified object streams;
factor private decoding only where the new reader actually shares it.

- [ ] Write `TestSnapshotReaderCopiesSelectedObjects` using production backup
  creation and the existing portable test application. Store an ordinary
  object, a `64<<20 + 1` byte object, and a chunked extra. Remove access to
  the source inputs. Copy into temporary files and independently compare full
  SHA-256 and byte count. Assert the fixture manifest contains recipes.

    ```go
    require.NoError(t, copyErr)
    assert.Equal(t, wantBytes, copiedBytes)
    assert.Equal(t, wantSHA256, gotSHA256)
    require.NoError(t, reader.Close())
    ```

- [ ] Add `TestSnapshotReaderRejectsUnselectedAndDamagedObjects`: a second
  snapshot's blob and an orphan blob produce no output; reordered/missing
  chunks, contradictory sizes, index/footer mismatch, and a newer required
  reader version produce errors. Include zero bytes and exactly 64 MiB. Add
  cancelled-copy and failing-writer cases that assert errors and no successful
  verification, rather than requiring rollback of caller output.

- [ ] Run `go test ./backup -run '^TestSnapshotReader'`; confirm failure from
  the missing API or each deliberately missing behavior before implementation.

- [ ] Implement the contract. Acquire the shared lease before manifests and
  indexes. Validate metadata without decoding unrelated content or catalog
  extras. Refactor `openObject`/`recipeStream` to consume a private verified
  blob opener, preserving full-object verification and early-close errors.
  Preserve existing restore/verify call sites through the same primitive.
  Release all acquired resources on open failure, and join close errors.

- [ ] Run the focused tests, then `go test ./backup/...`; run repository Go
  formatting, vet and required lint checks. Commit the verified read contract.

### Task K2: Bounded reuse and maintenance between readers

**Files:** `backup/pack_readers.go`, reader/stream tests, and
`backup/FORMAT.md`; `backup/verify.go` only for a shared primitive.

**Interfaces:** Keep K1's public contract unchanged. The private blob opener
owns the reader cache and checks index metadata against cached pack footers. The
snapshot reader owns its lifetime and releases it before the lease.

- [ ] Add `TestSnapshotReaderReusesAndClosesPacks`. Repeated real object/chunk
  reads from one pack open it once per reader, including its footer. Exceed
  the 64-pack bound and verify eviction closes descriptors; error and normal
  close release all of them. Instrument the actual pack-open boundary, not a
  fake object copier. A concurrent copy is outside this sequential contract.

    ```go
    assert.Equal(t, 1, opensForRepeatedPack)
    assert.LessOrEqual(t, peakOpenPacks, 64)
    require.NoError(t, reader.Close())
    assert.Equal(t, 0, remainingOpenPacks)
    ```

- [ ] Add `TestSnapshotReaderYieldsToBackupAndReopensAfterPrune`. Hold a real
  reader while a real backup acquires its exclusive claim; close the reader
  and observe backup progress. New readers must report `ErrRepoLocked` until
  backup releases. Prune/repack while no reader is open, then read the same
  snapshot with a fresh reader. Use channels for ordering and long hang
  guards, not a throughput deadline. Also remove a needed pack under a forced
  maintenance operation and require an integrity/read error, never success.

- [ ] Run these tests before adding cache/reopen behavior; confirm the cache
  assertion fails without reuse. Implement the 64-reader bound using the
  verifier's existing pattern; never evict a reader during an active copy.
  Reload placement on every `OpenSnapshot`, keeping no descriptors across
  close. Do not cache decoded bytes or retain a shared lease across sources.

- [ ] Document callers' responsibility to discard failed output and close
  readers promptly. Add a public example that copies one extra, closes the
  reader, then inspects the verified file.

- [ ] Run `go test ./backup/... ./pack/...` and focused race tests. Run
  formatting, vet and required lint checks; commit. Record the resulting
  commit for review, then use the normal release process when authorized.

The release gate is a tested public reader, not a change to existing backup
files. Docbank integration must not depend on a private function or an untagged
commit in a mergeable PR.
