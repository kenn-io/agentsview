package parser

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newQwenPawTestProvider builds a concrete qwenPawProvider for the given
// roots so package tests can exercise the folded parse, discovery, and
// source-lookup behavior directly through provider methods.
func newQwenPawTestProvider(t *testing.T, roots ...string) Provider {
	t.Helper()
	provider, ok := NewProvider(AgentQwenPaw, ProviderConfig{
		Roots:   roots,
		Machine: "local",
	})
	require.True(t, ok)
	return provider
}

// parseQwenPawTestSession parses a QwenPaw session JSON file at path with
// the given workspace project, replacing the removed package-level
// ParseQwenPawSession entrypoint with the provider-owned parse method.
func parseQwenPawTestSession(
	t *testing.T, path, project, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	t.Helper()
	return parseQwenPawSession(path, project, machine)
}

// discoverQwenPawTestSessions discovers QwenPaw sessions under root through
// the provider, returning the legacy DiscoveredFile shape (path + project)
// the tests assert against.
func discoverQwenPawTestSessions(
	t *testing.T, root string,
) []DiscoveredFile {
	t.Helper()
	if root == "" {
		return nil
	}
	provider := newQwenPawTestProvider(t, root)
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
			Agent:   AgentQwenPaw,
		})
	}
	return files
}

// findQwenPawTestSourceFile resolves a raw QwenPaw ID to a session file
// through the provider, replacing the removed FindQwenPawSourceFile.
func findQwenPawTestSourceFile(
	t *testing.T, root, rawID string,
) string {
	t.Helper()
	if root == "" {
		return ""
	}
	return qwenPawSourceFileForRawID(root, rawID)
}

// skipColonFilenamesOnWindows skips tests that must create files or
// directories whose names contain ":". That character is illegal in
// Windows filenames (reserved for NTFS alternate data streams), so the
// colon-collision scenario these tests guard against cannot occur
// there. TestIsValidQwenPawIDPart covers the rejection logic on every
// platform without touching the filesystem.
func skipColonFilenamesOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("':' is invalid in Windows filenames")
	}
}

// writeQwenPawSession creates a sessions/<name>.json file with the
// QwenPaw on-disk shape:
//
//	{"agent": {"memory": {"content": [[msg, []], ...]}}}
//
// Each entry in `messages` is wrapped as [msg, []]. The path mirrors
// ~/.copaw/workspaces/<workspace>/sessions/<name>.json.
func writeQwenPawSession(
	t *testing.T, workspace, name string,
	messages []string,
) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), workspace, "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	pairs := make([]string, len(messages))
	for i, m := range messages {
		pairs[i] = "[" + m + ",[]]"
	}
	wrapped := `{"agent":{"memory":{"content":[` +
		strings.Join(pairs, ",") + "]}}}"
	path := filepath.Join(dir, name+".json")
	require.NoError(t, os.WriteFile(path, []byte(wrapped), 0o644))
	return path
}

func TestParseQwenPawTimestamp(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  time.Time
	}{
		{
			"milliseconds",
			"2026-04-19 22:37:34.004",
			time.Date(2026, 4, 19, 22, 37, 34, 4_000_000, time.Local),
		},
		{
			"no fractional",
			"2026-04-19 22:37:34",
			time.Date(2026, 4, 19, 22, 37, 34, 0, time.Local),
		},
		{"empty", "", time.Time{}},
		{"invalid", "not-a-timestamp", time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseQwenPawTimestamp(tc.input)
			if tc.want.IsZero() {
				assert.True(t, got.IsZero(),
					"expected zero time, got %v", got)
				return
			}
			assert.True(t, got.Equal(tc.want),
				"timestamp = %v, want %v", got, tc.want)
		})
	}
}

func TestParseQwenPawSession_BasicUserAssistant(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1776607601691",
		[]string{
			`{"id":"msg_u1","name":"user","role":"user","content":[{"type":"text","text":"你好"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.004"}`,
			`{"id":"abc1","name":"Friday","role":"assistant","content":[{"type":"text","text":"你好，有什么可以帮你的？"}],"metadata":{},"timestamp":"2026-04-19 22:37:35.123"}`,
		},
	)
	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)

	assertSessionMeta(t, sess,
		"qwenpaw:default:default_1776607601691", "default", AgentQwenPaw,
	)
	assert.Equal(t, "你好", sess.FirstMessage)
	assertMessageCount(t, sess.MessageCount, 2)
	assert.Equal(t, 1, sess.UserMessageCount)

	wantStart := time.Date(
		2026, 4, 19, 22, 37, 34, 4_000_000, time.Local,
	)
	assertTimestamp(t, sess.StartedAt, wantStart)
	wantEnd := time.Date(
		2026, 4, 19, 22, 37, 35, 123_000_000, time.Local,
	)
	assertTimestamp(t, sess.EndedAt, wantEnd)

	require.Equal(t, 2, len(msgs))
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "你好", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "你好，有什么可以帮你的？", msgs[1].Content)
}

func TestParseQwenPawSession_ThinkingExtracted(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"}`,
			`{"id":"a1","name":"Friday","role":"assistant","content":[{"type":"thinking","thinking":"我应该先思考一下"},{"type":"text","text":"答案"}],"metadata":{},"timestamp":"2026-04-19 22:37:35.000"}`,
		},
	)
	_, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.Equal(t, 2, len(msgs))

	assert.True(t, msgs[1].HasThinking, "HasThinking flag")
	assert.Equal(t, "我应该先思考一下", msgs[1].ThinkingText)
	assert.Equal(t, "答案", msgs[1].Content)
}

func TestParseQwenPawSession_ToolUseAndResult(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"看下文件"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"}`,
			`{"id":"a1","name":"Friday","role":"assistant","content":[{"type":"tool_use","id":"call_abc","name":"read_file","input":{"file_path":"foo.txt"},"raw_input":"{\"file_path\":\"foo.txt\"}"}],"metadata":{},"timestamp":"2026-04-19 22:37:35.000"}`,
			`{"id":"s1","name":"system","role":"system","content":[{"type":"tool_result","id":"call_abc","name":"read_file","output":[{"type":"text","text":"file contents here"}]}],"metadata":{},"timestamp":"2026-04-19 22:37:36.000"}`,
			`{"id":"a2","name":"Friday","role":"assistant","content":[{"type":"text","text":"文件内容是 file contents here"}],"metadata":{},"timestamp":"2026-04-19 22:37:37.000"}`,
		},
	)
	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 4, len(msgs))

	aMsg := msgs[1]
	assert.Equal(t, RoleAssistant, aMsg.Role)
	assert.True(t, aMsg.HasToolUse, "HasToolUse flag")
	require.Equal(t, 1, len(aMsg.ToolCalls), "tool call count")
	assert.Equal(t, "call_abc", aMsg.ToolCalls[0].ToolUseID)
	assert.Equal(t, "read_file", aMsg.ToolCalls[0].ToolName)
	assert.Contains(t, aMsg.ToolCalls[0].InputJSON, "foo.txt")

	sMsg := msgs[2]
	assert.Equal(t, RoleUser, sMsg.Role)
	assert.True(t, sMsg.IsSystem, "IsSystem on tool_result carrier")
	require.Equal(t, 1, len(sMsg.ToolResults), "tool result count")
	assert.Equal(t, "call_abc", sMsg.ToolResults[0].ToolUseID)
	assert.Equal(t, len("file contents here"),
		sMsg.ToolResults[0].ContentLength,
		"tool result ContentLength")
}

func TestParseQwenPawSession_MultipleToolUsesInOneMessage(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"a1","name":"Friday","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"file_path":"a"}},{"type":"tool_use","id":"call_2","name":"read_file","input":{"file_path":"b"}}],"metadata":{},"timestamp":"2026-04-19 22:37:35.000"}`,
		},
	)
	_, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.Equal(t, 1, len(msgs))
	require.Equal(t, 2, len(msgs[0].ToolCalls))
	assert.Equal(t, "call_1", msgs[0].ToolCalls[0].ToolUseID)
	assert.Equal(t, "call_2", msgs[0].ToolCalls[1].ToolUseID)
}

func TestParseQwenPawSession_EmptyContentArray(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"a1","name":"Friday","role":"assistant","content":[],"metadata":{},"timestamp":"2026-04-19 22:37:35.000"}`,
		},
	)
	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assertMessageCount(t, sess.MessageCount, 1)
	require.Equal(t, 1, len(msgs))
	assert.Equal(t, "", msgs[0].Content)
}

func TestParseQwenPawSession_MissingTimestamp(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{}}`,
		},
	)
	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assertZeroTimestamp(t, sess.StartedAt, "StartedAt")
	assertZeroTimestamp(t, sess.EndedAt, "EndedAt")
	require.Equal(t, 1, len(msgs))
	assertZeroTimestamp(t, msgs[0].Timestamp, "msg timestamp")
}

func TestParseQwenPawSession_NonexistentFile(t *testing.T) {
	_, _, err := parseQwenPawTestSession(t,
		"/nonexistent/default/sessions/foo.json",
		"default", "local",
	)
	require.Error(t, err)
}

func TestParseQwenPawSession_FileMtimeIsNanoseconds(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"}`,
		},
	)
	info, err := os.Stat(path)
	require.NoError(t, err)
	sess, _, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, info.ModTime().UnixNano(), sess.File.Mtime,
		"Mtime must be nanoseconds")
}

func TestParseQwenPawSession_SystemTextMessage(t *testing.T) {
	path := writeQwenPawSession(t, "default", "default_1",
		[]string{
			`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"}`,
			`{"id":"s1","name":"system","role":"system","content":[{"type":"text","text":"system notice"}],"metadata":{},"timestamp":"2026-04-19 22:37:35.000"}`,
		},
	)
	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Equal(t, 2, len(msgs))
	assert.Equal(t, RoleUser, msgs[1].Role)
	assert.True(t, msgs[1].IsSystem)
	assert.Equal(t, "system notice", msgs[1].Content)
	assert.Equal(t, 1, sess.UserMessageCount)
}

func TestParseQwenPawSession_MalformedJsonReturnsError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "default", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "broken.json")
	require.NoError(t, os.WriteFile(path,
		[]byte("{not valid json"), 0o644))

	_, _, err := parseQwenPawTestSession(t, path, "default", "local")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed JSON",
		"error must come from the ValidBytes guard, not content-missing")
}

func TestParseQwenPawSession_RejectsColonInIDParts(t *testing.T) {
	content := `{"agent":{"memory":{"content":[]}}}`
	t.Run("workspace", func(t *testing.T) {
		// The workspace is passed as an argument, not derived from the
		// path, so this stays cross-platform: the file lives in a
		// normal directory and only the project string carries ":".
		dir := filepath.Join(t.TempDir(), "default", "sessions")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, "ok.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		_, _, err := parseQwenPawTestSession(t, path, "ws:bad", "local")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid workspace")
	})
	t.Run("subdir", func(t *testing.T) {
		// The subdir is derived from the path, so it must exist on disk
		// with a ":" in its name — impossible on Windows.
		skipColonFilenamesOnWindows(t)
		dir := filepath.Join(t.TempDir(), "default", "sessions", "sub:bad")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, "ok.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

		_, _, err := parseQwenPawTestSession(t, path, "default", "local")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid subdir")
	})
}

func TestParseQwenPawSession_EmptyContentArrayTopLevel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "default", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "empty.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644))

	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assertMessageCount(t, sess.MessageCount, 0)
	assert.Nil(t, msgs)
}

func TestParseQwenPawSession_SkipsNonMessageEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "default", "sessions")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	content := `{"agent":{"memory":{"content":[` +
		`[{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"},[]],` +
		`"orphan_string",` +
		`[{"id":"a1","name":"Friday","role":"assistant","content":[{"type":"text","text":"hello"}],"metadata":{},"timestamp":"2026-04-19 22:37:35.000"},[]]` +
		`]}}}`
	path := filepath.Join(dir, "mixed.json")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	sess, msgs, err := parseQwenPawTestSession(t, path, "default", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.MalformedLines)
	assertMessageCount(t, sess.MessageCount, 2)
	require.Equal(t, 2, len(msgs))
}

func TestDiscoverQwenPawSessions(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"default_1", "default_2"} {
		dir := filepath.Join(root, "default", "sessions")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, name+".json"),
			[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644,
		))
	}
	fmDir := filepath.Join(root, "fund_manager", "sessions")
	require.NoError(t, os.MkdirAll(fmDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(fmDir, "main_main.json"),
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644,
	))

	files := discoverQwenPawTestSessions(t, root)
	require.Equal(t, 3, len(files))
	for _, f := range files {
		assert.Equal(t, AgentQwenPaw, f.Agent)
	}
	projects := map[string]int{}
	for _, f := range files {
		projects[f.Project]++
	}
	assert.Equal(t, 2, projects["default"])
	assert.Equal(t, 1, projects["fund_manager"])
}

func TestDiscoverQwenPawSessions_IncludesConsoleSubdir(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "default", "sessions", "console")
	require.NoError(t, os.MkdirAll(consoleDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(consoleDir, "default_1.json"),
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644,
	))

	files := discoverQwenPawTestSessions(t, root)
	require.Equal(t, 1, len(files))
	assert.Equal(t, "default", files[0].Project)
}

func TestDiscoverQwenPawSessions_SkipsHiddenSubdirs(t *testing.T) {
	root := t.TempDir()
	legacyDir := filepath.Join(root, "default", "sessions", ".weixin-legacy")
	require.NoError(t, os.MkdirAll(legacyDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(legacyDir, "stale.json"),
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644,
	))

	files := discoverQwenPawTestSessions(t, root)
	assert.Nil(t, files)
}

func TestDiscoverQwenPawSessions_FiltersNonSessionFiles(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "default", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "default_1.json"),
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "notes.txt"),
		[]byte("ignore\n"), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "config.json.bak"),
		[]byte("{}\n"), 0o644,
	))
	dialogDir := filepath.Join(root, "default", "dialog")
	require.NoError(t, os.MkdirAll(dialogDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(dialogDir, "2026-04-08.jsonl"),
		[]byte("{}\n"), 0o644,
	))

	files := discoverQwenPawTestSessions(t, root)
	require.Equal(t, 1, len(files))
}

func TestDiscoverQwenPawSessions_EmptyAndMissing(t *testing.T) {
	assert.Nil(t, discoverQwenPawTestSessions(t, ""))
	assert.Nil(t, discoverQwenPawTestSessions(t, "/nonexistent"))
}

func TestDiscoverQwenPawSessions_SkipsFilesAtRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(root, "loose.json"),
		[]byte("{}\n"), 0o644,
	))
	files := discoverQwenPawTestSessions(t, root)
	assert.Nil(t, files)
}

// When sessions/foo:bar.json (a root file whose stem contains the ID
// separator) and sessions/foo/bar.json (a subdir file) coexist, both
// would otherwise collapse to qwenpaw:default:foo:bar. Discovery must
// emit only the unambiguous subdir file.
func TestDiscoverQwenPawSessions_RejectsColliding(t *testing.T) {
	skipColonFilenamesOnWindows(t)
	content := []byte(`{"agent":{"memory":{"content":[]}}}`)
	root := t.TempDir()
	sessDir := filepath.Join(root, "default", "sessions")
	require.NoError(t, os.MkdirAll(filepath.Join(sessDir, "foo"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(sessDir, "foo:bar.json"), content, 0o644))
	subFile := filepath.Join(sessDir, "foo", "bar.json")
	require.NoError(t, os.WriteFile(subFile, content, 0o644))

	// A workspace and a subdir whose names contain ":" are also dropped.
	colonWsDir := filepath.Join(root, "ws:bad", "sessions")
	require.NoError(t, os.MkdirAll(colonWsDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(colonWsDir, "ok.json"), content, 0o644))
	colonSubDir := filepath.Join(sessDir, "sub:bad")
	require.NoError(t, os.MkdirAll(colonSubDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(colonSubDir, "ok.json"), content, 0o644))

	files := discoverQwenPawTestSessions(t, root)
	require.Len(t, files, 1)
	assert.Equal(t, subFile, files[0].Path)

	sess, _, err := parseQwenPawTestSession(t,
		files[0].Path, files[0].Project, "local")
	require.NoError(t, err)
	assert.Equal(t, "qwenpaw:default:foo:bar", sess.ID)
}

func TestFindQwenPawSourceFile(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "default", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	path := filepath.Join(sessDir, "default_1776607601691.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644))

	assert.Equal(t, path,
		findQwenPawTestSourceFile(t, root, "default:default_1776607601691"))
	assert.Equal(t, "",
		findQwenPawTestSourceFile(t, root, "default:does_not_exist"))
	assert.Equal(t, "",
		findQwenPawTestSourceFile(t, root, "ghost:default_1"))
	assert.Equal(t, "",
		findQwenPawTestSourceFile(t, root, "invalid"))
	assert.Equal(t, "",
		findQwenPawTestSourceFile(t, root, "bad/workspace:default_1"))
	assert.Equal(t, "",
		findQwenPawTestSourceFile(t, "", "default:default_1"))
}

func TestFindQwenPawSourceFile_ConsoleSubdir(t *testing.T) {
	root := t.TempDir()
	consoleDir := filepath.Join(root, "default", "sessions", "console")
	require.NoError(t, os.MkdirAll(consoleDir, 0o755))
	path := filepath.Join(consoleDir, "default_1781268804068.json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644))

	// Subdir is encoded in the raw ID so the lookup is unambiguous.
	assert.Equal(t, path,
		findQwenPawTestSourceFile(t, root,
			"default:console:default_1781268804068"))
}

func TestParseQwenPawSession_IDsDifferBySubdir(t *testing.T) {
	// Two files with the same stem under sessions/ and sessions/console/
	// must produce distinct session IDs — otherwise the second sync
	// overwrites the first.
	root := t.TempDir()
	rootDir := filepath.Join(root, "default", "sessions")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))
	consoleDir := filepath.Join(root, "default", "sessions", "console")
	require.NoError(t, os.MkdirAll(consoleDir, 0o755))

	rootPath := filepath.Join(rootDir, "foo.json")
	consolePath := filepath.Join(consoleDir, "foo.json")
	for _, p := range []string{rootPath, consolePath} {
		require.NoError(t, os.WriteFile(p,
			[]byte(`{"agent":{"memory":{"content":[[`+
				`{"id":"u1","name":"user","role":"user","content":[{"type":"text","text":"hi"}],"metadata":{},"timestamp":"2026-04-19 22:37:34.000"}`+
				`,[]]]}}}`), 0o644))
	}

	rootSess, _, err := parseQwenPawTestSession(t, rootPath, "default", "local")
	require.NoError(t, err)
	consoleSess, _, err := parseQwenPawTestSession(t, consolePath, "default", "local")
	require.NoError(t, err)

	assert.Equal(t, "qwenpaw:default:foo", rootSess.ID,
		"root layout ID")
	assert.Equal(t, "qwenpaw:default:console:foo", consoleSess.ID,
		"console subdir ID — must differ from root")
	assert.NotEqual(t, rootSess.ID, consoleSess.ID)
}

func TestFindQwenPawSourceFile_RootAndConsoleResolveIndependently(t *testing.T) {
	root := t.TempDir()
	rootDir := filepath.Join(root, "default", "sessions")
	consoleDir := filepath.Join(root, "default", "sessions", "console")
	require.NoError(t, os.MkdirAll(rootDir, 0o755))
	require.NoError(t, os.MkdirAll(consoleDir, 0o755))
	rootPath := filepath.Join(rootDir, "same.json")
	consolePath := filepath.Join(consoleDir, "same.json")
	require.NoError(t, os.WriteFile(rootPath, []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(consolePath, []byte(`{}`), 0o644))

	// Each lookup returns the unique file that matches the encoded
	// layout — no precedence ambiguity, no silent overwrite.
	assert.Equal(t, rootPath,
		findQwenPawTestSourceFile(t, root, "default:same"))
	assert.Equal(t, consolePath,
		findQwenPawTestSourceFile(t, root, "default:console:same"))
}

func TestIsValidQwenPawIDPart(t *testing.T) {
	valid := []string{
		"default", "note_keeper", "main_main",
		"user@example.com_1700000000002",
		"o9cq80wkB8F3P4xWLXsuU2hZnVYU@im.wechat_wechat--abc",
	}
	for _, s := range valid {
		assert.Truef(t, IsValidQwenPawIDPart(s),
			"IsValidQwenPawIDPart(%q) should be true", s)
	}
	// Structural separators (":", "~"), path separators, traversal
	// components, and URL delimiters ("?", "#", "%") must be rejected.
	invalid := []string{
		"", ".", "..", "a/b", "a\\b", "a:b", "a~b",
		"a?b", "a#b", "a%b", "host~qwenpaw",
	}
	for _, s := range invalid {
		assert.Falsef(t, IsValidQwenPawIDPart(s),
			"IsValidQwenPawIDPart(%q) should be false", s)
	}
}

func TestFindQwenPawSourceFile_AcceptsWeirdFilenames(t *testing.T) {
	root := t.TempDir()
	sessDir := filepath.Join(root, "default", "sessions")
	require.NoError(t, os.MkdirAll(sessDir, 0o755))
	weird := "o9cq80wkB8F3P4xWLXsuU2hZnVYU@im.wechat_wechat--o9cq80wkB8F3P4xWLXsuU2hZnVYU@im.wechat"
	path := filepath.Join(sessDir, weird+".json")
	require.NoError(t, os.WriteFile(path,
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644))

	assert.Equal(t, path,
		findQwenPawTestSourceFile(t, root, "default:"+weird))
}

func TestFindQwenPawSourceFile_RejectsTraversal(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "root")
	require.NoError(t, os.MkdirAll(root, 0o755))
	// A real file sits one level above root (still inside the
	// test-owned tree) so traversal would have something to reach.
	outside := filepath.Join(tmp, "escape.json")
	require.NoError(t, os.WriteFile(outside,
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644))

	for _, rawID := range []string{
		"..:default_1",
		"default:..",
		"default:..:default_1",
		".:default_1",
		"default:.",
	} {
		assert.Equal(t, "", findQwenPawTestSourceFile(t, root, rawID),
			"rawID %q must not resolve", rawID)
	}
	for _, bad := range []string{".", ".."} {
		assert.False(t, IsValidQwenPawIDPart(bad),
			"IsValidQwenPawIDPart(%q) should be false", bad)
	}
}

// A stem containing ":" must be rejected: ":" is the ID-part separator,
// so a root file sessions/foo:bar.json and a subdir file
// sessions/foo/bar.json both yield the ID qwenpaw:default:foo:bar.
// Rejecting ":" in stems keeps the root file out of discovery so the
// subdir file is the unambiguous owner of that ID.
func TestFindQwenPawSourceFile_RejectsColonInStem(t *testing.T) {
	assert.False(t, IsValidQwenPawIDPart("foo:bar"),
		"IsValidQwenPawIDPart should reject the separator char")

	root := t.TempDir()
	subSess := filepath.Join(root, "default", "sessions", "foo")
	require.NoError(t, os.MkdirAll(subSess, 0o755))
	subFile := filepath.Join(subSess, "bar.json")
	require.NoError(t, os.WriteFile(subFile,
		[]byte(`{"agent":{"memory":{"content":[]}}}`), 0o644))

	// The shared raw ID resolves to the subdir file, not the rejected
	// root-level foo:bar.json.
	assert.Equal(t, subFile,
		findQwenPawTestSourceFile(t, root, "default:foo:bar"))
}

// TestQwenPawFixtures exercises the checked-in testdata/qwenpaw tree
// end to end: every fixture must discover, parse without error, and
// produce its expected canonical ID. This guards against malformed or
// drifting fixtures that would otherwise pass CI unnoticed.
func TestQwenPawFixtures(t *testing.T) {
	files := discoverQwenPawTestSessions(t, filepath.Join("testdata", "qwenpaw"))
	require.Len(t, files, 5)

	sessByID := make(map[string]*ParsedSession, len(files))
	msgsByID := make(map[string][]ParsedMessage, len(files))
	for _, f := range files {
		assert.Equal(t, AgentQwenPaw, f.Agent)
		sess, msgs, err := parseQwenPawTestSession(t, f.Path, f.Project, "local")
		require.NoErrorf(t, err, "parse %s", f.Path)
		require.NotNil(t, sess)
		sessByID[sess.ID] = sess
		msgsByID[sess.ID] = msgs
	}

	for _, id := range []string{
		"qwenpaw:default:default_1700000000000",
		"qwenpaw:default:main_main",
		"qwenpaw:default:console:default_1700000000001",
		"qwenpaw:note_keeper:user@example.com_1700000000002",
		"qwenpaw:researcher:empty",
	} {
		assert.Containsf(t, sessByID, id,
			"fixture with ID %q must be discovered and parsed", id)
	}

	// The empty fixture yields a session with no messages.
	assert.Empty(t, msgsByID["qwenpaw:researcher:empty"])

	// main_main exercises the tool_use -> tool_result round-trip: the
	// system-role carrier maps to RoleUser + IsSystem.
	toolMsgs := msgsByID["qwenpaw:default:main_main"]
	require.Len(t, toolMsgs, 4)
	assert.True(t, toolMsgs[1].HasToolUse, "assistant message has tool_use")
	require.Len(t, toolMsgs[1].ToolCalls, 1)
	assert.Equal(t, "call_aaaaaaaaaaaa", toolMsgs[1].ToolCalls[0].ToolUseID)
	resultMsg := toolMsgs[2]
	assert.Equal(t, RoleUser, resultMsg.Role)
	assert.True(t, resultMsg.IsSystem, "tool_result carrier is a system message")
	require.Len(t, resultMsg.ToolResults, 1)
	assert.Equal(t, "call_aaaaaaaaaaaa", resultMsg.ToolResults[0].ToolUseID)
	assert.Positive(t, resultMsg.ToolResults[0].ContentLength)
}
