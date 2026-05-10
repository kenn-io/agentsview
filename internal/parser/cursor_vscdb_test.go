package parser

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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

func TestParseCursorVscdbSession_PersistsToolResults(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.vscdb")
	db := createTestVscdb(t, dbPath)

	sessionID := "tool-result-session"
	bUser := "u1"
	bTool := "t1"

	insertComposerData(t, db, sessionID, cursorComposerData{
		ComposerID:    sessionID,
		CreatedAt:     1000000,
		LastUpdatedAt: 2000000,
		FullConversationHeadersOnly: []cursorBubbleHeader{
			{BubbleID: bUser, Type: 1},
			{BubbleID: bTool, Type: 2},
		},
	})

	insertBubble(t, db, sessionID, bUser, cursorBubble{
		BubbleID:  bUser,
		Type:      1,
		Text:      "List the files",
		CreatedAt: "2025-01-01T10:00:00.000Z",
	})
	resultJSON := json.RawMessage(`"file1.go\nfile2.go\nfile3.go"`)
	insertBubble(t, db, sessionID, bTool, cursorBubble{
		BubbleID:  bTool,
		Type:      2,
		CreatedAt: "2025-01-01T10:00:01.000Z",
		ToolFormerData: &cursorToolFormerData{
			Name:       "list_dir",
			ToolCallID: "call-list-001",
			Status:     "completed",
			Params:     json.RawMessage(`{"path":"/src"}`),
			Result:     resultJSON,
		},
	})

	db.Close()

	_, msgs, err := ParseCursorVscdbSession(
		dbPath, sessionID, "proj", "local",
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	asst := msgs[1]
	if len(asst.ToolResults) != 1 {
		t.Fatalf(
			"expected 1 tool result, got %d",
			len(asst.ToolResults),
		)
	}
	tr := asst.ToolResults[0]
	assertEq(t, "tr.ToolUseID", tr.ToolUseID, "call-list-001")
	if tr.ContentRaw != string(resultJSON) {
		t.Errorf(
			"tr.ContentRaw = %q, want %q",
			tr.ContentRaw, string(resultJSON),
		)
	}
	// "file1.go\nfile2.go\nfile3.go" decodes to 26 chars.
	if tr.ContentLength != 26 {
		t.Errorf(
			"tr.ContentLength = %d, want 26",
			tr.ContentLength,
		)
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

func TestCursorVscdbDSN(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		rawQuery string
		want     string
	}{
		{
			name:     "plain-path",
			path:     "/home/u/state.vscdb",
			rawQuery: "mode=ro",
			want:     "file:///home/u/state.vscdb?mode=ro",
		},
		{
			name:     "path-with-question-mark",
			path:     "/tmp/foo?bar/state.vscdb",
			rawQuery: "mode=ro",
			want:     "file:///tmp/foo%3Fbar/state.vscdb?mode=ro",
		},
		{
			name:     "path-with-hash",
			path:     "/tmp/foo#bar/state.vscdb",
			rawQuery: "mode=ro&_busy_timeout=3000",
			want:     "file:///tmp/foo%23bar/state.vscdb?mode=ro&_busy_timeout=3000",
		},
		{
			name:     "path-with-percent",
			path:     "/tmp/has%20space/state.vscdb",
			rawQuery: "mode=ro",
			want:     "file:///tmp/has%2520space/state.vscdb?mode=ro",
		},
		{
			name:     "path-with-space",
			path:     "/home/u/My Cursor/state.vscdb",
			rawQuery: "mode=ro",
			want:     "file:///home/u/My%20Cursor/state.vscdb?mode=ro",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("posix path expectations")
			}
			got := cursorVscdbDSN(tt.path, tt.rawQuery)
			if got != tt.want {
				t.Errorf(
					"cursorVscdbDSN(%q, %q) = %q, want %q",
					tt.path, tt.rawQuery, got, tt.want,
				)
			}
		})
	}
}

func TestOpenCursorVscdb_PathWithSpecialChars(t *testing.T) {
	// Verify openCursorVscdb opens DBs whose path contains
	// characters that would otherwise be parsed by the
	// sqlite3 driver as DSN separators when concatenated raw.
	if runtime.GOOS == "windows" {
		t.Skip("'?' and '#' are not valid in Windows filenames")
	}
	parent := filepath.Join(t.TempDir(), "weird?#dir")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(parent, "state.vscdb")
	// Bootstrap the DB through the same DSN helper so the
	// file lands at the intended path; sql.Open with a raw
	// path containing '?' would otherwise be split by the
	// sqlite3 driver.
	dsn := cursorVscdbDSN(dbPath, "")
	d, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	if _, err := d.Exec(`CREATE TABLE cursorDiskKV (
		key TEXT UNIQUE ON CONFLICT REPLACE,
		value BLOB
	)`); err != nil {
		d.Close()
		t.Fatalf("bootstrap exec: %v", err)
	}
	d.Close()

	got, err := openCursorVscdb(dbPath)
	if err != nil {
		t.Fatalf("openCursorVscdb: %v", err)
	}
	defer got.Close()
	var n int
	if err := got.QueryRow(
		"SELECT COUNT(*) FROM cursorDiskKV",
	).Scan(&n); err != nil {
		t.Fatalf("query: %v", err)
	}
}

func TestFileURLToPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		// posix is the expected output on macOS/Linux; tests
		// skip when running on Windows so the helper's
		// drive-letter/UNC branches don't perturb the assertion.
		posix string
	}{
		{
			name:  "posix-absolute",
			in:    "file:///home/user/proj",
			posix: "/home/user/proj",
		},
		{
			name:  "percent-encoded-spaces",
			in:    "file:///home/user/My%20Project",
			posix: "/home/user/My Project",
		},
		{
			name:  "percent-encoded-unicode",
			in:    "file:///home/user/r%C3%A9sum%C3%A9",
			posix: "/home/user/résumé",
		},
		{
			name:  "no-scheme-passthrough",
			in:    "/no/scheme",
			posix: "/no/scheme",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" {
				t.Skip("posix-only path expectations")
			}
			got := fileURLToPath(tt.in)
			if got != tt.posix {
				t.Errorf(
					"fileURLToPath(%q) = %q, want %q",
					tt.in, got, tt.posix,
				)
			}
		})
	}
}

func TestIsCursorVscdbVirtualPath(t *testing.T) {
	// Paths use OS-native separators since they originate from
	// filepath.Join in production code; filepath.Base only
	// recognizes the host OS's separator.
	good := filepath.Join(
		"globalStorage", "state.vscdb",
	) + "#abc-123"
	noSession := filepath.Join("globalStorage", "state.vscdb")
	wrongName := filepath.Join("notavscdb") + "#abc"

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"with-session-id", good, true},
		{"missing-session-id", noSession, false},
		{"jsonl", "/some/path/file.jsonl", false},
		{"wrong-basename", wrongName, false},
		{"only-hash", "#abc", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsCursorVscdbVirtualPath(tt.path)
			if got != tt.want {
				t.Errorf(
					"IsCursorVscdbVirtualPath(%q) = %v, want %v",
					tt.path, got, tt.want,
				)
			}
		})
	}
}

func TestIsCursorVscdbPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want bool
	}{
		{
			"linux-default",
			filepath.Join(
				".config", "Cursor", "User",
				"globalStorage", "state.vscdb",
			),
			true,
		},
		{
			"macos-default",
			filepath.Join(
				"Library", "Application Support", "Cursor",
				"User", "globalStorage", "state.vscdb",
			),
			true,
		},
		{"transcripts-dir", ".cursor/projects", false},
		{"jsonl", "/some/path/file.jsonl", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsCursorVscdbPath(
				tt.path,
			); got != tt.want {
				t.Errorf(
					"IsCursorVscdbPath(%q) = %v, want %v",
					tt.path, got, tt.want,
				)
			}
		})
	}
}

func TestFindCursorVscdb(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "state.vscdb")
	if err := os.WriteFile(good, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed vscdb: %v", err)
	}
	missing := filepath.Join(dir, "missing", "state.vscdb")
	notVscdb := filepath.Join(dir, "transcripts")
	if err := os.MkdirAll(notVscdb, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	t.Run("returns-existing-vscdb", func(t *testing.T) {
		got := FindCursorVscdb([]string{notVscdb, good})
		if got != good {
			t.Errorf("got %q, want %q", got, good)
		}
	})
	t.Run("skips-missing", func(t *testing.T) {
		if got := FindCursorVscdb(
			[]string{missing},
		); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("skips-non-vscdb", func(t *testing.T) {
		if got := FindCursorVscdb(
			[]string{notVscdb},
		); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
	t.Run("empty-input", func(t *testing.T) {
		if got := FindCursorVscdb(nil); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
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
