package remotesync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// paddedSource lists the remote's files plus enough made-up new files to
// pass the fetch cap, like a host whose change is too large to list.
type paddedSource struct {
	dirSource
	padDir string
}

func (s paddedSource) Manifest(
	ctx context.Context, targets TargetSet, visit func(ManifestEntry) error,
) (bool, error) {
	manifest, err := BuildManifest(ctx, targets)
	if err != nil {
		return false, err
	}
	for i := range mirrorFetchMaxBytes/4096 + 1 {
		name := filepath.Join(s.padDir, fmt.Sprintf("%08d-", i))
		manifest.Files = append(manifest.Files, ManifestEntry{
			Path: name + strings.Repeat("a", 4096-len(name)), Size: 1, MtimeNS: 1,
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

func TestRemoteSyncReplacesTheMirrorWhenTheChangeIsTooLargeToList(t *testing.T) {
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

	stats, err := RemoteSync{
		Host: "devbox", DataDir: dataDir, DB: database,
		Source: paddedSource{dirSource: source, padDir: filepath.Join(remoteDir, "padding")},
	}.Run(t.Context())

	require.NoError(t, err)
	assert.Equal(t, FullImportJournalOverflow, stats.FullReason)
	assert.NoFileExists(t, stale)
	added, err := safeRemappedRemotePath(mirrorRoot, filepath.Join(remoteDir, "project-b", "project-b.jsonl"))
	require.NoError(t, err)
	assert.FileExists(t, added)
	sessions, err := database.ListSessions(t.Context(), db.SessionFilter{})
	require.NoError(t, err)
	assert.Len(t, sessions.Sessions, 2)
}
