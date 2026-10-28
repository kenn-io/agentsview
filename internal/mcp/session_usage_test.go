package mcp

import (
	"encoding/json/jsontext"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

// Usage fixture. test-opus costs $2/Mtok input, $10/Mtok output, $2.50/Mtok
// cache writes, and $0.20/Mtok cache reads; zz-unpriced-model has no price.
//
//	parent row 1 test-opus:         1000 in, 500 out, 2000 cache write, 10000 cache read
//	                                = $0.002 + $0.005 + $0.005 + $0.002 = $0.014
//	parent row 2 zz-unpriced-model:  100 in,  50 out                    = unpriced
//	child  row 0 test-opus:         3000 in, 1000 out = $0.006 + $0.010 = $0.016
//	priced row 0 test-opus:         1000 in, 500 out  = $0.002 + $0.005 = $0.007
const (
	usageParentID   = "claude:usage-parent"
	usageChildID    = "agent-usage-child"
	usagePricedID   = "claude:usage-priced"
	usageModel      = "test-opus"
	usageUnpriced   = "zz-unpriced-model"
	usageParentCost = 14000 // microdollars
	usageChildCost  = 16000
	usagePricedCost = 7000
)

func seedSessionUsageFixture(t *testing.T, d *db.DB) {
	t.Helper()
	require.NoError(t, d.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         usageModel,
		InputPerMTok:         money.MustParseDollars("2.0"),
		OutputPerMTok:        money.MustParseDollars("10.0"),
		CacheCreationPerMTok: money.MustParseDollars("2.5"),
		CacheReadPerMTok:     money.MustParseDollars("0.2"),
	}}))
	parent := usageParentID
	seedUsageSession(t, d, usageParentID, nil, 550, 13000)
	seedUsageSession(t, d, usageChildID, &parent, 1000, 3000)
	seedUsageSession(t, d, usagePricedID, nil, 500, 1000)
	dbtest.SeedMessages(t, d,
		usageRow(usageParentID, 1, "10:00:00", usageModel,
			`{"input_tokens":1000,"output_tokens":500,"cache_creation_input_tokens":2000,"cache_read_input_tokens":10000}`),
		usageRow(usageParentID, 2, "10:05:00", usageUnpriced,
			`{"input_tokens":100,"output_tokens":50}`),
		usageRow(usageChildID, 0, "10:02:00", usageModel,
			`{"input_tokens":3000,"output_tokens":1000}`),
		usageRow(usagePricedID, 0, "11:00:00", usageModel,
			`{"input_tokens":1000,"output_tokens":500}`),
	)
}

func seedUsageSession(
	t *testing.T, d *db.DB, id string, parentID *string, output, peak int,
) {
	t.Helper()
	started := "2026-05-20T09:00:00Z"
	dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &started
		s.TotalOutputTokens = output
		s.HasTotalOutputTokens = true
		s.PeakContextTokens = peak
		s.HasPeakContextTokens = true
		if parentID != nil {
			s.ParentSessionID = parentID
			s.RelationshipType = "subagent"
		}
	})
}

func usageRow(sessionID string, ordinal int, clock, model, tokens string) db.Message {
	msg := dbtest.AsstMsg(sessionID, ordinal, "work")
	msg.Timestamp = "2026-05-20T" + clock + "Z"
	msg.Model = model
	msg.ClaudeMessageID = fmt.Sprintf("%s-m%d", sessionID, ordinal)
	msg.ClaudeRequestID = "req-" + msg.ClaudeMessageID
	msg.TokenUsage = jsontext.Value(tokens)
	return msg
}

// sessionUsageBackends returns the archive behind each service the MCP
// command can select: direct store reads (the PostgreSQL mode) and HTTP
// reads from a daemon or --server.
func sessionUsageBackends(t *testing.T) map[string]service.SessionService {
	t.Helper()
	direct := dbtest.OpenTestDB(t)
	seedSessionUsageFixture(t, direct)

	remote := dbtest.OpenTestDB(t)
	seedSessionUsageFixture(t, remote)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := server.New(config.Config{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port,
		DataDir: t.TempDir(), WriteTimeout: 30 * time.Second,
	}, remote, nil)
	ts := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, ts.Listener.Close())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)

	return map[string]service.SessionService{
		"direct": service.NewReadOnlyBackend(direct),
		"http": servicehttp.NewHTTPBackendForServer(ts.URL, "",
			servicehttp.HTTPServerCapabilities{APIVersion: server.SubagentUsageAPIVersion}),
	}
}

func newSessionUsageClient(t *testing.T, svc service.SessionService) *mcp.ClientSession {
	t.Helper()
	st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: svc}))
	t.Cleanup(func() {
		require.NoError(t, ct.Close())
		require.NoError(t, st.Wait())
	})
	return ct
}

func callSessionUsage(t *testing.T, ct *mcp.ClientSession, args map[string]any) sessionUsageOut {
	t.Helper()
	res, err := ct.CallTool(t.Context(), callParams(ToolGetSessionUsage, args))
	require.NoError(t, err)
	return decodeStructured[sessionUsageOut](t, res)
}

func TestServer_GetSessionUsage(t *testing.T) {
	for name, svc := range sessionUsageBackends(t) {
		t.Run(name, func(t *testing.T) {
			ct := newSessionUsageClient(t, svc)

			t.Run("subagents and unpriced model", func(t *testing.T) {
				// A raw UUID resolves to its stored ID.
				out := callSessionUsage(t, ct, map[string]any{"session_id": "usage-parent"})
				assert.Equal(t, usageParentID, out.SessionID)
				assert.Equal(t, "claude", out.Agent)
				assert.Equal(t, "proj", out.Project)
				assert.True(t, out.HasTokenData)
				assert.Equal(t, 1550, out.TotalOutputTokens)
				assert.Equal(t, 1, out.SubagentCount)
				assert.Equal(t, []string{usageModel, usageUnpriced}, out.Models)
				assert.Equal(t, []string{usageUnpriced}, out.UnpricedModels)
				assert.False(t, out.HasCost, "an unpriced model leaves the session cost unknown")
				assert.Zero(t, out.Cost.Microdollars)
				assert.Equal(t, 3, out.BreakdownCount)
				assert.Equal(t, []service.SessionUsageModel{
					{
						Model: usageModel, Rows: 2, InputTokens: 4000, OutputTokens: 1500,
						CacheCreationInputTokens: 2000, CacheReadInputTokens: 10000,
						Cost:    money.Money{Microdollars: usageParentCost + usageChildCost},
						HasCost: true,
					},
					{Model: usageUnpriced, Rows: 1, InputTokens: 100, OutputTokens: 50},
				}, out.ByModel)
				require.Len(t, out.Breakdown, 3)
				assert.Nil(t, out.BreakdownNextCursor)
				assert.Equal(t, usageChildID, out.Breakdown[1].SubagentSessionID,
					"rows stay in time order with the subagent row between the parent's")
			})

			t.Run("own only", func(t *testing.T) {
				out := callSessionUsage(t, ct, map[string]any{
					"session_id": usageParentID, "own_only": true,
				})
				assert.Zero(t, out.SubagentCount)
				assert.Equal(t, 550, out.TotalOutputTokens)
				assert.Equal(t, 13000, out.PeakContextTokens)
				assert.Equal(t, 2, out.BreakdownCount)
				require.Len(t, out.ByModel, 2)
				assert.Equal(t, money.Money{Microdollars: usageParentCost}, out.ByModel[0].Cost)
				assert.Equal(t, 1, out.ByModel[0].Rows)
			})

			t.Run("priced session", func(t *testing.T) {
				out := callSessionUsage(t, ct, map[string]any{"session_id": usagePricedID})
				assert.True(t, out.HasCost)
				assert.Equal(t, money.Money{Microdollars: usagePricedCost}, out.Cost)
				assert.Equal(t, export.CostSourceComputed, out.CostSource)
				assert.Empty(t, out.UnpricedModels)
			})

			t.Run("breakdown pages by cost", func(t *testing.T) {
				first := callSessionUsage(t, ct, map[string]any{
					"session_id": usageParentID, "breakdown_sort": "cost", "breakdown_limit": 1,
				})
				require.Len(t, first.Breakdown, 1)
				assert.Equal(t, usageChildID, first.Breakdown[0].SubagentSessionID)
				assert.Equal(t, money.Money{Microdollars: usageChildCost}, first.Breakdown[0].Cost)
				assert.Equal(t, 3000, first.Breakdown[0].InputTokens)
				require.NotNil(t, first.BreakdownNextCursor)
				assert.Equal(t, 1, *first.BreakdownNextCursor)

				rest := callSessionUsage(t, ct, map[string]any{
					"session_id": usageParentID, "breakdown_sort": "cost",
					"breakdown_cursor": *first.BreakdownNextCursor,
				})
				require.Len(t, rest.Breakdown, 2)
				assert.Equal(t, money.Money{Microdollars: usageParentCost}, rest.Breakdown[0].Cost)
				assert.Equal(t, 2000, rest.Breakdown[0].CacheCreationInputTokens)
				assert.Equal(t, 10000, rest.Breakdown[0].CacheReadInputTokens)
				assert.Equal(t, usageUnpriced, rest.Breakdown[1].Model)
				assert.False(t, rest.Breakdown[1].HasCost)
				assert.Nil(t, rest.BreakdownNextCursor)
			})

			t.Run("unknown session", func(t *testing.T) {
				res, err := ct.CallTool(t.Context(), callParams(ToolGetSessionUsage,
					map[string]any{"session_id": "claude:missing"}))
				require.NoError(t, err)
				require.True(t, res.IsError)
				require.NotEmpty(t, res.Content)
				text, ok := res.Content[0].(*mcp.TextContent)
				require.True(t, ok)
				assert.Contains(t, text.Text, "session not found: claude:missing")
			})

			t.Run("invalid sort", func(t *testing.T) {
				res, err := ct.CallTool(t.Context(), callParams(ToolGetSessionUsage,
					map[string]any{"session_id": usageParentID, "breakdown_sort": "tokens"}))
				require.NoError(t, err)
				assert.True(t, res.IsError)
			})
		})
	}
}
