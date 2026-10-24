package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func newNativeTitleEngine(t *testing.T, archive *db.DB, agent parser.AgentType, roots ...string) *sync.Engine {
	t.Helper()
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: roots}, Machine: "host-a",
	})
	t.Cleanup(engine.Close)
	return engine
}

func nativeStoredTitle(t *testing.T, archive *db.DB, id string) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func newAntigravityTitleEngine(t *testing.T, archive *db.DB, roots ...string) *sync.Engine {
	t.Helper()
	return newNativeTitleEngine(t, archive, parser.AgentAntigravity, roots...)
}

func antigravityStoredTitle(t *testing.T, archive *db.DB) *string {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session.SessionName
}

func TestAntigravityTitlesFollowStore(t *testing.T) {
	for _, mode := range []string{"brain", "database"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			if mode == "database" {
				dbPath := filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
				require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
				copyAntigravityCLITestSchemaTemplate(t, dbPath)
				transcript = dbPath
			}
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newAntigravityTitleEngine(t, archive, root)

			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("First"), antigravityStoredTitle(t, archive), "a new session takes its saved title")
			before, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new(transcript), before.FilePath)

			dbtest.SetAntigravityTitle(t, root, "Second")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			after, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
			require.NoError(t, err)
			assert.Equal(t, new("Second"), after.SessionName, "a store-only rename updates the title")
			assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
			assert.Equal(t, before.FileHash, after.FileHash, "a rename leaves the transcript fingerprint alone")

			dbtest.SetAntigravityTitle(t, root, "  ")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Nil(t, antigravityStoredTitle(t, archive), "a blank saved title clears the name")
			for _, tc := range []struct {
				title   string
				want    *string
				updates int
			}{
				{title: "ab", want: new("ab"), updates: 1},
				{title: "a\x00b", want: new("ab"), updates: 0},
				{title: "\x00", want: nil, updates: 1},
				{title: "\x00\x00", want: nil, updates: 0},
			} {
				dbtest.SetAntigravityTitle(t, root, tc.title)
				plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
				require.NoError(t, err)
				result, err := engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
				assert.Equal(t, tc.updates, result.Stats.TitlesUpdated)
				assert.Equal(t, tc.want, antigravityStoredTitle(t, archive))
			}
			dbtest.SetAntigravityTitle(t, root, "Direct refresh")
			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "antigravity:"+dbtest.AntigravityTitleID))
			assert.Equal(t, new("Direct refresh"), antigravityStoredTitle(t, archive))
		})
	}
}

func TestAntigravityTitlesReachSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	dbtest.SetAntigravityTitle(t, root, "Renamed")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed"), antigravityStoredTitle(t, archive))
}

func TestAntigravityUnreadableTitleStoreKeepsTitle(t *testing.T) {
	for name, schema := range map[string]string{
		"corrupt":        "",
		"missing_table":  "CREATE TABLE other (title TEXT)",
		"missing_column": "CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY)",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newAntigravityTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

			data := []byte("not a database")
			if schema != "" {
				incompatible := filepath.Join(t.TempDir(), parser.AntigravitySummariesDBName)
				conn, err := sql.Open("sqlite3", incompatible)
				require.NoError(t, err)
				_, err = conn.ExecContext(t.Context(), schema)
				require.NoError(t, err)
				require.NoError(t, conn.Close())
				data, err = os.ReadFile(incompatible)
				require.NoError(t, err)
			}
			require.NoError(t, os.WriteFile(store, data, 0o644))
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, new("First"), antigravityStoredTitle(t, archive), "an unreadable store keeps the stored title")
		})
	}
}

func TestAntigravityFullSyncBackfillsTitles(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	require.NoError(t, newAntigravityTitleEngine(t, archive, root).SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, antigravityStoredTitle(t, archive))

	dbtest.SetAntigravityTitle(t, root, "Saved while closed")
	newAntigravityTitleEngine(t, archive, root).SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"), antigravityStoredTitle(t, archive), "existing sessions pick up titles without a transcript change")
}

func TestAntigravityTitleStoreRenamesOnlyItsOwnSessions(t *testing.T) {
	owner, other := t.TempDir(), t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, owner, "hello")
	dbtest.SetAntigravityTitle(t, owner, "Owner title")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, owner, other)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := dbtest.SetAntigravityTitle(t, other, "Other root title")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Owner title"), antigravityStoredTitle(t, archive))
}

func TestAntigravityRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
	dbtest.SetAntigravityTitle(t, root, "Later")
	engine.ResyncAll(t.Context(), nil)
	assert.Equal(t, new("Later"), antigravityStoredTitle(t, archive), "orphan copies must not restore an older title")
}

func TestAntigravityTitleRenamesRespectCwdFilter(t *testing.T) {
	for _, mode := range []string{"applied", "rebuild"} {
		for _, tc := range []struct {
			name string
			want string
		}{
			{name: "excluded", want: "First"},
			{name: "allowed", want: "Later"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				root, claudeRoot, workspace := t.TempDir(), t.TempDir(), t.TempDir()
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := dbtest.SetAntigravityTitle(t, root, "First")
				archive := dbtest.OpenTestDB(t)
				require.NoError(t, newAntigravityTitleEngine(t, archive, root).SyncPathsContext(t.Context(), []string{transcript}))
				require.NoError(t, archive.UpdateSessionCwd(t.Context(), "antigravity:"+dbtest.AntigravityTitleID, filepath.Join(workspace, tc.name)))
				allowed := filepath.Join(workspace, "allowed")
				engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
					AgentDirs: map[parser.AgentType][]string{
						parser.AgentAntigravity: {root}, parser.AgentClaude: {claudeRoot},
					},
					Machine: "host-a", IncludeCwdPrefixes: []string{allowed},
				})
				if mode == "rebuild" {
					require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
					dbtest.WriteTestFile(t, filepath.Join(claudeRoot, "project", "live.jsonl"), []byte(fmt.Sprintf(
						`{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"live","cwd":%q,"message":{"role":"user","content":"hello"}}`+"\n", allowed)))
				}
				dbtest.SetAntigravityTitle(t, root, "Later")
				if mode == "rebuild" {
					stats := engine.ResyncAll(t.Context(), nil)
					require.False(t, stats.Aborted)
					require.Equal(t, 1, stats.Synced)
				} else {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
				}
				assert.Equal(t, new(tc.want), antigravityStoredTitle(t, archive))
			})
		}
	}
}

func TestAntigravityTrashedSessionRestoresWithCurrentTitle(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "antigravity:" + dbtest.AntigravityTitleID

	require.NoError(t, archive.SoftDeleteSession(t.Context(), id))
	dbtest.SetAntigravityTitle(t, root, "Renamed in trash")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	_, err := archive.RestoreSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in trash"), antigravityStoredTitle(t, archive))
}

// setAntigravitySummary writes a conversation_summaries row with the optional
// workspace and parent columns the Antigravity desktop app records.
func setAntigravitySummary(t *testing.T, root, title, workspaceURIs, parent string) string {
	t.Helper()
	store := filepath.Join(root, parser.AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS conversation_summaries (
		conversation_id TEXT PRIMARY KEY,
		title TEXT,
		workspace_uris TEXT NOT NULL DEFAULT '',
		parent_conversation_id TEXT NOT NULL DEFAULT ''
	)`)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(),
		`INSERT INTO conversation_summaries VALUES (?, ?, ?, ?)
		 ON CONFLICT(conversation_id) DO UPDATE SET title = excluded.title,
		     workspace_uris = excluded.workspace_uris,
		     parent_conversation_id = excluded.parent_conversation_id`,
		dbtest.AntigravityTitleID, title, workspaceURIs, parent,
	)
	require.NoError(t, err)
	return store
}

func antigravityWorkspaceURIs(dir string) string {
	slashed := filepath.ToSlash(dir)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	return `["file://` + slashed + `"]`
}

func antigravityStoredSession(t *testing.T, archive *db.DB) *db.Session {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), "antigravity:"+dbtest.AntigravityTitleID)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}

func requirePublishableAntigravityIdentity(t *testing.T, archive *db.DB) {
	t.Helper()
	snapshots, err := archive.ListPublishableSessionProjectIdentitySnapshots(
		t.Context(), []string{"antigravity:" + dbtest.AntigravityTitleID}, nil, nil,
	)
	require.NoError(t, err)
	require.Len(t, snapshots, 1, "the workspace's project identity is publishable")
	assert.NotEmpty(t, snapshots[0].Key)
}

func writeAntigravityLiveTranscript(t *testing.T, root string) {
	t.Helper()
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
}

func TestAntigravitySummaryFillsStoredSessions(t *testing.T) {
	for _, mode := range []string{"sync", "rebuild-live", "rebuild-orphan"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			archive := dbtest.OpenTestDB(t)
			engine := newAntigravityTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Empty(t, antigravityStoredSession(t, archive).Cwd)

			store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "synthetic-parent")
			switch mode {
			case "sync":
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			case "rebuild-orphan":
				require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
				writeAntigravityLiveTranscript(t, root)
				fallthrough
			default:
				require.False(t, engine.ResyncAll(t.Context(), nil).Aborted)
			}

			session := antigravityStoredSession(t, archive)
			assert.Equal(t, new("Title"), session.SessionName)
			assert.Equal(t, filepath.Clean(workspace), session.Cwd)
			assert.Equal(t, "synthetic_workspace", session.Project)
			assert.Equal(t, new("antigravity:synthetic-parent"), session.ParentSessionID)
			assert.Equal(t, "subagent", session.RelationshipType)
			requirePublishableAntigravityIdentity(t, archive)
		})
	}
}

func TestAntigravityStoredWorkspaceSurvivesStoreChanges(t *testing.T) {
	for _, mode := range []string{"sync", "rebuild"} {
		for _, change := range []string{"blank", "json-empty-string", "row-missing", "store-unreadable", "other-workspace"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				root := t.TempDir()
				workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "synthetic-parent")
				archive := dbtest.OpenTestDB(t)
				engine := newAntigravityTitleEngine(t, archive, root)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
				before := antigravityStoredSession(t, archive)
				require.Equal(t, filepath.Clean(workspace), before.Cwd)

				switch change {
				case "blank":
					setAntigravitySummary(t, root, "Title", "", "")
				case "json-empty-string":
					setAntigravitySummary(t, root, "Title", `""`, "")
				case "row-missing":
					conn, err := sql.Open("sqlite3", store)
					require.NoError(t, err)
					_, err = conn.ExecContext(t.Context(), `DELETE FROM conversation_summaries`)
					require.NoError(t, err)
					require.NoError(t, conn.Close())
				case "store-unreadable":
					require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
				case "other-workspace":
					setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(t.TempDir()), "other-parent")
				}
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
				if mode == "rebuild" {
					require.False(t, engine.ResyncAll(t.Context(), nil).Aborted)
				} else {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
				}

				after := antigravityStoredSession(t, archive)
				require.NotNil(t, after.FirstMessage)
				assert.Equal(t, "hello again", *after.FirstMessage, "the transcript was reparsed")
				assert.Equal(t, before.Cwd, after.Cwd)
				assert.Equal(t, before.Project, after.Project)
				assert.Equal(t, new("antigravity:synthetic-parent"), after.ParentSessionID)
				assert.Equal(t, "subagent", after.RelationshipType)
			})
		}
	}
}

func TestAntigravityStoredWorkspaceKeepsProjectAfterCheckoutIsGone(t *testing.T) {
	for _, change := range []string{"store-unchanged", "store-blank"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(t.TempDir(), "synthetic-repo")
			workspace := filepath.Join(repo, "pkg", "sub")
			require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
			require.NoError(t, os.MkdirAll(workspace, 0o755))
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "")
			archive := dbtest.OpenTestDB(t)
			engine := newAntigravityTitleEngine(t, archive, root)
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, "synthetic_repo", antigravityStoredSession(t, archive).Project)

			// No Git remote, and the checkout is gone.
			require.NoError(t, os.RemoveAll(repo))
			if change == "store-blank" {
				setAntigravitySummary(t, root, "Title", "", "")
			}
			dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))

			after := antigravityStoredSession(t, archive)
			assert.Equal(t, filepath.Clean(workspace), after.Cwd)
			assert.Equal(t, "synthetic_repo", after.Project,
				"the stored project is kept instead of the directory name")
		})
	}
}

func TestAntigravitySummaryWorkspaceFollowsWorktreeMapping(t *testing.T) {
	root := t.TempDir()
	mapped := t.TempDir()
	workspace := filepath.Join(mapped, "synthetic-checkout")
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	_, err := archive.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
		Machine: "host-a", PathPrefix: mapped, Project: "configured_project", Enabled: true,
	})
	require.NoError(t, err)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := setAntigravitySummary(t, root, "First", antigravityWorkspaceURIs(workspace), "")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, "configured_project", antigravityStoredSession(t, archive).Project,
		"a filled workspace takes the mapped project")

	setAntigravitySummary(t, root, "Renamed", antigravityWorkspaceURIs(workspace), "")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session := antigravityStoredSession(t, archive)
	assert.Equal(t, new("Renamed"), session.SessionName)
	assert.Equal(t, "configured_project", session.Project, "a rename keeps the mapped project")
}

func TestAntigravityCwdFilterJudgesStoredWorkspace(t *testing.T) {
	for _, mode := range []string{"sync", "rebuild"} {
		for _, change := range []string{"store-blank", "store-moves-outside"} {
			t.Run(mode+"/"+change, func(t *testing.T) {
				root := t.TempDir()
				allowed := t.TempDir()
				workspace := filepath.Join(allowed, "synthetic-workspace")
				transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				store := setAntigravitySummary(t, root, "Title", antigravityWorkspaceURIs(workspace), "")
				archive := dbtest.OpenTestDB(t)
				engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
					AgentDirs:          map[parser.AgentType][]string{parser.AgentAntigravity: {root}},
					Machine:            "host-a",
					IncludeCwdPrefixes: []string{allowed},
				})
				t.Cleanup(engine.Close)
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
				require.Equal(t, filepath.Clean(workspace), antigravityStoredSession(t, archive).Cwd)

				newWorkspace := ""
				if change == "store-moves-outside" {
					newWorkspace = antigravityWorkspaceURIs(t.TempDir())
				}
				setAntigravitySummary(t, root, "Title", newWorkspace, "")
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
				if mode == "rebuild" {
					require.False(t, engine.ResyncAll(t.Context(), nil).Aborted)
				} else {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript, store}))
				}
				after := antigravityStoredSession(t, archive)
				require.NotNil(t, after.FirstMessage)
				assert.Equal(t, "hello again", *after.FirstMessage, "the stored workspace passes the cwd filter")
				assert.Equal(t, filepath.Clean(workspace), after.Cwd)
			})
		}
	}
}

// A copy of a stored conversation from another machine, path, and content is
// unverified: it keeps its own workspace instead of the stored one and does
// not take the stored machine's worktree mapping, with or without a cwd filter.
func TestAntigravityUnverifiedCopyKeepsOwnWorkspace(t *testing.T) {
	for _, filter := range []bool{false, true} {
		t.Run(fmt.Sprintf("cwd-filter=%t", filter), func(t *testing.T) {
			allowed := t.TempDir()
			storedWorkspace := filepath.Join(allowed, "stored-workspace")
			copyWorkspace := filepath.Join(allowed, "copy-workspace")
			storedRoot, copyRoot := t.TempDir(), t.TempDir()
			storedTranscript := dbtest.WriteAntigravityTitleTranscript(t, storedRoot, "hello from b")
			setAntigravitySummary(t, storedRoot, "Title", antigravityWorkspaceURIs(storedWorkspace), "")
			copyTranscript := dbtest.WriteAntigravityTitleTranscript(t, copyRoot, "hello from a")
			setAntigravitySummary(t, copyRoot, "Title", antigravityWorkspaceURIs(copyWorkspace), "")

			archive := dbtest.OpenTestDB(t)
			storedEngine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {storedRoot}},
				Machine:   "host-b",
			})
			t.Cleanup(storedEngine.Close)
			require.NoError(t, storedEngine.SyncPathsContext(t.Context(), []string{storedTranscript}))
			require.Equal(t, filepath.Clean(storedWorkspace), antigravityStoredSession(t, archive).Cwd)
			_, err := archive.CreateWorktreeProjectMapping(t.Context(), db.WorktreeProjectMapping{
				Machine: "host-b", PathPrefix: allowed, Project: "mapped_project", Enabled: true,
			})
			require.NoError(t, err)

			cfg := sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentAntigravity: {copyRoot}},
				Machine:   "host-a",
			}
			if filter {
				cfg.IncludeCwdPrefixes = []string{allowed}
			}
			copyEngine := sync.NewEngine(t.Context(), archive, cfg)
			t.Cleanup(copyEngine.Close)
			require.NoError(t, copyEngine.SyncPathsContext(t.Context(), []string{copyTranscript}))

			after := antigravityStoredSession(t, archive)
			require.NotNil(t, after.FirstMessage)
			assert.Equal(t, "hello from a", *after.FirstMessage, "the copy is written")
			assert.Equal(t, "host-b", after.Machine, "the session keeps its archived machine")
			assert.Equal(t, filepath.Clean(copyWorkspace), after.Cwd)
			assert.Equal(t, "copy_workspace", after.Project)
		})
	}
}

func TestAntigravityUsageOnlyArchiveFillsWorkspaceAndParent(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(t.TempDir(), "synthetic-workspace")
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs:      map[parser.AgentType][]string{parser.AgentAntigravity: {root}},
		Machine:        "host-a",
		ArchiveContent: config.ArchiveContentUsage,
	})
	t.Cleanup(engine.Close)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := setAntigravitySummary(t, root, "Private title", antigravityWorkspaceURIs(workspace), "synthetic-parent")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	session := antigravityStoredSession(t, archive)
	assert.Equal(t, filepath.Clean(workspace), session.Cwd)
	assert.Equal(t, "synthetic_workspace", session.Project)
	assert.Equal(t, new("antigravity:synthetic-parent"), session.ParentSessionID)
	assert.Nil(t, session.SessionName, "usage-only archives keep no titles")
	// A full pass rereads the store; with nothing changed it writes nothing.
	assert.Zero(t, engine.SyncAll(t.Context(), nil).TitlesUpdated)
}

func TestAntigravityNativeParentOverridesLauncherLink(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	engine := newAntigravityTitleEngine(t, archive, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "antigravity:" + dbtest.AntigravityTitleID
	for _, other := range []string{"launcher-session", "antigravity:synthetic-parent"} {
		require.NoError(t, archive.UpsertSession(t.Context(), db.Session{
			ID: other, Project: "p", Machine: "host-a", Agent: string(parser.AgentAntigravity), MessageCount: 1,
		}))
	}
	_, err := archive.SetSessionExternalParent(t.Context(), id, "launcher-session")
	require.NoError(t, err)
	require.Equal(t, new("launcher-session"), antigravityStoredSession(t, archive).ParentSessionID)

	store := setAntigravitySummary(t, root, "Title", "", "synthetic-parent")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("antigravity:synthetic-parent"), antigravityStoredSession(t, archive).ParentSessionID,
		"a native parent takes precedence over the launcher link")

	// A reparse of the transcript reaches the same hierarchy.
	dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	assert.Equal(t, new("antigravity:synthetic-parent"), antigravityStoredSession(t, archive).ParentSessionID)
}
