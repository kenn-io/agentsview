package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	remoteParentRawID = "11111111-2222-4333-8444-555555555555"
	remoteChildFile   = "agent-remote-worker.jsonl"
	remoteChildID     = "hostA~agent-remote-worker"
)

// remoteParentEnv pulls one Claude subagent transcript from hostA. Its parent
// transcript is not on hostA, so the parser's raw parent ID must resolve
// against sessions other hosts already archived.
type remoteParentEnv struct {
	db        *db.DB
	engine    *sync.Engine
	childPath string
}

func newRemoteParentEnv(t *testing.T) *remoteParentEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	database := dbtest.OpenTestDB(t)
	claudeDir := t.TempDir()
	childPath := filepath.Join(
		claudeDir, "proj", remoteParentRawID, "subagents", remoteChildFile,
	)
	require.NoError(t, os.MkdirAll(filepath.Dir(childPath), 0o755))
	content := testjsonl.NewSessionBuilder().
		AddClaudeUserWithSessionID(tsEarly, "Do subtask", remoteParentRawID).
		AddClaudeAssistant(tsEarlyS5, "Subtask done.").
		String()
	require.NoError(t, os.WriteFile(childPath, []byte(content), 0o644))
	engine := sync.NewEngine(t.Context(), database, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		Machine:  "hostA",
		IDPrefix: "hostA~",
	})
	t.Cleanup(engine.Close)
	return &remoteParentEnv{db: database, engine: engine, childPath: childPath}
}

func (env *remoteParentEnv) archiveParent(t *testing.T, host string) {
	t.Helper()
	first := "parent task"
	require.NoError(t, env.db.UpsertSession(t.Context(), db.Session{
		ID:           host + "~" + remoteParentRawID,
		Project:      "proj",
		Machine:      host,
		Agent:        string(parser.AgentClaude),
		FirstMessage: &first,
		MessageCount: 1,
	}))
}

// syncOne runs one sync pass and requires that it wrote the child.
func (env *remoteParentEnv) syncOne(t *testing.T) {
	t.Helper()
	stats := env.engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, stats.Synced, "sync stats: %+v", stats)
}

// reparseChild forces a full parse of the unchanged child, as a remote full
// import or a parser version bump does. The full write upserts the session
// row, parent columns included.
func (env *remoteParentEnv) reparseChild(t *testing.T) {
	t.Helper()
	stats := env.engine.SyncAllForceParse(t.Context(), nil)
	require.Equal(t, 1, stats.Synced, "sync stats: %+v", stats)
}

func (env *remoteParentEnv) assertParent(t *testing.T, want string) {
	t.Helper()
	sess, err := env.db.GetSession(t.Context(), remoteChildID)
	require.NoError(t, err)
	require.NotNil(t, sess, "child session must be archived")
	require.NotNil(t, sess.ParentSessionID)
	assert.Equal(t, want, *sess.ParentSessionID, "parent_session_id")
	parent, parserParent, err := env.db.SessionParentIDs(t.Context(), remoteChildID)
	require.NoError(t, err)
	assert.Equal(t, want, parent, "stored parent_session_id")
	assert.Equal(t, want, parserParent, "stored parser_parent_session_id")
}

func TestRemoteChildLinksParentArchivedFromAnotherHost(t *testing.T) {
	env := newRemoteParentEnv(t)
	env.archiveParent(t, "hostB")

	env.syncOne(t)
	env.assertParent(t, "hostB~"+remoteParentRawID)
}

func TestRemoteChildKeepsSourceHostParentWhenNoneArchived(t *testing.T) {
	env := newRemoteParentEnv(t)

	env.syncOne(t)
	env.assertParent(t, "hostA~"+remoteParentRawID)

	// The parent arrives from hostB later; the next reparse links it.
	env.archiveParent(t, "hostB")
	env.reparseChild(t)
	env.assertParent(t, "hostB~"+remoteParentRawID)
}

func TestRemoteChildDoesNotGuessBetweenHostsWithSameParent(t *testing.T) {
	env := newRemoteParentEnv(t)
	env.archiveParent(t, "hostB")
	env.archiveParent(t, "hostC")

	env.syncOne(t)
	env.assertParent(t, "hostA~"+remoteParentRawID)
}

func TestRemoteChildReparseKeepsResolvedParent(t *testing.T) {
	env := newRemoteParentEnv(t)
	env.archiveParent(t, "hostB")
	env.syncOne(t)
	env.assertParent(t, "hostB~"+remoteParentRawID)

	// A second host now holds the same parent ID. Fresh resolution would be
	// ambiguous, so the earlier resolution must survive a full reparse.
	env.archiveParent(t, "hostC")
	env.reparseChild(t)
	env.assertParent(t, "hostB~"+remoteParentRawID)

	// A full resync rebuilds into a fresh database whose resolution must
	// consult the original archive.
	stats := env.engine.ResyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "resync warnings: %v", stats.Warnings)
	env.assertParent(t, "hostB~"+remoteParentRawID)
}

func TestRemoteChildPrefersParentFromItsOwnHost(t *testing.T) {
	env := newRemoteParentEnv(t)
	env.archiveParent(t, "hostB")
	env.syncOne(t)
	env.assertParent(t, "hostB~"+remoteParentRawID)

	// The source host's own copy of the parent is never a guess, so it
	// replaces the earlier cross-host link on the next full reparse.
	env.archiveParent(t, "hostA")
	env.reparseChild(t)
	env.assertParent(t, "hostA~"+remoteParentRawID)
}
