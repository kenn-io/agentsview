package parser

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// createTestVscdb creates a minimal Cursor state.vscdb SQLite
// database at path with the cursorDiskKV table.
func createTestVscdb(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open vscdb: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE cursorDiskKV (
			key TEXT UNIQUE ON CONFLICT REPLACE,
			value BLOB
		)
	`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

// insertComposerData inserts a composerData entry.
func insertComposerData(
	t *testing.T, db *sql.DB,
	sessionID string, data cursorComposerData,
) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal composerData: %v", err)
	}
	_, err = db.Exec(
		"INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)",
		"composerData:"+sessionID, raw,
	)
	if err != nil {
		t.Fatalf("insert composerData: %v", err)
	}
}

// insertBubble inserts a bubbleId entry.
func insertBubble(
	t *testing.T, db *sql.DB,
	sessionID, bubbleID string, bubble cursorBubble,
) {
	t.Helper()
	raw, err := json.Marshal(bubble)
	if err != nil {
		t.Fatalf("marshal bubble: %v", err)
	}
	_, err = db.Exec(
		"INSERT INTO cursorDiskKV (key, value) VALUES (?, ?)",
		"bubbleId:"+sessionID+":"+bubbleID, raw,
	)
	if err != nil {
		t.Fatalf("insert bubble: %v", err)
	}
}

func TestListCursorVscdbSessions_NonExistent(t *testing.T) {
	metas, err := ListCursorVscdbSessions(
		"/nonexistent/state.vscdb",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metas != nil {
		t.Errorf("expected nil for nonexistent db, got %v", metas)
	}
}

func TestListCursorVscdbSessions_Empty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)
	db.Close()

	metas, err := ListCursorVscdbSessions(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("expected 0 metas, got %d", len(metas))
	}
}

func TestListCursorVscdbSessions_SkipsEmpty(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)
	defer db.Close()

	// Session with no headers — should be skipped.
	insertComposerData(t, db, "session-empty", cursorComposerData{
		ComposerID:                  "session-empty",
		CreatedAt:                   1000000,
		LastUpdatedAt:               2000000,
		FullConversationHeadersOnly: nil,
	})

	// Session with headers — should appear.
	insertComposerData(t, db, "session-ok", cursorComposerData{
		ComposerID:    "session-ok",
		Name:          "Test session",
		CreatedAt:     1000000,
		LastUpdatedAt: 2000000,
		FullConversationHeadersOnly: []cursorBubbleHeader{
			{BubbleID: "b1", Type: 1},
		},
	})

	db.Close()

	metas, err := ListCursorVscdbSessions(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(metas) != 1 {
		t.Errorf("expected 1 meta, got %d", len(metas))
	}
	if metas[0].SessionID != "session-ok" {
		t.Errorf("got session %q, want session-ok", metas[0].SessionID)
	}
	if metas[0].Name != "Test session" {
		t.Errorf("got name %q, want 'Test session'", metas[0].Name)
	}
	if metas[0].FileMtime != 2000000*1_000_000 {
		t.Errorf(
			"FileMtime = %d, want %d",
			metas[0].FileMtime, 2000000*1_000_000,
		)
	}
	if metas[0].VirtualPath != dbPath+"#session-ok" {
		t.Errorf(
			"VirtualPath = %q, want %q",
			metas[0].VirtualPath, dbPath+"#session-ok",
		)
	}
}

func TestListCursorVscdbSessions_SubComposerIDs(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)

	insertComposerData(t, db, "parent-session", cursorComposerData{
		ComposerID:     "parent-session",
		CreatedAt:      1000000,
		LastUpdatedAt:  2000000,
		SubComposerIDs: []string{"child-1", "child-2"},
		FullConversationHeadersOnly: []cursorBubbleHeader{
			{BubbleID: "b1", Type: 1},
		},
	})
	db.Close()

	metas, err := ListCursorVscdbSessions(dbPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("expected 1 meta, got %d", len(metas))
	}
	if len(metas[0].SubComposerIDs) != 2 {
		t.Errorf(
			"SubComposerIDs len = %d, want 2",
			len(metas[0].SubComposerIDs),
		)
	}
}

func TestParseCursorVscdbSession_NonExistent(t *testing.T) {
	sess, msgs, err := ParseCursorVscdbSession(
		"/nonexistent/state.vscdb",
		"some-id", "myproject", "local",
	)
	if err == nil {
		t.Fatal("expected error for nonexistent db")
	}
	if sess != nil || msgs != nil {
		t.Error("expected nil session and messages")
	}
}

func TestParseCursorVscdbSession_BasicTextOnly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)

	sessionID := "test-session-1"
	bubble1 := "bubble-user-1"
	bubble2 := "bubble-asst-1"

	insertComposerData(t, db, sessionID, cursorComposerData{
		ComposerID:    sessionID,
		Name:          "My test session",
		CreatedAt:     1000000,
		LastUpdatedAt: 2000000,
		FullConversationHeadersOnly: []cursorBubbleHeader{
			{BubbleID: bubble1, Type: 1},
			{BubbleID: bubble2, Type: 2},
		},
	})

	insertBubble(t, db, sessionID, bubble1, cursorBubble{
		BubbleID:  bubble1,
		Type:      1,
		Text:      "Hello, can you help me?",
		CreatedAt: "2025-01-01T10:00:00.000Z",
	})
	insertBubble(t, db, sessionID, bubble2, cursorBubble{
		BubbleID:  bubble2,
		Type:      2,
		Text:      "Of course! What do you need?",
		CreatedAt: "2025-01-01T10:00:01.000Z",
	})

	db.Close()

	sess, msgs, err := ParseCursorVscdbSession(
		dbPath, sessionID, "myproject", "local",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}

	assertEq(t, "ID", sess.ID, "cursor:"+sessionID)
	assertEq(t, "Project", sess.Project, "myproject")
	assertEq(t, "Machine", sess.Machine, "local")
	assertEq(t, "Agent", string(sess.Agent), "cursor")
	assertEq(t, "MessageCount", sess.MessageCount, 2)
	assertEq(t, "UserMessageCount", sess.UserMessageCount, 1)
	if sess.FirstMessage == "" {
		t.Error("expected non-empty FirstMessage")
	}

	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	assertEq(t, "msgs[0].Role", string(msgs[0].Role), "user")
	assertEq(t, "msgs[0].Content", msgs[0].Content, "Hello, can you help me?")
	assertEq(t, "msgs[1].Role", string(msgs[1].Role), "assistant")
	assertEq(t, "msgs[1].Content", msgs[1].Content, "Of course! What do you need?")
}

func TestParseCursorVscdbSession_WithToolCall(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)

	sessionID := "tool-session"
	b1 := "b-user"
	b2 := "b-tool"
	b3 := "b-text"

	params := json.RawMessage(`{"pattern":"foo","path":"/src"}`)

	insertComposerData(t, db, sessionID, cursorComposerData{
		ComposerID:    sessionID,
		CreatedAt:     1000000,
		LastUpdatedAt: 2000000,
		FullConversationHeadersOnly: []cursorBubbleHeader{
			{BubbleID: b1, Type: 1},
			{BubbleID: b2, Type: 2},
			{BubbleID: b3, Type: 2},
		},
	})

	insertBubble(t, db, sessionID, b1, cursorBubble{
		BubbleID:  b1,
		Type:      1,
		Text:      "Search for foo in /src",
		CreatedAt: "2025-01-01T10:00:00.000Z",
	})
	insertBubble(t, db, sessionID, b2, cursorBubble{
		BubbleID:  b2,
		Type:      2,
		CreatedAt: "2025-01-01T10:00:01.000Z",
		ToolFormerData: &cursorToolFormerData{
			Name:       "grep",
			ToolCallID: "call-001",
			Status:     "completed",
			Params:     params,
		},
	})
	insertBubble(t, db, sessionID, b3, cursorBubble{
		BubbleID:  b3,
		Type:      2,
		Text:      "Found 3 matches.",
		CreatedAt: "2025-01-01T10:00:02.000Z",
	})

	db.Close()

	sess, msgs, err := ParseCursorVscdbSession(
		dbPath, sessionID, "myproject", "local",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess == nil {
		t.Fatal("expected non-nil session")
	}
	// User message + one merged assistant message.
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	asstMsg := msgs[1]
	assertEq(t, "asstMsg.Role", string(asstMsg.Role), "assistant")
	assertEq(t, "asstMsg.HasToolUse", asstMsg.HasToolUse, true)
	assertEq(t, "asstMsg.Content", asstMsg.Content, "Found 3 matches.")
	if len(asstMsg.ToolCalls) != 1 {
		t.Fatalf(
			"expected 1 tool call, got %d",
			len(asstMsg.ToolCalls),
		)
	}
	tc := asstMsg.ToolCalls[0]
	assertEq(t, "tc.ToolName", tc.ToolName, "grep")
	assertEq(t, "tc.Category", tc.Category, "Grep")
	assertEq(t, "tc.ToolUseID", tc.ToolUseID, "call-001")
	if tc.InputJSON == "" {
		t.Error("expected non-empty InputJSON")
	}
}

func TestParseCursorVscdbSession_EmptySession(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)

	// Session with headers but no matching bubble data.
	insertComposerData(t, db, "empty-session", cursorComposerData{
		ComposerID:    "empty-session",
		CreatedAt:     1000000,
		LastUpdatedAt: 2000000,
		FullConversationHeadersOnly: []cursorBubbleHeader{
			{BubbleID: "missing-bubble", Type: 1},
		},
	})
	db.Close()

	sess, msgs, err := ParseCursorVscdbSession(
		dbPath, "empty-session", "proj", "local",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess != nil {
		t.Errorf("expected nil session for empty content, got %+v", sess)
	}
	if msgs != nil {
		t.Errorf("expected nil messages, got %v", msgs)
	}
}

func TestNormalizeCursorVscdbTool(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"run_terminal_command_v2", "Bash"},
		{"run_terminal_cmd", "Bash"},
		{"read_file_v2", "Read"},
		{"edit_file_v2", "Edit"},
		{"search_replace", "Edit"},
		{"apply_patch", "Edit"},
		{"ripgrep_raw_search", "Grep"},
		{"rg", "Grep"},
		{"glob_file_search", "Glob"},
		{"file_search", "Glob"},
		{"task_v2", "Task"},
		{"delete_file", "Write"},
		{"list_dir_v2", "Read"},
		{"list_dir", "Read"},
		{"read_lints", "Read"},
		{"todo_write", "Tool"},
		{"create_plan", "Tool"},
		{"ask_question", "Tool"},
		{"switch_mode", "Tool"},
		{"codebase_search", "Tool"},
		{"semantic_search_full", "Tool"},
		{"web_search", "Tool"},
		{"web_fetch", "Tool"},
		{"mcp-github", "Tool"},
		{"mcp-linear-search", "Tool"},
		{"grep", "Grep"},
		{"shell", "Bash"},
		{"unknown_tool_xyz", "Other"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeToolCategory(tt.name)
			if got != tt.want {
				t.Errorf(
					"NormalizeToolCategory(%q) = %q, want %q",
					tt.name, got, tt.want,
				)
			}
		})
	}
}

func TestBuildCursorVscdbMessages_GroupsConsecutiveAssistant(t *testing.T) {
	headers := []cursorBubbleHeader{
		{BubbleID: "u1", Type: 1},
		{BubbleID: "a1", Type: 2}, // tool call
		{BubbleID: "a2", Type: 2}, // text
		{BubbleID: "u2", Type: 1},
		{BubbleID: "a3", Type: 2}, // text
	}
	params := json.RawMessage(`{"path":"/foo"}`)
	bubbles := map[string]cursorBubble{
		"u1": {BubbleID: "u1", Type: 1, Text: "First question"},
		"a1": {
			BubbleID:  "a1",
			Type:      2,
			CreatedAt: "2025-01-01T10:00:00Z",
			ToolFormerData: &cursorToolFormerData{
				Name:   "read_file_v2",
				Status: "completed",
				Params: params,
			},
		},
		"a2": {BubbleID: "a2", Type: 2, Text: "Here is the content."},
		"u2": {BubbleID: "u2", Type: 1, Text: "Second question"},
		"a3": {BubbleID: "a3", Type: 2, Text: "Another response."},
	}

	msgs := buildCursorVscdbMessages(headers, bubbles)

	// Expect: user, assistant(tool+text), user, assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}

	assertEq(t, "msgs[0].Role", string(msgs[0].Role), "user")
	assertEq(t, "msgs[1].Role", string(msgs[1].Role), "assistant")
	assertEq(t, "msgs[1].HasToolUse", msgs[1].HasToolUse, true)
	assertEq(t, "msgs[1].Content", msgs[1].Content, "Here is the content.")
	if len(msgs[1].ToolCalls) != 1 {
		t.Errorf("expected 1 tool call, got %d", len(msgs[1].ToolCalls))
	}
	assertEq(t, "msgs[2].Role", string(msgs[2].Role), "user")
	assertEq(t, "msgs[3].Role", string(msgs[3].Role), "assistant")
	assertEq(t, "msgs[3].Content", msgs[3].Content, "Another response.")
}

func TestParseCursorParamsJSON(t *testing.T) {
	tests := []struct {
		name  string
		input json.RawMessage
		want  string
	}{
		{
			name:  "object",
			input: json.RawMessage(`{"key":"value"}`),
			want:  `{"key":"value"}`,
		},
		{
			name:  "string wrapping json",
			input: json.RawMessage(`"{\"key\":\"value\"}"`),
			want:  `{"key":"value"}`,
		},
		{
			name:  "empty",
			input: json.RawMessage(nil),
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCursorParamsJSON(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
