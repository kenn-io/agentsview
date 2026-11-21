package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

func toolCallMsg(sid string, ordinal int, calls ...db.ToolCall) db.Message {
	m := dbtest.AsstMsg(sid, ordinal, "tool use")
	m.Timestamp = "2024-06-01T10:00:00Z"
	m.HasToolUse = true
	for i := range calls {
		calls[i].SessionID = sid
		calls[i].CallIndex = i
	}
	m.ToolCalls = calls
	return m
}

func decodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	require.False(t, res.IsError, "tool returned error: %+v", res.Content)
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	var out T
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestGetToolCalls_FiltersPagesAndMasksSecrets(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("s1", 0,
			db.ToolCall{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"export KEY=AKIA7QHWN2DKR4FYPLJM && make"}`},
			db.ToolCall{ToolName: "Edit", Category: "Edit", InputJSON: `{"file_path":"a.go"}`, ResultContentLength: 12},
		),
		toolCallMsg("s1", 1,
			db.ToolCall{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"go test ./..."}`},
		),
	)
	dbtest.SeedSession(t, d, "empty", "proj")
	dbtest.SeedSession(t, d, "blank", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("blank", 0, db.ToolCall{ToolName: "Bash"}))

	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "s1", Category: "bash", Limit: 1})
	require.NoError(t, err)
	require.NotNil(t, out.Total)
	assert.Equal(t, 2, *out.Total)
	require.Len(t, out.ToolCalls, 1)
	assert.Equal(t, "Bash", out.ToolCalls[0].ToolName)
	assert.NotContains(t, *out.ToolCalls[0].Input, "AKIA7QHWN2DKR4FYPLJM")
	assert.Contains(t, *out.ToolCalls[0].Input, "make")
	require.NotNil(t, out.NextCursor)

	_, next, err := ts.toolCalls(t.Context(), nil, toolCallsIn{
		SessionID: "s1", Category: "Bash", Limit: 1, Cursor: *out.NextCursor, MaxInputChars: 5,
	})
	require.NoError(t, err)
	require.Len(t, next.ToolCalls, 1)
	assert.Equal(t, 1, next.ToolCalls[0].Ordinal)
	assert.Equal(t, `{"com`, *next.ToolCalls[0].Input)
	assert.True(t, next.ToolCalls[0].InputTruncated)
	assert.Nil(t, next.NextCursor)

	_, all, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "s1"})
	require.NoError(t, err)
	require.Len(t, all.ToolCalls, 3)
	assert.Equal(t, 12, all.ToolCalls[1].ResultLength)

	_, none, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "empty"})
	require.NoError(t, err)
	assert.Empty(t, none.ToolCalls)
	raw, err := json.Marshal(none)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"total":0`)
	_, blank, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "blank"})
	require.NoError(t, err)
	raw, err = json.Marshal(blank)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"category":""`)
	assert.Contains(t, string(raw), `"input":""`)

	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "missing"})
	require.ErrorContains(t, err, "session not found")
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "s1", Cursor: -1})
	require.Error(t, err)
}

func TestGetRecentEdits_PagesByFile(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("s1", 0, db.ToolCall{ToolName: "Edit", Category: "Edit", FilePath: "internal/a.go"}),
		toolCallMsg("s1", 1, db.ToolCall{ToolName: "Write", Category: "Write", FilePath: "internal/b.go"}),
		toolCallMsg("s1", 2, db.ToolCall{ToolName: "Edit", Category: "Edit", FilePath: "docs/c.md"}),
	)

	_, out, err := ts.recentEdits(t.Context(), nil, recentEditsIn{Path: "INTERNAL/", Limit: 1})
	require.NoError(t, err)
	require.Len(t, out.Files, 1)
	require.NotNil(t, out.NextCursor)
	assert.Equal(t, 1, *out.NextCursor)

	_, next, err := ts.recentEdits(t.Context(), nil, recentEditsIn{Path: "internal/", Limit: 1, Cursor: 1})
	require.NoError(t, err)
	require.Len(t, next.Files, 1)
	assert.Nil(t, next.NextCursor)
	assert.ElementsMatch(t, []string{"internal/a.go", "internal/b.go"},
		[]string{out.Files[0].FilePath, next.Files[0].FilePath})
	require.Len(t, next.Files[0].Edits, 1)
	assert.Equal(t, "s1", next.Files[0].Edits[0].SessionID)

	_, none, err := ts.recentEdits(t.Context(), nil, recentEditsIn{Path: "nothing-matches"})
	require.NoError(t, err)
	assert.NotNil(t, none.Files)
	assert.Empty(t, none.Files)
}

func TestGetChildSessions_ListsChildrenWithRelationship(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "parent", "proj")
	dbtest.SeedSession(t, d, "child", "proj", func(s *db.Session) {
		s.ParentSessionID = new("parent")
		s.RelationshipType = "subagent"
	})
	srv := newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }})
	st, ct := newInMemoryPair(t, srv)
	defer func() {
		require.NoError(t, ct.Close())
		require.NoError(t, st.Wait())
	}()

	res, err := ct.CallTool(t.Context(), callParams(ToolGetChildSessions, map[string]any{"session_id": "parent"}))
	require.NoError(t, err)
	out := decodeStructured[struct {
		Sessions []struct {
			SessionID    string `json:"session_id"`
			Agent        string `json:"agent"`
			Relationship string `json:"relationship"`
		} `json:"sessions"`
	}](t, res)
	require.Len(t, out.Sessions, 1)
	assert.Equal(t, "child", out.Sessions[0].SessionID)
	assert.Equal(t, "claude", out.Sessions[0].Agent)
	assert.Equal(t, "subagent", out.Sessions[0].Relationship)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetChildSessions, map[string]any{"session_id": "child"}))
	require.NoError(t, err)
	assert.Empty(t, decodeStructured[childSessionsOut](t, res).Sessions)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetChildSessions, map[string]any{"session_id": "missing"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

type pairwiseRecordingService struct {
	service.SessionService
	last service.UsagePairwiseComparisonRequest
}

func (p *pairwiseRecordingService) UsagePairwiseComparison(
	_ context.Context, req service.UsagePairwiseComparisonRequest,
) (*service.UsagePairwiseComparisonResponse, error) {
	p.last = req
	return &service.UsagePairwiseComparisonResponse{}, nil
}

func TestCompareUsage_MapsBothSidesToOneDimension(t *testing.T) {
	t.Parallel()
	rec := &pairwiseRecordingService{}
	ts := &toolset{svc: rec}
	_, _, err := ts.compareUsage(t.Context(), nil, compareUsageIn{
		Dimension: "model", Left: "opus", Right: "sonnet,haiku",
		From: "2024-06-01", To: "2024-06-30", Agent: "claude", Machine: "laptop",
	})
	require.NoError(t, err)
	assert.Equal(t, "model", rec.last.LeftDimension)
	assert.Equal(t, "model", rec.last.RightDimension)
	assert.Equal(t, "opus", rec.last.LeftValue)
	assert.Equal(t, "sonnet,haiku", rec.last.RightValue)
	assert.Equal(t, "2024-06-01", rec.last.From)
	assert.Equal(t, "claude", rec.last.Agent)
	assert.Equal(t, "laptop", rec.last.Machine)
	assert.True(t, rec.last.IncludeOneShot)

	_, _, err = ts.compareUsage(t.Context(), nil, compareUsageIn{Dimension: "agent", Left: "a", Right: "b"})
	require.ErrorContains(t, err, "model or project")
	ts.svc = service.NewDirectBackend(dbtest.OpenTestDB(t), nil)
	for _, sides := range []compareUsageIn{
		{Dimension: "model"},
		{Dimension: "model", Right: "opus"},
		{Dimension: "project", Left: "alpha"},
	} {
		_, _, err = ts.compareUsage(t.Context(), nil, sides)
		if sides.Left == "" {
			require.EqualError(t, err, "left_value is required")
		} else {
			require.EqualError(t, err, "right_value is required")
		}
	}
}

// The comparison's ratios are null when a side is empty; the reply must still
// satisfy the advertised output schema.
func TestServer_CompareUsageOnEmptyArchive(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	srv := newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }})
	st, ct := newInMemoryPair(t, srv)
	defer func() {
		require.NoError(t, ct.Close())
		require.NoError(t, st.Wait())
	}()
	res, err := ct.CallTool(t.Context(), callParams(ToolCompareUsage, map[string]any{
		"dimension": "project", "left": "a", "right": "b",
		"from": "2024-06-01", "to": "2024-06-30",
	}))
	require.NoError(t, err)
	out := decodeStructured[service.UsagePairwiseComparisonResponse](t, res)
	assert.Zero(t, out.Left.SessionCount)
	assert.Nil(t, out.Deltas.TotalCostDeltaRatio)
}

// Current clients negotiate 2026-07-28 over stdio-style transports and the
// newest session-based version over HTTP, and can list and call tools on both.
func TestServer_NegotiatesProtocolPerTransport(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	opts := ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)

	connect := func(t *testing.T, transport func() mcp.Transport) *mcp.ClientSession {
		t.Helper()
		cs, err := client.Connect(t.Context(), transport(), nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = cs.Close() })
		tools, err := cs.ListTools(t.Context(), nil)
		require.NoError(t, err)
		assert.Len(t, tools.Tools, 22)
		res, err := cs.CallTool(t.Context(), callParams(ToolSearchDocs, map[string]any{"query": "mcp", "limit": 1}))
		require.NoError(t, err)
		assert.False(t, res.IsError)
		return cs
	}
	inMemory := func() mcp.Transport {
		st, ct := mcp.NewInMemoryTransports()
		_, err := newServer(opts).Connect(t.Context(), st, nil)
		require.NoError(t, err)
		return ct
	}
	httpServer := httptest.NewServer(newHTTPHandler(opts))
	t.Cleanup(httpServer.Close)
	overHTTP := func() mcp.Transport {
		return &mcp.StreamableClientTransport{Endpoint: httpServer.URL, MaxRetries: -1}
	}

	for _, tc := range []struct {
		name      string
		transport func() mcp.Transport
		want      string
	}{
		{"stdio current", inMemory, "2026-07-28"},
		// HTTP stays stateful, so current clients fall back to the newest
		// session-based version.
		{"http current", overHTTP, "2025-11-25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := connect(t, tc.transport)
			assert.Equal(t, tc.want, cs.InitializeResult().ProtocolVersion)
		})
	}
}

// A multiline key stored inside JSON has its newlines escaped; masking must
// still see the decoded key.
func TestRedactToolInput_MasksEscapedPrivateKey(t *testing.T) {
	t.Parallel()
	key := "-----BEGIN PRIVATE KEY-----\n" +
		"MC4CAQAwBQYDK2VwBCIEIHNhJUCu8VvJCV4O++0jHhjsfn4SwMjf3+3zctpGdZMe\n" +
		"-----END PRIVATE KEY-----"
	raw, err := json.Marshal(map[string]string{"file_path": "id.pem", "content": key})
	require.NoError(t, err)

	got, partial := redactToolInput(string(raw), 10000)
	assert.False(t, partial)
	assert.NotContains(t, got, "MC4CAQAwBQYDK2VwBCIEIHNhJUCu8VvJCV4O")
	assert.Contains(t, got, `"file_path":"id.pem"`)

	dup, _ := redactToolInput(`{"command":"export KEY=AKIA7QHWN2DKR4FYPLJM","command":"echo done"}`, 1000)
	assert.NotContains(t, dup, "KIA7QHWN2DKR4FYPLJM")
	assert.Contains(t, dup, "echo done")

	broken, partial := redactToolInput(`{"command":"export KEY=AKIA7QHWN2DKR4FYPLJM", oops`, 1000)
	assert.True(t, partial)
	assert.NotContains(t, broken, "KIA7QHWN2DKR4FYPLJM")
	assert.Contains(t, broken, `{"command":`)

	long := `{"a":"` + strings.Repeat("x", 20000) + `","b":"AKIA7QHWN2DKR4FYPLJM"}`
	cut, partial := redactToolInput(long, 10)
	assert.True(t, partial, "scanning stops past the bound")
	assert.NotContains(t, cut, "AKIA7QHWN2DKR4FYPLJM")

	pretty := "{\n  \"keep\": \"as stored\"\n}"
	same, partial := redactToolInput(pretty, 1000)
	assert.False(t, partial)
	assert.Equal(t, pretty, same)

	plain, _ := redactToolInput("not json AKIA7QHWN2DKR4FYPLJM", 100)
	assert.NotContains(t, plain, "AKIA7QHWN2DKR4FYPLJM")
	assert.Contains(t, plain, "not json")
}

func TestGetToolCalls_BudgetPages(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "budget-calls", "proj")
	messages := make([]db.Message, 0, 200)
	for i := range 200 {
		messages = append(messages, toolCallMsg("budget-calls", i, db.ToolCall{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"` + strings.Repeat("x", 5000) + `"}`}))
	}
	dbtest.SeedMessages(t, d, messages...)
	in := toolCallsIn{SessionID: "budget-calls", Limit: 200, MaxInputChars: 10000}
	var ordinals []int
	for {
		_, out, err := ts.toolCalls(t.Context(), nil, in)
		require.NoError(t, err)
		assert.True(t, fitsResponse(out))
		for _, call := range out.ToolCalls {
			ordinals = append(ordinals, call.Ordinal)
		}
		if out.NextCursor == nil {
			break
		}
		in.Cursor = *out.NextCursor
	}
	require.Len(t, ordinals, 200)
	for i, ordinal := range ordinals {
		assert.Equal(t, i, ordinal)
	}
}

func TestGetToolCalls_BudgetOversized(t *testing.T) {
	input := `{"command":"` + strings.Repeat("<", 9985) + `z"}`
	for _, tc := range []struct {
		name, toolName, input string
		wantError             bool
	}{
		{name: "input", toolName: "Bash", input: input},
		{name: "metadata", toolName: strings.Repeat("x", 12000), input: `{}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, d := newTestToolset(t)
			dbtest.SeedSession(t, d, "large-input", "proj")
			dbtest.SeedMessages(t, d,
				toolCallMsg("large-input", 0, db.ToolCall{ToolName: tc.toolName, Category: "Bash", InputJSON: tc.input}),
				toolCallMsg("large-input", 1, db.ToolCall{ToolName: "Bash", Category: "Bash", InputJSON: `{"command":"next"}`}),
			)
			_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "large-input", MaxInputChars: 10000})
			if tc.wantError {
				require.ErrorContains(t, err, "tool call metadata exceeds response budget")
				return
			}
			require.NoError(t, err)
			require.Len(t, out.ToolCalls, 1)
			assert.True(t, fitsResponse(out))
			assert.True(t, out.ToolCalls[0].InputTruncated)
			require.NotNil(t, out.ToolCalls[0].Input)
			assert.True(t, strings.HasPrefix(input, *out.ToolCalls[0].Input))
			require.NotNil(t, out.NextCursor)
			assert.Equal(t, 1, *out.NextCursor)
		})
	}
}

func TestGetToolCalls_BudgetLongInputWithShortEvents(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "short-events", "proj")
	events := make([]db.ToolResultEvent, 68)
	for i := range events {
		events[i] = db.ToolResultEvent{EventIndex: i, Source: "tool_result", Content: strings.Repeat("x", 100)}
	}
	dbtest.SeedMessages(t, d, toolCallMsg("short-events", 0, db.ToolCall{
		ToolUseID: "call", ToolName: "wait_agent", InputJSON: strings.Repeat("x", 10000),
		ResultContent: "summary", ResultEvents: events,
	}))
	_, smallInput, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "short-events", IncludeResults: true, MaxInputChars: 1})
	require.NoError(t, err)
	assert.True(t, fitsResponse(smallInput))
	require.Len(t, smallInput.ToolCalls, 1)
	require.Len(t, smallInput.ToolCalls[0].ResultEvents, 68)
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "short-events", IncludeResults: true, MaxInputChars: 10000})
	require.NoError(t, err)
	assert.True(t, fitsResponse(out))
	require.Len(t, out.ToolCalls, 1)
	require.Len(t, out.ToolCalls[0].ResultEvents, 68)
	for _, event := range out.ToolCalls[0].ResultEvents {
		assert.Equal(t, strings.Repeat("x", 100), event.Content)
	}
}

func TestGetToolCalls_BudgetLongResultWithShortEvents(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "long-result-events", "proj")
	events := make([]db.ToolResultEvent, 81)
	for i := range events {
		events[i] = db.ToolResultEvent{EventIndex: i, Source: "tool_result", Content: strings.Repeat("x", 53)}
	}
	dbtest.SeedMessages(t, d, toolCallMsg("long-result-events", 0, db.ToolCall{
		ToolUseID: "call", ToolName: "wait_agent", InputJSON: strings.Repeat("x", 3000),
		ResultContent: strings.Repeat("y", 3000), ResultEvents: events,
	}))
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long-result-events", IncludeResults: true})
	require.NoError(t, err)
	assert.True(t, fitsResponse(out))
	require.Len(t, out.ToolCalls, 1)
	require.Len(t, out.ToolCalls[0].ResultEvents, 81)
	for _, event := range out.ToolCalls[0].ResultEvents {
		assert.Equal(t, strings.Repeat("x", 53), event.Content)
	}
	assert.NotEmpty(t, out.ToolCalls[0].Result)
	assert.NotEmpty(t, out.ToolCalls[0].ResultCursor)
}

func TestGetToolCalls_IncludesResults(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "results", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("results", 0,
			db.ToolCall{ToolUseID: "one", ToolName: "Read", Category: "Read", ResultContent: "first result"},
			db.ToolCall{ToolName: "Bash", Category: "Bash", ResultContent: "second result"},
		),
		toolCallMsg("results", 1000, db.ToolCall{ToolUseID: "three", ToolName: "Bash", Category: "Bash", ResultContent: "AKIA7QHWN2DKR4FYPLJM ending"}),
	)
	recording := &recordingMessageService{SessionService: ts.svc}
	ts.svc = recording
	_, first, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "results", Category: "Bash", Limit: 1, IncludeResults: true, MaxResultChars: 6})
	require.NoError(t, err)
	require.Len(t, first.ToolCalls, 1)
	assert.Equal(t, "second", first.ToolCalls[0].Result)
	assert.True(t, first.ToolCalls[0].ResultTruncated)
	require.NotNil(t, first.NextCursor)
	require.Len(t, recording.filters, 1)
	require.NotNil(t, recording.filters[0].Around)
	assert.Equal(t, 0, *recording.filters[0].Around)
	recording.filters = nil
	_, second, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "results", Category: "Bash", Cursor: *first.NextCursor, IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, second.ToolCalls, 1)
	require.Len(t, recording.filters, 1)
	require.NotNil(t, recording.filters[0].Around)
	assert.Equal(t, 1000, *recording.filters[0].Around)
	assert.NotContains(t, second.ToolCalls[0].Result, "AKIA7QHWN2DKR4FYPLJM")
	assert.Contains(t, second.ToolCalls[0].Result, "ending")
	_, omitted, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "results"})
	require.NoError(t, err)
	encoded, err := json.Marshal(omitted)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), `"result":`)
}

type changingCallsService struct {
	service.SessionService
	d *db.DB
}

func (s changingCallsService) ToolCalls(ctx context.Context, id string) (*service.ToolCallList, error) {
	calls, err := s.SessionService.ToolCalls(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.d.ReplaceSessionMessages(ctx, id, []db.Message{toolCallMsg(id, 0, db.ToolCall{ToolUseID: "new", ToolName: "Bash", Category: "Bash", ResultContent: "new output"})}); err != nil {
		return nil, err
	}
	return calls, nil
}

type appendAfterToolCallsService struct {
	service.SessionService
	t *testing.T
	d *db.DB
}

func (s appendAfterToolCallsService) ToolCalls(ctx context.Context, id string) (*service.ToolCallList, error) {
	calls, err := s.SessionService.ToolCalls(ctx, id)
	if err == nil {
		dbtest.SeedMessages(s.t, s.d, dbtest.AsstMsg(id, 1, "later message"))
	}
	return calls, err
}

func TestGetToolCalls_FirstReadSurvivesAppend(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "growing", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("growing", 0, db.ToolCall{ToolUseID: "one", ToolName: "Bash", ResultContent: "saved result"}))
	ts.svc = appendAfterToolCallsService{SessionService: ts.svc, t: t, d: d}
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "growing", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 1)
	assert.Equal(t, "saved result", out.ToolCalls[0].Result)
}

func TestGetToolCalls_ContinuesPlainTextResult(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "long", "proj")
	call := toolCallMsg("long", 0, db.ToolCall{ToolUseID: "call", ToolName: "Bash", Category: "Bash", ResultContent: "200 OK café AKIA7QHWN2DKR4FYPLJM result"})
	dbtest.SeedMessages(t, d, call)
	_, page, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long", IncludeResults: true, MaxResultChars: 5})
	require.NoError(t, err)
	require.Len(t, page.ToolCalls, 1)
	var result strings.Builder
	result.WriteString(page.ToolCalls[0].Result)
	cursor := page.ToolCalls[0].ResultCursor
	require.NotEmpty(t, cursor)
	originalCursor := cursor
	dbtest.SeedMessages(t, d, dbtest.AsstMsg("long", 1, "later message"))
	for cursor != "" {
		_, page, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long", ResultCursor: cursor, MaxResultChars: 5})
		require.NoError(t, err)
		require.Len(t, page.ToolCalls, 1)
		raw, err := json.Marshal(page)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), `"input"`)
		assert.NotContains(t, string(raw), `"category"`)
		assert.NotContains(t, string(raw), `"total"`)
		result.WriteString(page.ToolCalls[0].Result)
		cursor = page.ToolCalls[0].ResultCursor
	}
	assert.Equal(t, "200 OK café AKIA…PLJM result", result.String())
	call.ToolCalls[0].ResultContent = "200 OK café AKIA7QHWN2DKR4FYPLJM OUTPUT"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "long", []db.Message{call, dbtest.AsstMsg("long", 1, "later message")}))
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long", ResultCursor: originalCursor})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

type recordingMessageService struct {
	service.SessionService
	filters []service.MessageFilter
}

func (s *recordingMessageService) Messages(ctx context.Context, id string, filter service.MessageFilter) (*service.MessageList, error) {
	s.filters = append(s.filters, filter)
	return s.SessionService.Messages(ctx, id, filter)
}

type changingMessagesService struct {
	service.SessionService
	d     *db.DB
	reads int
}

func (s *changingMessagesService) Messages(ctx context.Context, id string, filter service.MessageFilter) (*service.MessageList, error) {
	page, err := s.SessionService.Messages(ctx, id, filter)
	if err != nil {
		return nil, err
	}
	s.reads++
	if s.reads == 1 {
		err = s.d.ReplaceSessionMessages(ctx, id, []db.Message{toolCallMsg(id, 0, db.ToolCall{ToolUseID: "new", ToolName: "Bash", ResultContent: "replacement"})})
	}
	return page, err
}

func TestGetToolCalls_RejectsVanishedSelectedMessage(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "walk", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("walk", 0, db.ToolCall{ToolUseID: "one", ToolName: "Bash", ResultContent: "first"}),
		toolCallMsg("walk", 1000, db.ToolCall{ToolUseID: "two", ToolName: "Bash", ResultContent: "second"}),
	)
	ts.svc = &changingMessagesService{SessionService: ts.svc, d: d}
	_, _, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "walk", IncludeResults: true, Limit: 2})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

type mismatchedCallsService struct{ service.SessionService }

func (s mismatchedCallsService) ToolCalls(ctx context.Context, id string) (*service.ToolCallList, error) {
	calls, err := s.SessionService.ToolCalls(ctx, id)
	if err == nil && len(calls.ToolCalls) > 0 {
		calls.ToolCalls[0].ToolName = "different"
	}
	return calls, err
}

func TestGetToolCalls_RejectsMismatchedCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(service.SessionService, *db.DB) service.SessionService
	}{
		{"changed call ID", func(s service.SessionService, d *db.DB) service.SessionService {
			return changingCallsService{SessionService: s, d: d}
		}},
		{"changed tool name", func(s service.SessionService, _ *db.DB) service.SessionService {
			return mismatchedCallsService{SessionService: s}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, d := newTestToolset(t)
			dbtest.SeedSession(t, d, "mismatch", "proj")
			dbtest.SeedMessages(t, d, toolCallMsg("mismatch", 0, db.ToolCall{ToolUseID: "old", ToolName: "Bash", Category: "Bash", ResultContent: "old output"}))
			ts.svc = tc.wrap(ts.svc, d)
			_, _, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "mismatch", IncludeResults: true})
			require.ErrorIs(t, err, service.ErrSourceChanged)
		})
	}
}

func TestGetToolCalls_RetainsLengthWithoutResultText(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "withheld", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("withheld", 0, db.ToolCall{ToolUseID: "one", ToolName: "Bash", ResultContentLength: 17}))
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "withheld", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 1)
	assert.Empty(t, out.ToolCalls[0].Result)
	assert.Equal(t, 17, out.ToolCalls[0].ResultLength)
}

func TestGetToolCalls_ReturnsStoredSummary(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "summary", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("summary", 0, db.ToolCall{ToolUseID: "single", ToolName: "wait_agent", ResultContent: "latest B", ResultEvents: []db.ToolResultEvent{
			{AgentID: "agent-a", Source: "tool_result", Content: "earlier A"},
			{AgentID: "agent-a", Source: "tool_result", Content: "latest B"},
		}}),
		toolCallMsg("summary", 1, db.ToolCall{ToolUseID: "multiple", ToolName: "wait_agent", ResultContent: "agent-a:\nfirst\n\nagent-b:\nsecond", ResultEvents: []db.ToolResultEvent{
			{AgentID: "agent-a", Source: "tool_result", Content: "first"},
			{AgentID: "agent-b", Source: "tool_result", Content: "second"},
			{AgentID: "agent-b", Source: "tool_result", Status: "completed"},
		}}),
		toolCallMsg("summary", 2, db.ToolCall{ToolUseID: "anonymous", ToolName: "wait_agent", ResultContent: "output", ResultEvents: []db.ToolResultEvent{
			{Source: "tool_result", Content: "output"},
			{AgentID: "agent-c", Source: "tool_result", Status: "completed"},
		}}),
	)
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "summary", IncludeResults: true, Limit: 3})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 3)
	assert.Equal(t, "latest B", out.ToolCalls[0].Result)
	require.Len(t, out.ToolCalls[0].ResultEvents, 2)
	assert.Equal(t, "agent-a", out.ToolCalls[0].ResultEvents[0].AgentID)
	assert.Equal(t, "earlier A", out.ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "latest B", out.ToolCalls[0].ResultEvents[1].Content)
	assert.Equal(t, "agent-a:\nfirst\n\nagent-b:\nsecond", out.ToolCalls[1].Result)
	require.Len(t, out.ToolCalls[1].ResultEvents, 3)
	assert.Equal(t, "agent-b", out.ToolCalls[1].ResultEvents[2].AgentID)
	assert.Equal(t, "completed", out.ToolCalls[1].ResultEvents[2].Status)
	require.Len(t, out.ToolCalls[2].ResultEvents, 2)
	assert.Equal(t, "agent-c", out.ToolCalls[2].ResultEvents[1].AgentID)
	assert.Equal(t, "completed", out.ToolCalls[2].ResultEvents[1].Status)
}

func TestGetToolCalls_EventCursorRejectsReplacement(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "event-cursor", "proj")
	call := toolCallMsg("event-cursor", 0, db.ToolCall{ToolUseID: "one", ToolName: "wait_agent", ResultContent: "summary", ResultEvents: []db.ToolResultEvent{
		{AgentID: "agent-a", Source: "tool_result", Content: "first"},
		{AgentID: "agent-b", SubagentSessionID: "child", Status: "completed", Source: "tool_result", Content: "abcdefghij"},
	}})
	dbtest.SeedMessages(t, d, call)
	_, first, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "event-cursor", IncludeResults: true, MaxResultChars: 4})
	require.NoError(t, err)
	require.Len(t, first.ToolCalls, 1)
	type event struct {
		AgentID           string `json:"agent_id"`
		SubagentSessionID string `json:"subagent_session_id"`
		Status            string `json:"status"`
		EventIndex        int    `json:"event_index"`
		Content           string `json:"content"`
		ContentLength     int    `json:"content_length"`
		Cursor            string `json:"result_cursor"`
	}
	var firstCall struct {
		Events []event `json:"result_events"`
	}
	raw, err := json.Marshal(first.ToolCalls[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &firstCall))
	require.Len(t, firstCall.Events, 2)
	assert.Equal(t, "agent-b", firstCall.Events[1].AgentID)
	assert.Equal(t, 1, firstCall.Events[1].EventIndex)
	assert.Equal(t, "abcd", firstCall.Events[1].Content)
	cursor := firstCall.Events[1].Cursor
	require.NotEmpty(t, cursor)
	_, next, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "event-cursor", ResultCursor: cursor, MaxResultChars: 100})
	require.NoError(t, err)
	require.Len(t, next.ToolCalls, 1)
	raw, err = json.Marshal(next.ToolCalls[0])
	require.NoError(t, err)
	var nextCall struct {
		Events []event `json:"result_events"`
	}
	require.NoError(t, json.Unmarshal(raw, &nextCall))
	require.Len(t, nextCall.Events, 1)
	assert.Equal(t, "agent-b", nextCall.Events[0].AgentID)
	assert.Equal(t, "child", nextCall.Events[0].SubagentSessionID)
	assert.Equal(t, "completed", nextCall.Events[0].Status)
	assert.Equal(t, 1, nextCall.Events[0].EventIndex)
	assert.Equal(t, "efghij", nextCall.Events[0].Content)
	assert.Equal(t, 10, nextCall.Events[0].ContentLength)
	call.ToolCalls[0].ResultEvents[1].Content = "ABCDEFGHIJ"
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "event-cursor", []db.Message{call}))
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "event-cursor", ResultCursor: cursor})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestGetToolCalls_DefaultResultPageIsSmall(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "many", "proj")
	for i := range 50 {
		dbtest.SeedMessages(t, d, toolCallMsg("many", i, db.ToolCall{ToolUseID: strconv.Itoa(i), ToolName: "Bash", ResultContent: "result " + strconv.Itoa(i)}))
	}
	_, first, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "many", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, first.ToolCalls, 1)
	require.NotNil(t, first.NextCursor)
	_, next, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "many", IncludeResults: true, Cursor: *first.NextCursor})
	require.NoError(t, err)
	require.Len(t, next.ToolCalls, 1)
	assert.Equal(t, "result 1", next.ToolCalls[0].Result)
}

func TestGetToolCalls_CursorsAreDistinct(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "cursor", "proj")
	m := toolCallMsg("cursor", 0, db.ToolCall{ToolUseID: "one", ToolName: "Bash", ResultContent: "long result"})
	m.Content = "long message"
	dbtest.SeedMessages(t, d, m)
	_, calls, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "cursor", IncludeResults: true, MaxResultChars: 3})
	require.NoError(t, err)
	require.Len(t, calls.ToolCalls, 1)
	require.NotEmpty(t, calls.ToolCalls[0].ResultCursor)
	_, _, err = ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "cursor", BodyCursor: calls.ToolCalls[0].ResultCursor})
	require.ErrorContains(t, err, "invalid body_cursor")
	_, messages, err := ts.getMessages(t.Context(), nil, getMessagesIn{SessionID: "cursor", MaxCharsPerMessage: 3})
	require.NoError(t, err)
	require.Len(t, messages.Messages, 1)
	require.NotEmpty(t, messages.Messages[0].BodyCursor)
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "cursor", ResultCursor: messages.Messages[0].BodyCursor})
	require.ErrorContains(t, err, "invalid result_cursor")
	raw, err := base64.RawURLEncoding.DecodeString(calls.ToolCalls[0].ResultCursor)
	require.NoError(t, err)
	var cursor map[string]any
	require.NoError(t, json.Unmarshal(raw, &cursor))
	cursor["v"] = 999
	raw, err = json.Marshal(cursor)
	require.NoError(t, err)
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "cursor", ResultCursor: base64.RawURLEncoding.EncodeToString(raw)})
	require.ErrorContains(t, err, "invalid result_cursor")
}

func TestGetToolCalls_HTTPContinuesStoredResult(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "remote", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("remote", 0, db.ToolCall{ToolUseID: "call", ToolName: "Bash", ResultContent: "stored result"}))
	httpServer := httptest.NewUnstartedServer(nil)
	srv := server.New(config.Config{Host: "127.0.0.1", Port: httpServer.Listener.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir(), WriteTimeout: 30 * time.Second}, d, nil)
	httpServer.Config.Handler = srv.Handler()
	httpServer.Start()
	t.Cleanup(httpServer.Close)
	ss, client := newInMemoryPair(t, newServer(ServeOptions{Service: servicehttp.NewHTTPBackend(httpServer.URL, "", false, "")}))
	defer func() { require.NoError(t, client.Close()); require.NoError(t, ss.Wait()) }()
	res, err := client.CallTool(t.Context(), callParams(ToolGetToolCalls, map[string]any{"session_id": "remote", "include_results": true, "max_result_chars": 4}))
	require.NoError(t, err)
	calls := decodeStructured[toolCallsOut](t, res)
	require.Len(t, calls.ToolCalls, 1)
	assert.Equal(t, "stor", calls.ToolCalls[0].Result)
	cursor := calls.ToolCalls[0].ResultCursor
	require.NotEmpty(t, cursor)
	res, err = client.CallTool(t.Context(), callParams(ToolGetToolCalls, map[string]any{"session_id": "remote", "result_cursor": cursor, "max_result_chars": 4}))
	require.NoError(t, err)
	calls = decodeStructured[toolCallsOut](t, res)
	require.Len(t, calls.ToolCalls, 1)
	assert.Equal(t, "ed r", calls.ToolCalls[0].Result)
	assert.NotEmpty(t, calls.ToolCalls[0].ResultCursor)
}

// seedLongToolResult stores one call whose summary or sole saved event holds
// text, and returns a reader for that text from a page of get_tool_calls.
func seedLongToolResult(t *testing.T, d *db.DB, sid, field, text string) func(toolCallOut) (string, string) {
	t.Helper()
	dbtest.SeedSession(t, d, sid, "proj")
	call := db.ToolCall{ToolUseID: "call", ToolName: "wait_agent", ResultContent: "short"}
	if field == "summary" {
		call.ResultContent = text
	} else {
		call.ResultEvents = []db.ToolResultEvent{{AgentID: "agent-a", Source: "tool_result", Content: text}}
	}
	dbtest.SeedMessages(t, d, toolCallMsg(sid, 0, call))
	return func(c toolCallOut) (string, string) {
		if field == "summary" {
			return c.Result, c.ResultCursor
		}
		require.Len(t, c.ResultEvents, 1)
		return c.ResultEvents[0].Content, c.ResultEvents[0].ResultCursor
	}
}

func TestGetToolCalls_ResumedCursorFitsLargerPages(t *testing.T) {
	for _, field := range []string{"summary", "event"} {
		t.Run(field, func(t *testing.T) {
			ts, d := newTestToolset(t)
			text := strings.Repeat("가", 12000)
			read := seedLongToolResult(t, d, "resume", field, text)
			_, page, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "resume", IncludeResults: true, MaxResultChars: 10000})
			require.NoError(t, err)
			require.Len(t, page.ToolCalls, 1)
			assert.True(t, fitsResponse(page))
			got, cursor := read(page.ToolCalls[0])
			var recovered strings.Builder
			recovered.WriteString(got)
			for range len(text) {
				if cursor == "" {
					break
				}
				_, page, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "resume", ResultCursor: cursor, MaxResultChars: 10000})
				require.NoError(t, err)
				require.Len(t, page.ToolCalls, 1)
				assert.True(t, fitsResponse(page))
				got, cursor = read(page.ToolCalls[0])
				require.NotEmpty(t, got)
				recovered.WriteString(got)
			}
			assert.Empty(t, cursor)
			assert.Equal(t, text, recovered.String())
		})
	}
}
