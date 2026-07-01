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

// TestSearchSessions_Pagination walks the int cursor end to end: a first
// page sets next_cursor, and feeding it back returns the following,
// non-overlapping page.
func TestSearchSessions_Pagination(t *testing.T) {
	ts, d := newTestToolset(t)
	if !d.HasFTS() {
		t.Skip("FTS not available")
	}
	// Six sessions all matching "pageterm", with increasing ended_at so
	// recency ordering is deterministic. All old enough to not be excluded.
	for i := range 6 {
		id := "p" + string(rune('0'+i))
		ended := "2024-06-1" + string(rune('0'+i)) + "T10:00:00Z"
		seedFTSSession(t, d, id, "proj", "pageterm body", ended)
	}

	page1 := mustSearch(t, ts, searchSessionsIn{Query: "pageterm", Sort: "recency", Limit: 2})
	require.Len(t, page1.Results, 2)
	require.NotNil(t, page1.NextCursor, "first page should set next_cursor")

	page2 := mustSearch(t, ts, searchSessionsIn{
		Query: "pageterm", Sort: "recency", Limit: 2, Cursor: *page1.NextCursor,
	})
	require.Len(t, page2.Results, 2)

	seen := map[string]bool{}
	for _, r := range page1.Results {
		seen[r.SessionID] = true
	}
	for _, r := range page2.Results {
		assert.False(t, seen[r.SessionID],
			"page 2 must not repeat page 1 (overlap on %s)", r.SessionID)
	}
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

// Even when a caller explicitly allow-lists the "system" role, get_messages
// must still drop IsSystem-flagged messages: the schema promises system
// messages are always excluded. The IsSystem gate short-circuits ahead of the
// role allowlist, so an allow-listed assistant message comes through while the
// equally allow-listed system message stays filtered. This pins the
// security-relevant ordering against a future change that let an explicit
// roles request surface system content.
func TestGetMessages_ExplicitSystemRoleStillFiltered(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "hello"),
		{
			SessionID: "s1", Ordinal: 1, Role: "system",
			Content: "system noise", IsSystem: true, ContentLength: len("system noise"),
		},
		dbtest.AsstMsg("s1", 2, "short reply"),
	}))

	_, out, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1",
		Roles:     []string{"system", "assistant"},
	})
	require.NoError(t, err)
	// The allow-listed assistant message is returned; the system message is
	// not, even though "system" was also allow-listed. The user message is
	// filtered because its role is not in the allowlist.
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "assistant", out.Messages[0].Role)
	for _, m := range out.Messages {
		assert.NotEqual(t, "system", m.Role, "system messages must never be returned")
	}
	assert.Equal(t, 2, out.Filtered)
}

func TestSearchContent_SubstringMatch(t *testing.T) {
	ts, d := newTestToolset(t)
	// Not a one-shot: content search excludes one-shot sessions by default.
	// An explicit old ended_at keeps it out of the active-session guard.
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
		ended := "2024-06-15T10:00:00Z"
		s.EndedAt = &ended
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

// search_content's self-reference guard must exclude matches from sessions
// that are active now, even when the matching message itself is old. A
// long-running current session can match on a stale line; excluding by the
// match timestamp alone would leak it. Exclusion is by session activity,
// like search_sessions.
func TestSearchContent_ExcludesActiveSessionWithOldMatch(t *testing.T) {
	ts, d := newTestToolset(t)
	// Active session: ended one minute before now, but its matching message
	// is two hours old.
	dbtest.SeedSession(t, d, "active", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
		ended := "2024-06-15T11:59:00Z"
		s.EndedAt = &ended
	})
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID: "active", Ordinal: 0, Role: "user",
		Content: "old needle here", ContentLength: len("old needle here"),
		Timestamp: "2024-06-15T10:00:00Z",
	}}))
	// Idle session: ended two hours before now.
	dbtest.SeedSession(t, d, "idle", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
		ended := "2024-06-15T10:00:00Z"
		s.EndedAt = &ended
	})
	require.NoError(t, d.InsertMessages([]db.Message{{
		SessionID: "idle", Ordinal: 0, Role: "user",
		Content: "idle needle here", ContentLength: len("idle needle here"),
		Timestamp: "2024-06-15T10:00:00Z",
	}}))

	// Default (include_active=false): the active session is excluded despite
	// its old match; only the idle session is returned.
	_, out, err := ts.searchContent(context.Background(), nil, searchContentIn{
		Pattern: "needle", Mode: "substring",
	})
	require.NoError(t, err)
	require.Len(t, out.Matches, 1)
	assert.Equal(t, "idle", out.Matches[0].SessionID)
	assert.Equal(t, 1, out.ExcludedActive)

	// include_active=true returns both, excluding nothing.
	_, all, err := ts.searchContent(context.Background(), nil, searchContentIn{
		Pattern: "needle", Mode: "substring", IncludeActive: true,
	})
	require.NoError(t, err)
	assert.Len(t, all.Matches, 2)
	assert.Equal(t, 0, all.ExcludedActive)
}

// A freshly created/synced session can have no parsed ended_at or started_at
// yet, only created_at. Its activity must fall back to created_at so a
// current timestampless session is still excluded by the default guard,
// rather than resolving to an empty timestamp and leaking through. Uses a
// real clock because created_at is set to now by the DB at insert.
func TestSearchContent_TimestamplessSessionExcludedByCreatedAt(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	ts := &toolset{svc: service.NewDirectBackend(d, nil), now: time.Now}
	// StartedAt and EndedAt are left nil on purpose; created_at defaults to
	// now in the schema, so the session is active.
	dbtest.SeedSession(t, d, "fresh", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("fresh", 0, "needle in a fresh session"),
	}))

	// Default guard excludes the still-active session despite no start/end.
	_, out, err := ts.searchContent(context.Background(), nil, searchContentIn{
		Pattern: "needle", Mode: "substring",
	})
	require.NoError(t, err)
	assert.Empty(t, out.Matches)
	assert.Equal(t, 1, out.ExcludedActive)

	// include_active=true surfaces it.
	_, all, err := ts.searchContent(context.Background(), nil, searchContentIn{
		Pattern: "needle", Mode: "substring", IncludeActive: true,
	})
	require.NoError(t, err)
	assert.Len(t, all.Matches, 1)
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
	// Multi-turn with the same marker; old ended_at keeps it inactive.
	dbtest.SeedSession(t, d, "multi", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
		ended := "2024-06-15T10:00:00Z"
		s.EndedAt = &ended
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
		SessionID: "s1", Direction: "asc", From: &from,
	})
	require.NoError(t, err)
	require.NotEmpty(t, asc.Messages)
	assert.Equal(t, 2, asc.Messages[0].Ordinal, "asc honors the from anchor")
	for i := 1; i < len(asc.Messages); i++ {
		assert.Less(t, asc.Messages[i-1].Ordinal, asc.Messages[i].Ordinal)
	}
}

// Ordinal 0 is a valid anchor (search_sessions can return match_ordinal 0),
// so from:0 must be honored, not treated as "omitted". With desc it anchors
// at ordinal 0 -- returning only that message -- rather than falling back to
// newest-first.
func TestGetMessages_FromZeroAnchors(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "m0"),
		dbtest.AsstMsg("s1", 1, "m1"),
		dbtest.UserMsg("s1", 2, "m2"),
	}))

	zero := 0
	_, out, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "desc", From: &zero,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	assert.Equal(t, 0, out.Messages[0].Ordinal,
		"from:0 anchors at ordinal 0, not newest-first")
}

// get_messages promises system messages are always excluded. Legacy
// sessions store system-injected messages as user-role rows without the
// is_system flag, identified only by a content prefix; those must be
// excluded too, not just is_system rows.
func TestGetMessages_ExcludesSystemPrefixedUserMessage(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "real question"),
		// User role, is_system not set, but a system content prefix.
		{
			SessionID: "s1", Ordinal: 1, Role: "user",
			Content:       "<task-notification>done</task-notification>",
			ContentLength: 44,
		},
	}))

	_, out, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1",
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1)
	assert.Equal(t, "real question", out.Messages[0].Content)
	assert.Equal(t, 1, out.Filtered, "the system-prefixed user message is filtered")
}

// get_messages returns next_from when a full page may have more rows, so a
// client can page reliably even when filtering shortens a page. next_from
// is anchored on the last scanned ordinal, and the final partial page omits
// it.
func TestGetMessages_NextFromCursor(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 5
		s.UserMessageCount = 3
		ended := "2024-06-15T10:00:00Z"
		s.EndedAt = &ended
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "m0"),
		dbtest.AsstMsg("s1", 1, "m1"),
		dbtest.UserMsg("s1", 2, "m2"),
		dbtest.AsstMsg("s1", 3, "m3"),
		dbtest.UserMsg("s1", 4, "m4"),
	}))

	// asc page 1: ordinals 0,1 -> next_from 2.
	_, p1, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "asc", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, p1.Messages, 2)
	assert.Equal(t, 0, p1.Messages[0].Ordinal)
	require.NotNil(t, p1.NextFrom)
	assert.Equal(t, 2, *p1.NextFrom)

	// asc page 2 from the cursor: ordinals 2,3 -> next_from 4.
	_, p2, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "asc", Limit: 2, From: p1.NextFrom,
	})
	require.NoError(t, err)
	require.Len(t, p2.Messages, 2)
	assert.Equal(t, 2, p2.Messages[0].Ordinal)
	require.NotNil(t, p2.NextFrom)
	assert.Equal(t, 4, *p2.NextFrom)

	// asc final page: ordinal 4 only; partial page has no next cursor.
	_, p3, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "asc", Limit: 2, From: p2.NextFrom,
	})
	require.NoError(t, err)
	require.Len(t, p3.Messages, 1)
	assert.Equal(t, 4, p3.Messages[0].Ordinal)
	assert.Nil(t, p3.NextFrom, "final partial page omits next_from")

	// desc page 1 from newest: ordinals 4,3 -> next_from 2 (anchor moves down).
	_, dpage, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "desc", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, dpage.Messages, 2)
	assert.Equal(t, 4, dpage.Messages[0].Ordinal)
	require.NotNil(t, dpage.NextFrom)
	assert.Equal(t, 2, *dpage.NextFrom)
}

// next_from must advance past the last SCANNED ordinal, not the last visible
// one, so a filtered message at the page boundary is not re-scanned on the
// next page. The raw page fills the limit but a system message is filtered,
// making the visible page shorter than the limit.
func TestGetMessages_NextFromUsesScannedNotVisible(t *testing.T) {
	ts, d := newTestToolset(t)
	dbtest.SeedSession(t, d, "s1", "proj", func(s *db.Session) {
		s.MessageCount = 3
		s.UserMessageCount = 2
		ended := "2024-06-15T10:00:00Z"
		s.EndedAt = &ended
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("s1", 0, "v0"),
		{
			SessionID: "s1", Ordinal: 1, Role: "system",
			Content: "sys", IsSystem: true, ContentLength: 3,
		},
		dbtest.UserMsg("s1", 2, "v2"),
	}))

	// asc, limit 2: raw page = ordinals 0,1; ordinal 1 (system) is filtered.
	_, out, err := ts.getMessages(context.Background(), nil, getMessagesIn{
		SessionID: "s1", Direction: "asc", Limit: 2,
	})
	require.NoError(t, err)
	require.Len(t, out.Messages, 1, "filtered system message shortens the page")
	assert.Equal(t, 0, out.Messages[0].Ordinal)
	assert.Equal(t, 1, out.Filtered)
	require.NotNil(t, out.NextFrom)
	assert.Equal(t, 2, *out.NextFrom,
		"next_from advances past scanned ordinal 1, not visible ordinal 0")
}

// The schemas promise that message_count counts every stored message across
// all roles (system included), and that a full get_messages pagination sweep
// reconciles against it: returned messages plus filtered add up to
// message_count for any roles filter. This pins that contract so a refactor
// of either code path cannot silently break it (issue #944).
func TestGetMessages_FilteredReconcilesWithMessageCount(t *testing.T) {
	ts, d := newTestToolset(t)
	// A realistic mix: user/assistant turns, an is_system-flagged row, a
	// tool dump, and a legacy system-prefixed user row (parsed before
	// is_system was backfilled, so the flag is unset). message_count is
	// the row total, mirroring the sync engine's write-time derivation.
	msgs := []db.Message{
		dbtest.UserMsg("s1", 0, "hello"),
		dbtest.AsstMsg("s1", 1, "hi"),
		{
			SessionID: "s1", Ordinal: 2, Role: "system",
			Content: "sys", IsSystem: true, ContentLength: 3,
		},
		{
			SessionID: "s1", Ordinal: 3, Role: "tool",
			Content: "tool output", ContentLength: 11,
		},
		dbtest.UserMsg("s1", 4,
			"This session is being continued from a previous conversation"),
		dbtest.AsstMsg("s1", 5, "more"),
		{
			SessionID: "s1", Ordinal: 6, Role: "tool",
			Content: "tool output 2", ContentLength: 13,
		},
		dbtest.UserMsg("s1", 7, "bye"),
	}
	dbtest.SeedSessionWithMessages(t, d, "s1", "proj", msgs,
		dbtest.WithMessageCounts(len(msgs), 3))

	_, ov, err := ts.sessionOverview(context.Background(), nil,
		sessionOverviewIn{SessionID: "s1"})
	require.NoError(t, err)
	require.Equal(t, len(msgs), ov.Session.MessageCount)

	tests := []struct {
		name  string
		roles []string
	}{
		{"default roles", nil},
		{"user assistant tool", []string{"user", "assistant", "tool"}},
		{"tool only", []string{"tool"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			returned, filtered := 0, 0
			var from *int
			for range len(msgs) + 1 { // bounded: a sweep never needs more pages
				_, out, err := ts.getMessages(context.Background(), nil,
					getMessagesIn{
						SessionID: "s1", Direction: "asc",
						Limit: 3, From: from, Roles: tt.roles,
					})
				require.NoError(t, err)
				returned += len(out.Messages)
				filtered += out.Filtered
				if out.NextFrom == nil {
					break
				}
				from = out.NextFrom
			}
			assert.Equal(t, ov.Session.MessageCount, returned+filtered,
				"returned + filtered must reconcile to message_count")
		})
	}
}

// search_sessions must exclude a session active now even when its search
// result carries no ended_at/started_at (empty SessionEndedAt), by falling
// back to created_at like search_content -- mirroring the canonical
// activity expression.
func TestSearchSessions_TimestamplessExcludedByCreatedAt(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	if !d.HasFTS() {
		t.Skip("FTS not available")
	}
	ts := &toolset{svc: service.NewDirectBackend(d, nil), now: time.Now}
	// No ended_at/started_at; created_at defaults to now, so it is active.
	dbtest.SeedSession(t, d, "fresh", "proj", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	require.NoError(t, d.InsertMessages([]db.Message{
		dbtest.UserMsg("fresh", 0, "uniquesearchmarker here"),
	}))

	_, out, err := ts.searchSessions(context.Background(), nil, searchSessionsIn{
		Query: "uniquesearchmarker",
	})
	require.NoError(t, err)
	assert.Empty(t, out.Results)
	assert.Equal(t, 1, out.ExcludedActive)

	_, all, err := ts.searchSessions(context.Background(), nil, searchSessionsIn{
		Query: "uniquesearchmarker", IncludeActive: true,
	})
	require.NoError(t, err)
	assert.Len(t, all.Results, 1)
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
