package remotesync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// dirSource serves one remote directory tree in-process with the same
// package functions the remote side uses.
type dirSource struct {
	targets  TargetSet
	encoding ArchiveEncoding
}

func (s dirSource) Targets(context.Context) (TargetSet, error) { return s.targets, nil }

func (s dirSource) Manifest(
	ctx context.Context, targets TargetSet, visit func(ManifestEntry) error,
) (bool, error) {
	manifest, err := BuildManifest(ctx, targets)
	if err != nil {
		return false, err
	}
	for _, entry := range manifest.Files {
		if err := visit(entry); err != nil {
			return false, err
		}
	}
	return true, nil
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

// paddedSource lists the remote's files plus enough made-up new files that
// the fetch list passes its memory limit and the journal's entry limit. It
// records the paths of every delta archive request and can refuse them.
type paddedSource struct {
	dirSource
	padDir      string
	refuseDelta bool
	deltas      *[]int
}

func (s paddedSource) Manifest(
	ctx context.Context, targets TargetSet, visit func(ManifestEntry) error,
) (bool, error) {
	manifest, err := BuildManifest(ctx, targets)
	if err != nil {
		return false, err
	}
	// Short enough for any filesystem, and enough of them to need two
	// batches.
	for size, i := 0, 0; size <= mirrorBatchBytes; i++ {
		path := filepath.Join(
			s.padDir, fmt.Sprintf("%03d", i/1000),
			fmt.Sprintf("%08d-", i)+strings.Repeat("a", 200),
		)
		size += len(path)
		manifest.Files = append(manifest.Files, ManifestEntry{
			Path: path, Size: 1, MtimeNS: 1,
		})
	}
	sortManifestByMirrorPath(manifest.Files)
	for _, entry := range manifest.Files {
		if err := visit(entry); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (s paddedSource) Archive(ctx context.Context, request ArchiveRequest) (*ArchiveStream, error) {
	if request.DeltaFiles != nil {
		size := 0
		for _, path := range request.DeltaFiles {
			size += len(path)
		}
		*s.deltas = append(*s.deltas, size)
		if s.refuseDelta {
			return nil, &StatusError{Code: 400, Status: "400 Bad Request"}
		}
	}
	return s.dirSource.Archive(ctx, request)
}

func TestRemoteSyncAppliesAChangeTooLargeToHoldInMemory(t *testing.T) {
	tests := []struct {
		name        string
		refuseDelta bool
		wantDeltas  int
	}{
		{"in delta batches", false, 2},
		{"from the full archive after a refused delta", true, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remoteDir := filepath.Join(t.TempDir(), "claude-projects")
			writeSession := func(project, message string) {
				path := filepath.Join(remoteDir, project, project+".jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(
					testjsonl.NewSessionBuilder().
						AddClaudeUser("2024-01-01T00:00:00Z", message).
						String(),
				), 0o644))
			}
			writeSession("project-a", "first session")
			database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			dataDir := t.TempDir()
			source := dirSource{targets: TargetSet{Dirs: map[parser.AgentType][]string{
				parser.AgentClaude: {remoteDir},
			}}}
			_, err = RemoteSync{Host: "devbox", DataDir: dataDir, DB: database, Source: source}.Run(t.Context())
			require.NoError(t, err)
			mirrorRoot := MirrorDir(dataDir, "devbox")
			stale := writeMirrorFile(t, mirrorRoot,
				filepath.Join(remoteDir, "gone", "session.jsonl"), "old",
				time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
			writeSession("project-b", "second session")
			// The manifest lists this padding file as changed, but the remote
			// has no such file, as if it vanished after the manifest.
			padDir := filepath.Join(remoteDir, "padding")
			vanished := writeMirrorFile(t, mirrorRoot,
				filepath.Join(padDir, "000", "00000000-"+strings.Repeat("a", 200)), "old",
				time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC))
			var deltas []int
			var progress []syncpkg.Progress

			stats, err := RemoteSync{
				Host: "devbox", DataDir: dataDir, DB: database,
				Source: paddedSource{
					dirSource: source, padDir: padDir,
					refuseDelta: tt.refuseDelta, deltas: &deltas,
				},
				Progress: func(p syncpkg.Progress) { progress = append(progress, p) },
			}.Run(t.Context())

			require.NoError(t, err)
			assert.Equal(t, FullImportJournalOverflow, stats.FullReason)
			assert.Len(t, deltas, tt.wantDeltas)
			for _, size := range deltas {
				assert.LessOrEqual(t, size, mirrorBatchBytes)
			}
			details := uniqueProgressDetails(progress)
			assert.Contains(t, details, "Planning import from devbox: every session will be re-read")
			batch := regexp.MustCompile(
				`^Downloading batch (\d) of 2 \((\d+) of (\d+) changed files\) from devbox$`,
			)
			var batches []string
			for _, detail := range details {
				if m := batch.FindStringSubmatch(detail); m != nil {
					batches = append(batches, m[1])
					if m[1] == "2" {
						assert.Equal(t, m[3], m[2], "the last batch completes the change")
					}
				}
			}
			fallback := "devbox refused a partial download; downloading the full session archive instead"
			if tt.refuseDelta {
				// The first batch is refused before any of it downloads.
				assert.Empty(t, batches)
				assert.Contains(t, details, fallback)
			} else {
				assert.Equal(t, []string{"1", "2"}, batches)
				assert.NotContains(t, details, fallback)
			}
			assert.NoFileExists(t, stale)
			assert.NoFileExists(t, vanished)
			added, err := safeRemappedRemotePath(mirrorRoot, filepath.Join(remoteDir, "project-b", "project-b.jsonl"))
			require.NoError(t, err)
			assert.FileExists(t, added)
			sessions, err := database.ListSessions(t.Context(), db.SessionFilter{})
			require.NoError(t, err)
			assert.Len(t, sessions.Sessions, 2)
			// The delta lists' temporary files are gone.
			entries, err := os.ReadDir(filepath.Dir(mirrorRoot))
			require.NoError(t, err)
			for _, entry := range entries {
				assert.NotContains(t, entry.Name(), "-fetch-")
				assert.NotContains(t, entry.Name(), "-deletions-")
			}
		})
	}
}
