package sessionmaterialize

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Capture and attach real synthetic sources so publication is exercised through
// the same independent parent and subagent manifests as a saved archive.
func TestMaterializeSnapshotInstallsCapturedCompanions(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	projects := filepath.Join(root, "claude", "projects")
	primary := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "parent fixture", id).String()
	child := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "child fixture", "agent-child").String()
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id+".jsonl"), []byte(primary))
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "tool-results", "tool.txt"), []byte("tool output"))
	dbtest.WriteTestFile(t, filepath.Join(projects, "project", id, "subagents", "agent-child.jsonl"), []byte(child))
	capture := filepath.Join(root, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "fixture"}
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Settings: settings, Roots: []rawarchive.RootSpec{{Provider: "claude", Path: projects}}})
	require.NoError(t, err)
	seed := filepath.Join(root, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)

	repository := filepath.Join(root, "repository")
	report, err := archive.Backup(ctx, repository, settings, "fixture")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	require.NoError(t, os.RemoveAll(projects))

	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := archivesource.NewManager(ctx, cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Drain(ctx, nil)
	require.NoError(t, err)
	session, err := receiver.GetSessionFull(ctx, device+"~"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	home, scratch, cwd := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	result, err := Materialize(ctx, cfg, receiver, session, cwd)
	require.NoError(t, err)
	assert.Equal(t, "snapshot:"+report.SnapshotID, result.Source)
	assert.Equal(t, "installed", result.Status)
	dest := filepath.Join(home, "projects", "fixture")
	for name, want := range map[string]string{
		filepath.Join(dest, id+".jsonl"):                          primary,
		filepath.Join(dest, id, "tool-results", "tool.txt"):       "tool output",
		filepath.Join(dest, id, "subagents", "agent-child.jsonl"): child,
	} {
		content, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, want, string(content))
	}
	result, err = Materialize(ctx, cfg, receiver, session, cwd)
	require.NoError(t, err)
	assert.Equal(t, "unchanged", result.Status)
	entries, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, entries)
	bad := *session
	bad.Machine = "wrong-device"
	_, err = Materialize(ctx, cfg, receiver, &bad, cwd)
	require.ErrorContains(t, err, "archive session identity differs")
	require.NoError(t, receiver.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE archive_session_provenance SET native_session_id=? WHERE session_id=?`, "00000000-0000-4000-8000-000000000000", session.ID)
		return err
	}))
	_, err = Materialize(ctx, cfg, receiver, session, cwd)
	require.ErrorContains(t, err, "archive session identity differs")
}

func TestMaterializeCodexSnapshotLineage(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	codexHome := filepath.Join(root, "codex")
	live := filepath.Join(codexHome, "sessions")
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"))
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "question", "2026-01-01T00:00:01Z"))
	dbtest.WriteTestFile(t, filepath.Join(live, pageName), []byte(page))
	dbtest.WriteTestFile(t, filepath.Join(codexHome, "archived_sessions", parentName), []byte(ancestor))
	capture := filepath.Join(root, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "fixture"}
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Settings: settings, Roots: []rawarchive.RootSpec{{Provider: "codex", Path: live}}})
	require.NoError(t, err)
	seed := filepath.Join(root, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err = db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	repository := filepath.Join(root, "repository")
	report, err := archive.Backup(ctx, repository, settings, "fixture")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, database.Close())
	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	manager, err := archivesource.NewManager(ctx, cfg, receiver, engine, nil)
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Drain(ctx, nil)
	require.NoError(t, err)
	session, err := receiver.GetSessionFull(ctx, device+"~codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, session)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SESSIONS_DIR", "")
	result, err := Materialize(ctx, cfg, receiver, session, t.TempDir())
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	assert.Equal(t, "snapshot:"+report.SnapshotID, result.Source)
	assert.Equal(t, parentName, filepath.Base(result.Paths[0]))
	assert.Equal(t, pageName, filepath.Base(result.Paths[1]))
}

// A native fork can retain its replay parent under a different captured home.
// The accepted leaf manifest, rather than an unrelated root scan, owns that byte reference.
func TestMaterializeCodexSnapshotForkParentAcrossRoots(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	require.NoError(t, database.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	childHome, parentHome := filepath.Join(root, "child"), filepath.Join(root, "parent")
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	parentPath := filepath.Join(parentHome, "sessions", "2024", "01", "15", parentName)
	childPath := filepath.Join(childHome, "sessions", "2026", "01", "01", pageName)
	ancestor := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}),
		testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"),
	)
	page := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{
			"forked_from_id": parent, "forked_from_ordinal_exclusive": 2, "history_mode": "paginated",
			"history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)},
		}),
		testjsonl.CodexMsgJSON("user", "child", "2026-01-01T00:00:01Z"),
	)
	dbtest.WriteTestFile(t, parentPath, []byte(ancestor))
	dbtest.WriteTestFile(t, childPath, []byte(page))
	capture := filepath.Join(root, "capture")
	settings := rawarchive.RecoverySettings{LocalMachineName: "fixture"}
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Settings: settings, Roots: []rawarchive.RootSpec{{Provider: "codex", Path: childHome}, {Provider: "codex", Path: parentHome}}})
	require.NoError(t, err)
	seed := filepath.Join(root, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	saved, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = saved.Close() })
	archive, err := rawarchive.Open(ctx, saved, seed, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = archive.Close() })

	roots, err := saved.ListRawArchiveRoots(ctx)
	require.NoError(t, err)
	var childRoot, parentRoot db.RawArchiveRoot
	for _, item := range roots {
		switch item.OriginalPath {
		case childHome:
			childRoot = item
		case parentHome:
			parentRoot = item
		}
	}
	require.NotEmpty(t, childRoot.ID)
	require.NotEmpty(t, parentRoot.ID)
	require.NotEqual(t, childRoot.ID, parentRoot.ID)
	assert.Equal(t, device, childRoot.DeviceID)
	assert.Equal(t, device, parentRoot.DeviceID)
	source, err := saved.RawArchiveSourceByKey(ctx, childRoot.ID, "codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, source)
	assert.Equal(t, "codex:"+thread, source.SourceKey)
	assert.Equal(t, childPath, source.OriginalPath)
	archiveID, err := saved.GetArchiveID(ctx)
	require.NoError(t, err)
	identity, err := rawsync.NewAuthIdentity(archiveID, device)
	require.NoError(t, err)
	manifest, err := rawsync.ParseCanonicalManifest(identity, source.ManifestID, source.CanonicalJSON, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	parentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(ancestor)))
	var parentEntries int
	for _, entry := range manifest.Manifest.Entries {
		if filepath.Base(entry.Path) == parentName {
			parentEntries++
			assert.Equal(t, int64(len(ancestor)), entry.Length)
			assert.Equal(t, []rawsync.ObjectRef{{SHA256: parentHash, Length: int64(len(ancestor))}}, entry.Objects)
		}
	}
	require.Equal(t, 1, parentEntries, "accepted leaf manifest must authenticate the external parent")
	files, err := saved.ListRawArchiveFiles(ctx, 0, 1000)
	require.NoError(t, err)
	var parentRows int
	for _, file := range files {
		if file.SHA256 == parentHash {
			parentRows++
			assert.Equal(t, parentRoot.ID, file.RootID)
			assert.Equal(t, int64(len(ancestor)), file.Size)
			assert.Equal(t, parentName, filepath.Base(file.Path))
		}
	}
	require.Equal(t, 1, parentRows, "parent inventory must belong only to its captured root")

	repository := filepath.Join(root, "repository")
	report, err := archive.Backup(ctx, repository, settings, "fixture")
	require.NoError(t, err)
	require.NoError(t, archive.Close())
	require.NoError(t, saved.Close())
	require.NoError(t, os.RemoveAll(childHome))
	require.NoError(t, os.RemoveAll(parentHome))
	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	t.Cleanup(engine.Close)
	manager, err := archivesource.NewManager(ctx, cfg, receiver, engine, nil)
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	_, err = manager.Drain(ctx, nil)
	require.NoError(t, err)
	session, err := receiver.GetSessionFull(ctx, device+"~codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, session)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CODEX_SESSIONS_DIR", "")
	result, err := Materialize(ctx, cfg, receiver, session, t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "snapshot:"+report.SnapshotID, result.Source)
	assert.Equal(t, session.ID, result.RequestedID)
	assert.Equal(t, pageName, result.InstalledPage)
	require.Len(t, result.Paths, 2)
	for i, want := range []struct{ name, content string }{{parentName, ancestor}, {pageName, page}} {
		assert.Equal(t, want.name, filepath.Base(result.Paths[i]))
		got, err := os.ReadFile(result.Paths[i])
		require.NoError(t, err)
		assert.Equal(t, want.content, string(got))
	}
}
