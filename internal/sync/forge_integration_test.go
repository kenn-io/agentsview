package sync_test

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

type forgeTestDB struct {
	path string
	db   *sql.DB
}

func createForgeDB(t *testing.T, dir string) *forgeTestDB {
	t.Helper()
	path := filepath.Join(dir, ".forge.db")
	d, err := sql.Open("sqlite3", path)
	require.NoError(t, err, "opening forge test db")
	fixture := &forgeTestDB{path: path, db: d}
	t.Cleanup(func() {
		if fixture.db != nil {
			_ = fixture.db.Close()
		}
	})

	schema := `
		CREATE TABLE conversations (
			conversation_id TEXT PRIMARY KEY NOT NULL,
			title TEXT,
			workspace_id BIGINT NOT NULL,
			context TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP,
			metrics TEXT
		);
	`
	_, err = d.Exec(schema)
	require.NoError(t, err, "creating forge schema")
	return fixture
}

func (f *forgeTestDB) close(t *testing.T) {
	t.Helper()
	require.NoError(t, f.db.Close())
	f.db = nil
}

func (f *forgeTestDB) mustExec(t *testing.T, msg, query string, args ...any) {
	t.Helper()
	_, err := f.db.Exec(query, args...)
	require.NoError(t, err, msg)
}

func (f *forgeTestDB) addConversation(
	t *testing.T,
	conversationID, title, context, createdAt, updatedAt, metrics string,
) {
	t.Helper()
	f.mustExec(t, "insert conversation",
		`INSERT INTO conversations
			(conversation_id, title, workspace_id, context, created_at, updated_at, metrics)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		conversationID, title, int64(1), context, createdAt, updatedAt, metrics,
	)
}

func forgeTestContext(userPrompt, finalAnswer string) string {
	messages := []map[string]any{
		{
			"message": map[string]any{
				"text": map[string]any{
					"role":      "System",
					"content":   "<system_information>\n<current_working_directory>/home/mj/dev/projects/agentsview</current_working_directory>\n</system_information>",
					"model":     "gpt-5.4",
					"timestamp": "2026-05-02T09:58:15.741021507Z",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     map[string]any{"actual": 0},
				"completion_tokens": map[string]any{"actual": 0},
				"cached_tokens":     map[string]any{"actual": 0},
			},
		},
		{
			"message": map[string]any{
				"text": map[string]any{
					"role":        "User",
					"content":     userPrompt,
					"raw_content": map[string]any{"Text": userPrompt},
					"model":       "gpt-5.4",
					"timestamp":   "2026-05-02T09:58:16.000000000Z",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     map[string]any{"actual": 100},
				"completion_tokens": map[string]any{"actual": 5},
				"cached_tokens":     map[string]any{"actual": 20},
			},
		},
		{
			"message": map[string]any{
				"text": map[string]any{
					"role":    "Assistant",
					"content": "",
					"tool_calls": []map[string]any{{
						"name":      "read",
						"call_id":   "call_read_1",
						"arguments": map[string]any{"file_path": "/tmp/example.go", "show_line_numbers": true},
					}},
					"model": "gpt-5.4",
					"reasoning_details": []map[string]any{{
						"text": "Inspecting the code first.",
					}},
					"timestamp": "2026-05-02T09:58:17.000000000Z",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     map[string]any{"actual": 120},
				"completion_tokens": map[string]any{"actual": 10},
				"cached_tokens":     map[string]any{"actual": 30},
			},
		},
		{
			"message": map[string]any{
				"tool": map[string]any{
					"name":    "read",
					"call_id": "call_read_1",
					"output": map[string]any{
						"is_error": false,
						"values": []map[string]any{{
							"text": "<file path=\"/tmp/example.go\">package main</file>",
						}},
					},
				},
			},
		},
		{
			"message": map[string]any{
				"text": map[string]any{
					"role":        "Assistant",
					"content":     finalAnswer,
					"raw_content": map[string]any{"Text": finalAnswer},
					"model":       "gpt-5.4",
					"timestamp":   "2026-05-02T09:58:18.000000000Z",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     map[string]any{"actual": 140},
				"completion_tokens": map[string]any{"actual": 40},
				"cached_tokens":     map[string]any{"actual": 35},
			},
		},
	}
	root := map[string]any{
		"conversation_id": "forge-sync-1",
		"messages":        messages,
	}
	raw, _ := json.Marshal(root)
	return string(raw)
}

func TestSyncEngineForgeBulkSync(t *testing.T) {
	env := setupTestEnv(t)
	forge := createForgeDB(t, env.forgeDir)
	forge.addConversation(
		t,
		"forge-sync-1",
		"Forge Bulk Sync",
		forgeTestContext("Please add Forge support.", "Added Forge support."),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		`{"input_tokens":360,"output_tokens":55,"cached_input_tokens":85}`,
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})
	assertSessionProject(t, env.db, "forge:forge-sync-1", "agentsview")
	assertSessionMessageCount(t, env.db, "forge:forge-sync-1", 3)
	assertMessageRoles(t, env.db, "forge:forge-sync-1", "user", "assistant", "assistant")
	assertToolCallCount(t, env.db, "forge:forge-sync-1", 1)
	assertMessageContent(
		t, env.db, "forge:forge-sync-1",
		"Please add Forge support.",
		"[Thinking]\nInspecting the code first.\n[/Thinking]",
		"Added Forge support.",
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0, Synced: 0, Skipped: 0})
}

func TestReconcileWatchRootsGenericDBBackedProviderPreservesArchive(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentForge)
	forge := createForgeDB(t, env.forgeDir)
	forge.addConversation(
		t,
		"persistent-archive",
		"Persistent Forge Archive",
		forgeTestContext("Preserve this prompt.", "Preserved."),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		`{"input_tokens":360,"output_tokens":55,"cached_input_tokens":85}`,
	)
	forge.close(t)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)
	require.NoError(t, os.Remove(forge.path))

	require.NoError(t, env.engine.ReconcileWatchRoots(
		t.Context(), []string{env.forgeDir}, false,
	))

	sess, err := env.db.GetSession(t.Context(), "forge:persistent-archive")
	require.NoError(t, err)
	assert.NotNil(t, sess,
		"the generic DB-backed provider must preserve members after its SQLite archive vanishes")
}

func TestReconcileWatchRootsForgeDeletedMemberTombstonesSession(t *testing.T) {
	env := setupSingleAgentTestEnv(t, parser.AgentForge)
	forge := createForgeDB(t, env.forgeDir)
	for _, id := range []string{"deleted-member", "surviving-member"} {
		forge.addConversation(
			t, id, id,
			forgeTestContext("Prompt for "+id, "Answer for "+id),
			"2026-05-02 09:58:15.741021507",
			"2026-05-02 10:00:16.848497543",
			`{"input_tokens":1,"output_tokens":1,"cached_input_tokens":0}`,
		)
	}
	require.Equal(t, 2, env.engine.SyncAll(t.Context(), nil).Synced)
	beforeDelete, err := env.db.GetSessionFull(t.Context(), "forge:deleted-member")
	require.NoError(t, err)
	require.NotNil(t, beforeDelete)
	forge.mustExec(t, "delete Forge conversation",
		"DELETE FROM conversations WHERE conversation_id = ?", "deleted-member")

	require.NoError(t, env.engine.ReconcileWatchRoots(
		t.Context(), []string{env.forgeDir}, false,
	))

	deleted, err := env.db.GetSession(t.Context(), "forge:deleted-member")
	require.NoError(t, err)
	assert.Nil(t, deleted,
		"full sync must baseline dedicated streaming sources for later reconciliation")
	archived, err := env.db.GetSessionFull(t.Context(), "forge:deleted-member")
	require.NoError(t, err)
	require.NotNil(t, archived)
	require.NotNil(t, archived.DeletedAt)
	require.NotNil(t, archived.DeletionCause)
	assert.Equal(t, "source_missing", *archived.DeletionCause)
	assert.Equal(t, beforeDelete.MessageCount, archived.MessageCount,
		"source loss must retain the archived transcript")
	surviving, err := env.db.GetSession(t.Context(), "forge:surviving-member")
	require.NoError(t, err)
	assert.NotNil(t, surviving)
}

func TestReconcileWatchRootsForgeScopedPassPreservesUnscannedReplacement(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	env := setupSingleAgentTestEnvWithDirs(
		t, parser.AgentForge, []string{rootA, rootB},
	)
	forgeA := createForgeDB(t, rootA)
	forgeA.addConversation(
		t, "moved-member", "Moved member",
		forgeTestContext("original prompt", "original answer"),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		`{"input_tokens":1,"output_tokens":1,"cached_input_tokens":0}`,
	)
	require.Equal(t, 1, env.engine.SyncAll(t.Context(), nil).Synced)

	forgeB := createForgeDB(t, rootB)
	forgeB.addConversation(
		t, "moved-member", "Moved member",
		forgeTestContext("replacement prompt", "replacement answer"),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:01:16.848497543",
		`{"input_tokens":2,"output_tokens":2,"cached_input_tokens":0}`,
	)
	forgeA.mustExec(t, "remove original Forge conversation",
		"DELETE FROM conversations WHERE conversation_id = ?", "moved-member")

	require.NoError(t, env.engine.ReconcileWatchRoots(
		t.Context(), []string{rootA}, false,
	))

	active, err := env.db.GetSession(t.Context(), "forge:moved-member")
	require.NoError(t, err)
	assert.NotNil(t, active,
		"a scoped pass cannot prove absence below an unscanned provider root")
}

func TestSyncSingleSessionForge(t *testing.T) {
	env := setupTestEnv(t)
	forge := createForgeDB(t, env.forgeDir)
	forge.addConversation(
		t,
		"forge-sync-single",
		"Forge Single Sync",
		forgeTestContext("Please add single-sync support.", "Single sync complete."),
		"2026-05-02 09:58:15.741021507",
		"2026-05-02 10:00:16.848497543",
		`{"input_tokens":360,"output_tokens":55,"cached_input_tokens":85}`,
	)

	require.NoError(t, env.engine.SyncSingleSession("forge:forge-sync-single"), "SyncSingleSession")
	assertSessionProject(t, env.db, "forge:forge-sync-single", "agentsview")
	assertSessionMessageCount(t, env.db, "forge:forge-sync-single", 3)

	src := env.engine.FindSourceFile("forge:forge-sync-single")
	// Forge is a provider-authoritative DB-backed provider: FindSourceFile
	// resolves the per-session virtual <db>#<conversationID> path the
	// provider parses, matching the stored session file_path.
	wantSrc := filepath.Join(env.forgeDir, ".forge.db") + "#forge-sync-single"
	assert.Equal(t, wantSrc, src)

	mtime := env.engine.SourceMtime("forge:forge-sync-single")
	require.NotZero(t, mtime, "SourceMtime returned zero")

	_, storedMtime, ok := env.db.GetSessionFileInfo("forge:forge-sync-single")
	require.True(t, ok, "session file info not found")
	assert.Equal(t, mtime, storedMtime)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 0, Synced: 0, Skipped: 0})
}

// ---------------------------------------------------------------------------
// Priority 1 — Multi-conversation incremental sync
// ---------------------------------------------------------------------------

func TestSyncForgeMultiConversationIncremental(t *testing.T) {
	env := setupTestEnv(t)
	forge := createForgeDB(t, env.forgeDir)

	// Seed two conversations.
	forge.addConversation(
		t,
		"multi-conv-A", "Conversation A",
		forgeTestContext("First prompt A.", "Answer A."),
		"2026-05-02 09:00:00", "2026-05-02 09:01:00",
		`{"input_tokens":100,"output_tokens":20}`,
	)
	forge.addConversation(
		t,
		"multi-conv-B", "Conversation B",
		forgeTestContext("First prompt B.", "Answer B."),
		"2026-05-02 09:00:00", "2026-05-02 09:02:00",
		`{"input_tokens":120,"output_tokens":25}`,
	)

	// Full sync: both must be written.
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2, Skipped: 0})
	assertSessionProject(t, env.db, "forge:multi-conv-A", "agentsview")
	assertSessionProject(t, env.db, "forge:multi-conv-B", "agentsview")

	// Capture the stored mtime for A (should remain unchanged after the partial sync).
	_, storedMtimeA, okA := env.db.GetSessionFileInfo("forge:multi-conv-A")
	require.True(t, okA, "session A file info not found after initial sync")

	// Update only B's updated_at (and its context) to simulate a newer version.
	updatedContextB := forgeTestContext("First prompt B updated.", "Answer B updated.")
	forge.mustExec(t, "update B updated_at",
		`UPDATE conversations SET updated_at = '2026-05-02 09:03:00', context = ? WHERE conversation_id = 'multi-conv-B'`,
		updatedContextB,
	)

	// Partial sync: only B changed.
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})

	// A's stored mtime must be unchanged.
	_, storedMtimeA2, okA2 := env.db.GetSessionFileInfo("forge:multi-conv-A")
	require.True(t, okA2, "session A file info not found after partial sync")
	assert.Equal(t, storedMtimeA, storedMtimeA2, "A's stored mtime changed")

	// B's stored mtime must have advanced.
	_, storedMtimeB2, okB2 := env.db.GetSessionFileInfo("forge:multi-conv-B")
	require.True(t, okB2, "session B file info not found after partial sync")
	assert.Greater(t, storedMtimeB2, storedMtimeA, "B's stored mtime did not advance")
}

// ---------------------------------------------------------------------------
// Priority 3 — FindSourceFile / SourceMtime when conversation disappears
// ---------------------------------------------------------------------------

func TestSyncForgeMissingConversation(t *testing.T) {
	env := setupTestEnv(t)
	forge := createForgeDB(t, env.forgeDir)

	forge.addConversation(
		t,
		"disappearing-conv", "Disappearing",
		forgeTestContext("I will vanish.", "Gone soon."),
		"2026-05-02 09:00:00", "2026-05-02 09:01:00",
		`{"input_tokens":50,"output_tokens":10}`,
	)

	require.NoError(t, env.engine.SyncSingleSession("forge:disappearing-conv"), "SyncSingleSession")

	// Now delete the conversation from .forge.db.
	forge.mustExec(t, "delete conversation",
		`DELETE FROM conversations WHERE conversation_id = 'disappearing-conv'`,
	)

	// FindSourceFile still returns the db path (it's a directory-level lookup).
	// SourceMtime returns 0 because the conversation row is gone.
	mtime := env.engine.SourceMtime("forge:disappearing-conv")
	assert.Zero(t, mtime, "SourceMtime after delete")

	src := env.engine.FindSourceFile("forge:disappearing-conv")
	assert.Empty(t, src, "FindSourceFile after delete")

	// SyncSingleSession must return an error indicating the conversation
	// could not be found (either "not found" or a db no-rows error).
	err := env.engine.SyncSingleSession("forge:disappearing-conv")
	require.Error(t, err, "expected error from SyncSingleSession for deleted conversation")
	msg := strings.ToLower(err.Error())
	assert.True(t, strings.Contains(msg, "not found") || strings.Contains(msg, "no rows"),
		"expected 'not found' or 'no rows' error, got: %v", err)
}

func TestSyncForgeSyncSingleNonExistent(t *testing.T) {
	env := setupTestEnv(t)
	// Create an empty .forge.db so the provider discovers the DB.
	createForgeDB(t, env.forgeDir)

	err := env.engine.SyncSingleSession("forge:does-not-exist")
	require.Error(t, err, "expected error for non-existent session")
	msg := strings.ToLower(err.Error())
	assert.True(t, strings.Contains(msg, "not found") || strings.Contains(msg, "no rows"),
		"expected 'not found' or 'no rows' error, got: %v", err)
}

// ---------------------------------------------------------------------------
// Priority 4 — Subagent linking end-to-end
// ---------------------------------------------------------------------------

func forgeParentContext(childConvID string) string {
	messages := []map[string]any{
		{
			"message": map[string]any{
				"text": map[string]any{
					"role":      "User",
					"content":   "Run a sub-task.",
					"timestamp": "2026-05-02T10:00:00Z",
				},
			},
		},
		{
			"message": map[string]any{
				"text": map[string]any{
					"role": "Assistant",
					"tool_calls": []map[string]any{{
						"name":    "task",
						"call_id": "call_task_parent",
						"arguments": map[string]any{
							"session_id": childConvID,
							"prompt":     "do the child work",
						},
					}},
					"timestamp": "2026-05-02T10:00:01Z",
				},
			},
		},
	}
	raw, _ := json.Marshal(map[string]any{"messages": messages})
	return string(raw)
}

func TestSyncForgeSubagentLinking(t *testing.T) {
	env := setupTestEnv(t)
	forge := createForgeDB(t, env.forgeDir)

	childID := "child-conv"
	parentID := "parent-conv"

	forge.addConversation(
		t,
		parentID, "Parent",
		forgeParentContext(childID),
		"2026-05-02 09:00:00", "2026-05-02 09:01:00",
		"",
	)
	forge.addConversation(
		t,
		childID, "Child",
		forgeTestContext("Child work.", "Child done."),
		"2026-05-02 09:00:30", "2026-05-02 09:01:30",
		"",
	)

	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 2, Synced: 2, Skipped: 0})

	// After SyncAll the tool_call row must already carry subagent_session_id.
	// This is set by the parser before LinkSubagentSessions runs.
	var subagentSessID sql.NullString
	err := env.db.Reader().QueryRow(
		`SELECT subagent_session_id FROM tool_calls WHERE session_id = ? AND tool_name = 'task'`,
		"forge:"+parentID,
	).Scan(&subagentSessID)
	require.NoError(t, err, "query tool_calls for parent")
	require.True(t, subagentSessID.Valid, "tool_calls.subagent_session_id not valid")
	assert.Equal(t, "forge:"+childID, subagentSessID.String)

	// SyncAll must now call LinkSubagentSessions after the Forge write,
	// so parent_session_id and relationship_type on the child must already
	// be set without any SyncSingleSession workaround.
	var parentSessID sql.NullString
	var relType sql.NullString
	err = env.db.Reader().QueryRow(
		`SELECT parent_session_id, relationship_type FROM sessions WHERE id = ?`,
		"forge:"+childID,
	).Scan(&parentSessID, &relType)
	require.NoError(t, err, "query child session")
	require.True(t, parentSessID.Valid, "child parent_session_id not valid")
	assert.Equal(t, "forge:"+parentID, parentSessID.String)
	require.True(t, relType.Valid, "child relationship_type not valid")
	assert.Equal(t, "subagent", relType.String)
}

// ---------------------------------------------------------------------------
// Priority 6 — Failure isolation: malformed JSON in one conversation
// ---------------------------------------------------------------------------

func TestSyncForgeFailureIsolation(t *testing.T) {
	env := setupTestEnv(t)
	forge := createForgeDB(t, env.forgeDir)

	// Valid conversation.
	forge.addConversation(
		t,
		"valid-conv", "Valid",
		forgeTestContext("Valid prompt.", "Valid answer."),
		"2026-05-02 09:00:00", "2026-05-02 09:01:00",
		`{"input_tokens":50,"output_tokens":10}`,
	)

	// Malformed JSON context — buildForgeSession will return nil, nil, nil
	// (gjson.Parse(...).Get("messages").IsArray() == false).
	forge.addConversation(
		t,
		"broken-conv", "Broken",
		"{not valid json",
		"2026-05-02 09:00:00", "2026-05-02 09:02:00",
		"",
	)

	// Should not panic; valid session must be written.
	runSyncAndAssert(t, env.engine, sync.SyncStats{TotalSessions: 1, Synced: 1, Skipped: 0})
	assertSessionProject(t, env.db, "forge:valid-conv", "agentsview")

	// Broken session must not be in the DB.
	var count int
	err := env.db.Reader().QueryRow(
		"SELECT COUNT(*) FROM sessions WHERE id = 'forge:broken-conv'",
	).Scan(&count)
	require.NoError(t, err, "query broken session")
	assert.Zero(t, count, "broken session present in DB")
}
