# Incremental HTTP Remote Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the all-or-nothing HTTP remote sync transfer with a
manifest-diff protocol against a persistent per-host local mirror, making
network/server/extract cost O(changed files).

**Architecture:** The server gains a `POST /api/v1/remote-sync/manifest`
endpoint (file list with size + nanosecond mtime) and a delta mode on the
existing archive endpoint (explicit `files` list). The client keeps a persistent
byte-for-byte mirror under `<data_dir>/remote-mirrors/<host-key>/`, diffs the
manifest against a stat walk of the mirror, downloads only changed files, and
runs the existing `Importer` over the complete mirror tree so every parser
sibling-read keeps working. Spec:
`docs/superpowers/specs/2026-07-07-incremental-remote-sync-design.md`.

**Tech Stack:** Go stdlib (`archive/tar` with PAX, `compress/gzip`),
`github.com/gofrs/flock` (already a dependency), testify, httptest.

## Global Constraints

- Build/test always with `CGO_ENABLED=1` and `-tags fts5`.
- Tests use testify: `require.X` for aborting checks, `assert.X` for independent
  checks. Never `if got != want { t.Fatalf }`.
- Temp dirs via `t.TempDir()` only.
- After Go changes run `go fmt ./...` and `go vet ./...` before committing.
- Commit every task; pre-commit hook must pass. Set
  `GOLANGCI_LINT_CACHE="$PWD/.golangci-cache"` on every `git commit` (shared
  cache across worktrees corrupts suggested fixes).
- 100-char line limit; no emojis; prefer stdlib.
- Backward compatibility: old clients and old servers must keep working exactly
  as specced (see rollout table in the spec).

## Amendments (2026-07-08, post-review)

Roborev reviews of the executed Tasks 3 and 5 changed committed code beyond the
blocks printed below; the committed code is authoritative:

- `selectAllowedFile` accepts EXACT matches to allowed roots (Aider resolves
  history files into `Dirs`; a directory root streams nothing because
  `WriteArchiveFiles` skips non-regular entries) and uses an OS-neutral
  `isAbsRemotePath` instead of `filepath.IsAbs` (host semantics would reject
  POSIX paths on Windows CI).
- `ApplyMirrorDeletions` prunes directories that removals leave empty, and Task
  7's `runMirror` applies deletions BEFORE extraction, so remote path type
  changes (file to directory or back) cannot wedge the mirror.
- Task 9's test seeds four initial sessions (a, b, d, e) instead of two, so the
  second sync's two changed files (a appended, c added) stay under the
  half-corpus bootstrap heuristic and exercise the delta path; the test also
  asserts the second archive request carries exactly those two files. The
  committed test is authoritative over the block printed below.
- `selectAllowedFile` additionally requires the requested file and the allowed
  root to share a path dialect (POSIX, Windows drive, UNC) before the
  archive-name prefix comparison, and rejects files whose symlinked ancestors
  resolve outside the allowed root. `runMirror` removes directories stranded
  at fetch paths (crashed extraction) via `RemoveMirrorTypeConflicts`, and the
  archive handler treats an explicit empty `files` list as a delta request
  yielding an empty tar.

______________________________________________________________________

### Task 1: PAX headers so nanosecond mtimes survive the tar round-trip

Go's `tar.Writer.WriteHeader` rounds `ModTime` to whole seconds when
`Header.Format` is unknown. The manifest diff compares nanosecond mtimes, so
both archive writers must emit PAX headers.

**Files:**

- Modify: `internal/remotesync/archive.go` (`writeArchiveHeader`)
- Test: `internal/remotesync/archive_test.go`

**Interfaces:**

- Consumes: existing `WriteArchive`, `ExtractTarStream`.

- Produces: archives whose extracted files satisfy
  `extractedInfo.ModTime().UnixNano() == sourceInfo.ModTime().UnixNano()`
  (given a nanosecond-capable filesystem). Tasks 5 and 7 rely on this.

- [ ] **Step 1: Write the failing test** (append to
  `internal/remotesync/archive_test.go`)

```go
func TestWriteArchivePreservesNanosecondMtime(t *testing.T) {
	srcDir := t.TempDir()
	path := filepath.Join(srcDir, "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o644))
	mtime := time.Date(2026, 7, 8, 10, 30, 0, 123456789, time.UTC)
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	info, err := os.Stat(path)
	require.NoError(t, err)
	if info.ModTime().Nanosecond() == 0 {
		t.Skip("filesystem does not store nanosecond mtimes")
	}

	var buf bytes.Buffer
	require.NoError(t, WriteArchive(&buf, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {srcDir}},
	}))

	dstDir := t.TempDir()
	_, err = ExtractTarStream(context.Background(), &buf, dstDir)
	require.NoError(t, err)
	extracted, err := safeRemappedRemotePath(dstDir, path)
	require.NoError(t, err)
	extractedInfo, err := os.Stat(extracted)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), extractedInfo.ModTime().UnixNano())
}
```

Add any missing imports (`bytes`, `context`, `time`, `os`, `path/filepath`,
`go.kenn.io/agentsview/internal/parser`) to the existing import block.

- [ ] **Step 2: Run test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestWriteArchivePreservesNanosecondMtime -v`
Expected: FAIL — extracted `UnixNano` is second-rounded (ends in 000000000).

- [ ] **Step 3: Set PAX format in `writeArchiveHeader`**

In `internal/remotesync/archive.go`, after `hdr.Name = name` inside
`writeArchiveHeader`, add:

```go
	// PAX carries sub-second mtimes; the default (unknown) format
	// makes tar.Writer round ModTime to whole seconds, which would
	// desync the manifest's mtime_ns diff from extracted files.
	hdr.Format = tar.FormatPAX
```

- [ ] **Step 4: Run the package tests**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -v -run TestWriteArchive`
Expected: PASS (new test and existing archive tests).

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/archive.go internal/remotesync/archive_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(remotesync): emit PAX tar headers to preserve nanosecond mtimes"
```

______________________________________________________________________

### Task 2: Manifest types and server-side builder

**Files:**

- Create: `internal/remotesync/manifest.go`
- Test: `internal/remotesync/manifest_test.go`

**Interfaces:**

- Consumes: `TargetSet` from `types.go`.
- Produces (used by Tasks 5, 6, 7):

```go
type ManifestEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
}

type Manifest struct {
	Files []ManifestEntry `json:"files"`
}

func BuildManifest(targets TargetSet) (Manifest, error)
```

- [ ] **Step 1: Write the failing test**
  (`internal/remotesync/manifest_test.go`)

```go
package remotesync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestBuildManifestListsRegularFilesWithSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	a := filepath.Join(sub, "a.jsonl")
	require.NoError(t, os.WriteFile(a, []byte("aaaa"), 0o644))
	mtime := time.Date(2026, 7, 8, 9, 0, 0, 987654321, time.UTC)
	require.NoError(t, os.Chtimes(a, mtime, mtime))
	require.NoError(t, os.Symlink(a, filepath.Join(sub, "link.jsonl")))
	extra := filepath.Join(dir, "index.jsonl")
	require.NoError(t, os.WriteFile(extra, []byte("x"), 0o644))

	m, err := BuildManifest(TargetSet{
		Dirs:       map[parser.AgentType][]string{parser.AgentClaude: {sub}},
		ExtraFiles: []string{extra},
	})
	require.NoError(t, err)

	// Sorted by path: <tmp>/index.jsonl precedes <tmp>/proj/a.jsonl.
	require.Len(t, m.Files, 2)
	assert.Equal(t, extra, m.Files[0].Path)
	assert.Equal(t, a, m.Files[1].Path)
	assert.Equal(t, int64(4), m.Files[1].Size)
	info, err := os.Stat(a)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), m.Files[1].MtimeNS)
}

func TestBuildManifestToleratesMissingRootsAndExtraFiles(t *testing.T) {
	dir := t.TempDir()
	m, err := BuildManifest(TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {filepath.Join(dir, "gone")},
		},
		ExtraFiles: []string{filepath.Join(dir, "gone.jsonl")},
	})
	require.NoError(t, err)
	assert.Empty(t, m.Files)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestBuildManifest -v`
Expected: FAIL — `BuildManifest` undefined.

- [ ] **Step 3: Implement** (`internal/remotesync/manifest.go`)

```go
package remotesync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// ManifestEntry describes one regular file available for remote sync.
// MtimeNS is Unix nanoseconds from the server's stat; combined with
// PAX tar headers it gives the client's mirror diff the same
// (size, mtime) change signal local sync uses.
type ManifestEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
}

// Manifest lists every syncable regular file under a TargetSet.
type Manifest struct {
	Files []ManifestEntry `json:"files"`
}

// BuildManifest walks the target dirs and extra files and returns a
// manifest of regular files, sorted by path. Symlinks and special
// files are excluded, matching WriteArchive. Missing roots and extra
// files are tolerated: sync races against live agents deleting files.
func BuildManifest(targets TargetSet) (Manifest, error) {
	m := Manifest{Files: []ManifestEntry{}}
	add := func(path string, info os.FileInfo) {
		m.Files = append(m.Files, ManifestEntry{
			Path:    path,
			Size:    info.Size(),
			MtimeNS: info.ModTime().UnixNano(),
		})
	}
	for _, dirs := range targets.Dirs {
		for _, root := range dirs {
			if err := manifestWalk(root, add); err != nil {
				return Manifest{}, err
			}
		}
	}
	for _, path := range targets.ExtraFiles {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return Manifest{}, fmt.Errorf("stat manifest file %q: %w", path, err)
		}
		if info.Mode().IsRegular() {
			add(path, info)
		}
	}
	sort.Slice(m.Files, func(i, j int) bool {
		return m.Files[i].Path < m.Files[j].Path
	})
	return m, nil
}

func manifestWalk(root string, add func(string, os.FileInfo)) error {
	info, err := os.Lstat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat manifest root %q: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() {
			add(root, info)
		}
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		add(path, info)
		return nil
	})
}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestBuildManifest -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/manifest.go internal/remotesync/manifest_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(remotesync): add manifest types and server-side builder"
```

______________________________________________________________________

### Task 3: Per-file request validator

`SelectAllowedTargets` is exact-match only; delta requests name individual files
that must be validated as strictly under an allowed dir (or exactly an allowed
extra file).

**Files:**

- Modify: `internal/remotesync/resolve.go`
- Test: `internal/remotesync/resolve_test.go`

**Interfaces:**

- Consumes: `TargetSet`, `safeRemotePathArchiveName`, `remoteArchiveRel`,
  `selectAllowedString` (all in package).
- Produces (used by Task 6):

```go
func SelectAllowedFiles(allowed TargetSet, files []string) ([]string, bool)
```

Returns the canonical file list, or `(nil, false)` if ANY entry is not allowed
(fail closed, matching `SelectAllowedTargets`).

- [ ] **Step 1: Write the failing test** (append to
  `internal/remotesync/resolve_test.go`)

```go
func TestSelectAllowedFiles(t *testing.T) {
	allowed := TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"/home/u/.claude/projects"},
		},
		ExtraFiles: []string{"/home/u/.codex/session_index.jsonl"},
	}
	tests := []struct {
		name  string
		files []string
		ok    bool
	}{
		{"under allowed dir", []string{"/home/u/.claude/projects/p/s.jsonl"}, true},
		{"nested under allowed dir", []string{"/home/u/.claude/projects/a/b/c.jsonl"}, true},
		{"exact extra file", []string{"/home/u/.codex/session_index.jsonl"}, true},
		{"outside allowed dirs", []string{"/etc/passwd"}, false},
		{"prefix sibling escape", []string{"/home/u/.claude/projects-evil/x"}, false},
		{"dir itself not a file grant", []string{"/home/u/.claude/projects"}, false},
		{"dot dot traversal", []string{"/home/u/.claude/projects/../../etc/passwd"}, false},
		{"one bad entry rejects all", []string{
			"/home/u/.claude/projects/p/s.jsonl", "/etc/passwd",
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected, ok := SelectAllowedFiles(allowed, tt.files)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.files, selected)
			} else {
				assert.Nil(t, selected)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestSelectAllowedFiles -v`
Expected: FAIL — `SelectAllowedFiles` undefined.

- [ ] **Step 3: Implement** (append to `internal/remotesync/resolve.go`)

```go
// SelectAllowedFiles validates a delta-archive file list: every entry
// must be strictly under an allowed dir or exactly an allowed extra
// file. Any disallowed entry rejects the whole request (fail closed,
// like SelectAllowedTargets). Path traversal is rejected by
// safeRemotePathArchiveName before any prefix comparison.
func SelectAllowedFiles(allowed TargetSet, files []string) ([]string, bool) {
	selected := make([]string, 0, len(files))
	for _, file := range files {
		canonical, ok := selectAllowedFile(allowed, file)
		if !ok {
			return nil, false
		}
		selected = append(selected, canonical)
	}
	return selected, true
}

func selectAllowedFile(allowed TargetSet, file string) (string, bool) {
	if canonical, ok := selectAllowedString(allowed.ExtraFiles, file); ok {
		return canonical, true
	}
	if _, err := safeRemotePathArchiveName(file); err != nil {
		return "", false
	}
	for _, dirs := range allowed.Dirs {
		for _, dir := range dirs {
			rel, ok := remoteArchiveRel(dir, file)
			if ok && rel != "" {
				return file, true
			}
		}
	}
	return "", false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestSelectAllowedFiles -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/resolve.go internal/remotesync/resolve_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(remotesync): validate per-file delta archive requests"
```

______________________________________________________________________

### Task 4: Delta archive writer

**Files:**

- Modify: `internal/remotesync/archive.go`
- Test: `internal/remotesync/archive_test.go`

**Interfaces:**

- Consumes: `writeArchiveFile` (existing, already skips a file that vanishes
  between stat and open).
- Produces (used by Task 6):

```go
func WriteArchiveFiles(w io.Writer, files []string) error
```

Skips vanished/symlink/non-regular entries silently; `writeArchivePath` must NOT
be used here (it errors on a missing root).

- [ ] **Step 1: Write the failing test** (append to
  `internal/remotesync/archive_test.go`)

```go
func TestWriteArchiveFilesSkipsVanishedAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.jsonl")
	require.NoError(t, os.WriteFile(keep, []byte("k"), 0o644))
	link := filepath.Join(dir, "link.jsonl")
	require.NoError(t, os.Symlink(keep, link))
	gone := filepath.Join(dir, "gone.jsonl")

	var buf bytes.Buffer
	require.NoError(t, WriteArchiveFiles(&buf, []string{gone, link, keep}))

	names := []string{}
	tr := tar.NewReader(&buf)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
	}
	require.Len(t, names, 1)
	assert.Contains(t, names[0], "keep.jsonl")
}
```

Add missing imports (`archive/tar`, `errors`, `io`) if absent.

- [ ] **Step 2: Run test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestWriteArchiveFiles -v`
Expected: FAIL — `WriteArchiveFiles` undefined.

- [ ] **Step 3: Implement** (append to `internal/remotesync/archive.go`)

```go
// WriteArchiveFiles streams a tar containing exactly the given files.
// Entries that vanished since the client's manifest diff, symlinks,
// and non-regular files are skipped silently: deletions race live
// agents and are reconciled by the next manifest. writeArchivePath is
// unsuitable here because it fails on a missing root.
func WriteArchiveFiles(w io.Writer, files []string) error {
	tw := tar.NewWriter(w)
	for _, path := range files {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("stat archive file %q: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			continue
		}
		if err := writeArchiveFile(tw, path, info); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestWriteArchiveFiles -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/archive.go internal/remotesync/archive_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(remotesync): add per-file delta archive writer"
```

______________________________________________________________________

### Task 5: Mirror directory, diff, deletions, and lock

**Files:**

- Create: `internal/remotesync/mirror.go`
- Test: `internal/remotesync/mirror_test.go`

**Interfaces:**

- Consumes: `Manifest`/`ManifestEntry` (Task 2), `safeRemappedRemotePath`,
  `within` (package helpers), `github.com/gofrs/flock`.
- Produces (used by Task 7):

```go
func MirrorDir(dataDir, host string) string

type MirrorDelta struct {
	Fetch     []string // remote paths to download, sorted
	Deletions []string // local paths under the mirror to remove, sorted
	Total     int      // total manifest entries
}

func MirrorDiff(mirrorRoot string, m Manifest) (MirrorDelta, error)
func ApplyMirrorDeletions(mirrorRoot string, deletions []string) error

type MirrorLockHandle struct{ /* private */ }
func AcquireMirrorLock(ctx context.Context, mirrorRoot string) (*MirrorLockHandle, error)
func (h *MirrorLockHandle) Close() error
```

- [ ] **Step 1: Write the failing tests** (`internal/remotesync/mirror_test.go`)

```go
package remotesync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMirrorDirDisambiguatesSanitizedCollisions(t *testing.T) {
	a := MirrorDir("/data", "host:8080")
	b := MirrorDir("/data", "host_8080")
	assert.NotEqual(t, a, b)
	assert.True(t, strings.HasPrefix(a, filepath.Join("/data", "remote-mirrors")+string(filepath.Separator)))
	assert.NotContains(t, filepath.Base(a), ":")
}

func writeMirrorFile(t *testing.T, root, remotePath, content string, mtime time.Time) string {
	t.Helper()
	local, err := safeRemappedRemotePath(root, remotePath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
	require.NoError(t, os.WriteFile(local, []byte(content), 0o644))
	require.NoError(t, os.Chtimes(local, mtime, mtime))
	return local
}

func TestMirrorDiffFetchesNewChangedAndDeletesStale(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 7, 8, 10, 0, 0, 111222333, time.UTC)
	unchanged := "/home/u/.claude/projects/p/unchanged.jsonl"
	changedMtime := "/home/u/.claude/projects/p/newer.jsonl"
	changedSize := "/home/u/.claude/projects/p/grew.jsonl"
	writeMirrorFile(t, root, unchanged, "aa", base)
	writeMirrorFile(t, root, changedMtime, "bb", base)
	writeMirrorFile(t, root, changedSize, "cc", base)
	staleLocal := writeMirrorFile(t, root, "/home/u/.claude/projects/p/stale.jsonl", "dd", base)

	m := Manifest{Files: []ManifestEntry{
		{Path: unchanged, Size: 2, MtimeNS: base.UnixNano()},
		{Path: changedMtime, Size: 2, MtimeNS: base.Add(3 * time.Second).UnixNano()},
		{Path: changedSize, Size: 9, MtimeNS: base.UnixNano()},
		{Path: "/home/u/.claude/projects/p/new.jsonl", Size: 1, MtimeNS: base.UnixNano()},
	}}
	delta, err := MirrorDiff(root, m)
	require.NoError(t, err)
	assert.Equal(t, []string{
		changedSize,
		"/home/u/.claude/projects/p/new.jsonl",
		changedMtime,
	}, delta.Fetch)
	assert.Equal(t, []string{staleLocal}, delta.Deletions)
	assert.Equal(t, 4, delta.Total)
}

func TestMirrorDiffTruncatesMtimeToMicroseconds(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 7, 8, 10, 0, 0, 111222000, time.UTC)
	path := "/home/u/.claude/projects/p/s.jsonl"
	writeMirrorFile(t, root, path, "aa", base)
	m := Manifest{Files: []ManifestEntry{
		// Same microsecond, different nanosecond remainder: unchanged.
		{Path: path, Size: 2, MtimeNS: base.UnixNano() + 999},
	}}
	delta, err := MirrorDiff(root, m)
	require.NoError(t, err)
	assert.Empty(t, delta.Fetch)
	assert.Empty(t, delta.Deletions)
}

func TestMirrorDiffEmptyMirrorFetchesEverything(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	m := Manifest{Files: []ManifestEntry{
		{Path: "/home/u/.claude/projects/p/s.jsonl", Size: 1, MtimeNS: 1},
	}}
	delta, err := MirrorDiff(root, m)
	require.NoError(t, err)
	assert.Len(t, delta.Fetch, 1)
	assert.Empty(t, delta.Deletions)
}

func TestApplyMirrorDeletionsConfinedToRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "victim.txt")
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o644))
	err := ApplyMirrorDeletions(root, []string{outside})
	require.Error(t, err)
	assert.FileExists(t, outside)

	inside := writeMirrorFile(t, root, "/home/u/.claude/projects/p/s.jsonl", "x",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
	require.NoError(t, ApplyMirrorDeletions(root, []string{inside}))
	assert.NoFileExists(t, inside)
}

func TestAcquireMirrorLockIsExclusive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "mirror")
	lock, err := AcquireMirrorLock(context.Background(), root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = AcquireMirrorLock(ctx, root)
	require.Error(t, err)

	require.NoError(t, lock.Close())
	second, err := AcquireMirrorLock(context.Background(), root)
	require.NoError(t, err)
	require.NoError(t, second.Close())
}
```

Note on `TestMirrorDiffFetchesNewChangedAndDeletesStale` expected order: `Fetch`
is sorted lexically; `grew.jsonl` < `new.jsonl` < `newer.jsonl`.

Note on `TestAcquireMirrorLockIsExclusive`: flock is per-process on some
platforms via the same \*flock.Flock path, but `gofrs/flock` tracks lock state
per Flock instance, so a second `AcquireMirrorLock` in the same process still
contends. If the second acquire unexpectedly succeeds on some platform, convert
the contention check to spawn the check in a subprocess — but try the in-process
version first.

- [ ] **Step 2: Run tests to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run 'TestMirror|TestApplyMirror|TestAcquireMirror' -v`
Expected: FAIL — symbols undefined.

- [ ] **Step 3: Implement** (`internal/remotesync/mirror.go`)

```go
package remotesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// MirrorDir returns the persistent mirror root for a remote host.
// The sanitized host keeps the path readable; the hash suffix
// disambiguates hosts whose sanitized forms collide ("host:8080" vs
// "host_8080"). Keying on the configured host string matches the
// existing DB identity (remote_skipped_files host, session IDPrefix).
func MirrorDir(dataDir, host string) string {
	sum := sha256.Sum256([]byte(host))
	name := sanitizeMirrorHost(host) + "-" + hex.EncodeToString(sum[:4])
	return filepath.Join(dataDir, "remote-mirrors", name)
}

func sanitizeMirrorHost(host string) string {
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('_')
		}
	}
	s := b.String()
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" {
		s = "host"
	}
	return s
}

// MirrorDelta is the result of diffing a manifest against the mirror.
type MirrorDelta struct {
	Fetch     []string
	Deletions []string
	Total     int
}

// MirrorDiff stat-walks the mirror and compares it against the
// manifest. A file is fetched when it is absent or differs in size or
// microsecond-truncated mtime; mirror files absent from the manifest
// are queued for deletion. Stat-based diffing is self-healing: a
// crashed extraction leaves mismatched size/mtime and is re-fetched.
func MirrorDiff(mirrorRoot string, m Manifest) (MirrorDelta, error) {
	delta := MirrorDelta{Total: len(m.Files)}
	expected := make(map[string]ManifestEntry, len(m.Files))
	for _, entry := range m.Files {
		local, err := safeRemappedRemotePath(mirrorRoot, entry.Path)
		if err != nil {
			return MirrorDelta{}, fmt.Errorf("manifest path %q: %w", entry.Path, err)
		}
		expected[local] = entry
	}
	local, err := mirrorFiles(mirrorRoot)
	if err != nil {
		return MirrorDelta{}, err
	}
	for localPath, entry := range expected {
		info, ok := local[localPath]
		if !ok || info.Size() != entry.Size ||
			mtimeMicros(info.ModTime().UnixNano()) != mtimeMicros(entry.MtimeNS) {
			delta.Fetch = append(delta.Fetch, entry.Path)
		}
	}
	for localPath := range local {
		if _, ok := expected[localPath]; !ok {
			delta.Deletions = append(delta.Deletions, localPath)
		}
	}
	sort.Strings(delta.Fetch)
	sort.Strings(delta.Deletions)
	return delta, nil
}

// mtimeMicros truncates a Unix-nanosecond mtime to microseconds. Some
// mirror filesystems store coarser timestamps than the remote's (NTFS
// keeps 100ns units), so exact nanosecond equality could mismatch
// forever and defeat the delta.
func mtimeMicros(ns int64) int64 { return ns / 1000 }

func mirrorFiles(mirrorRoot string) (map[string]os.FileInfo, error) {
	files := make(map[string]os.FileInfo)
	err := filepath.WalkDir(mirrorRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		files[path] = info
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk mirror %q: %w", mirrorRoot, err)
	}
	return files, nil
}

// ApplyMirrorDeletions removes mirror files that no longer exist on
// the remote. Every path must lie inside the mirror root; deletion is
// the only destructive mirror operation, so it fails closed on any
// path that escapes. DB sessions are never deleted (the engine runs
// in Ephemeral mode); this only trims the transfer cache.
func ApplyMirrorDeletions(mirrorRoot string, deletions []string) error {
	for _, path := range deletions {
		if path == mirrorRoot || !within(mirrorRoot, path) {
			return fmt.Errorf("mirror deletion %q escapes mirror root %q", path, mirrorRoot)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete mirror file %q: %w", path, err)
		}
	}
	return nil
}

// MirrorLockHandle holds the exclusive per-mirror flock.
type MirrorLockHandle struct {
	lock *flock.Flock
}

// AcquireMirrorLock takes an exclusive lock serializing mirror
// mutation AND import across processes. The lock file lives NEXT TO
// the mirror root (<mirror>.lock), never inside it: an in-mirror lock
// file would be absent from every manifest and deleted while held,
// silently ending mutual exclusion. Callers hold the lock through
// import because extraction truncates files in place and the engine
// reads the mirror during SyncAll.
func AcquireMirrorLock(
	ctx context.Context, mirrorRoot string,
) (*MirrorLockHandle, error) {
	if err := os.MkdirAll(filepath.Dir(mirrorRoot), 0o700); err != nil {
		return nil, fmt.Errorf("create mirror parent dir: %w", err)
	}
	lock := flock.New(mirrorRoot + ".lock")
	locked, err := lock.TryLockContext(ctx, 250*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquire mirror lock %s: %w", lock.Path(), err)
	}
	if !locked {
		return nil, fmt.Errorf("mirror lock %s is held by another sync", lock.Path())
	}
	return &MirrorLockHandle{lock: lock}, nil
}

// Close releases the mirror lock.
func (h *MirrorLockHandle) Close() error {
	if h == nil || h.lock == nil {
		return nil
	}
	if err := h.lock.Unlock(); err != nil {
		return fmt.Errorf("release mirror lock %s: %w", h.lock.Path(), err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run 'TestMirror|TestApplyMirror|TestAcquireMirror' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/mirror.go internal/remotesync/mirror_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(remotesync): add persistent mirror diff, deletions, and lock"
```

______________________________________________________________________

### Task 6: Server endpoints — manifest route, delta archive mode, gzip

**Files:**

- Modify: `internal/remotesync/types.go` (add `ArchiveRequest`)
- Modify: `internal/server/huma_routes_remote_sync.go`
- Test: `internal/server/huma_routes_remote_sync_internal_test.go`

**Interfaces:**

- Consumes: `BuildManifest` (Task 2), `SelectAllowedFiles` (Task 3),
  `WriteArchiveFiles` (Task 4), existing `SelectAllowedTargets`,
  `ResolveTargets`, `streamErrorAwareResponseWriter`.
- Produces (used by Task 7):
    - `POST /api/v1/remote-sync/manifest`: body `TargetSet`, response gzip-encoded
      JSON `Manifest` with `Content-Encoding: gzip`.
    - `POST /api/v1/remote-sync/archive` accepts `ArchiveRequest`; with `files`
      set it streams only those files; response is gzipped when the request
      advertises `Accept-Encoding: gzip`.

```go
// internal/remotesync/types.go
type ArchiveRequest struct {
	TargetSet
	Files []string `json:"files,omitempty"`
}
```

- [ ] **Step 1: Write the failing tests** (append to
  `internal/server/huma_routes_remote_sync_internal_test.go`)

```go
func TestRemoteSyncManifestListsFiles(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs": map[string][]string{"claude": {filepath.Dir(sessionPath)}},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/manifest",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	var manifest remotesync.Manifest
	require.NoError(t, json.NewDecoder(gz).Decode(&manifest))
	require.Len(t, manifest.Files, 1)
	assert.Equal(t, sessionPath, manifest.Files[0].Path)
	assert.Equal(t, int64(3), manifest.Files[0].Size)
	info, err := os.Stat(sessionPath)
	require.NoError(t, err)
	assert.Equal(t, info.ModTime().UnixNano(), manifest.Files[0].MtimeNS)
}

func TestRemoteSyncManifestRejectsUnresolvedPath(t *testing.T) {
	_, handler, _ := newRemoteSyncServer(t)
	body := bytes.NewBufferString(`{"dirs":{"claude":["/etc"]}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/manifest", body)
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoteSyncArchiveDeltaStreamsOnlyRequestedFiles(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	other := filepath.Join(filepath.Dir(sessionPath), "other.jsonl")
	require.NoError(t, os.WriteFile(other, []byte("{}\n"), 0o644))
	payload, err := json.Marshal(map[string]any{
		"dirs":  map[string][]string{"claude": {filepath.Dir(sessionPath)}},
		"files": []string{other},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	names := []string{}
	tr := tar.NewReader(bytes.NewReader(w.Body.Bytes()))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, pathBaseSlash(hdr.Name))
	}
	assert.Equal(t, []string{"other.jsonl"}, names)
}

func TestRemoteSyncArchiveDeltaRejectsFileOutsideAllowedDirs(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs":  map[string][]string{"claude": {filepath.Dir(sessionPath)}},
		"files": []string{"/etc/passwd"},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRemoteSyncArchiveGzipsWhenAdvertised(t *testing.T) {
	_, handler, sessionPath := newRemoteSyncServer(t)
	payload, err := json.Marshal(map[string]any{
		"dirs": map[string][]string{"claude": {filepath.Dir(sessionPath)}},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/remote-sync/archive",
		bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer remote-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "gzip", w.Header().Get("Content-Encoding"))
	gz, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	require.NoError(t, err)
	assert.NotEmpty(t, hdr.Name)
}
```

Add missing imports to the test file: `compress/gzip`,
`go.kenn.io/agentsview/internal/remotesync`.

- [ ] **Step 2: Run tests to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run 'TestRemoteSync(Manifest|Archive)' -v`
Expected: FAIL — manifest route 404s, `files` field ignored (delta test sees
both files in the tar), no gzip encoding.

- [ ] **Step 3: Add `ArchiveRequest`** (append to
  `internal/remotesync/types.go`)

```go
// ArchiveRequest is the archive endpoint's request body. Files, when
// present, selects delta mode: only the named files are streamed
// (validated by SelectAllowedFiles). Old servers ignore the unknown
// field and return the full tree, which is why clients only send
// Files after a successful manifest probe.
type ArchiveRequest struct {
	TargetSet
	Files []string `json:"files,omitempty"`
}
```

- [ ] **Step 4: Rewrite the handlers**
  (`internal/server/huma_routes_remote_sync.go`)

Replace `registerRemoteSyncRoutes` and `remoteSyncArchiveHTTP`, and add
`remoteSyncManifestHTTP`:

```go
func (s *Server) registerRemoteSyncRoutes() {
	group := newRouteGroup(s.api, "/api/v1/remote-sync", "RemoteSync")
	get(s, group, "/targets", "Resolve remote sync targets", s.humaRemoteSyncTargets)
	s.mux.HandleFunc("/api/v1/remote-sync/archive", s.remoteSyncArchiveHTTP)
	s.mux.HandleFunc("/api/v1/remote-sync/manifest", s.remoteSyncManifestHTTP)
}
```

```go
func (s *Server) remoteSyncManifestHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.db.(*db.DB); !ok {
		http.Error(w, "not available in remote mode", http.StatusNotImplemented)
		return
	}
	var req remotesync.TargetSet
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid manifest request", http.StatusBadRequest)
		return
	}
	allowed := remotesync.ResolveTargets(s.cfg)
	manifestTargets, ok := remotesync.SelectAllowedTargets(allowed, req)
	if !ok {
		http.Error(w, "remote sync target is not allowed", http.StatusForbidden)
		return
	}
	manifest, err := remotesync.BuildManifest(manifestTargets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	if err := json.NewEncoder(gz).Encode(manifest); err != nil {
		log.Printf("remote sync manifest stream failed: %v", err)
		_ = gz.Close()
		return
	}
	if err := gz.Close(); err != nil {
		log.Printf("remote sync manifest stream failed: %v", err)
	}
}
```

```go
func (s *Server) remoteSyncArchiveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.db.(*db.DB); !ok {
		http.Error(w, "not available in remote mode", http.StatusNotImplemented)
		return
	}
	var req remotesync.ArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid archive request", http.StatusBadRequest)
		return
	}
	allowed := remotesync.ResolveTargets(s.cfg)
	archiveTargets, ok := remotesync.SelectAllowedTargets(allowed, req.TargetSet)
	if !ok {
		http.Error(w, "remote sync target is not allowed", http.StatusForbidden)
		return
	}
	var files []string
	if len(req.Files) > 0 {
		files, ok = remotesync.SelectAllowedFiles(allowed, req.Files)
		if !ok {
			http.Error(w, "remote sync file is not allowed", http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "application/x-tar")
	archiveWriter := &streamErrorAwareResponseWriter{ResponseWriter: w}
	out := io.Writer(archiveWriter)
	var gz *gzip.Writer
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz = gzip.NewWriter(archiveWriter)
		out = gz
	}
	var err error
	if len(files) > 0 {
		err = remotesync.WriteArchiveFiles(out, files)
	} else {
		err = remotesync.WriteArchive(out, archiveTargets)
	}
	if err == nil && gz != nil {
		err = gz.Close()
	}
	if err != nil {
		// Do NOT close gz on error: Close flushes a gzip header and
		// trailer to the response, which would mark the response as
		// written and turn a clean failure into a 200 with a valid
		// empty gzip stream.
		if archiveWriter.wrote {
			log.Printf("remote sync archive stream failed: %v", err)
			return
		}
		// Nothing streamed yet: drop the gzip claim so the error body
		// is readable, then fail the request.
		w.Header().Del("Content-Encoding")
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
```

Add `compress/gzip`, `io`, `strings` to the file's imports.

- [ ] **Step 5: Run tests to verify they pass**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/server/ -run TestRemoteSync -v`
Expected: PASS, including the pre-existing archive tests (old request shape
still decodes — `ArchiveRequest` embeds `TargetSet`).

- [ ] **Step 6: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/types.go internal/server/huma_routes_remote_sync.go \
  internal/server/huma_routes_remote_sync_internal_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(server): add remote-sync manifest route, delta archive mode, gzip"
```

______________________________________________________________________

### Task 7: Client mirror flow in HTTPSync

**Files:**

- Modify: `internal/remotesync/http.go`
- Test: `internal/remotesync/http_test.go`

**Interfaces:**

- Consumes: `MirrorDir`, `MirrorDiff`, `ApplyMirrorDeletions`,
  `AcquireMirrorLock` (Task 5), `ArchiveRequest` (Task 6), `Manifest` (Task
  2), existing `Importer`, `ExtractTarStream`, `progressReader`,
  `httpStatusError`.
- Produces: `HTTPSync` gains a `DataDir string` field. Empty `DataDir` preserves
  today's temp-dir flow exactly (existing tests keep passing). Task 8 wires
  `DataDir` at both call sites.

Behavior (spec "Client flow"):

1. `DataDir` empty → legacy flow.
1. Manifest probe; 404/405/501 → legacy flow (old daemon).
1. Acquire mirror lock; held through import.
1. `MirrorDiff`; apply deletions FIRST (type-changed paths must not block
   extraction); then bootstrap heuristic `len(Fetch)*2 >= Total` → full
   archive (no `files` field); otherwise delta with `files`.
1. Delta request rejected with any `StatusError` → retry once as full.
1. Extract stream directly into the mirror; import over the mirror root.

- [ ] **Step 1: Write the failing tests** (append to
  `internal/remotesync/http_test.go`)

```go
// mirrorTestRemote is a fake remote daemon backed by a real directory
// tree, serving targets/manifest/archive with the same package
// functions the real server uses.
type mirrorTestRemote struct {
	dir             string // remote-side agent dir (absolute)
	archiveRequests []ArchiveRequest
	manifestStatus  int // 0 = serve manifest; else respond with this status
	rejectDelta     bool
	ts              *httptest.Server
}

func newMirrorTestRemote(t *testing.T) *mirrorTestRemote {
	t.Helper()
	remote := &mirrorTestRemote{
		dir: filepath.Join(t.TempDir(), "claude-projects"),
	}
	require.NoError(t, os.MkdirAll(remote.dir, 0o755))
	targets := TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {remote.dir}},
	}
	remote.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(targets))
		case "/api/v1/remote-sync/manifest":
			if remote.manifestStatus != 0 {
				http.Error(w, "no manifest here", remote.manifestStatus)
				return
			}
			manifest, err := BuildManifest(targets)
			require.NoError(t, err)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			require.NoError(t, json.NewEncoder(gz).Encode(manifest))
			require.NoError(t, gz.Close())
		case "/api/v1/remote-sync/archive":
			var req ArchiveRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			remote.archiveRequests = append(remote.archiveRequests, req)
			if remote.rejectDelta && len(req.Files) > 0 {
				http.Error(w, "delta not allowed", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/x-tar")
			if len(req.Files) > 0 {
				require.NoError(t, WriteArchiveFiles(w, req.Files))
			} else {
				require.NoError(t, WriteArchive(w, targets))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.ts.Close)
	return remote
}

// writeSession writes a session file with one user message per text.
// Message timestamps are deterministic, so writing the same file again
// with the previous texts plus new ones is a byte-identical prefix
// append — the realistic mutation for JSONL session files, and one the
// engine's incremental parse handles. (In-place rewrites that grow are
// misread as appends for remote paths — pre-existing engine gap, out
// of scope here.)
func (r *mirrorTestRemote) writeSession(
	t *testing.T, name string, mtime time.Time, userTexts ...string,
) string {
	t.Helper()
	dir := filepath.Join(r.dir, "test-project")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	builder := testjsonl.NewSessionBuilder()
	for i, text := range userTexts {
		ts := time.Date(2024, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339)
		builder = builder.AddClaudeUser(ts, text)
	}
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(builder.String()), 0o644))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	return path
}

func newMirrorSync(t *testing.T, remote *mirrorTestRemote, dataDir string) (*db.DB, HTTPSync) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database, HTTPSync{
		Host:    "devbox",
		URL:     remote.ts.URL,
		Token:   "remote-token",
		DataDir: dataDir,
		DB:      database,
	}
}

func TestHTTPSyncMirrorSecondSyncTransfersOnlyDelta(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	staleRemote := remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "c.jsonl", base, "session c")
	remote.writeSession(t, "d.jsonl", base, "session d")
	remote.writeSession(t, "e.jsonl", base, "session e")
	dataDir := t.TempDir()
	database, hs := newMirrorSync(t, remote, dataDir)

	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 5, stats.SessionsSynced)
	require.Len(t, remote.archiveRequests, 1)
	assert.Empty(t, remote.archiveRequests[0].Files, "bootstrap uses the full archive")

	// Append to one, add one, delete one on the remote. The fetch set
	// (2 of the 5 files now in the manifest) stays under the bootstrap
	// heuristic's half-corpus threshold, so this sync must go delta.
	changed := remote.writeSession(t, "a.jsonl", base.Add(5*time.Second),
		"session a", "session a continued")
	added := remote.writeSession(t, "f.jsonl", base.Add(6*time.Second), "session f")
	require.NoError(t, os.Remove(staleRemote))

	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	require.Len(t, remote.archiveRequests, 2)
	assert.ElementsMatch(t, []string{changed, added}, remote.archiveRequests[1].Files)
	assert.Equal(t, 2, stats.SessionsSynced)

	// The deleted remote file is gone from the mirror, but its
	// session survives in the DB (archive semantics).
	mirrorRoot := MirrorDir(dataDir, "devbox")
	staleLocal, err := safeRemappedRemotePath(mirrorRoot, staleRemote)
	require.NoError(t, err)
	assert.NoFileExists(t, staleLocal)
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, page.Sessions, 6)

	// Third sync with no remote changes: no archive request at all.
	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Len(t, remote.archiveRequests, 2)
	assert.Equal(t, 0, stats.SessionsSynced)
}

func TestHTTPSyncFallsBackToLegacyWhenManifestMissing(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.manifestStatus = http.StatusNotFound
	remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "legacy fallback")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	require.Len(t, remote.archiveRequests, 1)
	assert.Empty(t, remote.archiveRequests[0].Files)
	assert.NoDirExists(t, MirrorDir(dataDir, "devbox"))
}

func TestHTTPSyncFallsBackToFullWhenDeltaRejected(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "c.jsonl", base, "session c")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	_, err := hs.Run(context.Background())
	require.NoError(t, err)

	remote.rejectDelta = true
	remote.writeSession(t, "a.jsonl", base.Add(5*time.Second),
		"session a", "session a continued")

	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	// Requests: bootstrap full, rejected delta, retried full.
	require.Len(t, remote.archiveRequests, 3)
	assert.NotEmpty(t, remote.archiveRequests[1].Files)
	assert.Empty(t, remote.archiveRequests[2].Files)
}
```

Add missing imports to `http_test.go`: `compress/gzip`, `encoding/json`, `os`,
`time`, `go.kenn.io/agentsview/internal/parser`.

- [ ] **Step 2: Run tests to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestHTTPSyncMirror -v`
Expected: FAIL — `DataDir` field undefined.

- [ ] **Step 3: Implement the client flow** (`internal/remotesync/http.go`)

Add the field:

```go
type HTTPSync struct {
	Host                    string
	URL                     string
	Token                   string
	Full                    bool
	DataDir                 string
	DB                      *db.DB
	BlockedResultCategories []string
	Progress                syncpkg.ProgressFunc
	Client                  *http.Client
}
```

Replace `Run` with a dispatcher and move the current post-targets body into
`runLegacy`:

```go
func (hs HTTPSync) Run(ctx context.Context) (SyncStats, error) {
	client := hs.Client
	if client == nil {
		client = http.DefaultClient
	}
	hs.report(syncpkg.Progress{
		Detail: fmt.Sprintf("Resolving agent directories on %s", hs.Host),
	})
	targets, err := hs.fetchTargets(ctx, client)
	if err != nil {
		return SyncStats{}, err
	}
	if err := validateTargetSetPaths(targets); err != nil {
		return SyncStats{}, err
	}
	if hs.DataDir != "" {
		manifest, supported, err := hs.fetchManifest(ctx, client, targets)
		if err != nil {
			return SyncStats{}, err
		}
		if supported {
			return hs.runMirror(ctx, client, targets, manifest)
		}
	}
	return hs.runLegacy(ctx, client, targets)
}

// runLegacy is the pre-manifest flow: download the full tree into a
// throwaway temp dir and import it. It remains the path for old
// daemons (no manifest endpoint) and for callers without a DataDir.
func (hs HTTPSync) runLegacy(
	ctx context.Context, client *http.Client, targets TargetSet,
) (SyncStats, error) {
	tmpDir, err := hs.downloadAndExtract(ctx, client, targets)
	if err != nil {
		return SyncStats{}, err
	}
	defer os.RemoveAll(tmpDir)
	return hs.importRoot(ctx, targets, tmpDir)
}

func (hs HTTPSync) importRoot(
	ctx context.Context, targets TargetSet, root string,
) (SyncStats, error) {
	stats, err := Importer{
		Host:                    hs.Host,
		Full:                    hs.Full,
		DB:                      hs.DB,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress,
	}.ImportExtracted(ctx, targets, root)
	if err != nil {
		return SyncStats{}, err
	}
	hs.report(syncpkg.Progress{
		Detail: fmt.Sprintf(
			"Synced %d sessions from %s (%d unchanged)",
			stats.SessionsSynced, hs.Host, stats.Skipped,
		),
	})
	return stats, nil
}
```

Add the mirror flow:

```go
// runMirror syncs incrementally: diff the manifest against the
// persistent mirror, download only changed files, and import over the
// complete mirror tree so parser sibling reads keep working. The
// mirror lock is held through import because extraction truncates
// files in place and the engine reads the mirror during SyncAll.
func (hs HTTPSync) runMirror(
	ctx context.Context,
	client *http.Client,
	targets TargetSet,
	manifest Manifest,
) (SyncStats, error) {
	mirrorRoot := MirrorDir(hs.DataDir, hs.Host)
	lock, err := AcquireMirrorLock(ctx, mirrorRoot)
	if err != nil {
		return SyncStats{}, err
	}
	defer func() { _ = lock.Close() }()
	delta, err := MirrorDiff(mirrorRoot, manifest)
	if err != nil {
		return SyncStats{}, err
	}
	// Deletions run BEFORE extraction so a remote path that changed
	// type does not wedge the mirror: a stale file would block
	// creating a directory of the same name and vice versa.
	// ApplyMirrorDeletions prunes emptied directories for the same
	// reason.
	if err := ApplyMirrorDeletions(mirrorRoot, delta.Deletions); err != nil {
		return SyncStats{}, err
	}
	if len(delta.Fetch) > 0 {
		// Bootstrap heuristic: past half the corpus a full archive is
		// cheaper than uploading a huge file list, and it doubles as
		// the empty-mirror bootstrap (fetch == total).
		full := len(delta.Fetch)*2 >= delta.Total
		err := hs.downloadIntoMirror(ctx, client, targets, delta.Fetch, full, mirrorRoot)
		var statusErr *StatusError
		if err != nil && !full && errors.As(err, &statusErr) {
			// Mid-rollout oddity: manifest worked but the delta
			// request was refused. Retry once as a full archive.
			err = hs.downloadIntoMirror(ctx, client, targets, delta.Fetch, true, mirrorRoot)
		}
		if err != nil {
			return SyncStats{}, err
		}
	}
	return hs.importRoot(ctx, targets, mirrorRoot)
}

func (hs HTTPSync) fetchManifest(
	ctx context.Context, client *http.Client, targets TargetSet,
) (Manifest, bool, error) {
	body, err := json.Marshal(targets)
	if err != nil {
		return Manifest{}, false, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, hs.endpoint("/api/v1/remote-sync/manifest"),
		bytes.NewReader(body),
	)
	if err != nil {
		return Manifest{}, false, err
	}
	hs.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		return Manifest{}, false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		// Old daemon without the manifest endpoint; also gates delta
		// archive usage (an old server would ignore the files field
		// and return the full corpus).
		return Manifest{}, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Manifest{}, false, httpStatusError(resp)
	}
	reader := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return Manifest{}, false, fmt.Errorf("decode manifest gzip: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	var manifest Manifest
	if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
		return Manifest{}, false, fmt.Errorf("decode remote manifest: %w", err)
	}
	return manifest, true, nil
}

func (hs HTTPSync) downloadIntoMirror(
	ctx context.Context,
	client *http.Client,
	targets TargetSet,
	fetch []string,
	full bool,
	mirrorRoot string,
) error {
	request := ArchiveRequest{TargetSet: targets}
	label := fmt.Sprintf("Downloading %d changed files from %s", len(fetch), hs.Host)
	if full {
		label = fmt.Sprintf("Downloading session archive from %s", hs.Host)
	} else {
		request.Files = fetch
	}
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, hs.endpoint("/api/v1/remote-sync/archive"),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	hs.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpStatusError(resp)
	}
	hs.report(syncpkg.Progress{
		Detail:     label,
		BytesTotal: positiveContentLength(resp.ContentLength),
	})
	// Progress counts compressed wire bytes so totals stay meaningful.
	progress := &progressReader{
		r:     resp.Body,
		total: positiveContentLength(resp.ContentLength),
		report: func(done, total int64) {
			hs.report(syncpkg.Progress{
				Detail:     label,
				BytesDone:  done,
				BytesTotal: total,
			})
		},
	}
	stream := io.Reader(progress)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(progress)
		if err != nil {
			return fmt.Errorf("decode archive gzip: %w", err)
		}
		defer gz.Close()
		stream = gz
	}
	if err := os.MkdirAll(mirrorRoot, 0o755); err != nil {
		return fmt.Errorf("create mirror dir: %w", err)
	}
	if _, err := ExtractTarStream(ctx, stream, mirrorRoot); err != nil {
		return fmt.Errorf("extract archive into mirror: %w", err)
	}
	return nil
}
```

Then simplify the tail of the old `Run` body: `downloadAndExtract` and the old
import block are now only referenced from `runLegacy`/`importRoot` — delete the
duplicated import/report code from `Run`. Add `compress/gzip` and `errors` to
the imports.

- [ ] **Step 4: Run the package tests**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -v` Expected: PASS
— new mirror tests AND all pre-existing HTTPSync tests (constructed without
`DataDir`, exercising the legacy flow).

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/http.go internal/remotesync/http_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(remotesync): sync incrementally through a persistent mirror"
```

______________________________________________________________________

### Task 8: Wire DataDir at both call sites

**Files:**

- Modify: `cmd/agentsview/sync.go` (`runHTTPRemoteSync`, ~line 363)
- Modify: `internal/server/huma_routes_sync.go` (`runHTTPRemoteSync`, ~line 71)

**Interfaces:**

- Consumes: `HTTPSync.DataDir` (Task 7); both call sites already receive a
  `config.Config` with `DataDir` set.

- Produces: mirror-based sync active for both CLI (`agentsview sync --remote`)
  and daemon-triggered remote syncs.

- [ ] **Step 1: Add the field at both call sites**

In `cmd/agentsview/sync.go`:

```go
	return remotesync.HTTPSync{
		Host:                    rh.Host,
		URL:                     rh.URL,
		Token:                   token,
		Full:                    full,
		DataDir:                 appCfg.DataDir,
		DB:                      database,
		BlockedResultCategories: appCfg.ResultContentBlockedCategories,
	}.Run(ctx)
```

In `internal/server/huma_routes_sync.go`:

```go
	return remotesync.HTTPSync{
		Host:                    rh.Host,
		URL:                     rh.URL,
		Token:                   token,
		Full:                    full,
		DataDir:                 cfg.DataDir,
		DB:                      local,
		BlockedResultCategories: cfg.ResultContentBlockedCategories,
		Progress:                progress,
	}.Run(ctx)
```

- [ ] **Step 2: Run the affected package tests**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ ./internal/server/ -count=1`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
go fmt ./... && go vet ./...
git add cmd/agentsview/sync.go internal/server/huma_routes_sync.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "feat(sync): enable mirror-based incremental HTTP remote sync"
```

______________________________________________________________________

### Task 9: End-to-end equivalence test

Prove the incremental path converges to the same DB state as a from-scratch full
sync after a mutation sequence.

**Files:**

- Test: `internal/remotesync/http_test.go` (reuses Task 7's `mirrorTestRemote`
  helpers)

- [ ] **Step 1: Write the test**

```go
func TestHTTPSyncIncrementalMatchesFreshFullSync(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 555666777, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")

	incDB, incSync := newMirrorSync(t, remote, t.TempDir())
	_, err := incSync.Run(context.Background())
	require.NoError(t, err)

	remote.writeSession(t, "a.jsonl", base.Add(2*time.Second),
		"session a", "session a continued")
	remote.writeSession(t, "c.jsonl", base.Add(3*time.Second), "session c")
	_, err = incSync.Run(context.Background())
	require.NoError(t, err)

	freshDB, freshSync := newMirrorSync(t, remote, t.TempDir())
	_, err = freshSync.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, sessionSummaries(t, freshDB), sessionSummaries(t, incDB))
}

// sessionSummaries returns a sorted, comparable projection of every
// session: identity plus message count, which changes when a session
// file's new content is parsed.
func sessionSummaries(t *testing.T, database *db.DB) []string {
	t.Helper()
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	out := make([]string, 0, len(page.Sessions))
	for _, s := range page.Sessions {
		count, ok := database.GetSessionMessageCount(s.ID)
		require.True(t, ok, "message count for %s", s.ID)
		hash, ok := database.GetSessionFileHash(s.ID)
		require.True(t, ok, "file hash for %s", s.ID)
		out = append(out, fmt.Sprintf("%s|%s|%d|%s", s.ID, s.Machine, count, hash))
	}
	sort.Strings(out)
	return out
}
```

Add `fmt` and `sort` to the test imports. Signature verified:
`GetSessionMessageCount(id string) (count int, ok bool)` at
`internal/db/sessions.go:1619`.

- [ ] **Step 2: Run the test**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestHTTPSyncIncrementalMatchesFreshFullSync -v`
Expected: PASS. If the summaries differ, the incremental path is dropping or
duplicating state — debug before proceeding, do not weaken the assertion.

- [ ] **Step 3: Run the full affected test suite**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ ./internal/server/ ./internal/sync/ ./cmd/agentsview/ -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
go fmt ./... && go vet ./...
git add internal/remotesync/http_test.go
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "test(remotesync): prove incremental sync matches fresh full sync"
```

______________________________________________________________________

### Task 10: Document incremental sync in remote-access docs

**Files:**

- Modify: `docs/remote-access.md`

- [ ] **Step 1: Add a short section**

Find the HTTP remote sync section in `docs/remote-access.md` (search for "HTTP
remote sync") and add after it:

```markdown
## Incremental sync

HTTP remote sync is incremental. The first sync downloads the full session
archive and stores a byte-for-byte mirror under
`<data_dir>/remote-mirrors/<host>/`; subsequent syncs fetch a file manifest,
diff it against the mirror, and download only files that changed on the
remote. Archives are gzip-compressed in transit.

The mirror is a disposable transfer cache: deleting it is always safe and
just makes the next sync download everything again. Sessions already
imported into the database are never removed, even when their source files
disappear from the remote.

When the remote daemon predates the manifest endpoint, sync automatically
falls back to the full-archive download, so mixed versions keep working.
```

- [ ] **Step 2: Format and commit**

```bash
mdformat --wrap 80 docs/remote-access.md
git add docs/remote-access.md
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" git commit -m "docs: describe incremental HTTP remote sync and mirror cache"
```

______________________________________________________________________

### Task 11: Whole-branch verification

Per the SDD finish gate: per-task test runs miss NilAway and cross-package
fallout.

- [ ] **Step 1: Run lint and the full test suite**

```bash
GOLANGCI_LINT_CACHE="$PWD/.golangci-cache" make lint
make test
```

Expected: both clean. Fix anything that surfaces (new code only — do not touch
unrelated findings; flag them instead), then commit fixes.

- [ ] **Step 2: Verify the feature end-to-end manually (optional but
  recommended)**

If a second machine or a second local daemon is available: point a client at it,
sync twice, and confirm the second sync's log shows a delta download
("Downloading N changed files from ...") instead of the full archive.
