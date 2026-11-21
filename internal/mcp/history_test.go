package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net"
	"net/http/httptest"
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

	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "s1", Category: "bash", Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 2, out.Total)
	require.Len(t, out.ToolCalls, 1)
	assert.Equal(t, "Bash", out.ToolCalls[0].ToolName)
	assert.NotContains(t, out.ToolCalls[0].Input, "AKIA7QHWN2DKR4FYPLJM")
	assert.Contains(t, out.ToolCalls[0].Input, "make")
	require.NotNil(t, out.NextCursor)

	_, next, err := ts.toolCalls(t.Context(), nil, toolCallsIn{
		SessionID: "s1", Category: "Bash", Limit: 1, Cursor: *out.NextCursor, MaxInputChars: 5,
	})
	require.NoError(t, err)
	require.Len(t, next.ToolCalls, 1)
	assert.Equal(t, 1, next.ToolCalls[0].Ordinal)
	assert.Equal(t, `{"com`, next.ToolCalls[0].Input)
	assert.True(t, next.ToolCalls[0].InputTruncated)
	assert.Nil(t, next.NextCursor)

	_, all, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "s1"})
	require.NoError(t, err)
	require.Len(t, all.ToolCalls, 3)
	assert.Equal(t, 12, all.ToolCalls[1].ResultLength)

	_, none, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "empty"})
	require.NoError(t, err)
	assert.Empty(t, none.ToolCalls)
	_, none, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "empty", IncludeResults: true})
	require.NoError(t, err)
	assert.Empty(t, none.ToolCalls)

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

func TestGetToolCalls_IncludesResults(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "results", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("results", 0,
			db.ToolCall{ToolUseID: "one", ToolName: "Read", Category: "Read", ResultContent: "first result"},
			db.ToolCall{ToolName: "Bash", Category: "Bash", ResultContent: "second result"},
		),
		toolCallMsg("results", 1, db.ToolCall{ToolUseID: "three", ToolName: "Bash", Category: "Bash", ResultContent: "AKIA7QHWN2DKR4FYPLJM ending"}),
	)
	_, first, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "results", Category: "Bash", Limit: 1, IncludeResults: true, MaxResultChars: 6})
	require.NoError(t, err)
	require.Len(t, first.ToolCalls, 1)
	assert.Equal(t, "second", first.ToolCalls[0].Result)
	assert.True(t, first.ToolCalls[0].ResultTruncated)
	require.NotNil(t, first.NextCursor)
	_, second, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "results", Category: "Bash", Cursor: *first.NextCursor, IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, second.ToolCalls, 1)
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

func TestGetToolCalls_RejectsChangedTranscript(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "changing", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("changing", 0, db.ToolCall{ToolUseID: "old", ToolName: "Bash", Category: "Bash", ResultContent: "old output"}))
	ts.svc = changingCallsService{SessionService: ts.svc, d: d}
	_, _, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "changing", IncludeResults: true})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestGetToolCalls_ContinuesPlainTextResult(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "long", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("long", 0, db.ToolCall{ToolUseID: "call", ToolName: "Bash", Category: "Bash", ResultContent: "200 OK café result"}))
	_, page, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long", IncludeResults: true, MaxResultChars: 5})
	require.NoError(t, err)
	require.Len(t, page.ToolCalls, 1)
	result := page.ToolCalls[0].Result
	cursor := page.ToolCalls[0].ResultCursor
	require.NotEmpty(t, cursor)
	originalCursor := cursor
	for cursor != "" {
		_, page, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long", ResultCursor: cursor, MaxResultChars: 5})
		require.NoError(t, err)
		require.Len(t, page.ToolCalls, 1)
		result += page.ToolCalls[0].Result
		cursor = page.ToolCalls[0].ResultCursor
	}
	assert.Equal(t, "200 OK café result", result)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "long", []db.Message{toolCallMsg("long", 0, db.ToolCall{ToolUseID: "other", ToolName: "Bash", ResultContent: "replacement"})}))
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "long", ResultCursor: originalCursor})
	require.ErrorIs(t, err, service.ErrSourceChanged)
}

func TestGetToolCalls_PreservesPlainResultText(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "plain", "proj")
	texts := []string{"3 files changed in the working tree", "true story follows", "[INFO] all tests passed", "42"}
	for i, text := range texts {
		dbtest.SeedMessages(t, d, toolCallMsg("plain", i, db.ToolCall{ToolUseID: fmt.Sprintf("call-%d", i), ToolName: "Bash", Category: "Bash", ResultContent: text}))
	}
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "plain", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, len(texts))
	for i, text := range texts {
		assert.Equal(t, text, out.ToolCalls[i].Result)
	}
}

type recordingMessageService struct {
	service.SessionService
	filters []service.MessageFilter
}

func (s *recordingMessageService) Messages(ctx context.Context, id string, filter service.MessageFilter) (*service.MessageList, error) {
	s.filters = append(s.filters, filter)
	return s.SessionService.Messages(ctx, id, filter)
}

func TestGetToolCalls_EventResultsAndSelectedOrdinals(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "events", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("events", 0,
			db.ToolCall{ToolUseID: "read", ToolName: "Read", Category: "Read", ResultContent: "unselected"},
			db.ToolCall{ToolUseID: "bash", ToolName: "Bash", Category: "Bash", ResultEvents: []db.ToolResultEvent{
				{Source: "tool_result", Content: "first result"},
				{Source: "tool_result", Content: "second result"},
			}},
		),
		toolCallMsg("events", 1000, db.ToolCall{ToolUseID: "later", ToolName: "Bash", Category: "Bash", ResultContent: "later result"}),
	)
	recording := &recordingMessageService{SessionService: ts.svc}
	ts.svc = recording
	_, first, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "events", IncludeResults: true, Category: "Bash", Limit: 1, MaxResultChars: 6})
	require.NoError(t, err)
	require.Len(t, first.ToolCalls, 1)
	assert.Equal(t, "first ", first.ToolCalls[0].Result)
	assert.Equal(t, len("first result\n\nsecond result"), first.ToolCalls[0].ResultLength)
	require.Len(t, recording.filters, 1)
	require.NotNil(t, recording.filters[0].Around)
	assert.Equal(t, 0, *recording.filters[0].Around)
	cursor := first.ToolCalls[0].ResultCursor
	require.NotEmpty(t, cursor)
	decoded, err := decodeMessageBodyCursor(cursor)
	require.NoError(t, err)
	assert.Equal(t, 3, decoded.Version)
	decoded.Version = 2
	_, _, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "events", ResultCursor: encodeMessageBodyCursor(decoded)})
	require.ErrorContains(t, err, "invalid result_cursor")
	full := first.ToolCalls[0].Result
	for cursor != "" {
		_, next, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "events", ResultCursor: cursor, MaxResultChars: 6})
		require.NoError(t, err)
		require.Len(t, next.ToolCalls, 1)
		full += next.ToolCalls[0].Result
		cursor = next.ToolCalls[0].ResultCursor
	}
	assert.Equal(t, "first result\n\nsecond result", full)
	recording.filters = nil
	require.NotNil(t, first.NextCursor)
	_, next, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "events", IncludeResults: true, Category: "Bash", Limit: 1, Cursor: *first.NextCursor})
	require.NoError(t, err)
	require.Len(t, next.ToolCalls, 1)
	assert.Equal(t, "later result", next.ToolCalls[0].Result)
	require.Len(t, recording.filters, 1)
	require.NotNil(t, recording.filters[0].Around)
	assert.Equal(t, 1000, *recording.filters[0].Around)
}

func TestGetToolCalls_MasksEveryResultEvent(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "events-secret", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("events-secret", 0, db.ToolCall{ToolUseID: "call", ToolName: "Bash", ResultEvents: []db.ToolResultEvent{
		{Source: "tool_result", Content: "first"},
		{Source: "tool_result", Content: `{"key":"AKIA7QHWN2DKR4FYPLJM"}`},
	}}))
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "events-secret", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 1)
	assert.Contains(t, out.ToolCalls[0].Result, "first")
	assert.NotContains(t, out.ToolCalls[0].Result, "AKIA7QHWN2DKR4FYPLJM")
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

func TestGetToolCalls_RejectsChangeBetweenSelectedMessages(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "walk", "proj")
	dbtest.SeedMessages(t, d,
		toolCallMsg("walk", 0, db.ToolCall{ToolUseID: "one", ToolName: "Bash", ResultContent: "first"}),
		toolCallMsg("walk", 1000, db.ToolCall{ToolUseID: "two", ToolName: "Bash", ResultContent: "second"}),
	)
	ts.svc = &changingMessagesService{SessionService: ts.svc, d: d}
	_, _, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "walk", IncludeResults: true})
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
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "mismatch", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("mismatch", 0, db.ToolCall{ToolUseID: "one", ToolName: "Bash", ResultContent: "output"}))
	ts.svc = mismatchedCallsService{SessionService: ts.svc}
	_, _, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "mismatch", IncludeResults: true})
	require.ErrorIs(t, err, service.ErrSourceChanged)
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

type redactedMessagesService struct{ service.SessionService }

func (s redactedMessagesService) Messages(ctx context.Context, id string, filter service.MessageFilter) (*service.MessageList, error) {
	page, err := s.SessionService.Messages(ctx, id, filter)
	if err != nil {
		return nil, err
	}
	for i, message := range page.Messages {
		page.Messages[i] = service.RedactMessageSecrets(message)
	}
	return page, nil
}

func TestGetToolCalls_RedactedMessagesKeepIdentityAndLength(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "masked", "proj")
	first := "first output AKIA7QHWN2DKR4FYPLJM"
	second := "second output AKIA7QHWN2DKR4FYPLJM"
	dbtest.SeedMessages(t, d, toolCallMsg("masked", 0, db.ToolCall{
		ToolUseID: "one", ToolName: "Bash", Category: "Bash", InputJSON: `{"key":"AKIA7QHWN2DKR4FYPLJM"}`,
		ResultEvents: []db.ToolResultEvent{{AgentID: "agent-a", Source: "tool_result", Content: first}, {Source: "tool_result", Content: second}},
	}))
	ts.svc = redactedMessagesService{SessionService: ts.svc}
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "masked", IncludeResults: true, MaxResultChars: 4})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 1)
	assert.Equal(t, len("agent-a:\n")+len(first)+2+len(second), out.ToolCalls[0].ResultLength)
	require.NotEmpty(t, out.ToolCalls[0].ResultCursor)
	_, continued, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "masked", ResultCursor: out.ToolCalls[0].ResultCursor})
	require.NoError(t, err)
	require.Len(t, continued.ToolCalls, 1)
	assert.Equal(t, len("agent-a:\n")+len(first)+2+len(second), continued.ToolCalls[0].ResultLength)
	assert.NotContains(t, out.ToolCalls[0].Input, "AKIA7QHWN2DKR4FYPLJM")
	assert.NotContains(t, continued.ToolCalls[0].Result, "AKIA7QHWN2DKR4FYPLJM")

	summary := "summary AKIA7QHWN2DKR4FYPLJM"
	dbtest.SeedSession(t, d, "masked-summary", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("masked-summary", 0, db.ToolCall{ToolUseID: "two", ToolName: "Bash", ResultContent: summary}))
	_, out, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "masked-summary", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 1)
	assert.Equal(t, len(summary), out.ToolCalls[0].ResultLength)
	assert.NotContains(t, out.ToolCalls[0].Result, "AKIA7QHWN2DKR4FYPLJM")
}

func TestGetToolCalls_AttributesRetainedResults(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "attribution", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("attribution", 0, db.ToolCall{ToolUseID: "call", ToolName: "wait_agent", Category: "Task", ResultEvents: []db.ToolResultEvent{
		{AgentID: "agent-a", Source: "tool_result", Content: "First finished"},
		{AgentID: "agent-b", Source: "tool_result", Content: "Second finished"},
	}}))
	_, out, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "attribution", IncludeResults: true})
	require.NoError(t, err)
	require.Len(t, out.ToolCalls, 1)
	assert.Equal(t, "agent-a:\nFirst finished\n\nagent-b:\nSecond finished", out.ToolCalls[0].Result)
}

func TestGetToolCalls_ContinuesAttributedResult(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "mixed-agents", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("mixed-agents", 0, db.ToolCall{ToolUseID: "call", ToolName: "wait_agent", ResultEvents: []db.ToolResultEvent{
		{AgentID: "agent-a", Source: "tool_result", Content: "first"},
		{Source: "tool_result", Content: "anonymous"},
		{AgentID: "agent-a", Source: "tool_result", Content: "again"},
		{AgentID: "agent-b", Source: "tool_result", Content: "finished"},
	}}))
	_, page, err := ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "mixed-agents", IncludeResults: true, MaxResultChars: 5})
	require.NoError(t, err)
	require.Len(t, page.ToolCalls, 1)
	want := "agent-a:\nfirst\n\nanonymous\n\nagent-a:\nagain\n\nagent-b:\nfinished"
	assert.Equal(t, len(want), page.ToolCalls[0].ResultLength)
	text, cursor := page.ToolCalls[0].Result, page.ToolCalls[0].ResultCursor
	require.NotEmpty(t, cursor)
	for cursor != "" {
		_, page, err = ts.toolCalls(t.Context(), nil, toolCallsIn{SessionID: "mixed-agents", ResultCursor: cursor, MaxResultChars: 5})
		require.NoError(t, err)
		require.Len(t, page.ToolCalls, 1)
		assert.Equal(t, len(want), page.ToolCalls[0].ResultLength)
		text += page.ToolCalls[0].Result
		cursor = page.ToolCalls[0].ResultCursor
	}
	assert.Equal(t, want, text)
}

func TestGetToolCalls_HTTPContinuesAttributedResults(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "remote", "proj")
	dbtest.SeedMessages(t, d, toolCallMsg("remote", 0, db.ToolCall{ToolUseID: "call", ToolName: "wait_agent", ResultEvents: []db.ToolResultEvent{
		{AgentID: "agent-a", Source: "tool_result", Content: "first"},
		{AgentID: "agent-b", Source: "tool_result", Content: "second"},
	}}))
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
	text, cursor := calls.ToolCalls[0].Result, calls.ToolCalls[0].ResultCursor
	require.NotEmpty(t, cursor)
	for cursor != "" {
		res, err = client.CallTool(t.Context(), callParams(ToolGetToolCalls, map[string]any{"session_id": "remote", "result_cursor": cursor, "max_result_chars": 4}))
		require.NoError(t, err)
		calls = decodeStructured[toolCallsOut](t, res)
		require.Len(t, calls.ToolCalls, 1)
		text += calls.ToolCalls[0].Result
		cursor = calls.ToolCalls[0].ResultCursor
	}
	assert.Equal(t, "agent-a:\nfirst\n\nagent-b:\nsecond", text)
}
