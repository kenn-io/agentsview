package sessionmaterialize

import (
	"database/sql"
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
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank/docbank"
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
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}, {Name: "directory", Directory: seed}}}
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
	checkDirectoryRouting(t, cfg, receiver, session, cwd)
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
	cfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "fixture", Repository: repository, Snapshot: report.SnapshotID}, {Name: "directory", Directory: seed}}}
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
	checkDirectoryRouting(t, cfg, receiver, session, t.TempDir())
}

func checkDirectoryRouting(t *testing.T, cfg config.Config, receiver *db.DB, session *db.Session, cwd string) {
	t.Helper()
	owners, err := receiver.ArchiveSessionOwners(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, hasDirectoryOwner(owners), "directory attachment must own this source")
	directoryOnly := cfg
	directoryOnly.ArchiveSources = cfg.ArchiveSources[1:]
	result, err := Materialize(t.Context(), directoryOnly, receiver, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Source, "directory:")
	// The imported foreign session's archive URI must retain its exact owner
	// binding; a different root is not an alternate source.
	require.NotNil(t, session.FilePath)
	assert.Contains(t, *session.FilePath, "archive://")
	wrong := *session
	wrongPath := "archive://wrong-root/source"
	wrong.FilePath = &wrongPath
	_, err = Materialize(t.Context(), directoryOnly, receiver, &wrong, cwd)
	require.ErrorContains(t, err, "binding")
	vault, err := docbank.New(t.Context(), docbank.Config{Root: filepath.Join(directoryOnly.ArchiveSources[0].Directory, rawarchive.Directory, "artifacts")})
	require.NoError(t, err)
	busyVault := vault
	t.Cleanup(func() { _ = busyVault.Close() })
	_, err = Materialize(t.Context(), directoryOnly, receiver, session, cwd)
	require.ErrorIs(t, err, docbank.ErrVaultLocked)
	assert.ErrorContains(t, err, "archive is busy")
	require.NoError(t, vault.Close())
	unavailable := cfg
	unavailable.ArchiveSources = append([]config.ArchiveSource(nil), cfg.ArchiveSources...)
	unavailable.ArchiveSources[0].Repository = filepath.Join(t.TempDir(), "missing")
	result, err = Materialize(t.Context(), unavailable, receiver, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Source, "directory:")
	// Create a real snapshot whose selected recovery inventory is invalid.
	repository, err := docbank.OpenBackupRepository(cfg.ArchiveSources[0].Repository)
	require.NoError(t, err)
	reader, err := repository.OpenSnapshot(t.Context(), cfg.ArchiveSources[0].Snapshot)
	require.NoError(t, err)
	var extras []docbank.BackupExtraFile
	for _, name := range []string{"sessions.db", "inventory.json", "recovery-settings.json", "telemetry-install-id"} {
		fileName := filepath.Join(t.TempDir(), name)
		file, err := os.Create(fileName)
		require.NoError(t, err)
		_, err = reader.CopyExtra(t.Context(), "application/"+name, file)
		require.NoError(t, err)
		require.NoError(t, file.Close())
		if name == "inventory.json" {
			require.NoError(t, os.WriteFile(fileName, []byte(`{"version":2}`), 0o600))
		}
		extras = append(extras, docbank.BackupExtraFile{Path: fileName, RecordAs: "application/" + name})
	}
	require.NoError(t, reader.Close())
	vault, err = docbank.New(t.Context(), docbank.Config{Root: filepath.Join(cfg.ArchiveSources[1].Directory, rawarchive.Directory, "artifacts")})
	require.NoError(t, err)
	bad, err := vault.CreateBackup(t.Context(), repository, docbank.BackupOptions{ExtraFiles: extras})
	require.NoError(t, err)
	require.NoError(t, vault.Close())
	var snapshotOwner db.ArchiveSessionProvenance
	for _, owner := range owners {
		if owner.Attachment.SnapshotID == cfg.ArchiveSources[0].Snapshot {
			snapshotOwner = owner
		}
	}
	require.NotEmpty(t, snapshotOwner.ManifestID)
	snapshotOwner.Attachment.SnapshotID = bad.ID
	require.NoError(t, receiver.RegisterArchiveAttachment(t.Context(), snapshotOwner.Attachment, snapshotOwner.Source.ArchiveID))
	require.NoError(t, receiver.SetArchiveAttachmentTargets(t.Context(), snapshotOwner.Attachment, "fixture", []db.ArchiveSourceTarget{{Provenance: snapshotOwner.ArchiveSourceProvenance, Target: "fixture"}}))
	broken := cfg
	broken.ArchiveSources = append([]config.ArchiveSource(nil), cfg.ArchiveSources...)
	broken.ArchiveSources[0].Snapshot = bad.ID
	brokenHome := t.TempDir()
	if session.Agent == "codex" {
		t.Setenv("CODEX_HOME", brokenHome)
	} else {
		t.Setenv("CLAUDE_CONFIG_DIR", brokenHome)
	}
	_, err = Materialize(t.Context(), broken, receiver, session, cwd)
	require.ErrorContains(t, err, "unsupported recovery inventory version")
	entries, err := os.ReadDir(brokenHome)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
