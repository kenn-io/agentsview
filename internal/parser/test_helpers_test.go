package parser

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const devinSyntheticSessionsSchema = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY,
	title TEXT,
	working_directory TEXT,
	model TEXT,
	created_at INTEGER,
	last_activity_at INTEGER,
	workspace_json TEXT,
	metadata_json TEXT,
	hidden INTEGER NOT NULL DEFAULT 0
);
`

const devinSyntheticMessageNodesSchema = `
CREATE TABLE message_nodes (
	row_id INTEGER PRIMARY KEY AUTOINCREMENT,
	session_id TEXT NOT NULL,
	node_id INTEGER NOT NULL,
	parent_node_id INTEGER,
	chat_message TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	metadata TEXT
);
`

type devinSessionRow struct {
	ID                 string
	Title              string
	WorkingDirectory   string
	Model              string
	CreatedAtMillis    *int64
	LastActivityMillis *int64
	WorkspaceJSON      string
	MetadataJSON       string
	Hidden             bool
}

type devinTestFixture struct {
	Root           string
	CLIDir         string
	TranscriptsDir string
	DBPath         string
}

type devinSyntheticMessageNodeRow struct {
	SessionID       string
	NodeID          int64
	ParentNodeID    *int64
	ChatMessage     string
	CreatedAtMillis int64
	MetadataJSON    string
}

func newDevinTestFixture(t *testing.T, rows ...devinSessionRow) *devinTestFixture {
	t.Helper()

	root := t.TempDir()
	cliDir := filepath.Join(root, "cli")
	transcriptsDir := filepath.Join(cliDir, "transcripts")
	require.NoError(t, os.MkdirAll(transcriptsDir, 0o755))

	fixture := &devinTestFixture{
		Root:           root,
		CLIDir:         cliDir,
		TranscriptsDir: transcriptsDir,
		DBPath:         filepath.Join(cliDir, devinDBFilename),
	}

	initDevinTestDB(t, fixture.DBPath)
	execDevinTestSQL(t, fixture.DBPath, devinSyntheticSessionsSchema)
	execDevinTestSQL(t, fixture.DBPath, devinSyntheticMessageNodesSchema)
	for _, row := range rows {
		insertDevinSessionRow(t, fixture.DBPath, row)
	}

	return fixture
}

func initDevinTestDB(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func insertDevinSessionRow(t *testing.T, dbPath string, row devinSessionRow) {
	t.Helper()

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`
		INSERT INTO sessions (
			id,
			title,
			working_directory,
			model,
			created_at,
			last_activity_at,
			workspace_json,
			metadata_json,
			hidden
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		row.ID,
		row.Title,
		row.WorkingDirectory,
		row.Model,
		devinNullableMillis(row.CreatedAtMillis),
		devinNullableMillis(row.LastActivityMillis),
		devinNullableString(row.WorkspaceJSON),
		devinNullableString(row.MetadataJSON),
		row.Hidden,
	)
	require.NoError(t, err)
}

func devinNullableMillis(ms *int64) any {
	if ms == nil {
		return nil
	}
	return *ms
}

func devinNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (f *devinTestFixture) writeTranscript(t *testing.T, sessionID, transcript string) string {
	t.Helper()
	transcriptPath := filepath.Join(f.TranscriptsDir, sessionID+".json")
	require.NoError(t, os.WriteFile(transcriptPath, []byte(transcript), 0o644))
	return transcriptPath
}

func (f *devinTestFixture) sessionVirtualPath(sessionID string) string {
	return VirtualSourcePath(f.DBPath, sessionID)
}

func (f *devinTestFixture) insertMessageNodes(t *testing.T, rows ...devinSyntheticMessageNodeRow) {
	t.Helper()
	for _, row := range rows {
		insertDevinMessageNodeRow(t, f.DBPath, row)
	}
}

func newDevinSessionFixture(t *testing.T, row devinSessionRow, transcript string) (string, string) {
	t.Helper()
	fixture := newDevinTestFixture(t, row)
	return fixture.DBPath, fixture.writeTranscript(t, row.ID, transcript)
}

func execDevinTestSQL(t *testing.T, dbPath, query string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(query)
	require.NoError(t, err)
}

func insertDevinMessageNodeRow(t *testing.T, dbPath string, row devinSyntheticMessageNodeRow) {
	t.Helper()

	db, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`
		INSERT INTO message_nodes (
			session_id,
			node_id,
			parent_node_id,
			chat_message,
			created_at,
			metadata
		) VALUES (?, ?, ?, ?, ?, ?)
	`,
		row.SessionID,
		row.NodeID,
		devinNullableInt64(row.ParentNodeID),
		row.ChatMessage,
		row.CreatedAtMillis,
		devinNullableString(row.MetadataJSON),
	)
	require.NoError(t, err)
}

func devinNullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func devinMetaIDs(metas []DevinSessionMeta) []string {
	ids := make([]string, 0, len(metas))
	for _, meta := range metas {
		ids = append(ids, meta.RawSessionID)
	}
	return ids
}

// parseChatGPTExport parses a ChatGPT export through the ChatGPT import-only
// provider. It is the test harness replacement for the former
// ParseChatGPTExport free function.
func parseChatGPTExport(
	dir string,
	assets AssetResolver,
	onConversation func(ParseResult) error,
) error {
	p, ok := NewProvider(AgentChatGPT, ProviderConfig{})
	if !ok {
		return fmt.Errorf("chatgpt provider unavailable")
	}
	exporter, ok := p.(ChatGPTExportParser)
	if !ok {
		return fmt.Errorf("chatgpt provider does not support exports")
	}
	return exporter.ParseChatGPTExport(dir, assets, onConversation)
}

// parseClaudeAIExport parses a Claude.ai export through the Claude.ai
// import-only provider. It is the test harness replacement for the former
// ParseClaudeAIExport free function.
func parseClaudeAIExport(
	r io.Reader,
	onConversation func(ParseResult) error,
) error {
	p, ok := NewProvider(AgentClaudeAI, ProviderConfig{})
	if !ok {
		return fmt.Errorf("claude.ai provider unavailable")
	}
	exporter, ok := p.(ClaudeAIExportParser)
	if !ok {
		return fmt.Errorf("claude.ai provider does not support exports")
	}
	return exporter.ParseClaudeAIExport(r, onConversation)
}

// Timestamp constants for test data.
const (
	tsZero    = "2024-01-01T00:00:00Z"
	tsZeroS1  = "2024-01-01T00:00:01Z"
	tsZeroS2  = "2024-01-01T00:00:02Z"
	tsEarly   = "2024-01-01T10:00:00Z"
	tsEarlyS1 = "2024-01-01T10:00:01Z"
	tsEarlyS5 = "2024-01-01T10:00:05Z"
	tsLate    = "2024-01-01T10:01:00Z"
	tsLateS5  = "2024-01-01T10:01:05Z"
)

// Parsed time.Time values used as expected results in
// timestamp parsing tests.
var testJan15_1030UTC = time.Date(
	2024, 1, 15, 10, 30, 0, 0, time.UTC,
)

// --- Data Generators ---

func generateLargeString(size int) string {
	return strings.Repeat("x", size)
}

// --- Assertions ---

func assertSessionMeta(t *testing.T, s *ParsedSession, wantID, wantProject string, wantAgent AgentType) {
	t.Helper()
	require.NotNil(t, s, "session is nil")
	assert.Equal(t, wantID, s.ID, "session ID")
	assert.Equal(t, wantProject, s.Project, "project")
	assert.Equal(t, wantAgent, s.Agent, "agent")
}

func assertMessage(t *testing.T, m ParsedMessage, wantRole RoleType, wantContentSnippet string) {
	t.Helper()
	assert.Equal(t, wantRole, m.Role, "role")
	if wantContentSnippet != "" {
		assert.Contains(t, m.Content, wantContentSnippet)
	}
}

func assertMessageCount(t *testing.T, count, want int) {
	t.Helper()
	require.Equal(t, want, count, "message count")
}

func assertTimestamp(t *testing.T, got time.Time, want time.Time) {
	t.Helper()
	assert.True(t, got.Equal(want), "timestamp = %v, want %v", got, want)
}

func assertZeroTimestamp(
	t *testing.T, ts time.Time, label string,
) {
	t.Helper()
	assert.True(t, ts.IsZero(), "%s = %v, want zero", label, ts)
}

// captureLog redirects log output to a buffer for the
// duration of the test and restores it on cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return &buf
}

func assertLogContains(
	t *testing.T, buf *bytes.Buffer, substrs ...string,
) {
	t.Helper()
	got := buf.String()
	for _, s := range substrs {
		assert.Contains(t, got, s, "log missing substring %q", s)
	}
}

func assertLogNotContains(
	t *testing.T, buf *bytes.Buffer, substrs ...string,
) {
	t.Helper()
	got := buf.String()
	for _, s := range substrs {
		assert.NotContains(t, got, s, "log should not contain substring %q", s)
	}
}

func assertLogEmpty(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	assert.Zero(t, buf.Len(), "expected no log output, got: %q", buf.String())
}

func assertToolCallField(t *testing.T, i int, field, got, want string) {
	t.Helper()
	assert.Equal(t, want, got, fmt.Sprintf("tool_calls[%d].%s", i, field))
}

func assertToolCall(t *testing.T, i int, got, want ParsedToolCall) {
	t.Helper()
	assertToolCallField(t, i, "ToolName", got.ToolName, want.ToolName)
	assertToolCallField(t, i, "Category", got.Category, want.Category)
	if want.ToolUseID != "" {
		assertToolCallField(t, i, "ToolUseID", got.ToolUseID, want.ToolUseID)
	}
	if want.InputJSON != "" {
		assertToolCallField(t, i, "InputJSON", got.InputJSON, want.InputJSON)
	}
	if want.SkillName != "" {
		assertToolCallField(t, i, "SkillName", got.SkillName, want.SkillName)
	}
	assertToolCallField(t, i, "SubagentSessionID", got.SubagentSessionID, want.SubagentSessionID)
}

func assertToolCalls(
	t *testing.T, got, want []ParsedToolCall,
) {
	t.Helper()
	if !assert.Equal(t, len(want), len(got), "tool calls count") {
		return
	}
	for i := range want {
		assertToolCall(t, i, got[i], want[i])
	}
}

func parseClaudeTestFile(
	t *testing.T, name, content, project string,
) (ParsedSession, []ParsedMessage) {
	t.Helper()
	path := createTestFile(t, name, content)
	results, err := parseClaudeSession(
		path, project, "local",
	)
	require.NoError(t, err, "parseClaudeSession")
	require.NotEmpty(t, results, "parseClaudeSession returned no results")
	return results[0].Session, results[0].Messages
}

// parseClaudeSession parses a standalone Claude transcript through the Claude
// provider's upload entry point, honoring the explicit project. It is the
// test harness replacement for the former ParseClaudeSession free function,
// exercising the same provider-owned parse body that production uploads use.
func parseClaudeSession(
	path, project, machine string,
) ([]ParseResult, error) {
	provider, ok := NewProvider(AgentClaude, ProviderConfig{Machine: machine})
	if !ok {
		return nil, fmt.Errorf("claude provider unavailable")
	}
	uploader, ok := provider.(ClaudeUploadParser)
	if !ok {
		return nil, fmt.Errorf("claude provider does not support upload parsing")
	}
	return uploader.ParseUploadedTranscript(path, project, machine)
}
