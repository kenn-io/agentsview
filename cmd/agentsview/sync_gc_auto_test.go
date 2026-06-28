package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/db"
)

// autoGCTestStore builds a data dir whose artifact store contains one superseded
// session manifest/segment/checkpoint (from a first export) plus the live set
// (from a second export after a content change), and mirrors it to a folder
// target. Every file is backdated past the grace window so the unreferenced
// ones are eligible for collection.
func autoGCTestStore(t *testing.T, target string) (dataDir, origin string) {
	t.Helper()
	ctx := context.Background()
	dataDir = t.TempDir()
	origin = "laptop-a1b2c3"
	artifacts := filepath.Join(dataDir, "artifacts")

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	require.NoError(t, database.UpsertSession(db.Session{
		ID:               "sess-1",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		MessageCount:     2,
		UserMessageCount: 1,
		CreatedAt:        "2026-06-14T01:02:03Z",
	}))
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
	_, err = artifact.Export(ctx, database, artifacts, origin)
	require.NoError(t, err)

	// Change content so the next export supersedes the first manifest/segment.
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "planet", ContentLength: 6},
	}))
	_, err = artifact.Export(ctx, database, artifacts, origin)
	require.NoError(t, err)

	require.NoError(t, artifact.CopyUnion(artifacts, target))

	old := time.Now().Add(-72 * time.Hour)
	backdateTree(t, artifacts, old)
	backdateTree(t, target, old)
	return dataDir, origin
}

func backdateTree(t *testing.T, root string, ts time.Time) {
	t.Helper()
	require.NoError(t, filepath.Walk(root, func(p string, _ fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, ts, ts)
	}))
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			n++
		}
	}
	return n
}

func TestAutoGCAfterFolderSyncCollectsBothLocalAndTarget(t *testing.T) {
	target := t.TempDir()
	dataDir, origin := autoGCTestStore(t, target)
	localOriginRoot := filepath.Join(dataDir, "artifacts", origin)
	targetOriginRoot := filepath.Join(target, origin)

	// Both stores start with the superseded set plus the live set.
	for _, root := range []string{localOriginRoot, targetOriginRoot} {
		require.Equal(t, 2, countFiles(t, filepath.Join(root, artifact.KindSegments)))
		require.Equal(t, 2, countFiles(t, filepath.Join(root, artifact.KindManifests)))
		require.Equal(t, 2, countFiles(t, filepath.Join(root, artifact.KindCheckpoints)))
	}

	autoGCAfterFolderSync(context.Background(), dataDir, target, time.Hour)

	// Only the live set remains, in both the local store and the shared target,
	// so a later set-union exchange cannot re-propagate the deleted files.
	for _, root := range []string{localOriginRoot, targetOriginRoot} {
		require.Equal(t, 1, countFiles(t, filepath.Join(root, artifact.KindSegments)))
		require.Equal(t, 1, countFiles(t, filepath.Join(root, artifact.KindManifests)))
		require.Equal(t, 1, countFiles(t, filepath.Join(root, artifact.KindCheckpoints)))
	}
}

func TestAutoGCAfterFolderSyncSkipsNonFolderTarget(t *testing.T) {
	// A non-folder target (an HTTP peer) collects itself; only the local store
	// is collected here. Use a placeholder folder for the mirror that must be
	// left untouched.
	untouched := t.TempDir()
	dataDir, origin := autoGCTestStore(t, untouched)
	localOriginRoot := filepath.Join(dataDir, "artifacts", origin)
	untouchedOriginRoot := filepath.Join(untouched, origin)

	autoGCAfterFolderSync(context.Background(), dataDir, "https://peer:8080", time.Hour)

	// Local store collected down to the live set.
	require.Equal(t, 1, countFiles(t, filepath.Join(localOriginRoot, artifact.KindSegments)))
	// The non-folder target argument is ignored: the placeholder folder keeps
	// both sets because GC never ran against it.
	require.Equal(t, 2, countFiles(t, filepath.Join(untouchedOriginRoot, artifact.KindSegments)))
}
