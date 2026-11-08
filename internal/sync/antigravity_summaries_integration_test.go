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
