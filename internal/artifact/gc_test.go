package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestGarbageCollectDeletesSupersededArtifactsAfterGrace(t *testing.T) {
	root, oldPaths, livePaths := supersededArtifactFixture(t)
	now := time.Unix(1_800_000_000, 0)
	touchPaths(t, now.Add(-2*time.Hour), oldPaths...)

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: time.Hour,
		Now:   now,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Origins)
	assert.Equal(t, 3, res.Candidates)
	assert.Equal(t, 3, res.Eligible)
	assert.Equal(t, 3, res.Deleted)
	assert.Zero(t, res.KeptByGrace)
	for _, path := range oldPaths {
		assertNoFile(t, path)
	}
	for _, path := range livePaths {
		assertFileExists(t, path)
	}
}

func TestGarbageCollectDryRunLogsAndKeepsArtifacts(t *testing.T) {
	root, oldPaths, _ := supersededArtifactFixture(t)
	now := time.Unix(1_800_000_000, 0)
	touchPaths(t, now.Add(-2*time.Hour), oldPaths...)

	var logs []string
	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:   root,
		Grace:  time.Hour,
		Now:    now,
		DryRun: true,
		Logf: func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Candidates)
	assert.Equal(t, 3, res.Eligible)
	assert.Zero(t, res.Deleted)
	require.NotEmpty(t, logs)
	assert.Contains(t, logs[0], "would delete")
	for _, path := range oldPaths {
		assertFileExists(t, path)
	}
}

func TestGarbageCollectKeepsUnreferencedArtifactsWithinGrace(t *testing.T) {
	root, oldPaths, _ := supersededArtifactFixture(t)
	now := time.Unix(1_800_000_000, 0)
	touchPaths(t, now.Add(-30*time.Minute), oldPaths...)

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: time.Hour,
		Now:   now,
	})
	require.NoError(t, err)

	assert.Equal(t, 3, res.Candidates)
	assert.Zero(t, res.Eligible)
	assert.Equal(t, 3, res.KeptByGrace)
	assert.Zero(t, res.Deleted)
	for _, path := range oldPaths {
		assertFileExists(t, path)
	}
}

func TestGarbageCollectSkipsOriginsWithoutCheckpoints(t *testing.T) {
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	path := filepath.Join(root, origin, KindManifests, hash+manifestExtension)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("orphan"), 0o644))

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: 0,
		Now:   time.Unix(1_800_000_000, 0),
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Origins)
	assert.Equal(t, 1, res.SkippedOrigins)
	assert.Zero(t, res.Candidates)
	assert.Zero(t, res.Deleted)
	assertFileExists(t, path)
}

func TestGarbageCollectKeepsRawReferencedByLiveManifest(t *testing.T) {
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	originRoot := filepath.Join(root, origin)
	for _, dir := range []string{KindCheckpoints, KindManifests, KindSegments, KindRaw} {
		require.NoError(t, os.MkdirAll(filepath.Join(originRoot, dir), 0o755))
	}

	segmentData := []byte("{\"content\":\"hello\",\"ordinal\":0,\"role\":\"user\",\"v\":1}\n")
	segmentHash := hashHex(segmentData)
	require.NoError(t, writeCompressed(
		filepath.Join(originRoot, KindSegments, segmentHash+segmentExtension),
		segmentData,
	))

	liveRaw := []byte("live raw")
	liveRawHash := hashHex(liveRaw)
	require.NoError(t, os.WriteFile(
		filepath.Join(originRoot, KindRaw, liveRawHash),
		liveRaw,
		0o644,
	))
	staleRaw := []byte("stale raw")
	staleRawHash := hashHex(staleRaw)
	staleRawPath := filepath.Join(originRoot, KindRaw, staleRawHash)
	require.NoError(t, os.WriteFile(staleRawPath, staleRaw, 0o644))

	gid := origin + "~sess-1"
	m := manifest{
		Version:         formatVersion,
		Origin:          origin,
		NativeSessionID: "sess-1",
		Session:         db.Session{ID: "sess-1", Machine: origin},
		Segments:        []string{segmentHash},
		RawSource: &rawSourceRef{
			Hash: liveRawHash,
			Size: int64(len(liveRaw)),
		},
		DataVersion: 1,
		Generation:  1,
	}
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	manifestHash := hashHex(manifestData)
	require.NoError(t, writeCompressed(
		filepath.Join(originRoot, KindManifests, manifestHash+manifestExtension),
		manifestData,
	))
	writeCheckpoint(t, originRoot, checkpoint{
		Version:  formatVersion,
		Origin:   origin,
		Sequence: 1,
		Sessions: map[string]string{gid: manifestHash},
	})

	res, err := GarbageCollect(context.Background(), GCOptions{
		Root:  root,
		Grace: 0,
		Now:   time.Unix(1_800_000_000, 0),
	})
	require.NoError(t, err)

	assert.Equal(t, 1, res.Candidates)
	assert.Equal(t, 1, res.Deleted)
	assertNoFile(t, staleRawPath)
	assertFileExists(t, filepath.Join(originRoot, KindRaw, liveRawHash))
}

func supersededArtifactFixture(t *testing.T) (string, []string, []string) {
	t.Helper()
	ctx := context.Background()
	database := testDB(t)
	root := t.TempDir()
	origin := "laptop-a1b2c3"
	gid := origin + "~sess-1"
	originRoot := filepath.Join(root, origin)

	seedSession(t, database, "sess-1", "alpha")
	_, err := Export(ctx, database, root, origin)
	require.NoError(t, err)
	firstCP, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, firstCP)
	firstManifestHash := firstCP.Sessions[gid]
	require.NotEmpty(t, firstManifestHash)
	firstManifest, err := readManifest(originRoot, firstManifestHash)
	require.NoError(t, err)
	require.Len(t, firstManifest.Segments, 1)

	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "planet", ContentLength: 6},
	}))
	_, err = Export(ctx, database, root, origin)
	require.NoError(t, err)
	latestCP, err := readLatestCheckpoint(originRoot)
	require.NoError(t, err)
	require.NotNil(t, latestCP)
	latestManifestHash := latestCP.Sessions[gid]
	require.NotEmpty(t, latestManifestHash)
	require.NotEqual(t, firstManifestHash, latestManifestHash)
	latestManifest, err := readManifest(originRoot, latestManifestHash)
	require.NoError(t, err)
	require.Len(t, latestManifest.Segments, 1)
	require.NotEqual(t, firstManifest.Segments[0], latestManifest.Segments[0])

	oldPaths := []string{
		filepath.Join(originRoot, KindCheckpoints, "cp-0000000001.json"),
		filepath.Join(originRoot, KindManifests, firstManifestHash+manifestExtension),
		filepath.Join(originRoot, KindSegments, firstManifest.Segments[0]+segmentExtension),
	}
	livePaths := []string{
		filepath.Join(originRoot, KindCheckpoints, "cp-0000000002.json"),
		filepath.Join(originRoot, KindManifests, latestManifestHash+manifestExtension),
		filepath.Join(originRoot, KindSegments, latestManifest.Segments[0]+segmentExtension),
	}
	return root, oldPaths, livePaths
}

func touchPaths(t *testing.T, ts time.Time, paths ...string) {
	t.Helper()
	for _, path := range paths {
		require.NoError(t, os.Chtimes(path, ts, ts))
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
}
