package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	calls, sources, err := readLookupTranscriptCalls(clientCodex, t.TempDir(), []byte(trace))
	require.ErrorIs(t, err, errCaptureIncomplete)
	assert.Zero(t, sources)
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
	ordinal := 0
	score := scoreLookup(lookupID+"?msg=0 violet ledger", []lookupItem{{SessionID: lookupID, Ordinal: &ordinal}}, []string{"violet ledger"}, []lookupCall{{Route: "mcp", Result: string(body), Start: 1, End: 2}}, nil)
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
	assert.True(t, calls[0].Nested)
	assert.True(t, calls[1].Nested)
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, calls, nil)
	assert.Equal(t, "success", score.Verdict)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 2, *score.CallsToHit)
	assert.Equal(t, "mcp", score.SupplyingRoute)
}

func TestLookupCodexPreflightScoresIdentifiedNestedResults(t *testing.T) {
	calls := []lookupCall{
		{Name: "search_content", Route: "mcp", Result: `{"matches":[{"session_id":"` + lookupID + `"}]}`, Start: 1, End: 2, Nested: true},
		{Name: "get_messages", Route: "mcp", Result: `{"messages":[{"ordinal":1,"content":"violet ledger"}]}`, Start: 3, End: 4, Nested: true},
	}
	assert.True(t, lookupPreflightComplete(calls))
	calls[0].Name = "search_sessions"
	assert.True(t, lookupPreflightComplete(calls))
	assert.Equal(t, "code-mode-only", lookupToolDeliveryMode(clientCodex, calls))
	assert.Equal(t, "native", lookupToolDeliveryMode(clientClaude, calls))
	calls[1].Result = `{"messages":[]}`
	calls[1].Empty = true
	assert.False(t, lookupPreflightComplete(calls))
	calls = []lookupCall{{Name: "exec", Route: "other", Missing: true}}
	assert.False(t, lookupPreflightComplete(calls))
}

func TestLookupPreflightUsesManifestSession(t *testing.T) {
	prompt, err := lookupPreflightPrompt([]lookupQuestion{{Expected: []lookupItem{{SessionID: "codex:" + lookupID}}}})
	require.NoError(t, err)
	assert.Contains(t, prompt, "codex:"+lookupID)
	assert.Contains(t, prompt, "search_sessions")
	assert.Contains(t, prompt, "get_messages")
	_, err = lookupPreflightPrompt([]lookupQuestion{{}})
	assert.Error(t, err)
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
	ordinal := 3
	item := lookupItem{SessionID: lookupID, Ordinal: &ordinal}
	cases := []struct {
		name, answer   string
		calls          []lookupCall
		err            error
		verdict, route string
		hit            int
		phrases        []string
	}{
		{"message", lookupID + "?msg=3 violet ledger lock contention", []lookupCall{{Route: "mcp", Result: lookupID + ` "ordinal":3 violet ledger lock contention`, Start: 1, End: 2}}, nil, "success", "mcp-only", 1, []string{"violet ledger", "lock contention"}},
		{"formatted citation", "session `" + lookupID + "`, message ordinal `3`", []lookupCall{{Route: "mcp", Result: lookupID + ` "ordinal":3`, Start: 1, End: 2}}, nil, "success", "mcp-only", 1, nil},
		{"message identity without answer evidence", lookupID + "?msg=3 violet ledger", []lookupCall{{Route: "mcp", Result: lookupID + ` "ordinal":3`, Start: 1, End: 2}}, nil, "unscored", "mcp-only", 0, []string{"violet ledger"}},
		{"message answer arrives in later read", lookupID + "?msg=3 violet ledger", []lookupCall{{Route: "mcp", Result: lookupID + ` "ordinal":3`, Start: 1, End: 2}, {Route: "mcp", Result: lookupID + ` "ordinal":3 violet ledger`, Start: 3, End: 4}}, nil, "success", "mcp-only", 2, []string{"violet ledger"}},
		{"message read joins session argument to result", lookupID + "?msg=3 violet ledger", []lookupCall{{Name: "get_messages", Route: "mcp", Input: `{"session_id":"codex:` + lookupID + `"}`, Result: `{"messages":[{"ordinal":3,"content":"violet ledger"}]}`, Start: 1, End: 2}}, nil, "success", "mcp-only", 1, []string{"violet ledger"}},
		{"message read rejects a different session argument", lookupID + "?msg=3 violet ledger", []lookupCall{{Name: "get_messages", Route: "mcp", Input: `{"session_id":"codex:22222222-2222-4222-8222-222222222222"}`, Result: `{"messages":[{"ordinal":3,"content":"violet ledger"}]}`, Start: 1, End: 2}}, nil, "unscored", "mcp-only", 0, []string{"violet ledger"}},
		{"message evidence cannot mix search rows", lookupID + "?msg=3 violet ledger", []lookupCall{{Name: "search_content", Route: "mcp", Result: `{"results":[{"session_id":"codex:` + lookupID + `","match_ordinal":3,"snippet":"unrelated"},{"session_id":"codex:22222222-2222-4222-8222-222222222222","match_ordinal":3,"snippet":"violet ledger"}]}`, Start: 1, End: 2}}, nil, "unscored", "mcp-only", 0, []string{"violet ledger"}},
		{"message evidence is in the cited search row", lookupID + "?msg=3 violet ledger", []lookupCall{{Name: "search_content", Route: "mcp", Result: `{"results":[{"session_id":"codex:` + lookupID + `","match_ordinal":3,"snippet":"violet ledger"}]}`, Start: 1, End: 2}}, nil, "success", "mcp-only", 1, []string{"violet ledger"}},
		{"message evidence in plain content block", lookupID + "?msg=3 violet ledger", []lookupCall{{Route: "mcp", Result: `{"content":[{"type":"text","text":"session ` + lookupID + ` message 3: violet ledger"}]}`, Start: 1, End: 2}}, nil, "success", "mcp-only", 1, []string{"violet ledger"}},
		{"content blocks cannot mix message evidence", lookupID + "?msg=3 violet ledger", []lookupCall{{Route: "mcp", Result: `{"content":[{"type":"text","text":"session ` + lookupID + ` message 3: unrelated"},{"type":"text","text":"session 22222222-2222-4222-8222-222222222222 message 3: violet ledger"}]}`, Start: 1, End: 2}}, nil, "unscored", "mcp-only", 0, []string{"violet ledger"}},
		{"session alone", lookupID + "?msg=3", []lookupCall{{Route: "mcp", Result: lookupID, Start: 1, End: 2}}, nil, "unscored", "mcp-only", 0, nil},
		{"direct message requires ordinal", lookupID + "?msg=3", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, nil, "unscored", "direct-only", 0, nil},
		{"cli message requires ordinal", lookupID + "?msg=3", []lookupCall{{Route: "cli", Result: lookupID, Start: 1, End: 2}}, nil, "unscored", "cli-only", 0, nil},
		{"fallback", lookupID + "?msg=3", []lookupCall{{Route: "mcp", Result: "empty", Start: 1, End: 2}, {Route: "direct", Result: lookupID + " ordinal 3", Start: 3, End: 4}}, nil, "success", "fell-back", 2, nil},
		{"missing", lookupID + "?msg=3", []lookupCall{{Route: "mcp", Missing: true, Start: 1, End: 2}, {Route: "direct", Result: lookupID, Start: 3, End: 4}}, nil, "unscored", "fell-back", 0, nil},
		{"bad time", lookupID + "?msg=3", []lookupCall{{Route: "direct", Result: lookupID, Start: 0, End: 0}}, nil, "unscored", "direct-only", 0, nil},
		{"uncited", "answer", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, nil, "failed", "direct-only", 0, nil},
		{"timeout", lookupID + "?msg=3", []lookupCall{{Route: "direct", Result: lookupID, Start: 1, End: 2}}, errors.New("timeout"), "failed", "direct-only", 0, nil},
		{"incomplete capture", lookupID + "?msg=3", nil, errCaptureIncomplete, "unscored", "none", 0, nil},
		{"incomplete without answer", "", nil, errCaptureIncomplete, "unscored", "none", 0, nil},
		{"incomplete wrong answer", "incorrect", nil, errCaptureIncomplete, "failed", "none", 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			score := scoreLookup(tc.answer, []lookupItem{item}, tc.phrases, tc.calls, tc.err)
			assert.Equal(t, tc.verdict, score.Verdict)
			assert.Equal(t, tc.route, score.Route)
			if tc.hit == 0 {
				assert.Nil(t, score.CallsToHit)
			} else {
				require.NotNil(t, score.CallsToHit)
				assert.Equal(t, tc.hit, *score.CallsToHit)
			}
		})
	}
}

func TestLookupSessionEvidenceCannotMixSearchRows(t *testing.T) {
	call := lookupCall{
		Name: "search_sessions", Route: "mcp", Start: 1, End: 2,
		Result: `{"results":[{"session_id":"codex:` + lookupID + `","snippet":"cedar"},{"session_id":"codex:22222222-2222-4222-8222-222222222222","snippet":"maple"}]}`,
	}
	score := scoreLookup(lookupID+" maple", []lookupItem{{SessionID: lookupID}}, []string{"maple"}, []lookupCall{call}, nil)
	assert.Equal(t, "unscored", score.Verdict)
	assert.Nil(t, score.CallsToHit)
}

func TestLookupDirectAndCLIEvidenceCannotMixRows(t *testing.T) {
	for _, route := range []string{"direct", "cli"} {
		for _, result := range []string{
			`{"results":[{"session_id":"codex:` + lookupID + `","snippet":"cedar"},{"session_id":"codex:22222222-2222-4222-8222-222222222222","snippet":"maple"}]}`,
			`{"session_id":"codex:` + lookupID + `","snippet":"cedar"}` + "\n" + `{"session_id":"codex:22222222-2222-4222-8222-222222222222","snippet":"maple"}`,
			"session codex:" + lookupID + " cedar\nsession codex:22222222-2222-4222-8222-222222222222 maple",
		} {
			call := lookupCall{Route: route, Result: result, Start: 1, End: 2}
			score := scoreLookup("codex:"+lookupID+" maple", []lookupItem{{SessionID: "codex:" + lookupID}}, []string{"maple"}, []lookupCall{call}, nil)
			assert.Equal(t, "unscored", score.Verdict, "%s: %s", route, result)
		}
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
	ordinal := 3
	item.Ordinal = &ordinal
	otherRead := lookupCall{Name: "get_messages", Route: "mcp", Input: `{"session_id":"claude:` + lookupID + `"}`, Result: `{"messages":[{"ordinal":3,"content":"violet ledger"}]}`, Start: 1, End: 2}
	score = scoreLookup("codex:"+lookupID+"?msg=3 violet ledger", []lookupItem{item}, []string{"violet ledger"}, []lookupCall{otherRead}, nil)
	assert.Equal(t, "unscored", score.Verdict)
}

func TestLookupClassifiesTranscriptFromGrepOutput(t *testing.T) {
	trace := `{"timestamp":"2026-11-24T12:00:00Z","message":{"content":[{"type":"tool_use","id":"1","name":"Grep","input":{"pattern":"violet ledger","path":"."}}]}}
{"timestamp":"2026-11-24T12:00:01Z","message":{"content":[{"type":"tool_result","tool_use_id":"1","content":"transcripts/2026/01/01/rollout-123.jsonl:session ` + lookupID + ` violet ledger"}]}}`
	calls, err := extractLookupCalls(clientClaude, [][]byte{[]byte(trace)})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	assert.Equal(t, "direct", calls[0].Route)
	score := scoreLookup(lookupID+" violet ledger", []lookupItem{{SessionID: lookupID}}, []string{"violet ledger"}, calls, nil)
	assert.Equal(t, "direct-only", score.Route)
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
	require.Len(t, summary, 2)
	assert.InDelta(t, 25, summary[0].OneCallPercent, 0.01)
	assert.InDelta(t, 50, summary[0].TwoCallPercent, 0.01)
	assert.Equal(t, 2, summary[0].ScoredSuccesses)
	assert.Equal(t, "all", summary[1].Case)
	assert.Equal(t, 4, summary[1].Scheduled)
	assert.InDelta(t, 50, summary[1].TwoCallPercent, 0.01)
}

func TestLookupDirectSessionResult(t *testing.T) {
	call := lookupCall{Route: "direct", Result: lookupID, Start: 1, End: 2}
	score := scoreLookup(lookupID, []lookupItem{{SessionID: lookupID}}, nil, []lookupCall{call}, nil)
	assert.Equal(t, "success", score.Verdict)
	assert.Equal(t, "direct-only", score.Route)
	require.NotNil(t, score.CallsToHit)
	assert.Equal(t, 1, *score.CallsToHit)
}

func TestLookupSavedOutputClassification(t *testing.T) {
	assert.Equal(t, "direct", routeFor("Read", `{"file_path":"/tmp/tool-results/result.txt"}`))
	assert.Equal(t, "cli", routeFor("Bash", `{"command":"agentsview session list"}`))
	assert.Equal(t, "direct", routeFor("Bash", `{"command":"sqlite3 sessions.db 'select 1'"}`))
	assert.False(t, strings.Contains(routeFor("Read", `{"file_path":"README.md"}`), "mcp"))
}

func TestReadLookupCorpusManifest(t *testing.T) {
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	manifest := `{"fixtures":[{"id":"fixture","title":"sample","content":"evidence"}],"sources":[{"provider":"codex","path":"source.jsonl","session_id":"codex:source"}],"questions":[{"name":"sample","prompt":"Find it","expected":[{"session_id":"codex:source"}],"phrases":["evidence"]}]}`
	require.NoError(t, os.WriteFile(manifestPath, []byte(manifest), 0o600))
	fixtures, sources, questions, root, err := readLookupCorpus("", manifestPath)
	require.NoError(t, err)
	assert.Equal(t, dir, root)
	assert.Len(t, fixtures, 1)
	assert.Equal(t, clientCodex, sources[0].Provider)
	assert.Equal(t, "source.jsonl", sources[0].Path)
	assert.Equal(t, "codex:source", sources[0].SessionID)
	assert.Equal(t, "sample", questions[0].Name)
}

func TestSeedLookupSourcesCopiesSelectedTranscript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test sync helper uses a shell script")
	}
	dir := t.TempDir()
	sourceRoot := filepath.Join(dir, "input")
	projectDir := filepath.Join(dir, "project")
	dataDir := filepath.Join(dir, "data")
	require.NoError(t, os.MkdirAll(sourceRoot, 0o700))
	require.NoError(t, os.MkdirAll(projectDir, 0o700))
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	source := filepath.Join(sourceRoot, "rollout-sample.jsonl")
	contents := []byte("frozen transcript\n")
	require.NoError(t, os.WriteFile(source, contents, 0o600))
	envOutput := filepath.Join(dir, "sync-env.txt")
	pathOutput := filepath.Join(dir, "sync-path.txt")
	bin := filepath.Join(dir, "agentsview")
	script := "#!/bin/sh\nprintf '%s' \"$CODEX_SESSIONS_DIR\" > " + shellQuote(envOutput) +
		"\nprintf '%s' \"$3\" > " + shellQuote(pathOutput) +
		"\nprintf '%s' '{\"id\":\"codex:source\"}'\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700))
	require.NoError(t, seedLookupSources(t.Context(), bin, dataDir, projectDir, []lookupSource{{
		Provider: clientCodex, Path: "rollout-sample.jsonl", SessionID: "codex:source",
	}}, sourceRoot))
	destination := filepath.Join(projectDir, "fixture-codex", "sessions", "2026", "11", "24", "source-1-rollout-sample.jsonl")
	got, err := os.ReadFile(destination)
	require.NoError(t, err)
	assert.Equal(t, contents, got)
	assert.Equal(t, filepath.Join(projectDir, "fixture-codex", "sessions"), mustReadFile(t, envOutput))
	assert.Equal(t, destination, mustReadFile(t, pathOutput))
	original, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, contents, original)
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}
