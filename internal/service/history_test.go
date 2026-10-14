package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// seedEditHistory stores a parent session that edits two files and two
// child sessions, one of them trashed.
func seedEditHistory(t *testing.T, d *db.DB) {
	t.Helper()
	dbtest.SeedSession(t, d, "parent", "proj")
	dbtest.SeedSession(t, d, "other", "proj-b")
	edit := func(sid string, ordinal int, path, ts string) db.Message {
		m := dbtest.AsstMsg(sid, ordinal, "[Edit: "+path+"]")
		m.Timestamp = ts
		m.HasToolUse = true
		m.ToolCalls = []db.ToolCall{{
			SessionID: sid, ToolName: "Edit", Category: "Edit", FilePath: path,
		}}
		return m
	}
	dbtest.SeedMessages(t, d,
		edit("parent", 0, "internal/app/main.go", "2024-06-01T10:00:00Z"),
		edit("parent", 1, "internal/app/util.go", "2024-06-01T10:01:00Z"),
	)
	dbtest.SeedMessages(t, d, edit("other", 0, "internal/app/main.go", "2024-06-01T11:00:00Z"))
	for _, child := range []struct{ id, relationship, started string }{
		{"child-sub", "subagent", "2024-06-01T10:05:00Z"},
		{"child-fork", "fork", "2024-06-01T10:06:00Z"},
		{"child-trashed", "subagent", "2024-06-01T10:07:00Z"},
	} {
		dbtest.SeedSession(t, d, child.id, "proj", func(s *db.Session) {
			s.ParentSessionID = new("parent")
			s.RelationshipType = child.relationship
			s.StartedAt = new(child.started)
		})
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "child-trashed"))
}

// historyBackends returns the direct and daemon-backed services over the
// same seeded archive so each assertion covers both transports.
func historyBackends(t *testing.T) map[string]service.SessionService {
	t.Helper()
	env := newHTTPBackendEnv(t)
	seedEditHistory(t, env.DB)
	return map[string]service.SessionService{
		"direct": service.NewDirectBackend(env.DB, nil),
		"http":   env.Backend("", false),
	}
}

func TestChildSessionsBothBackends(t *testing.T) {
	t.Parallel()
	for name, svc := range historyBackends(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			children, err := svc.ChildSessions(t.Context(), "parent")
			require.NoError(t, err)
			ids := make([]string, 0, len(children))
			for _, child := range children {
				ids = append(ids, child.ID)
			}
			assert.Equal(t, []string{"child-sub", "child-fork"}, ids)
			assert.Equal(t, "fork", children[1].RelationshipType)

			none, err := svc.ChildSessions(t.Context(), "child-sub")
			require.NoError(t, err)
			assert.NotNil(t, none)
			assert.Empty(t, none)
		})
	}
}

func TestRecentEditsBothBackends(t *testing.T) {
	t.Parallel()
	for name, svc := range historyBackends(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, err := svc.RecentEdits(t.Context(), service.RecentEditsFilter{Search: "MAIN.go"})
			require.NoError(t, err)
			require.Len(t, res.Files, 2, "one group per project")
			assert.Equal(t, "proj-b", res.Files[0].Project)
			assert.Equal(t, "other", res.Files[0].LastSessionID)
			assert.Equal(t, "proj", res.Files[1].Project)
			require.Len(t, res.Files[1].Edits, 1)
			assert.Equal(t, "parent", res.Files[1].Edits[0].SessionID)
			assert.False(t, res.HasMore)

			oversized, err := svc.RecentEdits(t.Context(), service.RecentEditsFilter{Limit: 201})
			require.NoError(t, err, "out-of-range limits fall back to the default")
			assert.Len(t, oversized.Files, 3)

			page, err := svc.RecentEdits(t.Context(), service.RecentEditsFilter{
				Project: "proj", Limit: 1, Offset: 1,
			})
			require.NoError(t, err)
			require.Len(t, page.Files, 1)
			assert.Equal(t, "internal/app/main.go", page.Files[0].FilePath)
			assert.False(t, page.HasMore)
		})
	}
}

// seedAnalyticsHistory stores sessions on 2024-06-01 with tool calls; one
// session carries tool failure signals. extra adds plain sessions so the
// activity report spans more than one daemon page.
func seedAnalyticsHistory(t *testing.T, d *db.DB, extra int) {
	t.Helper()
	seed := func(id string, opts ...func(*db.Session)) {
		opts = append([]func(*db.Session){func(s *db.Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.EndedAt = new("2024-06-01T10:30:00Z")
			s.MessageCount = 2
			s.UserMessageCount = 2
		}}, opts...)
		dbtest.SeedSession(t, d, id, "proj", opts...)
		user := dbtest.UserMsg(id, 0, "please fix it")
		user.Timestamp = "2024-06-01T10:00:00Z"
		reply := dbtest.AsstMsg(id, 1, "running")
		reply.Timestamp = "2024-06-01T10:05:00Z"
		reply.HasToolUse = true
		reply.ToolCalls = []db.ToolCall{{SessionID: id, ToolName: "Bash", Category: "Bash"}}
		dbtest.SeedMessages(t, d, user, reply)
	}
	seed("struggled")
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "struggled",
		db.SessionSignalUpdate{ToolFailureSignalCount: 3}))
	seed("smooth")
	for i := range extra {
		seed(fmt.Sprintf("extra-%03d", i))
	}
}

func analyticsBackends(t *testing.T, extra int) map[string]service.SessionService {
	t.Helper()
	env := newHTTPBackendEnv(t)
	seedAnalyticsHistory(t, env.DB, extra)
	return map[string]service.SessionService{
		"direct": service.NewDirectBackend(env.DB, nil),
		"http":   env.Backend("", false),
	}
}

var june1 = service.AnalyticsRequest{From: "2024-06-01", To: "2024-06-01"}

func TestToolAndSignalAnalyticsBothBackends(t *testing.T) {
	t.Parallel()
	for name, svc := range analyticsBackends(t, 0) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tools, err := svc.ToolAnalytics(t.Context(), june1)
			require.NoError(t, err)
			assert.Equal(t, 2, tools.TotalCalls)
			require.NotEmpty(t, tools.ByTool)
			assert.Equal(t, "Bash", tools.ByTool[0].ToolName)

			signals, err := svc.SignalAnalytics(t.Context(), june1)
			require.NoError(t, err)
			assert.Equal(t, 3, signals.ToolHealth.TotalFailureSignals)
			assert.Equal(t, 1, signals.ToolHealth.SessionsWithFailures)

			examples, err := svc.SignalSessions(t.Context(), service.SignalSessionsRequest{
				AnalyticsRequest: june1, Signal: "tool_failure_signals", Limit: 5,
			})
			require.NoError(t, err)
			require.Len(t, examples.Sessions, 1)
			assert.Equal(t, "struggled", examples.Sessions[0].SessionID)

			_, err = svc.SignalSessions(t.Context(), service.SignalSessionsRequest{
				AnalyticsRequest: june1, Signal: "not_a_signal",
			})
			var inputErr *service.AnalyticsInputError
			require.ErrorAs(t, err, &inputErr)

			_, err = svc.ToolAnalytics(t.Context(), service.AnalyticsRequest{From: "2024-06-02", To: "2024-06-01"})
			require.ErrorAs(t, err, &inputErr)
			assert.EqualError(t, err, "from must not be after to")
		})
	}
}

func TestActivityReportBothBackendsReturnsEverySession(t *testing.T) {
	t.Parallel()
	// More sessions than one daemon page, so the HTTP backend must follow
	// the session cursor.
	const extra = activity.DefaultSessionPageLimit + 5
	for name, svc := range analyticsBackends(t, extra) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			report, err := svc.ActivityReport(t.Context(), service.ActivityReportRequest{
				Preset: "day", Date: "2024-06-01", Timezone: "UTC",
			})
			require.NoError(t, err)
			assert.Equal(t, extra+2, report.Totals.Sessions)
			assert.Equal(t, extra+2, report.SessionsTotal)
			assert.Len(t, report.BySession, extra+2)
			assert.Empty(t, report.SessionsNextCursor)

			var inputErr *service.AnalyticsInputError
			_, err = svc.ActivityReport(t.Context(), service.ActivityReportRequest{Automation: "robots"})
			require.ErrorAs(t, err, &inputErr)
			_, err = svc.ActivityReport(t.Context(), service.ActivityReportRequest{
				Preset: "day", Date: "2024-06-01", Project: strings.Repeat("p", 1025),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "exceeds 1024 bytes")
			_, err = svc.ActivityReport(t.Context(), service.ActivityReportRequest{
				Preset: "custom", Date: "bad", From: "2024-06-01T00:00:00Z", To: "2024-06-02T00:00:00Z",
			})
			require.Error(t, err, "both backends reject a malformed date")
		})
	}
}
