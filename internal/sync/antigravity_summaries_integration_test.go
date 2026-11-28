package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const antigravitySummaryParent = "cccccccc-dddd-eeee-ffff-000000000000"

// antigravitySummaryCase is one provider's conversation fixture.
type antigravitySummaryCase struct {
	agent parser.AgentType
	id    string
	// write creates the conversation.
	write func(t *testing.T, root string)
}

func antigravitySummaryCases() map[string]antigravitySummaryCase {
	const (
		cliID     = "12121212-3434-5656-7878-909090909090"
		cliPrompt = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"Read calc.py"}` + "\n"
	)
	return map[string]antigravitySummaryCase{
		"app and IDE": {
			agent: parser.AgentAntigravity,
			id:    dbtest.AntigravityTitleID,
			write: func(t *testing.T, root string) {
				t.Helper()
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello")
				dbPath := filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
				require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
				copyAntigravityCLITestSchemaTemplate(t, dbPath)
			},
		},
		"CLI": {
			agent: parser.AgentAntigravityCLI,
			id:    cliID,
			// A brain transcript line for every database step makes the parse
			// complete, so an unchanged conversation is not retried each sync.
			write: func(t *testing.T, root string) {
				t.Helper()
				convDir := filepath.Join(root, "conversations")
				require.NoError(t, os.MkdirAll(convDir, 0o755))
				createAntigravityCLIDisplayStepDB(t, filepath.Join(convDir, cliID+".db"), "Read calc.py")
				testjsonl.WriteAntigravityBrainTranscript(t, root, cliID, cliPrompt)
			},
		},
	}
}

func newAntigravitySummaryEngine(
	t *testing.T, agent parser.AgentType, root string, policy config.ArchiveContent,
) (*sync.Engine, *db.DB) {
	t.Helper()
	archive, err := db.OpenWithArchiveContent(t.Context(), filepath.Join(t.TempDir(), "sessions.db"), policy)
	require.NoError(t, err)
	t.Cleanup(func() { archive.Close() })
	require.Equal(t, policy == config.ArchiveContentUsage, archive.ArchiveContent().UsageOnly())
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs:      map[parser.AgentType][]string{agent: {root}},
		Machine:        "host-a",
		ArchiveContent: policy,
	})
	t.Cleanup(engine.Close)
	return engine, archive
}

// A session archived without a workspace or parent, by an older build or
// before the store recorded them, gains both on a full sync although its
// own files are unchanged. This includes archives that keep usage only.
func TestAntigravitySummaryStoreFillsArchivedSessions(t *testing.T) {
	for name, tc := range antigravitySummaryCases() {
		for _, policy := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
			t.Run(name+"/"+string(policy), func(t *testing.T) {
				root := t.TempDir()
				tc.write(t, root)
				engine, archive := newAntigravitySummaryEngine(t, tc.agent, root, policy)
				id := string(tc.agent) + ":" + tc.id

				require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				before := antigravityStoredSession(t, archive, id)
				require.Empty(t, before.Cwd)
				require.Nil(t, before.ParentSessionID)

				testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
					tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
				})
				assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				filled := antigravityStoredSession(t, archive, id)
				assert.Equal(t, "/tmp/ws-proj", filled.Cwd)
				assert.Equal(t, "ws_proj", filled.Project)
				assert.Equal(t, before.MessageCount, filled.MessageCount)

				// The working directory already matches, so only the store row
				// in the fingerprint can tell sync that the parent appeared.
				testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
					tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`, ParentID: antigravitySummaryParent},
				})
				assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
				linked := antigravityStoredSession(t, archive, id)
				assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent), linked.ParentSessionID)
				assert.Equal(t, "subagent", linked.RelationshipType)

				assert.Equal(t, 0, engine.SyncAll(t.Context(), nil).Synced, "an unchanged store reparses nothing")
			})
		}
	}
}

// A store-only change reaches the archive on the next full sync through the
// source fingerprint. A cleared parent shows this: unlike a moved workspace,
// it does not change the source's cwd authority.
func TestAntigravitySummaryStoreChangeReachesNextFullSync(t *testing.T) {
	for name, tc := range antigravitySummaryCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.write(t, root)
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`, ParentID: antigravitySummaryParent},
			})
			engine, archive := newAntigravitySummaryEngine(t, tc.agent, root, config.ArchiveContentFull)
			id := string(tc.agent) + ":" + tc.id
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			require.NotNil(t, antigravityStoredSession(t, archive, id).ParentSessionID)

			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
			})
			assert.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			cleared := antigravityStoredSession(t, archive, id)
			assert.Equal(t, "/tmp/ws-proj", cleared.Cwd)
			assert.Nil(t, cleared.ParentSessionID, "a cleared parent reaches the archive")
		})
	}
}

// A store that cannot be read is eventually consistent: the stored working
// directory survives, and once the store reads again the next full sync
// restores everything the store supplies.
func TestAntigravityUnreadableSummaryStoreRecovers(t *testing.T) {
	for name, tc := range antigravitySummaryCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.write(t, root)
			store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`, ParentID: antigravitySummaryParent},
			})
			engine, archive := newAntigravitySummaryEngine(t, tc.agent, root, config.ArchiveContentFull)
			id := string(tc.agent) + ":" + tc.id
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			good, err := os.ReadFile(store)
			require.NoError(t, err)

			require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
			engine.SyncAll(t.Context(), nil)
			assert.Equal(t, "/tmp/ws-proj", antigravityStoredSession(t, archive, id).Cwd)

			require.NoError(t, os.WriteFile(store, good, 0o644))
			engine.SyncAll(t.Context(), nil)
			recovered := antigravityStoredSession(t, archive, id)
			assert.Equal(t, "/tmp/ws-proj", recovered.Cwd)
			assert.Equal(t, "ws_proj", recovered.Project)
			assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent), recovered.ParentSessionID)
		})
	}
}

func antigravityStoredSession(t *testing.T, archive *db.DB, id string) *db.Session {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}

// Local changed-path and watch-batch syncs refresh archived workspace,
// project, and parent metadata when conversation_summaries.db (or its WAL)
// changes, while title-only edits select no transcripts.
func TestAntigravitySummaryStoreChangedPathSyncRefreshesMetadata(t *testing.T) {
	const (
		changedID = "11111111-2222-3333-4444-555555555551"
		siblingID = "11111111-2222-3333-4444-555555555552"
	)
	type testCase struct {
		agent     parser.AgentType
		container bool
		write     func(t *testing.T, root, id, prompt string)
	}
	writeIDE := func(t *testing.T, root, id, prompt string) {
		t.Helper()
		testjsonl.WriteAntigravityBrainTranscript(t, root, id,
			`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"`+prompt+`"}`+"\n")
		dbPath := filepath.Join(root, "conversations", id+".db")
		require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))
		copyAntigravityCLITestSchemaTemplate(t, dbPath)
	}
	writeCLI := func(t *testing.T, root, id, prompt string) {
		t.Helper()
		convDir := filepath.Join(root, "conversations")
		require.NoError(t, os.MkdirAll(convDir, 0o755))
		createAntigravityCLIDisplayStepDB(t, filepath.Join(convDir, id+".db"), prompt)
		testjsonl.WriteAntigravityBrainTranscript(t, root, id,
			`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"`+prompt+`"}`+"\n")
	}
	cases := map[string]testCase{
		"app and IDE":   {agent: parser.AgentAntigravity, write: writeIDE},
		"CLI":           {agent: parser.AgentAntigravityCLI, write: writeCLI},
		"CLI container": {agent: parser.AgentAntigravityCLI, container: true, write: writeCLI},
	}

	for name, tc := range cases {
		for _, mode := range []string{"sync_paths", "watch_batch"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				base := t.TempDir()
				configuredRoot := base
				sessionRoot := base
				if tc.container {
					configuredRoot = filepath.Join(base, ".t3", "userdata", "providers", "antigravity")
					sessionRoot = filepath.Join(configuredRoot, "inst-a", "antigravity-acp")
				}
				tc.write(t, sessionRoot, changedID, "first prompt")
				tc.write(t, sessionRoot, siblingID, "second prompt")
				store := testjsonl.WriteAntigravitySummaries(t, sessionRoot, map[string]testjsonl.AntigravitySummary{
					changedID: {Title: "Alpha", WorkspaceURIs: `["file:///tmp/ws-old"]`},
					siblingID: {Title: "Beta", WorkspaceURIs: `["file:///tmp/ws-keep"]`},
				})

				engine, archive := newAntigravitySummaryEngine(t, tc.agent, configuredRoot, config.ArchiveContentFull)
				require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

				changedFullID := string(tc.agent) + ":" + changedID
				siblingFullID := string(tc.agent) + ":" + siblingID
				changedInitial := antigravityStoredSession(t, archive, changedFullID)
				siblingInitial := antigravityStoredSession(t, archive, siblingFullID)
				require.Equal(t, "/tmp/ws-old", changedInitial.Cwd)
				require.Nil(t, changedInitial.ParentSessionID)

				runChangedPath := func(path string) sync.SyncStats {
					t.Helper()
					if mode == "watch_batch" {
						stats, err := engine.SyncWatchBatchThenRun(
							t.Context(), sync.WatchBatch{Paths: []string{path}}, nil, nil,
						)
						require.NoError(t, err)
						return stats
					}
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
					return engine.LastSyncStats()
				}

				// Title-only edit selects zero transcripts.
				testjsonl.WriteAntigravitySummaries(t, sessionRoot, map[string]testjsonl.AntigravitySummary{
					changedID: {Title: "Alpha renamed", WorkspaceURIs: `["file:///tmp/ws-old"]`},
					siblingID: {Title: "Beta", WorkspaceURIs: `["file:///tmp/ws-keep"]`},
				})
				titleStats := runChangedPath(store)
				if mode == "watch_batch" {
					assert.Equal(t, 0, titleStats.Synced)
				} else {
					// A 0-file SyncPathsContext leaves LastSyncStats().Synced
					// from the prior SyncAll intact; selecting any unchanged
					// transcript would overwrite it with Synced=0, Skipped>0.
					assert.Equal(t, 2, titleStats.Synced)
				}
				assert.Equal(t, 0, titleStats.Skipped)
				assert.Equal(t, 1, titleStats.TitlesUpdated)
				afterTitle := antigravityStoredSession(t, archive, changedFullID)
				assert.Equal(t, new("Alpha renamed"), afterTitle.SessionName)
				assert.Equal(t, changedInitial.FileHash, afterTitle.FileHash)

				// Workspace and parent edit refreshes only the affected session,
				// while a title-only rename on the sibling skips reparsing.
				testjsonl.WriteAntigravitySummaries(t, sessionRoot, map[string]testjsonl.AntigravitySummary{
					changedID: {
						Title:         "Alpha renamed",
						WorkspaceURIs: `["file:///tmp/ws-new"]`,
						ParentID:      antigravitySummaryParent,
					},
					siblingID: {Title: "Beta renamed", WorkspaceURIs: `["file:///tmp/ws-keep"]`},
				})
				metaStats := runChangedPath(store)
				assert.Equal(t, 1, metaStats.Synced)
				assert.Equal(t, 1, metaStats.TitlesUpdated)

				updated := antigravityStoredSession(t, archive, changedFullID)
				assert.Equal(t, "/tmp/ws-new", updated.Cwd)
				assert.Equal(t, "ws_new", updated.Project)
				assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent), updated.ParentSessionID)
				assert.Equal(t, "subagent", updated.RelationshipType)
				assert.Equal(t, new("Alpha renamed"), updated.SessionName)

				siblingAfter := antigravityStoredSession(t, archive, siblingFullID)
				assert.Equal(t, new("Beta renamed"), siblingAfter.SessionName)
				assert.Equal(t, siblingInitial.FileHash, siblingAfter.FileHash)

				// An uncheckpointed WAL update clearing the parent refreshes via -wal.
				conn, err := sql.Open("sqlite3", store)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, conn.Close()) })
				_, err = conn.ExecContext(t.Context(), "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
				require.NoError(t, err)
				_, err = conn.ExecContext(
					t.Context(),
					"UPDATE conversation_summaries SET workspace_uris = ?, parent_conversation_id = '' WHERE conversation_id = ?",
					`["file:///tmp/ws-wal"]`, changedID,
				)
				require.NoError(t, err)

				walStats := runChangedPath(store + "-wal")
				assert.Equal(t, 1, walStats.Synced)
				walUpdated := antigravityStoredSession(t, archive, changedFullID)
				assert.Equal(t, "/tmp/ws-wal", walUpdated.Cwd)
				assert.Equal(t, "ws_wal", walUpdated.Project)
				assert.Nil(t, walUpdated.ParentSessionID)
			})
		}
	}
}
