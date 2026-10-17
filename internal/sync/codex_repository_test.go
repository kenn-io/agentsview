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
