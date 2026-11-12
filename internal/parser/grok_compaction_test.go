package parser

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// grokCompactedBase is the first updates.jsonl timestamp in the compacted
// fixture; offsets below are the seconds the fixture adds to it.
const grokCompactedBase = 1784800000

const grokCompactedDir = "%2Fworkspace%2Ffetchkit/019f6000-0000-7000-8000-000000000020"

func grokCompactedAt(offset int64) time.Time {
	return time.Unix(grokCompactedBase+offset, 0).UTC()
}

const (
	grokSummaryOne = "This session is being continued from a previous conversation that ran out of context. " +
		"The summary below covers the earlier portion of the conversation.\n\nSummary:\n" +
		"1. The user asked for a retry with backoff in the fetch helper.\n" +
		"2. Get now calls doWithRetry, which does not exist yet, so the tests fail."
	grokSummaryTwo = "This session is being continued from a previous conversation that ran out of context. " +
		"The summary below covers the earlier portion of the conversation.\n\nSummary:\n" +
		"1. The user asked for a retry with backoff in the fetch helper.\n" +
		"2. doWithRetry is missing; it belongs in retry.go."
)

func TestGrokProviderGoldenCompactedTranscript(t *testing.T) {
	result := parseGrokGolden(t, "compacted")
	msgs := result.Messages

	type row struct {
		role      RoleType
		dialogue  string
		thinking  string
		calls     []string
		resultFor string
		boundary  string
		at        int64
	}
	want := []row{
		{role: RoleUser, dialogue: "Add a retry with backoff to the fetch helper and run the tests.", at: 1},
		{
			role: RoleAssistant, dialogue: "I'll read the fetch helper first.",
			thinking: "The helper lives in fetch.go. Read it before editing.",
			calls:    []string{"call-0001-a-0"}, at: 4,
		},
		{role: RoleUser, resultFor: "call-0001-a-0", at: 7},
		{
			role: RoleAssistant, thinking: "Wrap the request in a retry loop, then run the tests.",
			calls: []string{"call-0002-b-0", "call-0002-b-1"}, at: 9,
		},
		{role: RoleUser, resultFor: "call-0002-b-0", at: 11},
		{role: RoleUser, resultFor: "call-0002-b-1", at: 14},
		{role: RoleAssistant, dialogue: "The tests fail because doWithRetry does not exist yet.", at: 16},
		{role: RoleAssistant, boundary: grokSummaryOne, at: 40},
		{
			role: RoleAssistant, thinking: "Find where doWithRetry should live.",
			calls: []string{"call-0003-c-0"}, at: 43,
		},
		{role: RoleUser, resultFor: "call-0003-c-0", at: 45},
		{role: RoleAssistant, dialogue: "There is no doWithRetry yet, so I'll add it in retry.go.", at: 46},
		{role: RoleAssistant, boundary: grokSummaryTwo, at: 50},
		{
			role: RoleAssistant, thinking: "Check the new retry helper.",
			calls: []string{"call-0004-d-0"}, at: 53,
		},
		// chat_history.jsonl result carriers carry no time; pairing drops them.
		{role: RoleUser, resultFor: "call-0004-d-0", at: -1},
		{
			role: RoleAssistant, dialogue: "Done: Get now retries three times with backoff.",
			thinking: "The helper is in place.", at: 57,
		},
	}
	require.Len(t, msgs, len(want))
	callOrdinal := map[string]int{}
	for i, w := range want {
		msg := msgs[i]
		assert.Equal(t, i, msg.Ordinal, "row %d ordinal", i)
		assert.Equal(t, w.role, msg.Role, "row %d role", i)
		if w.at >= 0 {
			assert.Equal(t, grokCompactedAt(w.at), msg.Timestamp.UTC(), "row %d timestamp", i)
		}
		if w.boundary != "" {
			assert.True(t, msg.IsCompactBoundary, "row %d boundary", i)
			assert.True(t, msg.IsSystem, "row %d system", i)
			assert.Equal(t, "compact_boundary", msg.SourceSubtype, "row %d subtype", i)
			assert.Equal(t, w.boundary, msg.Content, "row %d summary", i)
			continue
		}
		assert.False(t, msg.IsCompactBoundary, "row %d boundary", i)
		if w.resultFor != "" {
			require.Len(t, msg.ToolResults, 1, "row %d results", i)
			id := msg.ToolResults[0].ToolUseID
			assert.Equal(t, w.resultFor, id, "row %d result id", i)
			callAt, seen := callOrdinal[id]
			assert.True(t, seen && callAt < i, "result %s must follow its call", id)
			continue
		}
		if w.role == RoleUser {
			assert.Equal(t, w.dialogue, msg.Content, "row %d user text", i)
			continue
		}
		require.NotNil(t, msg.DialogueText, "row %d dialogue", i)
		assert.Equal(t, w.dialogue, *msg.DialogueText, "row %d dialogue", i)
		assert.Equal(t, w.thinking, msg.ThinkingText, "row %d thinking", i)
		var ids []string
		for _, call := range msg.ToolCalls {
			ids = append(ids, call.ToolUseID)
			callOrdinal[call.ToolUseID] = i
		}
		assert.Equal(t, w.calls, ids, "row %d calls", i)
		assert.Equal(t, len(w.calls) > 0, msg.HasToolUse, "row %d has tool use", i)
	}

	session := result.Session
	assert.Equal(t, "Add a retry with backoff to the fetch helper and run the tests.", session.FirstMessage)
	assert.Equal(t, 1, session.UserMessageCount, "the synthetic and carried prompts are not user turns")
	assert.Equal(t, TranscriptFidelityFull, session.TranscriptFidelity)
	assert.Len(t, result.UsageEvents, 2)
}

func TestGrokProviderGoldenCompactedToolCalls(t *testing.T) {
	result := parseGrokGolden(t, "compacted")
	calls := map[string]ParsedToolCall{}
	results := map[string]string{}
	for _, msg := range result.Messages {
		for _, call := range msg.ToolCalls {
			calls[call.ToolUseID] = call
		}
		for _, r := range msg.ToolResults {
			results[r.ToolUseID] = DecodeContent(r.ContentRaw)
		}
	}

	tests := []struct {
		id, name, rendering, input, result string
		statuses                           []string
	}{
		{
			id: "call-0001-a-0", name: "read_file",
			rendering: "[Read: /workspace/fetchkit/fetch.go]",
			input:     `{"target_file":"/workspace/fetchkit/fetch.go"}`,
			result:    "1|package fetchkit\n2|\n3|func (c *Client) Get(url string) (*Response, error) {",
			statuses:  []string{"started", "completed"},
		},
		{
			id: "call-0002-b-0", name: "search_replace",
			rendering: "[Edit: /workspace/fetchkit/fetch.go]",
			result:    `{"type":"EditsApplied","EditsApplied":{"path":"/workspace/fetchkit/fetch.go","count":1}}`,
			statuses:  []string{"started", "completed"},
		},
		{
			id: "call-0002-b-1", name: "run_terminal_command",
			rendering: "[Bash: Run the package tests]\n$ go test ./...",
			input:     `{"command":"go test ./...","description":"Run the package tests"}`,
			result:    "--- FAIL: TestGetRetries (0.00s)\n    fetch_test.go:18: doWithRetry undefined\nFAIL",
			statuses:  []string{"started", "errored"},
		},
		{
			id: "call-0003-c-0", name: "grep",
			rendering: "[Grep: func doWithRetry]",
			result:    "No matches found.",
			statuses:  []string{"started", "completed"},
		},
		{
			id: "call-0004-d-0", name: "read_file",
			rendering: "[Read: /workspace/fetchkit/retry.go]",
			input:     `{"target_file":"/workspace/fetchkit/retry.go","offset":1,"limit":40}`,
			result:    "1|package fetchkit\n2|\n3|func (c *Client) doWithRetry(req *Request, n int) (*Response, error) {",
			statuses:  []string{"started", "completed"},
		},
	}
	require.Len(t, calls, len(tests))
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			call, ok := calls[tt.id]
			require.True(t, ok)
			assert.Equal(t, tt.name, call.ToolName)
			assert.Equal(t, tt.rendering, call.Rendering)
			if tt.input != "" {
				assert.JSONEq(t, tt.input, call.InputJSON)
			}
			assert.Equal(t, tt.result, results[tt.id])
			var statuses []string
			for _, event := range call.ResultEvents {
				statuses = append(statuses, event.Status)
			}
			assert.Equal(t, tt.statuses, statuses)
			last := call.ResultEvents[len(call.ResultEvents)-1]
			assert.Equal(t, tt.result, last.Content)
		})
	}

	// The assistant text keeps the rendered calls after what the model said,
	// as Claude rows do, so search and exports see each call.
	row := result.Messages[3]
	assert.Equal(t,
		"[Thinking]\nWrap the request in a retry loop, then run the tests.\n[/Thinking]\n"+
			"[Edit: /workspace/fetchkit/fetch.go]\n[Bash: Run the package tests]\n$ go test ./...",
		row.Content)
}

func TestGrokCompactionWithoutCheckpointFileKeepsBoundary(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("testdata", "grok-build", "compacted"))))
	sessionDir := filepath.Join(root, filepath.FromSlash(grokCompactedDir))
	require.NoError(t, os.RemoveAll(filepath.Join(sessionDir, "compaction_checkpoints")))

	result, err := ParseGrokSummary(t.Context(), filepath.Join(sessionDir, "summary.json"), "", "m")
	require.NoError(t, err)
	var boundaries []string
	for _, msg := range result.Messages {
		if msg.IsCompactBoundary {
			boundaries = append(boundaries, msg.Content)
		}
	}
	// The first summary lived only in its checkpoint file; the boundary
	// still marks that compaction. The last comes from chat_history.jsonl.
	assert.Equal(t, []string{"", grokSummaryTwo}, boundaries)
	assert.Len(t, result.Messages, 15)
}

func TestGrokCompactionMetaRowsWithoutUpdates(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "019f6000-0000-7000-8000-000000000021")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"),
		`{"info":{"id":"019f6000-0000-7000-8000-000000000021","cwd":"/workspace/fetchkit"},"created_at":"2026-07-23T09:46:40Z"}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "chat_history.jsonl"), ""+
		`{"type":"system","content":"You are Grok."}`+"\n"+
		`{"type":"user","content":[{"type":"text","text":"<user_info>\nOS Version: linux\n</user_info>\n\n<rules>\nKeep changes small.\n</rules>"}],"synthetic_reason":"compaction_meta"}`+"\n"+
		`{"type":"user","content":[{"type":"text","text":"<user_query>\nAdd a retry.\n</user_query>"}]}`+"\n"+
		`{"type":"user","content":[{"type":"text","text":"`+grokCompactionSummaryPreamble+` Summary: retry pending."}],"synthetic_reason":"compaction_meta"}`+"\n"+
		`{"type":"user","content":[{"type":"text","text":"Attached image paths:\n- /tmp/screen.png"}],"synthetic_reason":"compaction_meta"}`+"\n"+
		`{"type":"assistant","content":"Adding it now."}`+"\n")

	result, err := ParseGrokSummary(t.Context(), filepath.Join(sessionDir, "summary.json"), "", "m")
	require.NoError(t, err)
	require.Len(t, result.Messages, 3)
	// Without updates.jsonl the carried query is the only copy, so it stays.
	assert.Equal(t, "Add a retry.", result.Messages[0].Content)
	assert.True(t, result.Messages[1].IsCompactBoundary)
	assert.Equal(t, grokCompactionSummaryPreamble+" Summary: retry pending.", result.Messages[1].Content)
	assert.Equal(t, "Adding it now.", result.Messages[2].Content)
	assert.Equal(t, 1, result.Session.UserMessageCount)
}

func TestGrokChatHistoryToolCallsRender(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "019f6000-0000-7000-8000-000000000022")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"),
		`{"info":{"id":"019f6000-0000-7000-8000-000000000022","cwd":"/workspace/fetchkit"},"created_at":"2026-07-23T09:46:40Z"}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "chat_history.jsonl"), ""+
		`{"type":"user","content":"List the package"}`+"\n"+
		`{"type":"assistant","content":"Listing it.","tool_calls":[{"id":"call-1","name":"list_dir","arguments":"{\"target_directory\":\"/workspace/fetchkit\"}"},{"id":"call-2","name":"use_tool","arguments":"{\"tool_name\":\"lint\",\"tool_input\":{}}"}]}`+"\n"+
		`{"type":"tool_result","tool_call_id":"call-1","content":"fetch.go\nretry.go"}`+"\n")

	result, err := ParseGrokSummary(t.Context(), filepath.Join(sessionDir, "summary.json"), "", "m")
	require.NoError(t, err)
	require.Len(t, result.Messages, 3)
	row := result.Messages[1]
	assert.Equal(t, "Listing it.\n[List: /workspace/fetchkit]\n[Tool: use_tool]", row.Content)
	require.NotNil(t, row.DialogueText)
	assert.Equal(t, "Listing it.", *row.DialogueText)
	require.Len(t, row.ToolCalls, 2)
	assert.Equal(t, "[List: /workspace/fetchkit]", row.ToolCalls[0].Rendering)
	assert.Equal(t, "[Tool: use_tool]", row.ToolCalls[1].Rendering)
}

// grokUpdateLine wraps a session update in the updates.jsonl envelope.
func grokUpdateLine(ts int64, update string) string {
	return `{"timestamp":` + strconv.FormatInt(ts, 10) +
		`,"method":"session/update","params":{"sessionId":"s","update":` + update + `}}` + "\n"
}

func TestGrokCompactedPrefixKeepsResultTheStreamNeverFinished(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "019f6000-0000-7000-8000-000000000024")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"),
		`{"info":{"id":"019f6000-0000-7000-8000-000000000024","cwd":"/workspace/fetchkit"},"created_at":"2026-07-23T09:46:40Z"}`)
	// The stream saw the call start, then the session compacted.
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "updates.jsonl"), ""+
		grokUpdateLine(1784800001, `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Run the tests"}}`)+
		grokUpdateLine(1784800002, `{"sessionUpdate":"tool_call","toolCallId":"call-7","title":"run_terminal_command","rawInput":{"command":"go test ./..."}}`)+
		grokUpdateLine(1784800009, `{"sessionUpdate":"compaction_checkpoint","checkpoint_file":"compaction_checkpoints/c.json"}`))
	// The compacted history carries the call's only result before the summary.
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "chat_history.jsonl"), ""+
		`{"type":"user","content":[{"type":"text","text":"<user_query>\nRun the tests\n</user_query>"}]}`+"\n"+
		`{"type":"assistant","content":"","tool_calls":[{"id":"call-7","name":"run_terminal_command","arguments":"{\"command\":\"go test ./...\"}"}]}`+"\n"+
		`{"type":"tool_result","tool_call_id":"call-7","content":"Command interrupted by the user."}`+"\n"+
		`{"type":"user","content":[{"type":"text","text":"`+grokCompactionSummaryPreamble+` Tests were interrupted."}],"synthetic_reason":"compaction_meta"}`+"\n"+
		`{"type":"assistant","content":"Rerunning them."}`+"\n")

	result, err := ParseGrokSummary(t.Context(), filepath.Join(sessionDir, "summary.json"), "", "m")
	require.NoError(t, err)
	var calls, results []string
	for _, msg := range result.Messages {
		for _, call := range msg.ToolCalls {
			calls = append(calls, call.ToolUseID)
			assert.Empty(t, call.ResultEvents, "a lone start must not replace the result")
		}
		for _, r := range msg.ToolResults {
			results = append(results, DecodeContent(r.ContentRaw))
		}
	}
	assert.Equal(t, []string{"call-7"}, calls, "the replayed call is not repeated")
	assert.Equal(t, []string{"Command interrupted by the user."}, results)
}

func TestGrokCompactedSessionWithoutChatHistoryStaysSummaryOnly(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("testdata", "grok-build", "compacted"))))
	sessionDir := filepath.Join(root, filepath.FromSlash(grokCompactedDir))
	require.NoError(t, os.Remove(filepath.Join(sessionDir, "chat_history.jsonl")))

	result, err := ParseGrokSummary(t.Context(), filepath.Join(sessionDir, "summary.json"), "", "m")
	require.NoError(t, err)
	// The turns after the last compaction live only in chat_history.jsonl,
	// so a rebuilt prefix alone would claim a full transcript it lacks.
	assert.Equal(t, TranscriptFidelitySummary, result.Session.TranscriptFidelity)
	assert.Equal(t, 16, result.Session.MessageCount)
}

func TestGrokLateToolInputRefreshesRendering(t *testing.T) {
	r := newGrokSegmentReducer(t.Context())
	for _, line := range []string{
		`{"sessionUpdate":"tool_call","toolCallId":"a","title":"read_file"}`,
		`{"sessionUpdate":"tool_call","toolCallId":"b","title":"read_file"}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"a","status":"completed","content":[{"type":"content","content":{"type":"text","text":"one"}}]}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"b","rawInput":{"target_file":"/workspace/fetchkit/b.go"}}`,
	} {
		r.process(gjson.Parse(line), time.Unix(grokCompactedBase, 0))
	}
	msgs := r.finish()
	require.NotEmpty(t, msgs)
	require.Len(t, msgs[0].ToolCalls, 2)
	assert.Equal(t, "[Read: /workspace/fetchkit/b.go]", msgs[0].ToolCalls[1].Rendering)
	assert.Equal(t, "[Read]\n[Read: /workspace/fetchkit/b.go]", msgs[0].Content)
}

func TestGrokNativeToolRenderingsAreRedactionCandidates(t *testing.T) {
	tests := []struct{ name, input string }{
		{"read_file", `{"target_file":"/workspace/fetchkit/fetch.go"}`},
		{"run_terminal_command", `{"command":"echo \"for now\"","description":"Print a note"}`},
		{"search_replace", `{"file_path":"/workspace/fetchkit/fetch.go","old_string":"a","new_string":"b"}`},
		{"list_dir", `{"target_directory":"/workspace/fetchkit"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendering := grokToolRendering(tt.name, tt.input)
			var fulls []string
			for _, pair := range ToolUseRenderingCandidates(AgentGrok, NormalizeToolCategory(tt.name), tt.name, tt.input) {
				fulls = append(fulls, pair.Full)
			}
			assert.Contains(t, fulls, rendering)
		})
	}
}

func TestGrokEarlierCheckpointKeepsResultLaterCompactionDropped(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(root, "cwd-key", "019f6000-0000-7000-8000-000000000025")
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "summary.json"),
		`{"info":{"id":"019f6000-0000-7000-8000-000000000025","cwd":"/workspace/fetchkit"},"created_at":"2026-07-23T09:46:40Z"}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "updates.jsonl"), ""+
		grokUpdateLine(1784800001, `{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Run the tests"}}`)+
		grokUpdateLine(1784800002, `{"sessionUpdate":"tool_call","toolCallId":"call-7","title":"run_terminal_command","rawInput":{"command":"go test ./..."}}`)+
		grokUpdateLine(1784800009, `{"sessionUpdate":"compaction_checkpoint","checkpoint_file":"compaction_checkpoints/c1.json"}`)+
		grokUpdateLine(1784800010, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Rerunning them."}}`)+
		grokUpdateLine(1784800019, `{"sessionUpdate":"compaction_checkpoint","checkpoint_file":"compaction_checkpoints/c2.json"}`))
	summary := func(text string) string {
		return `{"type":"user","content":[{"type":"text","text":"` + grokCompactionSummaryPreamble + ` ` + text + `"}],"synthetic_reason":"compaction_meta"}`
	}
	// Only the first compacted context still carries the interrupted result.
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "compaction_checkpoints", "c1.json"),
		`{"compacted_history":[`+
			`{"type":"assistant","content":"","tool_calls":[{"id":"call-7","name":"run_terminal_command","arguments":"{}"}]},`+
			`{"type":"tool_result","tool_call_id":"call-7","content":"Command interrupted by the user."},`+
			summary("First.")+`]}`)
	writeGrokFixtureFile(t, filepath.Join(sessionDir, "chat_history.jsonl"), ""+
		summary("Second.")+"\n"+
		`{"type":"assistant","content":"Done."}`+"\n")

	result, err := ParseGrokSummary(t.Context(), filepath.Join(sessionDir, "summary.json"), "", "m")
	require.NoError(t, err)
	var results, boundaries []string
	for _, msg := range result.Messages {
		for _, call := range msg.ToolCalls {
			assert.Empty(t, call.ResultEvents, "a lone start must not replace the result")
		}
		for _, r := range msg.ToolResults {
			results = append(results, DecodeContent(r.ContentRaw))
		}
		if msg.IsCompactBoundary {
			boundaries = append(boundaries, strings.TrimPrefix(msg.Content, grokCompactionSummaryPreamble+" "))
		}
	}
	assert.Equal(t, []string{"Command interrupted by the user."}, results)
	assert.Equal(t, []string{"First.", "Second."}, boundaries)
}

func TestGrokRebuiltRowsCarryTheirTurnModel(t *testing.T) {
	result := parseGrokGolden(t, "compacted")
	var models []string
	for _, msg := range result.Messages {
		if msg.Role == RoleAssistant && !msg.IsCompactBoundary {
			models = append(models, msg.Model)
		}
	}
	// The second turn runs across the first checkpoint and completes after
	// the second, so its turn_completed record names the model of rows in
	// both rebuilt segments. Rebuilt rows use the same name as the rows
	// read from chat_history.jsonl.
	require.NotEmpty(t, models)
	for i, model := range models {
		assert.Equal(t, "grok-4.7-build", model, "assistant row %d", i)
	}
}

func TestGrokRebuiltTurnModel(t *testing.T) {
	tests := []struct {
		name       string
		modelUsage string
		want       string
	}{
		{"one model", `{"grok-4.6-build":{"modelCalls":1}}`, "grok-4.6-build"},
		// The record does not say which row each model wrote.
		{"several models", `{"grok-4.6-build":{"modelCalls":1},"grok-4.7-build":{"modelCalls":1}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newGrokSegmentReducer(t.Context())
			at := time.Unix(grokCompactedBase, 0)
			for _, line := range []string{
				`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Read it"}}`,
				`{"sessionUpdate":"tool_call","toolCallId":"a","title":"read_file","rawInput":{"target_file":"/w/a.go"}}`,
				`{"sessionUpdate":"tool_call_update","toolCallId":"a","status":"completed","content":[{"type":"content","content":{"type":"text","text":"one"}}]}`,
				`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done."}}`,
				`{"sessionUpdate":"turn_completed","usage":{"modelUsage":` + tt.modelUsage + `}}`,
				`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Next"}}`,
				`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Still running."}}`,
			} {
				r.process(gjson.Parse(line), at)
			}
			var models []string
			for _, msg := range r.finish() {
				if msg.Role == RoleAssistant {
					models = append(models, msg.Model)
				}
			}
			// The open turn has no turn_completed record yet, so its row
			// stays without a model rather than borrowing the last one.
			assert.Equal(t, []string{tt.want, tt.want, ""}, models)
		})
	}
}

func TestGrokRebuiltWebSearchKeepsItsQuery(t *testing.T) {
	r := newGrokSegmentReducer(t.Context())
	at := time.Unix(grokCompactedBase, 0)
	// Grok Build records a backend web search this way: a "Web search:"
	// title and no query until the finished update's raw output.
	for _, line := range []string{
		`{"sessionUpdate":"tool_call","toolCallId":"ws-1","title":"Web search:","kind":"search","status":"in_progress","rawInput":{"variant":"WebSearch","backend":true},"_meta":{"backend":true}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"ws-1","title":"Web search:","status":"completed","rawOutput":{"action":{"type":"search","query":"go retry backoff"},"status":"completed"}}`,
	} {
		r.process(gjson.Parse(line), at)
	}
	msgs := r.finish()
	require.NotEmpty(t, msgs)
	require.Len(t, msgs[0].ToolCalls, 1)
	call := msgs[0].ToolCalls[0]
	assert.Equal(t, "web_search", call.ToolName)
	assert.JSONEq(t, `{"type":"search","query":"go retry backoff"}`, call.InputJSON)
	assert.Equal(t, "[backend web_search] search: go retry backoff", call.Rendering)
	assert.Equal(t, "[backend web_search] search: go retry backoff", msgs[0].Content)
}

func TestGrokParseReplacesStoredRowsOnlyWhenComplete(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		want   bool
	}{
		{"every source present", "", true},
		{"checkpoint file missing", "compaction_checkpoints/0a7c0000-0000-4000-8000-0000000000c1.json", false},
		{"chat history missing", "chat_history.jsonl", false},
		{"updates missing after a compaction", "updates.jsonl", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("testdata", "grok-build", "compacted"))))
			if tt.remove != "" {
				require.NoError(t, os.Remove(filepath.Join(root, filepath.FromSlash(grokCompactedDir), filepath.FromSlash(tt.remove))))
			}
			provider := newGrokTestProvider(t, root)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.Len(t, sources, 1)
			outcome, err := provider.Parse(t.Context(), ParseRequest{Source: sources[0]})
			require.NoError(t, err)
			// A parse missing a source file must not replace what an earlier
			// sync stored from it.
			assert.Equal(t, tt.want, outcome.ForceReplace)
		})
	}
}
