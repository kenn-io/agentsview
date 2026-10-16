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
