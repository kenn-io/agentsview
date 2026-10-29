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
	// write creates the conversation; change alters its own files; path is
	// the source the session is archived from.
	write  func(t *testing.T, root string)
	change func(t *testing.T, root string)
	path   func(root string) string
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
			path: func(root string) string {
				return filepath.Join(root, "conversations", dbtest.AntigravityTitleID+".db")
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
			path: func(root string) string { return filepath.Join(root, "conversations", cliID+".db") },
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
// incremental sync without waiting for a full sync. No discovery ever reads
// the store before the write, and each step uses a fresh engine, as after a
// restart or in a remote import that plans without discovery: routing
// compares the store with the archive, not with anything a process saw.
func TestAntigravitySummaryStoreWriteReachesIncrementalSync(t *testing.T) {
	for name, tc := range antigravitySummaryCases() {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tc.write(t, root)
			_, archive := newAntigravitySummaryEngine(t, tc.agent, root, config.ArchiveContentFull)
			id := string(tc.agent) + ":" + tc.id
			path := tc.path(root)
			require.NoError(t, archive.UpsertSession(t.Context(), db.Session{
				ID: id, Agent: string(tc.agent), Machine: "host-a", FilePath: &path, Cwd: "/tmp/ws-proj",
			}))
			store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/moved-proj"]`},
			})

			// Direct path sync, as `agentsview sync` paths use.
			require.NoError(t, antigravityEngineOn(t, tc.agent, root, archive).SyncPathsContext(t.Context(), []string{store}))
			moved := antigravityStoredSession(t, archive, id)
			assert.Equal(t, "/tmp/moved-proj", moved.Cwd)
			assert.Equal(t, "moved_proj", moved.Project)

			// The watcher's planned path, with only the parent changing: set,
			// then cleared.
			planned := func() {
				t.Helper()
				planner := antigravityEngineOn(t, tc.agent, root, archive)
				plan, err := planner.PlanChangedPathsContext(t.Context(), []string{store})
				require.NoError(t, err)
				_, err = planner.SyncChangedPathPlanContext(t.Context(), plan, nil)
				require.NoError(t, err)
			}
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/moved-proj"]`, ParentID: antigravitySummaryParent},
			})
			planned()
			assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent),
				antigravityStoredSession(t, archive, id).ParentSessionID)
			testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
				tc.id: {WorkspaceURIs: `["file:///tmp/moved-proj"]`},
			})
			planned()
			assert.Nil(t, antigravityStoredSession(t, archive, id).ParentSessionID, "a cleared parent reaches the archive")
		})
	}
}

// When two roots hold the same conversation, a store write in one root never
// routes the session archived from the other, which would replace its
// transcript with the other root's copy.
func TestAntigravitySummaryStoreWriteSkipsSessionsOfOtherRoots(t *testing.T) {
	tc := antigravitySummaryCases()["app and IDE"]
	roots := []string{t.TempDir(), t.TempDir()}
	stores := make([]string, len(roots))
	for i, root := range roots {
		tc.write(t, root)
		stores[i] = testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
			tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
		})
	}
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{tc.agent: roots}, Machine: "host-a",
	})
	t.Cleanup(engine.Close)
	id := string(tc.agent) + ":" + tc.id
	engine.SyncAll(t.Context(), nil)
	owned := antigravityStoredSession(t, archive, id)
	require.NotNil(t, owned.FilePath)
	owner, other := 0, 1
	if *owned.FilePath != tc.path(roots[0]) {
		owner, other = 1, 0
	}
	require.Equal(t, tc.path(roots[owner]), *owned.FilePath)

	testjsonl.WriteAntigravitySummaries(t, roots[other], map[string]testjsonl.AntigravitySummary{
		tc.id: {WorkspaceURIs: `["file:///tmp/other-copy"]`},
	})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{stores[other]}))
	kept := antigravityStoredSession(t, archive, id)
	assert.Equal(t, owned.FilePath, kept.FilePath)
	assert.Equal(t, "/tmp/ws-proj", kept.Cwd)

	testjsonl.WriteAntigravitySummaries(t, roots[owner], map[string]testjsonl.AntigravitySummary{
		tc.id: {WorkspaceURIs: `["file:///tmp/moved-proj"]`},
	})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{stores[owner]}))
	assert.Equal(t, "/tmp/moved-proj", antigravityStoredSession(t, archive, id).Cwd, "the owning root still routes")
}

// With a cwd allow-list, a conversation parsed before its store row named a
// workspace is filtered out and never archived. Once the store names an
// allowed workspace, the store write routes it.
func TestAntigravitySummaryStoreRoutesSessionsTheAllowListAdmitsLater(t *testing.T) {
	tc := antigravitySummaryCases()["app and IDE"]
	root := t.TempDir()
	tc.write(t, root)
	archive := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{tc.agent: {root}}, Machine: "host-a",
		IncludeCwdPrefixes: []string{"/tmp/allowed"},
	})
	t.Cleanup(engine.Close)
	id := string(tc.agent) + ":" + tc.id
	engine.SyncAll(t.Context(), nil)
	missing, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.Nil(t, missing, "without a workspace the allow-list filters the conversation out")

	store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		tc.id: {WorkspaceURIs: `["file:///tmp/allowed/proj"]`},
	})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, "/tmp/allowed/proj", antigravityStoredSession(t, archive, id).Cwd)
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

// A store change whose first processing fails is routed again on retry,
// because the archive still disagrees with the store. A parent shows this: it
// reaches the archive only through a successful reparse.
func TestAntigravitySummaryStoreChangeSurvivesFailedProcessing(t *testing.T) {
	root := t.TempDir()
	tc := antigravitySummaryCases()["app and IDE"]
	tc.write(t, root)
	store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
	})
	engine, archive := newAntigravitySummaryEngine(t, tc.agent, root, config.ArchiveContentFull)
	id := string(tc.agent) + ":" + tc.id
	require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

	testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		tc.id: {WorkspaceURIs: `["file:///tmp/ws-proj"]`, ParentID: antigravitySummaryParent},
	})
	dbPath := filepath.Join(root, "conversations", tc.id+".db")
	require.NoError(t, os.WriteFile(dbPath, []byte("not a database"), 0o644))
	require.Error(t, engine.SyncPathsContext(t.Context(), []string{store}))
	require.Nil(t, antigravityStoredSession(t, archive, id).ParentSessionID)

	require.NoError(t, os.Remove(dbPath))
	copyAntigravityCLITestSchemaTemplate(t, dbPath)
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, new(string(tc.agent)+":"+antigravitySummaryParent),
		antigravityStoredSession(t, archive, id).ParentSessionID)
}

// After a restart, an app or IDE root whose store is gone leaves alone every
// archived session holding a value the store supplied: a working directory,
// a parent, or a project. A reparse would clear them. A project the user
// assigned is not from the store, so it does not hold the session back.
func TestAntigravityMissingSummaryStoreKeepsArchivedSessions(t *testing.T) {
	parent := "antigravity:" + antigravitySummaryParent
	tests := map[string]struct {
		session  func(db.Session) db.Session
		assigned string
		kept     bool
	}{
		"working directory": {session: func(s db.Session) db.Session { s.Cwd = "/tmp/ws-proj"; return s }, kept: true},
		// A sub-agent whose workspace is not a local folder has no cwd.
		"parent only": {session: func(s db.Session) db.Session { s.ParentSessionID = &parent; return s }, kept: true},
		// A remote session's cwd is always empty.
		"project only": {session: func(s db.Session) db.Session { s.Project = "ws_proj"; return s }, kept: true},
		"assigned project only": {
			session:  func(s db.Session) db.Session { return s },
			assigned: "chosen",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			cases := antigravitySummaryCases()["app and IDE"]
			cases.write(t, root)
			engine, archive := newAntigravitySummaryEngine(t, cases.agent, root, config.ArchiveContentFull)
			id := string(cases.agent) + ":" + cases.id
			// A row as an earlier run stored it from a store this process never read.
			path := cases.path(root)
			want := tc.session(db.Session{ID: id, Agent: string(cases.agent), Machine: "host-a", FilePath: &path})
			require.NoError(t, archive.UpsertSession(t.Context(), want))
			if tc.assigned != "" {
				_, err := archive.AssignSessionProject(t.Context(), id, tc.assigned)
				require.NoError(t, err)
			}

			stats := engine.SyncAll(t.Context(), nil)
			got := antigravityStoredSession(t, archive, id)
			if !tc.kept {
				assert.Zero(t, stats.Failed)
				assert.Equal(t, 1, stats.Synced, "nothing from the store holds the session back")
				assert.Equal(t, tc.assigned, got.Project)
				return
			}
			assert.Equal(t, 1, stats.Failed, "the session is retried, not reparsed")
			assert.Equal(t, want.Cwd, got.Cwd)
			assert.Equal(t, want.Project, got.Project)
			assert.Equal(t, want.ParentSessionID, got.ParentSessionID)
		})
	}
}

// A CLI conversation that exists only as implicit/<id>.pb is routed too.
func TestAntigravitySummaryStoreRoutesCLIImplicitSessions(t *testing.T) {
	const uuid = "34343434-5656-7878-9090-121212121212"
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "implicit"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "implicit", uuid+".pb"), []byte("encrypted"), 0o644))
	testjsonl.WriteAntigravityBrainTranscript(t, root, uuid,
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"Read calc.py"}`+"\n")
	store := testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		uuid: {WorkspaceURIs: `["file:///tmp/ws-proj"]`},
	})
	engine, archive := newAntigravitySummaryEngine(t, parser.AgentAntigravityCLI, root, config.ArchiveContentFull)
	id := "antigravity-cli:implicit-" + uuid
	engine.SyncAll(t.Context(), nil)
	require.Equal(t, "/tmp/ws-proj", antigravityStoredSession(t, archive, id).Cwd)

	testjsonl.WriteAntigravitySummaries(t, root, map[string]testjsonl.AntigravitySummary{
		uuid: {WorkspaceURIs: `["file:///tmp/moved-proj"]`},
	})
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{store}))
	assert.Equal(t, "/tmp/moved-proj", antigravityStoredSession(t, archive, id).Cwd)
}

func antigravityEngineOn(t *testing.T, agent parser.AgentType, root string, archive *db.DB) *sync.Engine {
	t.Helper()
	engine := sync.NewEngine(t.Context(), archive, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: {root}},
		Machine:   "host-a",
	})
	t.Cleanup(engine.Close)
	return engine
}

func antigravityStoredSession(t *testing.T, archive *db.DB, id string) *db.Session {
	t.Helper()
	session, err := archive.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, session)
	return session
}
