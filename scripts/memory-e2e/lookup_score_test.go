package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const lookupID = "11111111-1111-4111-8111-111111111111"

func TestLookupCodexScriptRequiresIndividualResults(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"await tools.mcp__agentsview__search_content({}); await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":"` + lookupID + `"}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Missing)
}
func TestLookupCodexTopLevelCallPairsOutputAndError(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"function_call","name":"mcp__agentsview__search_content","call_id":"top","arguments":"{}"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"top","output":"{\"isError\":true,\"content\":[{\"type\":\"text\",\"text\":\"failed\"}]}"}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "mcp", calls[0].Route)
	assert.True(t, calls[0].Error)
	assert.False(t, calls[0].Missing)
	failed := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec_command","call_id":"shell","input":"sqlite3 sessions.db"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"shell","status":"failed","output":"error"}}`
	calls, err = extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(failed)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Error)
}

func TestLookupRetainsTraceCallsWhenCaptureIsIncomplete(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"assistant","message":{"content":[{"type":"tool_use","id":"top","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"top","content":"failed","is_error":true}]}}`
	calls, err := readLookupTranscriptCalls(t.Context(), clientClaude, t.TempDir(), []byte(trace))
	require.ErrorIs(t, err, errCaptureIncomplete)
	require.Len(t, calls, 1)
	assert.Equal(t, "mcp", calls[0].Route)
}

func TestLookupRetainsCallsBeforeMalformedJSONL(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{invalid`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.Error(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Missing)
}

func TestLookupTraceSetupError(t *testing.T) {
	assert.True(t, lookupTraceSetupError([]byte(`{"type":"result","is_error":true,"result":"invalid model"}`)))
	assert.True(t, lookupTraceSetupError([]byte(`{"type":"result","is_error":true,"result":"authentication required"}`)))
	assert.False(t, lookupTraceSetupError([]byte(`{"type":"result","is_error":true,"result":"maximum turns reached"}`)))
}

func TestLookupCodexParallelResults(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":"Script running with cell ID 123"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"wait","call_id":"w","arguments":"{\"cell_id\":\"123\"}"}}
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602500,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}}}
{"timestamp":"2026-11-24T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"w","output":"done"}}
{"timestamp":"2026-11-24T12:00:04Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602700,"completed_at_ms":1795521604000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.LessOrEqual(t, calls[1].Start, calls[0].End)
	assert.LessOrEqual(t, calls[0].Start, calls[1].End)
}
func TestLookupExtractsOtherToolResult(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"read","name":"Bash","input":{"command":"for file in $(find transcripts); do read_session \"$file\"; done"}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"read","content":"session_id: ` + lookupID + `\nviolet ledger"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "other", calls[0].Route)
}
func TestLookupClaudeTranscriptCalls(t *testing.T) {
	cases := []struct {
		name            string
		transcripts     [][]byte
		results, routes []string
	}{
		{"child transcripts", func() [][]byte {
			root := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"no match"}]}}`
			child := `{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}`
			return [][]byte{[]byte(root), []byte(child)}
		}(), []string{"no match", `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`}, []string{"mcp", "mcp"}},
		{"tool discovery", func() [][]byte {
			trace := `{"timestamp":"2026-11-24T11:59:58Z","message":{"content":[{"type":"tool_use","id":"other","name":"ToolSearch","input":{"query":"select:browser__search"}}]}}
{"timestamp":"2026-11-24T11:59:59Z","message":{"content":[{"type":"tool_result","tool_use_id":"other","content":"Found a browser search tool."}]}}
{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"discover","name":"ToolSearch","input":{"query":"select:mcp__agentsview__search_content"}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"discover","content":"Found the AgentsView search tool."}]}}
{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"search","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"search","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}`
			return [][]byte{[]byte(trace)}
		}(), []string{"Found the AgentsView search tool.", `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`}, []string{"other", "mcp"}},
		{"pairing", func() [][]byte {
			trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"empty"}]}}
{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"2","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"2","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}
{"timestamp":"2026-11-24T12:00:04Z","message":{"content":[{"type":"tool_use","id":"3","name":"mcp__agentsview__get_messages","input":{"session_id":"other"}}]}}
{"timestamp":"2026-11-24T12:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"3","content":"other"}]}}`
			return [][]byte{[]byte(trace)}
		}(), []string{"empty", `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, "other"}, []string{"mcp", "mcp", "mcp"}},
		{"request blob", func() [][]byte {
			trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}
{"type":"api-request-blob","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}},{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}`
			return [][]byte{[]byte(trace)}
		}(), []string{`{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`}, []string{"mcp"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := extractLookupCalls(t.Context(), clientClaude, tc.transcripts)
			require.NoError(t, err)
			require.Len(t, calls, len(tc.results))
			for i := range calls {
				assert.Equal(t, tc.results[i], calls[i].Result)
				assert.Equal(t, tc.routes[i], calls[i].Route)
			}
		})
	}
}

func TestLookupExtractsLaterUntimedCallOrder(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"function_call","name":"exec","call_id":"parent","arguments":"await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521601000,"completed_at_ms":1795521602000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec","call_id":"unfinished","arguments":"tools.mcp__agentsview__get_messages({})"}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.Equal(t, "mcp", calls[0].Route)
	assert.Equal(t, "other", calls[1].Route)
	assert.Greater(t, calls[1].StartOrder, calls[0].EndOrder)
}
func TestLookupCodexToolDiscoveryIsOneCall(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"discovery","input":"text(ALL_TOOLS.filter(x => x.name.includes(\"mcp__agentsview__search_content\")))"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"discovery","output":"tools"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"search","input":"await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602000,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}}}
{"timestamp":"2026-11-24T12:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"search","output":"done"}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.Equal(t, "tool discovery", calls[0].Name)
	assert.Equal(t, "other", calls[0].Route)
	assert.Equal(t, "mcp", calls[1].Route)
}

func TestLookupScoringRoutesAndEvidence(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	cases := []struct {
		name    string
		calls   []lookupCall
		err     error
		verdict string
		hit     int
	}{
		{"no hit and missing call", []lookupCall{{Route: "other", Name: "exec", Missing: true, Start: 1, End: 2}}, nil, "unscored", 0},
		{"overlapping call", []lookupCall{{Route: "mcp", Result: lookupID, Start: 1, End: 3}, {Route: "mcp", Result: lookupID, Start: 2, End: 4}}, nil, "success", 2},
		{"other route hit", []lookupCall{{Route: "other", Name: "Bash", Result: lookupID, Start: 1, End: 2}}, nil, "success", 1},
		{"later untimed call", []lookupCall{{Route: "mcp", Result: lookupID, Start: 1, End: 2, Source: 1, EndOrder: 2}, {Route: "other", Name: "exec", Missing: true, Start: 2, Source: 1, StartOrder: 3}}, nil, "success", 1},
		{"result lacks answer phrase", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"other"}`, Start: 1, End: 2}}, nil, "success", 1},
		{"failed session read", []lookupCall{{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + lookupID + `"}`, Result: `{"isError":true,"results":[{"session_id":"codex:` + lookupID + `","content":"violet ledger"}]}`, Error: true, Start: 1, End: 2}}, nil, "failed", 0},
		{"missing", []lookupCall{{Route: "mcp", Missing: true, Start: 1, End: 2}, {Route: "direct", Name: "Read", Input: `{"file_path":"/archive/rollout-` + lookupID + `.jsonl"}`, Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 3, End: 4}}, nil, "unscored", 0},
		{"generic run error", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, errors.New("timeout"), "failed", 0},
		{"timeout with incomplete capture", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}}, errors.Join(context.DeadlineExceeded, errCaptureIncomplete), "failed", 0},
		{"client failure with incomplete capture", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}}, errors.Join(errLookupClientFailed, errCaptureIncomplete), "failed", 0},
		{"unsupported lookup capture format", nil, errLookupCaptureUnsupported, "unscored", 0},
		{"incomplete capture", nil, errCaptureIncomplete, "unscored", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.calls {
				upgradeLookupRoute(&tc.calls[i])
			}
			question, answer := lookupQuestion{Expected: []lookupItem{item}, ExpectedAnswer: map[string]string{"value": "violet ledger"}}, `{"value":"violet ledger","source_session_id":"codex:`+lookupID+`"}`
			score := scoreLookupQuestion(question, answer, tc.calls, tc.err)
			assert.Equal(t, tc.verdict, score.Verdict)
			if tc.hit == 0 {
				assert.Nil(t, score.CallsToHit)
			} else {
				require.NotNil(t, score.CallsToHit)
				assert.Equal(t, tc.hit, *score.CallsToHit)
			}
		})
	}
}

func TestLookupRejectsDifferentProviderWithSameID(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	for _, text := range []string{"claude:" + lookupID, "http://example.com/sessions/claude/" + lookupID} {
		assert.False(t, containsSessionID(text, item.SessionID), text)
	}
	for _, text := range []string{lookupID, "codex:" + lookupID, "http://example.com/sessions/codex/" + lookupID} {
		assert.True(t, containsSessionID(text, item.SessionID), text)
	}

}

func TestLookupClassifiesTranscriptFromGrepOutput(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"violet ledger","path":"."}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"transcripts/2026/01/01/rollout-2026-11-24T12-00-00-` + lookupID + `.jsonl:session ` + lookupID + ` violet ledger"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "direct", calls[0].Route)
}
func TestLookupSummaryKeepsFailuresInDenominator(t *testing.T) {
	one, two := 1, 2
	summary := summarizeLookups([]lookupAttempt{{Case: "topic", Score: lookupScore{Verdict: "success", CallsToHit: &one}}, {Case: "topic", Score: lookupScore{Verdict: "success", CallsToHit: &two}}, {Case: "topic", Score: lookupScore{Verdict: "failed"}}, {Case: "topic", Score: lookupScore{Verdict: "unscored"}}})
	require.Len(t, summary, 1)
	assert.InDelta(t, 25, summary[0].OneCallPercent, 0.01)
	assert.InDelta(t, 50, summary[0].TwoCallPercent, 0.01)
}

func TestLookupSessionInInput(t *testing.T) {
	id := "codex:" + lookupID
	question, answer := lookupQuestion{Expected: []lookupItem{{SessionID: id}}, ExpectedAnswer: map[string]string{"value": "violet ledger"}}, `{"value":"violet ledger","source_session_id":"`+id+`"}`
	for _, tc := range []struct{ route, name, input string }{
		{"cli", "", "agentsview session messages " + id},
		{"direct", "", `{"command":"sqlite3 sessions.db SELECT WHERE session_id='` + id + `'"}`},
		{"mcp", "get_messages", `{"session_id":"` + id + `"}`},
	} {
		t.Run(tc.route, func(t *testing.T) {
			call := lookupCall{Route: tc.route, Name: tc.name, Input: tc.input, Result: "violet ledger", Start: 1, End: 2}
			score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
			assert.Equal(t, "success", score.Verdict)
			require.NotNil(t, score.CallsToHit)
			assert.Equal(t, 1, *score.CallsToHit)
		})
	}
}

func TestLookupCountsOnlyLookupCalls(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	calls := []lookupCall{
		{Name: "Bash", Route: "other", Input: "pwd", Start: 1, End: 2},
		{Name: "tool discovery", Route: "other", Start: 3, End: 4},
		{Route: "mcp", Result: `{"session_id":"` + item.SessionID + `","content":"answer"}`, Start: 5, End: 6},
	}
	question, answer := lookupQuestion{Expected: []lookupItem{item}, ExpectedAnswer: map[string]string{"value": "answer"}}, `{"value":"answer","source_session_id":"`+item.SessionID+`"}`
	score := scoreLookupQuestion(question, answer, calls, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
	assert.Equal(t, 2, score.Other)
}

func TestLookupStructuredAnswers(t *testing.T) {
	calls := []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger lock contention maple team now owns"}`, Start: 1, End: 2}}
	cases := []struct {
		name, answer string
		question     lookupQuestion
		runErr       error
		verdict      string
	}{
		{
			name:     "correct owner field",
			answer:   `{"team":"maple team","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "success",
		},
		{
			name:     "wrong namespace with expected reason",
			answer:   `{"namespace":"copper ledger, not violet ledger","reason":"lock contention","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"namespace": "violet ledger", "reason": "lock contention"}},
			verdict:  "failed",
		},
		{
			name:     "wrong source session",
			answer:   `{"team":"maple team","source_session_id":"codex:22222222-2222-4222-8222-222222222222"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "failed",
		},
		{
			name:     "extra field",
			answer:   `{"team":"maple team","source_session_id":"codex:` + lookupID + `","comment":"maple owns it"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "failed",
		},
		{
			name:     "bare source ID",
			answer:   `{"team":"maple team","source_session_id":"` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "success",
		},
		{
			name:     "wrong provider source ID",
			answer:   `{"team":"maple team","source_session_id":"claude:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "failed",
		},
		{
			name:     "wrong answer with incomplete capture",
			answer:   `{"team":"cedar team","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			runErr:   errCaptureIncomplete,
			verdict:  "failed",
		},
		{
			name:     "markdown fence",
			answer:   "```json\n{\"team\":\"maple team\",\"source_session_id\":\"codex:" + lookupID + "\"}\n```",
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "success",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := scoreLookupQuestion(tc.question, tc.answer, calls, tc.runErr)
			assert.Equal(t, tc.verdict, score.Verdict)
		})
	}
}

func TestLookupExpectedAnswersAppearInFixtures(t *testing.T) {
	var fixtures []lookupFixture
	require.NoError(t, readLookupJSON("testdata/lookups/corpus.json", &fixtures))
	sources := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		sources["codex:"+fixture.ID] = fixture.Content
	}
	var questions []lookupQuestion
	require.NoError(t, readLookupJSON("testdata/lookups/questions.json", &questions))
	for _, q := range questions {
		t.Run(q.Name, func(t *testing.T) {
			require.NotEmpty(t, q.Expected)
			sourceID := q.Expected[0].SessionID
			source, ok := sources[sourceID]
			require.True(t, ok, "fixture source for %s", sourceID)
			for _, answer := range q.ExpectedAnswer {
				assert.Contains(t, source, answer)
			}

		})
	}
}

func TestLookupMixedResultCountsForEachSession(t *testing.T) {
	ids := []string{"codex:" + lookupID, "codex:22222222-2222-4222-8222-222222222222"}
	call := lookupCall{Route: "mcp", Result: `{"results":[{"session_id":"` + ids[0] + `"},{"session_id":"` + ids[1] + `"}]}`, Start: 1, End: 2}
	for _, tc := range []struct{ id, verdict string }{
		{ids[0], "success"}, {ids[1], "success"}, {"codex:33333333-3333-4333-8333-333333333333", "failed"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			question := lookupQuestion{Expected: []lookupItem{{SessionID: tc.id}}, ExpectedAnswer: map[string]string{"team": "maple team"}}
			answer := `{"team":"maple team","source_session_id":"` + tc.id + `"}`
			score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
			assert.Equal(t, tc.verdict, score.Verdict)
			if tc.verdict == "failed" {
				assert.Nil(t, score.CallsToHit)
			} else {
				require.NotNil(t, score.CallsToHit)
				assert.Equal(t, 1, *score.CallsToHit)
			}
		})
	}
}

func TestLookupSavedOutputClassification(t *testing.T) {
	assert.Equal(t, "direct", routeFor("Read", `{"file_path":"/tmp/tool-results/result.txt"}`))
	assert.Equal(t, "direct", routeFor("Bash", "cat ~/.codex/sessions/**/*.jsonl"))
	assert.Equal(t, "direct", routeFor("Bash", `grep -r phrase "$HOME/.codex"`))
	assert.Equal(t, "cli", routeFor("Bash", `{"command":"agentsview session list"}`))
	assert.Equal(t, "direct", routeFor("Bash", `{"command":"sqlite3 sessions.db 'select 1'"}`))
	assert.Equal(t, "other", routeFor("Read", `{"file_path":"README.md"}`))
}

func TestLookupPromptHidesExpectedAnswer(t *testing.T) {
	question := lookupQuestion{
		Name:           "topic",
		Prompt:         "Which namespace did the review choose, and why?",
		Expected:       []lookupItem{{SessionID: "codex:" + lookupID}},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger", "reason": "lock contention"},
	}
	prompt := lookupPrompt(question)
	assert.Contains(t, prompt, `["namespace","reason","source_session_id"]`)
	assert.NotContains(t, prompt, "violet ledger")
	assert.NotContains(t, prompt, "codex:")
}
