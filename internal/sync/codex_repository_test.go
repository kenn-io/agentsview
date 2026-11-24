package sync

import (
	"database/sql"
	"fmt"
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

func TestCodexLabeledRunResyncAndExplicitWorktreeMappingWins(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	cwd := filepath.Join(root, "checkout")
	source := filepath.Join(root, "experiment", "case", ".codex-run-v1-abc", "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git", "branch": "feature"}})
	dbtest.WriteTestFile(t, source, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshots, 1)
	assert.Equal(t, "example.com/team/api", snapshots[0].NormalizedRemote)
	repair, err := database.RepairRepositoryProjects(t.Context(), "")
	require.NoError(t, err)
	assert.Zero(t, repair.Sessions)
	f, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "done", "2026-10-08T12:00:02Z") + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{source}))
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "experiment/case", session.Project)
	assert.True(t, session.LastWriteIncremental)
	require.Zero(t, engine.ResyncAll(t.Context(), nil).Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "experiment/case", session.Project)
	_, err = database.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
		Machine: "local", PathPrefix: cwd, Layout: db.WorktreeMappingLayoutExplicit,
		Project: "mapped_project", Enabled: true,
	})
	require.NoError(t, err)
	require.Zero(t, engine.ResyncAll(t.Context(), nil).Failed)
	session, err = database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	assert.Equal(t, "mapped_project", session.Project)
}

func TestCodexLabeledRunsKeepLabelsAfterRootMove(t *testing.T) {
	database := openTestDB(t)
	base := t.TempDir()
	oldRoot, newRoot := filepath.Join(base, "old"), filepath.Join(base, "new")
	ids := []string{"cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}
	var relativePaths []string
	for i, id := range ids {
		path := filepath.Join("team", fmt.Sprintf(".codex-run-v1-%d", i), "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
		relativePaths = append(relativePaths, path)
		meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
		dbtest.WriteTestFile(t, filepath.Join(oldRoot, path), []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	}
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {oldRoot}}})
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	for _, id := range ids {
		session, err := database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, "team", session.Project)
	}
	engine.Close()
	require.NoError(t, os.Rename(oldRoot, newRoot))
	engine = NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {newRoot}}})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)
	for i, id := range ids {
		session, err := database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, "team", session.Project)
		require.NotNil(t, session.FilePath)
		assert.Equal(t, filepath.Join(newRoot, relativePaths[i]), *session.FilePath)
	}
	result := engine.SyncAll(t.Context(), nil)
	assert.Zero(t, result.Synced)
	assert.Equal(t, 2, result.Skipped)
}

func TestCodexLabeledRunNewestCopyOnSync(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	var paths []string
	for _, label := range []string{"a", "b"} {
		path := filepath.Join(root, label, ".codex-run-v1-abc", "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
		meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", label, "2026-10-08T12:00:01Z")+"\n"))
		paths = append(paths, path)
	}
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	for i, index := range []int{0, 1, 0} {
		path := paths[index]
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "update", "2026-10-08T12:00:02Z") + "\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		newer := time.Now().Add(time.Duration(i+1) * time.Hour)
		require.NoError(t, os.Chtimes(path, newer, newer))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
		session, err := database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, []string{"a", "b"}[index], session.Project)
		require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
		session, err = database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b"}[index], session.Project)
		messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
		require.NoError(t, err)
		require.Len(t, messages, 2+i/2)
		assert.Equal(t, []string{"a", "b"}[index], messages[0].Content)
	}
}

func TestSyncSingleSessionFindsNewLabeledCopy(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	ordinary, logs := filepath.Join(root, "ordinary"), filepath.Join(root, "logs")
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	name := "rollout-2026-10-08T12-00-00-" + id + ".jsonl"
	ordinaryPath := filepath.Join(ordinary, "2026", "10", "08", name)
	labeledPath := filepath.Join(logs, "team", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name)
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
	dbtest.WriteTestFile(t, ordinaryPath, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "ordinary", "2026-10-08T12:00:01Z")+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {ordinary, logs}}})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	dbtest.WriteTestFile(t, labeledPath, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "labeled", "2026-10-08T12:00:01Z")+"\n"))
	require.NoError(t, engine.SyncSingleSession("codex:"+id))
	session, err := database.GetSessionFull(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "team", session.Project)
	require.NotNil(t, session.FilePath)
	assert.Equal(t, labeledPath, *session.FilePath)
	messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "labeled", messages[0].Content)
}

func TestCodexLabeledCopySurvivesOrdinaryUpdate(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	ordinary, logs := filepath.Join(root, "ordinary"), filepath.Join(root, "logs")
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	name := "rollout-2026-10-08T12-00-00-" + id + ".jsonl"
	paths := []string{filepath.Join(ordinary, "2026", "10", "08", name), filepath.Join(logs, "a", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name), filepath.Join(logs, "b", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name)}
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
	for i, path := range paths {
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", []string{"ordinary", "a", "b"}[i], "2026-10-08T12:00:01Z")+"\n"))
		mtime := time.Now().Add(time.Duration(i) * time.Hour)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {ordinary, logs}}})
	t.Cleanup(engine.Close)
	require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
	for step, index := range []int{0, 1} {
		file, err := os.OpenFile(paths[index], os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = file.WriteString(testjsonl.CodexMsgJSON("assistant", "update", "2026-10-08T12:00:02Z") + "\n")
		require.NoError(t, err)
		require.NoError(t, file.Close())
		mtime := time.Now().Add(time.Duration(step+3) * time.Hour)
		require.NoError(t, os.Chtimes(paths[index], mtime, mtime))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{paths[index]}))
		want := []int{2, 1}[step]
		session, err := database.GetSessionFull(t.Context(), "codex:"+id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, []string{"", "a", "b"}[want], session.Project)
		require.NotNil(t, session.FilePath)
		assert.Equal(t, paths[want], *session.FilePath)
		messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
		require.NoError(t, err)
		require.NotEmpty(t, messages)
		assert.Equal(t, []string{"", "a", "b"}[want], messages[0].Content)
	}
}

func TestReconcileCodexLabeledCopyWins(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	ordinary, logs := filepath.Join(root, "ordinary"), filepath.Join(root, "logs")
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	name := "rollout-2026-10-08T12-00-00-" + id + ".jsonl"
	paths := []string{filepath.Join(ordinary, "2026", "10", "08", name), filepath.Join(logs, "a", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name), filepath.Join(logs, "b", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name)}
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
	for i, path := range paths {
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", []string{"ordinary", "a", "b"}[i], "2026-10-08T12:00:01Z")+"\n"))
		mtime := time.Now().Add(time.Duration(i) * time.Hour)
		require.NoError(t, os.Chtimes(path, mtime, mtime))
	}
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {ordinary, logs}}})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.ReconcileWatchRoots(t.Context(), []string{ordinary, logs}, false))
	assert.Equal(t, paths[2], database.GetSessionFilePath(t.Context(), "codex:"+id))
	messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "b", messages[0].Content)
}

func TestCodexLabeledAndOrdinaryNewestCopyOnSync(t *testing.T) {
	root := t.TempDir()
	ordinary, logs := filepath.Join(root, "ordinary"), filepath.Join(root, "logs")
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	name := "rollout-2026-10-08T12-00-00-" + id + ".jsonl"
	paths := []string{filepath.Join(ordinary, "2026", "10", "08", name), filepath.Join(logs, "team", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name)}
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
	for i, path := range paths {
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", []string{"ordinary", "labeled"}[i], "2026-10-08T12:00:01Z")+"\n"))
	}
	for _, roots := range [][]string{{ordinary, logs}, {logs, ordinary}} {
		for i, path := range paths {
			newer := time.Now().Add(time.Hour)
			require.NoError(t, os.Chtimes(paths[1-i], newer.Add(-time.Hour), newer.Add(-time.Hour)))
			require.NoError(t, os.Chtimes(path, newer, newer))
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: roots}})
			require.Zero(t, engine.SyncAll(t.Context(), nil).Failed)
			messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "labeled", messages[0].Content)
			engine.Close()
		}
	}
}

func TestCodexLabeledCopyWinsQuickSync(t *testing.T) {
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	root := t.TempDir()
	ordinary, logs := filepath.Join(root, "ordinary"), filepath.Join(root, "logs")
	name := "rollout-2026-10-08T12-00-00-" + id + ".jsonl"
	paths := []string{filepath.Join(ordinary, "2026", "10", "08", name), filepath.Join(logs, "team", ".codex-run-v1-abc", "sessions", "2026", "10", "08", name)}
	meta := testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_exec", "2026-10-08T12:00:00Z")
	for i, path := range paths {
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", []string{"ordinary", "labeled"}[i], "2026-10-08T12:00:01Z")+"\n"))
	}
	now := time.Now()
	require.NoError(t, os.Chtimes(paths[0], now, now))
	require.NoError(t, os.Chtimes(paths[1], now.Add(-time.Hour), now.Add(-time.Hour)))
	for _, tc := range []struct {
		name   string
		cutoff time.Time
		want   bool
	}{{"both after cutoff", now.Add(-2 * time.Hour), true}, {"only ordinary after cutoff", now.Add(-time.Minute), false}} {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", DisableFilesystemProjectDiscovery: true, AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {ordinary, logs}}})
			t.Cleanup(engine.Close)
			require.Zero(t, engine.SyncAllSince(t.Context(), tc.cutoff, nil).Failed)
			messages, err := database.GetMessages(t.Context(), "codex:"+id, 0, 100, true)
			require.NoError(t, err)
			if tc.want {
				require.Len(t, messages, 1)
				assert.Equal(t, "labeled", messages[0].Content)
			} else {
				assert.Empty(t, messages)
			}
		})
	}
}

func TestCodexLabeledRunIndexRename(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	home := filepath.Join(root, "label", ".codex-run-v1-abc")
	source := filepath.Join(home, "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+id+".jsonl")
	dbtest.WriteTestFile(t, source, []byte(testjsonl.CodexSessionMetaJSON(id, "/work/project", "codex_cli_rs", "2026-10-08T12:00:00Z")+"\n"+testjsonl.CodexMsgJSON("user", "review", "2026-10-08T12:00:01Z")+"\n"))
	index := filepath.Join(home, parser.CodexSessionIndexFilename)
	dbtest.WriteTestFile(t, index, []byte(`{"id":"`+id+`","thread_name":"Old"}`+"\n"))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{source}))
	require.NoError(t, os.WriteFile(index, []byte(`{"id":"`+id+`","thread_name":"New"}`+"\n"), 0o600))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{index}))
	name, found, err := database.GetSessionName(t.Context(), "codex:"+id)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "New", name)
}

func TestCodexLabelIsNotRepositoryAlias(t *testing.T) {
	database := openTestDB(t)
	root := t.TempDir()
	checkout := filepath.Join(t.TempDir(), "checkout")
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "config"), []byte("[remote \"origin\"]\nurl = https://example.com/team/api.git\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}})
	t.Cleanup(engine.Close)
	write := func(id, cwd, path string) {
		meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z", map[string]any{"git": map[string]any{"repository_url": "https://example.com/team/api.git"}})
		dbtest.WriteTestFile(t, path, []byte(meta+"\n"+testjsonl.CodexMsgJSON("user", "hello", "2026-10-08T12:00:01Z")+"\n"))
		require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
	}
	const first = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	write(first, checkout, filepath.Join(root, "label", ".codex-run-v1-abc", "sessions", "2026", "10", "08", "rollout-2026-10-08T12-00-00-"+first+".jsonl"))
	const second = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	write(second, filepath.Join(root, "missing"), filepath.Join(root, "rollout-"+second+".jsonl"))
	session, err := database.GetSessionFull(t.Context(), "codex:"+second)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "api", session.Project)
}
