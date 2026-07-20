package remotesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// backgroundWaitTimeout bounds waits on background goroutines that are
// expected to finish promptly. It is generous because cold windows-latest
// runners can stall a full resync for several seconds; the timeout only
// delays failure output when the code under test genuinely hangs.
const backgroundWaitTimeout = 30 * time.Second

func TestHTTPSyncDownloadsArchiveAndImports(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/wes/.claude/projects/test-project/session.jsonl": testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "http remote").
			String(),
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer remote-token", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"dirs":{"claude":["/home/wes/.claude/projects"]}}`))
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })

	stats, err := HTTPSync{
		Host:  "devbox",
		URL:   ts.URL,
		Token: "remote-token",
		DB:    database,
	}.Run(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
}

func TestHTTPSyncReportsDownloadAndImportProgress(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/wes/.claude/projects/test-project/session.jsonl": testjsonl.NewSessionBuilder().
			AddClaudeUser("2024-01-01T00:00:00Z", "http remote progress").
			String(),
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"dirs":{"claude":["/home/wes/.claude/projects"]}}`))
		case "/api/v1/remote-sync/archive":
			w.Header().Set("Content-Type", "application/x-tar")
			w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	var progress []syncpkg.Progress

	stats, err := HTTPSync{
		Host:  "devbox",
		URL:   ts.URL,
		Token: "remote-token",
		DB:    database,
		Progress: func(p syncpkg.Progress) {
			progress = append(progress, p)
		},
	}.Run(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Contains(t, progressDetails(progress),
		"Resolving agent directories on devbox")
	assert.Contains(t, progressDetails(progress),
		"Downloading session archive from devbox")
	assert.Contains(t, progressDetails(progress),
		"Extracting session archive from devbox")
	assert.Contains(t, progressDetails(progress),
		"Processing sessions from devbox")
	require.NotEmpty(t, progress, "expected progress events")
	assert.Equal(t, int64(len(archive)), maxBytesDone(progress))
	assert.Equal(t, int64(len(archive)), maxBytesTotal(progress))
}

func TestHTTPSyncReportsCompressedTransferThenExtraction(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/user/.claude/projects/test/session.jsonl": "compressed archive",
	})
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write(archive)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/remote-sync/archive", r.URL.Path)
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	}))
	t.Cleanup(ts.Close)

	var progress []syncpkg.Progress
	hs := HTTPSync{
		Host: "devbox",
		URL:  ts.URL,
		Progress: func(p syncpkg.Progress) {
			progress = append(progress, p)
		},
	}
	mirrorRoot := filepath.Join(t.TempDir(), "remote-mirrors", "devbox")
	err = hs.downloadIntoMirror(
		context.Background(), ts.Client(), TargetSet{}, []string{"session.jsonl"},
		false, mirrorRoot,
	)
	require.NoError(t, err)

	downloadLabel := "Downloading 1 changed files from devbox"
	extractLabel := "Extracting 1 changed files from devbox"
	assert.Equal(t, []string{downloadLabel, extractLabel},
		uniqueProgressDetails(progress))
	require.NotEmpty(t, progress)
	assert.Equal(t, int64(compressed.Len()), maxBytesDone(progress))
	assert.Equal(t, int64(compressed.Len()), maxBytesTotal(progress))
}

func TestPrepareHTTPSyncReportsManifestComparisonBeforeTransfer(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
	_, hs := newMirrorSync(t, remote, t.TempDir())
	var progress []syncpkg.Progress
	hs.Progress = func(p syncpkg.Progress) {
		progress = append(progress, p)
	}

	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })

	assert.Equal(t, []string{
		"Resolving agent directories on devbox",
		"Fetching session manifest from devbox",
		"Compared session manifest from devbox: 1 total, 1 changed, 0 deleted",
		"Downloading session archive from devbox",
		"Extracting session archive from devbox",
	}, uniqueProgressDetails(progress))
}

func TestHTTPSyncLegacyCompressedTransferPreservesWireProgress(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/user/.claude/projects/test/session.jsonl": "legacy compressed",
	})
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write(archive)
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "gzip", r.Header.Get("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	}))
	t.Cleanup(ts.Close)

	var progress []syncpkg.Progress
	hs := HTTPSync{
		Host: "devbox",
		URL:  ts.URL,
		Progress: func(p syncpkg.Progress) {
			progress = append(progress, p)
		},
	}
	root, err := hs.downloadAndExtract(
		context.Background(), ts.Client(), TargetSet{},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	body, err := os.ReadFile(filepath.Join(
		root, "home/user/.claude/projects/test/session.jsonl",
	))
	require.NoError(t, err)
	assert.Equal(t, "legacy compressed", string(body))
	assert.Equal(t, []string{
		"Downloading session archive from devbox",
		"Extracting session archive from devbox",
	}, uniqueProgressDetails(progress))
	assert.Equal(t, int64(compressed.Len()), maxBytesDone(progress))
	assert.Equal(t, int64(compressed.Len()), maxBytesTotal(progress))
}

func TestHTTPSyncRemovesSpooledArchive(t *testing.T) {
	tests := []struct {
		name          string
		archive       []byte
		contentLength int
		status        int
		wantErr       bool
	}{
		{
			name: "after successful extraction",
			archive: buildHTTPTestTar(t, map[string]string{
				"home/user/.claude/projects/test/session.jsonl": "complete",
			}),
		},
		{
			name: "after extraction failure",
			archive: tarWithoutEndMarker(t,
				"home/user/.claude/projects/test/session.jsonl", "truncated"),
			wantErr: true,
		},
		{
			name: "after response body read failure",
			archive: buildHTTPTestTar(t, map[string]string{
				"home/user/.claude/projects/test/session.jsonl": "incomplete response",
			}),
			contentLength: 1 << 20,
			wantErr:       true,
		},
		{
			name:          "after non-success response body read failure",
			archive:       []byte("upstream failure"),
			contentLength: 1 << 20,
			status:        http.StatusBadGateway,
			wantErr:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.contentLength > 0 {
					w.Header().Set("Content-Length", strconv.Itoa(tt.contentLength))
				}
				if tt.status > 0 {
					w.WriteHeader(tt.status)
				}
				_, _ = w.Write(tt.archive)
			}))
			t.Cleanup(ts.Close)
			parent := filepath.Join(t.TempDir(), "remote-mirrors")
			mirrorRoot := filepath.Join(parent, "devbox")

			err := (HTTPSync{Host: "devbox", URL: ts.URL}).downloadIntoMirror(
				context.Background(), ts.Client(), TargetSet{}, []string{"session.jsonl"},
				false, mirrorRoot,
			)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.status > 0 {
				var statusErr *StatusError
				require.ErrorAs(t, err, &statusErr)
				assert.Equal(t, tt.status, statusErr.Code)
			}
			entries, readErr := os.ReadDir(parent)
			if errors.Is(readErr, os.ErrNotExist) {
				return
			}
			require.NoError(t, readErr)
			for _, entry := range entries {
				assert.NotContains(t, entry.Name(), "agentsview-http-archive-",
					"owned spool artifacts must be removed")
			}
		})
	}
}

func TestHTTPSyncMirrorRetainsPostExtractionSpoolCleanup(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/user/session.jsonl": "complete",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(ts.Close)
	removeErr := errors.New("spool removal failed")
	removeCalls := 0
	hs := HTTPSync{Host: "devbox", URL: ts.URL, removeArchiveSpool: func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return removeErr
		}
		return os.RemoveAll(path)
	}}
	mirrorRoot := filepath.Join(t.TempDir(), "mirrors", "devbox")

	err := hs.downloadIntoMirror(
		context.Background(), ts.Client(), TargetSet{}, []string{"session.jsonl"},
		false, mirrorRoot,
	)

	require.ErrorIs(t, err, removeErr)
	var owner *downloadedArchiveCleanupError
	require.ErrorAs(t, err, &owner,
		"post-extraction cleanup must remain retryable")
	assert.FileExists(t, filepath.Join(mirrorRoot, "home/user/session.jsonl"))
	require.NoError(t, owner.RetryCleanup())
	assert.Equal(t, 2, removeCalls)
}

func TestHTTPSyncLegacyRetainsCleanupWithoutLeakingExtractedRoot(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/user/session.jsonl": "complete",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	}))
	t.Cleanup(ts.Close)
	tempParent := t.TempDir()
	setPortableTempDir(t, tempParent)
	removeErr := errors.New("spool removal failed")
	removeCalls := 0
	hs := HTTPSync{Host: "devbox", URL: ts.URL, removeArchiveSpool: func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return removeErr
		}
		return os.RemoveAll(path)
	}}

	root, err := hs.downloadAndExtract(
		context.Background(), ts.Client(), TargetSet{},
	)

	assert.Empty(t, root, "failed cleanup must not transfer root ownership")
	require.ErrorIs(t, err, removeErr)
	var owner *downloadedArchiveCleanupError
	require.ErrorAs(t, err, &owner,
		"post-extraction cleanup must remain retryable")
	require.NoError(t, owner.RetryCleanup())
	entries, readErr := os.ReadDir(tempParent)
	require.NoError(t, readErr)
	assert.Empty(t, entries,
		"retry must release both the spool and extracted legacy root")
}

func TestHTTPSyncLegacyRetainsSpoolWhenExtractionRootCreationFails(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/user/session.jsonl": "complete",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
		_, _ = w.Write(archive)
	}))
	t.Cleanup(ts.Close)
	validTemp := t.TempDir()
	setPortableTempDir(t, validTemp)
	invalidTemp := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(invalidTemp, []byte("block"), 0o600))
	removeErr := errors.New("spool cleanup failed")
	removeCalls := 0
	hs := HTTPSync{
		Host: "devbox",
		URL:  ts.URL,
		Progress: func(p syncpkg.Progress) {
			if p.BytesDone == int64(len(archive)) {
				for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
					require.NoError(t, os.Setenv(name, invalidTemp))
				}
			}
		},
		removeArchiveSpool: func(string) error {
			removeCalls++
			if removeCalls == 1 {
				return removeErr
			}
			return nil
		},
	}

	root, err := hs.downloadAndExtract(
		context.Background(), ts.Client(), TargetSet{},
	)

	assert.Empty(t, root)
	require.ErrorContains(t, err, "create temp dir")
	require.ErrorIs(t, err, removeErr)
	var owner *downloadedArchiveCleanupError
	require.ErrorAs(t, err, &owner,
		"failed cleanup must retain the downloaded spool")
	require.NoError(t, owner.RetryCleanup())
	assert.Equal(t, 2, removeCalls)
}

func TestHTTPSyncRemovesSpooledArchiveOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	parent := filepath.Join(t.TempDir(), "remote-mirrors")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       &cancelEOFBody{ctx: ctx},
		Header:     make(http.Header),
	}
	hs := HTTPSync{Progress: func(p syncpkg.Progress) {
		if p.BytesDone > 0 {
			cancel()
		}
	}}

	archive, err := hs.downloadArchive(ctx, resp, "Downloading", parent)
	if archive != nil {
		t.Cleanup(func() { require.NoError(t, archive.Close()) })
	}
	require.ErrorIs(t, err, context.Canceled)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	assert.Empty(t, entries)
}

func TestDownloadArchiveCloseFailureRemovesIsolatedSpool(t *testing.T) {
	closeErr := errors.New("close response body")
	parent := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "existing"), []byte("keep"), 0o644,
	))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body: &closeErrorBody{
			Reader: bytes.NewReader([]byte("wire archive")),
			err:    closeErr,
		},
		Header: make(http.Header),
	}

	archive, err := (HTTPSync{}).downloadArchive(
		context.Background(), resp, "Downloading", parent,
	)

	assert.Nil(t, archive)
	require.ErrorIs(t, err, closeErr)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	require.Len(t, entries, 1)
	assert.Equal(t, "existing", entries[0].Name())
}

func TestDownloadedArchiveUsesIsolatedSpoolOutsideMirror(t *testing.T) {
	parent := t.TempDir()
	mirrorRoot := filepath.Join(parent, "devbox")
	require.NoError(t, os.Mkdir(mirrorRoot, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(parent, "existing"), []byte("keep"), 0o644,
	))
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader([]byte("wire archive"))),
		Header:     make(http.Header),
	}

	archive, err := (HTTPSync{}).downloadArchive(
		context.Background(), resp, "Downloading", parent,
	)
	require.NoError(t, err)
	require.NotNil(t, archive)
	t.Cleanup(func() { require.NoError(t, archive.Close()) })

	tempDir := filepath.Dir(archive.path)
	assert.DirExists(t, tempDir)
	assert.FileExists(t, archive.path)
	assert.Equal(t, parent, filepath.Dir(tempDir))
	assert.NotEqual(t, parent, tempDir)
	assert.False(t, within(mirrorRoot, archive.path),
		"spool must not be created inside the manifest mirror")
	require.NoError(t, archive.Close())
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	require.Len(t, entries, 2)
	assert.Equal(t, []string{"devbox", "existing"},
		[]string{entries[0].Name(), entries[1].Name()})
}

func TestDownloadedArchiveExtractionCancellationMidEntry(t *testing.T) {
	body := make([]byte, 32<<10)
	state := uint32(1)
	for i := range body {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		body[i] = byte(state)
	}
	archiveBytes := buildHTTPTestTar(t, map[string]string{
		"home/user/large-session.jsonl": string(body),
	})
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write(archiveBytes)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	require.Greater(t, compressed.Len(), 1024)

	tests := []struct {
		name       string
		wire       []byte
		compressed bool
	}{
		{name: "plain", wire: archiveBytes},
		{name: "gzip", wire: compressed.Bytes(), compressed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spoolParent := t.TempDir()
			spoolDir := filepath.Join(spoolParent, "agentsview-http-archive-test")
			require.NoError(t, os.Mkdir(spoolDir, 0o700))
			require.NoError(t, os.WriteFile(
				filepath.Join(spoolDir, "owned"), []byte("spool"), 0o600,
			))
			reached := make(chan struct{})
			release := make(chan struct{})
			reader := &gatedArchiveReader{
				r:         bytes.NewReader(tt.wire),
				gateAfter: 768,
				reached:   reached,
				release:   release,
			}
			archive := &downloadedArchive{
				dir:        spoolDir,
				path:       filepath.Join(spoolDir, "archive"),
				compressed: tt.compressed,
				open:       func() (io.ReadCloser, error) { return reader, nil },
			}
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			dst := filepath.Join(t.TempDir(), "extracted")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			released := false
			t.Cleanup(func() {
				if !released {
					close(release)
				}
			})
			errCh := make(chan error, 1)
			go func() {
				errCh <- archive.extract(ctx, dst, nil, "Extracting")
			}()

			select {
			case <-reached:
			case <-time.After(backgroundWaitTimeout):
				require.FailNow(t, "timed out waiting for extraction to enter file body")
			}
			cancel()
			close(release)
			released = true
			select {
			case err := <-errCh:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(backgroundWaitTimeout):
				require.FailNow(t, "timed out waiting for canceled extraction")
			}
			assert.NoFileExists(t,
				filepath.Join(dst, "home/user/large-session.jsonl"))
			require.NoError(t, archive.Close())
			assert.NoDirExists(t, spoolDir)
		})
	}
}

func TestPrepareHTTPSyncCancellationReleasesMirrorOwnership(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "large.jsonl",
		time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
		strings.Repeat("entry\n", 4096))
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	ctx, cancel := context.WithCancel(context.Background())
	extractionStarted := false
	hs.Progress = func(p syncpkg.Progress) {
		if strings.HasPrefix(p.Detail, "Extracting ") {
			extractionStarted = true
			cancel()
		}
	}

	prepared, err := hs.Prepare(ctx)

	assert.Nil(t, prepared)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, extractionStarted)
	mirrorRoot := MirrorDir(dataDir, "devbox")
	assertMirrorUnlocked(t, mirrorRoot)
	entries, readErr := os.ReadDir(filepath.Dir(mirrorRoot))
	require.NoError(t, readErr)
	for _, entry := range entries {
		assert.NotContains(t, entry.Name(), "agentsview-http-archive-",
			"canceled preparation must release its spool ownership")
	}
}

func TestHTTPSyncLegacyCancellationCleansOwnedRoots(t *testing.T) {
	archive := buildHTTPTestTar(t, map[string]string{
		"home/user/large-session.jsonl": strings.Repeat("entry\n", 4096),
	})
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write(archive)
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		_, _ = w.Write(compressed.Bytes())
	}))
	t.Cleanup(ts.Close)
	tempParent := t.TempDir()
	setPortableTempDir(t, tempParent)
	ctx, cancel := context.WithCancel(context.Background())
	hs := HTTPSync{
		Host: "devbox",
		URL:  ts.URL,
		Progress: func(p syncpkg.Progress) {
			if strings.HasPrefix(p.Detail, "Extracting ") {
				cancel()
			}
		},
	}

	root, err := hs.downloadAndExtract(ctx, ts.Client(), TargetSet{})

	assert.Empty(t, root)
	require.ErrorIs(t, err, context.Canceled)
	entries, readErr := os.ReadDir(tempParent)
	require.NoError(t, readErr)
	assert.Empty(t, entries,
		"canceled legacy extraction must remove its spool and extraction root")
}

type cancelEOFBody struct {
	ctx  context.Context
	sent bool
}

func (b *cancelEOFBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial response"), nil
	}
	<-b.ctx.Done()
	return 0, io.EOF
}

func (*cancelEOFBody) Close() error { return nil }

type closeErrorBody struct {
	io.Reader
	err error
}

func (b *closeErrorBody) Close() error { return b.err }

type gatedArchiveReader struct {
	r         *bytes.Reader
	gateAfter int
	read      int
	reached   chan struct{}
	release   chan struct{}
	gated     bool
}

func (r *gatedArchiveReader) Read(p []byte) (int, error) {
	if r.read >= r.gateAfter && !r.gated {
		r.gated = true
		close(r.reached)
		<-r.release
	}
	if len(p) > 256 {
		p = p[:256]
	}
	n, err := r.r.Read(p)
	r.read += n
	return n, err
}

func (*gatedArchiveReader) Close() error { return nil }

func progressDetails(progress []syncpkg.Progress) []string {
	out := make([]string, 0, len(progress))
	for _, p := range progress {
		if p.Detail != "" {
			out = append(out, p.Detail)
		}
	}
	return out
}

func uniqueProgressDetails(progress []syncpkg.Progress) []string {
	var out []string
	for _, detail := range progressDetails(progress) {
		if len(out) == 0 || out[len(out)-1] != detail {
			out = append(out, detail)
		}
	}
	return out
}

func maxBytesDone(progress []syncpkg.Progress) int64 {
	var max int64
	for _, p := range progress {
		if p.BytesDone > max {
			max = p.BytesDone
		}
	}
	return max
}

func maxBytesTotal(progress []syncpkg.Progress) int64 {
	var max int64
	for _, p := range progress {
		if p.BytesTotal > max {
			max = p.BytesTotal
		}
	}
	return max
}

func buildHTTPTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	mtime := time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)
	for name, body := range files {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0o644,
			Size:    int64(len(body)),
			ModTime: mtime,
		}
		require.NoError(t, tw.WriteHeader(hdr))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// mirrorTestRemote is a fake remote daemon backed by a real directory
// tree, serving targets/manifest/archive with the same package
// functions the real server uses.
type mirrorTestRemote struct {
	dir               string // remote-side agent dir (absolute)
	targets           TargetSet
	archiveRequests   []ArchiveRequest
	archiveBody       []byte
	onArchive         func(ArchiveRequest)
	manifestStatus    int    // 0 = serve manifest; else respond with this status
	manifestHTML      bool   // true = mimic an old daemon's SPA catch-all
	onManifest        func() // called before serving a manifest response
	onManifestRequest func(*http.Request)
	rejectDelta       bool
	ts                *httptest.Server
}

func newMirrorTestRemote(t *testing.T) *mirrorTestRemote {
	t.Helper()
	remote := &mirrorTestRemote{
		dir: filepath.Join(t.TempDir(), "claude-projects"),
	}
	require.NoError(t, os.MkdirAll(remote.dir, 0o755))
	remote.targets = TargetSet{
		Dirs: map[parser.AgentType][]string{parser.AgentClaude: {remote.dir}},
	}
	remote.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/remote-sync/targets":
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(remote.targets))
		case "/api/v1/remote-sync/manifest":
			if remote.onManifest != nil {
				remote.onManifest()
			}
			if remote.onManifestRequest != nil {
				remote.onManifestRequest(r)
			}
			if remote.manifestStatus != 0 {
				http.Error(w, "no manifest here", remote.manifestStatus)
				return
			}
			if remote.manifestHTML {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(
					"<!doctype html><html><body>spa</body></html>"))
				return
			}
			// Mirror the real handler: build the manifest from the
			// requested targets; BuildManifest itself refuses targets
			// the manifest cannot model, which the handler surfaces as
			// 501 so the client falls back to the full archive.
			var req TargetSet
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			manifest, err := BuildManifest(req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotImplemented)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			require.NoError(t, json.NewEncoder(gz).Encode(manifest))
			require.NoError(t, gz.Close())
		case "/api/v1/remote-sync/archive":
			var req ArchiveRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			remote.archiveRequests = append(remote.archiveRequests, req)
			if remote.onArchive != nil {
				remote.onArchive(req)
			}
			if remote.rejectDelta && req.DeltaFiles != nil {
				http.Error(w, "delta not allowed", http.StatusForbidden)
				return
			}
			// Serve the requested target subset, as the real handler
			// does after SelectAllowedTargets.
			w.Header().Set("Content-Type", "application/x-tar")
			if remote.archiveBody != nil {
				_, _ = w.Write(remote.archiveBody)
				return
			}
			if req.DeltaFiles != nil {
				require.NoError(t, WriteArchiveFiles(
					w, remote.targets, req.DeltaFiles))
			} else {
				require.NoError(t, WriteArchive(w, req.TargetSet))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.ts.Close)
	return remote
}

// addFileScopedAgent registers a second agent whose targets are file
// scoped, mimicking Windsurf's curated export, and returns the remote
// file path. The file's content never parses as a session; partition
// tests assert transfer behavior, and Windsurf import correctness is
// covered by the server and sync tests.
func (r *mirrorTestRemote) addFileScopedAgent(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(filepath.Dir(r.dir), "scoped-agent")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "export.txt")
	require.NoError(t, os.WriteFile(path, []byte("file-scoped export\n"), 0o644))
	r.targets.Dirs[parser.AgentGemini] = []string{dir}
	r.targets.Files = map[parser.AgentType][]string{
		parser.AgentGemini: {path},
	}
	return path
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
	assert.Empty(t, remote.archiveRequests[0].DeltaFiles, "bootstrap uses the full archive")

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
	assert.ElementsMatch(t, []string{changed, added}, remote.archiveRequests[1].DeltaFiles)
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

func TestHTTPSyncMirrorRemovesSidecarThatVanishesDuringDeltaArchive(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "c.jsonl", base, "session c")
	wal := filepath.Join(filepath.Dir(remote.dir), "state.db-wal")
	require.NoError(t, os.WriteFile(wal, []byte("first wal"), 0o644))
	require.NoError(t, os.Chtimes(wal, base, base))
	remote.targets.ExtraFiles = []string{wal}
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	localWAL, err := safeRemappedRemotePath(MirrorDir(dataDir, "devbox"), wal)
	require.NoError(t, err)
	require.FileExists(t, localWAL)

	require.NoError(t, os.WriteFile(wal, []byte("second wal"), 0o644))
	require.NoError(t, os.Chtimes(wal, base.Add(time.Second), base.Add(time.Second)))
	remote.onArchive = func(req ArchiveRequest) {
		if assert.Equal(t, []string{wal}, req.DeltaFiles) {
			assert.NoError(t, os.Remove(wal))
		}
	}

	prepared, err = hs.Prepare(context.Background())
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	assert.NoFileExists(t, localWAL)
}

func TestHTTPSyncMirrorRefreshesStateDBWhenWALVanishesDuringDeltaArchive(t *testing.T) {
	remote := newMirrorTestRemote(t)
	profileRoot := filepath.Join(filepath.Dir(remote.dir), "hermes-profile")
	sessionsDir := filepath.Join(profileRoot, "sessions")
	stateDB := filepath.Join(profileRoot, "state.db")
	stateWAL := stateDB + "-wal"
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	writeHermesImportStateDB(t, stateDB)
	for i := range 3 {
		require.NoError(t, os.WriteFile(
			filepath.Join(sessionsDir, fmt.Sprintf("padding-%d.txt", i)),
			[]byte("manifest padding\n"),
			0o644,
		))
	}
	remote.targets = TargetSet{
		Dirs: map[parser.AgentType][]string{
			parser.AgentHermes: {sessionsDir},
		},
		ExtraFiles: []string{stateDB, stateWAL},
	}

	writer, err := sql.Open("sqlite3", stateDB)
	require.NoError(t, err)
	writer.SetMaxOpenConns(1)
	var writerCloseOnce stdsync.Once
	var writerCloseErr error
	closeWriter := func() error {
		writerCloseOnce.Do(func() { writerCloseErr = writer.Close() })
		return writerCloseErr
	}
	t.Cleanup(func() { require.NoError(t, closeWriter()) })
	var journalMode string
	require.NoError(t, writer.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
	_, err = writer.Exec(`PRAGMA wal_autocheckpoint = 0`)
	require.NoError(t, err)
	_, err = writer.Exec(`PRAGMA user_version = 1`)
	require.NoError(t, err)
	require.FileExists(t, stateWAL)

	dataDir := t.TempDir()
	database, hs := newMirrorSync(t, remote, dataDir)
	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	baseline, err := database.GetSession(
		context.Background(), "devbox~hermes:database-only",
	)
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.NotNil(t, baseline.DisplayName)
	assert.Equal(t, "Database-only profile", *baseline.DisplayName)

	stateBefore, err := os.Stat(stateDB)
	require.NoError(t, err)
	_, err = writer.Exec(`
		UPDATE sessions
		SET title = 'Checkpointed profile'
		WHERE id = 'database-only'
	`)
	require.NoError(t, err)
	require.FileExists(t, stateWAL)
	stateAfter, err := os.Stat(stateDB)
	require.NoError(t, err)
	assert.Equal(t, stateBefore.Size(), stateAfter.Size())
	assert.Equal(t, stateBefore.ModTime(), stateAfter.ModTime(),
		"the metadata update must remain WAL-only before the second manifest")

	checkpointResult := make(chan error, 1)
	remote.onArchive = func(ArchiveRequest) {
		_, checkpointErr := writer.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		checkpointErr = errors.Join(checkpointErr, closeWriter())
		if removeErr := os.Remove(stateWAL); !os.IsNotExist(removeErr) {
			checkpointErr = errors.Join(checkpointErr, removeErr)
		}
		checkpointResult <- checkpointErr
	}

	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	select {
	case checkpointErr := <-checkpointResult:
		require.NoError(t, checkpointErr)
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "archive hook did not checkpoint the Hermes database")
	}
	require.Len(t, remote.archiveRequests, 2)
	assert.Equal(t, []string{stateDB}, remote.archiveRequests[1].DeltaFiles)
	localWAL, err := safeRemappedRemotePath(MirrorDir(dataDir, "devbox"), stateWAL)
	require.NoError(t, err)
	assert.NoFileExists(t, localWAL)

	refreshed, err := database.GetSession(
		context.Background(), "devbox~hermes:database-only",
	)
	require.NoError(t, err)
	require.NotNil(t, refreshed)
	require.NotNil(t, refreshed.DisplayName)
	assert.Equal(t, "Checkpointed profile", *refreshed.DisplayName)

	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Zero(t, stats.SessionsSynced)
	assert.Len(t, remote.archiveRequests, 2,
		"an unchanged standalone snapshot must match the consolidated manifest")
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
	assert.Empty(t, remote.archiveRequests[0].DeltaFiles)
	assert.NoDirExists(t, MirrorDir(dataDir, "devbox"))
}

func TestHTTPSyncFallsBackToLegacyWhenManifestServesSPAHTML(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
	remote.manifestHTML = true
	_, hs := newMirrorSync(t, remote, t.TempDir())

	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	require.Len(t, remote.archiveRequests, 1)
	assert.Empty(t, remote.archiveRequests[0].DeltaFiles)
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
	assert.NotEmpty(t, remote.archiveRequests[1].DeltaFiles)
	assert.Empty(t, remote.archiveRequests[2].DeltaFiles)
}

func TestHTTPSyncIncrementalMatchesFreshFullSync(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 555666777, time.UTC)
	// Enough unchanged files that the second sync's two changed files
	// stay under the half-corpus bootstrap heuristic and exercise the
	// delta path rather than a full re-download.
	appended := remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "d.jsonl", base, "session d")
	remote.writeSession(t, "e.jsonl", base, "session e")

	incDB, incSync := newMirrorSync(t, remote, t.TempDir())
	_, err := incSync.Run(context.Background())
	require.NoError(t, err)

	remote.writeSession(t, "a.jsonl", base.Add(2*time.Second),
		"session a", "session a continued")
	added := remote.writeSession(t, "c.jsonl", base.Add(3*time.Second), "session c")
	_, err = incSync.Run(context.Background())
	require.NoError(t, err)

	// Requests: bootstrap full, then a delta for exactly the changes.
	require.Len(t, remote.archiveRequests, 2)
	assert.Empty(t, remote.archiveRequests[0].DeltaFiles)
	assert.ElementsMatch(t, []string{appended, added},
		remote.archiveRequests[1].DeltaFiles)

	freshDB, freshSync := newMirrorSync(t, remote, t.TempDir())
	_, err = freshSync.Run(context.Background())
	require.NoError(t, err)

	assert.Equal(t, sessionSummaries(t, freshDB), sessionSummaries(t, incDB))
}

func TestHTTPSyncMirrorRecoversFromDirAtFilePath(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	wedged := remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "c.jsonl", base, "session c")
	remote.writeSession(t, "d.jsonl", base, "session d")
	remote.writeSession(t, "e.jsonl", base, "session e")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	_, err := hs.Run(context.Background())
	require.NoError(t, err)

	// Simulate a crashed extraction: a directory occupies a.jsonl's
	// mirror path (MkdirAll ran, the file write never happened).
	local, err := safeRemappedRemotePath(MirrorDir(dataDir, "devbox"), wedged)
	require.NoError(t, err)
	require.NoError(t, os.Remove(local))
	require.NoError(t, os.Mkdir(local, 0o755))

	_, err = hs.Run(context.Background())
	require.NoError(t, err)
	info, err := os.Stat(local)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	// Recovery re-fetched only the wedged file, via the delta path.
	require.Len(t, remote.archiveRequests, 2)
	assert.Equal(t, []string{wedged}, remote.archiveRequests[1].DeltaFiles)
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

// A host with a file-scoped agent (Windsurf) must not lose incremental
// sync for its dir-scoped corpus: the manifest request carries only the
// dir-scoped targets, and the file-scoped exports arrive as a separate
// small full archive on every sync.
func TestHTTPSyncMirrorPartitionsFileScopedAgents(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "c.jsonl", base, "session c")
	remote.writeSession(t, "d.jsonl", base, "session d")
	remote.writeSession(t, "e.jsonl", base, "session e")
	scoped := remote.addFileScopedAgent(t)
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 5, stats.SessionsSynced)
	// Bootstrap: one full archive for the dir-scoped corpus, one for
	// the file-scoped agent — never a combined whole-host archive.
	require.Len(t, remote.archiveRequests, 2)
	assert.Nil(t, remote.archiveRequests[0].DeltaFiles)
	assert.Contains(t, remote.archiveRequests[0].Dirs, parser.AgentClaude)
	assert.NotContains(t, remote.archiveRequests[0].Dirs, parser.AgentGemini)
	assert.Contains(t, remote.archiveRequests[1].Files, parser.AgentGemini)
	assert.NotContains(t, remote.archiveRequests[1].Dirs, parser.AgentClaude)

	mirrorRoot := MirrorDir(dataDir, "devbox")
	scopedLocal, err := safeRemappedRemotePath(mirrorRoot, scoped)
	require.NoError(t, err)
	assert.FileExists(t, scopedLocal)

	// Second sync: the changed session travels as a delta, and the
	// file-scoped export — cleared by the mirror deletion pass because
	// it is never in the manifest — is re-fetched in full and lands
	// back in the mirror.
	changed := remote.writeSession(t, "a.jsonl", base.Add(5*time.Second),
		"session a", "session a continued")
	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	require.Len(t, remote.archiveRequests, 4)
	assert.Equal(t, []string{changed}, remote.archiveRequests[2].DeltaFiles)
	assert.Contains(t, remote.archiveRequests[3].Files, parser.AgentGemini)
	assert.FileExists(t, scopedLocal)

	// The file-scoped export disappears from the remote: the deletion
	// pass clears its mirror copy and nothing re-populates it, matching
	// the legacy path where only the current export was ever extracted.
	require.NoError(t, os.Remove(scoped))
	delete(remote.targets.Dirs, parser.AgentGemini)
	remote.targets.Files = nil
	_, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.NoFileExists(t, scopedLocal)
	assert.Len(t, remote.archiveRequests, 4,
		"no archive requests when nothing changed and no file-scoped targets remain")
}

// addRooCodeAgent writes a RooCode globalStorage tree with taskCount
// tasks plus an mcp_settings.json secret, registers the resolved
// verbatim file-scoped targets on the remote, and returns the settings
// path and the per-task ui_messages.json transcripts.
func (r *mirrorTestRemote) addRooCodeAgent(
	t *testing.T, taskCount int, mtime time.Time,
) (string, []string) {
	t.Helper()
	rooRoot := filepath.Join(filepath.Dir(r.dir), "roo-cline")
	transcripts := make([]string, 0, taskCount)
	for i := range taskCount {
		taskDir := filepath.Join(rooRoot, "tasks", fmt.Sprintf("task-%d", i))
		require.NoError(t, os.MkdirAll(taskDir, 0o755))
		history := filepath.Join(taskDir, "history_item.json")
		require.NoError(t, os.WriteFile(history, fmt.Appendf(nil,
			`{"id":"task-%d","ts":1720000000000,"task":"task %d"}`, i, i), 0o644))
		messages := filepath.Join(taskDir, "ui_messages.json")
		require.NoError(t, os.WriteFile(messages, fmt.Appendf(nil,
			`[{"ts":1720000000000,"type":"say","say":"text","text":"hello %d"}]`,
			i), 0o644))
		require.NoError(t, os.Chtimes(history, mtime, mtime))
		require.NoError(t, os.Chtimes(messages, mtime, mtime))
		transcripts = append(transcripts, messages)
	}
	settingsDir := filepath.Join(rooRoot, "settings")
	require.NoError(t, os.MkdirAll(settingsDir, 0o755))
	mcpSettings := filepath.Join(settingsDir, "mcp_settings.json")
	require.NoError(t, os.WriteFile(mcpSettings,
		[]byte(`{"mcpServers":{"s":{"env":{"API_KEY":"sk-secret"}}}}`), 0o644))
	resolved := ResolveTargets(config.Config{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentRooCode: {rooRoot},
		},
	})
	r.targets.Dirs[parser.AgentRooCode] = resolved.Dirs[parser.AgentRooCode]
	if r.targets.Files == nil {
		r.targets.Files = make(map[parser.AgentType][]string)
	}
	r.targets.Files[parser.AgentRooCode] = resolved.Files[parser.AgentRooCode]
	return mcpSettings, transcripts
}

// RooCode is verbatim file-scoped: its curated files ride the
// manifest/delta path, so an appended transcript transfers alone
// instead of re-downloading the whole RooCode archive every sync, and
// the raw tree (mcp_settings.json secrets) still never leaves the
// remote.
func TestHTTPSyncMirrorRooCodeDeltaTransfersOnlyChangedTranscript(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	mcpSettings, transcripts := remote.addRooCodeAgent(t, 4, base)
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 4, stats.SessionsSynced)
	// Bootstrap: one full archive, and no recurring file-scoped
	// side-channel archive for a verbatim agent.
	require.Len(t, remote.archiveRequests, 1)
	assert.Nil(t, remote.archiveRequests[0].DeltaFiles)

	mirrorRoot := MirrorDir(dataDir, "devbox")
	mirroredTranscript, err := safeRemappedRemotePath(mirrorRoot, transcripts[0])
	require.NoError(t, err)
	assert.FileExists(t, mirroredTranscript)
	mirroredSettings, err := safeRemappedRemotePath(mirrorRoot, mcpSettings)
	require.NoError(t, err)
	assert.NoFileExists(t, mirroredSettings,
		"mcp_settings.json must never reach the mirror")

	// Append to one transcript: the delta request names exactly that
	// file — not the other tasks, not a full RooCode archive.
	changed := transcripts[0]
	require.NoError(t, os.WriteFile(changed, []byte(
		`[{"ts":1720000000000,"type":"say","say":"text","text":"hello 0"},`+
			`{"ts":1720000005000,"type":"say","say":"text","text":"continued"}]`,
	), 0o644))
	require.NoError(t, os.Chtimes(changed,
		base.Add(5*time.Second), base.Add(5*time.Second)))

	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	require.Len(t, remote.archiveRequests, 2)
	assert.Equal(t, []string{changed}, remote.archiveRequests[1].DeltaFiles)

	// Nothing changed: no archive request at all, so per-poll transfer
	// work is bounded by the changed batch, not the archive size.
	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, stats.SessionsSynced)
	assert.Len(t, remote.archiveRequests, 2)
}

// The mirror lock must already be held when the manifest is fetched:
// otherwise two concurrent syncs can fetch manifests in one order and
// apply them in another, and the stale manifest's deletion pass
// removes files the newer sync just mirrored.
func TestHTTPSyncHoldsMirrorLockDuringManifestFetch(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	mirrorRoot := MirrorDir(dataDir, "devbox")
	remote.onManifest = func() {
		// assert, not require: this runs on the server goroutine, where
		// FailNow must not be called.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		lock, err := AcquireMirrorLock(ctx, mirrorRoot)
		if lock != nil {
			// On regression the acquire succeeds; release immediately so
			// the sync under test fails on the assertion instead of
			// deadlocking against a leaked lock.
			_ = lock.Close()
		}
		assert.Error(t, err, "mirror lock must be held during the manifest fetch")
	}

	_, err := hs.Run(context.Background())
	require.NoError(t, err)
}

func TestPrepareHTTPSyncContributorDeltaCardinality(t *testing.T) {
	for _, fileCount := range []int{5, 500} {
		t.Run(strconv.Itoa(fileCount), func(t *testing.T) {
			remote := newMirrorTestRemote(t)
			base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
			var changed string
			for i := range fileCount {
				path := remote.writeSession(t, fmt.Sprintf("%03d.jsonl", i),
					base, fmt.Sprintf("session %d", i))
				if i == 0 {
					changed = path
				}
			}
			dataDir := t.TempDir()
			database, hs := newMirrorSync(t, remote, dataDir)

			prepared, err := hs.Prepare(context.Background())
			require.NoError(t, err)
			require.NoError(t, prepared.Close())
			require.Len(t, remote.archiveRequests, 1)
			assert.Nil(t, remote.archiveRequests[0].DeltaFiles)

			remote.writeSession(t, "000.jsonl", base.Add(time.Second),
				"session 0", "session 0 continued")
			hs.Full = true
			prepared, err = hs.Prepare(context.Background())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, prepared.Close()) })
			contributor, err := prepared.RebuildContributor()
			require.NoError(t, err)
			engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
			stats, err := engine.ResyncAllWithOptions(
				context.Background(), nil,
				syncpkg.RebuildOptions{Contributors: []syncpkg.RebuildContributor{
					contributor,
				}},
			)
			engine.Close()
			require.NoError(t, err)
			assert.False(t, stats.Aborted)
			require.NoError(t, prepared.Close())

			require.Len(t, remote.archiveRequests, 2)
			assert.Equal(t, []string{changed}, remote.archiveRequests[1].DeltaFiles)
		})
	}
}

func TestPreparedHTTPSyncContributorKeepsLockUntilClose(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "session.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "locked contributor")
	database, hs := newMirrorSync(t, remote, t.TempDir())
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	contributor, err := prepared.RebuildContributor()
	require.NoError(t, err)

	contributorStarted := make(chan struct{})
	continueContributor := make(chan struct{})
	originalProgress := contributor.Progress
	contributor.Progress = func(progress syncpkg.Progress) syncpkg.Progress {
		if progress.Phase == syncpkg.PhaseDiscovering {
			select {
			case <-contributorStarted:
			default:
				close(contributorStarted)
				<-continueContributor
			}
		}
		return originalProgress(progress)
	}
	engine := syncpkg.NewEngine(database, syncpkg.EngineConfig{})
	t.Cleanup(engine.Close)
	type rebuildResult struct {
		stats syncpkg.SyncStats
		err   error
	}
	rebuilt := make(chan rebuildResult, 1)
	go func() {
		stats, runErr := engine.ResyncAllWithOptions(
			context.Background(), nil,
			syncpkg.RebuildOptions{Contributors: []syncpkg.RebuildContributor{
				contributor,
			}},
		)
		rebuilt <- rebuildResult{stats: stats, err: runErr}
	}()
	select {
	case <-contributorStarted:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "timed out waiting for contributor")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	competing, lockErr := AcquireMirrorLock(ctx, prepared.Root())
	cancel()
	if competing != nil {
		require.NoError(t, competing.Close())
	}
	assert.Error(t, lockErr, "prepared source must retain its lock during contribution")
	close(continueContributor)
	select {
	case result := <-rebuilt:
		require.NoError(t, result.err)
		assert.False(t, result.stats.Aborted)
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "timed out waiting for rebuild")
	}

	require.NoError(t, prepared.Close())
	assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
}

func TestPrepareHTTPSyncUnchangedFullMakesNoArchiveRequest(t *testing.T) {
	for _, fileCount := range []int{5, 500} {
		t.Run(strconv.Itoa(fileCount), func(t *testing.T) {
			remote := newMirrorTestRemote(t)
			mtime := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
			for i := range fileCount {
				remote.writeSession(t, fmt.Sprintf("%03d.jsonl", i), mtime,
					fmt.Sprintf("session %d", i))
			}
			_, hs := newMirrorSync(t, remote, t.TempDir())

			prepared, err := hs.Prepare(context.Background())
			require.NoError(t, err)
			require.NoError(t, prepared.Close())
			require.Len(t, remote.archiveRequests, 1)

			hs.Full = true
			prepared, err = hs.Prepare(context.Background())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, prepared.Close()) })
			assert.Len(t, remote.archiveRequests, 1,
				"unchanged full preparation must not transfer the %d-file mirror",
				fileCount)
		})
	}
}

func TestPrepareHTTPSyncManualMirrorRemovalBootstrapsFull(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)

	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	require.NoError(t, prepared.Close())
	require.Len(t, remote.archiveRequests, 1)

	require.NoError(t, os.RemoveAll(MirrorDir(dataDir, "devbox")))
	prepared, err = hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })

	require.Len(t, remote.archiveRequests, 2)
	assert.Nil(t, remote.archiveRequests[1].DeltaFiles)
}

func TestPreparedHTTPSyncCloseReleasesLockAndRemovesLegacyRoot(t *testing.T) {
	t.Run("manifest mirror persists", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.writeSession(t, "a.jsonl",
			time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
		dataDir := t.TempDir()
		_, hs := newMirrorSync(t, remote, dataDir)

		prepared, err := hs.Prepare(context.Background())
		require.NoError(t, err)
		root := prepared.Root()
		assert.Equal(t, MirrorDir(dataDir, "devbox"), root)
		assert.DirExists(t, root)
		assert.Equal(t, remote.targets, prepared.Targets())
		assertMirrorLocked(t, root)

		require.NoError(t, prepared.Close())
		require.NoError(t, prepared.Close(), "persistent Close must be idempotent")
		assert.DirExists(t, root, "persistent mirror must survive Close")
		assertMirrorUnlocked(t, root)
	})

	t.Run("legacy root is owned", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusNotFound
		remote.writeSession(t, "a.jsonl",
			time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
		dataDir := t.TempDir()
		_, hs := newMirrorSync(t, remote, dataDir)

		prepared, err := hs.Prepare(context.Background())
		require.NoError(t, err)
		root := prepared.Root()
		assert.DirExists(t, root)
		assert.NotEqual(t, MirrorDir(dataDir, "devbox"), root)
		assert.NoDirExists(t, MirrorDir(dataDir, "devbox"))
		assertMirrorLocked(t, MirrorDir(dataDir, "devbox"))

		require.NoError(t, prepared.Close())
		require.NoError(t, prepared.Close(), "legacy Close must be idempotent")
		assert.NoDirExists(t, root, "owned legacy root must be removed")
		assertMirrorUnlocked(t, MirrorDir(dataDir, "devbox"))
	})
}

func TestPreparedHTTPSyncCloseRetriesFailedCleanup(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.manifestStatus = http.StatusNotFound
	remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	root := prepared.Root()

	removeErr := errors.New("transient remove failure")
	unlockErr := errors.New("transient unlock failure")
	removeCalls := 0
	prepared.removeRoot = func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return removeErr
		}
		return os.RemoveAll(path)
	}
	unlockCalls := 0
	prepared.releaseLock = func(lock *MirrorLockHandle) error {
		unlockCalls++
		if unlockCalls == 1 {
			return unlockErr
		}
		return lock.Close()
	}

	err = prepared.Close()
	assert.ErrorIs(t, err, removeErr)
	assert.ErrorIs(t, err, unlockErr)
	assert.DirExists(t, root)
	assertMirrorLocked(t, MirrorDir(dataDir, "devbox"))

	require.NoError(t, prepared.Close())
	assert.NoDirExists(t, root)
	assertMirrorUnlocked(t, MirrorDir(dataDir, "devbox"))
	require.NoError(t, prepared.Close(), "fully cleaned Close must be idempotent")
}

func TestPreparedHTTPSyncCloseTracksCleanupIndependently(t *testing.T) {
	prepareLegacy := func(t *testing.T) (*PreparedHTTP, string) {
		t.Helper()
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusNotFound
		remote.writeSession(t, "a.jsonl",
			time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
		dataDir := t.TempDir()
		_, hs := newMirrorSync(t, remote, dataDir)
		prepared, err := hs.Prepare(context.Background())
		require.NoError(t, err)
		return prepared, MirrorDir(dataDir, "devbox")
	}

	t.Run("successful unlock is not repeated", func(t *testing.T) {
		prepared, mirrorRoot := prepareLegacy(t)
		removeErr := errors.New("transient remove failure")
		removeCalls := 0
		prepared.removeRoot = func(path string) error {
			removeCalls++
			if removeCalls == 1 {
				return removeErr
			}
			return os.RemoveAll(path)
		}
		unlockCalls := 0
		prepared.releaseLock = func(lock *MirrorLockHandle) error {
			unlockCalls++
			return lock.Close()
		}

		assert.ErrorIs(t, prepared.Close(), removeErr)
		assertMirrorUnlocked(t, mirrorRoot)
		require.NoError(t, prepared.Close())
		assert.Equal(t, 1, unlockCalls)
	})

	t.Run("successful removal is not repeated", func(t *testing.T) {
		prepared, mirrorRoot := prepareLegacy(t)
		unlockErr := errors.New("transient unlock failure")
		removeCalls := 0
		prepared.removeRoot = func(path string) error {
			removeCalls++
			return os.RemoveAll(path)
		}
		unlockCalls := 0
		prepared.releaseLock = func(lock *MirrorLockHandle) error {
			unlockCalls++
			if unlockCalls == 1 {
				return unlockErr
			}
			return lock.Close()
		}

		assert.ErrorIs(t, prepared.Close(), unlockErr)
		assert.NoDirExists(t, prepared.Root())
		assertMirrorLocked(t, mirrorRoot)
		require.NoError(t, prepared.Close())
		assert.Equal(t, 1, removeCalls)
		assertMirrorUnlocked(t, mirrorRoot)
	})
}

func TestPrepareHTTPSyncClearsOnlySelectedHostCacheBeforeMirrorMutation(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)
	changed := remote.writeSession(t, "a.jsonl", base, "session a")
	remote.writeSession(t, "b.jsonl", base, "session b")
	remote.writeSession(t, "c.jsonl", base, "session c")
	database, hs := newMirrorSync(t, remote, t.TempDir())
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	require.NoError(t, prepared.Close())

	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"devbox", map[string]int64{changed: 101},
	))
	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"other-host", map[string]int64{"/remote/other.jsonl": 202},
	))
	remote.writeSession(t, "a.jsonl", base.Add(time.Second),
		"session a", "continued")
	type cacheSnapshot struct {
		selected map[string]int64
		other    map[string]int64
		err      error
	}
	atArchive := make(chan cacheSnapshot, 1)
	remote.onArchive = func(ArchiveRequest) {
		selected, err := database.LoadRemoteSkippedFiles("devbox")
		if err != nil {
			atArchive <- cacheSnapshot{err: err}
			return
		}
		other, err := database.LoadRemoteSkippedFiles("other-host")
		atArchive <- cacheSnapshot{selected: selected, other: other, err: err}
	}

	prepared, err = hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	var snapshot cacheSnapshot
	select {
	case snapshot = <-atArchive:
	case <-time.After(backgroundWaitTimeout):
		require.FailNow(t, "timed out waiting for archive cache snapshot")
	}
	require.NoError(t, snapshot.err)
	assert.Empty(t, snapshot.selected,
		"selected cache must be clear before archive extraction starts")
	assert.Equal(t, map[string]int64{"/remote/other.jsonl": 202}, snapshot.other)
	selected, err := database.LoadRemoteSkippedFiles("devbox")
	require.NoError(t, err)
	assert.Empty(t, selected)
}

func TestPrepareHTTPSyncUnchangedPreservesSelectedHostCache(t *testing.T) {
	remote := newMirrorTestRemote(t)
	path := remote.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
	database, hs := newMirrorSync(t, remote, t.TempDir())
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	require.NoError(t, prepared.Close())

	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"devbox", map[string]int64{path: 303},
	))
	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		"other-host", map[string]int64{"/remote/other.jsonl": 304},
	))
	requests := len(remote.archiveRequests)
	prepared, err = hs.Prepare(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	assert.Len(t, remote.archiveRequests, requests)
	selected, err := database.LoadRemoteSkippedFiles("devbox")
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{path: 303}, selected)
	other, err := database.LoadRemoteSkippedFiles("other-host")
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"/remote/other.jsonl": 304}, other)
}

func TestPrepareHTTPSyncFailureCleansOwnedResources(t *testing.T) {
	t.Run("manifest failure releases lock", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusInternalServerError
		_, hs := newMirrorSync(t, remote, t.TempDir())

		prepared, err := hs.Prepare(context.Background())
		require.Error(t, err)
		assert.Nil(t, prepared)
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, "devbox"))
	})

	t.Run("legacy extraction failure removes temp root", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusNotFound
		path := remote.writeSession(t, "a.jsonl",
			time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
		name, err := safeRemotePathArchiveName(path)
		require.NoError(t, err)
		remote.archiveBody = tarWithoutEndMarker(t, name, "partial legacy")
		tempParent := t.TempDir()
		setPortableTempDir(t, tempParent)
		_, hs := newMirrorSync(t, remote, t.TempDir())

		prepared, err := hs.Prepare(context.Background())
		require.Error(t, err)
		assert.Nil(t, prepared)
		roots, globErr := filepath.Glob(filepath.Join(tempParent, "agentsview-http-*"))
		require.NoError(t, globErr)
		assert.Empty(t, roots)
		assert.NoDirExists(t, MirrorDir(hs.DataDir, "devbox"))
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, "devbox"))
	})

	t.Run("failed mirror extraction leaves bytes and invalidated cache", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		path := remote.writeSession(t, "a.jsonl",
			time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC), "session a")
		name, err := safeRemotePathArchiveName(path)
		require.NoError(t, err)
		remote.archiveBody = tarWithoutEndMarker(t, name, "partial mirror")
		database, hs := newMirrorSync(t, remote, t.TempDir())
		require.NoError(t, database.ReplaceRemoteSkippedFiles(
			"devbox", map[string]int64{path: 404},
		))
		require.NoError(t, database.ReplaceRemoteSkippedFiles(
			"other-host", map[string]int64{"/remote/other.jsonl": 505},
		))

		prepared, err := hs.Prepare(context.Background())
		require.Error(t, err)
		assert.Nil(t, prepared)
		local, mapErr := safeRemappedRemotePath(MirrorDir(hs.DataDir, "devbox"), path)
		require.NoError(t, mapErr)
		body, readErr := os.ReadFile(local)
		require.NoError(t, readErr)
		assert.Equal(t, "partial mirror", string(body))
		selected, loadErr := database.LoadRemoteSkippedFiles("devbox")
		require.NoError(t, loadErr)
		assert.Empty(t, selected)
		other, loadErr := database.LoadRemoteSkippedFiles("other-host")
		require.NoError(t, loadErr)
		assert.Equal(t, map[string]int64{"/remote/other.jsonl": 505}, other)
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, "devbox"))
	})
}

func TestPrepareHTTPSyncsSortsHostsAndUnwindsOnFailure(t *testing.T) {
	remoteA := newMirrorTestRemote(t)
	remoteA.manifestStatus = http.StatusNotFound
	remoteA.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC), "host a")
	remoteB := newMirrorTestRemote(t)
	remoteB.manifestStatus = http.StatusInternalServerError
	remoteB.writeSession(t, "b.jsonl",
		time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC), "host b")

	dataDir := t.TempDir()
	tempParent := t.TempDir()
	setPortableTempDir(t, tempParent)
	database, syncA := newMirrorSync(t, remoteA, dataDir)
	syncA.Host = "host-a"
	syncB := syncA
	syncB.Host = "host-b"
	syncB.URL = remoteB.ts.URL
	syncB.DB = database
	var requestOrder []string
	remoteA.onManifest = func() { requestOrder = append(requestOrder, "host-a") }
	remoteB.onManifest = func() { requestOrder = append(requestOrder, "host-b") }

	syncs := []HTTPSync{syncB, syncA}
	prepared, err := PrepareHTTPSyncs(context.Background(), syncs)
	require.Error(t, err)
	assert.Nil(t, prepared)
	assert.Equal(t, []string{"host-b", "host-a"}, []string{
		syncs[0].Host, syncs[1].Host,
	}, "preparation must not mutate caller ordering")
	assert.Equal(t, []string{"host-a", "host-b"}, requestOrder)
	var hostErr *HostError
	require.ErrorAs(t, err, &hostErr)
	assert.Equal(t, "host-b", hostErr.Host)
	assertMirrorUnlocked(t, MirrorDir(dataDir, "host-a"))
	assertMirrorUnlocked(t, MirrorDir(dataDir, "host-b"))
	roots, globErr := filepath.Glob(filepath.Join(tempParent, "agentsview-http-*"))
	require.NoError(t, globErr)
	assert.Empty(t, roots, "unwind removes the successful legacy root and spools")
}

func TestPrepareHTTPSyncsLegacySourcesDoNotCreateWorkingDirectoryLocks(t *testing.T) {
	remoteA := newMirrorTestRemote(t)
	remoteA.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC), "host a")
	remoteB := newMirrorTestRemote(t)
	remoteB.writeSession(t, "b.jsonl",
		time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC), "host b")

	workingDir := t.TempDir()
	previousDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(workingDir))
	t.Cleanup(func() { require.NoError(t, os.Chdir(previousDir)) })
	setPortableTempDir(t, t.TempDir())

	var requestOrder []string
	remoteA.onArchive = func(ArchiveRequest) {
		requestOrder = append(requestOrder, "host-a")
	}
	remoteB.onArchive = func(ArchiveRequest) {
		requestOrder = append(requestOrder, "host-b")
	}
	syncA := HTTPSync{Host: "host-a", URL: remoteA.ts.URL, Client: remoteA.ts.Client()}
	syncB := HTTPSync{Host: "host-b", URL: remoteB.ts.URL, Client: remoteB.ts.Client()}
	syncs := []HTTPSync{syncB, syncA}

	prepared, err := PrepareHTTPSyncs(context.Background(), syncs)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	require.NoError(t, prepared.Close())

	assert.Equal(t, []string{"host-b", "host-a"}, []string{
		syncs[0].Host, syncs[1].Host,
	}, "preparation must not mutate caller ordering")
	assert.Equal(t, []string{"host-a", "host-b"}, requestOrder)
	assert.NoDirExists(t, filepath.Join(workingDir, "remote-mirrors"),
		"legacy sources have no persistent mirror lock identity")
}

func TestPrepareHTTPSyncsOrdersConcurrentCallersByMirrorLockPath(t *testing.T) {
	remoteA := newMirrorTestRemote(t)
	remoteA.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC), "mirror a")
	remoteB := newMirrorTestRemote(t)
	remoteB.writeSession(t, "b.jsonl",
		time.Date(2026, 7, 11, 14, 0, 0, 0, time.UTC), "mirror b")

	base := t.TempDir()
	dataDirA := base + string(filepath.Separator) + "z" +
		string(filepath.Separator) + ".." + string(filepath.Separator) + "a-data"
	dataDirB := filepath.Join(base, "b-data")
	database, syncA := newMirrorSync(t, remoteA, dataDirA)
	syncA.Host = "shared-host"
	syncB := syncA
	syncB.DataDir = dataDirB
	syncB.URL = remoteB.ts.URL
	syncB.DB = database
	assert.Less(t, MirrorDir(dataDirA, syncA.Host), MirrorDir(dataDirB, syncB.Host))

	firstA := make(chan struct{})
	firstB := make(chan struct{})
	var onceA, onceB stdsync.Once
	var orderMu stdsync.Mutex
	orders := map[string][]string{}
	record := func(
		caller, mirror string, ownOnce *stdsync.Once,
		own chan struct{}, other <-chan struct{},
	) {
		orderMu.Lock()
		orders[caller] = append(orders[caller], mirror)
		orderMu.Unlock()
		ownOnce.Do(func() { close(own) })
		select {
		case <-other:
		case <-time.After(50 * time.Millisecond):
		}
	}
	remoteA.onManifestRequest = func(r *http.Request) {
		record(r.Header.Get("Authorization"), "mirror-a",
			&onceA, firstA, firstB)
	}
	remoteB.onManifestRequest = func(r *http.Request) {
		record(r.Header.Get("Authorization"), "mirror-b",
			&onceB, firstB, firstA)
	}

	callerOne := []HTTPSync{syncB, syncA}
	callerOne[0].Token = "caller-one"
	callerOne[1].Token = "caller-one"
	callerTwo := []HTTPSync{syncA, syncB}
	callerTwo[0].Token = "caller-two"
	callerTwo[1].Token = "caller-two"
	type result struct {
		caller   string
		prepared *PreparedHTTPSyncs
		err      error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan result, 2)
	for caller, syncs := range map[string][]HTTPSync{
		"caller-one": callerOne,
		"caller-two": callerTwo,
	} {
		go func() {
			<-start
			prepared, err := PrepareHTTPSyncs(ctx, syncs)
			results <- result{caller: caller, prepared: prepared, err: err}
		}()
	}
	close(start)

	got := make(map[string]error, 2)
	for range 2 {
		select {
		case result := <-results:
			got[result.caller] = result.err
			if result.prepared != nil {
				require.NoError(t, result.prepared.Close())
			}
		case <-ctx.Done():
			require.FailNow(t, "concurrent preparation deadlocked",
				"orders=%v", orders)
		}
	}
	require.NoError(t, got["caller-one"])
	require.NoError(t, got["caller-two"])
	orderMu.Lock()
	assert.Equal(t, []string{"mirror-a", "mirror-b"}, orders["Bearer caller-one"])
	assert.Equal(t, []string{"mirror-a", "mirror-b"}, orders["Bearer caller-two"])
	orderMu.Unlock()
	assert.Equal(t, []string{"shared-host", "shared-host"}, []string{
		callerOne[0].Host, callerOne[1].Host,
	})
	assert.Equal(t, []string{dataDirB, dataDirA}, []string{
		callerOne[0].DataDir, callerOne[1].DataDir,
	}, "preparation must preserve caller-one input order")
	assert.Equal(t, []string{dataDirA, dataDirB}, []string{
		callerTwo[0].DataDir, callerTwo[1].DataDir,
	}, "preparation must preserve caller-two input order")
}

func TestPrepareHTTPSyncsRejectsDuplicateCanonicalLockIdentity(t *testing.T) {
	t.Run("clean parent alias", func(t *testing.T) {
		base := t.TempDir()
		canonical := filepath.Join(base, "data")
		alias := base + string(filepath.Separator) + "z" +
			string(filepath.Separator) + ".." + string(filepath.Separator) + "data"
		assertDuplicateCanonicalLockIdentity(t, canonical, alias)
	})

	t.Run("symlink parent alias", func(t *testing.T) {
		base := t.TempDir()
		realParent := filepath.Join(base, "real")
		require.NoError(t, os.MkdirAll(realParent, 0o755))
		linkParent := filepath.Join(base, "link")
		if err := os.Symlink(realParent, linkParent); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		assertDuplicateCanonicalLockIdentity(
			t, filepath.Join(realParent, "data"), filepath.Join(linkParent, "data"),
		)
	})

	t.Run("case alias on insensitive volume", func(t *testing.T) {
		base := t.TempDir()
		probe := filepath.Join(base, "CaseSensitiveProbe")
		require.NoError(t, os.Mkdir(probe, 0o755))
		probeAlias := filepath.Join(base, "cASEsENSITIVEpROBE")
		actualInfo, err := os.Stat(probe)
		require.NoError(t, err)
		aliasInfo, err := os.Stat(probeAlias)
		if os.IsNotExist(err) {
			t.Skip("test volume is case-sensitive")
		}
		require.NoError(t, err)
		if !os.SameFile(actualInfo, aliasInfo) {
			t.Skip("case aliases do not identify the same directory")
		}
		require.NoError(t, os.Remove(probe))
		actualParent := filepath.Join(base, "InitiallyMissingCaseParent")
		aliasParent := filepath.Join(base, "iNITIALLYmISSINGcASEpARENT")
		assert.NoDirExists(t, actualParent)
		assert.NoDirExists(t, aliasParent)
		assertDuplicateCanonicalLockIdentity(
			t, filepath.Join(actualParent, "data"), filepath.Join(aliasParent, "data"),
		)
	})

	t.Run("unicode normalization alias", func(t *testing.T) {
		base := t.TempDir()
		probe := filepath.Join(base, "Caf\u00e9Probe")
		probeAlias := filepath.Join(base, "Cafe\u0301Probe")
		require.NoError(t, os.Mkdir(probe, 0o755))
		actualInfo, err := os.Stat(probe)
		require.NoError(t, err)
		aliasInfo, err := os.Stat(probeAlias)
		if os.IsNotExist(err) {
			t.Skip("test volume does not alias Unicode normalization forms")
		}
		require.NoError(t, err)
		if !os.SameFile(actualInfo, aliasInfo) {
			t.Skip("Unicode normalization forms identify different directories")
		}
		require.NoError(t, os.Remove(probe))
		actualParent := filepath.Join(base, "Caf\u00e9Missing")
		aliasParent := filepath.Join(base, "Cafe\u0301Missing")
		assert.NoDirExists(t, actualParent)
		assert.NoDirExists(t, aliasParent)
		assertDuplicateCanonicalLockIdentity(
			t, filepath.Join(actualParent, "data"), filepath.Join(aliasParent, "data"),
		)
	})
}

func assertDuplicateCanonicalLockIdentity(t *testing.T, dataDirA, dataDirB string) {
	t.Helper()
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "duplicate.jsonl",
		time.Date(2026, 7, 11, 14, 30, 0, 0, time.UTC), "duplicate")
	database, syncA := newMirrorSync(t, remote, dataDirA)
	syncA.Host = "shared-host"
	syncB := syncA
	syncB.DataDir = dataDirB
	syncB.DB = database
	manifestRequests := 0
	remote.onManifest = func() { manifestRequests++ }
	syncs := []HTTPSync{syncB, syncA}

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	prepared, err := PrepareHTTPSyncs(ctx, syncs)
	require.Error(t, err)
	assert.Nil(t, prepared)
	assert.ErrorIs(t, err, ErrDuplicateMirrorLock)
	var hostErr *HostError
	require.ErrorAs(t, err, &hostErr)
	assert.Equal(t, "shared-host", hostErr.Host)
	assert.Equal(t, "resolve mirror lock", hostErr.Operation)
	assert.Contains(t, hostErr.Error(), `"shared-host"`)
	assert.Zero(t, manifestRequests,
		"duplicates must fail before acquiring a lock or preparing a source")
	lockArtifact := MirrorDir(dataDirA, syncA.Host) + ".lock"
	lockInfo, statErr := os.Stat(lockArtifact)
	require.NoError(t, statErr)
	assert.False(t, lockInfo.IsDir())
	if runtime.GOOS != "windows" {
		assert.Equal(t, os.FileMode(0o600), lockInfo.Mode().Perm(),
			"preflight creates only the same private lock artifact acquisition needs")
	}
	assert.Equal(t, []string{dataDirB, dataDirA}, []string{
		syncs[0].DataDir, syncs[1].DataDir,
	}, "duplicate detection must preserve caller order")
	lockCtx, lockCancel := context.WithTimeout(context.Background(), time.Second)
	defer lockCancel()
	lock, lockErr := AcquireMirrorLock(lockCtx, MirrorDir(dataDirA, syncA.Host))
	require.NoError(t, lockErr)
	require.NoError(t, lock.Close())
}

func TestPreparedHTTPSyncsHoldAllLocksUntilClose(t *testing.T) {
	remoteA := newMirrorTestRemote(t)
	remoteA.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC), "host a")
	remoteB := newMirrorTestRemote(t)
	remoteB.writeSession(t, "b.jsonl",
		time.Date(2026, 7, 11, 11, 0, 0, 0, time.UTC), "host b")
	dataDir := t.TempDir()
	database, syncA := newMirrorSync(t, remoteA, dataDir)
	syncA.Host = "host-a"
	syncB := syncA
	syncB.Host = "host-b"
	syncB.URL = remoteB.ts.URL
	syncB.DB = database

	prepared, err := PrepareHTTPSyncs(
		context.Background(), []HTTPSync{syncB, syncA},
	)
	require.NoError(t, err)
	contributors, release, err := prepared.BorrowRebuildContributors()
	require.NoError(t, err)
	t.Cleanup(func() {
		release()
		require.NoError(t, prepared.Close())
	})
	assert.Equal(t, []string{"host-a", "host-b"}, []string{
		contributors[0].Name, contributors[1].Name,
	})

	for _, host := range []string{"host-a", "host-b"} {
		lockCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		competing, lockErr := AcquireMirrorLock(
			lockCtx, MirrorDir(dataDir, host),
		)
		cancel()
		if competing != nil {
			require.NoError(t, competing.Close())
		}
		assert.Error(t, lockErr,
			"all mirror locks remain held across earlier contributor work")
	}

	release()
	require.NoError(t, prepared.Close())
	for _, host := range []string{"host-a", "host-b"} {
		lockCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		competing, lockErr := AcquireMirrorLock(
			lockCtx, MirrorDir(dataDir, host),
		)
		cancel()
		require.NoError(t, lockErr, host)
		require.NotNil(t, competing, host)
		require.NoError(t, competing.Close())
	}
	_, _, err = prepared.BorrowRebuildContributors()
	assert.ErrorIs(t, err, ErrPreparedClosed,
		"closed aggregate cannot expose contributors")
}

func TestPreparedHTTPSyncsBorrowBlocksCloseUntilRelease(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.writeSession(t, "borrow.jsonl",
		time.Date(2026, 7, 11, 15, 30, 0, 0, time.UTC), "borrowed")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	prepared, err := PrepareHTTPSyncs(context.Background(), []HTTPSync{hs})
	require.NoError(t, err)
	contributors, release, err := prepared.BorrowRebuildContributors()
	require.NoError(t, err)
	t.Cleanup(func() {
		release()
		require.NoError(t, prepared.Close())
	})
	require.Len(t, contributors, 1)
	assert.Equal(t, hs.Host, contributors[0].Name)

	assert.ErrorIs(t, prepared.Close(), ErrPreparedInUse)
	_, _, err = prepared.BorrowRebuildContributors()
	assert.ErrorIs(t, err, ErrPreparedClosed,
		"a Close attempt ends new borrowing while retained ownership remains retryable")
	lockCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	competing, lockErr := AcquireMirrorLock(lockCtx, MirrorDir(dataDir, hs.Host))
	cancel()
	if competing != nil {
		require.NoError(t, competing.Close())
	}
	assert.Error(t, lockErr, "borrowed Close must retain its mirror lock")

	release()
	release()
	require.NoError(t, prepared.Close())
	lockCtx, cancel = context.WithTimeout(context.Background(), time.Second)
	competing, lockErr = AcquireMirrorLock(lockCtx, MirrorDir(dataDir, hs.Host))
	cancel()
	require.NoError(t, lockErr)
	require.NotNil(t, competing)
	require.NoError(t, competing.Close())
	_, _, err = prepared.BorrowRebuildContributors()
	assert.ErrorIs(t, err, ErrPreparedClosed)
}

func TestPreparedHTTPSyncsBorrowAndCloseAreRaceSafe(t *testing.T) {
	prepared := &PreparedHTTPSyncs{sources: []*PreparedHTTP{{
		sync:        HTTPSync{Host: "race-host"},
		lock:        &MirrorLockHandle{},
		releaseLock: func(*MirrorLockHandle) error { return nil },
	}}}
	_, initialRelease, err := prepared.BorrowRebuildContributors()
	require.NoError(t, err)
	start := make(chan struct{})
	borrowResults := make(chan error, 8)
	closeResult := make(chan error, 1)
	for range 8 {
		go func() {
			<-start
			_, release, borrowErr := prepared.BorrowRebuildContributors()
			if borrowErr == nil {
				release()
				release()
			}
			borrowResults <- borrowErr
		}()
	}
	go func() {
		<-start
		closeResult <- prepared.Close()
	}()
	close(start)
	for range 8 {
		borrowErr := <-borrowResults
		assert.True(t, borrowErr == nil || errors.Is(borrowErr, ErrPreparedClosed),
			"unexpected borrow result: %v", borrowErr)
	}
	assert.ErrorIs(t, <-closeResult, ErrPreparedInUse)
	initialRelease()
	require.NoError(t, prepared.Close())
}

func TestPreparedHTTPSyncsCloseReversesJoinsAndRetries(t *testing.T) {
	transient := errors.New("transient release")
	var order []string
	bCalls := 0
	prepared := &PreparedHTTPSyncs{sources: []*PreparedHTTP{
		{
			lock: &MirrorLockHandle{},
			releaseLock: func(*MirrorLockHandle) error {
				order = append(order, "host-a")
				return nil
			},
		},
		{
			lock: &MirrorLockHandle{},
			releaseLock: func(*MirrorLockHandle) error {
				order = append(order, "host-b")
				bCalls++
				if bCalls == 1 {
					return transient
				}
				return nil
			},
		},
	}}

	assert.ErrorIs(t, prepared.Close(), transient)
	assert.Equal(t, []string{"host-b", "host-a"}, order)
	require.NoError(t, prepared.Close())
	assert.Equal(t, []string{"host-b", "host-a", "host-b"}, order,
		"only failed ownership is retained for retry")
	require.NoError(t, prepared.Close(), "fully closed aggregate is idempotent")
}

func TestPrepareHTTPSyncsReturnsFailedUnwindOwnership(t *testing.T) {
	remoteA := newMirrorTestRemote(t)
	remoteA.manifestStatus = http.StatusNotFound
	remoteA.writeSession(t, "a.jsonl",
		time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC), "legacy a")
	remoteB := newMirrorTestRemote(t)
	remoteB.manifestStatus = http.StatusInternalServerError
	remoteB.writeSession(t, "b.jsonl",
		time.Date(2026, 7, 11, 15, 0, 0, 0, time.UTC), "host b")
	tempParent := t.TempDir()
	setPortableTempDir(t, tempParent)
	dataDir := t.TempDir()
	database, syncA := newMirrorSync(t, remoteA, dataDir)
	syncA.Host = "host-a"
	syncB := syncA
	syncB.Host = "host-b"
	syncB.URL = remoteB.ts.URL
	syncB.DB = database
	removeErr := errors.New("transient aggregate remove failure")
	unlockErr := errors.New("transient aggregate unlock failure")
	removeCalls := 0
	unlockCalls := 0
	var ownedRoot string

	prepared, err := prepareHTTPSyncs(
		context.Background(), []HTTPSync{syncB, syncA},
		func(ctx context.Context, hs HTTPSync) (*PreparedHTTP, error) {
			source, prepareErr := hs.Prepare(ctx)
			if prepareErr != nil || hs.Host != "host-a" {
				return source, prepareErr
			}
			ownedRoot = source.Root()
			source.removeRoot = func(path string) error {
				removeCalls++
				if removeCalls == 1 {
					return removeErr
				}
				return os.RemoveAll(path)
			}
			source.releaseLock = func(lock *MirrorLockHandle) error {
				unlockCalls++
				if unlockCalls == 1 {
					return unlockErr
				}
				return lock.Close()
			}
			return source, nil
		},
	)
	require.Error(t, err)
	require.NotNil(t, prepared,
		"failed cleanup ownership must be returned with the preparation error")
	assert.ErrorIs(t, err, removeErr)
	assert.ErrorIs(t, err, unlockErr)
	var primary *HostError
	require.ErrorAs(t, err, &primary)
	assert.Equal(t, "host-b", primary.Host)
	assert.Equal(t, "prepare", primary.Operation)
	assert.Contains(t, err.Error(), `HTTP host "host-a" cleanup prepared source`)
	assert.DirExists(t, ownedRoot)
	assertMirrorLocked(t, MirrorDir(dataDir, "host-a"))

	require.NoError(t, prepared.Close())
	assert.NoDirExists(t, ownedRoot)
	assertMirrorUnlocked(t, MirrorDir(dataDir, "host-a"))
	require.NoError(t, prepared.Close(), "cleanup retry must remain idempotent")
}

func TestPrepareHTTPSyncRetainsCurrentSourceWhenCleanupFails(t *testing.T) {
	t.Run("single source", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusInternalServerError
		_, hs := newMirrorSync(t, remote, t.TempDir())
		unlockErr := errors.New("transient current-source unlock failure")
		unlockCalls := 0

		source, err := hs.prepare(context.Background(), func(source *PreparedHTTP) {
			source.releaseLock = func(lock *MirrorLockHandle) error {
				unlockCalls++
				if unlockCalls == 1 {
					return unlockErr
				}
				return lock.Close()
			}
		})
		require.Error(t, err)
		require.NotNil(t, source)
		assert.ErrorIs(t, err, unlockErr)
		var statusErr *StatusError
		assert.ErrorAs(t, err, &statusErr)
		assertMirrorLocked(t, MirrorDir(hs.DataDir, hs.Host))

		require.NoError(t, source.Close())
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
	})

	t.Run("aggregate current source", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusInternalServerError
		_, hs := newMirrorSync(t, remote, t.TempDir())
		unlockErr := errors.New("transient aggregate current-source unlock failure")
		unlockCalls := 0

		prepared, err := prepareHTTPSyncs(
			context.Background(), []HTTPSync{hs},
			func(ctx context.Context, hs HTTPSync) (*PreparedHTTP, error) {
				return hs.prepare(ctx, func(source *PreparedHTTP) {
					source.releaseLock = func(lock *MirrorLockHandle) error {
						unlockCalls++
						if unlockCalls <= 2 {
							return unlockErr
						}
						return lock.Close()
					}
				})
			},
		)
		require.Error(t, err)
		require.NotNil(t, prepared)
		assert.ErrorIs(t, err, unlockErr)
		var hostErr *HostError
		require.ErrorAs(t, err, &hostErr)
		assert.Equal(t, hs.Host, hostErr.Host)
		assert.Equal(t, "prepare", hostErr.Operation)
		assert.Contains(t, err.Error(), "cleanup prepared source")
		assertMirrorLocked(t, MirrorDir(hs.DataDir, hs.Host))

		require.NoError(t, prepared.Close())
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
	})
}

func TestHTTPSyncRunHandlesPreparationCleanupOwnership(t *testing.T) {
	t.Run("transient cleanup is completed before return", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusInternalServerError
		_, hs := newMirrorSync(t, remote, t.TempDir())
		unlockErr := errors.New("transient Run unlock failure")
		unlockCalls := 0
		base := hs
		hs.runPrepare = func(ctx context.Context) (*PreparedHTTP, error) {
			return base.prepare(ctx, func(source *PreparedHTTP) {
				source.releaseLock = func(lock *MirrorLockHandle) error {
					unlockCalls++
					if unlockCalls == 1 {
						return unlockErr
					}
					return lock.Close()
				}
			})
		}

		_, err := hs.Run(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, unlockErr)
		var owner *PreparedCleanupError
		assert.False(t, errors.As(err, &owner),
			"successful Run retry must not return cleanup ownership")
		assert.Equal(t, 2, unlockCalls)
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
	})

	t.Run("persistent cleanup ownership is retryable from error", func(t *testing.T) {
		remote := newMirrorTestRemote(t)
		remote.manifestStatus = http.StatusInternalServerError
		_, hs := newMirrorSync(t, remote, t.TempDir())
		unlockErr := errors.New("persistent Run unlock failure")
		unlockCalls := 0
		base := hs
		hs.runPrepare = func(ctx context.Context) (*PreparedHTTP, error) {
			return base.prepare(ctx, func(source *PreparedHTTP) {
				source.releaseLock = func(lock *MirrorLockHandle) error {
					unlockCalls++
					if unlockCalls <= 2 {
						return unlockErr
					}
					return lock.Close()
				}
			})
		}

		_, err := hs.Run(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, unlockErr)
		var statusErr *StatusError
		assert.ErrorAs(t, err, &statusErr)
		var owner *PreparedCleanupError
		require.ErrorAs(t, err, &owner)
		assert.Equal(t, 2, unlockCalls, "Run must not double-close before returning")
		assertMirrorLocked(t, MirrorDir(hs.DataDir, hs.Host))

		require.NoError(t, owner.RetryCleanup())
		assert.Equal(t, 3, unlockCalls)
		assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
		require.NoError(t, owner.RetryCleanup(), "cleanup retry is idempotent")
		assert.Equal(t, 3, unlockCalls)
	})
}

func TestCleanupRegistryBlocksLaterRunWithPreparedCleanupError(t *testing.T) {
	remote := newMirrorTestRemote(t)
	remote.manifestStatus = http.StatusInternalServerError
	_, hs := newMirrorSync(t, remote, t.TempDir())
	unlockErr := errors.New("persistent registry unlock failure")
	unlockCalls := 0
	base := hs
	hs.runPrepare = func(ctx context.Context) (*PreparedHTTP, error) {
		return base.prepare(ctx, func(source *PreparedHTTP) {
			source.releaseLock = func(lock *MirrorLockHandle) error {
				unlockCalls++
				if unlockCalls <= 4 {
					return unlockErr
				}
				return lock.Close()
			}
		})
	}
	var registry CleanupRegistry
	callbacks := 0

	_, err := registry.Run(func() (SyncStats, error) {
		callbacks++
		return hs.Run(context.Background())
	})
	require.Error(t, err)
	var owner *PreparedCleanupError
	require.ErrorAs(t, err, &owner)
	assert.ErrorIs(t, err, unlockErr)
	assert.Equal(t, 3, unlockCalls)
	assertMirrorLocked(t, MirrorDir(hs.DataDir, hs.Host))

	_, err = registry.Run(func() (SyncStats, error) {
		callbacks++
		return SyncStats{}, nil
	})
	var pending *PendingCleanupError
	require.ErrorAs(t, err, &pending)
	var retained *PreparedCleanupError
	require.ErrorAs(t, err, &retained)
	assert.Same(t, owner, retained)
	assert.ErrorIs(t, err, owner)
	assert.ErrorIs(t, err, unlockErr)
	assert.Equal(t, 1, callbacks, "pending cleanup must block the later callback")
	assert.Equal(t, 4, unlockCalls)
	assertMirrorLocked(t, MirrorDir(hs.DataDir, hs.Host))

	_, err = registry.Run(func() (SyncStats, error) {
		callbacks++
		return SyncStats{SessionsSynced: 1}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, callbacks)
	assert.Equal(t, 5, unlockCalls)
	assertMirrorUnlocked(t, MirrorDir(hs.DataDir, hs.Host))
}

func TestPreparedHTTPCloseAttemptMakesSourceUnusableUntilCleanupCompletes(t *testing.T) {
	unlockErr := errors.New("unlock remains pending")
	unlockCalls := 0
	prepared := &PreparedHTTP{
		root: t.TempDir(),
		lock: &MirrorLockHandle{},
		releaseLock: func(*MirrorLockHandle) error {
			unlockCalls++
			if unlockCalls == 1 {
				return unlockErr
			}
			return nil
		},
	}

	require.ErrorIs(t, prepared.Close(), unlockErr)
	_, importErr := prepared.ImportActive(context.Background())
	assert.ErrorContains(t, importErr, "closed")
	_, contributorErr := prepared.RebuildContributor()
	assert.ErrorContains(t, contributorErr, "closed")
	require.NoError(t, prepared.Close())
	assert.Equal(t, 2, unlockCalls)
}

type failingArchiveBody struct{ err error }

func (r failingArchiveBody) Read([]byte) (int, error) { return 0, r.err }
func (failingArchiveBody) Close() error               { return nil }

func TestCleanupRegistryRetainsFailedArchiveSpoolCleanup(t *testing.T) {
	spoolRoot := t.TempDir()
	readErr := errors.New("archive transfer failed")
	removeErr := errors.New("spool removal failed")
	removeCalls := 0
	hs := HTTPSync{removeArchiveSpool: func(path string) error {
		removeCalls++
		if removeCalls <= 2 {
			return removeErr
		}
		return os.RemoveAll(path)
	}}
	registry := new(CleanupRegistry)
	callbacks := 0

	_, err := registry.Run(func() (SyncStats, error) {
		callbacks++
		archive, downloadErr := hs.downloadArchive(
			context.Background(),
			&http.Response{
				StatusCode: http.StatusOK,
				Body:       failingArchiveBody{err: readErr},
			},
			"Downloading archive", spoolRoot,
		)
		assert.Nil(t, archive)
		return SyncStats{}, downloadErr
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, readErr)
	assert.ErrorIs(t, err, removeErr)
	var owner *downloadedArchiveCleanupError
	require.ErrorAs(t, err, &owner)
	assert.Equal(t, 2, removeCalls, "registry retries cleanup before retaining it")

	_, err = registry.Run(func() (SyncStats, error) {
		callbacks++
		return SyncStats{SessionsSynced: 1}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, callbacks)
	assert.Equal(t, 3, removeCalls)
	entries, readDirErr := os.ReadDir(spoolRoot)
	require.NoError(t, readDirErr)
	assert.Empty(t, entries)
}

func TestPrepareHTTPSyncsCacheInvalidationFailureDoesNotMutateMirror(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	path := remote.writeSession(t, "a.jsonl", base, "original")
	dataDir := t.TempDir()
	database, hs := newMirrorSync(t, remote, dataDir)
	hs.Host = "readonly-host"
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	require.NoError(t, prepared.Close())

	local, err := safeRemappedRemotePath(MirrorDir(dataDir, hs.Host), path)
	require.NoError(t, err)
	beforeBytes, err := os.ReadFile(local)
	require.NoError(t, err)
	beforeInfo, err := os.Stat(local)
	require.NoError(t, err)
	requestsBefore := len(remote.archiveRequests)
	remote.writeSession(t, "a.jsonl", base.Add(time.Second),
		"replacement with a different size")
	readonly, err := db.OpenReadOnly(database.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	hs.DB = readonly

	preparedSet, err := PrepareHTTPSyncs(context.Background(), []HTTPSync{hs})
	require.Error(t, err)
	assert.Nil(t, preparedSet)
	var hostErr *HostError
	require.ErrorAs(t, err, &hostErr)
	assert.Equal(t, "readonly-host", hostErr.Host)
	assert.ErrorIs(t, err, db.ErrReadOnly)
	afterBytes, readErr := os.ReadFile(local)
	require.NoError(t, readErr)
	afterInfo, statErr := os.Stat(local)
	require.NoError(t, statErr)
	assert.Equal(t, beforeBytes, afterBytes)
	assert.Equal(t, beforeInfo.ModTime(), afterInfo.ModTime())
	assert.Len(t, remote.archiveRequests, requestsBefore,
		"cache failure must stop before archive download or extraction")
	assertMirrorUnlocked(t, MirrorDir(dataDir, hs.Host))
}

func TestPrepareHTTPSyncSameMtimeSizeChangeRecoversAfterAbortedRebuild(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 11, 13, 0, 0, 0, time.UTC)
	changed := remote.writeSession(t, "changed.jsonl", base,
		"the original message is intentionally much longer")
	deleted := remote.writeSession(t, "deleted.jsonl", base, "retained archive row")
	dataDir := t.TempDir()
	database, hs := newMirrorSync(t, remote, dataDir)
	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, stats.SessionsSynced)
	initial, err := database.ListSessions(
		context.Background(), db.SessionFilter{Limit: 10},
	)
	require.NoError(t, err)
	var changedSessionID string
	for _, session := range initial.Sessions {
		messages, messageErr := database.GetMessages(
			context.Background(), session.ID, 0, 10, true,
		)
		require.NoError(t, messageErr)
		if len(messages) == 1 && strings.Contains(
			messages[0].Content, "intentionally much longer",
		) {
			changedSessionID = session.ID
		}
	}
	require.NotEmpty(t, changedSessionID)

	require.NoError(t, database.ReplaceRemoteSkippedFiles(
		hs.Host, map[string]int64{changed: base.UnixNano()},
	))
	remote.writeSession(t, "changed.jsonl", base, "new")
	require.NoError(t, os.Remove(deleted))
	prepared, err := hs.Prepare(context.Background())
	require.NoError(t, err)
	changedLocal, err := safeRemappedRemotePath(prepared.Root(), changed)
	require.NoError(t, err)
	changedBytes, err := os.ReadFile(changedLocal)
	require.NoError(t, err)
	assert.Contains(t, string(changedBytes), "new")
	deletedLocal, err := safeRemappedRemotePath(prepared.Root(), deleted)
	require.NoError(t, err)
	assert.NoFileExists(t, deletedLocal)
	cache, err := database.LoadRemoteSkippedFiles(hs.Host)
	require.NoError(t, err)
	assert.Empty(t, cache, "cache invalidation commits before mirror mutation")
	require.NoError(t, prepared.Close(), "simulate rebuild abort before import")

	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsSynced)
	page, err := database.ListSessions(context.Background(), db.SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 2,
		"remote deletion must not delete the persistent archive row")
	messages, err := database.GetMessages(
		context.Background(), changedSessionID, 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "new", messages[0].Content)
}

func tarWithoutEndMarker(t *testing.T, name, body string) []byte {
	t.Helper()
	archive := buildHTTPTestTar(t, map[string]string{name: body})
	require.GreaterOrEqual(t, len(archive), tarEndMarkerSize)
	return archive[:len(archive)-tarEndMarkerSize]
}

func setPortableTempDir(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}
}

func assertMirrorLocked(t *testing.T, mirrorRoot string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	lock, err := AcquireMirrorLock(ctx, mirrorRoot)
	if lock != nil {
		require.NoError(t, lock.Close())
	}
	assert.Error(t, err)
}

func assertMirrorUnlocked(t *testing.T, mirrorRoot string) {
	t.Helper()
	lock, err := AcquireMirrorLock(context.Background(), mirrorRoot)
	require.NoError(t, err)
	require.NoError(t, lock.Close())
}
