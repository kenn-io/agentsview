package sync_test

import (
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
	// write creates the conversation; change alters its own files.
	write  func(t *testing.T, root string)
	change func(t *testing.T, root string)
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
			change: func(t *testing.T, root string) {
				t.Helper()
				dbtest.WriteAntigravityTitleTranscript(t, root, "hello again")
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
			change: func(t *testing.T, root string) {
				t.Helper()
				conn := openAntigravityCLITestDB(t, filepath.Join(root, "conversations", cliID+".db"))
				insertAntigravityCLIStep(t, conn, 1, 17, "calc.py defines add")
				require.NoError(t, conn.Close())
				testjsonl.WriteAntigravityBrainTranscript(t, root, cliID, cliPrompt+
					`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"calc.py defines add"}`+"\n")
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

// A store write that changes a conversation's workspace or parent reaches
// incremental sync without waiting for a full sync.
func TestAntigravitySummaryStoreWriteReachesIncrementalSync(t *testing.T) {
	for name, tc := range antigravitySummaryCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.write(t, root)
			store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
			})
			engine, archive := newAntigravitySummaryEngine(t, tc.agent, root, config.ArchiveContentFull)
			id := string(tc.agent) + ":" + tc.id
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			require.Equal(t, "/tmp/ws-proj", antigravityStoredSession(t, archive, id).Cwd)

			// Direct path sync, as `agentsview sync` paths use.
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/moved-proj"]`},
			})
			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
			moved := antigravityStoredSession(t, archive, id)
			assert.Equal(t, "/tmp/moved-proj", moved.Cwd)
			assert.Equal(t, "moved_proj", moved.Project)

			// The watcher's planned path, with only the parent changing.
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/moved-proj"]`, ParentID: antigravitySummaryParent},
			})
			plan, err := engine.PlanChangedPathsContext(t.Context(), []string{store})
			require.NoError(t, err)
			_, err = engine.SyncChangedPathPlanContext(t.Context(), plan, nil)
			require.NoError(t, err)
			assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent),
				antigravityStoredSession(t, archive, id).ParentSessionID)
		})
	}
}

// A conversation that changes while its root's store cannot be read keeps
// the working directory and parent the store gave it.
func TestAntigravityUnreadableSummaryStoreKeepsArchivedMetadata(t *testing.T) {
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
			filled := antigravityStoredSession(t, archive, id)
			require.Equal(t, "/tmp/ws-proj", filled.Cwd)

			require.NoError(t, os.WriteFile(store, []byte("not a database"), 0o644))
			tc.change(t, root)
			engine.SyncAll(t.Context(), nil)
			changed := antigravityStoredSession(t, archive, id)
			assert.NotEqual(t, filled.FileHash, changed.FileHash, "the changed conversation was reparsed")
			assert.Equal(t, "/tmp/ws-proj", changed.Cwd)
			assert.Equal(t, "ws_proj", changed.Project)
			assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent), changed.ParentSessionID)
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
