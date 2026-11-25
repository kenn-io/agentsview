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

func TestLookupTraceSetupError(t *testing.T) {
	assert.True(t, lookupTraceSetupError([]byte(`{"type":"result","is_error":true,"result":"invalid model"}`)))
	assert.True(t, lookupTraceSetupError([]byte(`{"type":"result","is_error":true,"result":"authentication required"}`)))
	assert.False(t, lookupTraceSetupError([]byte(`{"type":"result","is_error":true,"result":"maximum turns reached"}`)))
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
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602500,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}}}
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
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(root), []byte(child)})
	require.NoError(t, err)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupIgnoresLaterUntimedCallForHit(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}
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
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602000,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}}}
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
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"2","content":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}
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
{"timestamp":"2026-11-24T12:00:01Z","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}
{"type":"api-request-blob","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}},{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\"}"}]}}`
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
		{"session result", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}}, nil, "success", 1},
		{"result lacks answer phrase", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"other"}`, Start: 1, End: 2}}, nil, "unscored", 0},
		{"empty session read", "codex:" + lookupID + " violet ledger", []lookupCall{{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + lookupID + `"}`, Result: `{"messages":[]}`, Empty: true, Start: 1, End: 2}}, nil, "unscored", 0},
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
	wrong := lookupCall{Route: "mcp", Result: `{"session_id":"claude:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}
	right := lookupCall{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 3, End: 4}
	score := scoreLookup("codex:"+lookupID+" violet ledger", []lookupItem{item}, []string{"violet ledger"}, []lookupCall{wrong, right}, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupClassifiesTranscriptFromGrepOutput(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"violet ledger","path":"."}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"transcripts/2026/01/01/rollout-2026-11-24T12-00-00-` + lookupID + `.jsonl:session ` + lookupID + ` violet ledger"}]}}`
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
		Result: `{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"chose violet ledger\"}"}]}`,
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
	call := lookupCall{
		Name: "Read", Route: "direct",
		Input:  `{"file_path":"/archive/rollout-2026-11-24T12-00-00-` + lookupID + `.jsonl"}`,
		Result: lookupID, Start: 1, End: 2,
	}
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
	assert.Equal(t, 1, score.Direct)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupCLIMessageReadAttribution(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	for _, tc := range []struct {
		name, input string
		verdict     string
	}{
		{"command", "agentsview session messages " + item.SessionID, "success"},
		{"shell input", `{"command":"agentsview session messages ` + item.SessionID + `"}`, "success"},
		{"documented flags", "agentsview session messages " + item.SessionID + " --from 0 --limit 20 --json", "success"},
		{"command input with flags", `{"command":"agentsview session messages ` + item.SessionID + ` --from 0 --limit 20"}`, "success"},
		{"equals flags", "agentsview session messages " + item.SessionID + " --direction=desc --limit=20 --json", "success"},
		{"cmd input", `{"cmd":"agentsview session messages ` + item.SessionID + ` --from 0 --limit 20"}`, "success"},
		{"combined commands", "agentsview session messages " + item.SessionID + " && echo unrelated", "unscored"},
		{"combined after flags", "agentsview session messages " + item.SessionID + " --limit 20; echo unrelated", "unscored"},
		{"pipe after flags", "agentsview session messages " + item.SessionID + " --json | cat", "unscored"},
		{"substitution in flag", "agentsview session messages " + item.SessionID + " --limit $(echo 20)", "unscored"},
		{"newline command", "agentsview session messages " + item.SessionID + " --limit 20\necho unrelated", "unscored"},
		{"ambiguous input", `{"command":"agentsview session messages ` + item.SessionID + `","cmd":"echo unrelated"}`, "unscored"},
		{"different command", "agentsview session list " + item.SessionID, "unscored"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := lookupCall{Route: "cli", Input: tc.input, Result: "violet ledger", Start: 1, End: 2}
			score := scoreLookup(item.SessionID+" violet ledger", []lookupItem{item}, []string{"violet ledger"}, []lookupCall{call}, nil)
			assert.Equal(t, tc.verdict, score.Verdict)
			if tc.verdict == "success" {
				require.NotNil(t, score.CallsToHit)
				assert.Equal(t, 1, *score.CallsToHit)
			}
		})
	}
}

func TestLookupDirectSessionInInput(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	for _, tc := range []struct {
		name, route, tool, input, result string
	}{
		{"single transcript file", "direct", "Read", `{"file_path":"/archive/rollout-2026-11-24T12-00-00-` + lookupID + `.jsonl"}`, "recorded answer"},
		{"CLI row with explicit source", "cli", "Bash", `{"command":"agentsview session show codex:` + lookupID + `"}`, `{"session_id":"codex:` + lookupID + `","content":"recorded answer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := lookupCall{Name: tc.tool, Route: tc.route, Input: tc.input, Result: tc.result, Start: 1, End: 2}
			score := scoreLookup("codex:"+lookupID+" answer", []lookupItem{item}, []string{"answer"}, []lookupCall{call}, nil)
			assert.Equal(t, "success", score.Verdict)
			require.NotNil(t, score.CallsToHit)
			assert.Equal(t, 1, *score.CallsToHit)
		})
	}
	call := lookupCall{Route: "direct", Input: `{"command":"sqlite3 sessions.db SELECT WHERE session_id='codex:` + lookupID + `'"}`, Result: "recorded answer", Start: 1, End: 2}
	score := scoreLookup("codex:"+lookupID+" answer", []lookupItem{item}, []string{"answer"}, []lookupCall{call}, nil)
	assert.Equal(t, "unscored", score.Verdict)
}

func TestReviewDirectReadFixture(t *testing.T) {
	for _, result := range []string{
		`{"type":"session_meta","payload":{"id":"` + lookupID + `"}}` + "\n" + `{"payload":{"content":"answer"}}`,
		`{"sessionId":"` + lookupID + `","message":{"content":"answer"}}`,
	} {
		call := lookupCall{Route: "direct", Result: result, Start: 1, End: 2}
		score := scoreLookup("codex:"+lookupID+" answer", []lookupItem{{SessionID: "codex:" + lookupID}}, []string{"answer"}, []lookupCall{call}, nil)
		assert.Equal(t, "success", score.Verdict, result)
		require.NotNil(t, score.CallsToHit)
		assert.Equal(t, 1, *score.CallsToHit)
	}
}

func TestLookupSeparatesEvidenceBetweenTranscriptHeaders(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	otherID := "22222222-2222-4222-8222-222222222222"
	transcript := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"` + otherID + `"}}`,
		`{"payload":{"content":"violet ledger lock contention"}}`,
		`{"type":"session_meta","payload":{"id":"` + lookupID + `"}}`,
		`{"payload":{"content":"unrelated note"}}`,
	}, "\n")
	calls := []lookupCall{
		{Route: "direct", Result: transcript, Start: 1, End: 2},
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + item.SessionID + `"}`, Result: "violet ledger lock contention", Start: 3, End: 4},
	}
	score := scoreLookup(item.SessionID+" violet ledger lock contention", []lookupItem{item}, []string{"violet ledger", "lock contention"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupWildcardTranscriptRead(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	transcript := `{"type":"session_meta","payload":{"id":"22222222-2222-4222-8222-222222222222"}}` + "\n" + `{"payload":{"content":"unrelated"}}` + "\n" + `{"type":"session_meta","payload":{"id":"` + lookupID + `"}}` + "\n" + `{"payload":{"content":"violet ledger"}}`
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"cat transcripts/**/*.jsonl"}}]}}` + "\n" + `{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":` + stringMustJSON(t, transcript) + `}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	score := scoreLookup(item.SessionID+" violet ledger", []lookupItem{item}, []string{"violet ledger"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	assert.Equal(t, 1, score.Direct)
}

func stringMustJSON(t *testing.T, value string) string {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	return string(b)
}

func TestLookupCountsOnlyLookupCalls(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	calls := []lookupCall{
		{Name: "Bash", Route: "other", Input: "pwd", Start: 1, End: 2},
		{Name: "tool discovery", Route: "other", Start: 3, End: 4},
		{Route: "mcp", Result: `{"session_id":"` + item.SessionID + `","content":"answer"}`, Start: 5, End: 6},
	}
	score := scoreLookup(item.SessionID+" answer", []lookupItem{item}, []string{"answer"}, calls, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
	assert.Equal(t, 2, score.Other)
}

func TestLookupBareSourceID(t *testing.T) {
	q := lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}}
	calls := []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","content":"maple team now owns"}`, Start: 1, End: 2}}
	answer := `{"team":"maple team","source_session_id":"` + lookupID + `"}`
	assert.Equal(t, "success", scoreLookupQuestion(q, answer, calls, nil).Verdict)
	assert.Equal(t, "failed", scoreLookupQuestion(q, strings.Replace(answer, lookupID, "claude:"+lookupID, 1), calls, nil).Verdict)
	q.Expected = append(q.Expected, lookupItem{SessionID: "claude:" + lookupID})
	assert.Equal(t, "failed", scoreLookupQuestion(q, answer, calls, nil).Verdict)
}

func TestReviewAnswerAcrossPages(t *testing.T) {
	id := "codex:" + lookupID
	calls := []lookupCall{
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + id + `"}`, Result: `{"messages":[{"content":"first page"}]}`, Start: 1, End: 2},
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + id + `"}`, Result: `{"messages":[{"content":"second page answer"}]}`, Start: 3, End: 4},
	}
	score := scoreLookup(id+" answer", []lookupItem{{SessionID: id}}, []string{"answer"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupAccumulatesFactsForCitedSession(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	calls := []lookupCall{
		{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2},
		{Route: "mcp", Result: `{"results":[{"session_id":"` + lookupID + `","content":"other"},{"session_id":"22222222-2222-4222-8222-222222222222","content":"lock contention"}]}`, Start: 3, End: 4},
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + item.SessionID + `"}`, Result: "lock contention", Start: 5, End: 6},
	}
	score := scoreLookup(item.SessionID+" violet ledger lock contention", []lookupItem{item}, []string{"violet ledger", "lock contention"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 3, *score.CallsToHit)
}

func TestLookupStructuredAnswers(t *testing.T) {
	calls := []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger lock contention maple team now owns"}`, Start: 1, End: 2}}
	cases := []struct {
		name, answer string
		question     lookupQuestion
		verdict      string
	}{
		{
			name:     "correct owner field",
			answer:   `{"team":"maple team","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}},
			verdict:  "success",
		},
		{
			name:     "wrong owner with expected word",
			answer:   `{"team":"cedar; maple is not the owner","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}},
			verdict:  "failed",
		},
		{
			name:     "wrong namespace with expected reason",
			answer:   `{"namespace":"copper ledger, not violet ledger","reason":"lock contention","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"namespace": "violet ledger", "reason": "lock contention"}, Phrases: []string{"violet ledger", "lock contention"}},
			verdict:  "failed",
		},
		{
			name:     "wrong source session",
			answer:   `{"team":"maple team","source_session_id":"codex:22222222-2222-4222-8222-222222222222"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}},
			verdict:  "failed",
		},
		{
			name:     "extra field",
			answer:   `{"team":"maple team","source_session_id":"codex:` + lookupID + `","comment":"maple owns it"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}},
			verdict:  "failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := scoreLookupQuestion(tc.question, tc.answer, calls, nil)
			assert.Equal(t, tc.verdict, score.Verdict)
		})
	}
}

func TestLookupAcceptsSourceExcerpts(t *testing.T) {
	var questions []lookupQuestion
	require.NoError(t, readLookupJSON("testdata/lookups/questions.json", &questions))
	for _, tc := range []struct {
		name, answer, sessionID, source string
		verdict                         string
	}{
		{"topic", `{"namespace":"violet ledger","reason":"the old namespace caused lock contention","source_session_id":"codex:11111111-1111-4111-8111-111111111111"}`, "codex:11111111-1111-4111-8111-111111111111", "The harbor cache uses the violet ledger namespace because the old namespace caused lock contention.", "success"},
		{"named", `{"release_marker":"north pier marker","source_session_id":"codex:22222222-2222-4222-8222-222222222222"}`, "codex:22222222-2222-4222-8222-222222222222", "The Aurora deploy checklist says to use the north pier marker for the release.", "success"},
		{"named", `{"release_marker":"south pier marker","source_session_id":"codex:22222222-2222-4222-8222-222222222222"}`, "codex:22222222-2222-4222-8222-222222222222", "The Aurora deploy checklist says to use the north pier marker for the release.", "failed"},
		{"named", `{"release_marker":"north pier marker","source_session_id":"codex:11111111-1111-4111-8111-111111111111"}`, "codex:11111111-1111-4111-8111-111111111111", "The harbor cache uses the violet ledger namespace because the old namespace caused lock contention.", "failed"},
		{"long-result", `{"index":"account-created-project","source_session_id":"codex:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`, "codex:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "The accepted index name is account_created_project.", "failed"},
	} {
		t.Run(tc.name+"/"+tc.answer, func(t *testing.T) {
			for _, q := range questions {
				if q.Name == tc.name {
					call := lookupCall{Route: "mcp", Result: `{"session_id":"` + tc.sessionID + `","content":` + stringMustJSON(t, tc.source) + `}`, Start: 1, End: 2}
					score := scoreLookupQuestion(q, tc.answer, []lookupCall{call}, nil)
					assert.Equal(t, tc.verdict, score.Verdict)
					return
				}
			}
			require.FailNow(t, "question missing")
		})
	}
}

func TestLookupStructuredAnswerWithIncompleteCapture(t *testing.T) {
	q := lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}}
	score := scoreLookupQuestion(q, "", nil, errCaptureIncomplete)
	assert.Equal(t, "unscored", score.Verdict)
}

func TestLookupStructuredAnswerInMarkdownFence(t *testing.T) {
	q := lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}, Phrases: []string{"maple team now owns"}}
	calls := []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"maple team now owns"}`, Start: 1, End: 2}}
	answer := "```json\n{\"team\":\"maple team\",\"source_session_id\":\"codex:" + lookupID + "\"}\n```"
	score := scoreLookupQuestion(q, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupIdentifierSpellingIsExact(t *testing.T) {
	item := lookupItem{SessionID: "codex:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}
	question := lookupQuestion{Expected: []lookupItem{item}, ExpectedAnswer: map[string]string{"index": "account_created_project"}, Phrases: []string{"account_created_project"}}
	calls := []lookupCall{{Route: "mcp", Result: `{"session_id":"` + item.SessionID + `","snippet":"account_created_project"}`, Start: 1, End: 2}}
	answer := `{"index":"account-created-project","source_session_id":"` + item.SessionID + `"}`
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "failed", score.Verdict)
}

func TestLookupAmbiguousDirectReadDoesNotAttributeOtherSession(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	otherID := "22222222-2222-4222-8222-222222222222"
	calls := []lookupCall{
		{Route: "direct", Input: "sqlite3 sessions.db select * where session_id not in ('" + lookupID + "')", Result: `{"rows":[{"session_id":"codex:` + otherID + `","content":"violet ledger lock contention"},{"session_id":"claude:33333333-3333-4333-8333-333333333333","content":"another note"}]}`, Start: 1, End: 2},
		{Route: "direct", Input: "sqlite3 sessions.db select " + lookupID, Result: `{"session_id":"` + lookupID + `","content":"violet ledger lock contention"}`, Start: 3, End: 4},
	}
	score := scoreLookup(item.SessionID+" violet ledger lock contention", []lookupItem{item}, []string{"violet ledger", "lock contention"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupExplicitResultSourceOverridesMCPInput(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	calls := []lookupCall{
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + item.SessionID + `"}`, Result: `{"messages":[{"session_id":"claude:22222222-2222-4222-8222-222222222222","content":"violet ledger lock contention"}]}`, Start: 1, End: 2},
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + item.SessionID + `"}`, Result: `{"messages":[{"content":"violet ledger lock contention"}]}`, Start: 3, End: 4},
	}
	score := scoreLookup(item.SessionID+" violet ledger lock contention", []lookupItem{item}, []string{"violet ledger", "lock contention"}, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupSavedOutputClassification(t *testing.T) {
	assert.Equal(t, "direct", routeFor("Read", `{"file_path":"/tmp/tool-results/result.txt"}`))
	assert.Equal(t, "cli", routeFor("Bash", `{"command":"agentsview session list"}`))
	assert.Equal(t, "direct", routeFor("Bash", `{"command":"sqlite3 sessions.db 'select 1'"}`))
	assert.NotContains(t, routeFor("Read", `{"file_path":"README.md"}`), "mcp")
}

func TestLookupShellTranscriptExcerptsUsePathSessionID(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	item := lookupItem{SessionID: "codex:" + id}
	for _, tc := range []struct {
		name, input string
	}{
		{"Bash sed", `{"command":"sed -n '1,20p' /fixture-home/.codex/sessions/2026/11/24/rollout-2026-11-24T12-00-00-` + id + `.jsonl"}`},
		{"Bash tail", `{"command":"tail -n 20 '/fixture-home/.codex/sessions/2026/11/24/rollout-2026-11-24T12-00-00-` + id + `.jsonl'"}`},
		{"Codex tail", `tail -n 20 /fixture-home/.codex/sessions/2026/11/24/rollout-2026-11-24T12-00-00-` + id + `.jsonl`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "Bash"
			if strings.HasPrefix(tc.name, "Codex") {
				name = "CommandExecution"
			}
			call := lookupCall{Name: name, Route: "direct", Input: tc.input, Result: "The violet ledger uses lock contention mitigation.", Start: 1, End: 2}
			score := scoreLookup(item.SessionID+" violet ledger", []lookupItem{item}, []string{"violet ledger"}, []lookupCall{call}, nil)
			assert.Equal(t, "success", score.Verdict)
			require.NotNil(t, score.CallsToHit)
			assert.Equal(t, 1, *score.CallsToHit)
		})
	}
}

func TestLookupPromptAndQuestionValidation(t *testing.T) {
	question := lookupQuestion{
		Name:           "topic",
		Prompt:         "Which namespace did the review choose, and why?",
		Expected:       []lookupItem{{SessionID: "codex:" + lookupID}},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger", "reason": "lock contention"},
		Phrases:        []string{"violet ledger", "lock contention"},
	}
	fixtures := []lookupFixture{{ID: lookupID, Content: "The violet ledger caused lock contention."}}
	require.NoError(t, validateLookupQuestions(fixtures, []lookupQuestion{question}))
	prompt := lookupPrompt(question)
	assert.Contains(t, prompt, `["namespace","reason","source_session_id"]`)
	assert.NotContains(t, prompt, "violet ledger")
	assert.NotContains(t, prompt, "codex:")

	question.ExpectedAnswer = nil
	assert.Error(t, validateLookupQuestions(fixtures, []lookupQuestion{question}))
}

func TestLookupQuestionValidationRejectsMissingEvidence(t *testing.T) {
	fixture := lookupFixture{ID: lookupID, Content: "The violet ledger caused lock contention."}
	question := lookupQuestion{
		Name:           "topic",
		Prompt:         "What namespace did the review choose?",
		Expected:       []lookupItem{{SessionID: "codex:" + lookupID}},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger"},
		Phrases:        []string{"violet ledger"},
	}

	missingSession := question
	missingSession.Expected = []lookupItem{{SessionID: "codex:22222222-2222-4222-8222-222222222222"}}
	require.ErrorContains(t, validateLookupQuestions([]lookupFixture{fixture}, []lookupQuestion{missingSession}), "missing from the corpus")

	missingPhrase := question
	missingPhrase.Phrases = []string{"copper namespace"}
	require.ErrorContains(t, validateLookupQuestions([]lookupFixture{fixture}, []lookupQuestion{missingPhrase}), "absent from its expected sessions")
}
