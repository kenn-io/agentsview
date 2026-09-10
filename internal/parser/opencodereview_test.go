package parser

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCodeReviewIssue1612Sample(t *testing.T) {
	raw := openCodeReviewFixtureBytes(t)
	root := t.TempDir()
	path := filepath.Join(root, "Users-reviewer-src-wagtail-wagtail", "efb23cb0-2f65-4f32-9f9d-310b0614b737.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, raw, 0o644))

	provider, ok := NewProvider(AgentOpenCodeReview, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	assert.Equal(t, CapabilitySupported, provider.Capabilities().Source.ForceReplaceOnParse)
	discovered, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	fingerprint, err := provider.Fingerprint(t.Context(), discovered[0])
	require.NoError(t, err)
	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      discovered[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result

	var toolEvents int
	for _, message := range result.Messages {
		for _, call := range message.ToolCalls {
			toolEvents += len(call.ResultEvents)
		}
	}
	assert.Equal(t, 1, result.Session.UserMessageCount)
	assert.Equal(t, 3, countMessagesWithRole(result.Messages, RoleAssistant))
	assert.Equal(t, 2, toolEvents)
	fileReadCall := findOpenCodeReviewToolCallByID(t, result.Messages, "call_94ddb2fe02f9436fa02c875d")
	codeSearchCall := findOpenCodeReviewToolCallByID(t, result.Messages, "call_99b138b06835486190c8fbee")
	assert.Equal(t, "call_94ddb2fe02f9436fa02c875d", fileReadCall.ToolUseID)
	assert.Equal(t, "call_94ddb2fe02f9436fa02c875d", fileReadCall.ResultEvents[0].ToolUseID)
	assert.Equal(t, "call_99b138b06835486190c8fbee", codeSearchCall.ToolUseID)
	assert.Equal(t, "call_99b138b06835486190c8fbee", codeSearchCall.ResultEvents[0].ToolUseID)
	assert.Equal(t, "wagtail", result.Session.Project)
	assert.Equal(t, "commit", result.Session.SessionKind)
	assert.Equal(t, "deepseek/deepseek-v4-flash-0731", result.Messages[1].Model)
	assert.Equal(t, "v1.9.2", result.Session.SourceVersion)
	assert.Equal(t, "efb23cb0-2f65-4f32-9f9d-310b0614b737", result.Session.SourceSessionID)
	assert.Equal(t, "Review commit eceeedd4e4", result.Session.SessionName)
	assert.True(t, result.Messages[1].HasContextTokens)
	assert.Equal(t, 4767, result.Messages[1].ContextTokens)
	assert.Equal(t, 5442, result.Messages[2].ContextTokens)
	assert.Equal(t, 5717, result.Messages[3].ContextTokens)
	assert.Contains(t, string(result.Messages[1].TokenUsage), `"input_tokens":4767`)
	assert.Contains(t, string(result.Messages[2].TokenUsage), `"cache_read_input_tokens":4864`)
	assert.Contains(t, string(result.Messages[2].TokenUsage), `"input_tokens":578`)
	assert.Contains(t, string(result.Messages[3].TokenUsage), `"cache_creation_input_tokens":0`)
	assert.True(t, result.Messages[3].HasContextTokens)
	assert.Equal(t, TerminationClean, result.Session.TerminationStatus)
	assert.Equal(t, 11, countFixtureRecords(t, raw))
	fmt.Printf(
		"OpenCodeReview issue sample: sources=%d sessions=%d user_messages=%d assistant_responses=%d tool_result_events=%d source_version=%s title=%q second_context_tokens=%d termination=%s\n",
		len(discovered), len(outcome.Results), result.Session.UserMessageCount,
		countMessagesWithRole(result.Messages, RoleAssistant), toolEvents,
		result.Session.SourceVersion, result.Session.SessionName,
		result.Messages[2].ContextTokens, result.Session.TerminationStatus,
	)
}

func TestOpenCodeReviewTaskDoneWithoutSessionEnd(t *testing.T) {
	raw := openCodeReviewFixtureBytes(t)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o644))
	result := openCodeReviewParseForTest(t, path)
	assert.Equal(t, TerminationClean, result.Session.TerminationStatus)
	call := findOpenCodeReviewToolCall(t, result.Messages, "task_done", "")
	assert.NotEmpty(t, call.ToolUseID)
	assert.Empty(t, call.ResultEvents, "task_done has no execution result upstream")

	for _, tc := range []struct {
		name   string
		result string
		want   TerminationStatus
	}{
		{"pending sibling", "", TerminationToolCallPending},
		{"completed sibling", `{"type":"tool_call","tool_name":"file_read","result":"file contents","ok":true}` + "\n", TerminationClean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := `{"type":"session_start","sessionId":"review"}` + "\n" +
				`{"type":"llm_response","tool_calls":[{"id":"read","name":"file_read","arguments":"{}"},{"id":"done","name":"task_done","arguments":"{}"}]}` + "\n" + tc.result
			require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
			assert.Equal(t, tc.want, openCodeReviewParseForTest(t, path).Session.TerminationStatus)
		})
	}
}

func TestOpenCodeReviewCacheWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// The native Anthropic adapter includes cache reads and writes in prompt_tokens.
	content := `{"type":"session_start","sessionId":"review"}` + "\n" +
		`{"type":"llm_response","content":"Review complete","usage":{"prompt_tokens":130,"completion_tokens":5,"cache_read_tokens":80,"cache_write_tokens":30}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	result := openCodeReviewParseForTest(t, path)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, 130, result.Messages[0].ContextTokens)
	assert.JSONEq(t, `{"input_tokens":20,"output_tokens":5,"cache_read_input_tokens":80,"cache_creation_input_tokens":30}`, string(result.Messages[0].TokenUsage))
}

func TestOpenCodeReviewCompressionAndAuxiliaryPrompts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []string{
		`{"type":"session_start","sessionId":"review"}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go"},{"role":"assistant","content":"Reading"},{"role":"tool","content":"File contents"}]}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"memory_compression_task","messages":[{"role":"user","content":"Compress <context>conversation</context>"}]}`,
		`{"type":"llm_response","filePath":"a.go","taskType":"memory_compression_task","content":"Summary","usage":{"prompt_tokens":100,"completion_tokens":10}}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go\n\n<previous_review_summary>Summary</previous_review_summary>"}]}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go\n\n<previous_review_summary>Summary</previous_review_summary>"},{"role":"assistant","content":"No tools"},{"role":"user","content":"Please try again or use task_done if finished."}]}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"re_location_task","messages":[{"role":"user","content":"Locate comment"}]}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"review_filter_task","messages":[{"role":"user","content":"Filter comments"}]}`,
		`{"type":"llm_request","taskType":"grouping_task","messages":[{"role":"user","content":"Group files"}]}`,
		`{"type":"llm_request","filePath":"b.go","taskType":"plan_task","messages":[{"role":"user","content":"Plan b.go"}]}`,
		`{"type":"llm_request","filePath":"b.go","taskType":"main_task","messages":[{"role":"user","content":"Review b.go"}]}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	result := openCodeReviewParseForTest(t, path)
	assert.Equal(t, 4, result.Session.UserMessageCount)
	assert.Equal(t, []string{"Review a.go", "Summary", "Please try again or use task_done if finished.", "Plan b.go", "Review b.go"}, messageContents(result.Messages))
	assert.Equal(t, 100, result.Messages[1].ContextTokens, "auxiliary response usage is retained")
}

func TestOpenCodeReviewUserTurnsAfterHistoryReset(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		request  string
		want     string
		users    int
	}{
		{
			name:     "compression followed by grace round",
			response: `{"type":"llm_response","filePath":"a.go","taskType":"main_task","content":"Reading","tool_calls":[{"id":"read","name":"file_read","arguments":"{}"}]}`,
			request:  `{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go\n\n<previous_review_summary>Summary</previous_review_summary>"},{"role":"assistant","content":"Reading"},{"role":"tool","content":"File contents"},{"role":"user","content":"Your tool-call budget is exhausted. This is your FINAL round."}]}`,
			want:     "Your tool-call budget is exhausted. This is your FINAL round.",
			users:    2,
		},
		{
			name:     "new review round on same stream",
			response: `{"type":"llm_response","filePath":"a.go","taskType":"main_task","tool_calls":[{"id":"done","name":"task_done","arguments":"{\"state\":\"DONE\"}"}]}`,
			request:  `{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go with the confirmed findings from round one"}]}`,
			want:     "Review a.go with the confirmed findings from round one",
			users:    2,
		},
		{
			name:     "invalid completion keeps existing history",
			response: `{"type":"llm_response","filePath":"a.go","taskType":"main_task","content":"Retrying","tool_calls":[{"id":"done","name":"task_done","arguments":"{\"state\":null}"}]}`,
			request:  `{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go"},{"role":"assistant","content":"Read file"},{"role":"tool","content":"File contents"},{"role":"assistant","content":"Search code"},{"role":"tool","content":"Search results"},{"role":"assistant","content":"Retrying"},{"role":"tool","content":"Error: task_done state must be DONE or FAILED."}]}`,
			want:     "Retrying",
			users:    1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			lines := []string{
				`{"type":"session_start","sessionId":"review"}`,
				`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"system","content":"Review code"},{"role":"user","content":"Review a.go"},{"role":"assistant","content":"Read file"},{"role":"tool","content":"File contents"},{"role":"assistant","content":"Search code"},{"role":"tool","content":"Search results"}]}`,
				tc.response,
				tc.request,
				tc.request,
			}
			require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
			result := openCodeReviewParseForTest(t, path)
			assert.Equal(t, tc.users, result.Session.UserMessageCount)
			assert.Equal(t, tc.want, result.Messages[len(result.Messages)-1].Content)
		})
	}
}

func TestOpenCodeReviewProviderAdmissionAndLookup(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "project", "11111111-1111-4111-8111-111111111111.jsonl")
	paths := map[string]string{
		valid: filepath.Join(root, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
		filepath.Join(root, "project", "99999999-9999-4999-8999-999999999999.jsonl"):           "foreign",
		filepath.Join(root, "root.jsonl"):                                                      "",
		filepath.Join(root, "project", "nested", "22222222-2222-4222-8222-222222222222.jsonl"): "",
		filepath.Join(root, "project", "33333333-3333-4333-8333-333333333333.txt"):             "",
		filepath.Join(root, "project", "invalid.session.jsonl"):                                "",
	}
	for path := range paths {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		if path == valid {
			require.NoError(t, os.WriteFile(path, []byte(`{"type":"session_start","sessionId":"11111111-1111-4111-8111-111111111111"}`+"\n"), 0o644))
		} else if paths[path] == "foreign" {
			require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"foreign transcript"}}`+"\n"), 0o644))
		} else {
			require.NoError(t, os.WriteFile(path, []byte("x\n"), 0o644))
		}
	}
	sources := newOpenCodeReviewSourceSet([]string{root})
	discovered, err := sources.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, discovered, 2)
	discoveredPaths := []string{discovered[0].DisplayPath, discovered[1].DisplayPath}
	assert.Contains(t, discoveredPaths, valid)
	foreignPath := filepath.Join(root, "project", "99999999-9999-4999-8999-999999999999.jsonl")
	foreignInfo, err := os.Stat(foreignPath)
	require.NoError(t, err)
	foreignResult, err := parseOpenCodeReviewFile(t.Context(), foreignPath, "project", "local", SourceFingerprint{Size: foreignInfo.Size(), MTimeNS: foreignInfo.ModTime().UnixNano()})
	require.NoError(t, err)
	assert.Nil(t, foreignResult)

	found, ok, err := sources.FindSource(t.Context(), FindSourceRequest{RawSessionID: "11111111-1111-4111-8111-111111111111"})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, valid, found.DisplayPath)
	changed, err := sources.SourcesForChangedPath(t.Context(), ChangedPathRequest{Path: valid, EventKind: "write"})
	require.NoError(t, err)
	require.Len(t, changed, 1)
}

func TestOpenCodeReviewReplayAndPartialLines(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	lines := []string{
		`{"type":"session_start","sessionId":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","cwd":"/src/project","reviewMode":"commit","diffCommit":"abc"}`,
		`{"type":"llm_request","uuid":"r1","filePath":"a.go","taskType":"main_task","messages":[{"role":"user","content":"one"}]}`,
		`{"type":"future_record","value":"ignored"}`,
		`{"type":"malformed"`,
		`{"type":"llm_response","uuid":"a1","filePath":"a.go","taskType":"main_task","content":"answer"}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	full := openCodeReviewParseForTest(t, path)
	require.Len(t, full.Messages, 2)
	assert.Equal(t, 1, full.Session.MalformedLines)

	appendLine := `{"type":"llm_request","uuid":"r2","filePath":"a.go","taskType":"main_task","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"answer"},{"role":"user","content":"two"}]}` + "\n"
	require.NoError(t, os.WriteFile(path, append(readFileTest(t, path), []byte(appendLine)...), 0o644))
	appended := openCodeReviewParseForTest(t, path)
	assert.Equal(t, 2, appended.Session.UserMessageCount)

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n{"), 0o644))
	partial := openCodeReviewParseForTest(t, path)
	assert.True(t, partial.Session.IsTruncated)
	assert.Equal(t, 0, partial.Session.MalformedLines)
	assert.Equal(t, TerminationTruncated, partial.Session.TerminationStatus)

	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o644))
	rewritten := openCodeReviewParseForTest(t, path)
	assert.False(t, rewritten.Session.IsTruncated)
	assert.Equal(t, 1, rewritten.Session.UserMessageCount)
}

func TestOpenCodeReviewRequestSuffixAndStreamPairing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []string{
		`{"type":"session_start","sessionId":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"user","content":"one"}]}`,
		`{"type":"llm_response","uuid":"a1","filePath":"a.go","taskType":"main_task","content":"a","tool_calls":[{"id":"call_a","name":"file_read","arguments":{"path":"a"}}]}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"a"},{"role":"user","content":"two"}]}`,
		`{"type":"llm_request","filePath":"a.go","taskType":"main_task","messages":[{"role":"user","content":"one"},{"role":"assistant","content":"a"},{"role":"user","content":"two"}]}`,
		`{"type":"llm_response","uuid":"a2","filePath":"a.go","taskType":"main_task","content":"b","tool_calls":[{"id":"call_b","name":"file_read","arguments":{"path":"b"}}]}`,
		`{"type":"llm_response","uuid":"p1","filePath":"b.go","taskType":"plan_task","content":"p","tool_calls":[{"id":"call_p","name":"file_read","arguments":{"path":"p"}}]}`,
		`{"type":"tool_call","filePath":"a.go","taskType":"main_task","tool_name":"file_read","result":"a result","ok":true}`,
		`{"type":"tool_call","filePath":"b.go","taskType":"plan_task","tool_name":"file_read","result":"p result","ok":true}`,
		`{"type":"tool_call","filePath":"a.go","taskType":"main_task","tool_name":"missing","result":"orphan","ok":false}`,
		`{"type":"llm_response","uuid":"pending","filePath":"a.go","taskType":"main_task","content":"waiting","tool_calls":[{"id":"call_pending","name":"file_read","arguments":{"path":"pending"}}]}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	result := openCodeReviewParseForTest(t, path)
	assert.Equal(t, 2, result.Session.UserMessageCount)
	aCall := findOpenCodeReviewToolCall(t, result.Messages, "file_read", `"path":"a"`)
	pCall := findOpenCodeReviewToolCall(t, result.Messages, "file_read", `"path":"p"`)
	assert.Equal(t, "Read", aCall.Category)
	assert.Equal(t, "call_a", aCall.ToolUseID)
	assert.Equal(t, "tool_execution", aCall.ResultEvents[0].Source)
	assert.Equal(t, "completed", aCall.ResultEvents[0].Status)
	assert.Equal(t, "a result", aCall.ResultEvents[0].Content)
	assert.Equal(t, "call_a", aCall.ResultEvents[0].ToolUseID)
	assert.Equal(t, "p result", pCall.ResultEvents[0].Content)
	assert.Equal(t, "call_p", pCall.ResultEvents[0].ToolUseID)
	assert.Contains(t, result.Messages[len(result.Messages)-2].Content, "orphan")
	assert.Equal(t, RoleSystem, result.Messages[len(result.Messages)-2].Role)
	assert.Equal(t, "completed", pCall.ResultEvents[0].Status)
	pendingCall := findOpenCodeReviewToolCall(t, result.Messages, "file_read", `"path":"pending"`)
	assert.Equal(t, "call_pending", pendingCall.ToolUseID)
	assert.Equal(t, TerminationToolCallPending, result.Session.TerminationStatus)
}

func TestOpenCodeReviewForceReplacesAppendedToolResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "dddddddd-dddd-4ddd-8ddd-dddddddddddd.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	initial := strings.Join([]string{
		`{"type":"session_start","sessionId":"dddddddd-dddd-4ddd-8ddd-dddddddddddd"}`,
		`{"type":"llm_response","uuid":"response","filePath":"review.go","taskType":"main_task","content":"checking","tool_calls":[{"id":"call_review","name":"file_read","arguments":{"path":"review.go"}}]}`,
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o644))

	provider, ok := NewProvider(AgentOpenCodeReview, ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	initialFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	first, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: initialFingerprint})
	require.NoError(t, err)
	require.Len(t, first.Results, 1)
	initialCall := findOpenCodeReviewToolCallByID(t, first.Results[0].Result.Messages, "call_review")
	assert.Empty(t, initialCall.ResultEvents)

	appended := `{"type":"tool_call","filePath":"review.go","taskType":"main_task","tool_name":"file_read","result":"review contents","ok":true}` + "\n"
	require.NoError(t, os.WriteFile(path, append(readFileTest(t, path), []byte(appended)...), 0o644))
	updatedFingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(t, err)
	second, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0], Fingerprint: updatedFingerprint})
	require.NoError(t, err)
	require.True(t, second.ForceReplace)
	require.Len(t, second.Results, 1)
	updatedCall := findOpenCodeReviewToolCallByID(t, second.Results[0].Result.Messages, "call_review")
	require.Len(t, updatedCall.ResultEvents, 1)
	assert.Equal(t, "review contents", updatedCall.ResultEvents[0].Content)
}

func TestOpenCodeReviewMetadataAndContinuation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []string{
		`{"type":"session_start","sessionId":"cccccccc-cccc-4ccc-8ccc-cccccccccccc","cwd":"/repo/project","gitBranch":"main","model":"review-model","reviewMode":"range","diffFrom":"base","diffTo":"head","resumedFrom":"parent-run"}`,
		`{"type":"resume_lineage","parent_run_id":"parent-run"}`,
		`{"type":"llm_response","model":"review-model","content":"response"}`,
		`{"type":"review_item_reused","sourceSessionId":"older","comments":[{"path":"a.go","start_line":12,"end_line":14,"severity":"high","content":"reused comment","suggestion_code":"return err"}]}`,
		`{"type":"review_item_failed","error":"failed review"}`,
		`{"type":"session_end","timestamp":"2026-01-01T00:00:01Z","run_manifest":{"execution":{"ocr_version":"v1.9.2"}}}`,
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	result := openCodeReviewParseForTest(t, path)
	assert.Equal(t, "opencodereview:parent-run", result.Session.ParentSessionID)
	assert.Equal(t, RelContinuation, result.Session.RelationshipType)
	assert.Equal(t, "review-model", result.Messages[0].Model)
	assert.Equal(t, "base..head", strings.TrimPrefix(result.Session.SessionName, "Review "))
	assert.Equal(t, "v1.9.2", result.Session.SourceVersion)
	assert.Equal(t, "cccccccc-cccc-4ccc-8ccc-cccccccccccc", result.Session.SourceSessionID)
	assert.Contains(t, strings.Join(messageContents(result.Messages), "\n"), "reused comment")
	assert.JSONEq(t, `{"sourceSessionId":"older","comments":[{"path":"a.go","start_line":12,"end_line":14,"severity":"high","content":"reused comment","suggestion_code":"return err"}]}`, result.Messages[1].Content)
	assert.Contains(t, strings.Join(messageContents(result.Messages), "\n"), "failed review")
}

func openCodeReviewFixtureBytes(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("AGENTSVIEW_OCR_REPRO")
	if path == "" {
		_, current, _, ok := runtime.Caller(0)
		require.True(t, ok)
		path = filepath.Join(filepath.Dir(current), "testdata", "opencodereview", "Users-reviewer-src-wagtail-wagtail", "efb23cb0-2f65-4f32-9f9d-310b0614b737.jsonl")
	}
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func openCodeReviewParseForTest(t *testing.T, path string) ParseResult {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	parsed, err := parseOpenCodeReviewFile(t.Context(), path, "project", "local", SourceFingerprint{Size: info.Size(), MTimeNS: info.ModTime().UnixNano()})
	require.NoError(t, err)
	require.NotNil(t, parsed)
	return *parsed
}

func readFileTest(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

func countMessagesWithRole(messages []ParsedMessage, role RoleType) int {
	count := 0
	for _, message := range messages {
		if message.Role == role {
			count++
		}
	}
	return count
}

func messageContents(messages []ParsedMessage) []string {
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

func findOpenCodeReviewToolCall(t *testing.T, messages []ParsedMessage, name, path string) ParsedToolCall {
	t.Helper()
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ToolName == name && strings.Contains(call.InputJSON, path) {
				return call
			}
		}
	}
	t.Fatalf("tool call %s for %s not found", name, path)
	return ParsedToolCall{}
}

func findOpenCodeReviewToolCallByID(t *testing.T, messages []ParsedMessage, id string) ParsedToolCall {
	t.Helper()
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if call.ToolUseID == id {
				return call
			}
		}
	}
	t.Fatalf("tool call %s not found", id)
	return ParsedToolCall{}
}

func countFixtureRecords(t *testing.T, raw []byte) int {
	t.Helper()
	count := 0
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		var value map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &value))
		count++
	}
	return count
}
