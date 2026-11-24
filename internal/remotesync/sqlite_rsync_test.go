package remotesync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

type rsyncFakeSource struct{ Source }

func (rsyncFakeSource) SQLiteRsyncOrigin(remotePath string) string { return "fake:" + remotePath }

// fakeRsync installs a sqlite3_rsync stand-in that copies $FAKE_ORIGIN to the
// replica (or fails when the script body says so) and records its argv.
func fakeRsync(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sqlite3_rsync")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" > "+dir+"/argv\n"+body+"\n"), 0o755))
	old := sqlite3RsyncPath
	sqlite3RsyncPath = func() (string, error) { return bin, nil }
	t.Cleanup(func() { sqlite3RsyncPath = old })
	return dir
}

func TestRsyncSQLiteSnapshotsReplacesArchiveFetchForStateDB(t *testing.T) {
	origin := filepath.Join(t.TempDir(), "origin.db")
	require.NoError(t, os.WriteFile(origin, []byte("new pages"), 0o600))
	t.Setenv("FAKE_ORIGIN", origin)
	argvDir := fakeRsync(t, `cp "$FAKE_ORIGIN" "$2"`)

	mirror := t.TempDir()
	const stateDB = "/srv/agent/.hermes/state.db"
	const other = "/srv/agent/.hermes/sessions/a.json"
	mtime := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentHermes: {stateDB}}}
	manifest := Manifest{Files: []ManifestEntry{
		{Path: stateDB, Size: int64(len("new pages")), MtimeNS: mtime.UnixNano()},
		{Path: other, Size: 3, MtimeNS: mtime.UnixNano()},
	}}
	hs := RemoteSync{Host: "devbox", Source: rsyncFakeSource{}}

	left := hs.rsyncSQLiteSnapshots(t.Context(), targets, []string{stateDB, other}, manifest, mirror)

	assert.Equal(t, []string{other}, left, "state.db must not be re-shipped in the archive")
	local, err := safeRemappedRemotePath(mirror, stateDB)
	require.NoError(t, err)
	got, err := os.ReadFile(local)
	require.NoError(t, err)
	assert.Equal(t, "new pages", string(got))
	info, err := os.Stat(local)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(mtime), "replica carries the manifest mtime so the mirror diff sees it as current")
	argv, err := os.ReadFile(filepath.Join(argvDir, "argv"))
	require.NoError(t, err)
	assert.Equal(t, "fake:"+stateDB+" "+local+"\n", string(argv))
	delta, err := diffManifest(mirror, Manifest{Files: manifest.Files[:1]})
	require.NoError(t, err)
	assert.Empty(t, delta.Fetch)
}

func TestRsyncSQLiteSnapshotsFallsBackToArchiveOnFailure(t *testing.T) {
	fakeRsync(t, "exit 3")
	const stateDB = "/srv/agent/.hermes/state.db"
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentHermes: {stateDB}}}
	manifest := Manifest{Files: []ManifestEntry{{Path: stateDB, Size: 1, MtimeNS: 1}}}
	hs := RemoteSync{Host: "devbox", Source: rsyncFakeSource{}}
	left := hs.rsyncSQLiteSnapshots(context.Background(), targets, []string{stateDB}, manifest, t.TempDir())
	assert.Equal(t, []string{stateDB}, left)
}

func TestRsyncSQLiteSnapshotsNeedsCapableSource(t *testing.T) {
	fakeRsync(t, "exit 0")
	const stateDB = "/srv/agent/.hermes/state.db"
	targets := TargetSet{Dirs: map[parser.AgentType][]string{parser.AgentHermes: {stateDB}}}
	manifest := Manifest{Files: []ManifestEntry{{Path: stateDB, Size: 1, MtimeNS: 1}}}
	hs := RemoteSync{Host: "devbox"} // e.g. the HTTP source: no rsync origin
	left := hs.rsyncSQLiteSnapshots(context.Background(), targets, []string{stateDB}, manifest, t.TempDir())
	assert.Equal(t, []string{stateDB}, left)
}
