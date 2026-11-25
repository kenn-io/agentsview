package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"strconv"
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

func lookupTestCase(answerText string, expected []lookupItem, values []string) (lookupQuestion, string) {
	sourceID := "unknown"
	for _, item := range expected {
		if _, _, prefixed := strings.Cut(item.SessionID, ":"); prefixed && strings.Contains(strings.ToLower(answerText), strings.ToLower(item.SessionID)) {
			sourceID = item.SessionID
			break
		}
	}
	if sourceID == "unknown" {
		for _, item := range expected {
			_, raw, prefixed := strings.Cut(item.SessionID, ":")
			if containsSessionID(answerText, item.SessionID) {
				if !prefixed {
					raw = item.SessionID
				}
				sourceID = raw
				break
			}
		}
	}
	if len(values) == 0 && len(expected) > 0 {
		values = []string{expected[0].SessionID}
	}
	q := lookupQuestion{Expected: expected, ExpectedAnswer: make(map[string]string, len(values))}
	answer := make(map[string]string, len(values)+1)
	for i, value := range values {
		field := "value_" + strconv.Itoa(i)
		q.ExpectedAnswer[field] = value
		answerValue := "incorrect"
		if containsLookupPhrase(answerText, value) {
			answerValue = value
		}
		answer[field] = answerValue
	}
	answer["source_session_id"] = sourceID
	b, _ := json.Marshal(answer)
	return q, string(b)
}

func TestLookupCodexScriptRequiresIndividualResults(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"await tools.mcp__agentsview__search_content({}); await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":"` + lookupID + `"}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.True(t, calls[0].Missing)
	question, answer := lookupTestCase(lookupID, []lookupItem{{SessionID: lookupID}}, nil)
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "unscored", score.Verdict)
	assert.Nil(t, score.CallsToHit)
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

func TestLookupDoesNotCountNestedEmptyResult(t *testing.T) {
	call := lookupCall{Route: "mcp", Name: "list_sessions", Result: `{"sessions":[{"id":"codex:1","messages":[]}],"total":1}`}
	resultFlags(&call)
	assert.False(t, call.Empty)
}

func TestLookupEmptyUsesToolResultField(t *testing.T) {
	call := lookupCall{Route: "mcp", Name: "mcp__agentsview__search_content", Result: `{"matches":[{"content":"found"}],"messages":[]}`}
	resultFlags(&call)
	assert.False(t, call.Empty)
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
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, []lookupCall{{Route: "mcp", Result: string(body), Start: 1, End: 2}}, nil)
	assert.Equal(t, "success", score.Verdict)
}

func TestLookupCodexParallelResultsAndWait(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"x","input":"tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"x","output":"Script running with cell ID 123"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"wait","call_id":"w","arguments":"{\"cell_id\":\"123\"}"}}
{"timestamp":"2026-11-24T12:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602500,"completed_at_ms":1795521603000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"unrelated\"}"}]}}}}
{"timestamp":"2026-11-24T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"w","output":"done"}}
{"timestamp":"2026-11-24T12:00:04Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521602700,"completed_at_ms":1795521604000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","status":"completed","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupKeepsStructuredSessionRowsSeparate(t *testing.T) {
	source := "codex:" + lookupID
	other := "codex:22222222-2222-4222-8222-222222222222"
	searchResult := `{"structuredContent":{"matches":[{"session_id":"` + source + `","snippet":"unrelated"},{"session_id":"` + other + `","snippet":"violet ledger"}]}}`
	messageResult := `{"structuredContent":{"messages":[{"content":"violet ledger"}]}}`
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"function_call","name":"mcp__agentsview__search_content","call_id":"search","arguments":"{}"}}
{"timestamp":"2026-11-24T12:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"search","output":` + stringMustJSON(t, searchResult) + `}}
{"timestamp":"2026-11-24T12:00:02Z","type":"response_item","payload":{"type":"function_call","name":"mcp__agentsview__get_messages","call_id":"messages","arguments":` + stringMustJSON(t, `{"session_id":"`+source+`"}`) + `}}
{"timestamp":"2026-11-24T12:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"messages","output":` + stringMustJSON(t, messageResult) + `}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	question := lookupQuestion{
		Expected:       []lookupItem{{SessionID: source}, {SessionID: other}},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger"},
	}
	answer := `{"namespace":"violet ledger","source_session_id":"` + source + `"}`
	for i := range calls {
		resultFlags(&calls[i])
	}
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupGetMessagesUsesItsRequestedSessionScope(t *testing.T) {
	source := "codex:" + lookupID
	other := "codex:22222222-2222-4222-8222-222222222222"
	call := lookupCall{
		Name: "get_messages", Route: "mcp",
		Input:            `{"session_id":"` + source + `"}`,
		StructuredResult: `{"messages":[{"content":"The older session ` + other + ` used violet ledger."}]}`,
		Result:           "The older session " + other + " used violet ledger.",
		Start:            1, End: 2,
	}
	question := lookupQuestion{
		Expected:       []lookupItem{{SessionID: source}},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger"},
	}
	answer := `{"namespace":"violet ledger","source_session_id":"` + source + `"}`
	score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupCountsOtherToolWhenItsResultSuppliesEvidence(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"read","name":"Bash","input":{"command":"for file in $(find transcripts); do read_session \"$file\"; done"}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"read","content":"session_id: ` + lookupID + `\nviolet ledger"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "other", calls[0].Route)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupAttributesMixedResultsToSessionRows(t *testing.T) {
	source := "codex:" + lookupID
	other := "codex:22222222-2222-4222-8222-222222222222"
	question := lookupQuestion{
		Expected:       []lookupItem{{SessionID: source}, {SessionID: other}},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger", "reason": "lock contention"},
	}
	answer := `{"namespace":"violet ledger","reason":"lock contention","source_session_id":"` + source + `"}`
	for _, tc := range []struct {
		name   string
		result string
		calls  int
	}{
		{
			name:   "other row has the complete answer",
			result: `{"results":[{"session_id":"` + source + `","content":"other"},{"session_id":"` + other + `","content":"violet ledger lock contention"}]}`,
			calls:  2,
		},
		{
			name:   "plain text rows have separate answers",
			result: "session_id: " + source + "\ncontent: other\n\nsession_id: " + other + "\ncontent: violet ledger lock contention",
			calls:  2,
		},
		{
			name:   "selected row has one answer field",
			result: `{"results":[{"session_id":"` + source + `","content":"violet ledger"},{"session_id":"` + other + `","content":"lock contention"}]}`,
			calls:  2,
		},
		{
			name:   "selected row has the complete answer",
			result: `{"results":[{"session_id":"` + source + `","content":"violet ledger lock contention"},{"session_id":"` + other + `","content":"other"}]}`,
			calls:  1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := []lookupCall{
				{Route: "mcp", Result: tc.result, Start: 1, End: 2},
				{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + source + `"}`, Result: "violet ledger lock contention", Start: 3, End: 4},
			}
			score := scoreLookupQuestion(question, answer, calls, nil)
			assert.Equal(t, "success", score.Verdict)
			require.NotNil(t, score.CallsToHit)
			assert.Equal(t, tc.calls, *score.CallsToHit)
		})
	}
}

func TestLookupIncludesChildTranscript(t *testing.T) {
	root := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"no match"}]}}`
	child := `{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(root), []byte(child)})
	require.NoError(t, err)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupIgnoresLaterUntimedCallForHit(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"response_item","payload":{"type":"function_call","name":"exec","call_id":"parent","arguments":"await tools.mcp__agentsview__search_content({})"}}
{"timestamp":"2026-11-24T12:00:02Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1795521601000,"completed_at_ms":1795521602000,"item":{"type":"McpToolCall","server":"agentsview","tool":"search_content","result":{"content":[{"type":"text","text":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}}}
{"timestamp":"2026-11-24T12:00:01.500Z","type":"response_item","payload":{"type":"function_call","name":"exec","call_id":"unfinished","arguments":"tools.mcp__agentsview__get_messages({})"}}`
	calls, err := extractLookupCalls(t.Context(), clientCodex, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
	assert.Equal(t, 1, score.MCP)
	assert.Equal(t, 1, score.Other)
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

func TestLookupClaudeToolSearchCountsAsDiscovery(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T11:59:58Z","message":{"content":[{"type":"tool_use","id":"other","name":"ToolSearch","input":{"query":"select:browser__search"}}]}}
{"timestamp":"2026-11-24T11:59:59Z","message":{"content":[{"type":"tool_result","tool_use_id":"other","content":"Found a browser search tool."}]}}
{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"discover","name":"ToolSearch","input":{"query":"select:mcp__agentsview__search_content"}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"discover","content":"Found the AgentsView search tool."}]}}
{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"search","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"search","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	assert.Equal(t, "tool discovery", calls[0].Name)
	assert.Equal(t, "other", calls[0].Route)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupClaudePairsResultsAndScoresFirstHit(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"empty"}]}}
{"timestamp":"2026-11-24T12:00:02Z","message":{"content":[{"type":"tool_use","id":"2","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"2","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}
{"timestamp":"2026-11-24T12:00:04Z","message":{"content":[{"type":"tool_use","id":"3","name":"mcp__agentsview__get_messages","input":{"session_id":"other"}}]}}
{"timestamp":"2026-11-24T12:00:05Z","message":{"content":[{"type":"tool_result","tool_use_id":"3","content":"other"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupClaudeIgnoresAPIRequestBlobs(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","type":"assistant","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}}]}}
{"timestamp":"2026-11-24T12:00:01Z","type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}
{"type":"api-request-blob","message":{"content":[{"type":"tool_use","id":"1","name":"mcp__agentsview__search_content","input":{}},{"type":"tool_result","tool_use_id":"1","content":"{\"session_id\":\"codex:` + lookupID + `\",\"snippet\":\"violet ledger\"}"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
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
		{"result lacks answer phrase", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"other"}`, Start: 1, End: 2}}, nil, "failed", 0},
		{"empty session read", "codex:" + lookupID + " violet ledger", []lookupCall{{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + lookupID + `"}`, Result: `{"messages":[],"results":[{"session_id":"codex:` + lookupID + `","content":"violet ledger"}]}`, Start: 1, End: 2}}, nil, "success", 1},
		{"empty direct output", "codex:" + lookupID + " violet ledger", []lookupCall{{Name: "Read", Route: "direct", Input: `{"file_path":"/archive/rollout-` + lookupID + `.jsonl"}`, Result: `{"sessions":[],"results":[{"session_id":"codex:` + lookupID + `","content":"violet ledger"}]}`, Start: 1, End: 2}}, nil, "success", 1},
		{"failed session read", "codex:" + lookupID + " violet ledger", []lookupCall{{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + lookupID + `"}`, Result: `{"isError":true,"results":[{"session_id":"codex:` + lookupID + `","content":"violet ledger"}]}`, Error: true, Start: 1, End: 2}}, nil, "failed", 0},
		{"missing", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Missing: true, Start: 1, End: 2}, {Route: "direct", Name: "Read", Input: `{"file_path":"/archive/rollout-` + lookupID + `.jsonl"}`, Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 3, End: 4}}, nil, "unscored", 0},
		{"uncited", "violet ledger", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, nil, "failed", 0},
		{"wrong answer", "codex:" + lookupID + " other", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, nil, "failed", 0},
		{"timeout", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, errors.New("timeout"), "failed", 0},
		{"timeout with incomplete capture", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}}, errors.Join(context.DeadlineExceeded, errCaptureIncomplete), "failed", 0},
		{"client failure with incomplete capture", "codex:" + lookupID + " violet ledger", []lookupCall{{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}}, errors.Join(errLookupClientFailed, errCaptureIncomplete), "failed", 0},
		{"unsupported lookup capture format", "codex:" + lookupID + " violet ledger", nil, errLookupCaptureUnsupported, "unscored", 0},
		{"incomplete capture", "codex:" + lookupID + " violet ledger", nil, errCaptureIncomplete, "unscored", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.calls {
				resultFlags(&tc.calls[i])
			}
			question, answer := lookupTestCase(tc.answer, []lookupItem{item}, []string{"violet ledger"})
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
	wrong := lookupCall{Route: "mcp", Result: `{"session_id":"claude:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2}
	right := lookupCall{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 3, End: 4}
	question, answer := lookupTestCase("codex:"+lookupID+" violet ledger", []lookupItem{item}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, []lookupCall{wrong, right}, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
}

func TestLookupClassifiesTranscriptFromGrepOutput(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"violet ledger","path":"."}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"transcripts/2026/01/01/rollout-2026-11-24T12-00-00-` + lookupID + `.jsonl:session ` + lookupID + ` violet ledger"}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "direct", calls[0].Route)
	question, answer := lookupTestCase(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, 1, score.Direct)
	assert.Equal(t, "success", score.Verdict)
}

func TestLookupSummaryKeepsFailuresInDenominator(t *testing.T) {
	one, two := 1, 2
	summary := summarizeLookups([]lookupAttempt{{Case: "topic", Score: lookupScore{Verdict: "success", CallsToHit: &one}}, {Case: "topic", Score: lookupScore{Verdict: "success", CallsToHit: &two}}, {Case: "topic", Score: lookupScore{Verdict: "failed"}}, {Case: "topic", Score: lookupScore{Verdict: "unscored"}}})
	require.Len(t, summary, 1)
	assert.InDelta(t, 25, summary[0].OneCallPercent, 0.01)
	assert.InDelta(t, 50, summary[0].TwoCallPercent, 0.01)
}

func TestLookupCLIMessageReadAttribution(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	for _, tc := range []struct {
		name, input string
	}{
		{"command", "agentsview session messages " + item.SessionID},
		{"shell input", `{"command":"agentsview session messages ` + item.SessionID + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := lookupCall{Route: "cli", Input: tc.input, Result: "violet ledger", Start: 1, End: 2}
			question, answer := lookupTestCase(item.SessionID+" violet ledger", []lookupItem{item}, []string{"violet ledger"})
			score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
			assert.Equal(t, "success", score.Verdict)
			require.NotNil(t, score.CallsToHit)
			assert.Equal(t, 1, *score.CallsToHit)
		})
	}
}

func TestLookupDirectSessionInInput(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	call := lookupCall{Route: "direct", Input: `{"command":"sqlite3 sessions.db SELECT WHERE session_id='codex:` + lookupID + `'"}`, Result: "recorded answer", Start: 1, End: 2}
	question, answer := lookupTestCase("codex:"+lookupID+" answer", []lookupItem{item}, []string{"answer"})
	score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
}

func TestLookupWildcardTranscriptRead(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	transcript := `{"type":"session_meta","payload":{"id":"` + lookupID + `"}}` + "\n" + `{"payload":{"content":"violet ledger"}}`
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Bash","input":{"command":"cat ~/.codex/sessions/**/*.jsonl"}}]}}` + "\n" + `{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":` + stringMustJSON(t, transcript) + `}]}}`
	calls, err := extractLookupCalls(t.Context(), clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	question, answer := lookupTestCase(item.SessionID+" violet ledger", []lookupItem{item}, []string{"violet ledger"})
	score := scoreLookupQuestion(question, answer, calls, nil)
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
	question, answer := lookupTestCase(item.SessionID+" answer", []lookupItem{item}, []string{"answer"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
	assert.Equal(t, 2, score.Other)
}

func TestLookupAccumulatesFactsForCitedSession(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	calls := []lookupCall{
		{Route: "mcp", Result: `{"session_id":"codex:` + lookupID + `","snippet":"violet ledger"}`, Start: 1, End: 2},
		{Route: "mcp", Result: `{"results":[{"session_id":"` + lookupID + `","content":"other"},{"session_id":"22222222-2222-4222-8222-222222222222","content":"lock contention"}]}`, Start: 3, End: 4},
		{Name: "get_messages", Route: "mcp", Input: `{"session_id":"` + item.SessionID + `"}`, Result: "lock contention", Start: 5, End: 6},
	}
	question, answer := lookupTestCase(item.SessionID+" violet ledger lock contention", []lookupItem{item}, []string{"violet ledger", "lock contention"})
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 3, *score.CallsToHit)
}

func TestLookupCallsToHitIncludesCompleteAnswerValue(t *testing.T) {
	id := "codex:" + lookupID
	question := lookupQuestion{
		Expected:       []lookupItem{{SessionID: id}},
		ExpectedAnswer: map[string]string{"release_marker": "north pier marker for the release"},
	}
	answer := `{"release_marker":"north pier marker for the release","source_session_id":"` + id + `"}`
	calls := []lookupCall{
		{Route: "mcp", Name: "search_content", Result: `{"matches":[{"session_id":"` + id + `","content":"north pier"}]}`, Start: 1, End: 2},
		{Route: "mcp", Name: "get_messages", Input: `{"session_id":"` + id + `"}`, Result: `{"messages":[{"content":"north pier marker for the release"}]}`, Start: 3, End: 4},
	}
	score := scoreLookupQuestion(question, answer, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
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
			name:     "wrong owner with expected word",
			answer:   `{"team":"cedar; maple is not the owner","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict:  "failed",
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
			name:   "ambiguous bare source ID",
			answer: `{"team":"maple team","source_session_id":"` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{
				{SessionID: "codex:" + lookupID},
				{SessionID: "claude:" + lookupID},
			}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			verdict: "failed",
		},
		{
			name:     "incomplete capture",
			answer:   `{"team":"maple team","source_session_id":"codex:` + lookupID + `"}`,
			question: lookupQuestion{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}, ExpectedAnswer: map[string]string{"team": "maple team"}},
			runErr:   errCaptureIncomplete,
			verdict:  "unscored",
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

func TestLookupAcceptsSourceExcerpts(t *testing.T) {
	var fixtures []lookupFixture
	require.NoError(t, readLookupJSON("testdata/lookups/corpus.json", &fixtures))
	sources := make(map[string]string, len(fixtures))
	for _, fixture := range fixtures {
		sources["codex:"+fixture.ID] = fixture.Content
	}
	var questions []lookupQuestion
	require.NoError(t, readLookupJSON("testdata/lookups/questions.json", &questions))
	for _, tc := range []struct {
		name, answer, sessionID string
		verdict                 string
	}{
		{"topic", `{"namespace":"violet ledger","reason":"lock contention","source_session_id":"codex:11111111-1111-4111-8111-111111111111"}`, "codex:11111111-1111-4111-8111-111111111111", "success"},
		{"named", `{"release_marker":"north pier","source_session_id":"codex:22222222-2222-4222-8222-222222222222"}`, "codex:22222222-2222-4222-8222-222222222222", "success"},
		{"named", `{"release_marker":"south pier marker","source_session_id":"codex:22222222-2222-4222-8222-222222222222"}`, "codex:22222222-2222-4222-8222-222222222222", "failed"},
		{"named", `{"release_marker":"north pier marker","source_session_id":"codex:11111111-1111-4111-8111-111111111111"}`, "codex:11111111-1111-4111-8111-111111111111", "failed"},
		{"long-result", `{"index":"account-created-project","source_session_id":"codex:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`, "codex:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "failed"},
	} {
		t.Run(tc.name+"/"+tc.answer, func(t *testing.T) {
			source, ok := sources[tc.sessionID]
			require.True(t, ok, "fixture source for %s", tc.sessionID)
			for _, q := range questions {
				if q.Name == tc.name {
					call := lookupCall{Route: "mcp", Result: `{"session_id":"` + tc.sessionID + `","content":` + stringMustJSON(t, source) + `}`, Start: 1, End: 2}
					score := scoreLookupQuestion(q, tc.answer, []lookupCall{call}, nil)
					assert.Equal(t, tc.verdict, score.Verdict)
					return
				}
			}
			require.FailNow(t, "question missing")
		})
	}
}

func TestLookupHumanReadableSearchRowWithoutPath(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	question := lookupQuestion{
		Expected:       []lookupItem{item},
		ExpectedAnswer: map[string]string{"namespace": "violet ledger"},
	}
	answer := `{"namespace":"violet ledger","source_session_id":"` + item.SessionID + `"}`
	call := lookupCall{
		Route:  "mcp",
		Result: "Search results\nsession_id: " + item.SessionID + "\nsnippet: violet ledger",
		Start:  1,
		End:    2,
	}
	score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupResultNamingOnlyAnotherSessionDoesNotCount(t *testing.T) {
	item := lookupItem{SessionID: "codex:" + lookupID}
	question := lookupQuestion{
		Expected:       []lookupItem{item},
		ExpectedAnswer: map[string]string{"team": "maple team"},
	}
	answer := `{"team":"maple team","source_session_id":"` + item.SessionID + `"}`
	other := "codex:22222222-2222-4222-8222-222222222222"
	call := lookupCall{
		Route:  "mcp",
		Result: "Search results\nsession_id: " + other + "\nsnippet: maple team",
		Start:  1,
		End:    2,
	}
	score := scoreLookupQuestion(question, answer, []lookupCall{call}, nil)
	assert.Equal(t, "failed", score.Verdict)
	assert.Nil(t, score.CallsToHit)
}

func TestLookupSavedOutputClassification(t *testing.T) {
	assert.Equal(t, "direct", routeFor("Read", `{"file_path":"/tmp/tool-results/result.txt"}`))
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
