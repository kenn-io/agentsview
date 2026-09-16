package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// TestClineReconciliation_DeletedTeammateLifecycle verifies the full lifecycle
// of a deleted Cline teammate transcript:
//  1. Sync parent session and teammate transcript -> both active.
//  2. Delete teammate file and resync -> teammate is tombstoned via
//     source-missing, NOT hard-deleted into trash (deleted_at is nil).
//  3. Restore the file and resync -> teammate revives and clears source_missing_at.
func TestClineReconciliation_DeletedTeammateLifecycle(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "data", "sessions", "sess-lifecycle")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, "sess-lifecycle.json")
	metaJSON := `{"session_id":"sess-lifecycle","cwd":"/workspace","started_at":"2026-09-12T10:00:00Z"}`
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	msgPath := filepath.Join(sessDir, "sess-lifecycle.messages.json")
	msgJSON := `{"version":1,"messages":[{"id":"m1","role":"user","content":[{"type":"text","text":"start"}],"ts":1000}]}`
	require.NoError(t, os.WriteFile(msgPath, []byte(msgJSON), 0o644))

	tmPath := filepath.Join(sessDir, "scout__t1.messages.json")
	tmJSON := `{"version":1,"sessionId":"sess-lifecycle__teamtask__scout__t1","origin":{"subagent":"scout"},"messages":[{"id":"tm1","role":"user","content":[{"type":"text","text":"scout task"}],"ts":1050}]}`
	require.NoError(t, os.WriteFile(tmPath, []byte(tmJSON), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {root},
		},
		Machine: "test-machine",
	})
	t.Cleanup(engine.Close)

	// Step 1: Initial sync (parent + teammate)
	res := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 2, res.Synced)

	teammateID := "cline:sess-lifecycle__teamtask__scout__t1"
	sess, err := database.GetSessionFull(t.Context(), teammateID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Nil(t, sess.DeletedAt)
	assert.Nil(t, sess.SourceMissingAt)

	// Step 2: Delete teammate file and resync.
	require.NoError(t, os.Remove(tmPath))
	now := time.Now().Add(5 * time.Second)
	require.NoError(t, os.Chtimes(metaPath, now, now))

	res2 := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 1, res2.Synced)

	archived, err := database.GetSessionFull(t.Context(), teammateID)
	require.NoError(t, err)
	assertSourceMissingState(t, archived)
	assert.Nil(t, archived.DeletedAt, "missing teammate file must not put session into trash")

	// Step 3: Restore teammate file and resync -> session revives.
	require.NoError(t, os.WriteFile(tmPath, []byte(tmJSON), 0o644))
	now2 := time.Now().Add(10 * time.Second)
	require.NoError(t, os.Chtimes(metaPath, now2, now2))

	res3 := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 2, res3.Synced)

	revived, err := database.GetSessionFull(t.Context(), teammateID)
	require.NoError(t, err)
	require.NotNil(t, revived)
	assert.Nil(t, revived.SourceMissingAt, "source_missing_at must be cleared when file returns")
	assert.Nil(t, revived.DeletedAt)
}

// TestClineReconciliation_DeletedMetadataLifecycle verifies that deleting the
// owning metadata file still reaches source-missing reconciliation for the
// parent and every teammate result in its session directory.
func TestClineReconciliation_DeletedMetadataLifecycle(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "data", "sessions", "sess-metadata")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, "sess-metadata.json")
	metaJSON := `{"session_id":"sess-metadata","cwd":"/workspace","started_at":"2026-09-12T10:00:00Z"}`
	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))

	msgPath := filepath.Join(sessDir, "sess-metadata.messages.json")
	msgJSON := `{"version":1,"messages":[{"id":"m1","role":"user","content":[{"type":"text","text":"start"}],"ts":1000}]}`
	require.NoError(t, os.WriteFile(msgPath, []byte(msgJSON), 0o644))

	tmPath := filepath.Join(sessDir, "scout__t1.messages.json")
	tmJSON := `{"version":1,"sessionId":"sess-metadata__teamtask__scout__t1","origin":{"subagent":"scout"},"messages":[{"id":"tm1","role":"user","content":[{"type":"text","text":"scout task"}],"ts":1050}]}`
	require.NoError(t, os.WriteFile(tmPath, []byte(tmJSON), 0o644))

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentCline: {root},
		},
		Machine: "test-machine",
	})
	t.Cleanup(engine.Close)

	first := engine.SyncAll(t.Context(), nil)
	require.Equal(t, 2, first.Synced)

	parentID := "cline:sess-metadata"
	teammateID := "cline:sess-metadata__teamtask__scout__t1"
	for _, id := range []string{parentID, teammateID} {
		session, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Nil(t, session.SourceMissingAt)
		assert.Nil(t, session.DeletedAt)
	}

	require.NoError(t, os.Remove(metaPath))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{metaPath}))

	for _, id := range []string{parentID, teammateID} {
		session, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session, "session %s should remain archived", id)
		assertSourceMissingState(t, session)
	}

	require.NoError(t, os.WriteFile(metaPath, []byte(metaJSON), 0o644))
	require.NoError(t, engine.SyncPathsContext(t.Context(), []string{metaPath}))

	for _, id := range []string{parentID, teammateID} {
		session, err := database.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Nil(t, session.SourceMissingAt)
		assert.Nil(t, session.DeletedAt)
	}
}
func TestClineRemoteIdentityStableAcrossResyncs(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "data", "sessions", "sess-remote")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))

	metaPath := filepath.Join(sessDir, "sess-remote.json")
	require.NoError(t, os.WriteFile(metaPath, []byte(`{"session_id":"sess-remote","cwd":"/workspace"}`), 0o644))
	parentPath := filepath.Join(sessDir, "sess-remote.messages.json")
	parentJSON := `{"messages":[
		{"id":"p1","role":"assistant","content":[{"type":"tool_use","id":"run-worker","name":"team_run_task","input":{"agentId":"worker","taskId":"task-1"}}],"ts":1000},
		{"id":"p2","role":"user","content":[{"type":"tool_result","tool_use_id":"run-worker","content":"done"}],"ts":2000}
	]}`
	require.NoError(t, os.WriteFile(parentPath, []byte(parentJSON), 0o644))
	teammatePath := filepath.Join(sessDir, "worker__remote.messages.json")
	teammateJSON := func(text string) string {
		return `{"sessionId":"sess-remote__teamtask__worker__remote","origin":{"subagent":"worker"},"messages":[{"id":"t1","role":"user","content":[{"type":"text","text":"` + text + `"}],"ts":1100}]}`
	}
	require.NoError(t, os.WriteFile(teammatePath, []byte(teammateJSON("first")), 0o644))

	logicalRoot := "remote:/cline"
	rewrite := func(path string) string {
		if strings.HasPrefix(path, logicalRoot+"/") {
			return path
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return path
		}
		return logicalRoot + "/" + filepath.ToSlash(rel)
	}
	resolve := func(path string) (string, bool) {
		rel, ok := strings.CutPrefix(path, logicalRoot+"/")
		if !ok {
			return "", false
		}
		return filepath.Join(root, filepath.FromSlash(rel)), true
	}

	database := dbtest.OpenTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{parser.AgentCline: {root}},
		Machine:   "remote-host", IDPrefix: "remote-host~",
		PathRewriter: rewrite, StoredPathResolver: resolve,
	})
	t.Cleanup(engine.Close)

	parentID := "remote-host~cline:sess-remote"
	childID := "remote-host~cline:sess-remote__teamtask__worker__remote"
	readIdentity := func() (string, string, string, string) {
		t.Helper()
		parent, err := database.GetSessionFull(t.Context(), parentID)
		require.NoError(t, err)
		require.NotNil(t, parent)
		messages, err := database.GetAllMessages(t.Context(), parentID)
		require.NoError(t, err)
		var link, resultLink string
		for _, message := range messages {
			for _, call := range message.ToolCalls {
				if call.ToolName == "team_run_task" {
					link = call.SubagentSessionID
					if len(call.ResultEvents) == 1 {
						resultLink = call.ResultEvents[0].SubagentSessionID
					}
				}
			}
		}
		return parent.ID, childID, link, resultLink
	}

	first := engine.SyncAll(t.Context(), nil)
	require.Greater(t, first.Synced, 0)
	wantParent, wantChild, wantLink, wantResultLink := readIdentity()
	assert.Equal(t, parentID, wantParent)
	assert.Equal(t, childID, wantChild)
	assert.Equal(t, childID, wantLink)
	assert.Equal(t, childID, wantResultLink)

	for _, text := range []string{"second", "third"} {
		require.NoError(t, os.WriteFile(teammatePath, []byte(teammateJSON(text)), 0o644))
		res := engine.SyncAll(t.Context(), nil)
		require.Greater(t, res.Synced, 0)
		gotParent, gotChild, gotLink, gotResultLink := readIdentity()
		assert.Equal(t, wantParent, gotParent)
		assert.Equal(t, wantChild, gotChild)
		assert.Equal(t, wantLink, gotLink)
		assert.Equal(t, wantResultLink, gotResultLink)
	}

	storedPath := rewrite(teammatePath)
	ids, err := database.ListSessionIDsByFilePath(storedPath, string(parser.AgentCline))
	require.NoError(t, err)
	assert.Equal(t, []string{childID}, ids)
}
