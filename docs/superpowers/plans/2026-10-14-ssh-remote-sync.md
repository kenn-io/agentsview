# SSH Remote Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Sync a remote host over plain SSH, opted into per `[[remote_hosts]]`
entry with `transport = "ssh"`. The remote side is a short-lived,
version-matched agentsview binary that agentsview places on the remote itself.

**Architecture:** The HTTP remote sync pipeline (mirror, manifest diff, delta
archives, journal, import) is made transport-neutral behind a `Source`
interface. A new `sshremote` package supplies the remote command
(`agentsview remote-sync probe|serve`), the ssh client wrapper with offline
classification, binary provisioning, and an SSH `Source`. A small `remotesource`
package picks the HTTP or SSH source for a configured host.

**Tech Stack:** Go 1.27.2, system OpenSSH client, `archive/tar`,
`github.com/klauspost/compress/zstd` (already a direct dependency),
`compress/gzip` only for the binary upload tarball, testify.

**Spec:** `docs/superpowers/specs/2026-10-14-ssh-remote-sync-design.md`. Read it
before starting any task.

## Global Constraints

- SSH runs only for entries with `transport = "ssh"`. An omitted transport means
  HTTP. `sync --host NAME` requires a configured entry.
- The remote process never opens or creates a database, never creates the data
  directory, never starts a daemon, watchers, telemetry, or update checks, and
  loads config only with `config.LoadReadOnly()`.
- ssh invocation is exactly
  `ssh -T -o BatchMode=yes -o ConnectTimeout=10 -o Compression=no -o ServerAliveInterval=15 -o ServerAliveCountMax=4 -- <destination> <command>`
  with `LC_ALL=C` added to the environment.
- Every remote command is `sh -c '<script>'`. Scripts are single-line, and
  contain no `'`, no newline, and no `!` (csh and tcsh login shells treat
  newlines and `!` specially even inside single quotes).
- Interpolated script values (binary identity, operation name) must match
  `^[0-9A-Za-z._+-]+$`.
- Remote binaries live in `$HOME/.cache/agentsview/bin/<id>/agentsview`. Release
  identity is `v<ver>` (version matching `^v?[0-9]+\.[0-9]+\.[0-9]+$`). Every
  other build uses `<ver>-<first 16 hex of SHA-256 of executable>`.
- Retention removes an identity only when its `.last-used` is older than 30
  days; it removes `.tmp-*` older than 1 day.
- Release download:
  `https://github.com/kenn-io/agentsview/releases/download/v<ver>/agentsview_<ver>_<os>_<arch>.tar.gz`
  verified against `SHA256SUMS` in the same release. The tarball holds one
  `agentsview` file at its root.
- Supported remote platforms: `linux` and `darwin` on `amd64` and `arm64`.
- Offline means exit 255 plus a whole stderr line matching the spec's OpenSSH
  forms. Everything else is a reported failure.
- Responses from `serve` are zstd. ssh compression stays off. HTTP keeps gzip.
- Follow `docs/agents/testing.md`: table-driven tests, testify (`require` for
  stop-on-failure, `assert` otherwise), `t.TempDir()`, no sub-second constant
  budgets outside `testing/synctest`.
- After Go changes run `go fmt ./...` and `go vet ./...`. Build tag `fts5` and
  `CGO_ENABLED=1` are needed for tests:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/...`.
- Commit at the end of every task with a conventional commit message. Never
  amend. Do not push.

## File Structure

| Path                                                                       | Responsibility                                                                                           |
| -------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `internal/remotesync/source.go` (new)                                      | `Source` interface, `ArchiveStream`, `ArchiveEncoding`                                                   |
| `internal/remotesync/http_source.go` (new)                                 | `HTTPSource`: the three HTTP fetches moved out of `http.go`                                              |
| `internal/remotesync/zstd.go` (new)                                        | Bounded zstd reader and writer shared by both sides                                                      |
| `internal/remotesync/serve.go` (new)                                       | Shared request handling: `ServeManifest`, `PlanArchive`, typed errors                                    |
| `internal/remotesync/http.go`, `http_prepare.go`, `http_transfer.go`       | Become transport-neutral `RemoteSync` code                                                               |
| `internal/remotesync/archive.go`, `sqlite_snapshot.go`                     | Cancellation reaches archive and snapshot work                                                           |
| `internal/remotesync/failure.go`                                           | Hooks that let other transports classify their errors                                                    |
| `internal/ctxio/writer.go` (new)                                           | Context-checking `io.Writer`                                                                             |
| `internal/config/config.go`                                                | `RemoteTransportSSH`, `RemoteHost.Destination`, validation                                               |
| `internal/sshremote/protocol.go` (new)                                     | Ops, request and error wire types, `ProbeResult`                                                         |
| `internal/sshremote/serve.go` (new)                                        | `Serve`: one operation from a request to a zstd response                                                 |
| `internal/sshremote/process.go`, `stdout_unix.go`, `stdout_other.go` (new) | Signals, pollable stdout, exit codes for the remote command                                              |
| `internal/sshremote/exec.go` (new)                                         | `Runner`: run ssh, capture stderr, build `CommandError`                                                  |
| `internal/sshremote/errors.go` (new)                                       | `CommandError`, offline classification, summaries                                                        |
| `internal/sshremote/identity.go` (new)                                     | `LocalBuild`, release and content identities                                                             |
| `internal/sshremote/release.go` (new)                                      | Platform mapping, verified release download and cache                                                    |
| `internal/sshremote/provision.go` (new)                                    | Probe and install scripts, binary selection, self-upload                                                 |
| `internal/sshremote/source.go` (new)                                       | SSH implementation of `remotesync.Source`                                                                |
| `internal/remotesource/remotesource.go` (new)                              | `For(rh, Options)`: pick HTTP or SSH source for a configured host                                        |
| `cmd/agentsview/remote_sync_cmd.go` (new)                                  | Hidden `remote-sync probe` and `remote-sync serve --op` commands                                         |
| `cmd/agentsview/sync.go`, `internal/server/huma_routes_sync.go`            | Dispatch through `remotesource.For`                                                                      |
| `internal/server/huma_routes_remote_sync.go`                               | HTTP routes call the shared handlers                                                                     |
| Docs                                                                       | `docs/remote-access.md`, `docs/configuration.md`, `docs/commands.md`, `SECURITY.md`, `docs/changelog.md` |

______________________________________________________________________

### Task 1: Rename HTTP sync types to transport-neutral names

The pipeline types are named for HTTP but will carry SSH too. Rename them first,
mechanically, so later diffs stay readable. No behavior change.

**Files:**

- Modify: every Go file that references the names below (found by `rg`).

**Interfaces:**

- Produces: `remotesync.RemoteSync`, `remotesync.RemoteSyncLifecycle`,
  `remotesync.PreparedRemote`, `remotesync.PreparedRemoteSyncs`,
  `remotesync.PrepareRemoteSyncs`, `remotesync.PrepareAvailableRemoteSyncs`.
  Unexported: `prepareRemoteSyncs`, `prepareRemoteSyncsWithUnavailable`.

- [ ] **Step 1: Rename identifiers**

```bash
names='\b(HTTPSync|HTTPSyncLifecycle|PreparedHTTP|PreparedHTTPSyncs|PrepareHTTPSyncs|PrepareAvailableHTTPSyncs|prepareHTTPSyncs|prepareHTTPSyncsWithUnavailable)\b'
rg -l --type go "$names" | xargs perl -pi -e '
  s/\bHTTPSyncLifecycle\b/RemoteSyncLifecycle/g;
  s/\bPreparedHTTPSyncs\b/PreparedRemoteSyncs/g;
  s/\bPrepareAvailableHTTPSyncs\b/PrepareAvailableRemoteSyncs/g;
  s/\bPrepareHTTPSyncs\b/PrepareRemoteSyncs/g;
  s/\bprepareHTTPSyncsWithUnavailable\b/prepareRemoteSyncsWithUnavailable/g;
  s/\bprepareHTTPSyncs\b/prepareRemoteSyncs/g;
  s/\bPreparedHTTP\b/PreparedRemote/g;
  s/\bHTTPSync\b/RemoteSync/g;
'
rg -n --type go "$names" || echo "no old names left"
```

Expected: `no old names left`. Test function names such as `TestHTTPSync...`
contain the old name inside a longer word and are left alone on purpose.

- [ ] **Step 2: Rename the files that now hold neutral code**

```bash
git mv internal/remotesync/http_prepare.go internal/remotesync/prepare.go
git mv internal/remotesync/http_transfer.go internal/remotesync/transfer.go
```

`http.go` keeps its name until Task 2 splits the HTTP fetches out.

- [ ] **Step 3: Build and test**

```bash
go build ./... && go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ ./internal/server/ ./cmd/agentsview/
```

Expected: PASS. A failure here means a reference the regex missed; fix it by
hand.

- [ ] **Step 4: Commit**

```bash
go fmt ./...
git add -A internal cmd
git commit -m "refactor(remotesync): name the sync pipeline for any transport"
```

______________________________________________________________________

### Task 2: Introduce `Source` and move HTTP fetches into `HTTPSource`

**Files:**

- Create: `internal/remotesync/source.go`, `internal/remotesync/http_source.go`,
  `internal/remotesync/zstd.go`, `internal/remotesync/source_test.go`
- Modify: `internal/remotesync/http.go` (keep `RemoteSync`, drop HTTP fetches),
  `internal/remotesync/prepare.go:331-410` (`prepare`), `:743` and
  `:1012-1033` (`prepareMirror` calls),
  `internal/remotesync/transfer.go:97-221` (`downloadArchive`,
  `extractWithSelection`)
- Modify callers: `cmd/agentsview/sync.go:527,709`,
  `internal/server/huma_routes_sync.go:100,824`, and every test that builds a
  `RemoteSync` literal with `URL`, `Token`, or `Client`.

**Interfaces:**

- Produces:

```go
type Source interface {
	Targets(ctx context.Context) (TargetSet, error)
	Manifest(ctx context.Context, targets TargetSet) (manifest Manifest, supported bool, err error)
	Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error)
}
type ArchiveEncoding string // ArchiveEncodingNone "", ArchiveEncodingGzip "gzip", ArchiveEncodingZstd "zstd"
type ArchiveStream struct { Body io.ReadCloser; Encoding ArchiveEncoding; Size int64 }
type HTTPSource struct { URL string; Token string; Client *http.Client }
func NewZstdReader(r io.Reader) (*zstd.Decoder, error)
func NewZstdWriter(w io.Writer, level zstd.EncoderLevel) (*zstd.Encoder, error)
// RemoteSync gains `Source Source` and loses URL, Token, Client.
```

- [ ] **Step 1: Write the failing test**

`internal/remotesync/source_test.go`. A fake `Source` serves a zstd archive
built by `WriteArchive`, and `RemoteSync.Run` imports the session. This proves
the pipeline no longer depends on HTTP and that zstd archives extract.

```go
package remotesync

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// dirSource serves one remote directory tree in-process with the same
// package functions the remote side uses.
type dirSource struct {
	targets  TargetSet
	encoding ArchiveEncoding
}

func (s dirSource) Targets(context.Context) (TargetSet, error) { return s.targets, nil }

func (s dirSource) Manifest(ctx context.Context, targets TargetSet) (Manifest, bool, error) {
	manifest, err := BuildManifest(ctx, targets)
	return manifest, err == nil, err
}

func (s dirSource) Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error) {
	var buf bytes.Buffer
	var out io.Writer = &buf
	var zw *zstd.Encoder
	if s.encoding == ArchiveEncodingZstd {
		var err error
		zw, err = NewZstdWriter(&buf, zstd.SpeedDefault)
		if err != nil {
			return nil, err
		}
		out = zw
	}
	var err error
	if request.DeltaFiles != nil {
		err = WriteArchiveFiles(ctx, out, s.targets, request.DeltaFiles)
	} else {
		err = WriteArchive(ctx, out, request.TargetSet)
	}
	if err != nil {
		return nil, err
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			return nil, err
		}
	}
	return &ArchiveStream{Body: io.NopCloser(&buf), Encoding: s.encoding}, nil
}

func TestRemoteSyncImportsThroughNonHTTPSource(t *testing.T) {
	for _, encoding := range []ArchiveEncoding{ArchiveEncodingNone, ArchiveEncodingZstd} {
		t.Run(string(encoding)+"encoding", func(t *testing.T) {
			remoteDir := filepath.Join(t.TempDir(), "claude-projects")
			sessionPath := filepath.Join(remoteDir, "project-a", "session.jsonl")
			require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
			require.NoError(t, os.WriteFile(sessionPath, []byte(
				testjsonl.NewSessionBuilder().
					AddClaudeUser("2024-01-01T00:00:00Z", "through a source").
					String(),
			), 0o644))
			database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })

			stats, err := RemoteSync{
				Host:    "devbox",
				DataDir: t.TempDir(),
				DB:      database,
				Source: dirSource{
					targets: TargetSet{Dirs: map[parser.AgentType][]string{
						parser.AgentClaude: {remoteDir},
					}},
					encoding: encoding,
				},
			}.Run(t.Context())

			require.NoError(t, err)
			assert.Equal(t, 1, stats.SessionsSynced)
		})
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run TestRemoteSyncImportsThroughNonHTTPSource`
Expected: compile failure, `unknown field Source` and undefined `ArchiveStream`.

- [ ] **Step 3: Add `source.go` and `zstd.go`**

`internal/remotesync/source.go`:

```go
package remotesync

import (
	"context"
	"io"
)

// Source fetches the remote half of one sync: the remote's targets, a
// manifest of them, and archives of their files. HTTPSource talks to a remote
// daemon; package sshremote runs agentsview on the remote over SSH.
type Source interface {
	// Targets resolves the files the remote publishes.
	Targets(ctx context.Context) (TargetSet, error)
	// Manifest lists targets with sizes and mtimes. supported is false when
	// the remote cannot build a manifest; the caller then falls back to a full
	// archive.
	Manifest(ctx context.Context, targets TargetSet) (manifest Manifest, supported bool, err error)
	// Archive opens a tar stream for request. Close on the stream reports
	// any failure the remote reported after the body ended.
	Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error)
}

// ArchiveEncoding names the compression of an ArchiveStream body.
type ArchiveEncoding string

const (
	ArchiveEncodingNone ArchiveEncoding = ""
	ArchiveEncodingGzip ArchiveEncoding = "gzip"
	ArchiveEncodingZstd ArchiveEncoding = "zstd"
)

// ArchiveStream is an open archive body returned by a Source.
type ArchiveStream struct {
	Body     io.ReadCloser
	Encoding ArchiveEncoding
	// Size is the encoded body length in bytes, or 0 when unknown.
	Size int64
}
```

`internal/remotesync/zstd.go`:

```go
package remotesync

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

// remoteZstdMaxWindow bounds decoder memory for remote-controlled streams.
// The writer pins the same window so every stream we produce decodes.
const remoteZstdMaxWindow = 8 << 20

// NewZstdReader decodes a remote-sync zstd stream with a bounded window.
func NewZstdReader(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(remoteZstdMaxWindow),
	)
}

// NewZstdWriter encodes a remote-sync zstd stream that NewZstdReader accepts.
func NewZstdWriter(w io.Writer, level zstd.EncoderLevel) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(level),
		zstd.WithWindowSize(remoteZstdMaxWindow),
	)
}
```

- [ ] **Step 4: Move the HTTP fetches into `http_source.go`**

Create `internal/remotesync/http_source.go` with `HTTPSource` and move these
from `http.go`, converting each `hs HTTPSync` method to an `s HTTPSource`
method: `fetchTargets` becomes `Targets`, `fetchManifest` becomes `Manifest`,
and `requestArchive`, `transferHeaders`, `generatedProviderPaths`,
`generatedTargets`, `StatusError`, and `httpStatusError` move unchanged apart
from receivers. Replace each `client` parameter with `s.client()`:

```go
// HTTPSource fetches remote sync data from a remote agentsview daemon.
type HTTPSource struct {
	URL    string
	Token  string
	Client *http.Client
}

func (s HTTPSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	// Archive responses can stream for hours; ctx governs their lifetime.
	return &http.Client{Timeout: 0}
}

// Archive requests an archive and returns its body once the daemon accepts.
func (s HTTPSource) Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error) {
	resp, err := s.requestArchive(ctx, request)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, httpStatusError(resp)
	}
	if err := ValidateProtocolHeader(resp.Header); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	encoding := ArchiveEncodingNone
	if resp.Header.Get("Content-Encoding") == "gzip" {
		encoding = ArchiveEncodingGzip
	}
	return &ArchiveStream{
		Body:     resp.Body,
		Encoding: encoding,
		Size:     positiveContentLength(resp.ContentLength),
	}, nil
}
```

- [ ] **Step 5: Make `RemoteSync` and `downloadArchive` use the source**

In `http.go`, replace the `URL`, `Token`, and `Client` fields of `RemoteSync`
with:

```go
	// Source fetches the remote half of the sync. Required.
	Source Source
```

In `prepare.go` `prepare`, delete the `client` block and start with:

```go
	if hs.Source == nil {
		return nil, errors.New("remote sync source is required")
	}
	hs.reportProgressResolvingTargets()
	targets, err := hs.Source.Targets(ctx)
```

Replace `hs.fetchManifest(ctx, client, dirScoped)` with
`hs.Source.Manifest(ctx, dirScoped)`. Remove the `client *http.Client` parameter
from `downloadIntoMirror`, `downloadAndExtract`, and `prepareMirror` and from
every call. In `downloadIntoMirror` and `downloadAndExtract`, replace the
`requestArchive` and status/header block with:

```go
	stream, err := hs.Source.Archive(ctx, request)
	if err != nil {
		return err
	}
	archive, err := hs.downloadArchive(ctx, stream, downloadLabel, filepath.Dir(mirrorRoot))
```

(`downloadAndExtract` passes `os.TempDir()` as before.)

In `transfer.go`, change `downloadArchive` to take `stream *ArchiveStream`:
close with `stream.Body.Close()` in the deferred join, drop the status check
(now in `HTTPSource.Archive`), read from `stream.Body`, use `stream.Size` as the
progress total, and store `encoding: stream.Encoding` instead of `compressed`.
Rename the `downloadedArchive.compressed bool` field to
`encoding ArchiveEncoding`. In `extractWithSelection` replace the gzip branch
with:

```go
	var stream io.Reader = archive
	switch a.encoding {
	case ArchiveEncodingNone:
	case ArchiveEncodingGzip:
		gz, gzipErr := gzip.NewReader(archive)
		if gzipErr != nil {
			return fmt.Errorf("decode archive gzip: %w", gzipErr)
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		stream = gz
	case ArchiveEncodingZstd:
		zr, zstdErr := NewZstdReader(archive)
		if zstdErr != nil {
			return fmt.Errorf("decode archive zstd: %w", zstdErr)
		}
		defer zr.Close()
		stream = zr
	default:
		return fmt.Errorf("unsupported archive encoding %q", a.encoding)
	}
```

In `prepareRemoteSyncsWithUnavailable`, the legacy sort key used `hs.URL`.
Replace it with:

```go
func legacySortKey(hs RemoteSync) string {
	key := "legacy\x00" + hs.Host
	if source, ok := hs.Source.(HTTPSource); ok {
		key += "\x00" + source.URL
	}
	return key
}
```

- [ ] **Step 6: Update callers**

At `cmd/agentsview/sync.go:527,709` and
`internal/server/huma_routes_sync.go:100,824` replace `URL: …, Token: …,` with
`Source: remotesync.HTTPSource{URL: rh.URL, Token: token},` (use `host.URL`,
`host.Token` where the loop variable is `host`). In tests, apply the same change
to every `RemoteSync{…}` literal; `rg -n "RemoteSync\{" --type go` lists them. A
literal that set `Client: c` becomes
`Source: HTTPSource{URL: …, Token: …, Client: c}`.

- [ ] **Step 7: Run tests**

```bash
go build ./... && go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ ./internal/server/ ./cmd/agentsview/
```

Expected: PASS, including the new test in both encodings.

- [ ] **Step 8: Commit**

```bash
go fmt ./...
git add -A internal cmd
git commit -m "refactor(remotesync): fetch remote data through a Source interface"
```

______________________________________________________________________

### Task 3: Let cancellation reach archive generation

**Files:**

- Create: `internal/ctxio/writer.go`, `internal/ctxio/writer_test.go`
- Modify: `internal/remotesync/archive.go:16-17` (`WriteArchive`), `:380`
  (`WriteArchiveFiles`), `:93-126` (`writeSQLiteStateDBSnapshot`),
  `internal/remotesync/sqlite_snapshot.go:20-90` (`writeSQLiteSnapshot`)
- Test: `internal/remotesync/archive_cancel_test.go` (new)

**Interfaces:**

- Produces: `ctxio.Writer{Context, Writer}`;
  `writeSQLiteSnapshot(ctx context.Context, dstPath, srcPath string) error`
  and `var writeSQLiteSnapshotFile = writeSQLiteSnapshot` with the new
  signature; test hook `var afterSQLiteBackupStep func()`.

- [ ] **Step 1: Write the failing tests**

`internal/ctxio/writer_test.go`:

```go
package ctxio

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterStopsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var buf bytes.Buffer
	w := Writer{Context: ctx, Writer: &buf}

	n, err := w.Write([]byte("before"))
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	cancel()
	n, err = w.Write([]byte("after"))
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, n)
	assert.Equal(t, "before", buf.String())
}
```

`internal/remotesync/archive_cancel_test.go`:

```go
package remotesync

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// cancelAfterWriter cancels its context after the first write so the next
// write observes cancellation, as when a signal arrives mid-transfer.
type cancelAfterWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(p), nil
}

func TestWriteArchiveStopsWhenContextCancelled(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.jsonl", "b.jsonl", "c.jsonl"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name),
			[]byte(strings.Repeat("x", 64<<10)), 0o644))
	}
	ctx, cancel := context.WithCancel(t.Context())
	out := &cancelAfterWriter{cancel: cancel}

	err := WriteArchive(ctx, out, TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {dir}},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, out.writes)
}

func TestSQLiteSnapshotStopsBetweenBackupSteps(t *testing.T) {
	src := filepath.Join(t.TempDir(), "state.db")
	conn, err := sql.Open("sqlite3", src)
	require.NoError(t, err)
	_, err = conn.Exec(`CREATE TABLE blobs (data BLOB)`)
	require.NoError(t, err)
	// Enough pages for several backup steps at the default page size.
	for range 40 {
		_, err = conn.Exec(`INSERT INTO blobs VALUES (randomblob(1 << 20))`)
		require.NoError(t, err)
	}
	require.NoError(t, conn.Close())

	ctx, cancel := context.WithCancel(t.Context())
	steps := 0
	afterSQLiteBackupStep = func() {
		steps++
		cancel()
	}
	t.Cleanup(func() { afterSQLiteBackupStep = nil })
	dst := filepath.Join(t.TempDir(), "snapshot.db")

	err = writeSQLiteSnapshot(ctx, dst, src)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, steps)
	_, statErr := os.Stat(dst)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

```

- [ ] **Step 2: Run them to verify they fail**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/ctxio/ ./internal/remotesync/ -run 'TestWriter|TestWriteArchiveStops|TestSQLiteSnapshotStops'`
Expected: compile failures (`Writer` undefined, `writeSQLiteSnapshot` arity,
`afterSQLiteBackupStep` undefined).

- [ ] **Step 3: Implement**

`internal/ctxio/writer.go`:

```go
package ctxio

import (
	"context"
	"io"
)

// Writer checks cancellation before each underlying write.
type Writer struct {
	Context context.Context
	io.Writer
}

func (w Writer) Write(p []byte) (int, error) {
	if err := w.Context.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}
```

In `archive.go`, make both `WriteArchive` and `WriteArchiveFiles` build their
tar writer on a context-checking writer:

```go
	tw := tar.NewWriter(ctxio.Writer{Context: ctx, Writer: w})
```

In `writeSQLiteStateDBSnapshot`, call
`writeSQLiteSnapshotFile(ctx, snapshotPath, stateDB)` and return a cancellation
error unchanged, before the re-probe:

```go
	if err := writeSQLiteSnapshotFile(ctx, snapshotPath, stateDB); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
```

Apply the same signature change to every other `writeSQLiteSnapshotFile` caller
(`rg -n writeSQLiteSnapshotFile internal/remotesync`).

In `sqlite_snapshot.go`, give `writeSQLiteSnapshot` a `ctx` first parameter, use
it for both `Conn` calls and the final `ExecContext`, and replace the single
`Step(-1)` with a loop:

```go
// sqliteBackupPagesPerStep bounds how long one backup step runs, so a
// cancelled context is observed between steps.
const sqliteBackupPagesPerStep = 1024

// afterSQLiteBackupStep is a test hook called after each completed step.
var afterSQLiteBackupStep func()
```

```go
			backup, err := dstSQLite.Backup("main", srcSQLite, "main")
			if err != nil {
				return fmt.Errorf("start sqlite online backup: %w", err)
			}
			for {
				done, stepErr := backup.Step(sqliteBackupPagesPerStep)
				if stepErr != nil {
					return fmt.Errorf("copy sqlite online backup: %w",
						errors.Join(stepErr, backup.Finish()))
				}
				if done {
					break
				}
				if afterSQLiteBackupStep != nil {
					afterSQLiteBackupStep()
				}
				if err := ctx.Err(); err != nil {
					return errors.Join(err, backup.Finish())
				}
			}
			if err := backup.Finish(); err != nil {
				return fmt.Errorf("finish sqlite online backup: %w", err)
			}
			return nil
```

The existing `complete` flag already removes `dstPath` on any error.

- [ ] **Step 4: Run tests**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/ctxio/ ./internal/remotesync/ ./internal/server/`
Expected: PASS. The server's archive route tests exercise the changed functions
with request contexts.

- [ ] **Step 5: Commit**

```bash
go fmt ./... && go vet ./internal/...
git add internal/ctxio internal/remotesync
git commit -m "fix(remotesync): stop archive and snapshot work when cancelled"
```

______________________________________________________________________

### Task 4: Share remote request handling between HTTP and SSH

**Files:**

- Create: `internal/remotesync/serve.go`, `internal/remotesync/serve_test.go`
- Modify: `internal/server/huma_routes_remote_sync.go:69-197`

**Interfaces:**

- Produces:

```go
var ErrTargetNotAllowed, ErrFileNotAllowed, ErrManifestUnsupported error
func ServeManifest(ctx context.Context, cfg config.Config, requested TargetSet) (Manifest, error)
type ArchivePlan struct { /* unexported */ }
func PlanArchive(cfg config.Config, request ArchiveRequest) (ArchivePlan, error)
func (p ArchivePlan) Write(ctx context.Context, w io.Writer) error
```

- [ ] **Step 1: Write the failing test**

`internal/remotesync/serve_test.go`. `ResolveTargets` reads agent directories
from `config.Config`; build one whose Claude directory is a temp dir. Find the
field that carries agent dirs with
`rg -n "func ResolveTargets" -A40 internal/remotesync/resolve.go` and the
matching test helper in `resolve_test.go` (for example a
`resolveTestConfig`-style helper); reuse it rather than hand-building config.

```go
func TestServeManifestAuthorizesRequestedTargets(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t) // helper from resolve_test.go pattern
	writeServeTestSession(t, claudeDir)

	tests := []struct {
		name      string
		requested TargetSet
		wantErr   error
	}{
		{"allowed dir", TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}}}, nil},
		{"outside allowed roots", TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}}}, ErrTargetNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest, err := ServeManifest(t.Context(), cfg, tt.requested)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Len(t, manifest.Files, 1)
		})
	}
}

func TestPlanArchiveRejectsDeltaOutsideAllowedFiles(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)

	_, err := PlanArchive(cfg, ArchiveRequest{
		TargetSet:  TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}}},
		DeltaFiles: []string{filepath.Join(t.TempDir(), "elsewhere.jsonl")},
	})

	require.ErrorIs(t, err, ErrFileNotAllowed)
}
```

Write `serveTestConfig(t) (config.Config, string)` and
`writeServeTestSession(t, dir)` in the same file. The session file uses
`testjsonl.NewSessionBuilder().AddClaudeUser(...)` as in `source_test.go`.

- [ ] **Step 2: Run it to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ -run 'TestServeManifest|TestPlanArchive'`
Expected: compile failure, `ServeManifest` undefined.

- [ ] **Step 3: Implement `serve.go`**

```go
package remotesync

import (
	"context"
	"errors"
	"fmt"
	"io"

	"go.kenn.io/agentsview/internal/config"
)

var (
	ErrTargetNotAllowed    = errors.New("remote sync target is not allowed")
	ErrFileNotAllowed      = errors.New("remote sync file is not allowed")
	ErrManifestUnsupported = errors.New("manifest unavailable for sanitized file-scoped agents")
)

// ServeManifest builds the manifest a collector asked for, limited to what
// the remote's own target resolution allows.
func ServeManifest(ctx context.Context, cfg config.Config, requested TargetSet) (Manifest, error) {
	allowed, err := ResolveTargets(cfg)
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve targets: %w", err)
	}
	targets, ok := SelectAllowedTargets(allowed, requested)
	if !ok {
		return Manifest{}, ErrTargetNotAllowed
	}
	// Sanitized file-scoped agents (Windsurf) stream a transformed subset of
	// their directory; a manifest of the raw tree would let a delta request
	// fetch unsanitized files. Collectors fall back to a full archive.
	if targets.HasSanitizedFileScopedAgents() {
		return Manifest{}, ErrManifestUnsupported
	}
	return BuildManifest(ctx, targets)
}

// ArchivePlan is an authorized archive request, ready to stream. Planning
// before writing lets transports report refusals before any body bytes.
type ArchivePlan struct {
	allowed TargetSet
	targets TargetSet
	files   []string
	delta   bool
}

// PlanArchive authorizes request against the remote's own resolution. A
// non-nil DeltaFiles selects delta mode even when empty.
func PlanArchive(cfg config.Config, request ArchiveRequest) (ArchivePlan, error) {
	allowed, err := ResolveTargets(cfg)
	if err != nil {
		return ArchivePlan{}, fmt.Errorf("resolve targets: %w", err)
	}
	targets, ok := SelectAllowedTargets(allowed, request.TargetSet)
	if !ok {
		return ArchivePlan{}, ErrTargetNotAllowed
	}
	plan := ArchivePlan{allowed: allowed, targets: targets, delta: request.DeltaFiles != nil}
	if plan.delta {
		plan.files, ok = SelectAllowedFiles(allowed, request.DeltaFiles)
		if !ok {
			return ArchivePlan{}, ErrFileNotAllowed
		}
	}
	return plan, nil
}

// Write streams the planned archive to w.
func (p ArchivePlan) Write(ctx context.Context, w io.Writer) error {
	if p.delta {
		return WriteArchiveFiles(ctx, w, p.allowed, p.files)
	}
	return WriteArchive(ctx, w, p.targets)
}
```

- [ ] **Step 4: Route HTTP handlers through the shared code**

In `remoteSyncManifestHTTP`, replace the resolve/select/sanitized/build block
with:

```go
	manifest, err := remotesync.ServeManifest(r.Context(), s.ingestionConfig(), req)
	switch {
	case errors.Is(err, remotesync.ErrTargetNotAllowed):
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	case errors.Is(err, remotesync.ErrManifestUnsupported):
		// 501 is in the client's manifest-unsupported set.
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

In `remoteSyncArchiveHTTP`, replace resolve/select/delta selection with
`plan, err := remotesync.PlanArchive(s.ingestionConfig(), req)`, mapping
`ErrTargetNotAllowed` and `ErrFileNotAllowed` to 403 and other errors to 500,
then replace the `if deltaMode … else …` write with
`err = plan.Write(r.Context(), out)`. Keep the gzip and
`streamErrorAwareResponseWriter` logic unchanged.

- [ ] **Step 5: Run tests**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/remotesync/ ./internal/server/`
Expected: PASS. The existing server tests in
`huma_routes_remote_sync_internal_test.go` pin the 403/501/500 behavior.

- [ ] **Step 6: Commit**

```bash
go fmt ./... && go vet ./internal/...
git add internal/remotesync internal/server
git commit -m "refactor(remotesync): share manifest and archive authorization"
```

______________________________________________________________________

### Task 5: Accept `transport = "ssh"` and `destination` in config

**Files:**

- Modify: `internal/config/config.go:763-765` (transport constants), `:837-844`
  (`RemoteHost`), `:1101-1142` (`ValidateRemoteHosts`), `:1976-1987` (load
  trimming)
- Test: `internal/config/config_test.go:2691-2730` (`TestValidateRemoteHosts`)
- Regenerate: `openapi.yaml`, `internal/apiclient/client.gen.go`,
  `frontend/src/lib/api/generated/models/configRemoteHost.ts`

**Interfaces:**

- Produces: `config.RemoteTransportSSH = "ssh"`; `RemoteHost.Destination string`
  (`toml:"destination,omitempty" json:"destination,omitempty"`);
  `func (h RemoteHost) SSHDestination() string` returning `Destination` or
  `Host`.

- [ ] **Step 1: Update the validation table (failing)**

Replace the `"retired transport"` row and add rows:

```go
		{"ssh valid", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH}}, nil},
		{"ssh with destination", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH, Destination: "user@a.example.test"}}, nil},
		{"ssh rejects url", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH, URL: "https://a.example.test"}}, []string{"url is only valid for http"}},
		{"ssh rejects token", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH, Token: "token"}}, []string{"token is only valid for http"}},
		{"ssh rejects option-shaped destination", []RemoteHost{{Host: "a", Transport: RemoteTransportSSH, Destination: "-oProxyCommand=x"}}, []string{"destination must not start with -"}},
		{"ssh rejects option-shaped host without destination", []RemoteHost{{Host: "-a", Transport: RemoteTransportSSH}}, []string{"destination must not start with -"}},
		{"http rejects destination", []RemoteHost{{Host: "a", Transport: RemoteTransportHTTP, URL: "https://a.example.test", Token: "token", Destination: "a"}}, []string{"destination is only valid for ssh"}},
		{"omitted transport rejects destination", []RemoteHost{{Host: "a", URL: "https://a.example.test", Token: "token", Destination: "a"}}, []string{"destination is only valid for ssh"}},
		{"duplicate host across transports", []RemoteHost{{Host: "a", URL: "https://a.example.test", Token: "token"}, {Host: "a", Transport: RemoteTransportSSH}}, []string{"duplicate host"}},
		{"unknown transport", []RemoteHost{{Host: "a", Transport: RemoteTransport("sftp")}}, []string{"invalid transport \"sftp\"", "use http or ssh"}},
```

Also add a load test next to the existing `remote_hosts` load tests (find them
with `rg -n "remote_hosts" internal/config/config_test.go`) asserting that
`destination = "  user@a.example.test  "` loads trimmed.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run 'TestValidateRemoteHosts|RemoteHost'`
Expected: FAIL (`RemoteTransportSSH` and `Destination` undefined).

- [ ] **Step 3: Implement**

```go
const (
	RemoteTransportHTTP RemoteTransport = "http"
	RemoteTransportSSH  RemoteTransport = "ssh"
)
```

Update the `RemoteHost` doc comment to describe both transports, and add the
field after `Token`:

```go
	// Destination is the ssh destination for ssh entries. Empty means Host.
	Destination string `toml:"destination,omitempty" json:"destination,omitempty"`
```

```go
// SSHDestination returns the argument passed to ssh for this entry.
func (h RemoteHost) SSHDestination() string {
	if h.Destination != "" {
		return h.Destination
	}
	return h.Host
}
```

Split validation by transport. Replace the transport, URL, and token checks in
`ValidateRemoteHosts` with a call per entry:

```go
		problems = append(problems, validateRemoteTransport(i+1, h)...)
```

```go
func validateRemoteTransport(entry int, h RemoteHost) []string {
	prefix := fmt.Sprintf("entry %d (%q): ", entry, h.Host)
	var problems []string
	switch h.Transport {
	case "", RemoteTransportHTTP:
		if err := validateRemoteHTTPURL(h.URL); err != nil {
			problems = append(problems, prefix+err.Error())
		}
		if h.Token == "" {
			problems = append(problems, prefix+"token is required for http")
		}
		if h.Destination != "" {
			problems = append(problems, prefix+"destination is only valid for ssh")
		}
	case RemoteTransportSSH:
		if h.URL != "" {
			problems = append(problems, prefix+"url is only valid for http")
		}
		if h.Token != "" {
			problems = append(problems, prefix+"token is only valid for http")
		}
		if strings.HasPrefix(h.SSHDestination(), "-") {
			problems = append(problems, prefix+"destination must not start with -")
		}
	default:
		problems = append(problems, fmt.Sprintf(
			"%sinvalid transport %q; use http or ssh", prefix, h.Transport))
	}
	return problems
}
```

In the load block at `:1976`, add
`Destination: strings.TrimSpace(h.Destination),`.

- [ ] **Step 4: Regenerate API artifacts**

```bash
cd frontend && npm run generate:api && cd ..
git diff --stat openapi.yaml internal/apiclient frontend/src/lib/api/generated
```

Expected: `destination` appears in all three. If generation needs a step not
covered here, read `frontend/scripts/generate-api-client.mjs`.

- [ ] **Step 5: Run tests**

Run: `go test ./internal/config/ && cd frontend && npm run check:api && cd ..`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
go fmt ./... && go vet ./internal/config/
git add internal/config openapi.yaml internal/apiclient frontend/src/lib/api/generated
git commit -m "feat(config): accept ssh remote hosts with an optional destination"
```

______________________________________________________________________

### Task 6: Remote command: `agentsview remote-sync probe` and `serve`

**Files:**

- Create: `internal/sshremote/protocol.go`, `serve.go`, `process.go`,
  `stdout_unix.go`, `stdout_other.go`, `serve_test.go`, `process_test.go`,
  `main_test.go`, `bench_test.go`
- Create: `cmd/agentsview/remote_sync_cmd.go`,
  `cmd/agentsview/remote_sync_cmd_test.go`
- Modify: `cmd/agentsview/cli.go:133` (register the command)

**Interfaces:**

- Produces:

```go
type Op string // OpTargets "targets", OpManifest "manifest", OpArchive "archive"
type ErrorCode string // CodeIncompatibleProtocol, CodeForbidden, CodeUnsupported, CodeInternal
type ServeError struct { Code ErrorCode; Message string }
type ProbeResult struct { Version string `json:"version"`; ProtocolVersion int `json:"protocol_version"` }
type serveRequest struct {
	ProtocolVersion int                         `json:"protocol_version"`
	Targets         *remotesync.TargetSet       `json:"targets,omitempty"`
	Archive         *remotesync.ArchiveRequest  `json:"request,omitempty"`
}
const ServeExitCode = 2
func Serve(ctx context.Context, op Op, cfg config.Config, in io.Reader, out io.Writer) error
func RunServeProcess(op Op, stdin io.Reader, stderr io.Writer) int
func WriteProbe(w io.Writer, version string) error
```

- [ ] **Step 1: Protocol types**

`internal/sshremote/protocol.go`:

```go
// Package sshremote syncs a remote host over SSH by running agentsview on
// the remote for one short operation at a time.
package sshremote

import (
	"fmt"

	"go.kenn.io/agentsview/internal/remotesync"
)

// Op names one remote-sync serve operation.
type Op string

const (
	OpTargets  Op = "targets"
	OpManifest Op = "manifest"
	OpArchive  Op = "archive"
)

// ParseOp validates a command-line operation name.
func ParseOp(name string) (Op, error) {
	switch op := Op(name); op {
	case OpTargets, OpManifest, OpArchive:
		return op, nil
	default:
		return "", fmt.Errorf("unknown remote-sync operation %q", name)
	}
}

type serveRequest struct {
	ProtocolVersion int                        `json:"protocol_version"`
	Targets         *remotesync.TargetSet      `json:"targets,omitempty"`
	Archive         *remotesync.ArchiveRequest `json:"request,omitempty"`
}

// ErrorCode classifies a serve failure. The codes carry the meaning of the
// HTTP remote-sync status codes 426, 403, 501, and 500.
type ErrorCode string

const (
	CodeIncompatibleProtocol ErrorCode = "incompatible_protocol"
	CodeForbidden            ErrorCode = "forbidden"
	CodeUnsupported          ErrorCode = "unsupported"
	CodeInternal             ErrorCode = "internal"
)

// ServeError is a failure reported by the remote serve command. It travels as
// one JSON line on stderr.
type ServeError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *ServeError) Error() string {
	return fmt.Sprintf("remote sync %s: %s", e.Code, e.Message)
}

// ServeExitCode is the exit status of a serve command that reported a
// ServeError on stderr.
const ServeExitCode = 2

// ProbeResult identifies a remote binary.
type ProbeResult struct {
	Version         string `json:"version"`
	ProtocolVersion int    `json:"protocol_version"`
}
```

- [ ] **Step 2: Write failing `Serve` tests**

`internal/sshremote/serve_test.go`. Build a `config.Config` the same way as
`serveTestConfig` from Task 4 (copy that helper into this package's test file;
it cannot be shared across packages). Cover, table-driven:

```go
func TestServe(t *testing.T) {
	cfg, claudeDir := serveTestConfig(t)
	writeServeTestSession(t, claudeDir)
	allowed := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {claudeDir}}}
	outside := remotesync.TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentClaude: {t.TempDir()}}}

	tests := []struct {
		name     string
		op       Op
		request  serveRequest
		wantCode ErrorCode // empty means success
	}{
		{"targets", OpTargets, serveRequest{ProtocolVersion: remotesync.ProtocolVersion}, ""},
		{"wrong protocol", OpTargets, serveRequest{ProtocolVersion: remotesync.ProtocolVersion + 1}, CodeIncompatibleProtocol},
		{"manifest", OpManifest, serveRequest{ProtocolVersion: remotesync.ProtocolVersion, Targets: &allowed}, ""},
		{"manifest outside roots", OpManifest, serveRequest{ProtocolVersion: remotesync.ProtocolVersion, Targets: &outside}, CodeForbidden},
		{"manifest without targets", OpManifest, serveRequest{ProtocolVersion: remotesync.ProtocolVersion}, CodeInternal},
		{"archive", OpArchive, serveRequest{ProtocolVersion: remotesync.ProtocolVersion, Archive: &remotesync.ArchiveRequest{TargetSet: allowed}}, ""},
		{"archive outside roots", OpArchive, serveRequest{ProtocolVersion: remotesync.ProtocolVersion, Archive: &remotesync.ArchiveRequest{TargetSet: outside}}, CodeForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in bytes.Buffer
			require.NoError(t, json.MarshalWrite(&in, tt.request))
			var out bytes.Buffer

			err := Serve(t.Context(), tt.op, cfg, &in, &out)

			if tt.wantCode != "" {
				serveErr, ok := errors.AsType[*ServeError](err)
				require.True(t, ok, "error %v is not a ServeError", err)
				assert.Equal(t, tt.wantCode, serveErr.Code)
				return
			}
			require.NoError(t, err)
			zr, err := remotesync.NewZstdReader(&out)
			require.NoError(t, err)
			defer zr.Close()
			body, err := io.ReadAll(zr)
			require.NoError(t, err)
			assert.NotEmpty(t, body)
		})
	}
}
```

Add one test that the archive response, decoded, is a tar containing the session
file (use `archive/tar` to list entries; the entry name is the absolute path
without its leading `/`).

- [ ] **Step 3: Run to verify failure**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run TestServe`
Expected: compile failure, `Serve` undefined.

- [ ] **Step 4: Implement `serve.go`**

```go
package sshremote

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
)

// serveZstdLevel is chosen from the benchmark in bench_test.go; the pull
// request records the measurements.
const serveZstdLevel = zstd.SpeedDefault

// maxServeRequestBytes bounds the request; delta file lists are the largest.
const maxServeRequestBytes = 64 << 20

// Serve runs one operation. It reads the JSON request from in and writes the
// zstd-compressed response to out. Failures are *ServeError values.
func Serve(ctx context.Context, op Op, cfg config.Config, in io.Reader, out io.Writer) error {
	var req serveRequest
	if err := json.UnmarshalRead(io.LimitReader(in, maxServeRequestBytes), &req); err != nil {
		return &ServeError{Code: CodeInternal, Message: "decode request: " + err.Error()}
	}
	if req.ProtocolVersion != remotesync.ProtocolVersion {
		return &ServeError{Code: CodeIncompatibleProtocol, Message: fmt.Sprintf(
			"collector speaks remote-sync protocol %d, this agentsview speaks %d",
			req.ProtocolVersion, remotesync.ProtocolVersion)}
	}
	zw, err := remotesync.NewZstdWriter(out, serveZstdLevel)
	if err != nil {
		return &ServeError{Code: CodeInternal, Message: err.Error()}
	}
	if err := serveOp(ctx, op, cfg, req, zw); err != nil {
		return classifyServeError(err)
	}
	if err := zw.Close(); err != nil {
		return &ServeError{Code: CodeInternal, Message: "finish response: " + err.Error()}
	}
	return nil
}

func serveOp(ctx context.Context, op Op, cfg config.Config, req serveRequest, out io.Writer) error {
	switch op {
	case OpTargets:
		targets, err := remotesync.ResolveTargets(cfg)
		if err != nil {
			return fmt.Errorf("resolve targets: %w", err)
		}
		return json.MarshalWrite(out, targets)
	case OpManifest:
		if req.Targets == nil {
			return errors.New("manifest request has no targets")
		}
		manifest, err := remotesync.ServeManifest(ctx, cfg, *req.Targets)
		if err != nil {
			return err
		}
		return json.MarshalWrite(out, manifest)
	case OpArchive:
		if req.Archive == nil {
			return errors.New("archive request has no body")
		}
		plan, err := remotesync.PlanArchive(cfg, *req.Archive)
		if err != nil {
			return err
		}
		return plan.Write(ctx, out)
	default:
		return fmt.Errorf("unknown remote-sync operation %q", op)
	}
}

func classifyServeError(err error) *ServeError {
	if serveErr, ok := errors.AsType[*ServeError](err); ok {
		return serveErr
	}
	code := CodeInternal
	switch {
	case errors.Is(err, remotesync.ErrTargetNotAllowed), errors.Is(err, remotesync.ErrFileNotAllowed):
		code = CodeForbidden
	case errors.Is(err, remotesync.ErrManifestUnsupported):
		code = CodeUnsupported
	}
	return &ServeError{Code: code, Message: err.Error()}
}
```

- [ ] **Step 5: Run serve tests**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run TestServe`
Expected: PASS.

- [ ] **Step 6: Write failing process tests (statelessness and interruption)**

`internal/sshremote/main_test.go` turns the test binary into a stand-in for the
remote agentsview when an env var is set. Later tasks reuse it.

```go
package sshremote

import (
	"fmt"
	"os"
	"testing"
)

// testChildEnv makes the test binary act as the remote agentsview, so tests
// exercise the real process code without building cmd/agentsview.
const testChildEnv = "AGENTSVIEW_SSHREMOTE_TEST_CHILD"

const testChildVersion = "test-child"

func TestMain(m *testing.M) {
	if os.Getenv(testChildEnv) == "1" {
		os.Exit(childMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func childMain(args []string) int {
	switch {
	case len(args) == 2 && args[0] == "remote-sync" && args[1] == "probe":
		if err := WriteProbe(os.Stdout, testChildVersion); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case len(args) == 4 && args[0] == "remote-sync" && args[1] == "serve" && args[2] == "--op":
		op, err := ParseOp(args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return RunServeProcess(op, os.Stdin, os.Stderr)
	default:
		fmt.Fprintf(os.Stderr, "unexpected child args %q\n", args)
		return 1
	}
}

```

`internal/sshremote/process_test.go`. Helpers:

- `hermesFixtureHome(t) string`: creates `HOME` with `.hermes/sessions/` holding
  one small session file and `.hermes/state.db` of about 40 MiB (same
  `randomblob` loop as Task 3), so the snapshot is still streaming when the
  pipe fills. Confirm with `remotesync.ResolveTargets` on a config loaded for
  that `HOME` that the targets include the state DB; if Hermes needs a
  different layout, read `resolveHermesTargets` in
  `internal/remotesync/resolve.go`.
- `startServeChild(t, home, tmpdir string, op Op, request serveRequest) (*exec.Cmd, io.ReadCloser)`:
  runs `os.Args[0] remote-sync serve --op <op>` with
  `Env: []string{testChildEnv + "=1", "HOME=" + home, "TMPDIR=" + tmpdir, "PATH=" + os.Getenv("PATH")}`,
  writes the JSON request to its stdin and closes stdin, and returns the
  stdout pipe.
- `treeState(t, root string) map[string]string`: walks `root` and records each
  path relative to `root` with its mode, size, and mtime.
- `snapshotDirs(t, tmpdir string) []string`: globs
  `agentsview-sqlite-snapshot-*` in `tmpdir`.
- `waitExit(t, cmd) *exec.ExitError`: waits in a goroutine and fails after a 60
  s hang guard, using `select` on the wait result and `time.After`. The guard
  only bounds a hang; the event under test is the process exit.
- `watchSnapshotDirs(t, tmpdir) <-chan struct{}`: starts an
  `github.com/fsnotify/fsnotify` watcher on `tmpdir` (already a dependency)
  before the child starts, and signals once on a `Create` event whose base
  name starts with `agentsview-sqlite-snapshot-`. Close the watcher in
  `t.Cleanup`.
- `waitForSnapshotDir(t, created)`: `select` on `created` with a 60 s
  `time.After` hang guard. This waits on the real event, as
  `docs/agents/testing.md` requires; do not poll with `Eventually`.

Tests:

```go
func TestServeProcessLeavesNoState(t *testing.T) {
	for _, op := range []Op{OpTargets, OpManifest, OpArchive} {
		t.Run(string(op), func(t *testing.T) {
			home := hermesFixtureHome(t)
			tmpdir := t.TempDir()
			before := treeState(t, home)
			cmd, stdout := startServeChild(t, home, tmpdir, op, requestFor(t, home, op))
			_, err := io.Copy(io.Discard, stdout)
			require.NoError(t, err)
			require.NoError(t, cmd.Wait())

			assert.Equal(t, before, treeState(t, home))
			entries, err := os.ReadDir(tmpdir)
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestServeProcessCleansUpWhenReaderCloses(t *testing.T) {
	home := hermesFixtureHome(t)
	tmpdir := t.TempDir()
	created := watchSnapshotDirs(t, tmpdir)
	cmd, stdout := startServeChild(t, home, tmpdir, OpArchive, requestFor(t, home, OpArchive))
	waitForSnapshotDir(t, created)

	require.NoError(t, stdout.Close())
	exitErr := waitExit(t, cmd)

	require.NotNil(t, exitErr)
	assert.Equal(t, ServeExitCode, exitErr.ExitCode())
	assert.Empty(t, snapshotDirs(t, tmpdir))
}

func TestServeProcessCleansUpOnSIGTERMWithStalledReader(t *testing.T) {
	home := hermesFixtureHome(t)
	tmpdir := t.TempDir()
	created := watchSnapshotDirs(t, tmpdir)
	cmd, stdout := startServeChild(t, home, tmpdir, OpArchive, requestFor(t, home, OpArchive))
	t.Cleanup(func() { _ = stdout.Close() })
	// Never read: the child fills the pipe and blocks in a stdout write while
	// the read end stays open, so only cancellation can end it.
	waitForSnapshotDir(t, created)

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	exitErr := waitExit(t, cmd)

	require.NotNil(t, exitErr)
	assert.Equal(t, ServeExitCode, exitErr.ExitCode())
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	assert.False(t, status.Signaled())
	assert.Empty(t, snapshotDirs(t, tmpdir))
}
```

`requestFor` builds the request per op: targets need only the protocol version;
manifest and archive use the target set returned by running the `targets` op
first through `startServeChild` (decode with `remotesync.NewZstdReader`). Put
`process_test.go` behind `//go:build unix`.

- [ ] **Step 7: Run to verify failure**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run TestServeProcess`
Expected: compile failure, `RunServeProcess` and `WriteProbe` undefined.

- [ ] **Step 8: Implement the process wrapper**

`internal/sshremote/stdout_unix.go`:

```go
//go:build unix

package sshremote

import (
	"fmt"
	"os"
	"syscall"
)

// pollableStdout reopens fd 1 in non-blocking mode. os.NewFile then returns a
// pollable file, and closing it makes a blocked write return os.ErrClosed.
func pollableStdout() (*os.File, error) {
	if err := syscall.SetNonblock(1, true); err != nil {
		return nil, fmt.Errorf("set stdout non-blocking: %w", err)
	}
	return os.NewFile(1, "stdout"), nil
}
```

`internal/sshremote/stdout_other.go`:

```go
//go:build !unix

package sshremote

import "os"

// pollableStdout returns stdout unchanged; remote serve supports Linux and
// macOS only.
func pollableStdout() (*os.File, error) { return os.Stdout, nil }
```

`internal/sshremote/process.go`:

```go
package sshremote

import (
	"context"
	"encoding/json/v2"
	"io"
	"os"
	"os/signal"
	"syscall"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
)

// RunServeProcess is the body of `agentsview remote-sync serve`. It owns
// stdout and signals so an interrupted transfer still removes its temporary
// files, and returns the process exit code.
func RunServeProcess(op Op, stdin io.Reader, stderr io.Writer) int {
	// Without Notify, a write to a closed stdout kills the process with
	// SIGPIPE before deferred cleanup runs. With it, the write returns EPIPE.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	out, err := pollableStdout()
	if err != nil {
		return reportServeError(stderr, &ServeError{Code: CodeInternal, Message: err.Error()})
	}
	// A signal cancels ctx but cannot interrupt a write blocked on a full
	// pipe. Closing the pollable stdout releases that write with an error.
	go func() {
		<-ctx.Done()
		_ = out.Close()
	}()

	cfg, err := config.LoadReadOnly()
	if err != nil {
		return reportServeError(stderr, &ServeError{Code: CodeInternal, Message: "load config: " + err.Error()})
	}
	if err := Serve(ctx, op, cfg, stdin, out); err != nil {
		return reportServeError(stderr, classifyServeError(err))
	}
	return 0
}

func reportServeError(stderr io.Writer, serveErr *ServeError) int {
	_ = json.MarshalWrite(stderr, serveErr)
	_, _ = io.WriteString(stderr, "\n")
	return ServeExitCode
}

// WriteProbe prints the ProbeResult line for this binary.
func WriteProbe(w io.Writer, version string) error {
	if err := json.MarshalWrite(w, ProbeResult{
		Version: version, ProtocolVersion: remotesync.ProtocolVersion,
	}); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
```

- [ ] **Step 9: Run process tests**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -count=1` Expected:
PASS. If `TestServeProcessLeavesNoState` finds a new file, the failure names it;
trace which code wrote it and stop that write on the serve path rather than
relaxing the test.

- [ ] **Step 10: Add the CLI command**

`cmd/agentsview/remote_sync_cmd.go`:

```go
package main

import (
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/agentsview/internal/sshremote"
)

// newRemoteSyncCommand is the remote half of SSH sync. Collectors run it
// over ssh; it is not meant for people.
func newRemoteSyncCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "remote-sync",
		Short:  "Serve one SSH remote-sync operation",
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "probe",
		Short: "Print this binary's version and remote-sync protocol",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return sshremote.WriteProbe(cmd.OutOrStdout(), version)
		},
	})
	var opName string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run one remote-sync operation over stdin and stdout",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			op, err := sshremote.ParseOp(opName)
			if err != nil {
				return err
			}
			if code := sshremote.RunServeProcess(op, os.Stdin, os.Stderr); code != 0 {
				return &cliExitError{code: code, silent: true}
			}
			return nil
		},
	}
	serve.Flags().StringVar(&opName, "op", "", "operation: targets, manifest, or archive")
	_ = serve.MarkFlagRequired("op")
	cmd.AddCommand(serve)
	return cmd
}
```

Register it in `cli.go` next to the other `root.AddCommand` calls:
`root.AddCommand(newRemoteSyncCommand())`.

`cmd/agentsview/remote_sync_cmd_test.go`: run `executeCLIWithLegacyFlagCompat`
with `[]string{"remote-sync", "probe"}` into a buffer and assert the JSON line
decodes to
`ProbeResult{Version: version, ProtocolVersion: remotesync.ProtocolVersion}`.

- [ ] **Step 11: Benchmark zstd levels**

`internal/sshremote/bench_test.go`:

```go
// BenchmarkServeArchiveZstd measures response size and time per zstd level.
// Point AGENTSVIEW_BENCH_REMOTE_HOME at a scratch copy of real agent data;
// never at a live home directory (docs/agents/background-work.md).
func BenchmarkServeArchiveZstd(b *testing.B) {
	home := os.Getenv("AGENTSVIEW_BENCH_REMOTE_HOME")
	if home == "" {
		b.Skip("set AGENTSVIEW_BENCH_REMOTE_HOME to a scratch copy of agent data")
	}
	b.Setenv("HOME", home)
	cfg, err := config.LoadReadOnly()
	require.NoError(b, err)
	targets, err := remotesync.ResolveTargets(cfg)
	require.NoError(b, err)
	var raw countingWriter
	require.NoError(b, remotesync.WriteArchive(b.Context(), &raw, targets))
	for _, level := range []zstd.EncoderLevel{
		zstd.SpeedFastest, zstd.SpeedDefault, zstd.SpeedBetterCompression, zstd.SpeedBestCompression,
	} {
		b.Run(level.String(), func(b *testing.B) {
			var out countingWriter
			for b.Loop() {
				out = countingWriter{}
				zw, err := remotesync.NewZstdWriter(&out, level)
				require.NoError(b, err)
				require.NoError(b, remotesync.WriteArchive(b.Context(), zw, targets))
				require.NoError(b, zw.Close())
			}
			b.ReportMetric(float64(raw.n), "raw-bytes")
			b.ReportMetric(float64(out.n), "zstd-bytes")
			b.ReportMetric(float64(raw.n)/float64(out.n), "ratio")
		})
	}
}

type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }
```

Run it against a scratch copy (for example `cp -a` a few hundred MB of
`~/.claude/projects` into the scratchpad as `<scratch>/.claude/projects`):

```bash
AGENTSVIEW_BENCH_REMOTE_HOME=<scratch> CGO_ENABLED=1 go test -tags fts5 \
  ./internal/sshremote/ -run '^$' -bench BenchmarkServeArchiveZstd -benchtime 3x
```

Also time decompression of each level's output (add a sub-benchmark or a one-off
`zstd -d` timing). Pick the level with the best size per second spent for a
typical home connection, set `serveZstdLevel`, and save the table (level, raw
bytes, zstd bytes, ratio, ns/op, decode time, Go and zstd module versions) for
the pull request. Delete the scratch copy afterwards.

- [ ] **Step 12: Run all checks and commit**

```bash
go fmt ./... && go vet ./internal/sshremote/ ./cmd/agentsview/
CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ ./cmd/agentsview/ -run 'TestServe|RemoteSync|Probe'
git add internal/sshremote cmd/agentsview
git commit -m "feat(sshremote): add the stateless remote-sync serve command"
```

______________________________________________________________________

### Task 7: Run ssh and classify its failures

**Files:**

- Create: `internal/sshremote/exec.go`, `internal/sshremote/errors.go`,
  `internal/sshremote/exec_test.go`, `internal/sshremote/fakessh_test.go`
- Modify: `internal/remotesync/failure.go:21` (`FailureSummary`), `:90`
  (`IsHostUnavailable`), `internal/remotesync/prepare.go:1017` (delta retry)
- Test: `internal/remotesync/failure_test.go`

**Interfaces:**

- Produces:

```go
type Runner struct { Destination string; SSHPath string }
func (r Runner) Output(ctx context.Context, script string, stdin io.Reader) ([]byte, error)
func (r Runner) Stream(ctx context.Context, script string, stdin io.Reader) (io.ReadCloser, error)
type CommandError struct { ExitCode int; Stderr string; Offline bool; Remote *ServeError }
func (e *CommandError) HostUnavailable() bool
func (e *CommandError) FailureSummary() string
func (e *CommandError) RemoteRefused() bool
func validateScript(script string) error
```

remotesync gains three unexported interfaces checked through `errors.AsType`:
`HostUnavailable() bool`, `FailureSummary() string`, `RemoteRefused() bool`.

- [ ] **Step 1: Write the fake ssh helpers**

`internal/sshremote/fakessh_test.go` writes executables into a temp dir:

```go
// fakeSSHScript stands in for ssh: it drops the options agentsview passes,
// records the destination, and runs the remote command locally with a clean
// environment whose HOME is FAKE_SSH_HOME.
const fakeSSHScript = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in
    -T) shift ;;
    -o) shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
dest=$1
shift
printf '%s\n' "$dest" >> "$FAKE_SSH_LOG"
exec env -i HOME="$FAKE_SSH_HOME" PATH="$FAKE_SSH_PATH" TMPDIR="$FAKE_SSH_TMPDIR" \
  AGENTSVIEW_SSHREMOTE_TEST_CHILD=1 /bin/sh -c "$*"
`

// fakeSSHFailScript prints FAKE_SSH_STDERR and exits FAKE_SSH_EXIT, like ssh
// failing before the remote command starts.
const fakeSSHFailScript = `#!/bin/sh
printf '%b' "$FAKE_SSH_STDERR" >&2
exit "$FAKE_SSH_EXIT"
`

type fakeSSH struct {
	Path    string // executable to pass as Runner.SSHPath
	Home    string // remote HOME
	TempDir string // remote TMPDIR
	Log     string // one destination per ssh call
}

func newFakeSSH(t *testing.T) fakeSSH { /* write fakeSSHScript, t.Setenv the FAKE_SSH_* vars; FAKE_SSH_PATH is os.Getenv("PATH") */ }

func newFailingSSH(t *testing.T, stderr string, exit int) string { /* write fakeSSHFailScript, t.Setenv FAKE_SSH_STDERR/EXIT, return path */ }
```

Mark the file `//go:build unix`.

- [ ] **Step 2: Write failing classification tests**

`internal/sshremote/exec_test.go` (`//go:build unix`):

```go
func TestRunnerClassifiesSSHFailures(t *testing.T) {
	tests := []struct {
		name        string
		stderr      string
		exit        int
		wantOffline bool
	}{
		{"dns", "ssh: Could not resolve hostname devbox.example.test: Name or service not known\n", 255, true},
		{"refused", "ssh: connect to host 192.0.2.10 port 22: Connection refused\n", 255, true},
		{"timed out", "ssh: connect to host 192.0.2.10 port 22: Connection timed out\n", 255, true},
		{"macos timeout", "ssh: connect to host 192.0.2.10 port 22: Operation timed out\n", 255, true},
		{"no route", "ssh: connect to host 192.0.2.10 port 22: No route to host\n", 255, true},
		{"unreachable", "ssh: connect to host 192.0.2.10 port 22: Network is unreachable\n", 255, true},
		{"banner", "Connection timed out during banner exchange\nConnection to UNKNOWN port 65535 timed out\n", 255, true},
		{"jump host refused", "ssh: connect to host jump.example.test port 22: Connection refused\nConnection closed by UNKNOWN port 65535\n", 255, true},
		{"bad port config", "command-line line 0: Bad port 'notaport'.\n", 255, false},
		{"auth", "user@devbox.example.test: Permission denied (publickey).\n", 255, false},
		{"host key", "Host key verification failed.\n", 255, false},
		{"target behind jump", "channel 0: open failed: connect failed: Connection refused\nstdio forwarding failed\nConnection closed by UNKNOWN port 65535\n", 255, false},
		{"offline line but other exit", "ssh: connect to host 192.0.2.10 port 22: Connection refused\n", 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := Runner{Destination: "devbox", SSHPath: newFailingSSH(t, tt.stderr, tt.exit)}

			_, err := runner.Output(t.Context(), "true", nil)

			cmdErr, ok := errors.AsType[*CommandError](err)
			require.True(t, ok, "error %v is not a CommandError", err)
			assert.Equal(t, tt.exit, cmdErr.ExitCode)
			assert.Equal(t, tt.wantOffline, cmdErr.Offline)
			assert.Equal(t, tt.wantOffline, remotesync.IsHostUnavailable(err))
			if !tt.wantOffline {
				assert.Contains(t, remotesync.FailureSummary(err), firstLine(tt.stderr))
			}
		})
	}
}

func TestRunnerReportsRemoteServeError(t *testing.T) {
	ssh := newFailingSSH(t, `{"code":"forbidden","message":"remote sync target is not allowed"}\n`, ServeExitCode)
	runner := Runner{Destination: "devbox", SSHPath: ssh}

	_, err := runner.Output(t.Context(), "true", strings.NewReader("{}"))

	cmdErr, ok := errors.AsType[*CommandError](err)
	require.True(t, ok)
	require.NotNil(t, cmdErr.Remote)
	assert.Equal(t, CodeForbidden, cmdErr.Remote.Code)
	assert.True(t, cmdErr.RemoteRefused())
	assert.False(t, remotesync.IsHostUnavailable(err))
}

func TestRunnerPassesStdinAndReturnsStdout(t *testing.T) {
	ssh := newFakeSSH(t)
	runner := Runner{Destination: "devbox", SSHPath: ssh.Path}

	out, err := runner.Output(t.Context(), "cat", strings.NewReader("hello"))

	require.NoError(t, err)
	assert.Equal(t, "hello", string(out))
}

func TestValidateScriptRejectsShellHazards(t *testing.T) {
	for _, script := range []string{"echo 'x'", "echo a\necho b", "echo hi!"} {
		assert.Error(t, validateScript(script), script)
	}
	assert.NoError(t, validateScript(`printf "os=%s\n" "$(uname -s)"`))
}
```

`firstLine` returns the first line of a string.

- [ ] **Step 3: Run to verify failure**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run 'TestRunner|TestValidateScript'`
Expected: compile failure, `Runner` undefined.

- [ ] **Step 4: Implement `errors.go`**

```go
package sshremote

import (
	"encoding/json/v2"
	"fmt"
	"regexp"
	"strings"

	"go.kenn.io/agentsview/internal/stringutil"
)

// sshConnectionFailed is OpenSSH's exit status for any error of its own.
const sshConnectionFailed = 255

// offlinePatterns are the OpenSSH lines that mean the network could not reach
// the host or its jump host. They match the network errors
// remotesync.IsHostUnavailable accepts for HTTP. Matching needs LC_ALL=C.
var offlinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^ssh: Could not resolve hostname .+: .+$`),
	regexp.MustCompile(`^ssh: connect to host .+ port [0-9]+: ` +
		`(Connection refused|Connection timed out|Operation timed out|No route to host|Network is unreachable)$`),
	regexp.MustCompile(`^Connection timed out during banner exchange$`),
}

// CommandError reports a failed ssh invocation.
type CommandError struct {
	ExitCode int
	// Stderr is the captured, bounded stderr of ssh and the remote command.
	Stderr string
	// Offline means ssh could not reach the host.
	Offline bool
	// Remote is the serve failure the remote command reported, if any.
	Remote *ServeError
}

func newCommandError(exitCode int, stderr string) *CommandError {
	e := &CommandError{ExitCode: exitCode, Stderr: stderr}
	lines := strings.Split(strings.ReplaceAll(stderr, "\r", ""), "\n")
	switch exitCode {
	case sshConnectionFailed:
		e.Offline = matchesOffline(lines)
	case ServeExitCode:
		e.Remote = lastServeError(lines)
	}
	return e
}

func matchesOffline(lines []string) bool {
	for _, line := range lines {
		for _, pattern := range offlinePatterns {
			if pattern.MatchString(line) {
				return true
			}
		}
	}
	return false
}

func lastServeError(lines []string) *ServeError {
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var serveErr ServeError
		if json.Unmarshal([]byte(line), &serveErr) == nil && serveErr.Code != "" {
			return &serveErr
		}
		return nil
	}
	return nil
}

func (e *CommandError) Error() string {
	if e.Remote != nil {
		return e.Remote.Error()
	}
	return fmt.Sprintf("ssh exited %d: %s", e.ExitCode, strings.TrimSpace(e.Stderr))
}

// HostUnavailable lets remotesync skip an unreachable host.
func (e *CommandError) HostUnavailable() bool { return e.Offline }

// RemoteRefused lets remotesync retry a refused delta as a full archive.
func (e *CommandError) RemoteRefused() bool { return e.Remote != nil }

const summaryDetailBytes = 300

// FailureSummary is the message the CLI and API show for this failure.
func (e *CommandError) FailureSummary() string {
	switch {
	case e.Offline:
		return "SSH remote sync skipped: host is unreachable"
	case e.Remote != nil:
		return fmt.Sprintf("SSH remote sync failed: remote agentsview reported %s: %s",
			e.Remote.Code, stringutil.SafeTruncate(e.Remote.Message, summaryDetailBytes))
	default:
		return fmt.Sprintf("SSH remote sync failed: %s",
			stringutil.SafeTruncate(firstNonEmptyLine(e.Stderr), summaryDetailBytes))
	}
}
```

Write `firstNonEmptyLine`. Check `stringutil.SafeTruncate`'s exact signature
with `rg -n "func SafeTruncate" internal/stringutil`.

- [ ] **Step 5: Implement `exec.go`**

```go
package sshremote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// maxCapturedStderr bounds stderr kept for error messages.
const maxCapturedStderr = 32 << 10

// Runner runs fixed scripts on a remote host through the system ssh client.
type Runner struct {
	// Destination is the ssh destination argument.
	Destination string
	// SSHPath is the ssh executable. Empty means "ssh" on PATH.
	SSHPath string
}

// validateScript enforces the shape every remote script must have. The
// remote login shell may be csh or tcsh, which treat newlines and "!"
// specially even inside single quotes.
func validateScript(script string) error {
	if strings.ContainsAny(script, "'\n!") {
		return fmt.Errorf("remote script must be one line without ' or !: %q", script)
	}
	return nil
}

func (r Runner) command(ctx context.Context, script string) (*exec.Cmd, error) {
	if err := validateScript(script); err != nil {
		return nil, err
	}
	path := r.SSHPath
	if path == "" {
		path = "ssh"
	}
	cmd := exec.CommandContext(ctx, path,
		"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "Compression=no",
		"--", r.Destination, "sh -c '"+script+"'")
	// Untranslated system error text keeps offline classification reliable.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	// Bound how long Wait blocks on pipes after ctx kills ssh.
	cmd.WaitDelay = 10 * time.Second
	return cmd, nil
}

// Output runs script, feeds it stdin, and returns stdout once ssh exits 0.
func (r Runner) Output(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	body, err := r.Stream(ctx, script, stdin)
	if err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if closeErr != nil {
		return nil, closeErr
	}
	if readErr != nil {
		return nil, fmt.Errorf("read ssh output: %w", readErr)
	}
	return out, nil
}

// Stream starts script and returns its stdout. Close waits for ssh and
// returns a *CommandError when it failed.
func (r Runner) Stream(ctx context.Context, script string, stdin io.Reader) (io.ReadCloser, error) {
	cmd, err := r.command(ctx, script)
	if err != nil {
		return nil, err
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	// Exec copies stdin in its own goroutine. A write error there (EPIPE when
	// ssh exits early) is reported by Wait only after ssh's own status, which
	// Close prefers.
	cmd.Stdin = stdin
	stderr := &boundedBuffer{limit: maxCapturedStderr}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open ssh stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh: %w", err)
	}
	return &commandStream{ReadCloser: stdout, cmd: cmd, stderr: stderr}, nil
}

type commandStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	stderr *boundedBuffer
}

func (s *commandStream) Close() error {
	err := s.cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() >= 0 {
		return newCommandError(exitErr.ExitCode(), s.stderr.String())
	}
	if err != nil {
		return fmt.Errorf("ssh: %w", err)
	}
	return nil
}

type boundedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
```

Notes for the implementer: `cmd.Wait` closes the stdout pipe, so callers must
finish reading before `Close`. `ExitCode()` is -1 when ssh was killed by a
signal (for example on ctx cancel); that path returns the wrapped error, and
`IsHostUnavailable` already treats a cancelled context as not unavailable.

- [ ] **Step 6: Add the remotesync hooks**

In `internal/remotesync/failure.go`:

```go
// Transports outside this package classify their own errors through these
// interfaces; remotesync cannot import them.
type hostAvailabilityError interface {
	error
	HostUnavailable() bool
}

type failureSummaryError interface {
	error
	FailureSummary() string
}

type remoteRefusalError interface {
	error
	RemoteRefused() bool
}

// isRemoteRefusal reports a remote that understood the request and refused
// it, which prepareMirror answers by retrying a delta as a full archive.
func isRemoteRefusal(err error) bool {
	if _, ok := errors.AsType[*StatusError](err); ok {
		return true
	}
	refusal, ok := errors.AsType[remoteRefusalError](err)
	return ok && refusal.RemoteRefused()
}
```

In `FailureSummary`, right after the `PendingCleanupError` check:

```go
	if summary, ok := errors.AsType[failureSummaryError](err); ok {
		return summary.FailureSummary()
	}
```

In `IsHostUnavailable`, right after the `cleanupRetrier` check:

```go
	if availability, ok := errors.AsType[hostAvailabilityError](err); ok {
		return availability.HostUnavailable()
	}
```

In `prepare.go` at the delta retry, replace
`_, hasStatusErr := errors.AsType[*StatusError](err)` and its use with
`if err != nil && !full && isRemoteRefusal(err) {`.

Add to `internal/remotesync/failure_test.go` a table test with a local fake
error type implementing each interface, asserting that `IsHostUnavailable`,
`FailureSummary`, and `isRemoteRefusal` honor them, including through
`fmt.Errorf("%w")` wrapping and a `*HostError`.

- [ ] **Step 7: Run tests**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ ./internal/remotesync/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
go fmt ./... && go vet ./internal/sshremote/ ./internal/remotesync/
git add internal/sshremote internal/remotesync
git commit -m "feat(sshremote): run ssh and keep only network failures offline"
```

______________________________________________________________________

### Task 8: Binary identity and verified release downloads

**Files:**

- Create: `internal/sshremote/identity.go`, `internal/sshremote/release.go`,
  `internal/sshremote/identity_test.go`, `internal/sshremote/release_test.go`

**Interfaces:**

- Produces:

```go
type LocalBuild struct { Version string; Executable string }
func (b LocalBuild) ReleaseID() (string, bool)        // "v0.45.0", true
func (b LocalBuild) ContentID() (string, error)       // "<ver>-<16 hex>"
type platform struct { OS, Arch string }
func parsePlatform(unameS, unameM string) (platform, error)
func localPlatform() platform
var errReleaseNotFound error
type releaseFetcher struct { BaseURL string; Client *http.Client; CacheDir string }
func (f releaseFetcher) Fetch(ctx context.Context, version string, p platform) (string, error)
```

- [ ] **Step 1: Write failing tests**

`identity_test.go`:

```go
func TestLocalBuildIdentities(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agentsview")
	require.NoError(t, os.WriteFile(exe, []byte("build one"), 0o755))

	tests := []struct {
		version     string
		wantRelease string
		isRelease   bool
	}{
		{"v0.45.0", "v0.45.0", true},
		{"0.45.0", "v0.45.0", true},
		{"dev", "", false},
		{"v0.45.0-50-g86f100ea", "", false},
		{"v0.45.0-50-g86f100ea-dirty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			build := LocalBuild{Version: tt.version, Executable: exe}
			id, ok := build.ReleaseID()
			assert.Equal(t, tt.isRelease, ok)
			assert.Equal(t, tt.wantRelease, id)
			content, err := build.ContentID()
			require.NoError(t, err)
			assert.Regexp(t, `^[0-9A-Za-z._+-]+-[0-9a-f]{16}$`, content)
		})
	}
}

func TestContentIDChangesWithBytes(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agentsview")
	require.NoError(t, os.WriteFile(exe, []byte("build one"), 0o755))
	first, err := LocalBuild{Version: "dev", Executable: exe}.ContentID()
	require.NoError(t, err)
	// A rebuild changes size or mtime, which invalidates the cached hash.
	require.NoError(t, os.WriteFile(exe, []byte("build two, longer"), 0o755))

	second, err := LocalBuild{Version: "dev", Executable: exe}.ContentID()

	require.NoError(t, err)
	assert.NotEqual(t, first, second)
}

func TestParsePlatform(t *testing.T) {
	tests := []struct {
		s, m    string
		want    platform
		wantErr bool
	}{
		{"Linux", "x86_64", platform{"linux", "amd64"}, false},
		{"Linux", "aarch64", platform{"linux", "arm64"}, false},
		{"Darwin", "arm64", platform{"darwin", "arm64"}, false},
		{"Darwin", "x86_64", platform{"darwin", "amd64"}, false},
		{"FreeBSD", "amd64", platform{}, true},
		{"Linux", "riscv64", platform{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.s+"/"+tt.m, func(t *testing.T) {
			got, err := parsePlatform(tt.s, tt.m)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.s)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
```

`release_test.go`: an `httptest.Server` serves
`/v0.45.0/agentsview_0.45.0_linux_amd64.tar.gz` and `/v0.45.0/SHA256SUMS`
(computed from the served bytes). Cases, table-driven over the server's
behavior:

- good download: `Fetch` returns a path under `CacheDir/0.45.0/` whose bytes
  match; a second `Fetch` with the server closed still succeeds (cache hit,
  re-verified against the cached `SHA256SUMS`).

- checksum mismatch: `SHA256SUMS` has a different hash; `Fetch` errors with
  "checksum", and no tarball remains in the cache dir.

- missing entry: `SHA256SUMS` lacks the file name; error names the file.

- 404 tarball or 404 `SHA256SUMS`: `errors.Is(err, errReleaseNotFound)`.

- [ ] **Step 2: Run to verify failure**

Run:
`go test ./internal/sshremote/ -run 'TestLocalBuild|TestContentID|TestParsePlatform|TestReleaseFetcher'`
Expected: compile failures.

- [ ] **Step 3: Implement `identity.go`**

```go
package sshremote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	releaseVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)
	identityPattern       = regexp.MustCompile(`^[0-9A-Za-z._+-]+$`)
	unsafeIdentityChars   = regexp.MustCompile(`[^0-9A-Za-z._+-]`)
)

// LocalBuild describes the collector binary that will provision remotes.
type LocalBuild struct {
	Version    string
	Executable string
}

// ReleaseID returns the identity of the official release binary for this
// version. Release tags are immutable, so the version alone names the bytes.
func (b LocalBuild) ReleaseID() (string, bool) {
	if !releaseVersionPattern.MatchString(b.Version) {
		return "", false
	}
	return "v" + strings.TrimPrefix(b.Version, "v"), true
}

// releaseVersion is the version without its leading "v", as used in
// release file names.
func (b LocalBuild) releaseVersion() string { return strings.TrimPrefix(b.Version, "v") }

// ContentID names the exact local executable. Development builds share
// version strings, so the hash keeps a rebuilt binary from reusing an old one.
func (b LocalBuild) ContentID() (string, error) {
	sum, err := executableHash(b.Executable)
	if err != nil {
		return "", err
	}
	version := unsafeIdentityChars.ReplaceAllString(b.Version, "_")
	if version == "" {
		version = "unknown"
	}
	return version + "-" + sum[:16], nil
}

type hashKey struct {
	path  string
	size  int64
	mtime time.Time
}

var (
	hashCacheMu sync.Mutex
	hashCache   = map[hashKey]string{}
)

// executableHash hashes path once per size and mtime.
func executableHash(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat local executable: %w", err)
	}
	key := hashKey{path: path, size: info.Size(), mtime: info.ModTime()}
	hashCacheMu.Lock()
	defer hashCacheMu.Unlock()
	if sum, ok := hashCache[key]; ok {
		return sum, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open local executable: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash local executable: %w", err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	hashCache[key] = sum
	return sum, nil
}
```

- [ ] **Step 4: Implement `release.go`**

```go
package sshremote

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const defaultReleaseBaseURL = "https://github.com/kenn-io/agentsview/releases/download"

var errReleaseNotFound = errors.New("no published release for this version")

type platform struct {
	OS   string
	Arch string
}

func (p platform) String() string { return p.OS + "/" + p.Arch }

func localPlatform() platform { return platform{OS: runtime.GOOS, Arch: runtime.GOARCH} }

// parsePlatform maps `uname -s` and `uname -m` to a release platform.
func parsePlatform(unameS, unameM string) (platform, error) {
	var p platform
	switch unameS {
	case "Linux":
		p.OS = "linux"
	case "Darwin":
		p.OS = "darwin"
	}
	switch unameM {
	case "x86_64", "amd64":
		p.Arch = "amd64"
	case "aarch64", "arm64":
		p.Arch = "arm64"
	}
	if p.OS == "" || p.Arch == "" {
		return platform{}, fmt.Errorf(
			"remote platform %s %s is not supported for SSH sync; install agentsview on the remote",
			unameS, unameM)
	}
	return p, nil
}

// releaseFetcher downloads and verifies release CLI tarballs.
type releaseFetcher struct {
	BaseURL  string
	Client   *http.Client
	CacheDir string // <data_dir>/ssh-binaries
}

func releaseFileName(version string, p platform) string {
	return fmt.Sprintf("agentsview_%s_%s_%s.tar.gz", version, p.OS, p.Arch)
}

// Fetch returns the path of a verified tarball for version (without "v").
func (f releaseFetcher) Fetch(ctx context.Context, version string, p platform) (string, error) {
	dir := filepath.Join(f.CacheDir, version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create release cache: %w", err)
	}
	name := releaseFileName(version, p)
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if _, err := os.Stat(sumsPath); errors.Is(err, os.ErrNotExist) {
		if err := f.download(ctx, version, "SHA256SUMS", sumsPath); err != nil {
			return "", err
		}
	}
	want, err := checksumFor(sumsPath, name)
	if err != nil {
		return "", err
	}
	tarball := filepath.Join(dir, name)
	if got, err := fileSHA256(tarball); err == nil && got == want {
		return tarball, nil
	}
	tmp := tarball + ".partial"
	if err := f.download(ctx, version, name, tmp); err != nil {
		return "", err
	}
	got, err := fileSHA256(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if got != want {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("release %s checksum mismatch: got %s, SHA256SUMS lists %s", name, got, want)
	}
	if err := os.Rename(tmp, tarball); err != nil {
		return "", fmt.Errorf("store release tarball: %w", err)
	}
	return tarball, nil
}
```

Write `download(ctx, version, name, dst string) error`: GET
`<BaseURL>/v<version>/<name>`; 404 returns
`fmt.Errorf("%w: %s", errReleaseNotFound, url)`; other non-2xx returns the
status; the body goes to `dst` through a temp file renamed on success. Write
`checksumFor(path, name)`: scan lines `"<hex>  <name>"`, return the hex for an
exact name match, or an error naming `name`. Write `fileSHA256(path)`. `BaseURL`
empty means `defaultReleaseBaseURL`; `Client` nil means
`&http.Client{Timeout: 10 * time.Minute}`.

- [ ] **Step 5: Run tests and commit**

```bash
go fmt ./... && go vet ./internal/sshremote/
go test ./internal/sshremote/ -run 'TestLocalBuild|TestContentID|TestParsePlatform|TestReleaseFetcher'
git add internal/sshremote
git commit -m "feat(sshremote): identify remote binaries and verify release downloads"
```

______________________________________________________________________

### Task 9: Provision the remote binary

**Files:**

- Create: `internal/sshremote/provision.go`,
  `internal/sshremote/provision_test.go`

**Interfaces:**

- Consumes: `Runner`, `LocalBuild`, `releaseFetcher`, `parsePlatform`,
  `localPlatform`, `ProbeResult`, `validateScript`, `identityPattern`.
- Produces:

```go
type Provisioner struct { Runner Runner; Build LocalBuild; DataDir string; releases releaseFetcher }
func (p Provisioner) Ensure(ctx context.Context) (remoteBinary, error)
type remoteBinary struct { shellPath string } // a shell word, e.g. "$HOME/.cache/agentsview/bin/v0.45.0/agentsview"
func probeScript(ids []string) (string, error)
func installScript(id string) (string, error)
func parseProbe(out []byte) (probeReport, error)
```

- [ ] **Step 1: Write failing tests**

`provision_test.go` (`//go:build unix`) uses `newFakeSSH`. A fake release
tarball holds a shell script named `agentsview` that prints a probe line, so
these tests never run a real binary:

```go
const fakeAgentsviewScript = `#!/bin/sh
[ "$1 $2" = "remote-sync probe" ] && printf '{"version":"%s","protocol_version":%s}\n' "$FAKE_AV_VERSION" "$FAKE_AV_PROTOCOL"
`
```

(`FAKE_AV_VERSION` and `FAKE_AV_PROTOCOL` must reach the remote side; add them
to the `env -i` list in `fakeSSHScript`.) Build the tarball in the test with
`archive/tar` and `compress/gzip`, serve it and its `SHA256SUMS` from
`httptest`, and point the provisioner's `releases.BaseURL` at the server.

Table of scenarios; each asserts the returned binary, the files under
`<fake home>/.cache/agentsview/bin/`, and how many ssh calls ran (lines in the
fake ssh log):

1. Release build, empty remote: probe, then download and install `v0.45.0`.
   `bin/v0.45.0/agentsview` exists and is executable, and
   `bin/v0.45.0/.last-used` exists. Two ssh calls.
1. Release build, `v0.45.0` already cached: one ssh call, no HTTP request (count
   server hits), `.last-used` mtime is refreshed (set it to 40 days ago first
   with `os.Chtimes` and assert it moved forward).
1. Development build (`Version: "dev"`, `Executable` is a temp file holding
   `fakeAgentsviewScript`), empty remote, platform equal to local: no HTTP
   request; installs under `ContentID()`; the installed file's bytes equal the
   executable's.
1. Two development builds with different bytes install two identities.
1. `agentsview` on the remote `PATH` with matching protocol (put
   `fakeAgentsviewScript` in a dir and include it in `FAKE_SSH_PATH`): one ssh
   call, binary is `agentsview`, nothing installed.
1. `PATH` binary with a different protocol: falls through to install.
1. Release 404 and remote platform equal to local: self-upload under the content
   identity.
1. Release 404 and a different remote platform: error mentions installing
   agentsview on the remote. Simulate the platform by putting a fake `uname`
   script first in `FAKE_SSH_PATH` that prints `FreeBSD` or `Linux`/`riscv64`.
1. Retention: before an install, create `bin/old-a/` with `.last-used` 40 days
   old, `bin/old-b/` with `.last-used` 1 day old, and `bin/.tmp-stale/` 2 days
   old and `bin/.tmp-fresh/` new (`os.Chtimes` on the directory). After
   install: `old-a` and `.tmp-stale` are gone; `old-b` and `.tmp-fresh`
   remain.
1. Install where the archive has no `agentsview` file: error contains "archive
   has no agentsview executable".
1. Install where the probe fails (fake script exits 1): error, and no
   `bin/<id>/agentsview` is left.
1. Two installs of the same identity at once: start two `installTarball` calls
   in goroutines against the same fake home and wait for both. Both return
   nil, `bin/<id>/agentsview` probes successfully, and no `.tmp-*` directory
   remains.

Also test that `probeScript` and `installScript` reject an identity outside
`identityPattern` and that both outputs pass `validateScript`.

- [ ] **Step 2: Run to verify failure**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run TestProvision`
Expected: compile failure, `Provisioner` undefined.

- [ ] **Step 3: Implement `provision.go`**

```go
package sshremote

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/remotesync"
)

// remoteBinDir is expanded by the remote shell.
const remoteBinDir = `$HOME/.cache/agentsview/bin`

type remoteBinary struct {
	// shellPath is a shell word naming the executable on the remote.
	shellPath string
}

func cachedBinary(id string) remoteBinary {
	return remoteBinary{shellPath: `"` + remoteBinDir + "/" + id + `/agentsview"`}
}

var pathBinary = remoteBinary{shellPath: "agentsview"}

func requireIdentity(id string) error {
	if !identityPattern.MatchString(id) {
		return fmt.Errorf("invalid remote binary identity %q", id)
	}
	return nil
}

// probeScript reports the platform, probes each cached identity (touching
// .last-used for every one that answers), then probes agentsview on PATH.
func probeScript(ids []string) (string, error) {
	for _, id := range ids {
		if err := requireIdentity(id); err != nil {
			return "", err
		}
	}
	script := strings.Join([]string{
		`bin="` + remoteBinDir + `"`,
		`printf "os=%s\n" "$(uname -s)"`,
		`printf "arch=%s\n" "$(uname -m)"`,
		`for id in ` + strings.Join(ids, " ") + `; do ` +
			`if [ -x "$bin/$id/agentsview" ] && out=$("$bin/$id/agentsview" remote-sync probe 2>/dev/null); then ` +
			`touch "$bin/$id/.last-used"; printf "cached %s %s\n" "$id" "$out"; fi; done`,
		`if command -v agentsview >/dev/null 2>&1 && out=$(agentsview remote-sync probe 2>/dev/null); then ` +
			`printf "path %s\n" "$out"; fi`,
	}, "; ")
	return script, validateScript(script)
}

// installScript reads a gzip tarball on stdin and installs its agentsview as
// identity id, then applies retention. Renaming the file (not a directory)
// keeps concurrent installs of one identity harmless.
func installScript(id string) (string, error) {
	if err := requireIdentity(id); err != nil {
		return "", err
	}
	script := strings.Join([]string{
		`set -eu`,
		`bin="` + remoteBinDir + `"`,
		`id="` + id + `"`,
		`mkdir -p "$bin"`,
		`tmp=$(mktemp -d "$bin/.tmp-XXXXXX")`,
		`trap "rm -rf \"$tmp\"" EXIT`,
		`tar -xzf - -C "$tmp"`,
		`if [ ! -f "$tmp/agentsview" ]; then echo "archive has no agentsview executable" >&2; exit 3; fi`,
		`chmod 755 "$tmp/agentsview"`,
		`"$tmp/agentsview" remote-sync probe >/dev/null`,
		`mkdir -p "$bin/$id"`,
		`mv -f "$tmp/agentsview" "$bin/$id/agentsview"`,
		`touch "$bin/$id/.last-used"`,
		`find "$bin" -mindepth 1 -maxdepth 1 -type d -name ".tmp-*" -mtime +0 -exec rm -rf {} +`,
		`for d in "$bin"/*; do [ -d "$d" ] || continue; [ "$d" = "$bin/$id" ] && continue; ` +
			`m="$d"; [ -f "$d/.last-used" ] && m="$d/.last-used"; ` +
			`if [ -n "$(find "$m" -prune -mtime +30)" ]; then rm -rf "$d"; fi; done`,
	}, "; ")
	return script, validateScript(script)
}

type probeReport struct {
	platform platform
	cached   map[string]ProbeResult
	path     *ProbeResult
}

// parseProbe reads the probe script's output. Unknown lines, such as text a
// shell start-up file printed, are ignored.
func parseProbe(out []byte) (probeReport, error) {
	report := probeReport{cached: map[string]ProbeResult{}}
	var unameS, unameM string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "os="):
			unameS = strings.TrimPrefix(line, "os=")
		case strings.HasPrefix(line, "arch="):
			unameM = strings.TrimPrefix(line, "arch=")
		case strings.HasPrefix(line, "cached "):
			id, payload, ok := strings.Cut(strings.TrimPrefix(line, "cached "), " ")
			var result ProbeResult
			if ok && json.Unmarshal([]byte(payload), &result) == nil {
				report.cached[id] = result
			}
		case strings.HasPrefix(line, "path "):
			var result ProbeResult
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "path ")), &result) == nil {
				report.path = &result
			}
		}
	}
	var err error
	report.platform, err = parsePlatform(unameS, unameM)
	return report, err
}
```

```go
// Provisioner finds or installs a usable agentsview on one remote host.
type Provisioner struct {
	Runner   Runner
	Build    LocalBuild
	DataDir  string
	releases releaseFetcher
}

// Ensure returns the remote binary to run for this sync.
func (p Provisioner) Ensure(ctx context.Context) (remoteBinary, error) {
	releaseID, isRelease := p.Build.ReleaseID()
	contentID, err := p.Build.ContentID()
	if err != nil {
		return remoteBinary{}, err
	}
	ids := []string{contentID}
	if isRelease {
		ids = []string{releaseID, contentID}
	}
	script, err := probeScript(ids)
	if err != nil {
		return remoteBinary{}, err
	}
	out, err := p.Runner.Output(ctx, script, nil)
	if err != nil {
		return remoteBinary{}, err
	}
	report, err := parseProbe(out)
	if err != nil {
		return remoteBinary{}, err
	}
	for _, id := range ids {
		if result, ok := report.cached[id]; ok && result.ProtocolVersion == remotesync.ProtocolVersion {
			return cachedBinary(id), nil
		}
	}
	if report.path != nil && report.path.ProtocolVersion == remotesync.ProtocolVersion {
		return pathBinary, nil
	}
	return p.install(ctx, report.platform, releaseID, isRelease, contentID)
}

func (p Provisioner) install(
	ctx context.Context, remote platform, releaseID string, isRelease bool, contentID string,
) (remoteBinary, error) {
	if isRelease {
		fetcher := p.releases
		if fetcher.CacheDir == "" {
			fetcher.CacheDir = filepath.Join(p.DataDir, "ssh-binaries")
		}
		tarball, err := fetcher.Fetch(ctx, p.Build.releaseVersion(), remote)
		switch {
		case err == nil:
			return p.installTarball(ctx, releaseID, tarball)
		case !errors.Is(err, errReleaseNotFound):
			return remoteBinary{}, err
		}
	}
	if remote != localPlatform() {
		return remoteBinary{}, fmt.Errorf(
			"no agentsview release matches this build (%s) for remote platform %s; "+
				"install agentsview on the remote or sync from a release build",
			p.Build.Version, remote)
	}
	return p.installSelf(ctx, contentID)
}
```

Write `installTarball(ctx, id, path)`: open the file, run
`p.Runner.Output(ctx, installScript(id), file)`, return `cachedBinary(id)`.
Write `installSelf(ctx, id)`: an `io.Pipe` whose writer goroutine writes a gzip
(`gzip.DefaultCompression`) tar with one entry `agentsview`, mode 0755, copied
from `p.Build.Executable`, closing the pipe with the goroutine's error; pass the
reader as stdin to the same install script.

- [ ] **Step 4: Run tests and commit**

```bash
go fmt ./... && go vet ./internal/sshremote/
CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run 'TestProvision|Script'
git add internal/sshremote
git commit -m "feat(sshremote): place a matching agentsview on the remote"
```

______________________________________________________________________

### Task 10: SSH source and dispatch from configured hosts

**Files:**

- Create: `internal/sshremote/source.go`, `internal/sshremote/source_test.go`,
  `internal/remotesource/remotesource.go`,
  `internal/remotesource/remotesource_test.go`
- Modify: `cmd/agentsview/sync.go:438-537` (transport check,
  `runHTTPRemoteSync`), `:689-721` (`prepareConfiguredHTTPHosts`),
  `internal/server/huma_routes_sync.go:81-111` (`runHTTPRemoteSync`),
  `:810-838` (`prepareHTTPHosts`), `:925-960` (log text)
- Test: `cmd/agentsview/sync_test.go`,
  `internal/server/huma_routes_sync_internal_test.go`

**Interfaces:**

- Consumes: `Provisioner`, `Runner`, `serveRequest`, `remotesync.NewZstdReader`,
  `config.RemoteHost.SSHDestination`.
- Produces:

```go
func NewSource(p Provisioner) *Source // *Source implements remotesync.Source
func serveScript(bin remoteBinary, op Op) (string, error)
type remotesource.Options struct { Version string; DataDir string }
func remotesource.For(rh config.RemoteHost, opts Options) (remotesync.Source, error)
```

`runHTTPRemoteSync` becomes `runRemoteHostSync` and `prepareHTTPHosts` /
`prepareConfiguredHTTPHosts` become `prepareRemoteHosts` /
`prepareConfiguredRemoteHosts` in both packages (update test references with
`perl -pi -e 's/\brunHTTPRemoteSync\b/runRemoteHostSync/g; …'`).

- [ ] **Step 1: Write the failing end-to-end test**

`internal/sshremote/source_test.go` (`//go:build unix`). The fake ssh runs
commands locally; the remote binary is the test binary itself (see `TestMain`),
placed in the fake home's cache so `Ensure` finds it:

```go
func TestSSHSourceSyncsRemoteHome(t *testing.T) {
	ssh := newFakeSSH(t)
	sessionPath := filepath.Join(ssh.Home, ".claude", "projects", "project-a", "session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte(
		testjsonl.NewSessionBuilder().AddClaudeUser("2024-01-01T00:00:00Z", "over ssh").String(),
	), 0o644))
	build := LocalBuild{Version: "dev", Executable: os.Args[0]}
	installTestBinary(t, ssh.Home, build) // hard-links os.Args[0] into bin/<ContentID>/agentsview
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	dataDir := t.TempDir()

	stats, err := remotesync.RemoteSync{
		Host:    "devbox",
		DataDir: dataDir,
		DB:      database,
		Source: NewSource(Provisioner{
			Runner:  Runner{Destination: "user@devbox.example.test", SSHPath: ssh.Path},
			Build:   build,
			DataDir: dataDir,
		}),
	}.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	page, err := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	session := page.Sessions[0]
	assert.True(t, strings.HasPrefix(session.ID, "devbox~"), session.ID)
	assert.Equal(t, "devbox", session.Machine)
	require.NotNil(t, session.FilePath)
	assert.Equal(t, "devbox:"+sessionPath, *session.FilePath)
	assert.Equal(t, []string{"user@devbox.example.test"}, uniqueLines(t, ssh.Log))
}
```

Add in the same file:

- A second `Run` after appending a line to the session: the archive request is a
  delta (wrap the source to record `ArchiveRequest.DeltaFiles != nil`) and the
  session's message count grows.

- Deleting the session file on the remote then syncing: the mirror copy is
  removed and the archived session is kept (`GetSession` still returns it).

- A remote whose shell prints a line before the command (wrap the fake ssh so it
  runs `echo banner; <command>`): `Run` fails with an error containing "shell
  start-up".

- One test that runs the self-upload path end to end: no pre-installed binary,
  `Build.Executable` is `os.Args[0]`; `Run` succeeds and
  `bin/<ContentID>/agentsview` exists.

- Statelessness of a full sync: compare `treeState(ssh.Home)` before and after,
  ignoring only `.cache/agentsview/bin`.

- [ ] **Step 2: Run to verify failure**

Run: `CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ -run TestSSHSource`
Expected: compile failure, `NewSource` undefined.

- [ ] **Step 3: Implement `source.go`**

```go
package sshremote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"sync"

	"go.kenn.io/agentsview/internal/remotesync"
)

// zstdMagic starts every zstd frame. A response that starts otherwise was
// corrupted by output from the remote shell.
var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

var errShellOutput = errors.New(
	"the remote shell printed output before agentsview ran; make shell start-up " +
		"files quiet for non-interactive sessions, as scp and rsync also require")

// Source fetches remote sync data by running agentsview over SSH.
type Source struct {
	provisioner Provisioner
	once        sync.Once
	binary      remoteBinary
	ensureErr   error
}

// NewSource returns a Source for one sync run.
func NewSource(p Provisioner) *Source { return &Source{provisioner: p} }

func (s *Source) ensure(ctx context.Context) (remoteBinary, error) {
	s.once.Do(func() { s.binary, s.ensureErr = s.provisioner.Ensure(ctx) })
	return s.binary, s.ensureErr
}

func serveScript(bin remoteBinary, op Op) (string, error) {
	if _, err := ParseOp(string(op)); err != nil {
		return "", err
	}
	script := "exec " + bin.shellPath + " remote-sync serve --op " + string(op)
	return script, validateScript(script)
}

func (s *Source) start(ctx context.Context, op Op, req serveRequest) (io.ReadCloser, error) {
	bin, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	script, err := serveScript(bin, op)
	if err != nil {
		return nil, err
	}
	req.ProtocolVersion = remotesync.ProtocolVersion
	var body bytes.Buffer
	if err := json.MarshalWrite(&body, req); err != nil {
		return nil, fmt.Errorf("encode %s request: %w", op, err)
	}
	stream, err := s.provisioner.Runner.Stream(ctx, script, &body)
	if err != nil {
		return nil, err
	}
	return newZstdChecked(stream), nil
}

func (s *Source) call(ctx context.Context, op Op, req serveRequest, out any) error {
	stream, err := s.start(ctx, op, req)
	if err != nil {
		return err
	}
	decodeErr := decodeZstdJSON(stream, out)
	if closeErr := stream.Close(); closeErr != nil {
		return closeErr
	}
	if decodeErr != nil {
		return fmt.Errorf("decode remote %s response: %w", op, decodeErr)
	}
	return nil
}

// Targets implements remotesync.Source.
func (s *Source) Targets(ctx context.Context) (remotesync.TargetSet, error) {
	var targets remotesync.TargetSet
	err := s.call(ctx, OpTargets, serveRequest{}, &targets)
	return targets, err
}

// Manifest implements remotesync.Source.
func (s *Source) Manifest(ctx context.Context, targets remotesync.TargetSet) (remotesync.Manifest, bool, error) {
	var manifest remotesync.Manifest
	err := s.call(ctx, OpManifest, serveRequest{Targets: &targets}, &manifest)
	if cmdErr, ok := errors.AsType[*CommandError](err); ok &&
		cmdErr.Remote != nil && cmdErr.Remote.Code == CodeUnsupported {
		return remotesync.Manifest{}, false, nil
	}
	return manifest, err == nil, err
}

// Archive implements remotesync.Source.
func (s *Source) Archive(ctx context.Context, request remotesync.ArchiveRequest) (*remotesync.ArchiveStream, error) {
	stream, err := s.start(ctx, OpArchive, serveRequest{Archive: &request})
	if err != nil {
		return nil, err
	}
	return &remotesync.ArchiveStream{Body: stream, Encoding: remotesync.ArchiveEncodingZstd}, nil
}
```

Write `newZstdChecked(rc io.ReadCloser) io.ReadCloser`: wraps the stream in a
`bufio.Reader`; the first `Read` peeks 4 bytes and returns `errShellOutput` when
there are at least 4 bytes and they differ from `zstdMagic` (an empty stream is
passed through, so `Close` reports the remote's error). `Close` delegates. Write
`decodeZstdJSON(r io.Reader, out any) error` using `remotesync.NewZstdReader`
and `json.UnmarshalRead`. For the archive path, the check runs when
`downloadArchive` copies the body, so the shell-output error surfaces before
extraction.

- [ ] **Step 4: Implement `remotesource.For`**

`internal/remotesource/remotesource.go`:

```go
// Package remotesource picks the transport for a configured remote host.
package remotesource

import (
	"fmt"
	"os"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/remotesync"
	"go.kenn.io/agentsview/internal/sshremote"
)

// Options describe the local collector.
type Options struct {
	// Version is the collector's build version.
	Version string
	// DataDir holds the downloaded release cache for SSH hosts.
	DataDir string
}

// For returns a new Source for one sync of rh.
func For(rh config.RemoteHost, opts Options) (remotesync.Source, error) {
	switch rh.Transport {
	case "", config.RemoteTransportHTTP:
		if rh.Token == "" {
			return nil, fmt.Errorf("http remote sync token is required for host %q", rh.Host)
		}
		return remotesync.HTTPSource{URL: rh.URL, Token: rh.Token}, nil
	case config.RemoteTransportSSH:
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate agentsview executable: %w", err)
		}
		return sshremote.NewSource(sshremote.Provisioner{
			Runner:  sshremote.Runner{Destination: rh.SSHDestination()},
			Build:   sshremote.LocalBuild{Version: opts.Version, Executable: exe},
			DataDir: opts.DataDir,
		}), nil
	default:
		return nil, fmt.Errorf("invalid remote transport %q for host %q; use http or ssh", rh.Transport, rh.Host)
	}
}
```

`remotesource_test.go`: table test over HTTP with token, HTTP without token
(error), SSH (returns `*sshremote.Source`), and an unknown transport (error).

- [ ] **Step 5: Dispatch from the CLI and the daemon**

`cmd/agentsview/sync.go`:

- In `runRemoteSyncTransportWithCleanup`, delete the transport check at `:446`;
  `remotesource.For` now rejects unknown transports.
- Rename `runHTTPRemoteSync` to `runRemoteHostSync`. Replace its token check and
  `Source` field with:

```go
	source, err := remotesource.For(rh, remotesource.Options{Version: version, DataDir: appCfg.DataDir})
	if err != nil {
		return remotesync.SyncStats{}, err
	}
```

and `Source: source` in the `RemoteSync` literal.

- In `prepareConfiguredHTTPHosts` (rename to `prepareConfiguredRemoteHosts`),
  build each `Source` with `remotesource.For` the same way; return a
  `*remotesync.HostError{Host: host.Host, Operation: "configure", Err: err}`
  on error.

`internal/server/huma_routes_sync.go`:

- Rename `runHTTPRemoteSync` to `runRemoteHostSync` and use
  `remotesource.For(rh, remotesource.Options{Version: s.version.Version, DataDir: cfg.DataDir})`.
  The variable is package-level, so pass the version in: change its
  signature to take `version string` after `cfg`, and update callers and test
  stubs.
- In `prepareHTTPHosts` (rename to `prepareRemoteHosts`), replace the token loop
  with `remotesource.For` per host, mapping errors to
  `&remotesync.HostError{Host: host.Host, Operation: "configure", Err: err}`.
  Add the same `version string` parameter.
- Replace the hard-coded `transport=http` in the host log lines at `:933` and
  `:950` with `transport=%s` and `remoteTransportLabel(rh)`:

```go
func remoteTransportLabel(rh config.RemoteHost) string {
	if rh.Transport == "" {
		return string(config.RemoteTransportHTTP)
	}
	return string(rh.Transport)
}
```

- [ ] **Step 6: Add dispatch tests**

In `cmd/agentsview/sync_test.go`, add a test that a configured SSH host reaches
`runRemoteSyncTransportWithCleanup` and is passed to `runRemoteHostSync` (stub
the variable, as existing tests do for `runHTTPRemoteSync`), and that an unknown
transport fails before sync starts. In
`internal/server/huma_routes_sync_internal_test.go`, add a test that
`POST /api/v1/sync/remotes` with a configured SSH host calls the stubbed
`runRemoteHostSync` with the stored entry (including `Destination`), and that
`prepareRemoteHosts` builds an SSH source without requiring a token. Find the
existing stub pattern with `rg -n "runHTTPRemoteSync = " internal/server cmd`
before renaming.

- [ ] **Step 7: Run tests**

```bash
go build ./... && go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/sshremote/ ./internal/remotesource/ ./internal/remotesync/ ./internal/server/ ./cmd/agentsview/
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
go fmt ./...
git add internal cmd
git commit -m "feat(sync): sync ssh remote hosts through the shared pipeline"
```

______________________________________________________________________

### Task 11: Documentation, changelog, and security notes

**Files:**

- Modify: `docs/remote-access.md` (add "SSH Remote Sync" beside "HTTP Remote
  Sync" at `:87`), `docs/configuration.md` ("Remote Hosts" at `:273-320`),
  `docs/commands.md` (`sync --host`), `SECURITY.md`, `docs/changelog.md`,
  `cmd/agentsview/cli.go` (sync help text that lists `remote_hosts` fields;
  find it with `rg -n "transport" cmd/agentsview/cli.go`)

Follow `docs/README.md` for publishing layout and checks, and the Documentation
rules in `AGENTS.md`.

- [ ] **Step 1: `docs/remote-access.md`**

Add an "SSH Remote Sync" section covering, in this order:

1. What it does: sync a host you can reach with `ssh` without installing or
   running agentsview there.
1. Setup: the `[[remote_hosts]]` example from the spec, `destination` defaulting
   to `host`, and that keys must work without a prompt (`BatchMode=yes`). Show
   the user how to check: `ssh -o BatchMode=yes <destination> true`.
1. What lands on the remote: `~/.cache/agentsview/bin/<id>/agentsview`, how
   identities are named, the 30-day idle retention, and that nothing else is
   written. Each sync runs one short process per step; nothing keeps running.
1. Where the binary comes from: the verified release tarball for your version, a
   compatible `agentsview` already on the remote's `PATH`, or, for a
   development build, your own executable when the platforms match.
1. Daemon note: a daemon started by a service manager may not see your SSH
   agent; configure `IdentityAgent` or a key without a passphrase.
1. Offline hosts: which failures are skipped and which are reported.
1. Limits, copied from the spec's Limits section.

Update the line that says HTTP is the only transport. Keep "Incremental Sync" as
the owner of mirror and delta details and link to it.

- [ ] **Step 2: `docs/configuration.md`**

Replace the sentence that says `"ssh"` is rejected. Document `transport` values
`http` and `ssh`, the new `destination` field, and the rejections from Task 5
(`url`/`token` on ssh entries, `destination` on http entries, a leading `-`).

- [ ] **Step 3: `docs/commands.md` and CLI help**

Say that `sync --host NAME` runs SSH entries the same way as HTTP entries.
Update the `sync` help text in `cli.go` to list `transport = "ssh"` and
`destination`.

- [ ] **Step 4: `SECURITY.md`**

Add an SSH remote sync section: agentsview uses the system `ssh` client and your
ssh config; it downloads release tarballs over HTTPS and checks them against
`SHA256SUMS` from the same release; the remote binary lives under the remote
user's `~/.cache/agentsview/bin`; the remote command reads only what target
resolution allows and writes nothing else. State that `SHA256SUMS.sig` is not
checked.

- [ ] **Step 5: `docs/changelog.md`**

Under the unreleased section, lead with the outcome:

> You can sync a machine you reach over SSH without installing agentsview on it.
> Add a `[[remote_hosts]]` entry with `transport = "ssh"`. agentsview places a
> matching copy of itself in the remote's `~/.cache/agentsview/bin` and runs it
> only while a sync is in progress. Transfers are compressed with zstd and send
> only changed files after the first sync.

- [ ] **Step 6: Check and commit**

```bash
command -v mdformat >/dev/null && mdformat --wrap 80 docs/remote-access.md docs/configuration.md docs/commands.md SECURITY.md docs/changelog.md
bash scripts/check-docs.sh
go build ./cmd/agentsview && CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run Help
git add docs SECURITY.md cmd/agentsview/cli.go
git commit -m "docs: explain ssh remote sync setup, footprint, and limits"
```

If `scripts/check-docs.sh` needs arguments or tools that are missing, read it
and run the checks it performs; report any check that could not run.

______________________________________________________________________

## Final verification

Run before opening the pull request:

```bash
go fmt ./... && go vet ./...
make lint
CGO_ENABLED=1 go test -tags fts5 ./...
```

Then do one real SSH sync against a disposable host or container you control,
with an isolated data directory (`AGENTSVIEW_DATA_DIR=<scratch>`), per the
`isolate-prod` rule in `AGENTS.md`. Record what you observed: the upload, the
second sync's delta size, the files under `~/.cache/agentsview/bin` on the
remote, and that no agentsview process remains on the remote afterwards
(`ssh <host> pgrep -fl agentsview`).

Write the pull request description from the current diff. Include the zstd
benchmark table from Task 6.
