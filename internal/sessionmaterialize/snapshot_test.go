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
	checkDirectoryRouting(t, cfg, receiver, session, cwd, map[string]string{
		filepath.Join("projects", "fixture", id+".jsonl"):                          primary,
		filepath.Join("projects", "fixture", id, "tool-results", "tool.txt"):       "tool output",
		filepath.Join("projects", "fixture", id, "subagents", "agent-child.jsonl"): child,
	})
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
	checkDirectoryRouting(t, cfg, receiver, session, t.TempDir(), map[string]string{
		filepath.Join("sessions", "2024", "01", "15", parentName): ancestor,
		filepath.Join("sessions", "2026", "01", "01", pageName):   page,
	})
}

func checkDirectoryRouting(t *testing.T, cfg config.Config, receiver *db.DB, session *db.Session, cwd string, expected map[string]string) {
	t.Helper()
	owners, err := receiver.ArchiveSessionOwners(t.Context(), session.ID)
	require.NoError(t, err)
	require.True(t, hasDirectoryOwner(owners), "directory attachment must own this source")
	directoryOnly := cfg
	directoryOnly.ArchiveSources = cfg.ArchiveSources[1:]
	directory := directoryOnly.ArchiveSources[0].Directory
	beforeDB, _, beforeNames := seedArchiveState(t, directory)
	setHome := func() string {
		t.Helper()
		home := t.TempDir()
		if session.Agent == "codex" {
			t.Setenv("CODEX_HOME", home)
		} else {
			t.Setenv("CLAUDE_CONFIG_DIR", home)
		}
		return home
	}
	checkInstalled := func(home string, result Result) {
		t.Helper()
		var paths []string
		for rel, want := range expected {
			name := filepath.Join(home, rel)
			paths = append(paths, name)
			got, err := os.ReadFile(name)
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		}
		assert.ElementsMatch(t, paths, result.Paths)
	}
	home := setHome()
	result, err := Materialize(t.Context(), directoryOnly, receiver, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Source, "directory:")
	checkInstalled(home, result)
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
	require.ErrorContains(t, err, "archive is busy")
	require.NoError(t, vault.Close())
	afterDB, _, afterNames := seedArchiveState(t, directory)
	assert.Equal(t, beforeDB, afterDB)
	assert.Equal(t, beforeNames, afterNames)
	unavailable := cfg
	unavailable.ArchiveSources = append([]config.ArchiveSource(nil), cfg.ArchiveSources...)
	unavailable.ArchiveSources[0].Repository = filepath.Join(t.TempDir(), "missing")
	home = setHome()
	result, err = Materialize(t.Context(), unavailable, receiver, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Source, "directory:")
	checkInstalled(home, result)
	// Corrupt the already attached snapshot; its valid directory alternate must not mask the error.
	manifest := filepath.Join(cfg.ArchiveSources[0].Repository, "snapshots", cfg.ArchiveSources[0].Snapshot+".mvmanifest")
	original, err := os.ReadFile(manifest)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(manifest, 0o600))
	require.NoError(t, os.WriteFile(manifest, []byte("corrupt"), 0o600))
	t.Cleanup(func() { _ = os.WriteFile(manifest, original, 0o600) })
	brokenHome := setHome()
	_, err = Materialize(t.Context(), cfg, receiver, session, cwd)
	require.ErrorContains(t, err, "parsing snapshot")
	entries, err := os.ReadDir(brokenHome)
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, os.WriteFile(manifest, original, 0o600))
}

// A native fork chain can retain each replay parent under a different captured home.
// Each accepted manifest authenticates its immediate parent bytes.
func TestMaterializeCodexSnapshotForkParentAcrossRoots(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reverted bool
	}{
		{name: "ordinary"},
		{name: "revert skips parent", reverted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSnapshotLineageFixture(t)
			child, parent, grandparent := filepath.Join(f.root, "child"), filepath.Join(f.root, "parent"), filepath.Join(f.root, "grandparent")
			gName := "rollout-2023-01-15T00-00-00-" + lineageG + ".jsonl"
			pName := "rollout-2024-01-15T00-00-00-" + lineageP + ".jsonl"
			key := lineageC
			if tc.reverted {
				key += "_" + lineageR
			}
			cName := "rollout-2026-01-01T00-00-00-" + key + ".jsonl"
			gPath := filepath.Join(grandparent, "sessions", "2023", "01", "15", gName)
			pPath := filepath.Join(parent, "sessions", "2024", "01", "15", pName)
			cPath := filepath.Join(child, "sessions", "2026", "01", "01", cName)
			g := lineagePage(lineageG, "2023-01-15T00:00:00Z", "grandparent", 0, map[string]any{})
			p := lineagePage(lineageP, "2024-01-15T00:00:00Z", "parent", 2, map[string]any{"forked_from_id": lineageG, "forked_from_ordinal_exclusive": 2, "history_base": lineageBase(lineageG, 2, len(g))})
			base, ordinal := lineageBase(lineageP, 4, len(p)), 4
			if tc.reverted {
				base, ordinal = lineageBase(lineageG, 2, len(g)), 2
			}
			c := lineagePage(lineageC, "2026-01-01T00:00:00Z", "child", ordinal, map[string]any{"forked_from_id": lineageP, "forked_from_ordinal_exclusive": ordinal, "history_base": base})
			dbtest.WriteTestFile(t, gPath, []byte(g))
			dbtest.WriteTestFile(t, pPath, []byte(p))
			dbtest.WriteTestFile(t, cPath, []byte(c))
			f.open(f.capture("capture", "", child, parent, grandparent))
			names, contents := []string{gName, pName, cName}, []string{g, p, c}
			if tc.reverted {
				names, contents = []string{gName, cName}, []string{g, c}
			}
			f.materialize(key, []string{child, parent, grandparent}, names, contents, nil)
		})
	}
}
