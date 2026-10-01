package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRawArchiveAcceptanceAndCopy(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	root := RawArchiveRoot{ID: "root", DeviceID: "device", Machine: "example", Provider: "claude", OriginalPath: "/example/sessions"}
	require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
	require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
	changed := root
	changed.OriginalPath = "/different"
	require.Error(t, d.RegisterRawArchiveRoot(ctx, changed))
	changed = root
	changed.ID = "other"
	changed.DeviceID = "other"
	require.Error(t, d.RegisterRawArchiveRoot(ctx, changed))
	f := RawArchiveFile{RootID: root.ID, Path: "session.jsonl", SHA256: strings.Repeat("a", 64), Size: 2, ModTimeNS: 3}
	require.NoError(t, d.RecordRawArchiveFile(ctx, f))
	f.ModTimeNS++
	f.Covered = true
	require.NoError(t, d.RecordRawArchiveFile(ctx, f))
	bad := f
	bad.Size++
	require.Error(t, d.RecordRawArchiveFile(ctx, bad))
	f.SHA256 = strings.Repeat("b", 64)
	require.NoError(t, d.RecordRawArchiveFile(ctx, f))
	source := RawArchiveSource{ManifestID: "m1", RootID: root.ID, SourceKey: "session", OriginalPath: "/example/sessions/session.jsonl", CanonicalJSON: []byte(`{"version":1}`)}
	accepted, err := d.AcceptRawArchiveSource(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, "m1", accepted.Receipt)
	_, err = d.AcceptRawArchiveSource(ctx, source)
	require.NoError(t, err)
	collision := source
	collision.CanonicalJSON = []byte(`{}`)
	_, err = d.AcceptRawArchiveSource(ctx, collision)
	require.Error(t, err)
	next := source
	next.ManifestID = "m2"
	_, err = d.AcceptRawArchiveSource(ctx, next)
	require.Error(t, err)
	next.ParentReceipt = "m1"
	_, err = d.AcceptRawArchiveSource(ctx, next)
	require.NoError(t, err)
	_, err = d.AcceptRawArchiveSource(ctx, source)
	require.NoError(t, err)
	require.NoError(t, d.RecordRawArchiveParse(ctx, "m2", "v1", "parse failed"))
	require.ErrorIs(t, d.RecordRawArchiveParse(ctx, "missing", "v1", ""), sql.ErrNoRows)
	target := testDB(t)
	require.NoError(t, target.CopySyncStateFrom(d.path))
	roots, err := target.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RawArchiveRoot{root}, roots)
	files, err := target.ListRawArchiveFiles(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, files, 2)
	assert.True(t, files[0].Covered)
	generations, err := target.ListRawArchiveSources(ctx, "", 10)
	require.NoError(t, err)
	require.Len(t, generations, 2)
	head, err := target.RawArchiveHead(ctx, root.ID, "session")
	require.NoError(t, err)
	require.NotNil(t, head)
	assert.Equal(t, "m2", head.ManifestID)
	assert.Equal(t, "parse failed", head.ParseError)
	head, err = target.RawArchiveHead(ctx, root.ID, "missing")
	require.NoError(t, err)
	assert.Nil(t, head)
}

func TestRawArchiveSnapshot(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	root := RawArchiveRoot{ID: "root", DeviceID: "device", Machine: "example", Provider: "codex", OriginalPath: "/example"}
	require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
	insertSession(t, d, "session-a", "example")
	path := filepath.Join(t.TempDir(), "snapshot.db")
	require.NoError(t, d.SnapshotTo(ctx, path))
	snapshot, err := Open(ctx, path)
	require.NoError(t, err)
	defer snapshot.Close()
	roots, err := snapshot.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RawArchiveRoot{root}, roots)
	session, err := snapshot.GetSession(ctx, "session-a")
	require.NoError(t, err)
	assert.Equal(t, "session-a", session.ID)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Error(t, d.SnapshotTo(ctx, path))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	missing := filepath.Join(t.TempDir(), "cancelled.db")
	require.ErrorIs(t, d.SnapshotTo(cancelled, missing), context.Canceled)
	assert.NoFileExists(t, missing)
}

func TestRawArchiveMigrationAndLegacyCopy(t *testing.T) {
	ctx := t.Context()
	legacy := testDB(t)
	// Simulate an archive written before these additive tables existed.
	for _, table := range []string{"raw_archive_heads", "raw_archive_sources", "raw_archive_files", "raw_archive_roots"} {
		_, err := legacy.getWriter().ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(t, err)
	}
	target := testDB(t)
	require.NoError(t, target.CopySyncStateFrom(legacy.path))
	roots, err := target.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Empty(t, roots)
	path := legacy.path
	require.NoError(t, legacy.Close())
	reopened, err := Open(ctx, path)
	require.NoError(t, err)
	defer reopened.Close()
	require.NoError(t, reopened.RegisterRawArchiveRoot(ctx, RawArchiveRoot{ID: "root", DeviceID: "device", Machine: "example", Provider: "claude", OriginalPath: "/example"}))
}

func TestRawArchiveReadOnly(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	root := RawArchiveRoot{ID: "root", DeviceID: "device", Machine: "example", Provider: "claude", OriginalPath: "/example"}
	require.NoError(t, d.RegisterRawArchiveRoot(ctx, root))
	readOnly, err := OpenReadOnly(ctx, d.path)
	require.NoError(t, err)
	defer readOnly.Close()
	roots, err := readOnly.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	assert.Equal(t, []RawArchiveRoot{root}, roots)
	require.ErrorIs(t, readOnly.RegisterRawArchiveRoot(ctx, root), ErrReadOnly)
	snapshotPath := filepath.Join(t.TempDir(), "readonly-snapshot.db")
	require.ErrorIs(t, readOnly.SnapshotTo(ctx, snapshotPath), ErrReadOnly)
	assert.NoFileExists(t, snapshotPath)
}
