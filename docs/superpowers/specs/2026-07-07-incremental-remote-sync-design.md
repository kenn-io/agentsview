# Incremental HTTP Remote Sync

Date: 2026-07-07

## Problem

HTTP remote sync transfers the entire remote session corpus on every sync. The
client POSTs the resolved target dirs to `/api/v1/remote-sync/archive`, the
server tars every file under those dirs (uncompressed), and the client downloads
and extracts the full tar into a throwaway temp dir before importing. Parsing is
already incremental via the per-host skip cache (`remote_skipped_files`, remote
path to nanosecond mtime), but network, server tar CPU, and client disk/extract
cost are O(total corpus) per sync when the real delta is usually a handful of
files.

## Goals

- Transfer, server walk/read, and client extract become O(changed files).
- Zero per-agent knowledge in the sync protocol. Parsers that read sibling files
  (OpenCode message/part trees, OpenHands event dirs, Antigravity companions,
  Kiro exec logs, Cortex/Qoder/Command Code/Reasonix sidecars, Claude
  persisted tool results, Codex session index, Gemini project maps, VS Copilot
  cross-file de-dup) must keep working without the protocol modeling any of
  them.
- Parse-side behavior is unchanged: skip cache, fingerprints, session identity,
  Ephemeral engine semantics, blocked categories.
- Graceful interop with old daemons in both directions.

## Non-goals (future work)

- Append-aware byte-range fetch for grown JSONL files.
- Directory-level rollup hashes to shrink the manifest.
- SSH transport (`internal/ssh.RemoteSync`) adoption of the mirror; the
  client-side mirror/import pieces are transport-agnostic so SSH can adopt
  them later.
- Mirror GC beyond manifest-driven deletions.

## Design

### Persistent per-host mirror

The client maintains a persistent byte-for-byte mirror of the remote agent dirs
instead of a throwaway temp tree:

- Location: `<data_dir>/remote-mirrors/<sanitized-host>-<hash8>/` where `hash8`
  is a short hash of the exact configured host string. Sanitization alone can
  collide ("host:8080" vs "host_8080"); the hash disambiguates. Keying by
  `RemoteHost.Host` matches existing DB identity (skip-cache host key and
  session `IDPrefix` are `Host` alone today).
- Layout inside the mirror is the existing remapped-archive layout
  (`safeRemotePathArchiveName`: absolute paths flattened, `__drive_C` and
  `__unc` prefixes for Windows paths), so `tempPathToRemotePath` and the
  skip-cache translation helpers work unchanged with the mirror root in place
  of the temp root.
- The mirror is a disposable cache, not an archive. Deleting it forces a full
  re-download; the SQLite DB remains the source of truth. Files deleted on the
  remote are deleted from the mirror, but DB sessions are never deleted
  (unchanged Ephemeral engine semantics).
- The mirror is a complete regular-file mirror. Delta transfers do not
  materialize empty directories; no parser depends on one (per the
  sibling-read audit).
- No separate diff-state file: the diff basis is a stat walk of the mirror
  itself, which makes recovery automatic (see Error handling).

### Manifest endpoint

New route: `POST /api/v1/remote-sync/manifest`.

- Request body: the existing `TargetSet`, validated with `SelectAllowedTargets`
  exactly like the archive endpoint.
- Response: gzipped JSON listing every regular file under the allowed dirs plus
  allowed `ExtraFiles`:

```json
{
  "files": [
    {"path": "/home/u/.claude/projects/a/b.jsonl", "size": 123, "mtime_ns": 1751871234567890123}
  ]
}
```

- `mtime_ns` is Unix nanoseconds from the server's stat.
- Nanosecond mtimes do NOT survive today's tar writer: `writeArchiveHeader`
  leaves `Header.Format` unset, and Go's `tar.Writer.WriteHeader` rounds
  `ModTime` to whole seconds when the format is unknown (to avoid forcing PAX
  on every header). The existing skip cache tolerates this only because both
  syncs see the same rounded value. Both archive writers (full and delta share
  `writeArchiveHeader`) must set `hdr.Format = tar.FormatPAX` so the PAX mtime
  record carries nanoseconds end to end, covered by a nanosecond round-trip
  test (write archive, extract, compare `UnixNano`).
- Diff comparison truncates both sides (manifest `mtime_ns` and mirror stat) to
  1 microsecond. Some mirror filesystems store coarser timestamps than the
  server's (NTFS keeps 100 ns units), and exact ns equality would then
  mismatch forever and defeat O(delta); microsecond truncation absorbs that
  while still far exceeding the effective one-second resolution the skip cache
  gets from today's rounded tar headers.
- Symlinks and non-regular files are excluded, matching `WriteArchive`.
- Scale check: about 100k files compresses to roughly 1 MB.

### Delta archive mode

The archive request body gains an optional field:

```json
{"dirs": {...}, "extra_files": [...], "files": ["/abs/remote/path", ...]}
```

- When `files` is present, the server validates each entry with a new dedicated
  validator (not `SelectAllowedTargets`, which is exact-match only): the
  cleaned path must be under an allowed dir, exactly an allowed root (Aider
  resolves individual history files into the dirs set; a directory root
  streams nothing because the delta writer skips non-regular entries), or
  equal an allowed extra file, with relative paths, `..` components, and
  absolute-path tricks rejected (reusing the `paths.go` predicates; the
  absolute check is remote-OS neutral, not host `filepath.IsAbs`). It then
  streams a tar of only those files via a new `WriteArchiveFiles` that Lstats
  each path, silently skips vanished files (`os.IsNotExist`), and writes
  regular files only. The existing `writeArchivePath` errors on a missing root
  and must not be used for per-file requests.
- When `files` is absent, file selection and semantics are today's full-tree
  archive, but the tar bytes differ: headers now carry `tar.FormatPAX` with
  nanosecond mtimes. Old clients extract PAX transparently; the finer mtimes
  trigger their one-time re-parse described under Rollout.

### Compression

The archive handler gzips the tar stream and sets `Content-Encoding: gzip` when
the request advertises `Accept-Encoding: gzip`.

- Old clients on Go's default transport already advertise gzip and transparently
  decompress, so compressing the full archive for them is backward compatible
  (their `ContentLength` becomes unknown; the progress reader already handles
  a zero total).
- New clients set the header explicitly (which disables Go's transparent
  decompression), decompress themselves, and report progress in compressed
  bytes so totals stay meaningful.
- The manifest response is always gzipped (new clients only).

### Client flow

1. Fetch targets (unchanged).
1. `POST /manifest`. On 404/405/501, fall back to the legacy full-archive flow,
   byte-for-byte today's behavior (old daemon). Manifest availability also
   gates delta-archive usage: both ship in the same release, and an old server
   would silently ignore an unknown `files` field and return the full corpus,
   so the client never sends a delta request unless the manifest succeeded.
1. Acquire the per-mirror lock (see Locking).
1. Stat-walk the mirror and diff against the manifest:
    - fetch: files absent locally or with differing size or microsecond-truncated
      mtime;
    - delete: mirror files absent from the manifest, with deletion paths
      validated by `safeLocalArchivePath` so removal is confined to the mirror
      root.
1. Apply manifest deletions, pruning directories the removals leave empty.
   Deletions run before extraction so a remote path that changed type (a file
   replaced by a directory, or a directory by a file) cannot block the
   extraction that follows.
1. Bootstrap heuristic: if the mirror is empty or the fetch set exceeds a large
   fraction of the manifest (for example one half), request the legacy full
   archive instead of uploading a very large file list; the full tar extracts
   into the mirror as a complete refresh.
1. Download the delta tar, extract into the mirror with the existing
   `ExtractTarStream` (restores mtimes, rejects unsafe paths). If the delta
   archive request is rejected by the server, fall back to the full-archive
   flow for this sync.
1. Still holding the lock, run the existing `Importer` over the mirror root
   instead of a temp dir. Skip cache load/save, path rewriting, host
   prefixing, and blocked categories are unchanged. The lock is released after
   import returns.
1. `--full` keeps meaning "bypass the skip cache and re-parse everything" but no
   longer forces a full re-download: the mirror already holds the bytes, so
   only the delta is transferred first.

Both call sites (`cmd/agentsview/sync.go` `runHTTPRemoteSync` and the daemon's
`runRemoteSyncHosts` path) pass the data dir through to `HTTPSync` so the mirror
location is derived consistently.

### Locking

`engine.RunExclusive` (daemon) and the DB write lock (CLI) serialize process-
and DB-level work but do not serialize mirror mutation across processes. The
client takes an exclusive lock file (flock-style, same mechanism family as
`cmd/agentsview/write_lock.go`) held from the diff through download, extract,
delete, and import. Two placement/lifetime rules keep the lock sound:

- The lock file lives NEXT to the mirror, at
  `remote-mirrors/<sanitized-host>-<hash8>.lock`, not inside it. A lock file
  inside the mirror root would itself be absent from the manifest, so the
  deletion pass would remove it while held; a later process would then
  recreate a different file and flock a different inode, silently ending
  mutual exclusion.
- The lock is held through import, not just mutation. `ImportExtracted` points
  the sync engine at the mirror and reads files during `SyncAll`, while
  extraction truncates files in place; a concurrent sync mutating the mirror
  mid-import would hand the parsers torn files.

## Error handling and recovery

- Crash mid-extract: a truncated file's (size, mtime_ns) disagrees with the next
  manifest, so it is re-fetched. No journal needed.
- File changes between manifest and archive: the tar carries newer bytes and
  mtime; the next sync's diff either matches or re-fetches once. Harmless.
- File deleted between manifest and archive: `WriteArchiveFiles` skips it; the
  client tolerates requested-but-absent tar entries; the next manifest drops
  it, and the mirror copy is deleted then.
- Delta archive rejected mid-rollout (manifest present, delta refused): fall
  back to the full-archive flow for that sync.
- Mirror tampering or filesystem loss: the stat-walk diff self-heals by
  re-fetching anything that disagrees with the manifest; deleting the mirror
  entirely is always safe.

## Testing

- Unit: manifest diff (new, changed, deleted, sub-second mtime edge, microsecond
  truncation, same-size rewrite), mirror deletion confinement (traversal
  attempts; lock file untouched), delta file-list validation (outside allowed
  dirs, symlinks, `..`, exact extra-file match), `WriteArchiveFiles`
  vanished-file skip, PAX nanosecond mtime round-trip through write-extract,
  bootstrap heuristic threshold.
- Integration (httptest): two-sync end-to-end - first sync bootstraps the
  mirror, then mutate one file, add one, delete one on the "remote"; assert
  the second sync transfers only the delta and the resulting DB equals a
  from-scratch full sync. Fallback tests: server without the manifest route
  (legacy flow), and manifest-present but delta-archive-rejected (full-archive
  fallback). Compression round-trips for both old-style (transparent) and
  new-style (explicit) clients.
- Concurrency: two clients racing on one mirror serialize via the lock.
- Backend parity: not implicated. This is transfer plumbing into local SQLite;
  PG push is downstream and unaffected.

## Rollout and compatibility

| Client | Server | Behavior                                             |
| ------ | ------ | ---------------------------------------------------- |
| old    | old    | unchanged                                            |
| old    | new    | full archive (PAX tar, gzipped); one-time re-parse   |
| new    | old    | manifest 404 -> legacy full-archive flow             |
| new    | new    | manifest diff -> delta archive -> mirror -> importer |

First sync with a new client pair is a full download (mirror bootstrap); every
subsequent sync is O(delta).

One-time mtime-precision costs during rollout, both self-correcting:

- A mirror bootstrapped from an old server's tar holds second-rounded mtimes;
  the first manifest from an upgraded server reports sub-second values, so
  that sync re-fetches broadly once, after which mtimes agree.
- Old clients extracting a new server's PAX tars see nanosecond mtimes where
  their skip cache stored rounded seconds, causing a one-time full re-parse
  (not a larger download - they always download everything).
