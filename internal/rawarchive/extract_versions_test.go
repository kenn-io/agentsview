package rawarchive

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawsync"
)

type extractionReads struct {
	rawsync.ObjectStore
	copies int
}

func (s *extractionReads) CopyObject(ctx context.Context, tenant string, ref rawsync.ObjectRef, dst io.Writer) (rawsync.ObjectInfo, error) {
	s.copies++
	return s.ObjectStore.CopyObject(ctx, tenant, ref, dst)
}

func TestExtractConflictingCapturesBeforeReadingContent(t *testing.T) {
	ctx := t.Context()
	opts := captureFixture(t)
	first, err := Capture(ctx, opts)
	require.NoError(t, err)
	firstPath := filepath.Join(opts.Destination, "capture.json")
	database := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(ctx))
	archive, err := Open(ctx, database, t.TempDir(), nil)
	require.NoError(t, err)
	defer archive.Close()
	reads := &extractionReads{ObjectStore: archive.objects}
	archive.objects = reads
	spec, err := LoadImportSpec(ctx, firstPath)
	require.NoError(t, err)
	_, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	_, err = archive.Extract(ctx, filepath.Join(t.TempDir(), "single"), "")
	require.NoError(t, err)
	assert.Positive(t, reads.copies)

	source, err := db.OpenIsolatedContext(ctx, filepath.Join(opts.DataDir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, source.UpsertSession(ctx, db.Session{
		ID: "second-session", Agent: "claude", Machine: first.Source.DeviceID, Project: "project-a",
	}))
	require.NoError(t, source.Close())
	opts.IdentityFrom = firstPath
	opts.Destination = filepath.Join(t.TempDir(), "second")
	second, err := Capture(ctx, opts)
	require.NoError(t, err)
	spec, err = LoadImportSpec(ctx, filepath.Join(opts.Destination, "capture.json"))
	require.NoError(t, err)
	_, err = archive.Import(ctx, spec)
	require.NoError(t, err)
	reads.copies = 0
	target := filepath.Join(t.TempDir(), "ambiguous")
	_, err = archive.Extract(ctx, target, "")
	require.ErrorContains(t, err, "--capture")
	assert.Zero(t, reads.copies, "conflicting versions must be detected before reading retained content")
	assert.NoDirExists(t, target)
	for _, captureID := range []string{first.CaptureID, second.CaptureID} {
		target := filepath.Join(t.TempDir(), "selected")
		_, err := archive.Extract(ctx, target, captureID)
		require.NoError(t, err)
		recovered, err := LoadCapture(ctx, filepath.Join(target, "capture.json"))
		require.NoError(t, err)
		assert.Equal(t, captureID, recovered.CaptureID)
	}
}
