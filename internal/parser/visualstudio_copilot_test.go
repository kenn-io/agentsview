package parser

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoverVisualStudioCopilotSessions(t *testing.T) {
	root := t.TempDir()
	tracesDir := filepath.Join(
		root, "VSGitHubCopilotLogs", "traces",
	)
	require.NoError(t, os.MkdirAll(tracesDir, 0o755))
	tracePath := filepath.Join(
		tracesDir,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
		}) + "\n"
	require.NoError(t, os.WriteFile(tracePath, []byte(data), 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(tracesDir, "not-copilot.jsonl"),
		[]byte("{}\n"), 0o644,
	))

	files := discoverVisualStudioCopilotTestSessions(t, tracesDir)

	require.Len(t, files, 1)
	assert.Equal(t, tracePath+"#"+conversationID, files[0].Path)
	assert.Equal(t, "visualstudio", files[0].Project)
	assert.Equal(t, AgentVSCopilot, files[0].Agent)
}

func TestDiscoverVisualStudioCopilotSessions_IgnoresParentDirs(t *testing.T) {
	root := t.TempDir()
	tracesDir := filepath.Join(
		root, "VSGitHubCopilotLogs", "traces",
	)
	require.NoError(t, os.MkdirAll(tracesDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(
		tracesDir,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	), []byte("{}\n"), 0o644))

	files := discoverVisualStudioCopilotTestSessions(t, root)

	assert.Empty(t, files)
}

func TestDiscoverVisualStudioCopilotSessions_DeduplicatesConversationTraceFiles(t *testing.T) {
	root := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	oldTrace := filepath.Join(
		root,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	newTrace := filepath.Join(
		root,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
		}) + "\n"
	require.NoError(t, os.WriteFile(oldTrace, []byte(data), 0o644))
	require.NoError(t, os.WriteFile(newTrace, []byte(data), 0o644))

	files := discoverVisualStudioCopilotTestSessions(t, root)

	require.Len(t, files, 1)
	assert.Equal(t, newTrace+"#"+conversationID, files[0].Path)
}

func TestParseVisualStudioCopilotSession_MalformedTraceLineReturnsError(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
		}) + "\n" + `{"resourceSpans":[` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestDiscoverVisualStudioCopilotSessions_EmitsWorkItemPerConversation(t *testing.T) {
	dir := t.TempDir()
	dominant := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	secondary := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"

	// Older file carries the dominant conversation plus a single span
	// for a secondary conversation. The secondary conversation can never
	// win the old "best conversation" heuristic, so it used to be dropped.
	oldTrace := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	oldData := strings.Join([]string{
		vsCopilotTraceLineJSONWithSpanID(dominant, "d1",
			"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
			}),
		vsCopilotTraceLineJSONWithSpanID(dominant, "d2",
			"chat gpt-5.5", "1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Now run the build."}]}]`,
			}),
		vsCopilotTraceLineJSONWithSpanID(secondary, "s1",
			"invoke_agent GitHub Copilot",
			"1781294552800436000", "1781294586729109400",
			map[string]string{
				"gen_ai.agent.name":    "GitHub Copilot",
				"gen_ai.request.model": "gpt-5.5",
				"copilot_chat.mode":    "Agent",
			}),
	}, "\n") + "\n"

	// Newer file carries only the dominant conversation, so it becomes
	// that conversation's representative file.
	newTrace := filepath.Join(
		dir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	newData := vsCopilotTraceLineJSONWithSpanID(dominant, "d3",
		"chat gpt-5.5", "1781293700000000000", "1781293710000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Ship it."}]}]`,
		}) + "\n"

	require.NoError(t, os.WriteFile(oldTrace, []byte(oldData), 0o644))
	require.NoError(t, os.WriteFile(newTrace, []byte(newData), 0o644))

	files := discoverVisualStudioCopilotTestSessions(t, dir)

	got := map[string]string{}
	for _, f := range files {
		assert.Equal(t, AgentVSCopilot, f.Agent)
		assert.Equal(t, "visualstudio", f.Project)
		got[vsConversationIDFromPath(t, f.Path)] = f.Path
	}
	require.Len(t, files, 2)
	assert.Equal(t, newTrace+"#"+dominant, got[dominant],
		"dominant conversation should point at its latest trace file")
	assert.Equal(t, oldTrace+"#"+secondary, got[secondary],
		"secondary conversation must not be dropped")
}

func TestDiscoverVisualStudioCopilotSessions_SampleFixturesEnumerateBothConversations(t *testing.T) {
	_, callerFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	sampleDir := filepath.Join(
		filepath.Dir(callerFile), "..", "..",
		"testdata", "visualstudio-copilot", "redacted",
	)
	if _, err := os.Stat(sampleDir); err != nil {
		t.Skipf("sample dir not available: %v", err)
	}

	files := discoverVisualStudioCopilotTestSessions(t, sampleDir)

	got := map[string]struct{}{}
	for _, f := range files {
		got[vsConversationIDFromPath(t, f.Path)] = struct{}{}
	}
	assert.Contains(t, got, "4a8f63f6-7626-4416-a874-fc7bd2c3f005",
		"dominant conversation should be discovered")
	assert.Contains(t, got, "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28",
		"secondary conversation in sample-4 must not be dropped")
}

// TestParseVisualStudioCopilotConversation_PropagatesSiblingDirReadError
// verifies that a failure to enumerate sibling trace files is surfaced rather
// than swallowed into "no siblings". Otherwise the primary trace would be
// written as a complete session even though sibling discovery failed, defeating
// the partial-transcript guard.
func TestParseVisualStudioCopilotConversation_PropagatesSiblingDirReadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	require.NoError(t, os.Mkdir(dir, 0o755))
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	tracePath := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	data := vsCopilotTraceLineJSON(conversationID, "chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Hello."}]}]`,
		}) + "\n"
	require.NoError(t, os.WriteFile(tracePath, []byte(data), 0o644))

	// Make the directory traversable but not readable: the known trace file can
	// still be opened, but enumerating siblings via ReadDir fails.
	require.NoError(t, os.Chmod(dir, 0o100))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, err := parseVisualStudioCopilotTestConversation(t,
		tracePath, conversationID, "visualstudio", "local",
	)
	require.Error(t, err,
		"a sibling directory read error must propagate, not be swallowed")
}

// TestVisualStudioCopilotTraceFingerprintStrictPropagatesDirError verifies that
// the strict fingerprint surfaces a directory-enumeration error while the
// best-effort fingerprint falls back to the representative file's stat. Sync
// skip checks rely on the strict variant so a ReadDir failure is retried rather
// than mistaken for an unchanged fingerprint in a single-trace directory.
func TestVisualStudioCopilotTraceFingerprintStrictPropagatesDirError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory read permissions")
	}
	dir := filepath.Join(t.TempDir(), "traces")
	require.NoError(t, os.Mkdir(dir, 0o755))
	tracePath := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(tracePath, []byte("{}\n"), 0o644))

	// Readable directory: both variants agree on the composite fingerprint.
	wantSize, wantMtime, err := VisualStudioCopilotTraceFingerprintStrict(
		tracePath,
	)
	require.NoError(t, err)
	lenientSize, lenientMtime := VisualStudioCopilotTraceFingerprint(tracePath)
	assert.Equal(t, wantSize, lenientSize)
	assert.Equal(t, wantMtime, lenientMtime)

	// Traversable but not readable: ReadDir fails. The strict variant must
	// return the error; the best-effort variant falls back to the file's stat.
	require.NoError(t, os.Chmod(dir, 0o100))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, err = VisualStudioCopilotTraceFingerprintStrict(tracePath)
	require.Error(t, err,
		"strict fingerprint must surface the directory read error")

	fallbackSize, fallbackMtime := VisualStudioCopilotTraceFingerprint(tracePath)
	info, statErr := os.Stat(tracePath)
	require.NoError(t, statErr)
	assert.Equal(t, info.Size(), fallbackSize,
		"best-effort fingerprint falls back to the representative file stat")
	assert.Equal(t, info.ModTime().UnixNano(), fallbackMtime)
}

// TestVisualStudioCopilotTraceFingerprintStrictPropagatesSiblingStatError
// verifies that a sibling trace file that lists but cannot be stat'd (a broken
// symlink) fails the strict fingerprint, while the best-effort fingerprint
// ignores it. The skip check uses the strict variant, so an unstattable sibling
// must not be treated as "unchanged" when the readable files still match the
// stored composite fingerprint.
func TestVisualStudioCopilotTraceFingerprintStrictPropagatesSiblingStatError(
	t *testing.T,
) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	dir := t.TempDir()
	tracePath := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(tracePath, []byte("{}\n"), 0o644))
	// A sibling that appears in the listing but cannot be stat'd: a symlink to a
	// missing target resolves to ENOENT on stat.
	broken := filepath.Join(
		dir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing-target"), broken))

	_, _, err := VisualStudioCopilotTraceFingerprintStrict(tracePath)
	require.Error(t, err,
		"strict fingerprint must surface a sibling stat failure")

	size, _ := VisualStudioCopilotTraceFingerprint(tracePath)
	info, statErr := os.Stat(tracePath)
	require.NoError(t, statErr)
	assert.Equal(t, info.Size(), size,
		"best-effort fingerprint counts only the statable trace files")
}

func TestParseVisualStudioCopilotConversation_PropagatesReadError(t *testing.T) {
	// A directory named like a trace file exists but cannot be scanned as
	// JSONL, so the read fails. The error must propagate rather than be
	// swallowed into an empty (cacheable) "no sessions" result.
	dir := filepath.Join(
		t.TempDir(),
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Mkdir(dir, 0o755))

	sess, msgs, err := parseVisualStudioCopilotTestConversation(t,
		dir, "4a8f63f6-7626-4416-a874-fc7bd2c3f005", "visualstudio", "local",
	)

	require.Error(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestDiscoverVisualStudioCopilotSessions_EnqueuesUnreadableTraceFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	// A trace file that cannot be read (here, a symlink to a directory)
	// must still be enqueued so the sync worker surfaces the failure,
	// rather than silently dropping every conversation it might contain.
	root := t.TempDir()
	target := filepath.Join(root, "target-dir")
	require.NoError(t, os.Mkdir(target, 0o755))
	link := filepath.Join(
		root, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Symlink(target, link))

	files := discoverVisualStudioCopilotTestSessions(t, root)

	require.Len(t, files, 1)
	assert.Equal(t, link, files[0].Path,
		"unreadable trace file should be enqueued by its physical path")
	assert.Equal(t, AgentVSCopilot, files[0].Agent)
}

func TestResolveSourceFilePath(t *testing.T) {
	trace := "/logs/20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl"
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"

	assert.Equal(t, trace,
		ResolveSourceFilePath(VisualStudioCopilotVirtualPath(trace, conversationID)),
		"virtual path should resolve to its physical trace file")
	assert.Equal(t, "/logs/session.jsonl",
		ResolveSourceFilePath("/logs/session.jsonl"),
		"a plain source path should be returned unchanged")
	assert.Equal(t, "", ResolveSourceFilePath(""))
}

// vsConversationIDFromPath extracts the conversation ID from a
// <traceFile>#<conversationID> virtual work-item path.
func vsConversationIDFromPath(t *testing.T, path string) string {
	t.Helper()
	idx := strings.LastIndex(path, "#")
	require.Greater(t, idx, 0,
		"expected virtual path with #conversationID, got %q", path)
	return path[idx+1:]
}

func TestParseVisualStudioCopilotSession_IgnoresNonTraceFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sess.json")
	data := `{
		"version": 3,
		"sessionId": "test-123",
		"requests": [{
			"requestId": "req1",
			"message": {"text": "Hello"},
			"response": [{"value": "Hi"}],
			"timestamp": 1755347728047
		}]
	}`
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	assert.Nil(t, sess)
	assert.Nil(t, msgs)
}

func TestParseVisualStudioCopilotTraceSession(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	data := strings.Join([]string{
		vsCopilotTraceLineJSON(conversationID,
			"execute_tool run_command_in_terminal",
			"1781293588624985000", "1781293588769581200",
			map[string]string{
				"gen_ai.tool.name":           "run_command_in_terminal",
				"gen_ai.tool.call.id":        "call_123",
				"gen_ai.tool.call.arguments": `{"command":"go test ./..."}`,
				"gen_ai.tool.call.result":    `{"Value":"ok"}`,
			}),
		vsCopilotTraceLineJSON(conversationID,
			"invoke_agent GitHub Copilot",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.agent.name":       "GitHub Copilot",
				"gen_ai.request.model":    "gpt-5.5",
				"copilot_chat.mode":       "Agent",
				"copilot_chat.turn_count": "1",
			}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, AgentVSCopilot, sess.Agent)
	assert.Equal(t, "visualstudio-copilot:"+conversationID, sess.ID)
	assert.Equal(t, "Run command: go test ./...", sess.FirstMessage)
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasToolUse)
	assert.Contains(t, msgs[0].Content, "$ go test ./...")
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "run_command_in_terminal",
		msgs[0].ToolCalls[0].ToolName)
	assert.Equal(t, "Bash", msgs[0].ToolCalls[0].Category)
	assert.Contains(t, msgs[0].ToolCalls[0].InputJSON,
		"go test ./...")
	assert.JSONEq(t, `{"command":"go test ./..."}`,
		msgs[0].ToolCalls[0].InputJSON)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "completed",
		msgs[0].ToolCalls[0].ResultEvents[0].Status)
	assert.Equal(t, "ok", msgs[0].ToolCalls[0].ResultEvents[0].Content)
}

func TestParseVisualStudioCopilotTraceSession_GetFileResult(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	result := `{"Value":{"Value":{"Content":"1: <Page>\n2:   <TextBlock Text=\"Hello\" />"}}}`
	data := vsCopilotTraceLineJSON(conversationID,
		"execute_tool get_file",
		"1781293588624985000", "1781293588769581200",
		map[string]string{
			"gen_ai.tool.name":           "get_file",
			"gen_ai.tool.call.id":        "call_file",
			"gen_ai.tool.call.arguments": `{"filename":"Views\\MainWindow.xaml","startLine":1,"endLine":400,"includeLineNumbers":true}`,
			"gen_ai.tool.call.result":    result,
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "Read file: Views\\MainWindow.xaml", sess.FirstMessage)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "[Read: get_file]")
	assert.Contains(t, msgs[0].Content, "Views\\MainWindow.xaml")
	require.Len(t, msgs[0].ToolCalls, 1)
	call := msgs[0].ToolCalls[0]
	assert.Equal(t, "get_file", call.ToolName)
	assert.Equal(t, "Read", call.Category)
	assert.Contains(t, call.InputJSON, `"file_path":"Views\\MainWindow.xaml"`)
	assert.NotContains(t, call.InputJSON, `"arguments"`)
	require.Len(t, call.ResultEvents, 1)
	assert.Equal(t, "visualstudio-copilot", call.ResultEvents[0].Source)
	assert.Equal(t, "completed", call.ResultEvents[0].Status)
	assert.Contains(t, call.ResultEvents[0].Content, "<Page>")
	assert.Contains(t, call.ResultEvents[0].Content, "TextBlock")
}

func TestParseVisualStudioCopilotTraceSession_InvokeOnlyFirstMessage(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	data := vsCopilotTraceLineJSON(conversationID,
		"invoke_agent GitHub Copilot",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.agent.name":            "GitHub Copilot",
			"gen_ai.request.model":         "gpt-5.5",
			"copilot_chat.mode":            "Agent",
			"copilot_chat.client_id":       "Microsoft.VisualStudio.Conversations.Chat.HelpWindow",
			"copilot_chat.root_request_id": "de788686-1331-4747-a2cd-7cc1009beec8",
			"copilot_chat.turn_count":      "1",
			"copilot_chat.initiator_type":  "User",
			"copilot_chat.entry_point":     "Microsoft.VisualStudio.Copilot.AgentModeResponder",
			"gen_ai.operation.name":        "invoke_agent",
			"gen_ai.provider.name":         "other",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t,
		"Visual Studio Copilot Agent | HelpWindow | gpt-5.5 | de788686",
		sess.FirstMessage)
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0].Content, "model: gpt-5.5")
}

func TestParseVisualStudioCopilotTraceSession_ChatPromptFirstMessage(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"system","parts":[{"type":"text","content":"You are an AI programming assistant."}]},{"role":"user","parts":[{"type":"text","content":"Remove the Details button and replace the expander with tabs."}]}]`
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":        "chat",
			"gen_ai.request.model":         "gpt-5.5",
			"gen_ai.input.messages":        inputMessages,
			"copilot_chat.client_id":       "Microsoft.Modernization.Agent",
			"copilot_chat.root_request_id": "398c5816-cfb4-4f51-a195-16e7f03edc69",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t,
		"Remove the Details button and replace the expander with tabs.",
		sess.FirstMessage)
	assert.Equal(t, 1, sess.UserMessageCount)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t,
		"Remove the Details button and replace the expander with tabs.",
		msgs[0].Content)
}

func TestParseVisualStudioCopilotTraceSession_PreservesPromptMarkdown(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	prompt := "Use this safer version:\n\n```powershell\ngit branch saved/real-self-service-v2-local-work\ngit reset --hard origin/real-self-service-v2\n```\n\nThat does two things."
	inputMessages := mustJSON(t, []vsCopilotChatMessage{{
		Role: "user",
		Parts: []vsCopilotChatPart{{
			Type:    "text",
			Content: prompt,
		}},
	}})
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": string(inputMessages),
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t,
		"Use this safer version: ```powershell git branch saved/real-self-service-v2-local-work git reset --hard origin/real-self-service-v2 ``` That does two things.",
		sess.FirstMessage)
	require.Len(t, msgs, 1)
	assert.Equal(t, prompt, msgs[0].Content)
	assert.Contains(t, msgs[0].Content, "```powershell\n")
	assert.Contains(t, msgs[0].Content,
		"git reset --hard origin/real-self-service-v2")
}

func TestParseVisualStudioCopilotTraceSession_CombinesConversationTraceFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	firstInput := `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`
	secondInput := `[{"role":"user","parts":[{"type":"text","content":"Now run the build."}]}]`
	firstData := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": firstInput,
		}) + "\n"
	secondData := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293620000000000", "1781293630000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": secondInput,
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(firstData), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(secondData), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "Update the XAML.", sess.FirstMessage)
	assert.Equal(t, 2, sess.UserMessageCount)
	require.Len(t, msgs, 2)
	assert.Equal(t, "Update the XAML.", msgs[0].Content)
	assert.Equal(t, "Now run the build.", msgs[1].Content)
}

// TestParseVisualStudioCopilotTraceSession_PropagatesSiblingReadError verifies
// that an unreadable sibling trace file fails the parse rather than silently
// reconstructing the conversation from a subset of its trace files. A
// conversation can have spans in any sibling, and sessions are written with
// full message replacement, so reconstructing from a subset would overwrite
// previously indexed messages with a partial transcript. A transient sibling
// read error must surface so the sync is retried instead.
func TestParseVisualStudioCopilotTraceSession_PropagatesSiblingReadError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	// An unreadable sibling that still exists: a symlink to a directory opens
	// but cannot be scanned as JSONL, mimicking a transiently locked or
	// permission-denied trace file. It must not be silently skipped, because
	// the conversation may have spans in it.
	target := filepath.Join(t.TempDir(), "dir")
	require.NoError(t, os.Mkdir(target, 0o755))
	sibling := filepath.Join(
		dir,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.Symlink(target, sibling))

	_, _, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)
	require.Error(t, err,
		"an unreadable sibling must fail the parse instead of yielding a "+
			"partial transcript")
}

func TestParseVisualStudioCopilotTraceSession_MalformedTraceLineErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5", "1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Update the XAML."}]}]`,
		}) + "\n" + `{"resourceSpans":` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	_, _, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)
	require.Error(t, err,
		"a malformed non-empty trace line must fail the parse instead of "+
			"silently indexing a partial transcript")
	assert.Contains(t, err.Error(), "decode")
}

func TestParseVisualStudioCopilotTraceSession_DeduplicatesChatOutputAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`
	outputMessages := `[{"role":"assistant","parts":[{"type":"text","content":"The tests passed."}]}]`
	chatSpan := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     outputMessages,
			"gen_ai.usage.input_tokens":  "100",
			"gen_ai.usage.output_tokens": "20",
		}) + "\n"
	// The same chat span is flushed to both trace files for the conversation.
	require.NoError(t, os.WriteFile(path, []byte(chatSpan), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(chatSpan), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Run the tests.", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "The tests passed.", msgs[1].Content)
	// Usage from the duplicated span is counted once, not doubled.
	assert.Equal(t, 20, sess.TotalOutputTokens)
	assert.Equal(t, 100, sess.PeakContextTokens)
}

// TestParseVisualStudioCopilotTraceSession_PrefersCompleteChatOutputAcrossFiles
// verifies that one chat span flushed to sibling files with a growing payload
// emits a single assistant message carrying the complete output, with usage
// counted once. A streaming chat span can be exported mid-stream (partial text,
// interim token counts) and again at completion; keying dedup on span identity
// rather than identity-plus-content collapses these to the richest copy instead
// of emitting both and double-counting usage.
func TestParseVisualStudioCopilotTraceSession_PrefersCompleteChatOutputAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`
	partial := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     `[{"role":"assistant","parts":[{"type":"text","content":"The tests"}]}]`,
			"gen_ai.usage.input_tokens":  "100",
			"gen_ai.usage.output_tokens": "10",
		}) + "\n"
	complete := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     `[{"role":"assistant","parts":[{"type":"text","content":"The tests passed."}]}]`,
			"gen_ai.usage.input_tokens":  "100",
			"gen_ai.usage.output_tokens": "20",
		}) + "\n"
	// The same span is flushed partially to one file and completely to another.
	require.NoError(t, os.WriteFile(path, []byte(partial), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(complete), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, "The tests passed.", msgs[1].Content,
		"the complete chat output payload must win")
	// Usage is counted once, from the complete copy, not summed across flushes.
	assert.Equal(t, 20, sess.TotalOutputTokens)
	assert.Equal(t, 100, sess.PeakContextTokens)
}

// TestParseVisualStudioCopilotTraceSession_PrefersCompleteChatUsageForVisibleOutput
// verifies that when one chat turn is flushed to sibling files with identical
// visible output but different token usage, the copy carrying the more complete
// usage is the one whose tokens are recorded, even when it ended earlier than a
// less complete copy. Choosing the latest flush alone would apply the leaner
// usage and undercount the turn's tokens.
func TestParseVisualStudioCopilotTraceSession_PrefersCompleteChatUsageForVisibleOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`
	outputMessages := `[{"role":"assistant","parts":[{"type":"text","content":"The tests passed."}]}]`
	// Same span identity and identical visible output. The richer-usage copy
	// ended earlier; the copy that ended later carries lower token counts.
	richEarlier := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     outputMessages,
			"gen_ai.usage.input_tokens":  "100",
			"gen_ai.usage.output_tokens": "20",
		}) + "\n"
	leanerLater := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293620000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     outputMessages,
			"gen_ai.usage.input_tokens":  "100",
			"gen_ai.usage.output_tokens": "10",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(richEarlier), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(leanerLater), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.Equal(t, 20, msgs[1].OutputTokens,
		"the copy with more complete usage must win the tie")
	assert.Equal(t, 20, sess.TotalOutputTokens)
	assert.Equal(t, 100, sess.PeakContextTokens)
}

func TestParseVisualStudioCopilotTraceSession_DeduplicatesToolSpanAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir,
		"20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	// The same execute_tool span (same span id and tool call id) is flushed
	// to both trace files. The primary file caught it before the result was
	// recorded; the sibling holds the completed copy with the result.
	partial := vsCopilotTraceLineJSONWithSpanID(conversationID, "tool_run",
		"execute_tool run_command_in_terminal",
		"1781293588624985000", "1781293588769581200",
		map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        "call_123",
			"gen_ai.tool.call.arguments": `{"command":"go test ./..."}`,
		}) + "\n"
	complete := vsCopilotTraceLineJSONWithSpanID(conversationID, "tool_run",
		"execute_tool run_command_in_terminal",
		"1781293588624985000", "1781293588769581200",
		map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        "call_123",
			"gen_ai.tool.call.arguments": `{"command":"go test ./..."}`,
			"gen_ai.tool.call.result":    `{"Value":"ok"}`,
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(partial), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(complete), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	// The duplicate execute_tool span collapses to a single message...
	require.Len(t, msgs, 1)
	assert.True(t, msgs[0].HasToolUse)
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "run_command_in_terminal", msgs[0].ToolCalls[0].ToolName)
	// ...and the surviving copy is the completed one carrying the result.
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "ok", msgs[0].ToolCalls[0].ResultEvents[0].Content)
}

func TestParseVisualStudioCopilotTraceSession_PreservesOrderWhenDedupingToolSpans(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	// call_1 is traced twice: an early partial copy without a result and,
	// after an intervening second tool call, a completed copy carrying the
	// result. Deduplication must keep the completed payload without moving
	// the message ahead of the intervening call.
	partial := vsCopilotTraceLineJSONWithSpanID(conversationID, "span_1",
		"execute_tool run_command_in_terminal",
		"1781293588000000000", "1781293588100000000",
		map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        "call_1",
			"gen_ai.tool.call.arguments": `{"command":"go build ./..."}`,
		})
	intervening := vsCopilotTraceLineJSONWithSpanID(conversationID, "span_2",
		"execute_tool get_file",
		"1781293589000000000", "1781293589100000000",
		map[string]string{
			"gen_ai.tool.name":           "get_file",
			"gen_ai.tool.call.id":        "call_2",
			"gen_ai.tool.call.arguments": `{"filename":"main.go"}`,
			"gen_ai.tool.call.result":    `{"Value":"package main"}`,
		})
	complete := vsCopilotTraceLineJSONWithSpanID(conversationID, "span_1",
		"execute_tool run_command_in_terminal",
		"1781293590000000000", "1781293590100000000",
		map[string]string{
			"gen_ai.tool.name":           "run_command_in_terminal",
			"gen_ai.tool.call.id":        "call_1",
			"gen_ai.tool.call.arguments": `{"command":"go build ./..."}`,
			"gen_ai.tool.call.result":    `{"Value":"build ok"}`,
		})
	data := strings.Join(
		[]string{partial, intervening, complete}, "\n",
	) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	// call_1 collapses to one message; call_2 stays distinct.
	require.Len(t, msgs, 2)
	// Messages stay in non-decreasing timestamp order after dedup.
	assert.False(t, msgs[1].Timestamp.Before(msgs[0].Timestamp),
		"deduped tool message must not jump ahead of a later message")
	// The surviving call_1 message keeps the completed result...
	require.Len(t, msgs[0].ToolCalls, 1)
	assert.Equal(t, "run_command_in_terminal", msgs[0].ToolCalls[0].ToolName)
	require.Len(t, msgs[0].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "build ok", msgs[0].ToolCalls[0].ResultEvents[0].Content)
	// ...and the intervening call_2 follows it.
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "get_file", msgs[1].ToolCalls[0].ToolName)
}

func TestParseVisualStudioCopilotTraceSession_ChatOutputMessages(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Inspect the project and run tests."}]}]`
	outputMessages := `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_123","name":"run_command_in_terminal","arguments":"{\"command\":\"go test ./...\"}"},{"type":"text","content":"I'll inspect the project and run the tests."}],"finish_reason":"tool_call"}]`
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":  "chat",
			"gen_ai.request.model":   "gpt-5.5",
			"gen_ai.input.messages":  inputMessages,
			"gen_ai.output.messages": outputMessages,
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "Inspect the project and run tests.", sess.FirstMessage)
	require.Len(t, msgs, 2)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Inspect the project and run tests.", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.True(t, msgs[1].HasToolUse)
	assert.Contains(t, msgs[1].Content, "$ go test ./...")
	assert.Contains(t, msgs[1].Content,
		"I'll inspect the project and run the tests.")
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "run_command_in_terminal",
		msgs[1].ToolCalls[0].ToolName)
	assert.Contains(t, msgs[1].ToolCalls[0].InputJSON,
		"go test ./...")
	assert.JSONEq(t, `{"command":"go test ./..."}`,
		msgs[1].ToolCalls[0].InputJSON)
}

// TestParseVisualStudioCopilotTraceSession_CountsUsageForToolOnlyChatTurn
// verifies that an LLM turn whose only output is an executed tool call still
// contributes its token usage. The chat span carries the turn's usage but its
// tool call is suppressed from the chat output because the execute_tool span
// shows it with a result, so without dedicated handling the turn - and its
// tokens - would be dropped from the transcript and usage totals.
func TestParseVisualStudioCopilotTraceSession_CountsUsageForToolOnlyChatTurn(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Fix the build."}]}]`
	// The assistant turn's only output is a tool call, which is executed by the
	// execute_tool span below, so the chat output collapses to nothing.
	outputMessages := `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_build","name":"run_build","arguments":"{}"}],"finish_reason":"tool_call"}]`
	chatSpan := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.request.model":       "gpt-5.5",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     outputMessages,
			"gen_ai.usage.input_tokens":  "500",
			"gen_ai.usage.output_tokens": "42",
		}) + "\n"
	toolSpan := vsCopilotTraceLineJSONWithSpanID(conversationID, "tool_run",
		"execute_tool run_build",
		"1781293611000000000", "1781293612000000000",
		map[string]string{
			"gen_ai.tool.name":        "run_build",
			"gen_ai.tool.call.id":     "call_build",
			"gen_ai.tool.call.result": "Build succeeded.",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(chatSpan+toolSpan), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	// The tool-calling turn's tokens are counted even though its only output
	// is the executed tool call.
	assert.Equal(t, 42, sess.TotalOutputTokens)
	assert.Equal(t, 500, sess.PeakContextTokens)
	// The user prompt and the executed tool are both still present.
	require.NotEmpty(t, msgs)
	assert.Equal(t, "Fix the build.", msgs[0].Content)
	var sawTool bool
	for _, m := range msgs {
		for _, call := range m.ToolCalls {
			if call.ToolName == "run_build" {
				sawTool = true
			}
		}
	}
	assert.True(t, sawTool, "the executed tool must still be shown")
}

// TestParseVisualStudioCopilotTraceSession_DoesNotDoubleCountTextPlusToolUsage
// verifies that a chat turn carrying both assistant text and an executed tool
// call counts its token usage exactly once. The text message carries the usage;
// the separate execute_tool span (no usage of its own) must not add it again.
func TestParseVisualStudioCopilotTraceSession_DoesNotDoubleCountTextPlusToolUsage(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Fix the build."}]}]`
	// The assistant turn has both text and a tool call; the tool call is
	// executed by the execute_tool span below and suppressed from chat output.
	outputMessages := `[{"role":"assistant","parts":[{"type":"text","content":"Patched it."},{"type":"tool_call","id":"call_build","name":"run_build","arguments":"{}"}],"finish_reason":"tool_call"}]`
	chatSpan := vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.request.model":       "gpt-5.5",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     outputMessages,
			"gen_ai.usage.input_tokens":  "500",
			"gen_ai.usage.output_tokens": "42",
		}) + "\n"
	toolSpan := vsCopilotTraceLineJSONWithSpanID(conversationID, "tool_run",
		"execute_tool run_build",
		"1781293611000000000", "1781293612000000000",
		map[string]string{
			"gen_ai.tool.name":        "run_build",
			"gen_ai.tool.call.id":     "call_build",
			"gen_ai.tool.call.result": "Build succeeded.",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(chatSpan+toolSpan), 0o644))

	sess, _, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 42, sess.TotalOutputTokens,
		"the turn's output tokens must be counted once")
	assert.Equal(t, 500, sess.PeakContextTokens)
}

// TestParseVisualStudioCopilotTraceSession_PrefersCompleteToolOnlyChatUsage
// verifies that a tool-only chat turn flushed to sibling files with growing
// token counts records the most complete usage, not the first-seen partial copy.
func TestParseVisualStudioCopilotTraceSession_PrefersCompleteToolOnlyChatUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Fix the build."}]}]`
	outputMessages := `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_build","name":"run_build","arguments":"{}"}],"finish_reason":"tool_call"}]`
	chatSpan := func(in, out string) string {
		return vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_run",
			"chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name":      "chat",
				"gen_ai.request.model":       "gpt-5.5",
				"gen_ai.input.messages":      inputMessages,
				"gen_ai.output.messages":     outputMessages,
				"gen_ai.usage.input_tokens":  in,
				"gen_ai.usage.output_tokens": out,
			}) + "\n"
	}
	toolSpan := vsCopilotTraceLineJSONWithSpanID(conversationID, "tool_run",
		"execute_tool run_build",
		"1781293611000000000", "1781293612000000000",
		map[string]string{
			"gen_ai.tool.name":        "run_build",
			"gen_ai.tool.call.id":     "call_build",
			"gen_ai.tool.call.result": "Build succeeded.",
		}) + "\n"
	// The primary file carries the partial flush; the sibling carries the
	// complete one with higher token counts.
	require.NoError(t, os.WriteFile(path, []byte(chatSpan("200", "10")), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(chatSpan("500", "42")+toolSpan), 0o644))

	sess, _, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 42, sess.TotalOutputTokens,
		"the most complete usage copy must win")
	assert.Equal(t, 500, sess.PeakContextTokens)
}

func TestParseVisualStudioCopilotTraceSession_ChatUsage(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Run the tests."}]}]`
	outputMessages := `[{"role":"assistant","parts":[{"type":"text","content":"The tests passed."}]}]`
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.4",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":      "chat",
			"gen_ai.request.model":       "gpt-5.3-codex",
			"gen_ai.response.model":      "gpt-5.4-2026-03-05",
			"gen_ai.input.messages":      inputMessages,
			"gen_ai.output.messages":     outputMessages,
			"gen_ai.usage.input_tokens":  "11294",
			"gen_ai.usage.output_tokens": "241",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	require.Len(t, msgs, 2)
	assert.Equal(t, "gpt-5.4-2026-03-05", msgs[1].Model)
	assert.JSONEq(t,
		`{"input_tokens":11294,"output_tokens":241}`,
		string(msgs[1].TokenUsage),
	)
	assert.Equal(t, 11294, msgs[1].ContextTokens)
	assert.Equal(t, 241, msgs[1].OutputTokens)
	assert.True(t, msgs[1].HasContextTokens)
	assert.True(t, msgs[1].HasOutputTokens)
	assert.Equal(t, 241, sess.TotalOutputTokens)
	assert.Equal(t, 11294, sess.PeakContextTokens)
	assert.True(t, sess.HasTotalOutputTokens)
	assert.True(t, sess.HasPeakContextTokens)
}

func TestParseVisualStudioCopilotTraceSession_StandardToolInputs(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	data := strings.Join([]string{
		vsCopilotTraceLineJSON(conversationID,
			"execute_tool file_search",
			"1781293588624985000", "1781293588769581200",
			map[string]string{
				"gen_ai.tool.name":           "file_search",
				"gen_ai.tool.call.id":        "call_search",
				"gen_ai.tool.call.arguments": `{"queries":["ToolBlock","displayToolName"]}`,
			}),
		vsCopilotTraceLineJSON(conversationID,
			"execute_tool apply_patch",
			"1781293588769581201", "1781293588869581200",
			map[string]string{
				"gen_ai.tool.name":           "apply_patch",
				"gen_ai.tool.call.id":        "call_patch",
				"gen_ai.tool.call.arguments": `{"patch":"*** Begin Patch\n*** Update File: app.go\n@@\n-old\n+new\n*** End Patch"}`,
			}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	_, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.Len(t, msgs, 2)
	searchCall := msgs[0].ToolCalls[0]
	assert.Equal(t, "Grep", searchCall.Category)
	assert.JSONEq(t, `{"message":"ToolBlock, displayToolName","pattern":"ToolBlock, displayToolName","queries":["ToolBlock","displayToolName"]}`,
		searchCall.InputJSON)
	assert.NotContains(t, searchCall.InputJSON, `"arguments"`)

	editCall := msgs[1].ToolCalls[0]
	assert.Equal(t, "Edit", editCall.Category)
	assert.JSONEq(t, `{"patch":"*** Begin Patch\n*** Update File: app.go\n@@\n-old\n+new\n*** End Patch","file_path":"app.go","diff":"--- a/app.go\n+++ b/app.go\n@@\n-old\n+new"}`,
		editCall.InputJSON)
	assert.NotContains(t, editCall.InputJSON, `"arguments"`)
}

func TestParseVisualStudioCopilotTraceSession_UsesSiblingPromptSpan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir,
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir,
		"20260611T145207_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Remove the Details button and replace the expander with tabs."}]}]`
	primaryData := vsCopilotTraceLineJSON(conversationID,
		"invoke_agent GitHub Copilot",
		"1781293500000000000", "1781293510000000000",
		map[string]string{
			"gen_ai.request.model":    "gpt-5.5",
			"copilot_chat.mode":       "Agent",
			"copilot_chat.turn_count": "1",
		}) + "\n"
	siblingData := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":  "chat",
			"gen_ai.request.model":   "gpt-5.5",
			"gen_ai.input.messages":  inputMessages,
			"copilot_chat.client_id": "Microsoft.Modernization.Agent",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(primaryData), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(siblingData), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, "visualstudio-copilot:"+conversationID, sess.ID)
	assert.Equal(t,
		"Remove the Details button and replace the expander with tabs.",
		sess.FirstMessage)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t,
		"Remove the Details button and replace the expander with tabs.",
		msgs[0].Content)
	assert.Equal(t, path+"#"+conversationID, sess.File.Path)
}

func TestParseVisualStudioCopilotTraceSession_DeduplicatesPromptAndToolSpans(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145205_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Inline this into the XAML."}]}]`
	patchOutput := `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_patch","name":"apply_patch","arguments":"{\"patch\":\"*** Begin Patch\\n*** Update File: app.xaml\\n@@\\n-old\\n+new\\n*** End Patch\"}"},{"type":"text","content":"I’ll update the XAML."}],"finish_reason":"tool_call"}]`
	buildOutput := `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_build","name":"run_build","arguments":"{}"},{"type":"text","content":"I’ll build it now."}],"finish_reason":"tool_call"}]`
	data := strings.Join([]string{
		vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_patch",
			"chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name":  "chat",
				"gen_ai.input.messages":  inputMessages,
				"gen_ai.output.messages": patchOutput,
			}),
		vsCopilotTraceLineJSONWithSpanID(conversationID, "exec_patch",
			"execute_tool apply_patch",
			"1781293610000000001", "1781293611000000000",
			map[string]string{
				"gen_ai.tool.name":           "apply_patch",
				"gen_ai.tool.call.id":        "call_patch",
				"gen_ai.tool.call.arguments": `{"patch":"*** Begin Patch\n*** Update File: app.xaml\n@@\n-old\n+new\n*** End Patch"}`,
				"gen_ai.tool.call.result":    `{"Value":"Patch operation completed successfully."}`,
			}),
		vsCopilotTraceLineJSONWithSpanID(conversationID, "chat_build",
			"chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name":  "chat",
				"gen_ai.input.messages":  inputMessages,
				"gen_ai.output.messages": buildOutput,
			}),
		vsCopilotTraceLineJSONWithSpanID(conversationID, "exec_build_empty",
			"execute_tool run_build",
			"1781293630000000001", "1781293631000000000",
			map[string]string{
				"gen_ai.tool.name":           "run_build",
				"gen_ai.tool.call.id":        "call_build",
				"gen_ai.tool.call.arguments": `{}`,
			}),
		vsCopilotTraceLineJSONWithSpanID(conversationID, "exec_build_result",
			"execute_tool run_build",
			"1781293631000000001", "1781293632000000000",
			map[string]string{
				"gen_ai.tool.name":           "run_build",
				"gen_ai.tool.call.id":        "call_build",
				"gen_ai.tool.call.arguments": `{}`,
				"gen_ai.tool.call.result":    `{"Value":"Build successful"}`,
			}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 1, sess.UserMessageCount)
	require.Len(t, msgs, 5)
	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Inline this into the XAML.", msgs[0].Content)
	assert.Equal(t, RoleAssistant, msgs[1].Role)
	assert.False(t, msgs[1].HasToolUse)
	assert.Equal(t, "I’ll update the XAML.", msgs[1].Content)
	assert.Equal(t, RoleAssistant, msgs[2].Role)
	assert.True(t, msgs[2].HasToolUse)
	assert.Equal(t, "call_patch", msgs[2].ToolCalls[0].ToolUseID)
	assert.Equal(t, RoleAssistant, msgs[3].Role)
	assert.False(t, msgs[3].HasToolUse)
	assert.Equal(t, "I’ll build it now.", msgs[3].Content)
	assert.Equal(t, RoleAssistant, msgs[4].Role)
	assert.True(t, msgs[4].HasToolUse)
	assert.Equal(t, "call_build", msgs[4].ToolCalls[0].ToolUseID)
	require.Len(t, msgs[4].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "Build successful", msgs[4].ToolCalls[0].ResultEvents[0].Content)
}

func TestParseVisualStudioCopilotTraceSession_ChatSummaryFallback(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145207_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	conversationID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	data := vsCopilotTraceLineJSON(conversationID,
		"chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name":        "chat",
			"gen_ai.request.model":         "gpt-5.5",
			"gen_ai.input.messages":        `not-json`,
			"copilot_chat.client_id":       "Microsoft.Modernization.Agent",
			"copilot_chat.root_request_id": "398c5816-cfb4-4f51-a195-16e7f03edc69",
		}) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	sess, msgs, err := parseVisualStudioCopilotTestSession(t,
		path, "visualstudio", "local",
	)

	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t,
		"Visual Studio Copilot chat | Agent | gpt-5.5 | 398c5816",
		sess.FirstMessage)
	require.Len(t, msgs, 1)
	assert.Equal(t, sess.FirstMessage, msgs[0].Content)
}

func TestParseVisualStudioCopilotConversation_ParsesEachConversationIndependently(t *testing.T) {
	path := filepath.Join(
		t.TempDir(),
		"20260611T145207_d9b231f1_VSGitHubCopilot_traces.jsonl",
	)
	promptID := "1c4ff921-fa0c-46f6-a043-c282c49761da"
	ambientID := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"
	inputMessages := `[{"role":"user","parts":[{"type":"text","content":"Make the update screen calmer and easier to scan."}]}]`
	data := strings.Join([]string{
		vsCopilotTraceLineJSONWithSpanID(ambientID, "ambient",
			"invoke_agent GitHub Copilot",
			"1781293500000000000", "1781293510000000000",
			map[string]string{
				"gen_ai.response.model":        "gpt-5.3-codex",
				"copilot_chat.client_id":       "Microsoft.VisualStudio.Copilot.CodeMappers.PatchHealService",
				"copilot_chat.root_request_id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			}),
		vsCopilotTraceLineJSONWithSpanID(promptID, "prompt",
			"chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name":  "chat",
				"gen_ai.request.model":   "gpt-5.5",
				"gen_ai.input.messages":  inputMessages,
				"copilot_chat.client_id": "Microsoft.Modernization.Agent",
			}),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	// The prompt conversation parses with its user message.
	promptSess, _, err := parseVisualStudioCopilotTestConversation(t,
		path, promptID, "visualstudio", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, promptSess)
	assert.Equal(t, "visualstudio-copilot:"+promptID, promptSess.ID)
	assert.Equal(t, path+"#"+promptID, promptSess.File.Path)
	assert.Equal(t,
		"Make the update screen calmer and easier to scan.",
		promptSess.FirstMessage)

	// The ambient conversation in the same file is not dropped; it
	// parses on its own with its invoke_agent turn.
	ambientSess, ambientMsgs, err := parseVisualStudioCopilotTestConversation(t,
		path, ambientID, "visualstudio", "local",
	)
	require.NoError(t, err)
	require.NotNil(t, ambientSess)
	assert.Equal(t, "visualstudio-copilot:"+ambientID, ambientSess.ID)
	assert.Equal(t, path+"#"+ambientID, ambientSess.File.Path)
	require.NotEmpty(t, ambientMsgs)
}

func TestFindVisualStudioCopilotSourceFile(t *testing.T) {
	dir := t.TempDir()
	uuid := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tracesDir := filepath.Join(dir, "VSGitHubCopilotLogs", "traces")
	require.NoError(t, os.MkdirAll(tracesDir, 0o755))
	oldTrace := filepath.Join(
		tracesDir,
		"20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	newTrace := filepath.Join(
		tracesDir,
		"20260612T194441_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	traceLine := vsCopilotTraceLineJSON(uuid,
		"invoke_agent GitHub Copilot", "1", "2", nil)
	require.NoError(t, os.WriteFile(oldTrace,
		[]byte(traceLine+"\n"), 0o644))
	require.NoError(t, os.WriteFile(newTrace,
		[]byte(traceLine+"\n"), 0o644))

	assert.Equal(t, VisualStudioCopilotVirtualPath(newTrace, uuid),
		findVisualStudioCopilotTestSourceFile(t, tracesDir, uuid),
		"source lookup must return a conversation-scoped virtual path so a "+
			"single-session resync does not reparse the whole trace file")
	assert.Equal(t, "",
		findVisualStudioCopilotTestSourceFile(t, dir, uuid))
	assert.Equal(t, "",
		findVisualStudioCopilotTestSourceFile(t, tracesDir, "../etc/passwd"))
}

// TestWriteVisualStudioCopilotConversationJSONL verifies that exporting one
// conversation from a shared trace file emits only that conversation's lines and
// never discloses spans belonging to another conversation in the same file.
func TestWriteVisualStudioCopilotConversationJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	convA := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	convB := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"
	lineA := vsCopilotTraceLineJSONWithSpanID(convA, "a1", "chat gpt-5.5",
		"1781293600000000000", "1781293610000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Conversation A prompt."}]}]`,
		})
	lineB := vsCopilotTraceLineJSONWithSpanID(convB, "b1", "chat gpt-5.5",
		"1781293620000000000", "1781293630000000000",
		map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Other conversation secret B."}]}]`,
		})
	data := lineA + "\n" + lineB + "\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o644))

	var buf bytes.Buffer
	require.NoError(
		t, WriteVisualStudioCopilotConversationJSONL(&buf, path, convA),
	)

	out := buf.String()
	assert.Contains(t, out, "Conversation A prompt.")
	assert.NotContains(t, out, convB,
		"another conversation's id must not be exported")
	assert.NotContains(t, out, "Other conversation secret B.",
		"another conversation's content must not be exported")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	require.Len(t, lines, 1, "only the requested conversation's line is emitted")
	assert.JSONEq(t, lineA, lines[0],
		"the matching line is emitted verbatim")
}

// vsCopilotSpanJSON builds one OTLP span object carrying a marker attribute,
// optionally tagged with a conversation id. Used to assemble batched trace lines
// that hold several spans.
func vsCopilotSpanJSON(conversationID, spanID, marker string) string {
	var attrs []string
	if conversationID != "" {
		attrs = append(attrs,
			`{"key":"gen_ai.conversation.id","value":{"stringValue":"`+
				conversationID+`"}}`)
	}
	attrs = append(attrs,
		`{"key":"copilot_marker","value":{"stringValue":"`+marker+`"}}`)
	return `{"traceId":"trace","spanId":"` + spanID +
		`","name":"chat gpt-5.5","startTimeUnixNano":"1781293600000000000",` +
		`"endTimeUnixNano":"1781293610000000000","attributes":[` +
		strings.Join(attrs, ",") + `]}`
}

// TestWriteVisualStudioCopilotConversationJSONLMissingConversationNotFound
// verifies that exporting a conversation that no current trace file contains
// reports a not-found error instead of succeeding with empty output, so the
// export command surfaces a clear error when the source data is gone.
func TestWriteVisualStudioCopilotConversationJSONLMissingConversationNotFound(
	t *testing.T,
) {
	dir := t.TempDir()
	path := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(path, []byte(
		vsCopilotSpanJSONLine("c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28",
			"x1", "Some other conversation.")+"\n"), 0o644))

	var buf bytes.Buffer
	err := WriteVisualStudioCopilotConversationJSONL(
		&buf, path, "4a8f63f6-7626-4416-a874-fc7bd2c3f005",
	)
	require.ErrorIs(t, err, os.ErrNotExist,
		"exporting a conversation absent from every trace file must report "+
			"not found, not succeed with empty output")
	assert.Empty(t, buf.String())
}

// vsCopilotSpanJSONLine wraps a single span built by vsCopilotSpanJSON in a full
// OTLP trace line.
func vsCopilotSpanJSONLine(conversationID, spanID, marker string) string {
	return `{"resourceSpans":[{"scopeSpans":[{"spans":[` +
		vsCopilotSpanJSON(conversationID, spanID, marker) + `]}]}]}`
}

// TestWriteVisualStudioCopilotConversationJSONLFiltersSpansWithinLine verifies
// that a single OTLP line batching spans for several conversations (plus a span
// with no conversation id) exports only the requested conversation's span,
// neither disclosing the others nor losing the requested span.
func TestWriteVisualStudioCopilotConversationJSONLFiltersSpansWithinLine(
	t *testing.T,
) {
	dir := t.TempDir()
	path := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	convA := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	convB := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"
	line := `{"resourceSpans":[{"scopeSpans":[{"spans":[` +
		vsCopilotSpanJSON(convA, "a1", "Wanted A content.") + "," +
		vsCopilotSpanJSON(convB, "b1", "Other conversation secret.") + "," +
		vsCopilotSpanJSON("", "n1", "Id-less span data.") +
		`]}]}]}`
	require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o644))

	var buf bytes.Buffer
	require.NoError(
		t, WriteVisualStudioCopilotConversationJSONL(&buf, path, convA),
	)
	out := strings.TrimSpace(buf.String())

	assert.Contains(t, out, "Wanted A content.",
		"the requested span must be kept even when batched with others")
	assert.NotContains(t, out, "Other conversation secret.",
		"another conversation's span must not be disclosed")
	assert.NotContains(t, out, "Id-less span data.",
		"a span with no conversation id must not be disclosed")
	assert.NotContains(t, out, convB)
	require.NotEmpty(t, out)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &parsed),
		"the re-encoded line must be valid JSON")
}

// TestWriteVisualStudioCopilotConversationJSONLTraversesSiblings verifies that a
// conversation split across rotated sibling trace files is exported in full, not
// just from the representative trace file.
func TestWriteVisualStudioCopilotConversationJSONLTraversesSiblings(
	t *testing.T,
) {
	dir := t.TempDir()
	conv := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	other := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"
	primary := filepath.Join(
		dir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	sibling := filepath.Join(
		dir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotTraceLineJSONWithSpanID(conv, "a1", "chat gpt-5.5",
			"1781293600000000000", "1781293610000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"First half."}]}]`,
			})+"\n"), 0o644))
	require.NoError(t, os.WriteFile(sibling, []byte(strings.Join([]string{
		vsCopilotTraceLineJSONWithSpanID(conv, "a2", "chat gpt-5.5",
			"1781293620000000000", "1781293630000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Second half."}]}]`,
			}),
		vsCopilotTraceLineJSONWithSpanID(other, "b1", "chat gpt-5.5",
			"1781293640000000000", "1781293650000000000",
			map[string]string{
				"gen_ai.operation.name": "chat",
				"gen_ai.input.messages": `[{"role":"user","parts":[{"type":"text","content":"Unrelated conversation."}]}]`,
			}),
	}, "\n")+"\n"), 0o644))

	var buf bytes.Buffer
	require.NoError(
		t, WriteVisualStudioCopilotConversationJSONL(&buf, primary, conv),
	)
	out := buf.String()

	assert.Contains(t, out, "First half.")
	assert.Contains(t, out, "Second half.",
		"spans from sibling trace files must be exported")
	assert.NotContains(t, out, "Unrelated conversation.")
	assert.NotContains(t, out, other)
}

func vsCopilotTraceLineJSON(
	conversationID, name, start, end string,
	attrs map[string]string,
) string {
	return vsCopilotTraceLineJSONWithSpanID(
		conversationID, "span", name, start, end, attrs,
	)
}

func vsCopilotTraceLineJSONWithSpanID(
	conversationID, spanID, name, start, end string,
	attrs map[string]string,
) string {
	allAttrs := []string{
		`{"key":"gen_ai.conversation.id","value":{"stringValue":"` +
			conversationID + `"}}`,
	}
	for key, value := range attrs {
		encoded, _ := json.Marshal(value)
		allAttrs = append(allAttrs,
			`{"key":"`+key+`","value":{"stringValue":`+
				string(encoded)+`}}`,
		)
	}
	return `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"trace","spanId":"` + spanID + `","name":"` +
		name + `","startTimeUnixNano":"` + start +
		`","endTimeUnixNano":"` + end +
		`","attributes":[` + strings.Join(allAttrs, ",") +
		`] }]}]}]}`
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
