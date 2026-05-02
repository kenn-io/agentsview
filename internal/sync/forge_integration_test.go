package sync_test

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
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
