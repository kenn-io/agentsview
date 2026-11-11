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

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// kiro-cli writes a session's transcript to
// <workspace>/sess_<id>/messages.jsonl and each subagent it spawns to
// <workspace>/sess_<id>/sub-executions/<subSessionId>.jsonl. The spawning
// transcript records the delegation as sub_agent_start/sub_agent_complete.
const (
	kiroSubagentParentRawID = "sess_0123456789abcdef"
	kiroSubagentParentID    = "kiro:" + kiroSubagentParentRawID
	kiroSubagentChildRawID  = "11111111-2222-3333-4444-555555555555"
	kiroSubagentChildID     = "kiro:" + kiroSubagentChildRawID
	kiroSubagentLaterRawID  = "66666666-7777-8888-9999-000000000000"
	kiroSubagentLaterID     = "kiro:" + kiroSubagentLaterRawID
)

func writeKiroLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(lines...)), 0o644))
}

// appendKiroLine appends one record to a transcript, as kiro-cli does while a
// session runs.
func appendKiroLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err, "open %s for append", path)
	defer func() { assert.NoError(t, f.Close(), "close %s", path) }()
	_, err = f.WriteString(line + "\n")
	require.NoError(t, err, "append to %s", path)
}

// writeKiroSessionWithSubagent lays out one parent session that spawned one
// subagent and returns the session directory.
func writeKiroSessionWithSubagent(t *testing.T, kiroDir string) string {
	t.Helper()
	sessDir := filepath.Join(kiroDir, "workspace", kiroSubagentParentRawID)
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "session.json"),
		[]byte(`{"title":"Parent session","workspacePaths":["/home/user/code/kiro-app"]}`),
		0o644,
	))
	writeKiroLines(t, filepath.Join(sessDir, "messages.jsonl"),
		testjsonl.KiroUserJSON("2026-06-01T10:00:00Z", "parent prompt"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test01-sub-agent-start", "2026-06-01T10:00:01Z",
			kiroSubagentChildRawID, "coder", "do the subtask"),
		testjsonl.KiroSubAgentCompleteJSON("invoke_subagent_toolu_test01-sub-agent-complete", "2026-06-01T10:00:09Z",
			kiroSubagentChildRawID, map[string]any{"status": "success", "response": "subtask done"}),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:10Z", "parent reply"),
	)
	writeKiroLines(t, filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl"),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:02Z", "SUBAGENT_MARKER working"),
		testjsonl.KiroToolCallJSON("2026-06-01T10:00:03Z", "t1", "execute_bash", map[string]any{"command": "ls"}),
		testjsonl.KiroToolResultJSON("2026-06-01T10:00:04Z", "t1", "ok"),
	)
	return sessDir
}

// spawnKiroLaterSubagent records a second spawn in the parent transcript and
// writes that subagent's transcript, returning its path.
func spawnKiroLaterSubagent(t *testing.T, sessDir string) string {
	t.Helper()
	appendKiroLine(t, filepath.Join(sessDir, "messages.jsonl"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test02-sub-agent-start", "2026-06-01T10:01:00Z",
			kiroSubagentLaterRawID, "reviewer", "review it"))
	laterChild := filepath.Join(sessDir, "sub-executions", kiroSubagentLaterRawID+".jsonl")
	writeKiroLines(t, laterChild, testjsonl.KiroAssistantJSON("2026-06-01T10:01:01Z", "reviewing"))
	return laterChild
}

// kiroSpawnLinks maps each Task tool call in a session to the subagent
// session it links to.
func kiroSpawnLinks(t *testing.T, database *db.DB, sessionID string) map[string]string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	links := map[string]string{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.Category == "Task" {
				links[call.ToolName+":"+call.ToolUseID] = call.SubagentSessionID
			}
		}
	}
	return links
}

func TestKiroSubagentTranscriptSyncsFromChangedPath(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	// kiro-cli spawns a second subagent after the first sync: the parent
	// gains a sub_agent_start and a new sub-executions file appears.
	laterChild := spawnKiroLaterSubagent(t, sessDir)
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))

	childIDs := func() []string {
		t.Helper()
		children, err := env.db.GetChildSessions(t.Context(), kiroSubagentParentID)
		require.NoError(t, err)
		ids := make([]string, len(children))
		for i, child := range children {
			assert.Nil(t, child.SourceMissingAt, "child %s should not be marked source-missing", child.ID)
			ids[i] = child.ID
		}
		return ids
	}
	want := []string{kiroSubagentChildID, kiroSubagentLaterID}
	assert.ElementsMatch(t, want, childIDs(), "a watcher event for a new subagent transcript should sync it")

	stats = env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	assert.ElementsMatch(t, want, childIDs(), "a later full sync keeps every child")

	// Removing a subagent transcript keeps its archived session and marks it
	// as missing its source.
	require.NoError(t, os.Remove(laterChild))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))
	removed, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
	require.NoError(t, err)
	require.NotNil(t, removed, "a removed subagent transcript must stay archived")
	assert.NotNil(t, removed.SourceMissingAt, "a removed subagent transcript should be marked source-missing")
	kept, err := env.db.GetSessionFull(t.Context(), kiroSubagentChildID)
	require.NoError(t, err)
	require.NotNil(t, kept)
	assert.Nil(t, kept.SourceMissingAt, "subagents whose transcripts remain stay current")
}

// writeKiroDecoySession writes an unrelated session with one subagent and
// returns that subagent's session ID.
func writeKiroDecoySession(t *testing.T, kiroDir string, i int) string {
	t.Helper()
	rawID := fmt.Sprintf("sess_%016x", i)
	childRawID := fmt.Sprintf("%08x-0000-4000-8000-%012x", i, i)
	sessDir := filepath.Join(kiroDir, "workspace", rawID)
	writeKiroLines(t, filepath.Join(sessDir, "messages.jsonl"),
		testjsonl.KiroUserJSON("2026-06-01T09:00:00Z", "decoy prompt"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_decoy-sub-agent-start", "2026-06-01T09:00:01Z",
			childRawID, "coder", "decoy subtask"),
	)
	writeKiroLines(t, filepath.Join(sessDir, "sub-executions", childRawID+".jsonl"),
		testjsonl.KiroAssistantJSON("2026-06-01T09:00:02Z", "decoy work"),
	)
	return "kiro:" + childRawID
}

// kiroSubagentEventWork is what one archive did for an idle full sync and for
// the events that add and then remove one subagent transcript.
type kiroSubagentEventWork struct {
	idleSynced    int
	addTotal      int
	addSynced     int
	deleteTotal   int
	deleteSynced  int
	laterCaptured bool
	laterMissing  bool
	// affectedDecoys lists unrelated subagent sessions that the events
	// removed from the archive or marked source-missing.
	affectedDecoys []string
}

// measureKiroSubagentEventWork syncs an archive of one session with
// subagents plus decoys unrelated sessions, then adds and removes a subagent
// transcript through changed-path events.
func measureKiroSubagentEventWork(t *testing.T, decoys int) kiroSubagentEventWork {
	t.Helper()
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	decoyChildren := make([]string, decoys)
	for i := range decoys {
		decoyChildren[i] = writeKiroDecoySession(t, env.kiroDir, i)
	}
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted, "initial sync with %d decoys", decoys)
	idle := env.engine.SyncAll(t.Context(), nil)
	require.False(t, idle.Aborted, "idle sync with %d decoys", decoys)

	laterChild := spawnKiroLaterSubagent(t, sessDir)
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))
	addStats := env.engine.LastSyncStats()
	captured, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
	require.NoError(t, err)

	require.NoError(t, os.Remove(laterChild))
	require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{laterChild}))
	deleteStats := env.engine.LastSyncStats()
	removed, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
	require.NoError(t, err)
	require.NotNil(t, removed, "a removed subagent transcript must stay archived (%d decoys)", decoys)

	var affected []string
	for _, id := range decoyChildren {
		decoy, err := env.db.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		if decoy == nil || decoy.SourceMissingAt != nil {
			affected = append(affected, id)
		}
	}
	return kiroSubagentEventWork{
		idleSynced:     idle.Synced,
		addTotal:       addStats.TotalSessions,
		addSynced:      addStats.Synced,
		deleteTotal:    deleteStats.TotalSessions,
		deleteSynced:   deleteStats.Synced,
		laterCaptured:  captured != nil,
		laterMissing:   removed.SourceMissingAt != nil,
		affectedDecoys: affected,
	}
}

// A subagent event touches only its own session: an archive of unrelated
// sessions with subagents must not change how much work the event does,
// for new transcripts or for deleted ones.
func TestKiroSubagentChangedPathWorkIsCardinalityBounded(t *testing.T) {
	small := measureKiroSubagentEventWork(t, 3)
	assert.Zero(t, small.idleSynced, "an unchanged full sync should not rewrite sessions or subagents")
	assert.True(t, small.laterCaptured, "a watcher event for a new subagent transcript should sync it")
	assert.True(t, small.laterMissing, "a removed subagent transcript should be marked source-missing")
	assert.Empty(t, small.affectedDecoys, "unrelated sessions and subagents stay archived and current")

	large := measureKiroSubagentEventWork(t, 30)
	assert.Equal(t, small, large,
		"subagent changed-path work, deletion, and idle syncs must be the same with 3 and 30 unrelated sessions")
}

func TestKiroSubagentTranscriptsSyncAsChildSessions(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	writeKiroSessionWithSubagent(t, env.kiroDir)

	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)

	parent, err := env.db.GetSessionFull(t.Context(), kiroSubagentParentID)
	require.NoError(t, err)
	require.NotNil(t, parent, "parent session must sync")

	children, err := env.db.GetChildSessions(t.Context(), kiroSubagentParentID)
	require.NoError(t, err)
	require.Len(t, children, 1, "the subagent transcript should sync as a child session")
	child := children[0]
	assert.Equal(t, kiroSubagentChildID, child.ID)
	assert.Equal(t, "subagent", child.RelationshipType)
	assert.Equal(t, parent.Project, child.Project)

	childMessages, err := env.db.GetAllMessages(t.Context(), kiroSubagentChildID)
	require.NoError(t, err)
	var childText []string
	for _, message := range childMessages {
		childText = append(childText, message.Content)
	}
	assert.Contains(t, strings.Join(childText, "\n"), "SUBAGENT_MARKER")

	assert.Equal(t,
		map[string]string{"invoke_subagent:" + kiroSubagentChildRawID: kiroSubagentChildID},
		kiroSpawnLinks(t, env.db, kiroSubagentParentID),
		"the parent transcript should record the spawn as a Task call linked to the child",
	)
}

// kiroSessionText joins the content of a session's stored messages.
func kiroSessionText(t *testing.T, database *db.DB, sessionID string) string {
	t.Helper()
	messages, err := database.GetAllMessages(t.Context(), sessionID)
	require.NoError(t, err)
	text := make([]string, len(messages))
	for i, message := range messages {
		text[i] = message.Content
	}
	return strings.Join(text, "\n")
}

// Each subagent transcript is its own source, so an unreadable one fails
// alone: its session and the other subagents keep syncing, and it syncs once
// it can be read.
func TestKiroUnreadableSubagentFailsOnlyItsOwnSource(t *testing.T) {
	breaks := []struct {
		name       string
		breakChild func(t *testing.T, path string) (restore func())
	}{
		{
			name: "permission denied",
			breakChild: func(t *testing.T, path string) func() {
				t.Helper()
				require.NoError(t, os.Chmod(path, 0))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(path, 0o644)) })
				if _, err := os.ReadFile(path); err == nil {
					t.Skip("file permissions do not block reads for this user")
				}
				return func() { require.NoError(t, os.Chmod(path, 0o644)) }
			},
		},
		{
			name: "symlink loop",
			breakChild: func(t *testing.T, path string) func() {
				t.Helper()
				content, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.Remove(path))
				if err := os.Symlink(path, path); err != nil {
					t.Skipf("symlink creation unavailable: %v", err)
				}
				return func() {
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.WriteFile(path, content, 0o644))
				}
			},
		},
	}
	for _, br := range breaks {
		t.Run(br.name, func(t *testing.T) {
			env := setupSingleAgentTestEnv(t, parser.AgentKiro)
			sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
			laterChild := spawnKiroLaterSubagent(t, sessDir)
			stats := env.engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			require.Zero(t, stats.Failed)

			appendKiroLine(t, filepath.Join(sessDir, "messages.jsonl"),
				testjsonl.KiroAssistantJSON("2026-06-01T10:02:00Z", "PARENT_UPDATE"))
			sibling := filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl")
			appendKiroLine(t, sibling,
				testjsonl.KiroAssistantJSON("2026-06-01T10:02:01Z", "SIBLING_UPDATE"))
			restore := br.breakChild(t, laterChild)

			stats = env.engine.SyncAll(t.Context(), nil)
			require.False(t, stats.Aborted)
			assert.Equal(t, 1, stats.Failed, "only the unreadable subagent fails")
			assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentParentID), "PARENT_UPDATE",
				"the session syncs past its unreadable subagent")
			assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentChildID), "SIBLING_UPDATE",
				"a readable subagent syncs past its unreadable sibling")
			later, err := env.db.GetSessionFull(t.Context(), kiroSubagentLaterID)
			require.NoError(t, err)
			require.NotNil(t, later, "the unreadable subagent stays archived")
			assert.Nil(t, later.SourceMissingAt, "an unreadable subagent transcript is not missing")

			restore()
			appendKiroLine(t, laterChild,
				testjsonl.KiroAssistantJSON("2026-06-01T10:02:02Z", "LATER_UPDATE"))
			retry := env.engine.SyncAll(t.Context(), nil)
			require.False(t, retry.Aborted)
			assert.Zero(t, retry.Failed)
			assert.Contains(t, kiroSessionText(t, env.db, kiroSubagentLaterID), "LATER_UPDATE",
				"the subagent syncs once it can be read")
		})
	}
}

// kiro-cli writes every subagent transcript flat in the session's
// sub-executions directory, so the parser links each subagent to the
// session. The subagent linker then moves a nested subagent onto the
// subagent whose Task call spawned it.
func TestKiroNestedSubagentLinksToItsSpawner(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	appendKiroLine(t, filepath.Join(sessDir, "sub-executions", kiroSubagentChildRawID+".jsonl"),
		testjsonl.KiroSubAgentStartJSON("invoke_subagent_toolu_test03-sub-agent-start", "2026-06-01T10:00:05Z",
			kiroSubagentLaterRawID, "reviewer", "review it"))
	writeKiroLines(t, filepath.Join(sessDir, "sub-executions", kiroSubagentLaterRawID+".jsonl"),
		testjsonl.KiroAssistantJSON("2026-06-01T10:00:06Z", "reviewing"))

	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	require.Zero(t, stats.Failed)

	for id, wantParent := range map[string]string{
		kiroSubagentChildID: kiroSubagentParentID,
		kiroSubagentLaterID: kiroSubagentChildID,
	} {
		session, err := env.db.GetSessionFull(t.Context(), id)
		require.NoError(t, err)
		require.NotNil(t, session, "subagent %s must sync", id)
		require.NotNil(t, session.ParentSessionID, "subagent %s must have a parent", id)
		assert.Equal(t, wantParent, *session.ParentSessionID, "parent of %s", id)
		assert.Equal(t, "subagent", session.RelationshipType, "relationship of %s", id)
	}
}

// A delegation's completion status reaches tool health: a failed spawn counts
// as a tool failure of the spawning session and a successful one does not.
func TestKiroSubagentDelegationStatusFeedsToolHealth(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentKiro)
	sessDir := writeKiroSessionWithSubagent(t, env.kiroDir)
	spawnKiroLaterSubagent(t, sessDir)
	appendKiroLine(t, filepath.Join(sessDir, "messages.jsonl"),
		testjsonl.KiroSubAgentCompleteJSON("invoke_subagent_toolu_test02-sub-agent-complete", "2026-06-01T10:01:30Z",
			kiroSubagentLaterRawID, map[string]any{"status": "error", "errorMessage": "reviewer failed"}))
	stats := env.engine.SyncAll(t.Context(), nil)
	require.False(t, stats.Aborted)
	require.Zero(t, stats.Failed)

	messages, err := env.db.GetAllMessages(t.Context(), kiroSubagentParentID)
	require.NoError(t, err)
	statuses := map[string]string{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			for _, event := range call.ResultEvents {
				statuses[call.ToolUseID] = event.Status
			}
		}
	}
	assert.Equal(t, map[string]string{
		kiroSubagentChildRawID: "completed",
		kiroSubagentLaterRawID: "errored",
	}, statuses, "each delegation result records its completion status")
	parent, err := env.db.GetSessionFull(t.Context(), kiroSubagentParentID)
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, 1, parent.ToolFailureSignalCount, "only the failed delegation is a tool failure")
}

// Data version 151 reparses Kiro sources in place, so a session archived
// before spawns were parsed gains its Task calls without changing identity.
func TestKiroSubagentUpgradeRecordsSpawnsInPlace(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	writeKiroSessionWithSubagent(t, root)
	archivePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.OpenIsolated(ctx, archivePath)
	require.NoError(t, err)
	cfg := sync.EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentKiro: {root}}, Machine: "local"}
	engine := sync.NewEngine(ctx, database, cfg)
	require.Zero(t, engine.SyncAll(ctx, nil).Failed)
	require.NotEmpty(t, kiroSpawnLinks(t, database, kiroSubagentParentID))
	engine.Close()
	require.NoError(t, database.Close())

	// Model version 150, which dropped spawn records, without changing the
	// source fingerprint.
	previousVersion := 150
	raw, err := sql.Open("sqlite3", archivePath)
	require.NoError(t, err)
	_, err = raw.ExecContext(ctx, fmt.Sprintf(`
		DELETE FROM tool_calls WHERE session_id = '%[2]s';
		UPDATE sessions SET data_version = %[1]d; PRAGMA user_version = %[1]d`,
		previousVersion, kiroSubagentParentID))
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	reopened, err := db.OpenIsolated(ctx, archivePath)
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()
	assert.False(t, reopened.NeedsResync(), "the archive upgrades in place")
	require.Empty(t, kiroSpawnLinks(t, reopened, kiroSubagentParentID))
	upgraded := sync.NewEngine(ctx, reopened, cfg)
	defer upgraded.Close()
	stats := upgraded.SyncAll(ctx, nil)
	require.Zero(t, stats.Failed)
	assert.Positive(t, stats.Synced)

	assert.Equal(t,
		map[string]string{"invoke_subagent:" + kiroSubagentChildRawID: kiroSubagentChildID},
		kiroSpawnLinks(t, reopened, kiroSubagentParentID),
		"the reparse restores the spawn as a Task call linked to the subagent")
	parent, err := reopened.GetSessionFull(ctx, kiroSubagentParentID)
	require.NoError(t, err)
	require.NotNil(t, parent)
	assert.Equal(t, db.CurrentDataVersion(), parent.DataVersion)
}
