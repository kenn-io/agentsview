package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	mcpserver "go.kenn.io/agentsview/internal/mcp"
	"go.kenn.io/agentsview/internal/mcpdiscovery"
	"go.kenn.io/agentsview/internal/service"
)

const lookupID = "11111111-1111-4111-8111-111111111111"

func TestLookupCodexScriptRequiresIndividualResults(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"await tools.mcp__agentsview__search_content({}); await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":"` + lookupID + `"}}`
	calls, err := extractLookupCalls(clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Missing)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "unscored", score.Verdict)
	assert.Nil(t, score.CallsToHit)
}

func TestLookupCodexTopLevelCallPairsOutputAndError(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"function_call","name":"mcp__agentsview__search_content","call_id":"top","arguments":"{}"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521600000,"completed_at_ms":1795521601000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","result":{"isError":true,"content":[{"type":"text","text":"failed"}]}}}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"top","output":"{\"isError\":true,\"content\":[{\"type\":\"text\",\"text\":\"failed\"}]}"}}`
	calls, err := extractLookupCalls(clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "mcp", calls[0].Route)
	assert.True(t, calls[0].Error)
	assert.False(t, calls[0].Missing)
	failed := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec_command","call_id":"shell","input":"sqlite3 sessions.db"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"shell","status":"failed","output":"error"}}`
	calls, err = extractLookupCalls(clientCodex, [][]byte{[]byte(failed)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Error)
}

func TestLookupRetainsTraceCallsWhenCaptureIsIncomplete(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"function_call","name":"mcp__agentsview__search_content","call_id":"top","arguments":"{}"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"top","output":"{\"isError\":true,\"content\":[{\"type\":\"text\",\"text\":\"failed\"}]}"}}`
	calls, err := readLookupTranscriptCalls(clientCodex, t.TempDir(), []byte(trace))
	require.ErrorIs(t, err, errCaptureIncomplete)
	score := scoreLookup("", nil, nil, calls, errors.New("client timed out"))
	assert.Equal(t, "failed", score.Verdict)
	assert.Equal(t, 1, score.MCP)
	assert.Equal(t, 1, score.Errors)
}

func TestLookupRetainsCallsBeforeMalformedJSONL(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{invalid`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.Error(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Missing)
}

func TestLookupTimeoutAndClientFailureFailWithIncompleteCapture(t *testing.T) {
	calls := []lookupCall{{Route: "mcp", Result: lookupID, Start: 1, End: 2}}
	for _, runErr := range []error{
		errors.Join(context.DeadlineExceeded, errCaptureIncomplete),
		errors.Join(errLookupClientFailed, errCaptureIncomplete),
	} {
		score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, runErr)
		assert.Equal(t, "failed", score.Verdict)
	}
}

func TestLookupCountsEmptyMessageAndToolCallResults(t *testing.T) {
	for _, result := range []string{`{"messages":[]}`, `{"tool_calls":null}`} {
		call := lookupCall{Result: result}
		resultFlags(&call)
		assert.True(t, call.Empty, result)
	}
}

func TestLookupScoresSearchResult(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, lookupID, "example", dbtest.WithMessageCounts(2, 1))
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: lookupID, Ordinal: 0, Role: "assistant", Content: "violet ledger"}}))
	var err error
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() {
		done <- mcpserver.ServeHTTP(ctx, mcpserver.ServeOptions{Service: service.NewDirectBackend(d, nil), DiscoveryDirectory: dir}, "127.0.0.1:0")
	}()
	t.Cleanup(func() { cancel(); <-done })
	var endpoints []mcpdiscovery.Endpoint
	require.Eventually(t, func() bool { endpoints, err = mcpdiscovery.List(dir); return err == nil && len(endpoints) == 1 }, 10*time.Second, time.Second)
	client := sdk.NewClient(&sdk.Implementation{Name: "lookup-test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: endpoints[0].URL, MaxRetries: -1}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, session.Close()) })
	response, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "search_content", Arguments: map[string]any{"pattern": "violet ledger", "include_active": true, "include_one_shot": true}})
	require.NoError(t, err)
	require.False(t, response.IsError)
	body, err := json.Marshal(response.StructuredContent)
	require.NoError(t, err)
	score := scoreLookup(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"}, []lookupCall{{Route: "mcp", Result: string(body), Start: 1, End: 2}}, nil)
	assert.Equal(t, "success", score.Verdict)
}

func TestLookupCodexParallelResultsAndWait(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":"Script running with cell ID 123"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"wait","call_id":"w","arguments":"{\"cell_id\":\"123\"}"}}
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602500,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"` + lookupID + `"}]}}}}
{"timestamp":"2026-11-24T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"w","output":"done"}}
{"timestamp":"2026-11-24T12:00:04Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602700,"completed_at_ms":1795521604000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"other"}]}}}}`
	calls, err := extractLookupCalls(clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupIncludesChildTranscript(t *testing.T) {
	root := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"no match"}]}}`
	child := `{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"` + lookupID + `"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(root), []byte(child)})
	require.NoError(t, err)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupIgnoresLaterUntimedCallForHit(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"` + lookupID + `"}]}}
{"message":{"content":[{"type":"tool_use","id":"2","name":"mcp__agentsview__get_messages","input":{}}]}}
{"message":{"content":[{"type":"tool_result","tool_use_id":"2","content":"other"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
	assert.Equal(t, 2, score.MCP)
}

func TestLookupCodexToolDiscoveryIsOneCall(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"discovery","input":"text(ALL_TOOLS.filter(x => x.name.includes(\"mcp__agentsview__search_content\")))"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"discovery","output":"tools"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"search","input":"await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602000,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","result":{"content":[{"type":"text","text":"` + lookupID + `"}]}}}}
{"timestamp":"2026-11-24T12:00:03Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"search","output":"done"}}`
	calls, err := extractLookupCalls(clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupClaudePairsResultsAndScoresFirstHit(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"empty"}]}}
{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"2","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"2","content":"` + lookupID + `"}]}}
{"timestamp":"2026-11-24T12:00:04Z","message":{"content":[{"type":"tool_use","id":"3","name":"mcp__agentsview__get_messages","input":{"session_id":"other"}}]}}
{"timestamp":"2026-11-24T12:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"3","content":"other"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupClaudeIgnoresAPIRequestBlobs(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"` + lookupID + `"}]}}
{"type":"api-request-blob","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}},{"type":"tool_result","tool_use_id":"1","content":"` + lookupID + `"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupScoringRoutesAndEvidence(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	cases := []struct {
		name, answer string
		calls        []lookupCall
		err          error
		verdict      string
		hit          int
	}{
		{"session result", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: lookupID, Start: 1, End: 2}}, nil, "success", 1},
		{"result needs no answer phrase", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: lookupID + " other", Start: 1, End: 2}}, nil, "success", 1},
		{"empty session read", "codex:" + lookupID + " violet ledger", []lookupCall{{Name: "get_messages", Route: "mcp", Input: lookupID, Result: `{"messages":[]}`, Empty: true, Start: 1, End: 2}}, nil, "unscored", 0},
		{"empty direct output", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "direct", Input: lookupID, Result: "", Start: 1, End: 2}}, nil, "unscored", 0},
		{"failed session read", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "direct", Input: lookupID, Result: "error", Error: true, Start: 1, End: 2}}, nil, "unscored", 0},
		{"missing", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Missing: true, Start: 1, End: 2}, {Route: "direct", Result: lookupID, Start: 3, End: 4}}, nil, "unscored", 0},
		{"uncited", "violet ledger", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, nil, "failed", 0},
		{"wrong answer", "codex:" + lookupID + " other", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, nil, "failed", 0},
		{"timeout", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, errors.New("timeout"), "failed", 0},
		{"incomplete capture", "codex:" + lookupID + " violet ledger", nil, errCaptureIncomplete, "unscored", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := scoreLookup(tc.answer, []lookupItem{item}, []string{"violet ledger"}, tc.calls, tc.err)
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
		assert.False(t, containsItem(text, item), text)
		score := scoreLookup(text+" violet ledger", []lookupItem{item}, []string{"violet ledger"}, []lookupCall{{Route: "mcp", Result: text + " violet ledger", Start: 1, End: 2}}, nil)
		assert.Equal(t, "failed", score.Verdict, text)
	}
	for _, text := range []string{lookupID, "codex:" + lookupID, "http://example.com/sessions/codex/" + lookupID} {
		assert.True(t, containsItem(text, item), text)
	}
	wrong := lookupCall{Route: "mcp", Result: "claude:" + lookupID + " violet ledger", Start: 1, End: 2}
	right := lookupCall{Route: "mcp", Result: "codex:" + lookupID + " violet ledger", Start: 3, End: 4}
	score := scoreLookup("codex:"+lookupID+" violet ledger", []lookupItem{item}, []string{"violet ledger"}, []lookupCall{wrong, right}, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)

}

func TestLookupClassifiesTranscriptFromGrepOutput(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"violet ledger","path":"."}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"transcripts/2026/01/01/rollout-123.jsonl:session ` + lookupID + ` violet ledger"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "direct", calls[0].Route)
	score := scoreLookup(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"}, calls, nil)
	assert.Equal(t, 1, score.Direct)
	assert.Equal(t, "success", score.Verdict)
}

func TestLookupSessionEvidenceInPlainContentBlock(t *testing.T) {
	call := lookupCall{
		Name: "search_sessions", Route: "mcp", Start: 1, End: 2,
		Result: `{"content":[{"type":"text","text":"session ` + lookupID + ` chose violet ledger"}]}`,
	}
	score := scoreLookup(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"}, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupSummaryKeepsFailuresInDenominator(t *testing.T) {
	one, two := 1, 2
	summary := summarizeLookups([]lookupAttempt{{Case: "topic", Score: lookupScore{Verdict: "success", CallsToHit: &one}}, {Case: "topic", Score: lookupScore{Verdict: "success", CallsToHit: &two}}, {Case: "topic", Score: lookupScore{Verdict: "failed"}}, {Case: "topic", Score: lookupScore{Verdict: "unscored"}}})
	require.Len(t, summary, 1)
	assert.InDelta(t, 25, summary[0].OneCallPercent, 0.01)
	assert.InDelta(t, 50, summary[0].TwoCallPercent, 0.01)
}

func TestLookupDirectSessionResult(t *testing.T) {
	call := lookupCall{Route: "direct", Result: lookupID, Start: 1, End: 2}
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
	assert.Equal(t, 1, score.Direct)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupDirectSessionInInput(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	for _, tc := range []struct{ route, input string }{
		{"direct", "cat /archive/rollout-2026-11-24T12-00-00-" + lookupID + ".jsonl"},
		{"direct", "sqlite3 sessions.db SELECT WHERE session_id='codex:" + lookupID + "'"},
		{"cli", "agentsview session show codex:" + lookupID},
	} {
		t.Run(tc.route+tc.input, func(t *testing.T) {
			call := lookupCall{Route: tc.route, Input: tc.input, Result: "recorded content", Start: 1, End: 2}
			score := scoreLookup("codex:"+lookupID+" answer", []lookupItem{item}, []string{"answer"}, []lookupCall{call}, nil)
			assert.Equal(t, "success", score.Verdict)
			require.NotNil(t, score.CallsToHit)
			assert.Equal(t, 1, *score.CallsToHit)
		})
	}
	for _, text := range []string{"rollout-2026-11-24T12-00-00-" + lookupID + ".jsonl", "rollout_" + lookupID + ".jsonl"} {
		assert.True(t, containsSessionID(text, item.SessionID), text)
	}
	assert.False(t, containsSessionID("claude:"+lookupID, item.SessionID))
}

func TestReviewDirectReadFixture(t *testing.T) {
	for _, result := range []string{
		`{"type":"session_meta","payload":{"id":"` + lookupID + `"}}` + "\n" + `{"payload":{"content":"other"}}`,
		`{"sessionId":"` + lookupID + `","message":{"content":"other"}}`,
	} {
		call := lookupCall{Route: "direct", Result: result, Start: 1, End: 2}
		score := scoreLookup("codex:"+lookupID+" answer", []lookupItem{{SessionID: "codex:" + lookupID}}, []string{"answer"}, []lookupCall{call}, nil)
		assert.Equal(t, "success", score.Verdict, result)
		require.NotNil(t, score.CallsToHit)
		assert.Equal(t, 1, *score.CallsToHit)
	}
}

func TestReviewAnswerAcrossPages(t *testing.T) {
	id := "codex:" + lookupID
	calls := []lookupCall{
		{Name: "get_messages", Route: "mcp", Input: id, Result: `{"messages":[{"content":"first page"}]}`, Start: 1, End: 2},
		{Name: "get_messages", Route: "mcp", Input: id, Result: `{"messages":[{"content":"second page"}]}`, Start: 3, End: 4},
	}
	score := scoreLookup(id+" answer", []lookupItem{{SessionID: id}}, []string{"answer"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupSavedOutputClassification(t *testing.T) {
	assert.Equal(t, "direct", routeFor("Read", `{"file_path":"/tmp/tool-results/result.txt"}`))
	assert.Equal(t, "cli", routeFor("Bash", `{"command":"agentsview session list"}`))
	assert.Equal(t, "direct", routeFor("Bash", `{"command":"sqlite3 sessions.db 'select 1'"}`))
	assert.False(t, strings.Contains(routeFor("Read", `{"file_path":"README.md"}`), "mcp"))
}
