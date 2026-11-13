package sessionmaterialize

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawarchive"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/docbank/docbank"
)

func seedRetainedFixture(t *testing.T, root, data string, roots []rawarchive.RootSpec) (string, *db.DB, func() error) {
	t.Helper()
	ctx := t.Context()
	capture := filepath.Join(root, "capture")
	_, err := rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: capture, Roots: roots, Settings: rawarchive.RecoverySettings{LocalMachineName: "fixture"}})
	require.NoError(t, err)
	seed := filepath.Join(root, "seed")
	_, err = rawarchive.Seed(ctx, filepath.Join(capture, "capture.json"), seed, nil)
	require.NoError(t, err)
	database, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, database.Close()) })
	archive, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	archiveOpen := true
	closeArchive := func() error {
		if !archiveOpen {
			return nil
		}
		archiveOpen = false
		return archive.Close()
	}
	t.Cleanup(func() { assert.NoError(t, closeArchive()) })
	_, err = archive.Reparse(ctx, rawarchive.ReparseOptions{All: true, ScratchBytes: 1 << 20})
	require.NoError(t, err)
	return seed, database, closeArchive
}

func TestMaterializeRetainedCodexLineage(t *testing.T) {
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
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"))
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "question", "2026-01-01T00:00:01Z"))
	dbtest.WriteTestFile(t, filepath.Join(codexHome, "sessions", pageName), []byte(page))
	dbtest.WriteTestFile(t, filepath.Join(codexHome, "archived_sessions", parentName), []byte(ancestor))
	seed, database, closeArchive := seedRetainedFixture(t, root, data, []rawarchive.RootSpec{{Provider: "codex", Path: codexHome}})
	session, err := database.GetSessionFull(ctx, "codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, session)
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CODEX_SESSIONS_DIR", "")
	cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {filepath.Join(codexHome, "sessions"), filepath.Join(codexHome, "archived_sessions")}}}
	cwd := t.TempDir()
	configured, err := Materialize(ctx, cfg, database, session, cwd)
	require.NoError(t, err, "configured original must remain usable while the vault is busy")
	assert.NotContains(t, configured.Source, "retained:")
	// The configured leaf is readable, but its missing parent and a busy vault
	// must both remain visible in the public failure without publishing files.
	require.NoError(t, os.Remove(filepath.Join(codexHome, "archived_sessions", parentName)))
	t.Run("missing ancestor while vault busy", func(t *testing.T) {
		busyHome := t.TempDir()
		t.Setenv("CODEX_HOME", busyHome)
		_, err := Materialize(t.Context(), cfg, database, session, cwd)
		require.ErrorIs(t, err, docbank.ErrVaultLocked)
		assert.ErrorContains(t, err, parent)
		assert.ErrorContains(t, err, "archive is busy")
		entries, err := os.ReadDir(busyHome)
		require.NoError(t, err)
		assert.Empty(t, entries)
	})
	require.NoError(t, closeArchive())
	// Recovery must install the entire retained chain after the owner releases it.
	t.Setenv("CODEX_HOME", t.TempDir())
	retainedChain, err := Materialize(ctx, cfg, database, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, retainedChain.Source, "retained:")
	require.Len(t, retainedChain.Paths, 2)
	for i, want := range []string{ancestor, page} {
		got, err := os.ReadFile(retainedChain.Paths[i])
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}
	// Removing a configured root, while the original remains on disk, may use
	// only the explicitly bound retained source.
	unconfigured := cfg
	unconfigured.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {t.TempDir()}}
	t.Setenv("CODEX_HOME", t.TempDir())
	result, err := Materialize(ctx, unconfigured, database, session, cwd)
	require.NoError(t, err)
	assert.Contains(t, result.Source, "retained:")
	// A configured lexical path escaping its root through a link remains fatal.
	t.Run("configured symlink escape", func(t *testing.T) {
		escapeRoot := t.TempDir()
		link := filepath.Join(escapeRoot, "sessions")
		if err := os.Symlink(filepath.Join(codexHome, "sessions"), link); err != nil {
			t.Skipf("directory symlinks unavailable: %v", err)
		}
		escaped := *session
		escapePath := filepath.Join(link, pageName)
		escaped.FilePath = &escapePath
		escapeCfg := cfg
		escapeCfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {escapeRoot}}
		_, err := Materialize(t.Context(), escapeCfg, database, &escaped, cwd)
		require.ErrorContains(t, err, "outside configured root")
	})
	require.NoError(t, os.Remove(filepath.Join(codexHome, "sessions", pageName)))
	home = t.TempDir()
	t.Setenv("CODEX_HOME", home)
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(page)))
	blob := filepath.Join(seed, rawarchive.Directory, "artifacts", "blobs", hash[:2], hash)
	original, err := os.ReadFile(blob)
	require.NoError(t, err)
	require.NotEmpty(t, original)
	corrupt := append([]byte(nil), original...)
	corrupt[0] ^= 1
	require.NoError(t, os.WriteFile(blob, corrupt, 0o600))
	_, err = Materialize(ctx, cfg, database, session, cwd)
	require.ErrorIs(t, err, artifact.ErrArtifactCorrupt)
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, os.WriteFile(blob, original, 0o600))
	result, err = Materialize(ctx, cfg, database, session, cwd)
	require.NoError(t, err)
	require.Len(t, result.Paths, 2)
	assert.Contains(t, result.Source, "retained:")
	for i, want := range []string{ancestor, page} {
		got, err := os.ReadFile(result.Paths[i])
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}
}

func TestMaterializeClaudeConfiguredBeforeRetained(t *testing.T) {
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
	name := filepath.Join(projects, "project", id+".jsonl")
	original := testjsonl.NewSessionBuilder().AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "configured original", id).String()
	dbtest.WriteTestFile(t, name, []byte(original))
	seed, database, closeArchive := seedRetainedFixture(t, root, data, []rawarchive.RootSpec{{Provider: "claude", Path: projects}})
	session, err := database.GetSessionFull(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, session)
	cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {projects}}}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_PROJECT_DIR_NAME", "fixture")
	result, err := Materialize(ctx, cfg, database, session, t.TempDir())
	require.NoError(t, err, "configured original must remain usable while the vault is busy")
	assert.Equal(t, name, result.Source)
	require.NoError(t, closeArchive())
	require.NoError(t, os.Remove(name))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	result, err = Materialize(ctx, cfg, database, session, t.TempDir())
	require.NoError(t, err)
	assert.Contains(t, result.Source, "retained:")
}

// Capture records the external replay parent in the leaf's own manifest even
// though that parent's catalog row belongs to its separate configured root.
func TestMaterializeRetainedCodexForkParentAcrossRoots(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	ctx := t.Context()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(data, 0o700))
	initial, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = initial.Close() })
	require.NoError(t, initial.Close())
	dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
	childRoot, parentRoot := filepath.Join(root, "child"), filepath.Join(root, "parent")
	parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
	pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
	ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "ancestor", "2024-01-15T00:00:01Z"))
	page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"forked_from_id": parent, "forked_from_ordinal_exclusive": 2, "history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "child", "2026-01-01T00:00:01Z"))
	dbtest.WriteTestFile(t, filepath.Join(childRoot, "sessions", pageName), []byte(page))
	dbtest.WriteTestFile(t, filepath.Join(parentRoot, "sessions", parentName), []byte(ancestor))
	seed, database, closeArchive := seedRetainedFixture(t, root, data, []rawarchive.RootSpec{{Provider: "codex", Path: childRoot}, {Provider: "codex", Path: parentRoot}})
	require.NoError(t, closeArchive())
	// A later independent capture adds an unrelated same-rollout copy without
	// changing the already accepted leaf manifest.
	thirdRoot := filepath.Join(root, "third")
	unrelated := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/elsewhere", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "unrelated", "2024-01-15T00:00:01Z"))
	dbtest.WriteTestFile(t, filepath.Join(thirdRoot, "sessions", parentName), []byte(unrelated))
	thirdCapture := filepath.Join(root, "third-capture")
	_, err = rawarchive.Capture(ctx, rawarchive.CaptureOptions{DataDir: data, Destination: thirdCapture, Roots: []rawarchive.RootSpec{{Provider: "codex", Path: thirdRoot}}, Settings: rawarchive.RecoverySettings{LocalMachineName: "fixture"}})
	require.NoError(t, err)
	spec, err := rawarchive.LoadImportSpec(ctx, filepath.Join(thirdCapture, "capture.json"))
	require.NoError(t, err)
	later, err := rawarchive.Open(ctx, database, seed, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = later.Close() })
	_, err = later.Import(ctx, spec)
	require.NoError(t, err)
	require.NoError(t, later.Close())
	session, err := database.GetSessionFull(ctx, "codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, session)
	var manifestID, childID, archiveID string
	var canonical []byte
	require.NoError(t, database.Reader().QueryRowContext(ctx, `SELECT s.manifest_id,s.root_id,s.canonical_json,(SELECT value FROM archive_metadata WHERE key='archive_id') FROM raw_archive_sources s JOIN raw_archive_sessions b ON b.root_id=s.root_id AND b.source_key=s.source_key WHERE b.session_id=?`, session.ID).Scan(&manifestID, &childID, &canonical, &archiveID))
	identity, err := rawsync.NewAuthIdentity(archiveID, device)
	require.NoError(t, err)
	manifest, err := rawsync.ParseCanonicalManifest(identity, manifestID, canonical, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	found := false
	for _, entry := range manifest.Manifest.Entries {
		for _, ref := range entry.Objects {
			assert.NotEqual(t, fmt.Sprintf("%x", sha256.Sum256([]byte(unrelated))), ref.SHA256)
		}
		if filepath.Base(entry.Path) == parentName {
			found = true
			require.Len(t, entry.Objects, 1)
			assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(ancestor))), entry.Objects[0].SHA256)
			assert.Equal(t, int64(len(ancestor)), entry.Length)
		}
	}
	require.True(t, found, "leaf manifest must authenticate its external parent")
	var parentID string
	require.NoError(t, database.Reader().QueryRowContext(ctx, `SELECT root_id FROM raw_archive_files WHERE path LIKE ? AND sha256=?`, "%"+parentName, fmt.Sprintf("%x", sha256.Sum256([]byte(ancestor)))).Scan(&parentID))
	assert.NotEqual(t, childID, parentID)
	var thirdID string
	require.NoError(t, database.Reader().QueryRowContext(ctx, `SELECT root_id FROM raw_archive_files WHERE path LIKE ? AND sha256=?`, "%"+parentName, fmt.Sprintf("%x", sha256.Sum256([]byte(unrelated)))).Scan(&thirdID))
	assert.NotEqual(t, childID, thirdID)
	assert.NotEqual(t, parentID, thirdID)
	require.NoError(t, os.RemoveAll(childRoot))
	require.NoError(t, os.RemoveAll(parentRoot))
	require.NoError(t, os.RemoveAll(thirdRoot))
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CODEX_SESSIONS_DIR", "")
	cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {filepath.Join(childRoot, "sessions"), filepath.Join(parentRoot, "sessions")}}}
	t.Run("local retained", func(t *testing.T) {
		result, err := Materialize(t.Context(), cfg, database, session, t.TempDir())
		require.NoError(t, err)
		assert.Contains(t, result.Source, "retained:")
		require.Len(t, result.Paths, 2)
		for i, want := range []string{ancestor, page} {
			got, err := os.ReadFile(result.Paths[i])
			require.NoError(t, err)
			assert.Equal(t, want, string(got))
		}
	})
	require.NoError(t, database.Close())
	receiver := dbtest.OpenTestDB(t)
	_, err = receiver.EnsureInstallationIdentity(ctx, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	directoryCfg := config.Config{DataDir: filepath.Dir(receiver.Path()), ArchiveContent: config.ArchiveContentFull, ArchiveSources: []config.ArchiveSource{{Name: "directory", Directory: seed}}}
	engine := syncer.NewEngine(ctx, receiver, syncer.EngineConfig{Ephemeral: true})
	t.Cleanup(engine.Close)
	manager, err := archivesource.NewManager(ctx, directoryCfg, receiver, engine, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Close() })
	_, err = manager.Drain(ctx, nil)
	require.NoError(t, err)
	foreign, err := receiver.GetSessionFull(ctx, device+"~codex:"+thread)
	require.NoError(t, err)
	require.NotNil(t, foreign)
	// The attachment reader is closed before the saved database is checkpointed.
	checkpointed, err := db.OpenIsolatedContext(ctx, filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = checkpointed.Close() })
	require.NoError(t, checkpointed.Close())
	beforeDB, err := os.ReadFile(filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	beforeEntries, err := os.ReadDir(seed)
	require.NoError(t, err)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Stat(filepath.Join(seed, "sessions.db") + suffix)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	result, err := Materialize(ctx, directoryCfg, receiver, foreign, t.TempDir())
	require.NoError(t, err)
	assert.Contains(t, result.Source, "directory:")
	afterDB, err := os.ReadFile(filepath.Join(seed, "sessions.db"))
	require.NoError(t, err)
	assert.Equal(t, beforeDB, afterDB)
	afterEntries, err := os.ReadDir(seed)
	require.NoError(t, err)
	var beforeNames, afterNames []string
	for _, entry := range beforeEntries {
		beforeNames = append(beforeNames, entry.Name())
	}
	for _, entry := range afterEntries {
		afterNames = append(afterNames, entry.Name())
	}
	assert.Equal(t, beforeNames, afterNames)
	require.Len(t, result.Paths, 2)
	for i, want := range []string{ancestor, page} {
		got, err := os.ReadFile(result.Paths[i])
		require.NoError(t, err)
		assert.Equal(t, want, string(got))
	}
}

// Root rows retain native ranking; an ancestry reference without authenticated
// capture evidence remains unavailable rather than searching other roots.
func TestMaterializeRetainedCodexRootRankAndMissingAncestor(t *testing.T) {
	const device = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const parent = "11111111-1111-4111-8111-111111111111"
	const thread = "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name       string
		withParent bool
	}{
		{"live root outranks archive", true},
		{"missing manifest ancestor", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			data := filepath.Join(root, "data")
			require.NoError(t, os.MkdirAll(data, 0o700))
			initial, err := db.OpenIsolatedContext(ctx, filepath.Join(data, "sessions.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = initial.Close() })
			require.NoError(t, initial.Close())
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(device))
			home := filepath.Join(root, "codex")
			parentName := "rollout-2024-01-15T00-00-00-" + parent + ".jsonl"
			pageName := "rollout-2026-01-01T00-00-00-" + thread + ".jsonl"
			ancestor := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(parent, "/work", "codex_cli_rs", "2024-01-15T00:00:00Z", map[string]any{"history_mode": "paginated"}), testjsonl.CodexMsgJSON("user", "live parent", "2024-01-15T00:00:01Z"))
			page := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaWithFieldsJSON(thread, "/work", "codex_cli_rs", "2026-01-01T00:00:00Z", map[string]any{"history_mode": "paginated", "history_base": map[string]any{"thread_id": parent, "end_ordinal_exclusive": 2, "end_byte_offset": len(ancestor)}}), testjsonl.CodexMsgJSON("user", "child", "2026-01-01T00:00:01Z"))
			dbtest.WriteTestFile(t, filepath.Join(home, "sessions", pageName), []byte(page))
			if tc.withParent {
				dbtest.WriteTestFile(t, filepath.Join(home, "sessions", parentName), []byte(ancestor))
				dbtest.WriteTestFile(t, filepath.Join(home, "archived_sessions", parentName), []byte("corrupt archived copy\n"))
			}
			seed, database, closeArchive := seedRetainedFixture(t, root, data, []rawarchive.RootSpec{{Provider: "codex", Path: home}})
			session, err := database.GetSessionFull(ctx, "codex:"+thread)
			require.NoError(t, err)
			require.NotNil(t, session)
			if !tc.withParent {
				var manifestID, archiveID string
				var canonical []byte
				require.NoError(t, database.Reader().QueryRowContext(ctx, `SELECT s.manifest_id,s.canonical_json,(SELECT value FROM archive_metadata WHERE key='archive_id') FROM raw_archive_sources s JOIN raw_archive_sessions b ON b.root_id=s.root_id AND b.source_key=s.source_key WHERE b.session_id=?`, session.ID).Scan(&manifestID, &canonical, &archiveID))
				identity, err := rawsync.NewAuthIdentity(archiveID, device)
				require.NoError(t, err)
				manifest, err := rawsync.ParseCanonicalManifest(identity, manifestID, canonical, rawsync.DefaultManifestLimits())
				require.NoError(t, err)
				for _, entry := range manifest.Manifest.Entries {
					assert.NotEqual(t, parentName, filepath.Base(entry.Path))
				}
			}
			require.NoError(t, closeArchive())
			require.NoError(t, os.RemoveAll(home))
			destination := t.TempDir()
			t.Setenv("CODEX_HOME", destination)
			t.Setenv("CODEX_SESSIONS_DIR", "")
			cfg := config.Config{DataDir: seed, InstallationID: device, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {filepath.Join(home, "sessions")}}}
			result, err := Materialize(ctx, cfg, database, session, t.TempDir())
			if !tc.withParent {
				require.ErrorContains(t, err, "missing Codex ancestor")
				assert.ErrorContains(t, err, parent)
				entries, err := os.ReadDir(destination)
				require.NoError(t, err)
				assert.Empty(t, entries)
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Paths, 2)
			for i, want := range []string{ancestor, page} {
				got, err := os.ReadFile(result.Paths[i])
				require.NoError(t, err)
				assert.Equal(t, want, string(got))
			}
		})
	}
}
