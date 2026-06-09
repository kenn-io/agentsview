package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	_ "github.com/mattn/go-sqlite3"
)

func TestDiscoverZedSessions(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	files := DiscoverZedSessions(root)
	if len(files) != 1 {
		t.Fatalf("len(files) = %d, want 1", len(files))
	}
	if files[0].Path != dbPath || files[0].Agent != AgentZed {
		t.Fatalf("file = %+v, want %s / %s", files[0], dbPath, AgentZed)
	}

	if files := DiscoverZedSessions(""); files != nil {
		t.Fatalf("DiscoverZedSessions(empty) = %v, want nil", files)
	}
	if files := DiscoverZedSessions(filepath.Join(root, "missing")); files != nil {
		t.Fatalf("DiscoverZedSessions(missing) = %v, want nil", files)
	}
}

func TestFindZedSourceFile(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, zedThreadsDBRelPath)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	threadID := "10431c84-c47b-4e6c-b2df-f9f3b9ad025b"
	createZedThreadsDBAt(t, dbPath, []zedTestThread{{
		id:       threadID,
		dataType: "json",
		data:     []byte(`{"messages":[]}`),
	}})

	got := FindZedSourceFile(root, threadID)
	want := ZedSQLiteVirtualPath(dbPath, threadID)
	if got != want {
		t.Fatalf("FindZedSourceFile() = %q, want %q", got, want)
	}
	if got := FindZedSourceFile(root, "../bad"); got != "" {
		t.Fatalf("FindZedSourceFile(invalid) = %q, want empty", got)
	}
	if got := FindZedSourceFile(root, "missing"); got != "" {
		t.Fatalf("FindZedSourceFile(missing) = %q, want empty", got)
	}
	if got := FindZedSourceFile(filepath.Join(root, "missing"), threadID); got != "" {
		t.Fatalf("FindZedSourceFile(missing root) = %q, want empty", got)
	}
}

func TestParseZedSessions_JSON(t *testing.T) {
	dbPath := createZedThreadsDB(t, []zedTestThread{{
		id:          "10431c84-c47b-4e6c-b2df-f9f3b9ad025b",
		summary:     "WP Record Scaffold Generation",
		createdAt:   "2026-06-08T09:12:41.962819Z",
		updatedAt:   "2026-06-08T09:14:10.475149Z",
		folderPaths: "/Users/alice/code/my-app",
		dataType:    "json",
		data: []byte(`{
			"model": {"model": "claude-opus-4", "provider": "anthropic"},
			"request_token_usage": {
				"req-1": {"input_tokens": 1000, "output_tokens": 200},
				"req-2": {"input_tokens": 1500, "output_tokens": 300}
			},
			"messages": [
				{"User": {"content": [{"Text": "Generate code"}]}},
				{"Agent": {"content": [
					{"Thinking": {"text": "Plan"}},
					{"Text": "Done"},
					{"ToolUse": {"id": "call_1", "name": "terminal", "input": {"command": "make test"}}}
				], "tool_results": {"call_1": {"content": [{"Text": "ok"}]}}}}
			]
		}`),
	}})

	results, err := ParseZedSessions(dbPath, "local")
	if err != nil {
		t.Fatalf("ParseZedSessions: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}

	sess := results[0].Session
	if sess.ID != "zed:10431c84-c47b-4e6c-b2df-f9f3b9ad025b" {
		t.Fatalf("session ID = %q", sess.ID)
	}
	if sess.Project != "my_app" || sess.Cwd != "/Users/alice/code/my-app" {
		t.Fatalf("project/cwd = %q/%q", sess.Project, sess.Cwd)
	}
	if sess.FirstMessage != "Generate code" || sess.MessageCount != 2 || sess.UserMessageCount != 1 {
		t.Fatalf("session summary = %+v", sess)
	}
	if sess.File.Path != ZedSQLiteVirtualPath(dbPath, "10431c84-c47b-4e6c-b2df-f9f3b9ad025b") {
		t.Fatalf("file path = %q", sess.File.Path)
	}
	if sess.TotalOutputTokens != 500 || sess.PeakContextTokens != 1500 {
		t.Fatalf("tokens: total_out=%d (want 500), peak_ctx=%d (want 1500)", sess.TotalOutputTokens, sess.PeakContextTokens)
	}
	if !sess.HasTotalOutputTokens || !sess.HasPeakContextTokens {
		t.Fatalf("token presence flags not set: has_total=%v, has_peak=%v", sess.HasTotalOutputTokens, sess.HasPeakContextTokens)
	}

	msgs := results[0].Messages
	if msgs[0].Role != RoleUser || msgs[0].Content != "Generate code" {
		t.Fatalf("user msg = %+v", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || msgs[1].Content != "Done" || !msgs[1].HasThinking || !msgs[1].HasToolUse {
		t.Fatalf("assistant msg = %+v", msgs[1])
	}
	if msgs[1].Model != "claude-opus-4" {
		t.Fatalf("assistant model = %q, want %q", msgs[1].Model, "claude-opus-4")
	}
	if len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].ToolName != "terminal" {
		t.Fatalf("tool calls = %+v", msgs[1].ToolCalls)
	}
	if len(msgs[1].ToolResults) != 1 || msgs[1].ToolResults[0].ToolUseID != "call_1" {
		t.Fatalf("tool results = %+v", msgs[1].ToolResults)
	}
	tr := msgs[1].ToolResults[0]
	if got := DecodeContent(tr.ContentRaw); got != "ok" {
		t.Fatalf("tool result ContentRaw decoded = %q, want %q (raw=%s)", got, "ok", tr.ContentRaw)
	}

	ue := results[0].UsageEvents
	if len(ue) != 1 {
		t.Fatalf("UsageEvents len = %d, want 1", len(ue))
	}
	if ue[0].Model != "claude-opus-4" {
		t.Fatalf("usage event model = %q, want %q", ue[0].Model, "claude-opus-4")
	}
	// input: 1000+1500=2500; output: 200+300=500
	if ue[0].InputTokens != 2500 || ue[0].OutputTokens != 500 {
		t.Fatalf("usage event tokens: in=%d (want 2500), out=%d (want 500)", ue[0].InputTokens, ue[0].OutputTokens)
	}
	if ue[0].DedupKey != "session:zed:10431c84-c47b-4e6c-b2df-f9f3b9ad025b" {
		t.Fatalf("usage event dedup key = %q", ue[0].DedupKey)
	}
}

func TestExtractZedDocMeta(t *testing.T) {
	tests := []struct {
		name         string
		doc          map[string]any
		wantModel    string
		wantTotalIn  int
		wantTotalOut int
		wantPeakCtx  int
		wantHasUsage bool
	}{
		{
			name:         "empty doc",
			doc:          map[string]any{},
			wantHasUsage: false,
		},
		{
			name: "model only",
			doc: map[string]any{
				"model": map[string]any{"model": "gpt-4o", "provider": "openai"},
			},
			wantModel:    "gpt-4o",
			wantHasUsage: false,
		},
		{
			name: "model and request usage",
			doc: map[string]any{
				"model": map[string]any{"model": "claude-opus-4", "provider": "anthropic"},
				"request_token_usage": map[string]any{
					"r1": map[string]any{"input_tokens": float64(1000), "output_tokens": float64(100)},
					"r2": map[string]any{"input_tokens": float64(2000), "output_tokens": float64(200)},
				},
			},
			wantModel:    "claude-opus-4",
			wantTotalIn:  3000,
			wantTotalOut: 300,
			wantPeakCtx:  2000,
			wantHasUsage: true,
		},
		{
			name: "empty request_token_usage",
			doc: map[string]any{
				"model":               map[string]any{"model": "gpt-5.5", "provider": "newapi"},
				"request_token_usage": map[string]any{},
			},
			wantModel:    "gpt-5.5",
			wantHasUsage: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractZedDocMeta(tc.doc)
			if got.model != tc.wantModel {
				t.Errorf("model = %q, want %q", got.model, tc.wantModel)
			}
			if got.totalInputTokens != tc.wantTotalIn {
				t.Errorf("totalInputTokens = %d, want %d", got.totalInputTokens, tc.wantTotalIn)
			}
			if got.totalOutputTokens != tc.wantTotalOut {
				t.Errorf("totalOutputTokens = %d, want %d", got.totalOutputTokens, tc.wantTotalOut)
			}
			if got.peakContextTokens != tc.wantPeakCtx {
				t.Errorf("peakContextTokens = %d, want %d", got.peakContextTokens, tc.wantPeakCtx)
			}
			if got.hasTokenUsage != tc.wantHasUsage {
				t.Errorf("hasTokenUsage = %v, want %v", got.hasTokenUsage, tc.wantHasUsage)
			}
		})
	}
}

func TestParseZedSessions_ZstdAndFiltersChildren(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	compressed := encoder.EncodeAll([]byte(`{
		"messages": [{"User": {"content": [{"Text": "Hello"}]}}]
	}`), nil)
	encoder.Close()

	dbPath := createZedThreadsDB(t, []zedTestThread{
		{
			id:        "parent",
			summary:   "Parent",
			updatedAt: "2026-06-08T09:14:10Z",
			dataType:  "zstd",
			data:      compressed,
		},
		{
			id:        "child",
			summary:   "Child",
			parentID:  "parent",
			updatedAt: "2026-06-08T09:14:11Z",
			dataType:  "json",
			data:      []byte(`{"messages":[{"User":{"content":[{"Text":"skip"}]}}]}`),
		},
	})

	results, err := ParseZedSessions(dbPath, "local")
	if err != nil {
		t.Fatalf("ParseZedSessions: %v", err)
	}
	if len(results) != 1 || results[0].Session.ID != "zed:parent" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Messages[0].Content != "Hello" {
		t.Fatalf("message = %+v", results[0].Messages[0])
	}
}

func TestParseZedSessions_SkipsUnsupportedDataType(t *testing.T) {
	dbPath := createZedThreadsDB(t, []zedTestThread{{
		id:       "bad",
		dataType: "brotli",
		data:     []byte("x"),
	}})
	results, err := ParseZedSessions(dbPath, "local")
	if err != nil {
		t.Fatalf("ParseZedSessions: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

func TestParseZedSQLiteVirtualPath(t *testing.T) {
	dbPath := filepath.Join("/tmp", "with#hash", "threads.db")
	virtual := ZedSQLiteVirtualPath(dbPath, "sess-1")
	gotDB, gotID, ok := ParseZedSQLiteVirtualPath(virtual)
	if !ok || gotDB != dbPath || gotID != "sess-1" {
		t.Fatalf("ParseZedSQLiteVirtualPath = (%q, %q, %v)", gotDB, gotID, ok)
	}
	if _, _, ok := ParseZedSQLiteVirtualPath("/tmp/not-db#sess-1"); ok {
		t.Fatal("ParseZedSQLiteVirtualPath accepted non-threads DB")
	}
}

type zedTestThread struct {
	id          string
	summary     string
	updatedAt   string
	dataType    string
	data        []byte
	parentID    string
	folderPaths string
	createdAt   string
}

func createZedThreadsDB(t *testing.T, threads []zedTestThread) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "threads.db")
	createZedThreadsDBAt(t, dbPath, threads)
	return dbPath
}

func createZedThreadsDBAt(
	t *testing.T,
	dbPath string,
	threads []zedTestThread,
) {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY,
		summary TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		data_type TEXT NOT NULL,
		data BLOB NOT NULL,
		parent_id TEXT,
		folder_paths TEXT,
		folder_paths_order TEXT,
		created_at TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range threads {
		_, err = db.Exec(`INSERT INTO threads (
			id, summary, updated_at, data_type, data,
			parent_id, folder_paths, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			thread.id,
			thread.summary,
			thread.updatedAt,
			thread.dataType,
			thread.data,
			nullString(thread.parentID),
			thread.folderPaths,
			thread.createdAt,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
