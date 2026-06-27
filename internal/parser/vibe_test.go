package parser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newVibeTestProvider builds a Vibe provider for the given roots so package
// tests can exercise discovery through the Provider interface.
func newVibeTestProvider(t *testing.T, roots ...string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentVibe, ProviderConfig{
		Roots:   roots,
		Machine: "local",
	})
	require.True(t, ok)
	return provider
}

// parseVibeTestSession parses a Vibe messages.jsonl file at path into a
// ParseResult through the folded free function, replacing the removed
// package-level ParseVibeSession entrypoint.
func parseVibeTestSession(t *testing.T, path string, fileInfo FileInfo) (ParseResult, error) {
	t.Helper()
	return parseVibeResultFile(path, fileInfo)
}

// discoverVibeTestSessions discovers Vibe sessions under root through the
// provider, returning the legacy DiscoveredFile shape (path + project) the
// tests assert against.
func discoverVibeTestSessions(t *testing.T, root string) []DiscoveredFile {
	t.Helper()
	provider := newVibeTestProvider(t, root)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	if len(sources) == 0 {
		return nil
	}
	files := make([]DiscoveredFile, 0, len(sources))
	for _, source := range sources {
		files = append(files, DiscoveredFile{
			Path:    source.DisplayPath,
			Project: source.ProjectHint,
			Agent:   AgentVibe,
		})
	}
	return files
}

// findVibeTestSourceFile resolves a Vibe session ID to a messages.jsonl path,
// replacing the removed FindVibeSourceFile.
func findVibeTestSourceFile(t *testing.T, root, sessionID string) string {
	t.Helper()
	return findVibeSourceFile(root, sessionID)
}

func TestDiscoverVibeSessions(t *testing.T) {
	tmpDir := t.TempDir()

	// Create file system structure
	files := map[string]string{
		"session_20260613_123456_abc123def/messages.jsonl": "test",
	}
	setupFileSystem(t, tmpDir, files)

	// Create invalid directory (no messages.jsonl)
	invalidDir := filepath.Join(tmpDir, "session_invalid")
	require.NoError(t, os.MkdirAll(invalidDir, 0755))

	// Create directory without session prefix
	otherDir := filepath.Join(tmpDir, "other_dir")
	require.NoError(t, os.MkdirAll(otherDir, 0755))

	// Run discovery
	discovered := discoverVibeTestSessions(t, tmpDir)

	// Verify results
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentVibe, discovered[0].Agent)
	assert.Equal(t, "session_20260613_123456_abc123def", discovered[0].Project)
}

func TestDiscoverVibeSessionsMultiple(t *testing.T) {
	tmpDir := t.TempDir()

	// Create file system structure with multiple sessions
	files := map[string]string{
		"session_20260613_100000_aaa/messages.jsonl": "test",
		"session_20260613_110000_bbb/messages.jsonl": "test",
		"session_20260613_120000_ccc/messages.jsonl": "test",
	}
	setupFileSystem(t, tmpDir, files)

	// Create a directory without messages.jsonl
	invalidDir := filepath.Join(tmpDir, "session_20260613_130000_ddd")
	require.NoError(t, os.MkdirAll(invalidDir, 0755))

	// Run discovery
	discovered := discoverVibeTestSessions(t, tmpDir)

	// Verify results - should find only 3 valid sessions
	require.Len(t, discovered, 3)
	for i, f := range discovered {
		assert.Equal(t, AgentVibe, f.Agent)
		assert.Contains(t, f.Path, "session_20260613_")
		assert.Contains(t, f.Path, "messages.jsonl")
		_ = i // avoid unused variable
	}
}

func TestDiscoverVibeSessionsEmptyDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Run discovery on empty directory
	files := discoverVibeTestSessions(t, tmpDir)

	// Should return empty slice
	assert.Len(t, files, 0)
}

func TestDiscoverVibeSessionsNonExistentDir(t *testing.T) {
	// Run discovery on non-existent directory
	files := discoverVibeTestSessions(t, "/nonexistent/path")

	// Should return empty slice without error
	assert.Len(t, files, 0)
}

func TestFindVibeSourceFile(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_20260613_123456_abc123def"
	setupFileSystem(t, root, map[string]string{
		filepath.Join(sessionID, "messages.jsonl"): "test",
	})

	// When the ID matches the directory name (no meta.json), the file is
	// resolved directly.
	result := findVibeTestSourceFile(t, root, sessionID)
	expected := filepath.Join(root, sessionID, "messages.jsonl")
	assert.Equal(t, expected, result)
}

func TestFindVibeSourceFileWithSpecialChars(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_20260613_123456_abc-123-def"
	setupFileSystem(t, root, map[string]string{
		filepath.Join(sessionID, "messages.jsonl"): "test",
	})

	result := findVibeTestSourceFile(t, root, sessionID)
	expected := filepath.Join(root, sessionID, "messages.jsonl")
	assert.Equal(t, expected, result)
}

func TestFindVibeSourceFileByMetaSessionID(t *testing.T) {
	root := t.TempDir()
	dirName := "session_20260613_123456_abc123def"
	setupFileSystem(t, root, map[string]string{
		filepath.Join(dirName, "messages.jsonl"): "test",
		filepath.Join(dirName, "meta.json"):      `{"session_id": "uuid-1234"}`,
	})

	// The canonical ID is the meta.json session_id, which differs from the
	// directory name; the lookup must scan meta.json to resolve it.
	result := findVibeTestSourceFile(t, root, "uuid-1234")
	expected := filepath.Join(root, dirName, "messages.jsonl")
	assert.Equal(t, expected, result)

	// An unknown ID resolves to nothing.
	assert.Empty(t, findVibeTestSourceFile(t, root, "does-not-exist"))
}

func TestParseVibeSession(t *testing.T) {
	path := "testdata/vibe/session_basic/messages.jsonl"
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Verify session metadata
	assert.Equal(t, AgentVibe, result.Session.Agent)
	assert.NotEmpty(t, result.Session.ID)
	assert.True(t, len(result.Session.ID) > 0)

	// Verify messages
	require.NotEmpty(t, result.Messages)
	assert.Equal(t, 5, len(result.Messages))

	// Verify first message is user
	assert.Equal(t, RoleUser, result.Messages[0].Role)
	assert.Equal(t, "Create a Python function to sort a list", result.Messages[0].Content)

	// Verify last message is assistant
	assert.Equal(t, RoleAssistant, result.Messages[4].Role)

	// Verify session metadata from meta.json
	assert.Equal(t, "vibe:abc123def-0000-0000-0000-000000000000", result.Session.ID)
	assert.Equal(t, "abc123def-0000-0000-0000-000000000000", result.Session.SourceSessionID)
	assert.Equal(t, "Create a Python function to sort a list", result.Session.SessionName)
	assert.Equal(t, "/home/user/projects/myapp", result.Session.Cwd)
	// Project is derived from the working directory (basename here, since
	// the path is not a real git repo), not the cryptic session directory.
	assert.Equal(t, "myapp", result.Session.Project)
	assert.Equal(t, "main", result.Session.GitBranch)
	assert.Equal(t, "abc123def456", result.Session.SourceVersion)
	assert.True(t, result.Session.HasTotalOutputTokens)
	assert.Equal(t, 50, result.Session.TotalOutputTokens)
	assert.True(t, result.Session.HasPeakContextTokens)
	assert.Equal(t, 150, result.Session.PeakContextTokens)

	// Verify usage events are created from session stats
	require.Len(t, result.UsageEvents, 1)
	usageEvent := result.UsageEvents[0]
	assert.Equal(t, "vibe:abc123def-0000-0000-0000-000000000000", usageEvent.SessionID)
	assert.Equal(t, "session", usageEvent.Source)
	assert.Equal(t, "mistral-medium-3.5", usageEvent.Model)
	assert.Equal(t, 100, usageEvent.InputTokens)
	assert.Equal(t, 50, usageEvent.OutputTokens) // Uses session_completion_tokens
	assert.Equal(t, 0, usageEvent.CacheCreationInputTokens)
	assert.Equal(t, 0, usageEvent.CacheReadInputTokens)
	assert.Equal(t, 0, usageEvent.ReasoningTokens)
	assert.Nil(t, usageEvent.CostUSD)
	assert.Equal(t, "", usageEvent.CostStatus)
	assert.Equal(t, "", usageEvent.CostSource)
	assert.Equal(t, "session:vibe:abc123def-0000-0000-0000-000000000000", usageEvent.DedupKey)
}

func TestParseVibeSessionWithTools(t *testing.T) {
	path := "testdata/vibe/session_with_tools/messages.jsonl"
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Verify messages
	require.NotEmpty(t, result.Messages)
	assert.Equal(t, 4, len(result.Messages))

	// Verify tool calls were parsed
	hasToolCalls := false
	for _, msg := range result.Messages {
		if len(msg.ToolCalls) > 0 {
			hasToolCalls = true
			assert.Equal(t, 1, len(msg.ToolCalls))
			assert.Equal(t, "call_001", msg.ToolCalls[0].ToolUseID)
			assert.Equal(t, "read_file", msg.ToolCalls[0].ToolName)
			assert.Equal(t, "Read", msg.ToolCalls[0].Category)
			break
		}
	}
	assert.True(t, hasToolCalls, "Expected tool calls to be parsed")

	// Verify tool results are carried in a dedicated empty RoleUser
	// message (matching the Hermes/QClaw/OpenClaw convention) rather
	// than duplicated as a separate visible "tool" message.
	hasToolResults := false
	for _, msg := range result.Messages {
		if len(msg.ToolResults) > 0 {
			hasToolResults = true
			assert.Equal(t, RoleUser, msg.Role)
			assert.Equal(t, "", msg.Content)
			assert.Equal(t, 1, len(msg.ToolResults))
			assert.Equal(t, "call_001", msg.ToolResults[0].ToolUseID)

			// ContentRaw must be valid JSON (a quoted string) so
			// DecodeContent can surface the plain-text tool output.
			var decoded string
			require.NoError(
				t, json.Unmarshal(
					[]byte(msg.ToolResults[0].ContentRaw), &decoded,
				),
			)
			assert.Contains(t, decoded, "# My Project")
			break
		}
	}
	assert.True(t, hasToolResults, "Expected tool results to be linked")

	for _, msg := range result.Messages {
		assert.NotEqual(
			t, RoleType("tool"), msg.Role,
			"raw tool-result records must not appear as standalone messages",
		)
	}
}

func TestParseVibeSessionEmpty(t *testing.T) {
	path := "testdata/vibe/session_empty/messages.jsonl"
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Empty file should have no messages
	assert.Len(t, result.Messages, 0)
	assert.Equal(t, 0, result.Session.MessageCount)
}

func TestParseVibeSessionMalformedLines(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a messages.jsonl with malformed JSON
	content := `{"role": "user", "content": "valid", "message_id": "1"}
{this is not valid json}
{"role": "assistant", "content": "another valid", "message_id": "2"}
`
	files := map[string]string{
		"session_test/messages.jsonl": content,
	}
	setupFileSystem(t, tmpDir, files)

	path := filepath.Join(tmpDir, "session_test", "messages.jsonl")
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Should have parsed 2 valid messages and counted 1 malformed line
	assert.Len(t, result.Messages, 2)
	assert.Equal(t, 1, result.Session.MalformedLines)
}

func TestParseVibeSessionWithoutMeta(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a messages.jsonl without meta.json in a session subdirectory
	content := `{"role": "user", "content": "test message", "message_id": "1"}
{"role": "assistant", "content": "test response", "message_id": "2"}
`
	files := map[string]string{
		"session_test/messages.jsonl": content,
	}
	setupFileSystem(t, tmpDir, files)

	path := filepath.Join(tmpDir, "session_test", "messages.jsonl")
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Should have parsed messages but no metadata from meta.json. The ID
	// falls back to the directory name but still carries the "vibe:" prefix
	// so prefix-based routing works.
	assert.Len(t, result.Messages, 2)
	assert.Equal(t, "vibe:session_test", result.Session.ID)
	assert.Equal(t, "vibe", result.Session.Project) // Vibe sessions use "vibe" as project

	// Should have no usage events since there's no meta.json with stats
	assert.Empty(t, result.UsageEvents)
}

func TestParseVibeSessionEmptyStats(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a messages.jsonl with meta.json that has empty stats
	content := `{"role": "user", "content": "test message", "message_id": "1"}
{"role": "assistant", "content": "test response", "message_id": "2"}
`
	metaContent := `{
		"session_id": "test-session-123",
		"start_time": "2026-06-13T10:00:00Z",
		"end_time": "2026-06-13T10:05:00Z",
		"model": "mistral-medium-3.5",
		"title": "Test session",
		"stats": {
			"steps": 0,
			"session_prompt_tokens": 0,
			"session_completion_tokens": 0,
			"context_tokens": 0,
			"last_turn_prompt_tokens": 0,
			"last_turn_completion_tokens": 0,
			"session_total_llm_tokens": 0,
			"last_turn_total_tokens": 0
		}
	}
`
	files := map[string]string{
		"session_test/messages.jsonl": content,
		"session_test/meta.json":      metaContent,
	}
	setupFileSystem(t, tmpDir, files)

	path := filepath.Join(tmpDir, "session_test", "messages.jsonl")
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Should have parsed messages and metadata but no usage events due to empty stats
	assert.Len(t, result.Messages, 2)
	assert.Equal(t, "vibe:test-session-123", result.Session.ID)
	assert.Equal(t, "Test session", result.Session.SessionName)

	// Should have no usage events since all stats are zero
	assert.Empty(t, result.UsageEvents)
}

func TestParseVibeSessionModelFromMessages(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a messages.jsonl with meta.json that has stats but no model
	content := `{"role": "user", "content": "test message", "message_id": "1"}
{"role": "assistant", "content": "test response", "model": "mistral-medium", "message_id": "2"}
`
	metaContent := `{
		"session_id": "test-session-456",
		"start_time": "2026-06-13T10:00:00Z",
		"end_time": "2026-06-13T10:05:00Z",
		"title": "Test session with model in messages",
		"stats": {
			"steps": 2,
			"session_prompt_tokens": 50,
			"session_completion_tokens": 25,
			"context_tokens": 75,
			"last_turn_prompt_tokens": 25,
			"last_turn_completion_tokens": 12,
			"session_total_llm_tokens": 75,
			"last_turn_total_tokens": 37
		}
	}
`
	files := map[string]string{
		"session_test/messages.jsonl": content,
		"session_test/meta.json":      metaContent,
	}
	setupFileSystem(t, tmpDir, files)

	path := filepath.Join(tmpDir, "session_test", "messages.jsonl")
	fileInfo := FileInfo{
		Path:  path,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	// Should have parsed messages and metadata
	assert.Len(t, result.Messages, 2)
	assert.Equal(t, "vibe:test-session-456", result.Session.ID)
	assert.Equal(t, "Test session with model in messages", result.Session.SessionName)

	// Should have usage events created with model extracted from assistant message
	require.Len(t, result.UsageEvents, 1)
	usageEvent := result.UsageEvents[0]
	assert.Equal(t, "vibe:test-session-456", usageEvent.SessionID)
	assert.Equal(t, "session", usageEvent.Source)
	assert.Equal(t, "mistral-medium", usageEvent.Model)
	assert.Equal(t, 50, usageEvent.InputTokens)
	assert.Equal(t, 25, usageEvent.OutputTokens)
}

// TestParseVibeSessionModelFromConfig verifies that the model is read from
// config.active_model when there is no top-level "model" field. Real Vibe
// meta.json files record the model only under config.active_model, so without
// this fallback no usage event is emitted and the model never reaches the
// usage view.
func TestParseVibeSessionModelFromConfig(t *testing.T) {
	tmpDir := t.TempDir()

	content := `{"role": "user", "content": "test message", "message_id": "1"}
{"role": "assistant", "content": "test response", "message_id": "2"}
`
	// No top-level "model"; model lives under config.active_model, and no
	// message carries a model either, matching real Vibe sessions.
	metaContent := `{
		"session_id": "test-session-789",
		"start_time": "2026-06-13T10:00:00Z",
		"end_time": "2026-06-13T10:05:00Z",
		"title": "Test session with model in config",
		"config": {"active_model": "mistral-medium-3.5"},
		"stats": {
			"session_prompt_tokens": 100,
			"session_completion_tokens": 40,
			"context_tokens": 140,
			"session_total_llm_tokens": 140
		}
	}
`
	files := map[string]string{
		"session_test/messages.jsonl": content,
		"session_test/meta.json":      metaContent,
	}
	setupFileSystem(t, tmpDir, files)

	path := filepath.Join(tmpDir, "session_test", "messages.jsonl")
	fileInfo := FileInfo{Path: path, Mtime: time.Now().UnixNano()}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	require.Len(t, result.UsageEvents, 1)
	usageEvent := result.UsageEvents[0]
	assert.Equal(t, "mistral-medium-3.5", usageEvent.Model)
	assert.Equal(t, 100, usageEvent.InputTokens)
	assert.Equal(t, 40, usageEvent.OutputTokens)
}

// TestParseVibeSessionInjectedUserExcluded verifies that an injected user
// record (system context) is marked system and excluded from both the first
// message and the user-message count, so it cannot masquerade as the user's
// opening prompt or inflate the count.
func TestParseVibeSessionInjectedUserExcluded(t *testing.T) {
	tmpDir := t.TempDir()

	content := `{"role": "user", "content": "<system context>", "injected": true, "message_id": "0"}
{"role": "user", "content": "real prompt", "message_id": "1"}
{"role": "assistant", "content": "ok", "message_id": "2"}
`
	setupFileSystem(t, tmpDir, map[string]string{
		"session_test/messages.jsonl": content,
	})

	path := filepath.Join(tmpDir, "session_test", "messages.jsonl")
	fileInfo := FileInfo{Path: path, Mtime: time.Now().UnixNano()}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	require.Len(t, result.Messages, 3)
	// The injected record is preserved but marked system so the UI hides it.
	assert.True(t, result.Messages[0].IsSystem, "injected record must be system")
	assert.False(t, result.Messages[1].IsSystem, "real prompt must not be system")

	// The first message and user count skip the injected record.
	assert.Equal(t, "real prompt", result.Session.FirstMessage)
	assert.Equal(t, 1, result.Session.UserMessageCount)
}

// TestParseVibeSessionToolResultNotCountedAsUser verifies the empty RoleUser
// carrier emitted for a "tool" record is not counted as a user message.
func TestParseVibeSessionToolResultNotCountedAsUser(t *testing.T) {
	path := "testdata/vibe/session_with_tools/messages.jsonl"
	fileInfo := FileInfo{Path: path, Mtime: time.Now().UnixNano()}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	assert.Equal(t, 1, result.Session.UserMessageCount)
	assert.Equal(t, "Read the README file", result.Session.FirstMessage)
}

// TestParseVibeSessionMalformedMetaRecoversID verifies that when meta.json has
// a valid session_id but a malformed optional field (a bad timestamp here that
// fails the full parse), the canonical ID is still recovered rather than
// dropping to the directory-name fallback, which would abandon the canonical
// session row.
func TestParseVibeSessionMalformedMetaRecoversID(t *testing.T) {
	tmpDir := t.TempDir()

	content := `{"role": "user", "content": "hello", "message_id": "1"}
`
	metaContent := `{"session_id": "uuid-canonical-1", "start_time": "not-a-timestamp"}`
	setupFileSystem(t, tmpDir, map[string]string{
		"session_dir/messages.jsonl": content,
		"session_dir/meta.json":      metaContent,
	})

	path := filepath.Join(tmpDir, "session_dir", "messages.jsonl")
	fileInfo := FileInfo{Path: path, Mtime: time.Now().UnixNano()}

	result, err := parseVibeTestSession(t, path, fileInfo)
	require.NoError(t, err)

	assert.Equal(t, "vibe:uuid-canonical-1", result.Session.ID)
	assert.Equal(t, "uuid-canonical-1", result.Session.SourceSessionID)
	// The malformed optional fields are skipped, so no usage event is emitted.
	assert.Empty(t, result.UsageEvents)
}

// TestParseVibeSessionCorruptMetaReturnsError verifies that a meta.json which
// is not even minimally valid JSON (a truncated/partial write) makes the parse
// fail, so the sync retries and leaves any existing canonical row untouched
// rather than silently re-creating it under the directory-name fallback.
func TestParseVibeSessionCorruptMetaReturnsError(t *testing.T) {
	tmpDir := t.TempDir()

	content := `{"role": "user", "content": "hello", "message_id": "1"}
`
	metaContent := `{"session_id": "uuid-canonical-2",`
	setupFileSystem(t, tmpDir, map[string]string{
		"session_dir/messages.jsonl": content,
		"session_dir/meta.json":      metaContent,
	})

	path := filepath.Join(tmpDir, "session_dir", "messages.jsonl")
	fileInfo := FileInfo{Path: path, Mtime: time.Now().UnixNano()}

	_, err := parseVibeTestSession(t, path, fileInfo)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "meta.json")
}

func TestVibeAgentByType(t *testing.T) {
	def, ok := AgentByType(AgentVibe)
	require.True(t, ok, "Expected AgentVibe to be registered")

	assert.Equal(t, AgentVibe, def.Type)
	assert.Equal(t, "Mistral Vibe", def.DisplayName)
	assert.Equal(t, "VIBE_SESSIONS_DIR", def.EnvVar)
	assert.Equal(t, "vibe_session_dirs", def.ConfigKey)
	assert.Equal(t, "vibe:", def.IDPrefix)
	assert.True(t, def.FileBased)
	// Vibe is provider-authoritative: discovery and source lookup live on the
	// vibeProvider, not on legacy AgentDef hooks.
	assert.Nil(t, def.DiscoverFunc)
	assert.Nil(t, def.FindSourceFunc)
}

func TestVibeAgentByPrefix(t *testing.T) {
	// Test with vibe: prefix
	def, ok := AgentByPrefix("vibe:session_123")
	require.True(t, ok, "Expected vibe: prefix to match")
	assert.Equal(t, AgentVibe, def.Type)

	// Test with just session_123 - this will match Claude (empty prefix) since it has no colon
	// This is expected behavior per AgentByPrefix logic
	def2, ok2 := AgentByPrefix("session_123")
	assert.True(t, ok2, "session_123 matches Claude (empty prefix)")
	assert.Equal(t, AgentClaude, def2.Type)
}

func TestConvertVibeMessageToolCalls(t *testing.T) {
	// Tool categorization is delegated to the shared NormalizeToolCategory,
	// and string-encoded arguments are unwrapped to the raw JSON object.
	vibeMsg := VibeMessage{
		Role: "assistant",
		ToolCalls: []VibeToolCall{
			{ID: "call_obj", Function: VibeToolCallFunction{
				Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`),
			}},
			{ID: "call_str", Function: VibeToolCallFunction{
				Name: "run_shell_command", Arguments: json.RawMessage(`"{\"cmd\":\"ls\"}"`),
			}},
		},
	}

	msg, toolCalls := convertVibeMessage(vibeMsg, 0, "")
	require.Len(t, toolCalls, 2)
	assert.True(t, msg.HasToolUse)

	assert.Equal(t, "Read", toolCalls[0].Category)
	assert.JSONEq(t, `{"path":"a.txt"}`, toolCalls[0].InputJSON)

	assert.Equal(t, "Bash", toolCalls[1].Category)
	assert.JSONEq(t, `{"cmd":"ls"}`, toolCalls[1].InputJSON)
}

// TestParseRealVibeSession tests parsing with real Vibe session data only when
// explicitly requested. It is skipped by default so tests never read private
// local session data from a developer's home directory.
func TestParseRealVibeSession(t *testing.T) {
	vibeSessionDir := os.Getenv("AGENTSVIEW_TEST_VIBE_SESSION_DIR")
	if vibeSessionDir == "" {
		t.Skip("AGENTSVIEW_TEST_VIBE_SESSION_DIR not set")
	}
	messagesPath := filepath.Join(vibeSessionDir, "messages.jsonl")
	metaPath := filepath.Join(vibeSessionDir, "meta.json")

	// Skip if test session doesn't exist
	if _, err := os.Stat(messagesPath); os.IsNotExist(err) {
		t.Skipf("Test Vibe session not found at %s", messagesPath)
	}
	if _, err := os.Stat(metaPath); os.IsNotExist(err) {
		t.Skipf("Test Vibe meta.json not found at %s", metaPath)
	}

	fileInfo := FileInfo{
		Path:  messagesPath,
		Mtime: time.Now().UnixNano(),
	}

	result, err := parseVibeTestSession(t, messagesPath, fileInfo)
	require.NoError(t, err)

	// Verify basic session metadata
	assert.Equal(t, AgentVibe, result.Session.Agent)
	assert.NotEmpty(t, result.Session.ID)
	assert.True(t, len(result.Session.ID) > 0)

	// Should have parsed messages
	require.NotEmpty(t, result.Messages)
	assert.Greater(t, len(result.Messages), 0)

	// Verify timestamps from meta.json
	assert.False(t, result.Session.StartedAt.IsZero())
	assert.False(t, result.Session.EndedAt.IsZero())

	// Verify token usage from meta.json
	assert.True(t, result.Session.HasTotalOutputTokens || result.Session.TotalOutputTokens > 0)

	t.Logf("Successfully parsed real Vibe session with %d messages", len(result.Messages))
}
