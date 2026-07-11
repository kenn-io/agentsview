package remotesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

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

func TestHTTPSyncReportsMirrorProgressPhases(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "http remote progress")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	var progress []syncpkg.Progress

	hs.Progress = func(p syncpkg.Progress) {
		progress = append(progress, p)
	}
	stats, err := hs.Run(context.Background())

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
	require.Len(t, remote.archiveRequests, 1)
	assert.Empty(t, remote.archiveRequests[0].DeltaFiles)
}

func progressDetails(progress []syncpkg.Progress) []string {
	out := make([]string, 0, len(progress))
	for _, p := range progress {
		if p.Detail != "" {
			out = append(out, p.Detail)
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
	dir             string // remote-side agent dir (absolute)
	targets         TargetSet
	archiveRequests []ArchiveRequest
	manifestStatus  int    // 0 = serve manifest; else respond with this status
	manifestHTML    bool   // true = mimic an old daemon's SPA catch-all
	onManifest      func() // called before serving a manifest response
	rejectDelta     bool
	ts              *httptest.Server
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
			// requested targets and refuse file-scoped agents.
			var req TargetSet
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.HasFileScopedAgents() {
				http.Error(w, "manifest unavailable for file-scoped agents",
					http.StatusNotImplemented)
				return
			}
			manifest, err := BuildManifest(req)
			require.NoError(t, err)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			require.NoError(t, json.NewEncoder(gz).Encode(manifest))
			require.NoError(t, gz.Close())
		case "/api/v1/remote-sync/archive":
			var req ArchiveRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			remote.archiveRequests = append(remote.archiveRequests, req)
			if remote.rejectDelta && req.DeltaFiles != nil {
				http.Error(w, "delta not allowed", http.StatusForbidden)
				return
			}
			// Serve the requested target subset, as the real handler
			// does after SelectAllowedTargets.
			w.Header().Set("Content-Type", "application/x-tar")
			if req.DeltaFiles != nil {
				require.NoError(t, WriteArchiveFiles(
					w, remote.targets.DeltaAllowedRoots(), req.DeltaFiles))
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

	hs.Full = true
	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	require.Len(t, remote.archiveRequests, 2)
	assert.ElementsMatch(t, []string{changed, added}, remote.archiveRequests[1].DeltaFiles)
	assert.GreaterOrEqual(t, stats.SessionsTotal, 2)

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
	hs.Full = false
	stats, err = hs.Run(context.Background())
	require.NoError(t, err)
	assert.Len(t, remote.archiveRequests, 2)
	assert.Equal(t, 0, stats.SessionsSynced)
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

func TestHTTPSyncRepairMirrorRefreshesBytes(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	path := remote.writeSession(t, "a.jsonl", base, "session a")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	_, err := hs.Run(context.Background())
	require.NoError(t, err)

	// Corrupt the mirror copy preserving size and mtime: invisible to
	// the stat diff.
	local, err := safeRemappedRemotePath(MirrorDir(dataDir, "devbox"), path)
	require.NoError(t, err)
	good, err := os.ReadFile(local)
	require.NoError(t, err)
	info, err := os.Stat(local)
	require.NoError(t, err)
	corrupt := bytes.Repeat([]byte("x"), len(good))
	require.NoError(t, os.WriteFile(local, corrupt, 0o644))
	require.NoError(t, os.Chtimes(local, info.ModTime(), info.ModTime()))

	// A normal sync sees no delta and leaves the corrupt bytes.
	_, err = hs.Run(context.Background())
	require.NoError(t, err)
	stale, err := os.ReadFile(local)
	require.NoError(t, err)
	require.Equal(t, corrupt, stale)
	require.Len(t, remote.archiveRequests, 1, "no-delta sync must not download")

	// Explicit mirror repair forces a full archive refresh and reparse.
	hs.RepairMirror = true
	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	healed, err := os.ReadFile(local)
	require.NoError(t, err)
	assert.Equal(t, good, healed)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Zero(t, stats.Skipped)
	require.Len(t, remote.archiveRequests, 2)
	assert.Nil(t, remote.archiveRequests[1].DeltaFiles,
		"repair must request the full archive, not a delta")
}

func TestHTTPSyncFullReprocessesWithoutFullMirrorDownload(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	require.NoError(t, func() error { _, err := hs.Run(context.Background()); return err }())

	hs.Full = true
	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsTotal)
	assert.Len(t, remote.archiveRequests, 1,
		"full reprocess must not download an unchanged dir-scoped mirror")
}

func TestHTTPSyncFullReprocessesAllSessionsWithoutMirrorRepair(t *testing.T) {
	remote := newMirrorTestRemote(t)
	base := time.Date(2026, 7, 8, 10, 0, 0, 123456789, time.UTC)
	remote.writeSession(t, "a.jsonl", base, "session a")
	dataDir := t.TempDir()
	_, hs := newMirrorSync(t, remote, dataDir)
	_, err := hs.Run(context.Background())
	require.NoError(t, err)

	hs.Full = true
	stats, err := hs.Run(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, stats.SessionsTotal)
	assert.Equal(t, 1, stats.SessionsSynced)
	assert.Zero(t, stats.Skipped)
	assert.Len(t, remote.archiveRequests, 1,
		"full reprocess must not download an unchanged dir-scoped mirror")
}
