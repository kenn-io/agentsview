package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexRecordedRepositorySurvivesMissingCheckout(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	id := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	source := filepath.Join(root, "rollout-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(
		id, filepath.Join(root, "removed-checkout"), "codex_cli_rs", "2026-10-08T12:00:00Z",
		map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git", "branch": "feature"}},
	)
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
		testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "codex:"+id, snapshots[0].SessionID)
	assert.Equal(t, "example.com/team/api", snapshots[0].NormalizedRemote)
	assert.Equal(t, "git_remote", snapshots[0].KeySource)
}

func TestCodexRemoteWorktreeProjectSurvivesAppendAndRebuild(t *testing.T) {
	for _, cwd := range []string{
		"/remote/code/sample-root-package",
		"/remote/code/sample/.worktrees/feature",
		"/remote/.config/superpowers/worktrees/sample/feature",
		"/remote/scratchpad/pr123",
	} {
		t.Run(cwd, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			id := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
			source := filepath.Join(root, "rollout-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z",
				map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/sample.git", "branch": "embedding/root-package"}})
			dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
				testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
			engine := NewEngine(t.Context(), database, EngineConfig{
				Machine: "remote", DisableFilesystemProjectDiscovery: true,
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			session, err := database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "sample", session.Project)

			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o600)
			require.NoError(t, err)
			_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "added a limiter", "2026-10-08T12:00:02Z") + "\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			session, err = database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			assert.True(t, session.LastWriteIncremental)
			assert.Equal(t, "sample", session.Project)

			stats := engine.ResyncAll(t.Context(), nil)
			require.Zero(t, stats.Failed)
			session, err = database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, "sample", session.Project)
			messages, err := database.GetAllMessages(t.Context(), session.ID)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "added a limiter", messages[1].Content)
		})
	}
}

func TestRecordedRepositoryIdentityWritePaths(t *testing.T) {
	for _, mode := range []string{"ordinary", "bulk", "full"} {
		t.Run(mode, func(t *testing.T) {
			database := openTestDB(t)
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/replacement.git\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/replacement\n"), 0o644))
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
			t.Cleanup(engine.Close)
			pw := pendingWrite{sess: parser.ParsedSession{
				ID: "recorded-repository", Project: "api", Machine: "local", Agent: parser.AgentCodex,
				Cwd: root, GitRemote: "https://example.com/team/original.git", StartedAt: time.Now(),
			}}
			if mode == "full" {
				require.NoError(t, engine.writeSessionFull(pw))
			} else {
				writeMode := syncWriteDefault
				if mode == "bulk" {
					writeMode = syncWriteBulk
				}
				written, _, failed, _ := engine.writeBatch([]pendingWrite{pw}, writeMode, true)
				require.Equal(t, 1, written)
				require.Zero(t, failed)
			}
			snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			assert.Equal(t, "example.com/team/original", snapshots[0].NormalizedRemote)
			assert.Equal(t, export.ProjectResolutionResolved, snapshots[0].RemoteResolution)
			assert.Empty(t, snapshots[0].GitRemoteName, "the source does not name a git config remote")
		})
	}
}

func TestCodexIncrementalAppendKeepsRecordedRepository(t *testing.T) {
	for _, oldRemote := range []string{"", "https://example.com/team/api.git", "https://example.com/team/previous.git"} {
		t.Run(oldRemote, func(t *testing.T) {
			database := openTestDB(t)
			sessionsDir := t.TempDir()
			checkout := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(checkout, ".git"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/replacement.git\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
			id := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			source := filepath.Join(sessionsDir, "rollout-"+id+".jsonl")
			meta := testjsonl.CodexSessionMetaWithFieldsJSON(
				id, checkout, "codex_cli_rs", "2026-10-08T12:00:00Z",
				map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}},
			)
			dbtest.WriteTestFile(t, source, []byte(meta+"\n"+
				testjsonl.CodexMsgJSON("user", "add rate limiting", "2026-10-08T12:00:01Z")+"\n"))
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessionsDir}}})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			if oldRemote != "" {
				engine.Close()
				identity := export.BuildStoredProjectIdentity(export.ProjectIdentityInput{RootPath: checkout, GitRemote: oldRemote, GitRemoteName: "origin"})
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					for _, table := range []string{"session_project_identity_snapshots", "project_identity_observations"} {
						_, err := tx.ExecContext(t.Context(), "UPDATE "+table+" SET git_remote=?, git_remote_name='origin', normalized_remote=?, key=?, key_source=?", oldRemote, identity.NormalizedRemote, identity.Key, identity.KeySource)
						if err != nil {
							return err
						}
					}
					_, err := tx.ExecContext(t.Context(), "UPDATE sessions SET data_version=137; PRAGMA user_version=137")
					return err
				}))
				require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
					_, err := tx.ExecContext(t.Context(), "ALTER TABLE session_project_identity_snapshots DROP COLUMN recorded_git_remote")
					return err
				}))
				path := database.Path()
				require.NoError(t, database.Close())
				var err error
				database, err = db.OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, database.Close()) })
				cfg := EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessionsDir}}}
				engine = NewEngine(t.Context(), database, cfg)
				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				engine.Close()
				// Reopen so the append must use persisted source evidence.
				require.NoError(t, database.Close())
				database, err = db.OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				engine = NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
			}

			f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "added a limiter", "2026-10-08T12:00:02Z") + "\n")
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

			sess, err := database.GetSessionFull(t.Context(), "codex:"+id)
			require.NoError(t, err)
			require.NotNil(t, sess)
			require.True(t, sess.LastWriteIncremental, "the append must take the incremental path")
			observations, err := database.ListProjectIdentityObservations(t.Context(), nil)
			require.NoError(t, err)
			wantRemotes := []string{"example.com/team/api"}
			if oldRemote == "https://example.com/team/previous.git" {
				wantRemotes = append(wantRemotes, "example.com/team/previous")
			}
			var remotes []string
			for _, obs := range observations {
				remotes = append(remotes, obs.NormalizedRemote)
			}
			assert.ElementsMatch(t, wantRemotes, remotes, "the append must not add the checkout's conflicting remote")
			snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
			require.NoError(t, err)
			require.Len(t, snapshots, 1)
			wantSnapshot := "example.com/team/api"
			if oldRemote == "https://example.com/team/previous.git" {
				wantSnapshot = "example.com/team/previous"
			}
			assert.Equal(t, wantSnapshot, snapshots[0].NormalizedRemote)
		})
	}
}

func TestCodexLabeledRunKeepsProjectThroughResync(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cwd := filepath.Join(root, "checkout")
	source := filepath.Join(root, "experiment", "case", ".codex-run-v1-abc", "sessions", "2026", "10", "08", "rollout-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git", "branch": "feature"}})
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "experiment/case", session.Project)
	f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "done", "2026-10-08T12:00:02Z") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "experiment/case", session.Project)
	assert.True(t, session.LastWriteIncremental)
	require.Zero(t, engine.ResyncAll(t.Context(), nil).Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "experiment/case", session.Project)
}

func TestCodexLabeledProjectChangesWithRoots(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/api.git\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	const id = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	sessions := filepath.Join(root, "label", ".codex-run-v1-a", "sessions")
	path := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, checkout, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
	dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	config := EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}}
	engine := NewEngine(t.Context(), database, config)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.Equal(t, "checkout", session.Project)
	engine.Close()
	require.NoError(t, os.Remove(filepath.Join(checkout, ".git", "config")))
	require.NoError(t, os.Remove(filepath.Join(checkout, ".git", "HEAD")))
	require.NoError(t, os.Remove(filepath.Join(checkout, ".git")))
	require.NoError(t, os.Remove(checkout))
	config.AgentDirs[parser.AgentCodex] = []string{root}
	engine = NewEngine(t.Context(), database, config)
	var oldMarker, marker string
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&oldMarker))
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&marker))
	assert.NotEqual(t, oldMarker, marker)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.Equal(t, "label", session.Project)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "done", "2026-10-08T12:00:02Z") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "label", session.Project)
	engine.Close()
	config.AgentDirs[parser.AgentCodex] = []string{sessions}
	engine = NewEngine(t.Context(), database, config)
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "checkout", session.Project)

	const assignedID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	const mappedID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	for _, siblingID := range []string{assignedID, mappedID} {
		siblingPath := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+siblingID+".jsonl")
		siblingMeta := testjsonl.CodexSessionMetaJSON(siblingID, checkout, "codex_cli_rs", "2026-10-08T12:00:00Z")
		dbtest.WriteTestFile(t, siblingPath, []byte(siblingMeta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	}
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	_, err = database.AssignSessionProject(t.Context(), "codex:"+assignedID, "assigned")
	require.NoError(t, err)
	_, err = database.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{Machine: "local", PathPrefix: checkout, Project: "mapped", Enabled: true})
	require.NoError(t, err)
	engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	stats := engine.SyncAll(t.Context(), nil)
	require.Zero(t, stats.Failed)
	assert.Equal(t, 3, stats.Synced)
	for id, project := range map[string]string{assignedID: "assigned", mappedID: "mapped"} {
		session, err = database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		assert.Equal(t, project, session.Project)
	}
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&oldMarker))
	engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root, filepath.Join(root, "unavailable")}}})
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&marker))
	assert.Equal(t, oldMarker, marker)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
	engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}})
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	for id, project := range map[string]string{assignedID: "assigned", mappedID: "mapped"} {
		session, err = database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		assert.Equal(t, project, session.Project)
	}
}

func TestCodexLabelRootsRefreshDuringOrdinarySync(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "12345678-1234-4234-8234-123456789abc"
	sessions := filepath.Join(root, "label", ".codex-run-v1-a", "sessions")
	path := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-10-08T12:00:00Z")
	dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	config := EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}}
	engine := NewEngine(t.Context(), database, config)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	var oldMarker, marker string
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&oldMarker))
	require.Zero(t, engine.SyncRootsSince(t.Context(), []string{root}, time.Now().Add(time.Hour), nil).Failed)
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&marker))
	assert.Equal(t, oldMarker, marker)
	require.Zero(t, engine.SyncAllSince(t.Context(), time.Now().Add(time.Hour), nil).Failed)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.Equal(t, "label", session.Project)
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&marker))
	assert.NotEqual(t, oldMarker, marker)
	assert.Zero(t, engine.SyncAllSince(t.Context(), time.Now().Add(time.Hour), nil).Synced)
}

func TestCodexLabelRootsUnavailableAndUnknownMarker(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	const id = "12345678-1234-4234-8234-123456789abc"
	path := filepath.Join(root, "label", ".codex-run-v1-a", "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-10-08T12:00:00Z")
	dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root, missing}}})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.MkdirAll(missing, 0o755))
	assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	assert.Zero(t, engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE archive_metadata SET value = ? WHERE key = ?`, `["v2"]`, codexLabelRootsKey)
		return err
	}))
	assert.True(t, engine.SyncAll(t.Context(), nil).Aborted)
	var marker string
	require.NoError(t, database.Reader().QueryRow(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&marker))
	assert.Equal(t, `["v2"]`, marker)
}

func TestCodexLabelRootRefreshWaitsForUnreadableDirectory(t *testing.T) {
	root := t.TempDir()
	label := filepath.Join(root, "label")
	sessions := filepath.Join(label, ".codex-run-v1-a", "sessions")
	const id = "12345678-1234-4234-8234-123456789abc"
	path := filepath.Join(sessions, "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-10-08T12:00:00Z")
	dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {sessions}}})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	engine.ReconfigureSources(SourceConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	require.NoError(t, os.Chmod(label, 0))
	t.Cleanup(func() { require.NoError(t, os.Chmod(label, 0o755)) })
	if _, err := os.ReadDir(label); err == nil {
		t.Skip("directory permissions do not block reads")
	}
	stats := engine.SyncAll(t.Context(), nil)
	assert.Positive(t, stats.Failed)
	require.NoError(t, os.Chmod(label, 0o755))
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "label", session.Project)
}
