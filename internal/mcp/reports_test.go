package mcp

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/service"
)

// seedReportSessions stores n sessions on 2024-06-01, session i lasting
// i+1 minutes, each with one Bash call. Session s0 has tool failures.
func seedReportSessions(t *testing.T, d *db.DB, n int) {
	t.Helper()
	for i := range n {
		id := fmt.Sprintf("s%d", i)
		start := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
		end := start.Add(time.Duration(i+1) * time.Minute)
		dbtest.SeedSession(t, d, id, "proj", func(s *db.Session) {
			s.StartedAt = new(start.Format(time.RFC3339))
			s.EndedAt = new(end.Format(time.RFC3339))
			s.MessageCount = 2
			s.UserMessageCount = 2
		})
		user := dbtest.UserMsg(id, 0, "go")
		user.Timestamp = start.Format(time.RFC3339)
		reply := toolCallMsg(id, 1, db.ToolCall{ToolName: "Bash", Category: "Bash"})
		reply.Timestamp = end.Format(time.RFC3339)
		dbtest.SeedMessages(t, d, user, reply)
	}
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "s0",
		db.SessionSignalUpdate{ToolFailureSignalCount: 2}))
}

func newReportClient(t *testing.T, n int) *mcp.ClientSession {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	seedReportSessions(t, d, n)
	srv := newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }})
	st, ct := newInMemoryPair(t, srv)
	t.Cleanup(func() {
		require.NoError(t, ct.Close())
		require.NoError(t, st.Wait())
	})
	return ct
}

func TestServer_ActivityReportPagesSessions(t *testing.T) {
	ct := newReportClient(t, 5)
	args := map[string]any{"preset": "day", "date": "2024-06-01", "sessions_limit": 2}
	res, err := ct.CallTool(t.Context(), callParams(ToolGetActivityReport, args))
	require.NoError(t, err)
	first := decodeStructured[activityReportOut](t, res)
	assert.Equal(t, 5, first.Totals.Sessions)
	assert.Equal(t, 5, first.SessionsTotal)
	require.Len(t, first.Sessions, 2)
	assert.Equal(t, "s4", first.Sessions[0].SessionID, "longest session first")
	require.NotNil(t, first.SessionsNextCursor)
	assert.Equal(t, 2, *first.SessionsNextCursor)

	args["sessions_cursor"] = 4
	res, err = ct.CallTool(t.Context(), callParams(ToolGetActivityReport, args))
	require.NoError(t, err)
	last := decodeStructured[activityReportOut](t, res)
	require.Len(t, last.Sessions, 1)
	assert.Equal(t, "s0", last.Sessions[0].SessionID)
	assert.Nil(t, last.SessionsNextCursor)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetActivityReport, map[string]any{
		"preset": "day", "date": "2024-06-01", "sessions_sort": "title",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

type activityReportService struct {
	service.SessionService
	report *activity.Report
	last   service.ActivityReportRequest
}

func (a *activityReportService) ActivityReport(
	_ context.Context, req service.ActivityReportRequest,
) (*activity.Report, error) {
	a.last = req
	return a.report, nil
}

func TestActivityReport_ForwardsSelectionAndSortsByCost(t *testing.T) {
	t.Parallel()
	minutes := 5.0
	svc := &activityReportService{report: &activity.Report{BySession: []activity.SessionRow{
		{SessionID: "cheap", AgentMinutes: &minutes},
		{SessionID: "pricey", Cost: money.Money{Microdollars: 900}},
	}}}
	ts := &toolset{svc: svc}
	_, out, err := ts.activityReport(t.Context(), nil, activityReportIn{
		Preset: "week", Date: "2024-06-03", Timezone: "Europe/Berlin",
		Project: "p", Agent: "codex", Machine: "m", Automation: "interactive",
		SessionsSort: "cost",
	})
	require.NoError(t, err)
	assert.Equal(t, service.ActivityReportRequest{
		Preset: "week", Date: "2024-06-03", Timezone: "Europe/Berlin",
		Project: "p", Agent: "codex", Machine: "m", Automation: "interactive",
	}, svc.last)
	require.Len(t, out.Sessions, 2)
	assert.Equal(t, "pricey", out.Sessions[0].SessionID)

	_, _, err = ts.activityReport(t.Context(), nil, activityReportIn{SessionsCursor: -1})
	require.Error(t, err)
}

func TestServer_ToolUsageAndQualitySignals(t *testing.T) {
	ct := newReportClient(t, 3)
	window := map[string]any{"from": "2024-06-01", "to": "2024-06-01"}

	res, err := ct.CallTool(t.Context(), callParams(ToolGetToolUsage, window))
	require.NoError(t, err)
	usage := decodeStructured[db.ToolsAnalyticsResponse](t, res)
	assert.Equal(t, 3, usage.TotalCalls)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetQualitySignals, window))
	require.NoError(t, err)
	plain := decodeStructured[qualitySignalsOut](t, res)
	require.NotNil(t, plain.Signals)
	assert.Equal(t, 2, plain.Signals.ToolHealth.TotalFailureSignals)
	assert.Nil(t, plain.Examples)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetQualitySignals, map[string]any{
		"from": "2024-06-01", "to": "2024-06-01", "signal": "tool_failure_signals", "examples": 3,
	}))
	require.NoError(t, err)
	withExamples := decodeStructured[qualitySignalsOut](t, res)
	require.NotNil(t, withExamples.Examples)
	require.Len(t, withExamples.Examples.Sessions, 1)
	assert.Equal(t, "s0", withExamples.Examples.Sessions[0].SessionID)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetQualitySignals, map[string]any{"signal": "vibes"}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}

func TestServer_TermTrends(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	const start = "2024-06-03T09:00:00Z"
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.StartedAt = new(start)
		s.CreatedAt = start
		s.MessageCount = 2
		s.UserMessageCount = 2
	})
	user := dbtest.UserMsg("s1", 0, "a seam here")
	user.Timestamp = start
	reply := dbtest.AsstMsg("s1", 1, "two seams")
	reply.Timestamp = start
	dbtest.SeedMessages(t, d, user, reply)
	srv := newServer(ServeOptions{Service: service.NewDirectBackend(d, nil), Now: func() time.Time { return fixedNow }})
	st, ct := newInMemoryPair(t, srv)
	t.Cleanup(func() {
		require.NoError(t, ct.Close())
		require.NoError(t, st.Wait())
	})

	res, err := ct.CallTool(t.Context(), callParams(ToolGetTermTrends, map[string]any{
		"terms": []string{"seam"}, "from": "2024-06-01", "to": "2024-06-09", "granularity": "day",
	}))
	require.NoError(t, err)
	out := decodeStructured[db.TrendsTermsResponse](t, res)
	assert.Equal(t, "day", out.Granularity)
	assert.Equal(t, 2, out.MessageCount)
	require.Len(t, out.Series, 1)
	assert.Equal(t, 2, out.Series[0].Total)

	res, err = ct.CallTool(t.Context(), callParams(ToolGetTermTrends, map[string]any{
		"terms": []string{"seam"}, "granularity": "year",
	}))
	require.NoError(t, err)
	assert.True(t, res.IsError)
}
