package mcp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// fixedNow is the deterministic clock used in tests so the 10-minute
// self-reference exclusion window is reproducible.
var fixedNow = time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

func newTestToolset(t *testing.T) (*toolset, *db.DB) {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	return &toolset{
		svc: service.NewDirectBackend(d, nil),
		now: func() time.Time { return fixedNow },
	}, d
}

func seedFTSSession(t *testing.T, d *db.DB, id, project, content, endedAt string) {
	t.Helper()
	dbtest.SeedSession(t, d, id, project, func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		ended := endedAt
		s.EndedAt = &ended
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg(id, 0, content),
	}))
}

func TestSearchSessions_ReturnsHitsWithOrdinal(t *testing.T) {
	ts, d := newTestToolset(t)
	if !d.HasFTS() {
		t.Skip("FTS not available")
	}
	seedFTSSession(t, d, "s1", "proj-a",
		"the quick brown fox", "2024-06-15T10:00:00Z")
	seedFTSSession(t, d, "s2", "proj-b",
		"lazy dogs", "2024-06-15T10:00:00Z")

	_, out, err := ts.searchSessions(context.Background(), nil, searchSessionsIn{
		Query: "fox",
	})
	require.NoError(t, err)
	require.Len(t, out.Results, 1)
	assert.Equal(t, "s1", out.Results[0].SessionID)
	assert.Equal(t, "proj-a", out.Results[0].Project)
	assert.Zero(t, out.ExcludedActive)
}

func TestSearchSessions_ExcludesRecentlyActive(t *testing.T) {
	ts, d := newTestToolset(t)
	if !d.HasFTS() {
		t.Skip("FTS not available")
	}
	// s1 ended an hour before fixedNow -> kept; s2 ended 5 min before -> excluded.
	seedFTSSession(t, d, "s1", "proj", "shared term alpha", "2024-06-15T11:00:00Z")
	seedFTSSession(t, d, "s2", "proj", "shared term alpha", "2024-06-15T11:55:00Z")

	out := mustSearch(t, ts, searchSessionsIn{Query: "alpha"})
	require.Len(t, out.Results, 1)
	assert.Equal(t, "s1", out.Results[0].SessionID)
	assert.Equal(t, 1, out.ExcludedActive)

	// With include_active, the recent one is returned too.
	withActive := mustSearch(t, ts, searchSessionsIn{Query: "alpha", IncludeActive: true})
	assert.Len(t, withActive.Results, 2)
	assert.Zero(t, withActive.ExcludedActive)
}

func mustSearch(t *testing.T, ts *toolset, in searchSessionsIn) searchSessionsOut {
	t.Helper()
	_, out, err := ts.searchSessions(context.Background(), nil, in)
	require.NoError(t, err)
	return out
}

func TestListSessions_ReturnsRows(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "a-1", "proj-a", func(s *db.Session) {
		s.MessageCount = 4
		s.UserMessageCount = 2
	})
	dbtest.SeedSession(t, d, "b-1", "proj-b", func(s *db.Session) {
		s.MessageCount = 4
		s.UserMessageCount = 2
	})

	_, out, err := ts.listSessions(context.Background(), nil, listSessionsIn{
		Project: "proj-a",
	})
	require.NoError(t, err)
	require.Len(t, out.Sessions, 1)
	assert.Equal(t, "a-1", out.Sessions[0].SessionID)
	assert.Equal(t, "proj-a", out.Sessions[0].Project)
}

func TestGetSessionOverview_ChronologicalTail(t *testing.T) {
	ts, d := newTestToolset(t)
	cwd := "/home/u/proj"
	first := "open the door"
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 4
		s.UserMessageCount = 2
		s.Cwd = cwd
		s.FirstMessage = &first
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "open the door"),
		dbtest.AsstMsg("s1", 1, "opening"),
		dbtest.UserMsg("s1", 2, "now close it"),
		dbtest.AsstMsg("s1", 3, "closed"),
	}))

	_, out, err := ts.sessionOverview(context.Background(), nil, sessionOverviewIn{
		SessionID: "s1",
	})
	require.NoError(t, err)
	assert.Equal(t, "s1", out.Session.SessionID)
	assert.Equal(t, cwd, out.CWD)
	assert.Equal(t, "open the door", out.FirstMessage)
	require.NotEmpty(t, out.LastMessages)
	// Tail is restored to chronological (ascending) ordinal order.
	for i := 1; i < len(out.LastMessages); i++ {
		assert.Less(t, out.LastMessages[i-1].Ordinal, out.LastMessages[i].Ordinal)
	}
	// The very last message is surfaced.
	assert.Equal(t, "closed", out.LastMessages[len(out.LastMessages)-1].Content)
}

func TestGetSessionOverview_NotFound(t *testing.T) {
	ts, _ := newTestToolset(t)
	_, _, err := ts.sessionOverview(context.Background(), nil, sessionOverviewIn{
		SessionID: "nope",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGetMessages_RoleFilterAndTruncation(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	long := make([]byte, 50)
	for i := range long {
		long[i] = 'x'
	}
	sysMsg := db.Message{
		SessionID: "s1", Ordinal: 1, Role: "system",
		Content: "system noise", IsSystem: true, ContentLength: len("system noise"),
	}
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, string(long)),
		sysMsg,
		dbtest.AsstMsg("s1", 2, "short reply"),
	}))

	_, out, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID:          "s1",
		MaxCharsPerMessage: 10,
	})
	require.NoError(t, err)
	// system message filtered out; user+assistant kept.
	require.Len(t, out.Messages, 2)
	assert.Equal(t, 1, out.Filtered, "the system message is filtered")
	// The 50-char user message is truncated to 10 with FullLength set.
	first := out.Messages[0]
	assert.True(t, first.Truncated)
	assert.Equal(t, 50, first.FullLength)
	assert.Len(t, first.Content, 10)
}

func TestSearchContent_SubstringMatch(t *testing.T) {
	ts, d := newTestToolset(t)
	// Not a one-shot: content search excludes one-shot sessions by default.
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "error code DEADBEEF here"),
		dbtest.AsstMsg("s1", 1, "looking into it"),
	}))

	_, out, err := ts.searchContent(context.Background(), nil, searchContentIn{
		Pattern: "DEADBEEF",
		Mode:    "substring",
	})
	require.NoError(t, err)
	require.Len(t, out.Matches, 1)
	assert.Equal(t, "s1", out.Matches[0].SessionID)
}

func TestUsageSummary_EmptyRange(t *testing.T) {
	ts, _ := newTestToolset(t)
	_, out, err := ts.usageSummary(context.Background(), nil, usageSummaryIn{
		From: "2024-06-01", To: "2024-06-03",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "2024-06-01", out.From)
	assert.Equal(t, "2024-06-03", out.To)
}

// recordingService captures the request a tool builds, so MCP-layer
// request mapping can be asserted without a full backend. Unused methods
// fall through to the embedded nil interface (never called by the tools
// under test).
type recordingService struct {
	service.SessionService
	lastUsage service.UsageRequest
}

func (r *recordingService) UsageSummary(
	_ context.Context, req service.UsageRequest,
) (*service.UsageSummaryResult, error) {
	r.lastUsage = req
	return &service.UsageSummaryResult{From: req.From, To: req.To}, nil
}

// get_usage_summary must request one-shot sessions (matching the REST
// /usage/summary default), since cost analysis wants every session.
func TestUsageSummary_RequestsOneShotSessions(t *testing.T) {
	t.Parallel()
	rec := &recordingService{}
	ts := &toolset{svc: rec, now: func() time.Time { return fixedNow }}
	_, _, err := ts.usageSummary(context.Background(), nil, usageSummaryIn{
		From: "2024-06-01", To: "2024-06-02", Project: "p", Agent: "claude",
	})
	require.NoError(t, err)
	assert.True(t, rec.lastUsage.IncludeOneShot,
		"usage summary should include one-shot sessions")
	assert.Equal(t, "p", rec.lastUsage.Project)
	assert.Equal(t, "claude", rec.lastUsage.Agent)
}

// search_content excludes one-shot sessions by default, matching the
// standalone/REST behavior. (Tracked as a possible follow-up: expose an
// include_one_shot opt-in so single-exchange sessions can be searched.)
func TestSearchContent_ExcludesOneShotByDefault(t *testing.T) {
	ts, d := newTestToolset(t)
	// One-shot (UserMessageCount=1) with the marker.
	dbtest.SeedSession(t, d, "one", "proj", func(s *db.Session) {
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("one", 0, "marker ZEBRA42"),
	}))
	// Multi-turn with the same marker.
	dbtest.SeedSession(t, d, "multi", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("multi", 0, "marker ZEBRA42"),
		dbtest.AsstMsg("multi", 1, "ok"),
	}))

	_, out, err := ts.searchContent(context.Background(), nil, searchContentIn{
		Pattern: "ZEBRA42", Mode: "substring",
	})
	require.NoError(t, err)
	require.Len(t, out.Matches, 1, "one-shot session should be excluded")
	assert.Equal(t, "multi", out.Matches[0].SessionID)
}

func TestGetMessages_DescAndFromAnchor(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 5
		s.UserMessageCount = 3
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "m0"),
		dbtest.AsstMsg("s1", 1, "m1"),
		dbtest.UserMsg("s1", 2, "m2"),
		dbtest.AsstMsg("s1", 3, "m3"),
		dbtest.UserMsg("s1", 4, "m4"),
	}))

	// desc with no anchor -> newest first.
	_, desc, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "desc",
	})
	require.NoError(t, err)
	require.NotEmpty(t, desc.Messages)
	assert.Equal(t, 4, desc.Messages[0].Ordinal, "desc returns newest first")

	// asc anchored at ordinal 2 -> starts at 2, ascending.
	from := 2
	_, asc, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "asc", From: from,
	})
	require.NoError(t, err)
	require.NotEmpty(t, asc.Messages)
	assert.Equal(t, 2, asc.Messages[0].Ordinal, "asc honors the from anchor")
	for i := 1; i < len(asc.Messages); i++ {
		assert.Less(t, asc.Messages[i-1].Ordinal, asc.Messages[i].Ordinal)
	}
}

// TestServer_EndToEnd connects a real MCP client to the server over an
// in-memory transport and calls a tool, validating registration, schema
// inference, and the structured-output round-trip through the SDK.
func TestServer_EndToEnd(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	if !d.HasFTS() {
		t.Skip("FTS not available")
	}
	seedFTSSession(t, d, "s1", "proj", "unique end-to-end marker", "2024-01-01T10:00:00Z")

	srv := newServer(ServeOptions{
		Service: service.NewDirectBackend(d, nil),
		Now:     func() time.Time { return fixedNow },
	})

	ctx := context.Background()
	st, ct := newInMemoryPair(t, srv)

	tools, err := ct.ListTools(ctx, nil)
	require.NoError(t, err)
	names := make([]string, 0, len(tools.Tools))
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	assert.ElementsMatch(t, []string{
		ToolSearchSessions, ToolListSessions, ToolGetSessionOverview,
		ToolGetMessages, ToolSearchContent, ToolGetUsageSummary,
	}, names)

	res, err := ct.CallTool(ctx, callParams("search_sessions", map[string]any{
		"query": "marker",
	}))
	require.NoError(t, err)
	require.False(t, res.IsError, "tool returned error")

	var out searchSessionsOut
	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.Results, 1)
	assert.Equal(t, "s1", out.Results[0].SessionID)

	require.NoError(t, ct.Close())
	require.NoError(t, st.Wait())
}
