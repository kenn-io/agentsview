package sync_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)

			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
			require.Equal(t, new("First"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "a new session takes its saved title")
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
			assert.Nil(t, nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "a blank saved title clears the name")
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
				assert.Equal(t, tc.want, nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
			}
			dbtest.SetAntigravityTitle(t, root, "Direct refresh")
			require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "antigravity:"+dbtest.AntigravityTitleID))
			assert.Equal(t, new("Direct refresh"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
		})
	}
}

func TestAntigravityTitlesReachSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	dbtest.SetAntigravityTitle(t, root, "Renamed")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Renamed"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
}

func TestAntigravityUnreadableTitleStoreKeepsTitle(t *testing.T) {
	for name, schema := range map[string]string{
		"corrupt":       "",
		"missing_table": "CREATE TABLE other (title TEXT)",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
			store := dbtest.SetAntigravityTitle(t, root, "First")
			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
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
			stats := engine.SyncAll(t.Context(), nil)
			if name == "missing_table" {
				assert.Zero(t, stats.Failed)
			} else {
				assert.Equal(t, 1, stats.Failed)
			}
			assert.Equal(t, new("First"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "an unreadable store keeps the stored title")
		})
	}
}

func TestAntigravityFullSyncBackfillsTitles(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	archive := dbtest.OpenTestDB(t)
	require.NoError(t, newNativeTitleEngine(t, archive, parser.AgentAntigravity, root).SyncPathsContext(t.Context(), []string{transcript}))
	require.Nil(t, nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))

	dbtest.SetAntigravityTitle(t, root, "Saved while closed")
	newNativeTitleEngine(t, archive, parser.AgentAntigravity, root).SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "existing sessions pick up titles without a transcript change")
}

func TestAntigravityTitleStoreRenamesOnlyItsOwnSessions(t *testing.T) {
	owner, other := t.TempDir(), t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, owner, "hello")
	dbtest.SetAntigravityTitle(t, owner, "Owner title")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, owner, other)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	store := dbtest.SetAntigravityTitle(t, other, "Other root title")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new("Owner title"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
}

func TestAntigravityRebuildKeepsRenamesOfSessionsWithoutTranscript(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))

	require.NoError(t, os.RemoveAll(filepath.Join(root, "brain")))
	// A rebuild with no live sources refuses to swap.
	dbtest.WriteTestFile(t, filepath.Join(root, "brain", "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee", ".system_generated", "logs", "transcript.jsonl"),
		[]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"live"}`+"\n"))
	dbtest.SetAntigravityTitle(t, root, "Later")
	engine.ResyncAll(t.Context(), nil)
	assert.Equal(t, new("Later"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID), "orphan copies must not restore an older title")
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
				require.NoError(t, newNativeTitleEngine(t, archive, parser.AgentAntigravity, root).SyncPathsContext(t.Context(), []string{transcript}))
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
				assert.Equal(t, new(tc.want), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
			})
		}
	}
}

func TestAntigravityTrashedSessionRestoresWithCurrentTitle(t *testing.T) {
	root := t.TempDir()
	transcript := dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
	store := dbtest.SetAntigravityTitle(t, root, "First")
	archive := dbtest.OpenTestDB(t)
	engine := newNativeTitleEngine(t, archive, parser.AgentAntigravity, root)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{transcript}))
	id := "antigravity:" + dbtest.AntigravityTitleID

	require.NoError(t, archive.SoftDeleteSession(t.Context(), id))
	dbtest.SetAntigravityTitle(t, root, "Renamed in trash")
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	_, err := archive.RestoreSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, new("Renamed in trash"), nativeStoredTitle(t, archive, "antigravity:"+dbtest.AntigravityTitleID))
}

func writeAntigravityCLITitleConversation(t *testing.T, root, prompt string) string {
	t.Helper()
	convDir := filepath.Join(root, "conversations")
	require.NoError(t, os.MkdirAll(convDir, 0o755))
	dbPath := filepath.Join(convDir, dbtest.AntigravityTitleID+".db")
	createAntigravityCLIDisplayStepDB(t, dbPath, prompt)
	dbtest.WriteAntigravityTitleTranscript(t, root, prompt)
	return dbPath
}

func TestAntigravityCLITitlesFollowStore(t *testing.T) {
	for _, layout := range []string{"standalone", "container"} {
		t.Run(layout, func(t *testing.T) {
			base := t.TempDir()
			configuredRoot := base
			sessionRoot := base
			if layout == "container" {
				configuredRoot = filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
				sessionRoot = filepath.Join(configuredRoot, "instance-a", "antigravity-acp")
			}
			dbPath := writeAntigravityCLITitleConversation(t, sessionRoot, "hello")
			store := dbtest.SetAntigravityTitle(t, sessionRoot, "First")

			archive := dbtest.OpenTestDB(t)
			engine := newNativeTitleEngine(t, archive, parser.AgentAntigravityCLI, configuredRoot)
			const sessionID = "antigravity-cli:" + dbtest.AntigravityTitleID

			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath}))
			require.Equal(t, new("First"), nativeStoredTitle(t, archive, sessionID), "a new session takes its saved title")
			before, err := archive.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			assert.Equal(t, new(dbPath), before.FilePath)

			dbtest.SetAntigravityTitle(t, sessionRoot, "Second")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			after, err := archive.GetSessionFull(t.Context(), sessionID)
			require.NoError(t, err)
			assert.Equal(t, new("Second"), after.SessionName, "a store-only rename updates the title")
			assert.Equal(t, 1, engine.LastSyncStats().TitlesUpdated)
			assert.Equal(t, before.FileHash, after.FileHash, "a rename leaves the transcript fingerprint alone")

			dbtest.SetAntigravityTitle(t, sessionRoot, "  ")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Nil(t, nativeStoredTitle(t, archive, sessionID), "a blank saved title clears the name")

			dbtest.SetAntigravityTitle(t, sessionRoot, "Saved title")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			require.Equal(t, new("Saved title"), nativeStoredTitle(t, archive, sessionID))

			// An unreadable store keeps the archived title even if the session reparses.
			savedStore, err := os.ReadFile(store)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o600))
			dbtest.WriteAntigravityTitleTranscript(t, sessionRoot, "hello updated")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{dbPath, store}))
			stats := engine.SyncAll(t.Context(), nil)
			assert.Equal(t, 1, stats.Failed)
			assert.Equal(t, new("Saved title"), nativeStoredTitle(t, archive, sessionID), "an unreadable store keeps the stored title")

			// Restoring the store lets subsequent renames apply again.
			require.NoError(t, os.WriteFile(store, savedStore, 0o600))
			dbtest.SetAntigravityTitle(t, sessionRoot, "Recovered title")
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			assert.Equal(t, new("Recovered title"), nativeStoredTitle(t, archive, sessionID))
		})
	}
}

func TestAntigravityCLIFullSyncBackfillsTitlesInContainer(t *testing.T) {
	container := filepath.Join(t.TempDir(), ".t3", "userdata", "providers", "antigravity")
	rootA := filepath.Join(container, "instance-a", "antigravity-acp")
	rootB := filepath.Join(container, "instance-b", "antigravity-acp")
	dbPathA := writeAntigravityCLITitleConversation(t, rootA, "hello")
	archive := dbtest.OpenTestDB(t)
	const sessionID = "antigravity-cli:" + dbtest.AntigravityTitleID

	require.NoError(t, newNativeTitleEngine(t, archive, parser.AgentAntigravityCLI, container).SyncPathsContext(t.Context(), []string{dbPathA}))
	require.Nil(t, nativeStoredTitle(t, archive, sessionID))

	dbtest.SetAntigravityTitle(t, rootA, "Saved while closed")
	// A sibling instance with the same conversation ID must not overwrite rootA's session.
	require.NoError(t, os.MkdirAll(rootB, 0o755))
	dbtest.SetAntigravityTitle(t, rootB, "Wrong instance title")
	newNativeTitleEngine(t, archive, parser.AgentAntigravityCLI, container).SyncAll(t.Context(), nil)
	assert.Equal(t, new("Saved while closed"), nativeStoredTitle(t, archive, sessionID))
}
