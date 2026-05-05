package sync_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/wesm/agentsview/internal/sync"
)

type forgeTestDB struct {
	path string
	db   *sql.DB
}

func createForgeDB(t *testing.T, dir string) *forgeTestDB {
	t.Helper()
	path := filepath.Join(dir, ".forge.db")
	d, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("opening forge test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

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
	if _, err := d.Exec(schema); err != nil {
		t.Fatalf("creating forge schema: %v", err)
	}
	return &forgeTestDB{path: path, db: d}
}

func (f *forgeTestDB) mustExec(t *testing.T, msg, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
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

	if err := env.engine.SyncSingleSession("forge:forge-sync-single"); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}
	assertSessionProject(t, env.db, "forge:forge-sync-single", "agentsview")
	assertSessionMessageCount(t, env.db, "forge:forge-sync-single", 3)

	src := env.engine.FindSourceFile("forge:forge-sync-single")
	wantSrc := filepath.Join(env.forgeDir, ".forge.db")
	if src != wantSrc {
		t.Fatalf("FindSourceFile() = %q, want %q", src, wantSrc)
	}

	mtime := env.engine.SourceMtime("forge:forge-sync-single")
	if mtime == 0 {
		t.Fatal("SourceMtime returned zero")
	}

	_, storedMtime, ok := env.db.GetSessionFileInfo("forge:forge-sync-single")
	if !ok {
		t.Fatal("session file info not found")
	}
	if storedMtime != mtime {
		t.Fatalf("stored mtime = %d, want %d", storedMtime, mtime)
	}

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
	if !okA {
		t.Fatal("session A file info not found after initial sync")
	}

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
	if !okA2 {
		t.Fatal("session A file info not found after partial sync")
	}
	if storedMtimeA != storedMtimeA2 {
		t.Errorf("A's stored mtime changed: was %d, now %d", storedMtimeA, storedMtimeA2)
	}

	// B's stored mtime must have advanced.
	_, storedMtimeB2, okB2 := env.db.GetSessionFileInfo("forge:multi-conv-B")
	if !okB2 {
		t.Fatal("session B file info not found after partial sync")
	}
	if storedMtimeB2 <= storedMtimeA {
		t.Errorf("B's stored mtime did not advance: got %d, A had %d", storedMtimeB2, storedMtimeA)
	}
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

	if err := env.engine.SyncSingleSession("forge:disappearing-conv"); err != nil {
		t.Fatalf("SyncSingleSession: %v", err)
	}

	// Now delete the conversation from .forge.db.
	forge.mustExec(t, "delete conversation",
		`DELETE FROM conversations WHERE conversation_id = 'disappearing-conv'`,
	)

	// FindSourceFile still returns the db path (it's a directory-level lookup).
	// SourceMtime returns 0 because the conversation row is gone.
	mtime := env.engine.SourceMtime("forge:disappearing-conv")
	if mtime != 0 {
		t.Errorf("SourceMtime after delete = %d, want 0", mtime)
	}

	src := env.engine.FindSourceFile("forge:disappearing-conv")
	if src != "" {
		t.Errorf("FindSourceFile after delete = %q, want empty", src)
	}

	// SyncSingleSession must return an error indicating the conversation
	// could not be found (either "not found" or a db no-rows error).
	err := env.engine.SyncSingleSession("forge:disappearing-conv")
	if err == nil {
		t.Fatal("expected error from SyncSingleSession for deleted conversation, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "not found") && !strings.Contains(msg, "no rows") {
		t.Errorf("expected 'not found' or 'no rows' error, got: %v", err)
	}
}

func TestSyncForgeSyncSingleNonExistent(t *testing.T) {
	env := setupTestEnv(t)
	// Create an empty .forge.db so FindForgeDBPath finds it.
	createForgeDB(t, env.forgeDir)

	err := env.engine.SyncSingleSession("forge:does-not-exist")
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "not found") && !strings.Contains(msg, "no rows") {
		t.Errorf("expected 'not found' or 'no rows' error, got: %v", err)
	}
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
	if err != nil {
		t.Fatalf("query tool_calls for parent: %v", err)
	}
	if !subagentSessID.Valid || subagentSessID.String != "forge:"+childID {
		t.Errorf("tool_calls.subagent_session_id = %v, want forge:%s", subagentSessID, childID)
	}

	// SyncAll must now call LinkSubagentSessions after the Forge write,
	// so parent_session_id and relationship_type on the child must already
	// be set without any SyncSingleSession workaround.
	var parentSessID sql.NullString
	var relType sql.NullString
	err = env.db.Reader().QueryRow(
		`SELECT parent_session_id, relationship_type FROM sessions WHERE id = ?`,
		"forge:"+childID,
	).Scan(&parentSessID, &relType)
	if err != nil {
		t.Fatalf("query child session: %v", err)
	}
	if !parentSessID.Valid || parentSessID.String != "forge:"+parentID {
		t.Errorf("child parent_session_id = %v, want forge:%s", parentSessID, parentID)
	}
	if !relType.Valid || relType.String != "subagent" {
		t.Errorf("child relationship_type = %v, want subagent", relType)
	}
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
	if err != nil {
		t.Fatalf("query broken session: %v", err)
	}
	if count != 0 {
		t.Errorf("broken session present in DB (count=%d), expected 0", count)
	}
}
