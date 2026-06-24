package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeReasonixJSONL writes JSONL lines to a temp file and
// returns the file path.
func writeReasonixJSONL(
	t *testing.T, lines ...string,
) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-session.jsonl")
	content := strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(
		path, []byte(content), 0o644,
	))
	return path
}

// writeReasonixMetadata writes a .jsonl.meta sidecar file.
func writeReasonixMetadata(
	t *testing.T, transcriptPath string, meta reasonixMetadata,
) {
	t.Helper()
	metaPath := transcriptPath + ".meta"
	data, err := json.Marshal(meta)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(metaPath, data, 0o644))
}

func TestParseReasonixSession_Basic(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Write a simple function"}`,
		`{"role":"assistant","content":"Here's a function","reasoning_content":"I need to write a function"}`,
	)

	sess, msgs, _, err := parseReasonixSession(path, "test-machine")
	require.NoError(t, err)
	require.NotNil(t, sess, "expected non-nil session")
	require.Len(t, msgs, 2)

	assert.Equal(t, AgentReasonix, sess.Agent, "agent")
	assert.Equal(t, "test-machine", sess.Machine, "machine")
	assert.Equal(t, "Write a simple function", sess.FirstMessage, "first_message")
	assert.True(t, strings.Contains(sess.ID, "reasonix:"), "session ID prefix")

	// Check message roles
	assert.Equal(t, RoleUser, msgs[0].Role, "msgs[0].Role")
	assert.Equal(t, RoleAssistant, msgs[1].Role, "msgs[1].Role")

	// Check that reasoning content is included in display content
	assert.True(t, strings.Contains(msgs[1].Content, "[Thinking]"), "thinking block in content")
	assert.True(t, msgs[1].HasThinking, "HasThinking flag")
}

func TestParseReasonixSession_ToolCalls(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Read the file"}`,
		`{"role":"assistant","content":"I'll read it","tool_calls":[{"id":"call_1","name":"read_file","arguments":"{\"path\":\"config.json\"}"}]}`,
	)

	_, msgs, _, err := parseReasonixSession(path, "m")
	require.NoError(t, err)
	require.Len(t, msgs, 2)

	// Check tool call message
	tcMsg := msgs[1]
	assert.True(t, tcMsg.HasToolUse, "expected HasToolUse")
	require.Len(t, tcMsg.ToolCalls, 1)
	assert.Equal(t, "call_1", tcMsg.ToolCalls[0].ToolUseID)
	assert.Equal(t, "read_file", tcMsg.ToolCalls[0].ToolName)
	assert.Equal(t, `{"path":"config.json"}`, tcMsg.ToolCalls[0].InputJSON)
}

func TestParseReasonixSession_ToolResults(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Read the file"}`,
		`{"role":"assistant","content":"I'll read it","tool_calls":[{"id":"call_1","name":"read_file","arguments":"{\"path\":\"config.json\"}"}]}`,
		`{"role":"tool","content":"file contents here","tool_call_id":"call_1"}`,
	)

	_, msgs, _, err := parseReasonixSession(path, "m")
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	resultMsg := msgs[2]
	require.Len(t, resultMsg.ToolResults, 1)
	assert.Equal(t, RoleUser, resultMsg.Role)
	assert.Equal(t, "", resultMsg.Content)
	assert.Equal(t, "call_1", resultMsg.ToolResults[0].ToolUseID)
	assert.Equal(t, len("file contents here"), resultMsg.ToolResults[0].ContentLength)
	assert.Equal(t, "file contents here", DecodeContent(resultMsg.ToolResults[0].ContentRaw))
}

func TestParseReasonixSession_TimestampSessionID(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Hello"}`,
	)

	// Rename to match timestamp-based format
	dir := filepath.Dir(path)
	timestampPath := filepath.Join(dir, "20260617-081849.643965200-deepseek-v4-pro.jsonl")
	require.NoError(t, os.Rename(path, timestampPath))

	sess, _, _, err := parseReasonixSession(timestampPath, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "reasonix:20260617-081849.643965200-deepseek-v4-pro", sess.ID)
}

func TestParseReasonixSession_SubagentID(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Subagent task"}`,
	)

	// Rename to match subagent format
	dir := filepath.Dir(path)
	subagentPath := filepath.Join(dir, "sa_20260612_105316_000000000_6b991b514f0a.jsonl")
	require.NoError(t, os.Rename(path, subagentPath))

	sess, _, _, err := parseReasonixSession(subagentPath, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "reasonix:sa_20260612_105316_000000000_6b991b514f0a", sess.ID)
}

func TestParseReasonixSession_SpaceInSessionDir(t *testing.T) {
	baseDir := t.TempDir()
	// Create directory with space in name
	sessionDir := filepath.Join(baseDir, "session dir with spaces")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	path := filepath.Join(sessionDir, "20260617-081849.643965200-gpt4.jsonl")
	content := `{"role":"user","content":"Test message"}`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, _, _, err := parseReasonixSession(path, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	// Should extract just the filename, not the directory
	assert.Equal(t, "reasonix:20260617-081849.643965200-gpt4", sess.ID)
}

func TestParseReasonixSession_MetadataFallback(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Test"}`,
		`{"role":"assistant","content":"Response"}`,
	)

	// Write metadata sidecar
	meta := reasonixMetadata{
		ID:            "test-session-id",
		CreatedAt:     "2026-06-12T10:42:35.2672024Z",
		UpdatedAt:     "2026-06-12T10:58:03.6456434Z",
		TopicTitle:    "Test Session",
		WorkspaceRoot: "/home/user/project",
		Model:         "claude-opus-4",
	}
	writeReasonixMetadata(t, path, meta)

	sess, _, _, err := parseReasonixSession(path, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)

	// Parse expected times from metadata
	expectedStart, err := time.Parse(time.RFC3339Nano, meta.CreatedAt)
	require.NoError(t, err)
	expectedEnd, err := time.Parse(time.RFC3339Nano, meta.UpdatedAt)
	require.NoError(t, err)

	// Verify metadata timestamps were preserved (not overwritten by time.Now())
	assert.Equal(t, expectedStart, sess.StartedAt, "StartedAt should match CreatedAt from metadata")
	assert.Equal(t, expectedEnd, sess.EndedAt, "EndedAt should match UpdatedAt from metadata")
}

func TestParseReasonixSession_MetadataFields(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Test"}`,
		`{"role":"assistant","content":"Response"}`,
	)
	workspaceRoot := filepath.Join("workspace", "my-app")

	meta := reasonixMetadata{
		CreatedAt:     "2026-06-12T10:42:35.2672024Z",
		UpdatedAt:     "2026-06-12T10:58:03.6456434Z",
		TopicTitle:    "Metadata title",
		WorkspaceRoot: workspaceRoot,
		Model:         "claude-opus-4",
	}
	writeReasonixMetadata(t, path, meta)

	sess, _, _, err := parseReasonixSession(path, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "Metadata title", sess.SessionName)
	assert.Equal(t, workspaceRoot, sess.Cwd)
	assert.Equal(t, "my_app", sess.Project)
}

func TestParseReasonixSession_PartialMetadataFallsBackToFileMtime(t *testing.T) {
	tests := []struct {
		name string
		meta reasonixMetadata
	}{
		{
			name: "missing updated_at",
			meta: reasonixMetadata{
				CreatedAt: "2026-06-12T10:42:35.2672024Z",
			},
		},
		{
			name: "malformed created_at",
			meta: reasonixMetadata{
				CreatedAt: "not-a-timestamp",
				UpdatedAt: "2026-06-12T10:58:03.6456434Z",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeReasonixJSONL(t,
				`{"role":"user","content":"Test"}`,
				`{"role":"assistant","content":"Response"}`,
			)
			wantMtime := time.Date(2024, time.March, 14, 15, 9, 26, 0, time.UTC)
			require.NoError(t, os.Chtimes(path, wantMtime, wantMtime))
			info, err := os.Stat(path)
			require.NoError(t, err)
			writeReasonixMetadata(t, path, tt.meta)

			sess, _, _, err := parseReasonixSession(path, "m")
			require.NoError(t, err)
			require.NotNil(t, sess)
			assert.Equal(t, info.ModTime(), sess.StartedAt)
			assert.Equal(t, info.ModTime(), sess.EndedAt)
		})
	}
}

func TestParseReasonixSession_ArchiveWithoutMeta(t *testing.T) {
	path := writeReasonixJSONL(t,
		`{"role":"user","content":"Archived message"}`,
		`{"role":"assistant","content":"Archived response"}`,
	)

	// Don't write metadata sidecar - archive files often lack .meta

	sess, msgs, _, err := parseReasonixSession(path, "m")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)

	// Should still parse successfully with fallback times
	assert.NotZero(t, sess.StartedAt)
	assert.NotZero(t, sess.EndedAt)
}

func TestDiscoverReasonixSessions_ProjectSessions(t *testing.T) {
	baseDir := t.TempDir()

	// Create project session structure
	projectDir := filepath.Join(baseDir, "projects", "my-project", "sessions")
	sessionDir := filepath.Join(projectDir, "session-123")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	sessionFile := filepath.Join(sessionDir, "session-123.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(`{"role":"user","content":"test"}`), 0o644))

	files := discoverReasonixSessions(baseDir)
	require.Len(t, files, 1)
	assert.Equal(t, sessionFile, files[0].Path)
	assert.Equal(t, "my-project", files[0].Project)
	assert.Equal(t, AgentReasonix, files[0].Agent)
}

func TestDiscoverReasonixSessions_ProjectBareSession(t *testing.T) {
	baseDir := t.TempDir()

	sessionsDir := filepath.Join(baseDir, "projects", "my-project", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	sessionFile := filepath.Join(sessionsDir, "session-123.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(`{"role":"user","content":"test"}`), 0o644))

	files := discoverReasonixSessions(baseDir)
	require.Len(t, files, 1)
	assert.Equal(t, sessionFile, files[0].Path)
	assert.Equal(t, "my-project", files[0].Project)
	assert.Equal(t, AgentReasonix, files[0].Agent)
}

func TestDiscoverReasonixSessions_GlobalSessions(t *testing.T) {
	baseDir := t.TempDir()

	// Create global session (bare .jsonl)
	sessionsDir := filepath.Join(baseDir, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	sessionFile := filepath.Join(sessionsDir, "global-session.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(`{"role":"user","content":"test"}`), 0o644))

	files := discoverReasonixSessions(baseDir)
	require.Len(t, files, 1)
	assert.Equal(t, sessionFile, files[0].Path)
	assert.Equal(t, AgentReasonix, files[0].Agent)
}

func TestDiscoverReasonixSessions_Subagents(t *testing.T) {
	baseDir := t.TempDir()

	// Create subagent session
	subagentsDir := filepath.Join(baseDir, "sessions", "subagents")
	require.NoError(t, os.MkdirAll(subagentsDir, 0o755))

	subagentFile := filepath.Join(subagentsDir, "sa_20260612_105316_000000000_hash.jsonl")
	require.NoError(t, os.WriteFile(subagentFile, []byte(`{"role":"user","content":"test"}`), 0o644))

	files := discoverReasonixSessions(baseDir)
	require.Len(t, files, 1)
	assert.Equal(t, subagentFile, files[0].Path)
}

func TestDiscoverReasonixSessions_Archive(t *testing.T) {
	baseDir := t.TempDir()

	// Create archive session
	archiveDir := filepath.Join(baseDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	archiveFile := filepath.Join(archiveDir, "20260612-104235.267202400.jsonl")
	require.NoError(t, os.WriteFile(archiveFile, []byte(`{"role":"user","content":"test"}`), 0o644))

	files := discoverReasonixSessions(baseDir)
	require.Len(t, files, 1)
	assert.Equal(t, archiveFile, files[0].Path)
}

func TestFindReasonixSourceFile_ProjectSession(t *testing.T) {
	baseDir := t.TempDir()

	// Create project session
	projectDir := filepath.Join(baseDir, "projects", "my-project", "sessions")
	sessionDir := filepath.Join(projectDir, "test-id")
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))

	sessionFile := filepath.Join(sessionDir, "test-id.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(""), 0o644))

	found := findReasonixSourceFile(baseDir, "test-id")
	assert.Equal(t, sessionFile, found)
}

func TestFindReasonixSourceFile_ProjectBareSession(t *testing.T) {
	baseDir := t.TempDir()

	sessionsDir := filepath.Join(baseDir, "projects", "my-project", "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	sessionFile := filepath.Join(sessionsDir, "test-id.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(""), 0o644))

	found := findReasonixSourceFile(baseDir, "test-id")
	assert.Equal(t, sessionFile, found)
}

func TestFindReasonixSourceFile_GlobalSession(t *testing.T) {
	baseDir := t.TempDir()

	// Create global session (bare)
	sessionsDir := filepath.Join(baseDir, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	sessionFile := filepath.Join(sessionsDir, "global-id.jsonl")
	require.NoError(t, os.WriteFile(sessionFile, []byte(""), 0o644))

	found := findReasonixSourceFile(baseDir, "global-id")
	assert.Equal(t, sessionFile, found)
}

func TestFindReasonixSourceFile_Archive(t *testing.T) {
	baseDir := t.TempDir()

	// Create archive session
	archiveDir := filepath.Join(baseDir, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	archiveFile := filepath.Join(archiveDir, "archive-id.jsonl")
	require.NoError(t, os.WriteFile(archiveFile, []byte(""), 0o644))

	found := findReasonixSourceFile(baseDir, "archive-id")
	assert.Equal(t, archiveFile, found)
}
