package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const cortexTestUUID = "11111111-2222-3333-4444-555555555555"

// minimalCortexSession returns a valid Cortex session JSON with the
// given session_id, a user message, and an assistant reply.
func minimalCortexSession(sessionID string) string {
	return `{
	"session_id": "` + sessionID + `",
	"title": "Test session",
	"working_directory": "/home/user/project",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z",
	"history": [
		{
			"role": "user",
			"id": "msg1",
			"content": [{"type": "text", "text": "Hello Cortex"}]
		},
		{
			"role": "assistant",
			"id": "msg2",
			"content": [{"type": "text", "text": "Hi there!"}]
		}
	]
}`
}

func TestParseCortexSession_Basic(t *testing.T) {
	content := minimalCortexSession(cortexTestUUID)
	path := createTestFile(t, cortexTestUUID+".json", content)

	sess, msgs, err := ParseCortexSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"cortex:"+cortexTestUUID, "project", AgentCortex,
	)
	assert.Equal(t, "Hello Cortex", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "Hello Cortex")
	assertMessage(t, msgs[1], RoleAssistant, "Hi there!")
}

func TestParseCortexSession_EmptySessionID(t *testing.T) {
	content := `{"session_id": "", "history": []}`
	path := createTestFile(t, "empty.json", content)

	sess, msgs, err := ParseCortexSession(path, "local")
	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseCortexSession_SkipsInternalBlocks(t *testing.T) {
	content := `{
	"session_id": "` + cortexTestUUID + `",
	"working_directory": "/tmp",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z",
	"history": [
		{
			"role": "user", "id": "m1",
			"content": [
				{"type": "text", "text": "<system-reminder>internal</system-reminder>"},
				{"type": "text", "text": "Real question"}
			]
		},
		{
			"role": "assistant", "id": "m2",
			"content": [
				{"type": "text", "text": "Answer", "internalOnly": false},
				{"type": "text", "text": "Secret", "internalOnly": true}
			]
		}
	]
}`

	path := createTestFile(t, cortexTestUUID+".json", content)
	sess, msgs, err := ParseCortexSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	require.Len(t, msgs, 2)
	assert.Equal(t, "Real question", msgs[0].Content)
	assert.Equal(t, "Answer", msgs[1].Content)
}

func TestParseCortexSession_ToolUse(t *testing.T) {
	content := `{
	"session_id": "` + cortexTestUUID + `",
	"working_directory": "/tmp",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z",
	"history": [
		{
			"role": "user", "id": "m1",
			"content": [{"type": "text", "text": "Read main.go"}]
		},
		{
			"role": "assistant", "id": "m2",
			"content": [
				{"type": "tool_use", "tool_use": {
					"tool_use_id": "tu1",
					"name": "read",
					"input": {"file_path": "/tmp/main.go"}
				}}
			]
		},
		{
			"role": "user", "id": "m3",
			"content": [
				{"type": "tool_result", "tool_result": {
					"tool_use_id": "tu1",
					"name": "read",
					"content": [{"type": "text", "text": "package main"}],
					"status": "success"
				}}
			]
		}
	]
}`

	path := createTestFile(t, cortexTestUUID+".json", content)
	sess, msgs, err := ParseCortexSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 3)
	require.Len(t, msgs, 3)
	assert.True(t, msgs[1].HasToolUse)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read", msgs[1].ToolCalls[0].ToolName)
	assert.Contains(t, msgs[1].Content, "/tmp/main.go")

	// Tool result message carries ContentLength > 0.
	require.Len(t, msgs[2].ToolResults, 1)
	assert.Equal(t, "tu1", msgs[2].ToolResults[0].ToolUseID)
	assert.Greater(t, msgs[2].ToolResults[0].ContentLength, 0,
		"tool result ContentLength must be populated")
}

func TestParseCortexSession_SplitHistoryJSONL(t *testing.T) {
	dir := t.TempDir()
	uuid := cortexTestUUID

	// Metadata file with no embedded history.
	meta := `{
	"session_id": "` + uuid + `",
	"working_directory": "/tmp",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z"
}`
	metaPath := filepath.Join(dir, uuid+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(meta), 0o644))

	// Companion JSONL file.
	lines := strings.Join([]string{
		`{"role":"user","id":"m1","content":[{"type":"text","text":"Hello from JSONL"}]}`,
		`{"role":"assistant","id":"m2","content":[{"type":"text","text":"Got it"}]}`,
	}, "\n")
	histPath := filepath.Join(dir, uuid+".history.jsonl")
	require.NoError(t, os.WriteFile(histPath, []byte(lines), 0o644))

	sess, msgs, err := ParseCortexSession(metaPath, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertMessageCount(t, sess.MessageCount, 2)
	require.Len(t, msgs, 2)
	assertMessage(t, msgs[0], RoleUser, "Hello from JSONL")
	assertMessage(t, msgs[1], RoleAssistant, "Got it")
}

func TestParseCortexSession_SplitHistoryReadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0o000 does not prevent reads on Windows")
	}
	dir := t.TempDir()
	uuid := cortexTestUUID

	// Metadata file with no embedded history.
	meta := `{
	"session_id": "` + uuid + `",
	"working_directory": "/tmp",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z"
}`
	metaPath := filepath.Join(dir, uuid+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(meta), 0o644))

	// Create the history file but make it unreadable.
	histPath := filepath.Join(dir, uuid+".history.jsonl")
	require.NoError(t, os.WriteFile(histPath, []byte("{}"), 0o644))
	require.NoError(t, os.Chmod(histPath, 0o000))
	t.Cleanup(func() { os.Chmod(histPath, 0o644) })

	_, _, err := ParseCortexSession(metaPath, "local")
	require.Error(t, err, "non-ENOENT read error should propagate")
	assert.Contains(t, err.Error(), "read history")
}

func TestParseCortexSession_SplitHistoryMissing(t *testing.T) {
	dir := t.TempDir()
	uuid := cortexTestUUID

	// Metadata file with no embedded history, no JSONL companion.
	meta := `{
	"session_id": "` + uuid + `",
	"working_directory": "/tmp",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z"
}`
	metaPath := filepath.Join(dir, uuid+".json")
	require.NoError(t, os.WriteFile(metaPath, []byte(meta), 0o644))

	sess, msgs, err := ParseCortexSession(metaPath, "local")
	require.NoError(t, err)
	assert.Nil(t, sess, "missing JSONL should silently skip")
	assert.Nil(t, msgs)
}

func TestParseCortexSession_FirstUserTurnSystemOnly(t *testing.T) {
	content := `{
	"session_id": "` + cortexTestUUID + `",
	"working_directory": "/tmp",
	"created_at": "2024-06-01T10:00:00Z",
	"last_updated": "2024-06-01T10:05:00Z",
	"history": [
		{
			"role": "user", "id": "m1",
			"content": [
				{"type": "text", "text": "<system-reminder>setup</system-reminder>"}
			]
		},
		{
			"role": "user", "id": "m2",
			"content": [{"type": "text", "text": "Real prompt"}]
		},
		{
			"role": "assistant", "id": "m3",
			"content": [{"type": "text", "text": "OK"}]
		}
	]
}`

	path := createTestFile(t, cortexTestUUID+".json", content)
	sess, msgs, err := ParseCortexSession(path, "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// First system-only turn skipped.
	assertMessageCount(t, sess.MessageCount, 2)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Real prompt", sess.FirstMessage)
	assertMessage(t, msgs[0], RoleUser, "Real prompt")
}

// --- Discovery and file-finding ---

func TestIsCortexSessionFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{cortexTestUUID + ".json", true},
		{cortexTestUUID + ".back.12345.json", false},
		{cortexTestUUID + ".history.jsonl", false},
		{"has spaces.json", false},
		{"", false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, IsCortexSessionFile(tt.name),
			"IsCortexSessionFile(%q)", tt.name)
	}
}

func TestIsCortexBackupFile(t *testing.T) {
	assert.True(t, IsCortexBackupFile(
		cortexTestUUID+".back.1234567890.json"))
	assert.False(t, IsCortexBackupFile(cortexTestUUID+".json"))
}

func TestDiscoverCortexSessions(t *testing.T) {
	dir := t.TempDir()
	uuid2 := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Valid session files.
	for _, name := range []string{
		cortexTestUUID + ".json",
		uuid2 + ".json",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, name), []byte("{}"), 0o644))
	}
	// Files that should be skipped.
	for _, name := range []string{
		cortexTestUUID + ".back.999.json",
		cortexTestUUID + ".history.jsonl",
		"readme.txt",
	} {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, name), []byte(""), 0o644))
	}

	files := DiscoverCortexSessions(dir)
	require.Len(t, files, 2)
	for _, f := range files {
		assert.Equal(t, AgentCortex, f.Agent)
	}
}

func TestDiscoverCortexSessions_EmptyDir(t *testing.T) {
	assert.Nil(t, DiscoverCortexSessions(""))
	assert.Nil(t, DiscoverCortexSessions("/nonexistent"))
}

func TestFindCortexSourceFile(t *testing.T) {
	dir := t.TempDir()
	fpath := filepath.Join(dir, cortexTestUUID+".json")
	require.NoError(t, os.WriteFile(fpath, []byte("{}"), 0o644))

	tests := []struct {
		name      string
		dir       string
		sessionID string
		want      string
	}{
		{"raw UUID", dir, cortexTestUUID, fpath},
		{"prefixed ID", dir, "cortex:" + cortexTestUUID, fpath},
		{"missing file", dir, "00000000-0000-0000-0000-000000000000", ""},
		{"empty dir", "", cortexTestUUID, ""},
		{"invalid ID", dir, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FindCortexSourceFile(tt.dir, tt.sessionID)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- Tool detail formatting ---

func TestCortexToolDetail(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{
			"bash command",
			"bash",
			`{"command": "ls -la\necho done"}`,
			"$ ls -la",
		},
		{
			"read file",
			"read",
			`{"file_path": "/tmp/main.go"}`,
			"/tmp/main.go",
		},
		{
			"grep pattern",
			"grep",
			`{"pattern": "TODO"}`,
			"TODO",
		},
		{
			"unknown tool",
			"unknown",
			`{"foo": "bar"}`,
			"unknown",
		},
		{
			"non-JSON input",
			"bash",
			`not json`,
			"bash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cortexToolDetail(tt.tool, tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestExtractCortexProject(t *testing.T) {
	tests := []struct {
		name string
		meta cortexSessionJSON
		want string
	}{
		{
			"from working_directory",
			cortexSessionJSON{WorkingDirectory: "/home/user/myproject"},
			"myproject",
		},
		{
			"from git_root",
			cortexSessionJSON{GitRoot: "/home/user/repo"},
			"repo",
		},
		{
			"unknown when empty",
			cortexSessionJSON{},
			"unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, extractCortexProject(tt.meta))
		})
	}
}
