package mcp

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
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

// Older clients still complete the initialize handshake on stdio-style and
// HTTP transports; current clients negotiate 2026-07-28 where the transport
// supports it.
func TestServer_NegotiatesCurrentAndOlderProtocols(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	opts := ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)

	connect := func(t *testing.T, transport func() mcp.Transport, version string) *mcp.ClientSession {
		t.Helper()
		cs, err := client.Connect(t.Context(), transport(), &mcp.ClientSessionOptions{ProtocolVersion: version})
		require.NoError(t, err)
		t.Cleanup(func() { _ = cs.Close() })
		tools, err := cs.ListTools(t.Context(), nil)
		require.NoError(t, err)
		assert.Len(t, tools.Tools, 16)
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
		request   string
		want      string
	}{
		{"stdio current", inMemory, "", "2026-07-28"},
		{"stdio 2025-06-18", inMemory, "2025-06-18", "2025-06-18"},
		{"stdio 2024-11-05", inMemory, "2024-11-05", "2024-11-05"},
		// HTTP stays stateful, so current clients fall back to the newest
		// session-based version.
		{"http current", overHTTP, "", "2025-11-25"},
		{"http 2025-03-26", overHTTP, "2025-03-26", "2025-03-26"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := connect(t, tc.transport, tc.request)
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
