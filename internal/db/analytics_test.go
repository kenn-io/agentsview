package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type seedStats struct {
	TotalSessions          int
	TotalMessages          int
	TotalUserMessages      int
	TotalAssistantMessages int
	ActiveProjects         int
	ActiveDays             int
}

func seedAnalyticsData(t *testing.T, d *DB) seedStats {
	t.Helper()

	type sessionData struct {
		id      string
		project string
		start   string
		end     string
		msgs    int
		agent   string
		model   string
	}

	sessions := []sessionData{
		// Project A: 3 sessions across 2 days, mixed agents
		{"a1", "project-alpha", "2024-06-01T09:00:00Z", tsMidYear, 10, "claude", "claude-3-5-sonnet"},
		{"a2", "project-alpha", "2024-06-01T14:00:00Z", "2024-06-01T15:00:00Z", 20, "codex", "gpt-4o"},
		{"a3", "project-alpha", "2024-06-03T09:00:00Z", "2024-06-03T10:00:00Z", 5, "claude", "claude-3-5-sonnet"},
		// Project B: 2 sessions on 1 day
		{"b1", "project-beta", "2024-06-02T10:00:00Z", "2024-06-02T11:00:00Z", 30, "claude", "gpt-4o-mini"},
		{"b2", "project-beta", "2024-06-02T15:00:00Z", "2024-06-02T16:00:00Z", 15, "claude", "gpt-4o"},
	}

	stats := seedStats{}
	projects := make(map[string]bool)
	days := make(map[string]bool)

	for _, sess := range sessions {
		stats.TotalSessions++
		stats.TotalMessages += sess.msgs
		for i := 0; i < sess.msgs; i++ {
			if i%2 == 1 {
				stats.TotalAssistantMessages++
			} else {
				stats.TotalUserMessages++
			}
		}

		projects[sess.project] = true
		if len(sess.start) >= 10 {
			days[sess.start[:10]] = true
		}

		insertSession(t, d, sess.id, sess.project, func(s *Session) {
			s.StartedAt = new(sess.start)
			s.EndedAt = new(sess.end)
			s.MessageCount = sess.msgs
			s.Agent = sess.agent
		})

		msgs := make([]Message, sess.msgs)
		for i := 0; i < sess.msgs; i++ {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			msgs[i] = Message{
				SessionID:     sess.id,
				Ordinal:       i,
				Role:          role,
				Content:       fmt.Sprintf("msg %d", i),
				ContentLength: 5,
				Timestamp:     tsMidYear,
				Model:         sess.model,
			}
		}
		insertMessages(t, d, msgs...)
	}

	stats.ActiveProjects = len(projects)
	stats.ActiveDays = len(days)

	return stats
}

func baseFilter() AnalyticsFilter {
	return AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-03",
		Timezone: "UTC",
	}
}

func emptyFilter() AnalyticsFilter {
	return AnalyticsFilter{
		From:     "2020-01-01",
		To:       "2020-01-02",
		Timezone: "UTC",
	}
}

func mustSummary(
	t *testing.T, d *DB, ctx context.Context, f AnalyticsFilter,
) AnalyticsSummary {
	t.Helper()
	s, err := d.GetAnalyticsSummary(ctx, f)
	require.NoError(t, err, "GetAnalyticsSummary")
	return s
}

func mustActivity(
	t *testing.T, d *DB, ctx context.Context,
	f AnalyticsFilter, gran string,
) ActivityResponse {
	t.Helper()
	r, err := d.GetAnalyticsActivity(ctx, f, gran)
	require.NoError(t, err, "GetAnalyticsActivity")
	return r
}

func mustHeatmap(
	t *testing.T, d *DB, ctx context.Context,
	f AnalyticsFilter, metric string,
) HeatmapResponse {
	t.Helper()
	r, err := d.GetAnalyticsHeatmap(ctx, f, metric)
	require.NoError(t, err, "GetAnalyticsHeatmap")
	return r
}

func mustProjects(
	t *testing.T, d *DB, ctx context.Context,
	f AnalyticsFilter,
) ProjectsAnalyticsResponse {
	t.Helper()
	r, err := d.GetAnalyticsProjects(ctx, f)
	require.NoError(t, err, "GetAnalyticsProjects")
	return r
}

func TestGetAnalyticsSummary(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		s := mustSummary(t, d, ctx, baseFilter())
		assert.Equal(t, 0, s.TotalSessions, "TotalSessions")
	})

	stats := seedAnalyticsData(t, d)

	t.Run("FullRange", func(t *testing.T) {
		s := mustSummary(t, d, ctx, baseFilter())
		assert.Equal(t, stats.TotalSessions, s.TotalSessions, "TotalSessions")
		assert.Equal(t, stats.TotalMessages, s.TotalMessages, "TotalMessages")
		assert.Equal(t, stats.ActiveProjects, s.ActiveProjects, "ActiveProjects")
		assert.Equal(t, stats.ActiveDays, s.ActiveDays, "ActiveDays")
		assert.Equal(t, "project-beta", s.MostActive, "MostActive")
		// 2 projects, both in top 3 → concentration = 1.0
		assert.Equal(t, 1.0, s.Concentration, "Concentration")

		// Sorted message counts: [5, 10, 15, 20, 30]
		assert.Equal(t, 15, s.MedianMessages, "MedianMessages")
		// P90 index = int(5*0.9) = 4 → value 30
		assert.Equal(t, 30, s.P90Messages, "P90Messages")
		assert.Equal(t,
			[]string{
				"claude-3-5-sonnet",
				"gpt-4o",
				"gpt-4o-mini",
			},
			s.Models,
			"Models",
		)

		require.NotNil(t, s.Agents["claude"], "expected claude agent entry")
		assert.Equal(t, 4, s.Agents["claude"].Sessions, "claude sessions")
		require.NotNil(t, s.Agents["codex"], "expected codex agent entry")
		assert.Equal(t, 1, s.Agents["codex"].Sessions, "codex sessions")
	})

	t.Run("DateSubset", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
	})

	t.Run("MachineFilter", func(t *testing.T) {
		f := baseFilter()
		f.Machine = "nonexistent"
		s := mustSummary(t, d, ctx, f)
		assert.Equal(t, 0, s.TotalSessions, "TotalSessions")
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		s := mustSummary(t, d, ctx, emptyFilter())
		assert.Equal(t, 0, s.TotalSessions, "TotalSessions")
		assert.Empty(t, s.Models, "Models")
	})
}

// TestRelationshipExclusionSQL covers the shared predicate helper that
// the analytics builders and the stats pipeline both use, including the
// column qualifier the DuckDB builder relies on.
func TestRelationshipExclusionSQL(t *testing.T) {
	cases := []struct {
		includeSubagents bool
		colPrefix        string
		want             string
	}{
		{false, "", "relationship_type NOT IN ('subagent', 'fork')"},
		{true, "", "relationship_type NOT IN ('fork')"},
		{false, "s.", "s.relationship_type NOT IN ('subagent', 'fork')"},
		{true, "s.", "s.relationship_type NOT IN ('fork')"},
	}
	for _, c := range cases {
		got := RelationshipExclusionSQL(c.includeSubagents, c.colPrefix)
		assert.Equal(t, c.want, got,
			"includeSubagents=%v colPrefix=%q", c.includeSubagents, c.colPrefix)
	}
	// The method form delegates to the unqualified helper.
	assert.Equal(t,
		RelationshipExclusionSQL(true, ""),
		AnalyticsFilter{IncludeSubagents: true}.RelationshipExclusionSQL(),
		"method must match the free function")
}

// TestAnalyticsSubagentScope verifies the two-bucket rule for subagent
// sessions (e.g. workflow subagents):
//   - Sum/count surfaces (summary) COUNT subagents: their output tokens
//     and messages are real, independent spend in separate transcripts.
//   - Distribution surfaces (session-shape) stay ROOT-ONLY so the many
//     short subagent sessions do not skew length/duration distributions.
//
// Fork rows stay excluded on both surfaces because their tokens overlap
// their root session and would double-count.
func TestAnalyticsSubagentScope(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	withTokens := func(rel string, msgs, userMsgs, tokens int) func(*Session) {
		return func(s *Session) {
			s.StartedAt = new("2024-06-02T10:00:00Z")
			s.EndedAt = new("2024-06-02T11:00:00Z")
			s.MessageCount = msgs
			s.UserMessageCount = userMsgs
			s.RelationshipType = rel
			s.TotalOutputTokens = tokens
			s.HasTotalOutputTokens = true
		}
	}

	// Root session: 10 msgs, multi-turn, 1000 output tokens.
	insertSession(t, d, "root", "project-alpha", withTokens("", 10, 3, 1000))
	// Subagent: ONE-SHOT (1 user msg), 400 tokens. Workflow subagents
	// are always one-shot; it must still be counted in aggregates.
	insertSession(t, d, "agent-x", "project-alpha",
		withTokens("subagent", 4, 1, 400))
	// Fork: shares the root conversation — excluded everywhere.
	insertSession(t, d, "root-fork", "project-alpha",
		withTokens("fork", 6, 1, 600))

	// Default filter excludes one-shot sessions, matching the summary
	// endpoint default. The one-shot subagent must survive this.
	f := baseFilter()
	f.ExcludeOneShot = true
	s := mustSummary(t, d, ctx, f)

	// Summary counts the one-shot subagent; fork stays excluded.
	assert.Equal(t, 1400, s.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
	assert.Equal(t, 14, s.TotalMessages, "TotalMessages")

	// Session-shape is a distribution surface: root only. The subagent
	// is excluded, so the count is 1 (just the root; fork also excluded).
	shape, err := d.GetAnalyticsSessionShape(ctx, f)
	require.NoError(t, err, "GetAnalyticsSessionShape")
	assert.Equal(t, 1, shape.Count, "session-shape stays root-only")
}

func TestAnalyticsModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "model-a", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "model-a", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "model-a", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T09:06:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "model-b", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T11:00:00Z")
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.MessageCount = 3
		s.Agent = "codex"
	})
	insertMessages(t, d,
		Message{
			SessionID: "model-b", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T11:05:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "model-b", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T11:06:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "model-b", Ordinal: 2, Role: "user",
			Content: "follow-up", ContentLength: 9,
			Timestamp: "2024-06-01T11:07:00Z",
			Model:     "gpt-4o",
		},
	)

	f := AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
	}

	t.Run("SingleModel", func(t *testing.T) {
		ff := f
		ff.Model = "gpt-4o"
		s := mustSummary(t, d, ctx, ff)
		assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
		assert.Equal(t, 3, s.TotalMessages, "TotalMessages")
		assert.Equal(t, []string{"gpt-4o"}, s.Models, "Models")
	})

	t.Run("MultiModel", func(t *testing.T) {
		ff := f
		ff.Model = "gpt-4o, claude-3-5-sonnet"
		s := mustSummary(t, d, ctx, ff)
		assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
		assert.Equal(t,
			[]string{"claude-3-5-sonnet", "gpt-4o"},
			s.Models,
			"Models",
		)
	})
}

func TestAnalyticsModelFilterGoTimePath(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "dst-claude", "proj", func(s *Session) {
		s.StartedAt = new("2026-03-10T14:00:00Z")
		s.EndedAt = new("2026-03-10T14:30:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "dst-claude", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2026-03-10T14:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "dst-claude", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2026-03-10T14:06:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "dst-gpt", "proj", func(s *Session) {
		s.StartedAt = new("2026-03-05T15:00:00Z")
		s.EndedAt = new("2026-03-05T15:30:00Z")
		s.MessageCount = 2
		s.Agent = "codex"
	})
	insertMessages(t, d,
		Message{
			SessionID: "dst-gpt", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2026-03-05T15:05:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "dst-gpt", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2026-03-05T15:06:00Z",
			Model:     "gpt-4o",
		},
	)

	hour := 10
	s, err := d.GetAnalyticsSummary(ctx, AnalyticsFilter{
		From:     "2026-03-01",
		To:       "2026-03-31",
		Timezone: "America/New_York",
		Model:    "gpt-4o",
		Hour:     &hour,
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
	assert.Equal(t, []string{"gpt-4o"}, s.Models, "Models")
}

func TestAnalyticsSummaryModelFilterCountsOnlyMatchingMessages(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "summary-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "summary-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "summary-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp: "2024-06-01T09:06:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	resp := mustSummary(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, 1, resp.TotalMessages, "TotalMessages")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
	assert.Equal(t, 1.0, resp.AvgMessages, "AvgMessages")
	assert.Equal(t, 1, resp.MedianMessages, "MedianMessages")
	assert.Equal(t, 1, resp.P90Messages, "P90Messages")
	require.Contains(t, resp.Agents, "mixed")
	assert.Equal(t, 1, resp.Agents["mixed"].Messages, "AgentMessages")
}

func TestAnalyticsSummaryModelsRespectHourFilterSQLiteSQL(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "hour-a", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:30:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "hour-a", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "hour-b", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
	})
	insertMessages(t, d,
		Message{
			SessionID: "hour-b", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T10:05:00Z",
			Model:     "gpt-4o",
		},
	)

	hour := 9
	s := mustSummary(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Hour: &hour,
	})
	assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
	assert.Equal(t,
		[]string{"claude-3-5-sonnet"},
		s.Models,
		"Models",
	)
}

func TestAnalyticsSummaryModelsRespectHourFilterGoPath(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "ktm-a", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T04:15:00Z")
		s.EndedAt = new("2024-06-01T04:45:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "ktm-a", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T04:15:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "ktm-b", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T05:15:00Z")
		s.EndedAt = new("2024-06-01T05:45:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
	})
	insertMessages(t, d,
		Message{
			SessionID: "ktm-b", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T05:15:00Z",
			Model:     "gpt-4o",
		},
	)

	hour := 10
	s := mustSummary(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01",
		Timezone: "Asia/Kathmandu",
		Hour:     &hour,
	})
	assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
	assert.Equal(t,
		[]string{"claude-3-5-sonnet"},
		s.Models,
		"Models",
	)
}

func TestAnalyticsSummaryModelsUseMatchingHourRowsOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "summary-hour-mixed", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "summary-hour-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "summary-hour-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp: "2024-06-01T10:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	hour := 9
	resp := mustSummary(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Hour: &hour,
	})
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
}

func TestAnalyticsFilterMachineMultiSelect(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for _, sess := range []struct {
		id      string
		machine string
	}{
		{"machine-a", "laptop"},
		{"machine-b", "server"},
		{"machine-c", "desktop"},
	} {
		insertSession(t, d, sess.id, "project", func(s *Session) {
			s.Machine = sess.machine
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.EndedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 4
		})
	}

	f := baseFilter()
	f.Machine = "laptop,server"
	s := mustSummary(t, d, ctx, f)
	require.Equal(t, 2, s.TotalSessions, "TotalSessions")
}

// TestAnalyticsFilterAgentMultiSelectTrimsWhitespace guards backend
// parity: a comma-separated agent filter with surrounding spaces must
// match every listed agent, the same way the PostgreSQL and DuckDB
// analytics paths trim CSV values. Before the fix the SQLite path split
// on "," without trimming, so "claude, codex" matched only claude.
func TestAnalyticsFilterAgentMultiSelectTrimsWhitespace(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	for _, sess := range []struct {
		id    string
		agent string
	}{
		{"agent-a", "claude"},
		{"agent-b", "codex"},
		{"agent-c", "gemini"},
	} {
		insertSession(t, d, sess.id, "project", func(s *Session) {
			s.Agent = sess.agent
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.EndedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 4
		})
	}

	f := baseFilter()
	f.Agent = "claude, codex"
	s := mustSummary(t, d, ctx, f)
	require.Equal(t, 2, s.TotalSessions, "TotalSessions")
}

func TestGetAnalyticsActivity(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	stats := seedAnalyticsData(t, d)

	t.Run("DayGranularity", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "day")
		assert.Equal(t, "day", resp.Granularity, "Granularity")
		require.Len(t, resp.Series, stats.ActiveDays, "len(Series)")
		// Day 1: 2 sessions (a1, a2)
		assert.Equal(t, 2, resp.Series[0].Sessions, "Day1 sessions")
	})

	t.Run("WeekGranularity", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "week")
		// 2024-06-01 is Saturday, 2024-06-03 is Monday
		// So we expect 2 weeks: week of May 27 and week of Jun 3
		assert.Equal(t, 2, len(resp.Series), "len(Series)")
	})

	t.Run("MonthGranularity", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "month")
		assert.Equal(t, 1, len(resp.Series), "len(Series)")
		assert.Equal(t, stats.TotalSessions, resp.Series[0].Sessions, "month sessions")
	})

	t.Run("HasRoleCounts", func(t *testing.T) {
		resp := mustActivity(t, d, ctx, baseFilter(), "day")
		totalUser := 0
		totalAsst := 0
		for _, e := range resp.Series {
			totalUser += e.UserMessages
			totalAsst += e.AssistantMessages
		}
		assert.Equal(t, stats.TotalMessages, totalUser+totalAsst, "total messages")
		assert.Equal(t, stats.TotalUserMessages, totalUser, "total user messages")
		assert.Equal(t, stats.TotalAssistantMessages, totalAsst, "total assistant messages")
	})
}

func TestGetAnalyticsActivityModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-a", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-a", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "activity-a", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T09:01:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "activity-b", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "codex"
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-b", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "activity-b", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:01:00Z",
			Model:     "gpt-4o",
		},
	)

	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 2, resp.Series[0].Messages, "Messages")
}

func TestGetAnalyticsActivityModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-mixed", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-mixed", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "activity-mixed", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 0, resp.Series[0].AssistantMessages,
		"AssistantMessages")
}

func TestGetAnalyticsActivityModelFilterKeepsNullTimestampSessionsWithoutTimeFilter(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-null-ts", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "gpt"
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-null-ts", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "",
		},
		Message{
			SessionID: "activity-null-ts", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Model: "gpt-4o",
		},
	)

	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 2, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 1, resp.Series[0].AssistantMessages,
		"AssistantMessages")
}

func TestGetAnalyticsActivityModelAndHourFilterUseSameMessage(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-time", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-time", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "activity-time", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	hour := 10
	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	assert.Empty(t, resp.Series, "Series")
}

func TestGetAnalyticsActivityModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-paired-hour", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "activity-paired-hour", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			// Empty model: paired with the gpt-4o assistant at 10:00. Filtering
			// by hour 9 must keep the session via this paired user turn, even
			// though the model-bearing assistant sits in hour 10.
		},
		Message{
			SessionID: "activity-paired-hour", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o",
		},
	)

	hour := 9
	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 0, resp.Series[0].AssistantMessages,
		"AssistantMessages")
}

func TestGetAnalyticsHeatmapSessionsModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "heatmap-sessions-paired", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "heatmap-sessions-paired", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
		},
		Message{
			SessionID: "heatmap-sessions-paired", Ordinal: 1,
			Role: "assistant", Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o",
		},
	)

	// Empty-model user turn at hour 9 paired with a gpt-4o assistant at hour
	// 10. The sessions heatmap must keep the session via the paired user turn.
	hour := 9
	resp := mustHeatmap(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "sessions")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 1, resp.Entries[0].Value, "Value")
}

func TestGetAnalyticsTopSessionsDurationModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "top-duration-paired", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "top-duration-paired", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
		},
		Message{
			SessionID: "top-duration-paired", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o",
		},
	)

	// Empty-model user turn at hour 9 paired with a gpt-4o assistant at hour
	// 10. Ranking by duration under the gpt-4o + hour-9 filter must keep the
	// session via the paired user turn.
	hour := 9
	resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "duration")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "top-duration-paired", resp.Sessions[0].ID, "ID")
}

func TestGetAnalyticsActivityModelAndHourFilterCountsOnlyMatchingHourRows(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "activity-hour-gpt", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 0
		s.Agent = "claude"
	})

	m1 := asstMsgAt("activity-hour-gpt", 0, "[Read: a.go]", "2024-06-01T09:00:00Z")
	m1.Model = "gpt-4o"
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "activity-hour-gpt", ToolName: "Read", Category: "Read"},
		{SessionID: "activity-hour-gpt", ToolName: "Bash", Category: "Bash"},
	}

	m2 := asstMsgAt("activity-hour-gpt", 1, "[Grep: b.go]", "2024-06-01T10:00:00Z")
	m2.Model = "gpt-4o"
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "activity-hour-gpt", ToolName: "Grep", Category: "Grep"},
	}

	insertMessages(t, d, m1, m2)

	hour := 10
	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].AssistantMessages,
		"AssistantMessages")
	assert.Equal(t, 1, resp.Series[0].ToolCalls, "ToolCalls")
}

func TestGetAnalyticsHeatmap(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	stats := seedAnalyticsData(t, d)

	t.Run("MessageMetric", func(t *testing.T) {
		resp := mustHeatmap(t, d, ctx, baseFilter(), "messages")
		assert.Equal(t, "messages", resp.Metric, "Metric")
		// 3 days in range: Jun 1, 2, 3
		require.Len(t, resp.Entries, stats.ActiveDays, "len(Entries)")

		totalMessages := 0
		for _, e := range resp.Entries {
			totalMessages += e.Value
		}
		assert.Equal(t, stats.TotalMessages, totalMessages, "total messages across heatmap")

		// Jun 1: 10+20=30, Jun 2: 30+15=45, Jun 3: 5
		assert.Equal(t, 30, resp.Entries[0].Value, "Jun1 value")
		assert.Equal(t, 45, resp.Entries[1].Value, "Jun2 value")
		assert.Equal(t, 5, resp.Entries[2].Value, "Jun3 value")
	})

	t.Run("SessionMetric", func(t *testing.T) {
		resp := mustHeatmap(t, d, ctx, baseFilter(), "sessions")
		assert.Equal(t, "sessions", resp.Metric, "Metric")

		totalSessions := 0
		for _, e := range resp.Entries {
			totalSessions += e.Value
		}
		assert.Equal(t, stats.TotalSessions, totalSessions, "total sessions across heatmap")

		// Jun 1: 2, Jun 2: 2, Jun 3: 1
		assert.Equal(t, 2, resp.Entries[0].Value, "Jun1 sessions")
	})

	t.Run("LevelsAssigned", func(t *testing.T) {
		resp := mustHeatmap(t, d, ctx, baseFilter(), "messages")
		// All entries should have levels 0-4
		for _, e := range resp.Entries {
			assert.GreaterOrEqual(t, e.Level, 0,
				"date %s level", e.Date)
			assert.LessOrEqual(t, e.Level, 4,
				"date %s level", e.Date)
		}
	})

	t.Run("OutputTokensNoReporting", func(t *testing.T) {
		// When no sessions report token coverage, the
		// output_tokens heatmap must return empty entries
		// rather than a zero-filled date grid.
		resp := mustHeatmap(
			t, d, ctx, baseFilter(), "output_tokens",
		)
		assert.Equal(t, "output_tokens", resp.Metric, "Metric")
		assert.Empty(t, resp.Entries,
			"len(Entries) want 0 (no sessions report token coverage)")
	})

	t.Run("EmptyRange", func(t *testing.T) {
		f := emptyFilter()
		f.To = "2020-01-03"
		resp := mustHeatmap(t, d, ctx, f, "messages")
		require.Len(t, resp.Entries, 3, "len(Entries) =")
		for _, e := range resp.Entries {
			assert.Equal(t, 0, e.Value, "date %s value", e.Date)
			assert.Equal(t, 0, e.Level, "date %s level", e.Date)
		}
	})
}

func TestGetAnalyticsSummaryModelFilterUsesFilteredOutputTokens(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "summary-output-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
		s.TotalOutputTokens = 111
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "summary-output-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o", OutputTokens: 11, HasOutputTokens: true,
		},
		Message{
			SessionID: "summary-output-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp:    "2024-06-01T10:05:00Z",
			Model:        "claude-3-5-sonnet",
			OutputTokens: 100, HasOutputTokens: true,
		},
	)

	insertSession(t, d, "summary-output-uncovered", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:40:00Z")
		s.EndedAt = new("2024-06-01T11:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
		s.TotalOutputTokens = 90
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "summary-output-uncovered", Ordinal: 0,
			Role:    "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T10:40:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "summary-output-uncovered", Ordinal: 1,
			Role:    "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp:    "2024-06-01T10:45:00Z",
			Model:        "claude-3-5-sonnet",
			OutputTokens: 90, HasOutputTokens: true,
		},
	)

	hour := 10
	resp := mustSummary(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	})
	assert.Equal(t, 2, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, 2, resp.TotalMessages, "TotalMessages")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
	assert.Equal(t, 11, resp.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 1, resp.TokenReportingSessions, "TokenReportingSessions")
}

func TestGetAnalyticsHeatmapModelFilterUsesFilteredOutputTokens(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "heatmap-output-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
		s.TotalOutputTokens = 111
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "heatmap-output-mixed", Ordinal: 0,
			Role:    "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o", OutputTokens: 11, HasOutputTokens: true,
		},
		Message{
			SessionID: "heatmap-output-mixed", Ordinal: 1,
			Role:    "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp:    "2024-06-01T10:05:00Z",
			Model:        "claude-3-5-sonnet",
			OutputTokens: 100, HasOutputTokens: true,
		},
	)

	insertSession(t, d, "heatmap-output-uncovered", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:40:00Z")
		s.EndedAt = new("2024-06-01T11:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
		s.TotalOutputTokens = 90
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "heatmap-output-uncovered", Ordinal: 0,
			Role:    "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T10:40:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "heatmap-output-uncovered", Ordinal: 1,
			Role:    "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp:    "2024-06-01T10:45:00Z",
			Model:        "claude-3-5-sonnet",
			OutputTokens: 90, HasOutputTokens: true,
		},
	)

	hour := 10
	resp := mustHeatmap(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "output_tokens")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 11, resp.Entries[0].Value, "Value")
}

func TestGetAnalyticsProjects(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	stats := seedAnalyticsData(t, d)

	t.Run("FullRange", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, baseFilter())
		require.Len(t, resp.Projects, stats.ActiveProjects, "len(Projects)")

		totalMessages := 0
		for _, p := range resp.Projects {
			totalMessages += p.Messages
		}
		assert.Equal(t, stats.TotalMessages, totalMessages, "total messages across projects")

		// Sorted by message count desc: beta (45) > alpha (35)
		assert.Equal(t, "project-beta", resp.Projects[0].Name, "first project")
		assert.Equal(t, 45, resp.Projects[0].Messages, "beta messages")
		assert.Equal(t, "project-alpha", resp.Projects[1].Name, "second project")
		assert.Equal(t, 3, resp.Projects[1].Sessions, "alpha sessions")
	})

	t.Run("AgentBreakdown", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, baseFilter())
		alpha := resp.Projects[1]
		assert.Equal(t, 2, alpha.Agents["claude"], "alpha claude")
		assert.Equal(t, 1, alpha.Agents["codex"], "alpha codex")
	})

	t.Run("MedianMessages", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, baseFilter())
		// Alpha counts sorted: [5, 10, 20], median = 10
		alpha := resp.Projects[1]
		assert.Equal(t, 10, alpha.MedianMessages, "alpha median")
	})

	t.Run("EmptyRange", func(t *testing.T) {
		resp := mustProjects(t, d, ctx, emptyFilter())
		assert.Equal(t, 0, len(resp.Projects), "len(Projects)")
	})
}

func TestGetAnalyticsProjectsModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "projects-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "projects-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "projects-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp: "2024-06-01T09:06:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	resp, err := d.GetAnalyticsProjects(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsProjects")
	require.Len(t, resp.Projects, 1, "len(Projects)")
	assert.Equal(t, 1, resp.Projects[0].Messages, "Messages")
	assert.Equal(t, 1.0, resp.Projects[0].AvgMessages, "AvgMessages")
	assert.Equal(t, 1, resp.Projects[0].MedianMessages, "MedianMessages")
	assert.Equal(t, 1.0, resp.Projects[0].DailyTrend, "DailyTrend")
}

func TestGetAnalyticsHeatmapModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "heatmap-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "heatmap-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "heatmap-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp: "2024-06-01T09:06:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	resp := mustHeatmap(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "messages")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 1, resp.Entries[0].Value, "Value")
}

func TestMedianInt(t *testing.T) {
	tests := []struct {
		name   string
		sorted []int
		want   int
	}{
		{"Empty", []int{}, 0},
		{"Single", []int{5}, 5},
		{"OddCount", []int{1, 3, 7}, 3},
		{"EvenCount", []int{1, 3, 7, 9}, 5},
		{"EvenCountTwo", []int{10, 20}, 15},
		{"EvenCountFour", []int{2, 4, 6, 8}, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := medianInt(tt.sorted, len(tt.sorted))
			assert.Equal(t, tt.want, got,
				"medianInt(%v)", tt.sorted)
		})
	}
}

func TestLocalDate(t *testing.T) {
	utc := time.UTC

	tests := []struct {
		name string
		ts   string
		want string
	}{
		{"RFC3339", "2024-06-01T15:00:00Z", "2024-06-01"},
		{"RFC3339Nano", "2024-06-01T15:00:00.123Z", "2024-06-01"},
		{"NoFraction", "2024-06-01T15:00:00Z", "2024-06-01"},
		{"Fallback10Char", "2024-06-01", "2024-06-01"},
		{"Short", "2024", ""},
		{"Empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := localDate(tt.ts, utc)
			assert.Equal(t, tt.want, got,
				"localDate(%q)", tt.ts)
		})
	}
}

func TestMostActiveTieBreak(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Two projects with equal message counts
	insertSession(t, d, "t1", "zebra", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 20
		s.Agent = "claude"
	})
	insertSession(t, d, "t2", "alpha", func(s *Session) {
		s.StartedAt = new(tsMidYear)
		s.MessageCount = 20
		s.Agent = "claude"
	})

	f := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-01",
		Timezone: "UTC",
	}
	s := mustSummary(t, d, ctx, f)

	// Alphabetically, "alpha" < "zebra"
	assert.Equal(t, "alpha", s.MostActive,
		"MostActive want alphabetically first (alpha)")
}

func TestEvenCountMedianInSummary(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// 4 sessions: message counts [5, 10, 20, 30]
	for i, mc := range []int{10, 30, 5, 20} {
		id := fmt.Sprintf("e%d", i)
		insertSession(t, d, id, "proj", func(s *Session) {
			ts := fmt.Sprintf("2024-06-01T%02d:00:00Z", i+9)
			s.StartedAt = &ts
			s.MessageCount = mc
			s.Agent = "claude"
		})
	}

	f := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-01",
		Timezone: "UTC",
	}
	s := mustSummary(t, d, ctx, f)

	// Sorted: [5, 10, 20, 30] → median = (10+20)/2 = 15
	assert.Equal(t, 15, s.MedianMessages, "MedianMessages")
}

func TestAnalyticsTimezone(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session at 2024-06-01T23:00:00Z = 2024-06-02 in UTC+5
	insertSession(t, d, "tz1", "tz-project", func(s *Session) {
		s.StartedAt = new("2024-06-01T23:00:00Z")
		s.MessageCount = 10
		s.Agent = "claude"
	})
	insertMessages(t, d, userMsg("tz1", 0, "late night"))

	t.Run("UTCBucket", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-02",
			Timezone: "UTC",
		}
		resp := mustHeatmap(t, d, ctx, f, "messages")
		// In UTC, this is Jun 1
		assert.Equal(t, 10, resp.Entries[0].Value, "Jun1 UTC value")
		assert.Equal(t, 0, resp.Entries[1].Value, "Jun2 UTC value")
	})

	t.Run("PlusFiveBucket", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-02",
			Timezone: "Asia/Karachi", // UTC+5
		}
		resp := mustHeatmap(t, d, ctx, f, "messages")
		// In UTC+5, 23:00Z = 04:00 Jun 2
		assert.Equal(t, 0, resp.Entries[0].Value, "Jun1 PKT value")
		assert.Equal(t, 10, resp.Entries[1].Value, "Jun2 PKT value")
	})
}

func TestAnalyticsCanceledContext(t *testing.T) {
	d := testDB(t)
	ctx := canceledCtx()

	f := baseFilter()

	tests := []struct {
		name string
		fn   func() error
	}{
		{"Summary", func() error {
			_, err := d.GetAnalyticsSummary(ctx, f)
			return err
		}},
		{"Activity", func() error {
			_, err := d.GetAnalyticsActivity(ctx, f, "day")
			return err
		}},
		{"Heatmap", func() error {
			_, err := d.GetAnalyticsHeatmap(ctx, f, "messages")
			return err
		}},
		{"Projects", func() error {
			_, err := d.GetAnalyticsProjects(ctx, f)
			return err
		}},
		{"HourOfWeek", func() error {
			_, err := d.GetAnalyticsHourOfWeek(ctx, f)
			return err
		}},
		{"SessionShape", func() error {
			_, err := d.GetAnalyticsSessionShape(ctx, f)
			return err
		}},
		{"Velocity", func() error {
			_, err := d.GetAnalyticsVelocity(ctx, f)
			return err
		}},
		{"Tools", func() error {
			_, err := d.GetAnalyticsTools(ctx, f)
			return err
		}},
		{"TopSessions", func() error {
			_, err := d.GetAnalyticsTopSessions(
				ctx, f, "messages",
			)
			return err
		}},
		{"Signals", func() error {
			_, err := d.GetAnalyticsSignals(ctx, f)
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireCanceledErr(t, tt.fn())
		})
	}
}

func TestConcentrationTopThree(t *testing.T) {
	ctx := context.Background()

	t.Run("OneProject", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "c1", "solo", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 50
			s.Agent = "claude"
		})
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		assert.Equal(t, 1.0, s.Concentration, "Concentration")
	})

	t.Run("TwoProjects", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "c1", "alpha", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 35
			s.Agent = "claude"
		})
		insertSession(t, d, "c2", "beta", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 45
			s.Agent = "claude"
		})
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		// Both in top 3 → concentration = 1.0
		assert.Equal(t, 1.0, s.Concentration, "Concentration")
	})

	t.Run("FourProjects", func(t *testing.T) {
		d := testDB(t)
		for i, tc := range []struct {
			proj string
			msgs int
		}{
			{"p1", 40}, {"p2", 30}, {"p3", 20}, {"p4", 10},
		} {
			id := fmt.Sprintf("c%d", i)
			insertSession(t, d, id, tc.proj, func(s *Session) {
				ts := fmt.Sprintf(
					"2024-06-01T%02d:00:00Z", i+9,
				)
				s.StartedAt = &ts
				s.MessageCount = tc.msgs
				s.Agent = "claude"
			})
		}
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
		}
		s := mustSummary(t, d, ctx, f)
		// Top 3: 40+30+20 = 90, total = 100
		assert.Equal(t, 0.9, s.Concentration, "Concentration")
	})
}

func TestGetAnalyticsHourOfWeek(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsHourOfWeek(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsHourOfWeek")
		assert.Len(t, resp.Cells, 168, "len(Cells)")
		for _, c := range resp.Cells {
			assert.Equal(t, 0, c.Messages,
				"day=%d hour=%d messages",
				c.DayOfWeek, c.Hour)
		}
	})

	// Seed sessions with known UTC times:
	// 2024-06-01 is Saturday, 09:00 UTC
	insertSession(t, d, "hw1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "hw1", Ordinal: 0, Role: "user",
			Content: "hi", ContentLength: 2,
			Timestamp: "2024-06-01T09:00:00Z",
		},
		Message{
			SessionID: "hw1", Ordinal: 1, Role: "assistant",
			Content: "hello", ContentLength: 5,
			Timestamp: "2024-06-01T09:30:00Z",
		},
	)

	// 23:00 UTC on a Saturday
	insertSession(t, d, "hw2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T23:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d, Message{
		SessionID: "hw2", Ordinal: 0, Role: "user",
		Content: "late", ContentLength: 4,
		Timestamp: "2024-06-01T23:00:00Z",
	})

	t.Run("UTCBucketing", func(t *testing.T) {
		resp, err := d.GetAnalyticsHourOfWeek(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsHourOfWeek")
		// Saturday = ISO day 5 (Mon=0)
		// hour 9: 2 messages (user@09:00 + assistant@09:30)
		satH9 := findHOWCell(resp.Cells, 5, 9)
		assert.Equal(t, 2, satH9, "Sat 09:xx")
		satH23 := findHOWCell(resp.Cells, 5, 23)
		assert.Equal(t, 1, satH23, "Sat 23:00")
	})

	t.Run("TimezoneShift", func(t *testing.T) {
		f := AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-03",
			Timezone: "Asia/Karachi", // UTC+5
		}
		resp, err := d.GetAnalyticsHourOfWeek(ctx, f)
		require.NoError(t, err, "GetAnalyticsHourOfWeek")
		// 23:00 UTC Sat → 04:00 Sun in UTC+5
		// Sunday = ISO day 6
		sunH4 := findHOWCell(resp.Cells, 6, 4)
		assert.Equal(t, 1, sunH4, "Sun 04:00 PKT")
		// 09:00 UTC Sat → 14:00 Sat in UTC+5
		// 09:30 UTC Sat → 14:30 Sat in UTC+5
		// Both fall in hour 14
		satH14 := findHOWCell(resp.Cells, 5, 14)
		assert.Equal(t, 2, satH14, "Sat 14:xx PKT")
	})
}

func TestGetAnalyticsHourOfWeekModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "how-a", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:30:00Z")
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "how-a", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "how-b", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 1
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "how-b", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o",
		},
	)

	resp, err := d.GetAnalyticsHourOfWeek(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 0, findHOWCell(resp.Cells, 5, 9), "Sat 09:00")
	assert.Equal(t, 1, findHOWCell(resp.Cells, 5, 10), "Sat 10:00")
}

func TestGetAnalyticsHourOfWeekModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "how-mixed", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "how-mixed", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "how-mixed", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	resp, err := d.GetAnalyticsHourOfWeek(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 1, findHOWCell(resp.Cells, 5, 9), "Sat 09:00")
	assert.Equal(t, 0, findHOWCell(resp.Cells, 5, 10), "Sat 10:00")
}

func TestGetAnalyticsHourOfWeekModelFilterIncludesPairedUserTurns(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "how-paired", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
	})
	insertMessages(t, d,
		Message{
			SessionID: "how-paired", Ordinal: 0, Role: "user",
			Content: "q", ContentLength: 1,
			Timestamp: "2024-06-01T09:00:00Z",
			// Empty model: this user turn is paired with the selected-model
			// assistant below, so the heatmap must count it like the summary,
			// activity, velocity, and trends panels do.
		},
		Message{
			SessionID: "how-paired", Ordinal: 1, Role: "assistant",
			Content: "a", ContentLength: 1,
			Timestamp: "2024-06-01T10:00:00Z",
			Model:     "gpt-4o",
		},
	)

	resp, err := d.GetAnalyticsHourOfWeek(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 1, findHOWCell(resp.Cells, 5, 9),
		"paired empty-model user turn at Sat 09:00")
	assert.Equal(t, 1, findHOWCell(resp.Cells, 5, 10),
		"selected-model assistant at Sat 10:00")
}

func findHOWCell(cells []HourOfWeekCell, dow, hour int) int {
	for _, c := range cells {
		if c.DayOfWeek == dow && c.Hour == hour {
			return c.Messages
		}
	}
	return -1
}

func TestGetAnalyticsSessionShape(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, baseFilter(),
		)
		require.NoError(t, err, "GetAnalyticsSessionShape")
		assert.Equal(t, 0, resp.Count, "Count")
	})

	// Session with 10 messages, 1h duration
	insertSession(t, d, "ss1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 10
		s.Agent = "claude"
	})
	// 5 user + 5 assistant, assistant has tool_use
	for i := range 10 {
		role := "user"
		hasTool := false
		if i%2 == 1 {
			role = "assistant"
			hasTool = true
		}
		insertMessages(t, d, Message{
			SessionID: "ss1", Ordinal: i, Role: role,
			Content:       fmt.Sprintf("msg %d", i),
			ContentLength: 10, HasToolUse: hasTool,
			Timestamp: "2024-06-01T09:00:00Z",
		})
	}

	// Session with 25 messages, no ended_at
	insertSession(t, d, "ss2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 25
		s.Agent = "claude"
	})
	for i := range 25 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		insertMessages(t, d, Message{
			SessionID: "ss2", Ordinal: i, Role: role,
			Content:       fmt.Sprintf("msg %d", i),
			ContentLength: 10,
			Timestamp:     "2024-06-02T10:00:00Z",
		})
	}

	t.Run("FullRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, baseFilter(),
		)
		require.NoError(t, err, "GetAnalyticsSessionShape")
		assert.Equal(t, 2, resp.Count, "Count")

		// Length: 10 → "6-15", 25 → "16-30"
		lenMap := bucketMap(resp.LengthDistribution)
		assert.Equal(t, 1, lenMap["6-15"], "6-15")
		assert.Equal(t, 1, lenMap["16-30"], "16-30")

		// Duration: only ss1 has both start/end (60m → "1-2h")
		durMap := bucketMap(resp.DurationDistribution)
		assert.Equal(t, 1, durMap["1-2h"], "1-2h")
		totalDur := 0
		for _, b := range resp.DurationDistribution {
			totalDur += b.Count
		}
		assert.Equal(t, 1, totalDur, "total duration entries")
	})

	t.Run("Autonomy", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, baseFilter(),
		)
		require.NoError(t, err, "GetAnalyticsSessionShape")
		// ss1: 5 user, 5 assistant w/ tool → ratio 5/5=1.0 → "1-2"
		// ss2: 13 user, 0 tool → ratio 0/13=0 → "<0.5"
		autoMap := bucketMap(resp.AutonomyDistribution)
		assert.Equal(t, 1, autoMap["1-2"], "1-2")
		assert.Equal(t, 1, autoMap["<0.5"], "<0.5")
	})

	t.Run("EmptyRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSessionShape(
			ctx, emptyFilter(),
		)
		require.NoError(t, err, "GetAnalyticsSessionShape")
		assert.Equal(t, 0, resp.Count, "Count")
	})
}

func TestGetAnalyticsSessionShapeModelFilterUsesMatchingRowsOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "shape-model-filter", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:30:00Z")
		s.MessageCount = 6
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "shape-model-filter", Ordinal: 0, Role: "user",
			Content: "gpt q", ContentLength: 5,
			Timestamp: "2024-06-01T09:00:00Z",
		},
		Message{
			SessionID: "shape-model-filter", Ordinal: 1, Role: "assistant",
			Content: "gpt tool", ContentLength: 8,
			Timestamp:  "2024-06-01T09:01:00Z",
			Model:      "gpt-4o",
			HasToolUse: true,
			ToolCalls: []ToolCall{
				{SessionID: "shape-model-filter", ToolName: "Read", Category: "Read"},
			},
		},
		Message{
			SessionID: "shape-model-filter", Ordinal: 2, Role: "user",
			Content: "claude q1", ContentLength: 9,
			Timestamp: "2024-06-01T09:02:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "shape-model-filter", Ordinal: 3, Role: "user",
			Content: "claude q2", ContentLength: 9,
			Timestamp: "2024-06-01T09:03:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "shape-model-filter", Ordinal: 4, Role: "user",
			Content: "claude q3", ContentLength: 9,
			Timestamp: "2024-06-01T09:04:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "shape-model-filter", Ordinal: 5, Role: "assistant",
			Content: "claude reply", ContentLength: 12,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	resp, err := d.GetAnalyticsSessionShape(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSessionShape")
	assert.Equal(t, 1, resp.Count, "Count")

	lenMap := bucketMap(resp.LengthDistribution)
	assert.Equal(t, 1, lenMap["1-5"], "filtered 2-message session stays in 1-5")
	assert.Equal(t, 0, lenMap["6-15"], "full-session count must not leak")

	autoMap := bucketMap(resp.AutonomyDistribution)
	assert.Equal(t, 1, autoMap["1-2"], "filtered autonomy bucket")
	assert.Equal(t, 0, autoMap["<0.5"], "off-model user turns must not leak")
}

func bucketMap(
	buckets []DistributionBucket,
) map[string]int {
	m := make(map[string]int)
	for _, b := range buckets {
		m[b.Label] = b.Count
	}
	return m
}

func assertEq[T comparable](t *testing.T, name string, got, want T) {
	t.Helper()
	assert.Equal(t, want, got, name)
}

type testClock struct {
	curr time.Time
}

func newTestClock(start string) *testClock {
	t, _ := time.Parse(time.RFC3339, start)
	return &testClock{curr: t}
}

func (c *testClock) Now() string {
	return c.curr.Format(time.RFC3339)
}

func (c *testClock) Next(d time.Duration) string {
	c.curr = c.curr.Add(d)
	return c.Now()
}

func insertConversation(t *testing.T, d *DB, id, proj, agent, start string, delays []time.Duration) {
	t.Helper()
	clock := newTestClock(start)

	insertSession(t, d, id, proj, func(s *Session) {
		s.StartedAt = new(start)
		s.MessageCount = len(delays)
		s.Agent = agent
		if len(delays) > 0 {
			endClock := newTestClock(start)
			for _, delay := range delays {
				endClock.Next(delay)
			}
			s.EndedAt = new(endClock.Now())
		}
	})

	var msgs []Message
	for i, delay := range delays {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, Message{
			SessionID:     id,
			Ordinal:       i,
			Role:          role,
			Content:       fmt.Sprintf("msg %d", i),
			ContentLength: 5,
			Timestamp:     clock.Next(delay),
		})
	}
	if len(msgs) > 0 {
		insertMessages(t, d, msgs...)
	}
}

func TestGetAnalyticsVelocity_Metrics(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 0)
	})

	// Session with messages at precise timestamps (10s apart)
	insertConversation(t, d, "v1", "proj", "claude", "2024-06-01T09:00:00Z", []time.Duration{
		0, 10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second, 10 * time.Second,
	})

	t.Run("TurnCycle", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "TurnCycle P50", resp.Overall.TurnCycleSec.P50, 10.0)
	})

	t.Run("FirstResponse", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 10.0)
	})

	t.Run("Throughput", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		// Active time: 5 gaps of 10s = 50s ≈ 0.833 min
		// 6 msgs / 0.833 = ~7.2 msgs/min
		assert.InDelta(t, 7.2, resp.Overall.MsgsPerActiveMin, 0.3,
			"MsgsPerActiveMin want ~7.2")
	})

	t.Run("ByAgent", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 1)
		assertEq(t, "ByAgent[0].Label", resp.ByAgent[0].Label, "claude")
		assertEq(t, "ByAgent[0].Sessions", resp.ByAgent[0].Sessions, 1)
	})

	t.Run("ByComplexity", func(t *testing.T) {
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "len(ByComplexity)", len(resp.ByComplexity), 1)
		assertEq(t, "ByComplexity[0].Label", resp.ByComplexity[0].Label, "1-15")
	})
}

func TestGetAnalyticsVelocity_EdgeCases(t *testing.T) {
	ctx := context.Background()

	t.Run("LargeCycleExcluded", func(t *testing.T) {
		d := testDB(t)
		insertConversation(t, d, "v2", "proj", "claude", "2024-06-01T09:00:00Z", []time.Duration{
			0, 45 * time.Minute,
		})
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "TurnCycle P50", resp.Overall.TurnCycleSec.P50, 0.0)
	})

	t.Run("EmptyTimestampsSkipped", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v3", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v3", Ordinal: 0, Role: "user", Content: "q", ContentLength: 1, Timestamp: ""},
			Message{SessionID: "v3", Ordinal: 1, Role: "assistant", Content: "a", ContentLength: 1, Timestamp: ""},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "TurnCycle P50", resp.Overall.TurnCycleSec.P50, 0.0)
	})

	t.Run("AssistantBeforeUser", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v4", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 3
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v4", Ordinal: 0, Role: "assistant", Content: "system greeting", ContentLength: 15, Timestamp: "2024-06-01T09:00:00Z"},
			Message{SessionID: "v4", Ordinal: 1, Role: "user", Content: "hi", ContentLength: 2, Timestamp: "2024-06-01T09:00:10Z"},
			Message{SessionID: "v4", Ordinal: 2, Role: "assistant", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:20Z"},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 10.0)
	})

	t.Run("OrdinalVsTimestampSkew", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v5", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 3
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v5", Ordinal: 0, Role: "user", Content: "setup", ContentLength: 5, Timestamp: "2024-06-01T09:00:00Z"},
			Message{SessionID: "v5", Ordinal: 1, Role: "user", Content: "real question", ContentLength: 13, Timestamp: "2024-06-01T09:00:30Z"},
			Message{SessionID: "v5", Ordinal: 2, Role: "assistant", Content: "answer", ContentLength: 6, Timestamp: "2024-06-01T09:00:20Z"},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 20.0)
	})

	t.Run("NegativeDeltaClampsToZero", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "v6", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "v6", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:30Z"},
			Message{SessionID: "v6", Ordinal: 1, Role: "assistant", Content: "hi", ContentLength: 2, Timestamp: "2024-06-01T09:00:10Z"},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "FirstResponse P50", resp.Overall.FirstResponseSec.P50, 0.0)
	})
}

func TestGetAnalyticsVelocity_ToolUsage(t *testing.T) {
	ctx := context.Background()

	t.Run("ToolCallsPerActiveMin", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "vt1", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 4
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "vt1", Ordinal: 0, Role: "user", Content: "hi", ContentLength: 2, Timestamp: "2024-06-01T09:00:00Z"},
			Message{SessionID: "vt1", Ordinal: 1, Role: "assistant", Content: "hello", ContentLength: 5, Timestamp: "2024-06-01T09:00:10Z"},
			Message{SessionID: "vt1", Ordinal: 2, Role: "user", Content: "do X", ContentLength: 4, Timestamp: "2024-06-01T09:00:20Z"},
			Message{
				SessionID: "vt1", Ordinal: 3, Role: "assistant", Content: "done", ContentLength: 4,
				Timestamp: "2024-06-01T09:00:30Z", HasToolUse: true,
				ToolCalls: []ToolCall{
					{SessionID: "vt1", ToolName: "Read", Category: "Read"},
					{SessionID: "vt1", ToolName: "Bash", Category: "Bash"},
					{SessionID: "vt1", ToolName: "Edit", Category: "Edit"},
				},
			},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "ToolCallsPerActiveMin", resp.Overall.ToolCallsPerActiveMin, 6.0)
	})

	t.Run("ToolCallsByAgentBreakdown", func(t *testing.T) {
		d := testDB(t)
		insertSession(t, d, "vta1", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T09:00:00Z")
			s.MessageCount = 2
			s.Agent = "claude"
		})
		insertMessages(t, d,
			Message{SessionID: "vta1", Ordinal: 0, Role: "user", Content: "q", ContentLength: 1, Timestamp: "2024-06-01T09:00:00Z"},
			Message{
				SessionID: "vta1", Ordinal: 1, Role: "assistant", Content: "a", ContentLength: 1,
				Timestamp: "2024-06-01T09:00:30Z", HasToolUse: true,
				ToolCalls: []ToolCall{{SessionID: "vta1", ToolName: "Read", Category: "Read"}},
			},
		)
		insertSession(t, d, "vta2", "proj", func(s *Session) {
			s.StartedAt = new("2024-06-01T10:00:00Z")
			s.MessageCount = 2
			s.Agent = "codex"
		})
		insertMessages(t, d,
			Message{SessionID: "vta2", Ordinal: 0, Role: "user", Content: "q", ContentLength: 1, Timestamp: "2024-06-01T10:00:00Z"},
			Message{
				SessionID: "vta2", Ordinal: 1, Role: "assistant", Content: "a", ContentLength: 1,
				Timestamp: "2024-06-01T10:00:30Z", HasToolUse: true,
				ToolCalls: []ToolCall{
					{SessionID: "vta2", ToolName: "Bash", Category: "Bash"},
					{SessionID: "vta2", ToolName: "Edit", Category: "Edit"},
				},
			},
		)
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		require.GreaterOrEqual(t, len(resp.ByAgent), 2,
			"ByAgent entries want >= 2")
		agentMap := make(map[string]VelocityBreakdown)
		for _, b := range resp.ByAgent {
			agentMap[b.Label] = b
		}
		assertEq(t, "claude ToolCallsPerActiveMin", agentMap["claude"].Overview.ToolCallsPerActiveMin, 2.0)
		assertEq(t, "codex ToolCallsPerActiveMin", agentMap["codex"].Overview.ToolCallsPerActiveMin, 4.0)
	})

	t.Run("ToolCallsPerActiveMinZero", func(t *testing.T) {
		d := testDB(t)
		insertConversation(t, d, "vt2", "proj", "claude", "2024-06-01T09:00:00Z", []time.Duration{
			0, 10 * time.Second,
		})
		resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsVelocity")
		assertEq(t, "ToolCallsPerActiveMin", resp.Overall.ToolCallsPerActiveMin, 0.0)
	})
}

func TestGetAnalyticsVelocity_ModelFilterUsesMatchingRowsOnly(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "velocity-model", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:11:00Z")
		s.MessageCount = 4
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "velocity-model", Ordinal: 0, Role: "user",
			Content: "claude q", ContentLength: 8,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "velocity-model", Ordinal: 1, Role: "assistant",
			Content: "offscope-offscope-xx", ContentLength: 20,
			Timestamp:  "2024-06-01T09:00:10Z",
			Model:      "claude-3-5-sonnet",
			HasToolUse: true,
			ToolCalls: []ToolCall{
				{SessionID: "velocity-model", ToolName: "Read", Category: "Read"},
				{SessionID: "velocity-model", ToolName: "Bash", Category: "Bash"},
				{SessionID: "velocity-model", ToolName: "Grep", Category: "Grep"},
			},
		},
		Message{
			SessionID: "velocity-model", Ordinal: 2, Role: "user",
			Content: "gpt q", ContentLength: 5,
			Timestamp: "2024-06-01T09:10:00Z",
		},
		Message{
			SessionID: "velocity-model", Ordinal: 3, Role: "assistant",
			Content: "reply", ContentLength: 5,
			Timestamp:  "2024-06-01T09:11:00Z",
			Model:      "gpt-4o",
			HasToolUse: true,
			ToolCalls: []ToolCall{
				{SessionID: "velocity-model", ToolName: "Edit", Category: "Edit"},
				{SessionID: "velocity-model", ToolName: "Write", Category: "Write"},
			},
		},
	)

	resp, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	assert.Equal(t, 60.0, resp.Overall.FirstResponseSec.P50,
		"FirstResponse P50")
	assert.Equal(t, 2.0, resp.Overall.MsgsPerActiveMin,
		"MsgsPerActiveMin")
	assert.Equal(t, 5.0, resp.Overall.CharsPerActiveMin,
		"CharsPerActiveMin")
	assert.Equal(t, 2.0, resp.Overall.ToolCallsPerActiveMin,
		"ToolCallsPerActiveMin")
}

func TestGetAnalyticsVelocityModelFilterUsesMatchingComplexityBucket(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "velocity-model-complexity", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:15:00Z")
		s.MessageCount = 16
		s.Agent = "mixed"
	})
	msgs := []Message{
		{
			SessionID: "velocity-model-complexity", Ordinal: 0, Role: "user",
			Content: "gpt q", ContentLength: 5,
			Timestamp: "2024-06-01T09:00:00Z",
		},
		{
			SessionID: "velocity-model-complexity", Ordinal: 1, Role: "assistant",
			Content: "reply", ContentLength: 5,
			Timestamp: "2024-06-01T09:01:00Z",
			Model:     "gpt-4o",
		},
	}
	for i := 2; i < 16; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, Message{
			SessionID:     "velocity-model-complexity",
			Ordinal:       i,
			Role:          role,
			Content:       "claude",
			ContentLength: 6,
			Timestamp:     fmt.Sprintf("2024-06-01T09:%02d:00Z", i),
			Model:         "claude-3-5-sonnet",
		})
	}
	insertMessages(t, d, msgs...)

	resp, err := d.GetAnalyticsVelocity(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	require.Len(t, resp.ByComplexity, 1, "len(ByComplexity)")
	assert.Equal(t, "1-15", resp.ByComplexity[0].Label,
		"complexity bucket should use filtered message count")
	assert.Equal(t, 1, resp.ByComplexity[0].Sessions, "Sessions")
}

func TestVelocityChunkedQuery(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Seed >maxSQLVars sessions to exercise chunked IN-clause logic.
	const n = maxSQLVars + 1
	writes := make([]SessionBatchWrite, 0, n)
	for i := range n {
		id := fmt.Sprintf("chunk-%d", i)
		writes = append(writes, SessionBatchWrite{
			Session: Session{
				ID:               id,
				Project:          "proj",
				Machine:          defaultMachine,
				Agent:            "claude",
				MessageCount:     2,
				UserMessageCount: 1,
				StartedAt:        new("2024-06-01T09:00:00Z"),
				FirstMessage:     new("q"),
			},
			Messages: []Message{
				{
					SessionID: id, Ordinal: 0, Role: "user",
					Content: "q", ContentLength: 1,
					Timestamp: "2024-06-01T09:00:00Z",
				},
				{
					SessionID: id, Ordinal: 1,
					Role:    "assistant",
					Content: "a", ContentLength: 1,
					Timestamp: "2024-06-01T09:00:10Z",
				},
			},
		})
	}
	result, err := d.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	require.Equal(t, n, result.WrittenSessions)
	require.Equal(t, 2*n, result.WrittenMessages)

	// Velocity must not fail even with >500 sessions
	resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
	require.NoError(t, err,
		"GetAnalyticsVelocity with %d sessions", n)
	assert.Equal(t, n, resp.ByComplexity[0].Sessions, "sessions")

	// SessionShape must not fail either
	shape, err := d.GetAnalyticsSessionShape(ctx, baseFilter())
	require.NoError(t, err,
		"GetAnalyticsSessionShape with %d sessions", n)
	assert.Equal(t, n, shape.Count, "Count")
}

// TestGetAnalyticsVelocity_NullTimestamp guards against the velocity scan
// crashing on a NULL messages.timestamp. NULLs only occur on imported or
// migrated archives (fresh inserts always bind a Go string), so reach past
// InsertMessages with raw SQL to plant one. Before the COALESCE fix this
// failed with "converting NULL to string is unsupported"; now the NULL row
// is treated as an invalid timestamp and excluded while the rest of the
// session's messages still drive velocity metrics.
func TestGetAnalyticsVelocity_NullTimestamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertConversation(t, d, "v-null", "proj", "claude", "2024-06-01T09:00:00Z",
		[]time.Duration{
			0, 10 * time.Second, 10 * time.Second,
			10 * time.Second, 10 * time.Second, 10 * time.Second,
		})

	require.NoError(t, d.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(
			"UPDATE messages SET timestamp = NULL"+
				" WHERE session_id = ? AND ordinal = ?", "v-null", 5)
		return err
	}), "null the stored timestamp")

	resp, err := d.GetAnalyticsVelocity(ctx, baseFilter())
	require.NoError(t, err, "GetAnalyticsVelocity over NULL timestamp")

	// The session is still processed and its remaining timestamped
	// messages still produce velocity metrics; the NULL row is simply
	// excluded.
	require.Len(t, resp.ByAgent, 1)
	assert.Equal(t, "claude", resp.ByAgent[0].Label)
	assert.Equal(t, 1, resp.ByAgent[0].Sessions, "session counted")
	assert.Equal(t, 10.0, resp.Overall.TurnCycleSec.P50,
		"valid-timestamped turns still drive velocity")
}

func TestPercentileFloat(t *testing.T) {
	tests := []struct {
		name   string
		sorted []float64
		pct    float64
		want   float64
	}{
		{"Empty", []float64{}, 0.5, 0},
		{"Single", []float64{5.0}, 0.5, 5.0},
		{"P50Odd", []float64{1, 3, 7}, 0.5, 3.0},
		{"P90", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.9, 10.0},
		{"P50Even", []float64{1, 2, 3, 4}, 0.5, 3.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentileFloat(tt.sorted, tt.pct)
			assert.Equal(t, tt.want, got,
				"percentileFloat(%v, %f)",
				tt.sorted, tt.pct)
		})
	}
}

func TestGetAnalyticsTools(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsTools")
		assert.Equal(t, 0, resp.TotalCalls, "TotalCalls")
		assert.Len(t, resp.ByCategory, 0, "len(ByCategory)")
	})

	// Seed sessions with tool_calls.
	insertSession(t, d, "t1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 3
		s.Agent = "claude"
	})
	m1 := asstMsgAt("t1", 0, "[Read: a.go]",
		"2024-06-01T09:00:00Z")
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "t1", ToolName: "Read", Category: "Read"},
		{SessionID: "t1", ToolName: "Read", Category: "Read"},
	}
	m2 := asstMsgAt("t1", 1, "[Bash: ls]",
		"2024-06-01T09:05:00Z")
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "t1", ToolName: "Bash", Category: "Bash"},
	}
	m3 := asstMsgAt("t1", 2, "[Edit: b.go]",
		"2024-06-01T09:10:00Z")
	m3.HasToolUse = true
	m3.ToolCalls = []ToolCall{
		{SessionID: "t1", ToolName: "Edit", Category: "Edit"},
	}
	insertMessages(t, d, m1, m2, m3)

	insertSession(t, d, "t2", "beta", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
	})
	m4 := asstMsgAt("t2", 0, "[Read: c.go]",
		"2024-06-02T10:00:00Z")
	m4.HasToolUse = true
	m4.ToolCalls = []ToolCall{
		{SessionID: "t2", ToolName: "Read", Category: "Read"},
		{SessionID: "t2", ToolName: "Grep", Category: "Grep"},
	}
	insertMessages(t, d, m4)

	t.Run("TotalCalls", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsTools")
		// 2 Read + 1 Bash + 1 Edit + 1 Read + 1 Grep = 6
		assert.Equal(t, 6, resp.TotalCalls, "TotalCalls")
	})

	t.Run("ByCategory", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsTools")
		catMap := make(map[string]int)
		for _, c := range resp.ByCategory {
			catMap[c.Category] = c.Count
		}
		assert.Equal(t, 3, catMap["Read"], "Read")
		assert.Equal(t, 1, catMap["Bash"], "Bash")
		assert.Equal(t, 1, catMap["Edit"], "Edit")
		assert.Equal(t, 1, catMap["Grep"], "Grep")
		// Sorted by count desc: Read first
		assert.Equal(t, "Read", resp.ByCategory[0].Category, "first category")
	})

	t.Run("ByCategoryPct", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsTools")
		// Read: 3/6 = 50%
		assert.Equal(t, 50.0, resp.ByCategory[0].Pct, "Read pct")
	})

	t.Run("ByAgent", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsTools")
		require.Len(t, resp.ByAgent, 2, "len(ByAgent)")
		// Alphabetical: claude, codex
		assert.Equal(t, "claude", resp.ByAgent[0].Agent, "first agent")
		assert.Equal(t, 4, resp.ByAgent[0].Total, "claude total")
		assert.Equal(t, "codex", resp.ByAgent[1].Agent, "second agent")
		assert.Equal(t, 2, resp.ByAgent[1].Total, "codex total")
	})

	t.Run("Trend", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsTools")
		// 2024-06-01 is Saturday, 2024-06-02 is Sunday.
		// Both in same ISO week (May 27 week start).
		// But 2024-06-03 is Monday, different week.
		// So trend should have 1 entry (week of May 27).
		require.Len(t, resp.Trend, 1, "len(Trend)")
		total := 0
		for _, v := range resp.Trend[0].ByCat {
			total += v
		}
		assert.Equal(t, 6, total, "week total")
	})

	t.Run("ProjectFilter", func(t *testing.T) {
		f := baseFilter()
		f.Project = "alpha"
		resp, err := d.GetAnalyticsTools(ctx, f)
		require.NoError(t, err, "GetAnalyticsTools")
		assert.Equal(t, 4, resp.TotalCalls, "TotalCalls")
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsTools(
			ctx, emptyFilter(),
		)
		require.NoError(t, err, "GetAnalyticsTools")
		assert.Equal(t, 0, resp.TotalCalls, "TotalCalls")
	})
}

func TestAnalyticsToolsToolCallsQueryAggregatesInSQL(t *testing.T) {
	q := analyticsToolsQuery("(?,?)", "", false)
	normalized := strings.Join(strings.Fields(strings.ToLower(q)), " ")

	assert.Contains(t, normalized,
		"select tc.session_id, tc.category, count(*)")
	assert.Contains(t, normalized,
		"group by tc.session_id, tc.category")
}

func TestGetAnalyticsToolsModelFilterCountsOnlyMatchingToolCalls(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "tool-model-1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})

	m1 := asstMsgAt("tool-model-1", 0, "[Read: a.go]", "2024-06-01T09:00:00Z")
	m1.Model = "gpt-4o"
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "tool-model-1", ToolName: "Read", Category: "Read"},
		{SessionID: "tool-model-1", ToolName: "Bash", Category: "Bash"},
	}

	m2 := asstMsgAt("tool-model-1", 1, "[Grep: b.go]", "2024-06-01T09:05:00Z")
	m2.Model = "claude-3-5-sonnet"
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "tool-model-1", ToolName: "Grep", Category: "Grep"},
	}

	insertMessages(t, d, m1, m2)

	resp, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsTools")
	assert.Equal(t, 2, resp.TotalCalls, "TotalCalls")
	require.Len(t, resp.ByCategory, 2, "len(ByCategory)")

	catMap := make(map[string]int)
	for _, c := range resp.ByCategory {
		catMap[c.Category] = c.Count
	}
	assert.Equal(t, 1, catMap["Read"], "Read")
	assert.Equal(t, 1, catMap["Bash"], "Bash")
	assert.Zero(t, catMap["Grep"], "Grep")
}

func TestGetAnalyticsToolsModelAndHourFilterCountsOnlyMatchingHourToolCalls(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "tool-model-hour", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})

	m1 := asstMsgAt("tool-model-hour", 0, "[Read: a.go]", "2024-06-01T09:00:00Z")
	m1.Model = "gpt-4o"
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "tool-model-hour", ToolName: "Read", Category: "Read"},
		{SessionID: "tool-model-hour", ToolName: "Bash", Category: "Bash"},
	}

	m2 := asstMsgAt("tool-model-hour", 1, "[Grep: b.go]", "2024-06-01T10:00:00Z")
	m2.Model = "gpt-4o"
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "tool-model-hour", ToolName: "Grep", Category: "Grep"},
	}

	insertMessages(t, d, m1, m2)

	hour := 10
	resp, err := d.GetAnalyticsTools(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsTools")
	assert.Equal(t, 1, resp.TotalCalls, "TotalCalls")
	require.Len(t, resp.ByCategory, 1, "len(ByCategory)")
	assert.Equal(t, "Grep", resp.ByCategory[0].Category, "Category")
	assert.Equal(t, 1, resp.ByCategory[0].Count, "Count")
}

func TestGetAnalyticsSkills(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsSkills(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSkills")
		assert.Equal(t, 0, resp.TotalSkillCalls, "TotalSkillCalls")
		assert.Equal(t, 0, resp.DistinctSkills, "DistinctSkills")
		assert.Empty(t, resp.BySkill, "BySkill")
		assert.Empty(t, resp.Trend, "Trend")
	})

	insertSession(t, d, "sk1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 3
		s.Agent = "claude"
		s.Machine = "mac"
	})
	sk1m1 := asstMsgAt("sk1", 0, "skill calls", "2024-06-01T09:00:00Z")
	sk1m1.HasToolUse = true
	sk1m1.ToolCalls = []ToolCall{
		{SessionID: "sk1", ToolName: "Skill", Category: "Skill", SkillName: "review-code"},
		{SessionID: "sk1", ToolName: "Skill", Category: "Skill", SkillName: "review-code"},
		{SessionID: "sk1", ToolName: "Skill", Category: "Skill", SkillName: " write-tests "},
		{SessionID: "sk1", ToolName: "Skill", Category: "Skill"},
		{SessionID: "sk1", ToolName: "Skill", Category: "Skill", SkillName: "   "},
	}
	insertMessages(t, d, sk1m1)

	insertSession(t, d, "sk2", "beta", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
		s.Machine = "linux"
	})
	sk2m1 := asstMsgAt("sk2", 0, "skill call", "2024-06-02T10:00:00Z")
	sk2m1.HasToolUse = true
	sk2m1.ToolCalls = []ToolCall{
		{SessionID: "sk2", ToolName: "Skill", Category: "Skill", SkillName: "review-code"},
	}
	insertMessages(t, d, sk2m1)

	insertSession(t, d, "sk3", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-03T11:00:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
		s.Machine = "mac"
	})
	_, err := d.getWriter().Exec(
		`UPDATE sessions SET is_automated = 1 WHERE id = 'sk3'`,
	)
	require.NoError(t, err, "mark sk3 automated")
	sk3m1 := asstMsgAt("sk3", 0, "automated skill call", "2024-06-03T11:00:00Z")
	sk3m1.HasToolUse = true
	sk3m1.ToolCalls = []ToolCall{
		{SessionID: "sk3", ToolName: "Skill", Category: "Skill", SkillName: "write-tests"},
	}
	insertMessages(t, d, sk3m1)

	t.Run("Aggregates", func(t *testing.T) {
		resp, err := d.GetAnalyticsSkills(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSkills")
		assert.Equal(t, 5, resp.TotalSkillCalls, "TotalSkillCalls")
		assert.Equal(t, 2, resp.DistinctSkills, "DistinctSkills")
		require.Len(t, resp.BySkill, 2, "BySkill")

		review := resp.BySkill[0]
		assert.Equal(t, "review-code", review.SkillName, "SkillName")
		assert.Equal(t, 3, review.CallCount, "CallCount")
		assert.Equal(t, 2, review.SessionCount, "SessionCount")
		assert.Equal(t, 60.0, review.Pct, "Pct")
		assert.Equal(t, "2024-06-02T10:00:00Z", review.LastUsedAt, "LastUsedAt")
		assert.Equal(t, []SkillAgentBreakdown{
			{Agent: "claude", Count: 2},
			{Agent: "codex", Count: 1},
		}, review.AgentBreakdown, "AgentBreakdown")
		assert.Equal(t, []SkillProjectBreakdown{
			{Project: "alpha", Count: 2},
			{Project: "beta", Count: 1},
		}, review.ProjectBreakdown, "ProjectBreakdown")

		write := resp.BySkill[1]
		assert.Equal(t, "write-tests", write.SkillName, "trimmed SkillName")
		assert.Equal(t, 2, write.CallCount, "write CallCount")
		assert.Equal(t, 2, write.SessionCount, "write SessionCount")
		assert.Equal(t, 40.0, write.Pct, "write Pct")
		assert.Equal(t, "2024-06-03T11:00:00Z", write.LastUsedAt, "write LastUsedAt")
	})

	t.Run("Trend", func(t *testing.T) {
		resp, err := d.GetAnalyticsSkills(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSkills")
		require.Len(t, resp.Trend, 2, "Trend")
		assert.Equal(t, "2024-05-27", resp.Trend[0].Date, "first week")
		assert.Equal(t, 3, resp.Trend[0].BySkill["review-code"], "week 1 review")
		assert.Equal(t, 1, resp.Trend[0].BySkill["write-tests"], "week 1 write")
		assert.Equal(t, "2024-06-03", resp.Trend[1].Date, "second week")
		assert.Equal(t, 1, resp.Trend[1].BySkill["write-tests"], "week 2 write")
	})

	t.Run("Filters", func(t *testing.T) {
		f := baseFilter()
		f.Project = "alpha"
		resp, err := d.GetAnalyticsSkills(ctx, f)
		require.NoError(t, err, "project GetAnalyticsSkills")
		assert.Equal(t, 4, resp.TotalSkillCalls, "project TotalSkillCalls")

		f = baseFilter()
		f.Agent = "claude"
		resp, err = d.GetAnalyticsSkills(ctx, f)
		require.NoError(t, err, "agent GetAnalyticsSkills")
		assert.Equal(t, 3, resp.TotalSkillCalls, "agent TotalSkillCalls")

		f = baseFilter()
		f.Machine = "linux"
		resp, err = d.GetAnalyticsSkills(ctx, f)
		require.NoError(t, err, "machine GetAnalyticsSkills")
		assert.Equal(t, 1, resp.TotalSkillCalls, "machine TotalSkillCalls")

		f = baseFilter()
		f.From = "2024-06-01"
		f.To = "2024-06-01"
		resp, err = d.GetAnalyticsSkills(ctx, f)
		require.NoError(t, err, "date GetAnalyticsSkills")
		assert.Equal(t, 3, resp.TotalSkillCalls, "date TotalSkillCalls")

		f = baseFilter()
		f.ExcludeAutomated = true
		resp, err = d.GetAnalyticsSkills(ctx, f)
		require.NoError(t, err, "automation GetAnalyticsSkills")
		assert.Equal(t, 4, resp.TotalSkillCalls, "automation TotalSkillCalls")
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSkills(ctx, emptyFilter())
		require.NoError(t, err, "GetAnalyticsSkills")
		assert.Equal(t, 0, resp.TotalSkillCalls, "TotalSkillCalls")
		assert.Equal(t, 0, resp.DistinctSkills, "DistinctSkills")
	})
}

func TestGetAnalyticsSkillsUsesMessageTimestamp(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	filter := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-30",
		Timezone: "UTC",
	}

	// Long-running session: starts early, uses the skill weeks later.
	insertSession(t, d, "lr1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	lrMsg := asstMsgAt("lr1", 0, "late skill call", "2024-06-20T15:00:00Z")
	lrMsg.HasToolUse = true
	lrMsg.ToolCalls = []ToolCall{
		{SessionID: "lr1", ToolName: "Skill", Category: "Skill", SkillName: "deploy"},
	}
	insertMessages(t, d, lrMsg)

	// Fallback session: skill-call message has no timestamp.
	insertSession(t, d, "fb1", "beta", func(s *Session) {
		s.StartedAt = new("2024-06-05T08:00:00Z")
		s.MessageCount = 1
		s.Agent = "codex"
	})
	fbMsg := asstMsg("fb1", 0, "skill call")
	fbMsg.Timestamp = ""
	fbMsg.HasToolUse = true
	fbMsg.ToolCalls = []ToolCall{
		{SessionID: "fb1", ToolName: "Skill", Category: "Skill", SkillName: "build"},
	}
	insertMessages(t, d, fbMsg)

	resp, err := d.GetAnalyticsSkills(ctx, filter)
	require.NoError(t, err, "GetAnalyticsSkills")
	require.Len(t, resp.BySkill, 2, "BySkill")

	bySkill := map[string]SkillUsage{}
	for _, s := range resp.BySkill {
		bySkill[s.SkillName] = s
	}

	deploy, ok := bySkill["deploy"]
	require.True(t, ok, "deploy present")
	assert.Equal(t, "2024-06-20T15:00:00Z", deploy.LastUsedAt,
		"deploy LastUsedAt uses message timestamp, not session start")

	build, ok := bySkill["build"]
	require.True(t, ok, "build present")
	assert.Equal(t, "2024-06-05T08:00:00Z", build.LastUsedAt,
		"build LastUsedAt falls back to session timestamp")

	trend := map[string]int{}
	for _, e := range resp.Trend {
		if e.BySkill["deploy"] > 0 {
			trend[e.Date] += e.BySkill["deploy"]
		}
	}
	assert.Equal(t, map[string]int{"2024-06-17": 1}, trend,
		"deploy trend bucket follows message timestamp week")
}

func TestGetAnalyticsSkillsSpreadsTrendAcrossWeeks(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	filter := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-30",
		Timezone: "UTC",
	}

	// One long-running session invokes the same skill in two messages
	// that fall in different weeks. Each call must bucket into its own
	// week, not roll up onto the latest message's week.
	insertSession(t, d, "sp1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-03T09:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	early := asstMsgAt("sp1", 0, "early skill call", "2024-06-03T09:00:00Z")
	early.HasToolUse = true
	early.ToolCalls = []ToolCall{
		{SessionID: "sp1", ToolName: "Skill", Category: "Skill", SkillName: "deploy"},
	}
	late := asstMsgAt("sp1", 1, "late skill call", "2024-06-17T15:00:00Z")
	late.HasToolUse = true
	late.ToolCalls = []ToolCall{
		{SessionID: "sp1", ToolName: "Skill", Category: "Skill", SkillName: "deploy"},
		{SessionID: "sp1", ToolName: "Skill", Category: "Skill", SkillName: "deploy"},
	}
	insertMessages(t, d, early, late)

	resp, err := d.GetAnalyticsSkills(ctx, filter)
	require.NoError(t, err, "GetAnalyticsSkills")
	require.Len(t, resp.BySkill, 1, "BySkill")
	assert.Equal(t, 3, resp.BySkill[0].CallCount, "rolled-up CallCount")
	assert.Equal(t, 1, resp.BySkill[0].SessionCount, "SessionCount")

	trend := map[string]int{}
	for _, e := range resp.Trend {
		if c := e.BySkill["deploy"]; c > 0 {
			trend[e.Date] += c
		}
	}
	assert.Equal(t, map[string]int{
		"2024-06-03": 1,
		"2024-06-17": 2,
	}, trend, "each call buckets into its own message-timestamp week")
}

func TestBuildSkillsAnalyticsLastUsedChronological(t *testing.T) {
	// The fractional-second timestamp is chronologically later but
	// lexically smaller ('.' sorts before 'Z'), so a string compare
	// would pick the wrong value.
	rows := []SkillAnalyticsRow{
		{
			SessionID: "a", SkillName: "deploy", Date: "2024-06-10",
			LastUsedAt: "2024-06-10T09:00:00Z", Count: 1,
		},
		{
			SessionID: "b", SkillName: "deploy", Date: "2024-06-10",
			LastUsedAt: "2024-06-10T09:00:00.500Z", Count: 1,
		},
	}

	resp := BuildSkillsAnalytics(rows)
	require.Len(t, resp.BySkill, 1, "BySkill")
	assert.Equal(t, "2024-06-10T09:00:00.500Z",
		resp.BySkill[0].LastUsedAt,
		"LastUsedAt picks the chronologically latest timestamp")
}

func TestResolveSkillRowTime(t *testing.T) {
	hour := 10
	monday := 0 // ISO Mon=0
	base := func() AnalyticsFilter {
		return AnalyticsFilter{From: "2024-06-01", To: "2024-06-30", Timezone: "UTC"}
	}
	tests := []struct {
		name          string
		f             AnalyticsFilter
		msgTS, sessTS string
		wantUsed      string
		wantDate      string
		wantKeep      bool
	}{
		{"in range", base(), "2024-06-10T10:00:00Z", "2024-05-01T00:00:00Z",
			"2024-06-10T10:00:00Z", "2024-06-10", true},
		{"before range", base(), "2024-05-31T10:00:00Z", "ignored",
			"2024-05-31T10:00:00Z", "2024-05-31", false},
		{"after range", base(), "2024-07-01T10:00:00Z", "ignored",
			"2024-07-01T10:00:00Z", "2024-07-01", false},
		{"fallback to session ts", base(), "", "2024-06-15T08:00:00Z",
			"2024-06-15T08:00:00Z", "2024-06-15", true},
		{"hour excludes", func() AnalyticsFilter {
			f := base()
			f.Hour = &hour
			return f
		}(), "2024-06-10T09:00:00Z", "", "2024-06-10T09:00:00Z", "2024-06-10", false},
		{"hour includes", func() AnalyticsFilter {
			f := base()
			f.Hour = &hour
			return f
		}(), "2024-06-10T10:00:00Z", "", "2024-06-10T10:00:00Z", "2024-06-10", true},
		{"day-of-week excludes", func() AnalyticsFilter {
			f := base()
			f.DayOfWeek = &monday
			return f
		}(), "2024-06-11T10:00:00Z", "", "2024-06-11T10:00:00Z", "2024-06-11", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			used, date, keep := tc.f.ResolveSkillRowTime(tc.msgTS, tc.sessTS)
			assert.Equal(t, tc.wantUsed, used, "usedTS")
			assert.Equal(t, tc.wantDate, date, "date")
			assert.Equal(t, tc.wantKeep, keep, "keep")
		})
	}
}

func TestGetAnalyticsSkillsDateBoundaries(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	filter := AnalyticsFilter{From: "2024-06-01", To: "2024-06-30", Timezone: "UTC"}

	// The session starts before the range and uses the skill before,
	// inside, and after it. Only the in-range call should count.
	insertSession(t, d, "span", "alpha", func(s *Session) {
		s.StartedAt = new("2024-05-20T09:00:00Z")
		s.MessageCount = 3
		s.Agent = "claude"
	})
	mkCall := func(ord int, ts string) Message {
		m := asstMsgAt("span", ord, "call", ts)
		m.HasToolUse = true
		m.ToolCalls = []ToolCall{
			{SessionID: "span", ToolName: "Skill", Category: "Skill", SkillName: "deploy"},
		}
		return m
	}
	insertMessages(t, d,
		mkCall(0, "2024-05-25T10:00:00Z"), // before From
		mkCall(1, "2024-06-10T10:00:00Z"), // in range
		mkCall(2, "2024-07-05T10:00:00Z"), // after To
	)

	resp, err := d.GetAnalyticsSkills(ctx, filter)
	require.NoError(t, err, "GetAnalyticsSkills")
	require.Len(t, resp.BySkill, 1, "BySkill")
	assert.Equal(t, "deploy", resp.BySkill[0].SkillName)
	assert.Equal(t, 1, resp.BySkill[0].CallCount,
		"only the in-range call counts, even though the session started "+
			"before the range")
	assert.Equal(t, "2024-06-10T10:00:00Z", resp.BySkill[0].LastUsedAt)

	trend := map[string]int{}
	for _, e := range resp.Trend {
		if c := e.BySkill["deploy"]; c > 0 {
			trend[e.Date] += c
		}
	}
	assert.Equal(t, map[string]int{"2024-06-10": 1}, trend,
		"only the in-range week is bucketed")
}

func TestAnalyticsSkillsToolCallsQueryAggregatesInSQL(t *testing.T) {
	q := analyticsSkillsQuery("(?,?)", "")
	normalized := strings.Join(strings.Fields(strings.ToLower(q)), " ")

	assert.Contains(t, normalized,
		"select tc.session_id, trim(tc.skill_name), count(*), "+
			"coalesce(m.timestamp, '')")
	assert.Contains(t, normalized,
		"left join messages m on m.session_id = tc.session_id "+
			"and m.id = tc.message_id")
	assert.Contains(t, normalized,
		"trim(coalesce(tc.skill_name, '')) != ''")
	assert.Contains(t, normalized,
		"group by tc.session_id, trim(tc.skill_name), "+
			"coalesce(m.timestamp, '')")
}

func TestGetAnalyticsSkillsModelFilterCountsOnlyMatchingSkillCalls(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "skill-model-1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})

	m1 := asstMsgAt("skill-model-1", 0, "skill call", "2024-06-01T09:00:00Z")
	m1.Model = "gpt-4o"
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{
			SessionID: "skill-model-1",
			ToolName:  "Skill",
			Category:  "Skill",
			SkillName: "review-code",
		},
	}

	m2 := asstMsgAt("skill-model-1", 1, "skill call", "2024-06-01T09:05:00Z")
	m2.Model = "claude-3-5-sonnet"
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{
			SessionID: "skill-model-1",
			ToolName:  "Skill",
			Category:  "Skill",
			SkillName: "write-tests",
		},
	}

	insertMessages(t, d, m1, m2)

	resp, err := d.GetAnalyticsSkills(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSkills")
	assert.Equal(t, 1, resp.TotalSkillCalls, "TotalSkillCalls")
	assert.Equal(t, 1, resp.DistinctSkills, "DistinctSkills")
	require.Len(t, resp.BySkill, 1, "len(BySkill)")
	assert.Equal(t, "review-code", resp.BySkill[0].SkillName, "SkillName")
	assert.Equal(t, 1, resp.BySkill[0].CallCount, "CallCount")
	assert.Equal(t, "2024-06-01T09:00:00Z", resp.BySkill[0].LastUsedAt,
		"LastUsedAt")
}

func TestGetAnalyticsToolsCanceled(t *testing.T) {
	d := testDB(t)
	ctx := canceledCtx()
	_, err := d.GetAnalyticsTools(ctx, baseFilter())
	requireCanceledErr(t, err)
}

func TestGetAnalyticsSkillsCanceled(t *testing.T) {
	d := testDB(t)
	ctx := canceledCtx()
	_, err := d.GetAnalyticsSkills(ctx, baseFilter())
	requireCanceledErr(t, err)
}

func TestActivityToolAndThinkingCounts(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "at1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 3
		s.Agent = "claude"
	})

	// User message, assistant with thinking, assistant with tool
	u := userMsg("at1", 0, "hello")
	a1 := asstMsg("at1", 1, "thinking response")
	a1.HasThinking = true
	a2 := asstMsg("at1", 2, "[Read: a.go]")
	a2.HasToolUse = true
	a2.ToolCalls = []ToolCall{
		{SessionID: "at1", ToolName: "Read", Category: "Read"},
		{SessionID: "at1", ToolName: "Bash", Category: "Bash"},
	}
	insertMessages(t, d, u, a1, a2)

	resp := mustActivity(t, d, ctx, baseFilter(), "day")
	require.NotEmpty(t, resp.Series, "expected non-empty series")

	entry := resp.Series[0]
	assert.Equal(t, 1, entry.ThinkingMessages, "ThinkingMessages")
	assert.Equal(t, 2, entry.ToolCalls, "ToolCalls")
}

func TestActivityToolCallsRespectModelFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "at-model", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})

	m1 := asstMsgAt("at-model", 0, "[Read: a.go]", "2024-06-01T09:00:00Z")
	m1.Model = "gpt-4o"
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "at-model", ToolName: "Read", Category: "Read"},
		{SessionID: "at-model", ToolName: "Bash", Category: "Bash"},
	}

	m2 := asstMsgAt("at-model", 1, "[Grep: b.go]", "2024-06-01T09:05:00Z")
	m2.Model = "claude-3-5-sonnet"
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "at-model", ToolName: "Grep", Category: "Grep"},
	}

	insertMessages(t, d, m1, m2)

	resp := mustActivity(t, d, ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 2, resp.Series[0].ToolCalls, "ToolCalls")
}

func TestGetAnalyticsTopSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "messages",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		assert.Len(t, resp.Sessions, 0, "len(Sessions)")
		assert.Equal(t, "messages", resp.Metric, "Metric")
	})

	stats := seedAnalyticsData(t, d)

	t.Run("ByMessages", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "messages",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		require.Len(t, resp.Sessions, stats.TotalSessions,
			"len(Sessions)")
		// First should be the session with most messages (b1=30)
		assert.Equal(t, 30, resp.Sessions[0].MessageCount, "top session messages")
		assert.Equal(t, "project-beta", resp.Sessions[0].Project, "top session project")
	})

	t.Run("ByDuration", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "duration",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		assert.Equal(t, "duration", resp.Metric, "Metric")
		// All seeded sessions have 1h duration except a1
		// which runs from 09:00 to midyear
		require.NotEmpty(t, resp.Sessions,
			"expected non-empty sessions")
		// All sessions should have positive duration
		for _, s := range resp.Sessions {
			assert.Greater(t, s.DurationMin, 0.0,
				"session %s duration", s.ID)
		}
	})

	t.Run("ByDurationRanksByActiveDuration", func(t *testing.T) {
		insertSession(t, d, "wall-dominant", "project-gamma", func(s *Session) {
			s.StartedAt = Ptr("2024-06-02T09:00:00Z")
			s.EndedAt = Ptr("2024-06-02T11:00:00Z")
			s.MessageCount = 3
		})
		insertMessages(
			t,
			d,
			userMsgAt("wall-dominant", 0, "noop", "2024-06-02T09:00:00Z"),
			func() Message {
				m := asstMsgAt(
					"wall-dominant",
					1,
					"idle wait",
					"2024-06-02T10:59:00Z",
				)
				m.HasToolUse = true
				return m
			}(),
			userMsgAt(
				"wall-dominant",
				2,
				"done",
				"2024-06-02T11:00:00Z",
			),
		)

		insertSession(t, d, "actively-working", "project-gamma", func(s *Session) {
			s.StartedAt = Ptr("2024-06-02T09:30:00Z")
			s.EndedAt = Ptr("2024-06-02T09:50:00Z")
			s.MessageCount = 3
		})
		insertMessages(
			t,
			d,
			userMsgAt("actively-working", 0, "start", "2024-06-02T09:30:00Z"),
			func() Message {
				m := asstMsgAt(
					"actively-working",
					1,
					"tooling",
					"2024-06-02T09:35:00Z",
				)
				m.HasToolUse = true
				return m
			}(),
			userMsgAt("actively-working", 2, "finish", "2024-06-02T09:50:00Z"),
		)

		resp, err := d.GetAnalyticsTopSessions(
			ctx, AnalyticsFilter{Project: "project-gamma"}, "duration",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		assert.Equal(t, "duration", resp.Metric, "Metric")
		require.Len(t, resp.Sessions, 2, "sessions")
		assert.Equal(t, "actively-working", resp.Sessions[0].ID, "top session by active duration")
		assert.Equal(t, 20.0, resp.Sessions[0].DurationMin, "active total duration")
		// 5 min user->asst gap + a 15 min gap capped at the 5 min idle
		// cap = 10.
		assert.Equal(t, 10.0, resp.Sessions[0].ActiveDurationMin, "active duration")
		assert.Equal(t, 120.0, resp.Sessions[1].DurationMin, "wall-only duration")
		// 119 min idle gap capped to 5 + a 1 min gap = 6, so the
		// mostly-idle 2-hour session ranks below the engaged 20-min one.
		assert.Equal(t, 6.0, resp.Sessions[1].ActiveDurationMin, "idle active duration")
	})

	t.Run("ByDurationKeepsNearTieOrderBeforeDisplayRounding", func(t *testing.T) {
		// Two sessions whose active durations differ only below the
		// display-rounding granularity (2.504 vs 2.496 min). Both render
		// as "2.5", but the raw value must decide the rank. The single
		// gap stays under the 5 min idle cap so the clamp does not
		// flatten the sub-second difference.
		insertSession(t, d, "near-tie-longer", "project-precision", func(s *Session) {
			s.StartedAt = Ptr("2024-06-03T09:00:00.000Z")
			s.EndedAt = Ptr("2024-06-03T09:02:30.240Z")
			s.MessageCount = 2
		})
		insertMessages(
			t,
			d,
			userMsgAt("near-tie-longer", 0, "start", "2024-06-03T09:00:00.000Z"),
			asstMsgAt("near-tie-longer", 1, "work", "2024-06-03T09:02:30.240Z"),
		)

		insertSession(t, d, "near-tie-shorter", "project-precision", func(s *Session) {
			s.StartedAt = Ptr("2024-06-03T09:00:00.000Z")
			s.EndedAt = Ptr("2024-06-03T09:02:29.760Z")
			s.MessageCount = 2
		})
		insertMessages(
			t,
			d,
			userMsgAt("near-tie-shorter", 0, "start", "2024-06-03T09:00:00.000Z"),
			asstMsgAt("near-tie-shorter", 1, "work", "2024-06-03T09:02:29.760Z"),
		)

		resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
			Project: "project-precision",
			From:    "2024-06-03",
			To:      "2024-06-03",
		}, "duration")
		require.NoError(t, err, "GetAnalyticsTopSessions")
		require.Len(t, resp.Sessions, 2, "precision sessions")
		assert.Equal(t, "near-tie-longer", resp.Sessions[0].ID, "raw active duration should decide the rank")
		assert.Equal(t, 2.5, resp.Sessions[0].ActiveDurationMin, "display rounding")
		assert.Equal(t, 2.5, resp.Sessions[1].ActiveDurationMin, "display rounding")
	})

	t.Run("ByDurationRanksByActiveDurationInGoFallback", func(t *testing.T) {
		writes := make([]SessionBatchWrite, 0, 202)
		for i := range 201 {
			id := fmt.Sprintf("dst-wall-%03d", i)
			writes = append(writes, SessionBatchWrite{
				Session: Session{
					ID:               id,
					Project:          "project-dst",
					Machine:          defaultMachine,
					Agent:            defaultAgent,
					StartedAt:        Ptr("2026-03-10T09:00:00Z"),
					EndedAt:          Ptr("2026-03-10T11:00:00Z"),
					MessageCount:     3,
					UserMessageCount: 2,
				},
				Messages: []Message{
					userMsgAt(id, 0, "noop", "2026-03-10T09:00:00Z"),
					func() Message {
						m := asstMsgAt(
							id,
							1,
							"idle wait",
							"2026-03-10T10:59:00Z",
						)
						m.HasToolUse = true
						return m
					}(),
					userMsgAt(id, 2, "done", "2026-03-10T11:00:00Z"),
				},
			})
		}
		writes = append(writes, SessionBatchWrite{
			Session: Session{
				ID:               "dst-actively-working",
				Project:          "project-dst",
				Machine:          defaultMachine,
				Agent:            defaultAgent,
				StartedAt:        Ptr("2026-03-10T09:30:00Z"),
				EndedAt:          Ptr("2026-03-10T09:50:00Z"),
				MessageCount:     3,
				UserMessageCount: 2,
			},
			Messages: []Message{
				userMsgAt("dst-actively-working", 0, "start", "2026-03-10T09:30:00Z"),
				func() Message {
					m := asstMsgAt(
						"dst-actively-working",
						1,
						"tooling",
						"2026-03-10T09:35:00Z",
					)
					m.HasToolUse = true
					return m
				}(),
				userMsgAt("dst-actively-working", 2, "finish", "2026-03-10T09:50:00Z"),
			},
		})
		result, err := d.WriteSessionBatchAtomic(writes)
		require.NoError(t, err)
		require.Equal(t, len(writes), result.WrittenSessions)
		require.Equal(t, 3*len(writes), result.WrittenMessages)

		resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
			Project:  "project-dst",
			From:     "2026-03-01",
			To:       "2026-03-31",
			Timezone: "America/New_York",
		}, "duration")
		require.NoError(t, err, "GetAnalyticsTopSessions")
		require.NotEmpty(t, resp.Sessions, "sessions")
		assert.Equal(t, "dst-actively-working", resp.Sessions[0].ID, "top session by active duration in fallback")
		// 5 + 5 (15 min gap capped at the 5 min idle cap) = 10.
		assert.Equal(t, 10.0, resp.Sessions[0].ActiveDurationMin, "fallback active duration")
	})

	t.Run("ByDurationKeepsNearTieOrderInGoFallback", func(t *testing.T) {
		// Same near-tie invariant as the SQLite SQL case, but the
		// timezone filter forces the Go fallback ranking path.
		insertSession(t, d, "dst-near-tie-longer", "project-dst-precision", func(s *Session) {
			s.StartedAt = Ptr("2026-03-10T09:00:00.000Z")
			s.EndedAt = Ptr("2026-03-10T09:02:30.240Z")
			s.MessageCount = 2
		})
		insertMessages(
			t,
			d,
			userMsgAt("dst-near-tie-longer", 0, "start", "2026-03-10T09:00:00.000Z"),
			asstMsgAt("dst-near-tie-longer", 1, "work", "2026-03-10T09:02:30.240Z"),
		)

		insertSession(t, d, "dst-near-tie-shorter", "project-dst-precision", func(s *Session) {
			s.StartedAt = Ptr("2026-03-10T09:00:00.000Z")
			s.EndedAt = Ptr("2026-03-10T09:02:29.760Z")
			s.MessageCount = 2
		})
		insertMessages(
			t,
			d,
			userMsgAt("dst-near-tie-shorter", 0, "start", "2026-03-10T09:00:00.000Z"),
			asstMsgAt("dst-near-tie-shorter", 1, "work", "2026-03-10T09:02:29.760Z"),
		)

		resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
			Project:  "project-dst-precision",
			From:     "2026-03-01",
			To:       "2026-03-31",
			Timezone: "America/New_York",
		}, "duration")
		require.NoError(t, err, "GetAnalyticsTopSessions")
		require.Len(t, resp.Sessions, 2, "fallback precision sessions")
		assert.Equal(t, "dst-near-tie-longer", resp.Sessions[0].ID, "fallback should keep raw active ordering")
		assert.Equal(t, 2.5, resp.Sessions[0].ActiveDurationMin, "fallback display rounding")
		assert.Equal(t, 2.5, resp.Sessions[1].ActiveDurationMin, "fallback display rounding")
	})

	t.Run("ByDurationCapsLongGapsAndCountsGeneration", func(t *testing.T) {
		// No tool calls anywhere: under the old tool-execution-only
		// definition this session scored 0. The clamp counts every gap
		// -- model generation included -- and bounds the one long idle
		// gap at the 5 min cap: 4 (gen) + 60->5 (idle cap) + 1 = 10.
		insertSession(t, d, "clamp-mixed", "project-clamp-sql", func(s *Session) {
			s.StartedAt = Ptr("2024-06-04T09:00:00Z")
			s.EndedAt = Ptr("2024-06-04T10:05:00Z")
			s.MessageCount = 4
		})
		insertMessages(
			t,
			d,
			userMsgAt("clamp-mixed", 0, "start", "2024-06-04T09:00:00Z"),
			asstMsgAt("clamp-mixed", 1, "thinking", "2024-06-04T09:04:00Z"),
			userMsgAt("clamp-mixed", 2, "back", "2024-06-04T10:04:00Z"),
			asstMsgAt("clamp-mixed", 3, "reply", "2024-06-04T10:05:00Z"),
		)

		resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
			Project: "project-clamp-sql",
		}, "duration")
		require.NoError(t, err, "GetAnalyticsTopSessions")
		require.Len(t, resp.Sessions, 1, "clamp session")
		assert.Equal(t, "clamp-mixed", resp.Sessions[0].ID)
		assert.Equal(t, 65.0, resp.Sessions[0].DurationMin, "wall duration")
		assert.Equal(t, 10.0, resp.Sessions[0].ActiveDurationMin, "clamped active duration")
	})

	t.Run("ByDurationCapsLongGapsAndCountsGenerationInGoFallback", func(t *testing.T) {
		// Same clamp + generation accounting as the SQLite SQL case,
		// exercised through the timezone-aware Go fallback path.
		insertSession(t, d, "dst-clamp-mixed", "project-clamp-fallback", func(s *Session) {
			s.StartedAt = Ptr("2026-03-10T09:00:00Z")
			s.EndedAt = Ptr("2026-03-10T10:05:00Z")
			s.MessageCount = 4
		})
		insertMessages(
			t,
			d,
			userMsgAt("dst-clamp-mixed", 0, "start", "2026-03-10T09:00:00Z"),
			asstMsgAt("dst-clamp-mixed", 1, "thinking", "2026-03-10T09:04:00Z"),
			userMsgAt("dst-clamp-mixed", 2, "back", "2026-03-10T10:04:00Z"),
			asstMsgAt("dst-clamp-mixed", 3, "reply", "2026-03-10T10:05:00Z"),
		)

		resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
			Project:  "project-clamp-fallback",
			From:     "2026-03-01",
			To:       "2026-03-31",
			Timezone: "America/New_York",
		}, "duration")
		require.NoError(t, err, "GetAnalyticsTopSessions")
		require.Len(t, resp.Sessions, 1, "fallback clamp session")
		assert.Equal(t, "dst-clamp-mixed", resp.Sessions[0].ID)
		assert.Equal(t, 65.0, resp.Sessions[0].DurationMin, "wall duration")
		assert.Equal(t, 10.0, resp.Sessions[0].ActiveDurationMin, "fallback clamped active duration")
	})

	t.Run("ByDurationExcludesReversedAndEmptyTimestamps", func(t *testing.T) {
		// A reversed (ended < started) or empty-timestamp session can
		// still accumulate positive message-gap active duration, so
		// without an eligibility guard it would rank into the duration
		// list. The SQL path must reject both, matching DuckDB and the Go
		// fallback.
		insertSession(t, d, "elig-valid", "project-elig-sql", func(s *Session) {
			s.StartedAt = Ptr("2024-07-01T09:00:00Z")
			s.EndedAt = Ptr("2024-07-01T09:30:00Z")
			s.MessageCount = 2
		})
		insertMessages(
			t, d,
			userMsgAt("elig-valid", 0, "start", "2024-07-01T09:00:00Z"),
			asstMsgAt("elig-valid", 1, "work", "2024-07-01T09:03:00Z"),
		)

		insertSession(t, d, "elig-reversed", "project-elig-sql", func(s *Session) {
			s.StartedAt = Ptr("2024-07-01T10:00:00Z")
			s.EndedAt = Ptr("2024-07-01T09:00:00Z")
			s.MessageCount = 2
		})
		insertMessages(
			t, d,
			userMsgAt("elig-reversed", 0, "start", "2024-07-01T09:00:00Z"),
			asstMsgAt("elig-reversed", 1, "work", "2024-07-01T09:04:00Z"),
		)

		insertSession(t, d, "elig-empty", "project-elig-sql", func(s *Session) {
			s.StartedAt = Ptr("")
			s.EndedAt = Ptr("")
			s.MessageCount = 2
		})
		insertMessages(
			t, d,
			userMsgAt("elig-empty", 0, "start", "2024-07-01T09:00:00Z"),
			asstMsgAt("elig-empty", 1, "work", "2024-07-01T09:04:00Z"),
		)

		resp, err := d.GetAnalyticsTopSessions(
			ctx, AnalyticsFilter{Project: "project-elig-sql"}, "duration",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		var ids []string
		for _, s := range resp.Sessions {
			ids = append(ids, s.ID)
		}
		assert.Equal(t, []string{"elig-valid"}, ids,
			"reversed and empty duration rows must be excluded")
	})

	t.Run("ByDurationExcludesReversedAndEmptyTimestampsInGoFallback", func(t *testing.T) {
		// Same eligibility guard as the SQL path, but the timezone filter
		// forces the Go fallback ranking path. Parity matters because the
		// ranking is by message-gap active duration, which stays positive
		// regardless of reversed or missing wall-clock timestamps.
		insertSession(t, d, "elig-fb-valid", "project-elig-fb", func(s *Session) {
			s.StartedAt = Ptr("2026-03-12T09:00:00Z")
			s.EndedAt = Ptr("2026-03-12T09:30:00Z")
			s.MessageCount = 2
		})
		insertMessages(
			t, d,
			userMsgAt("elig-fb-valid", 0, "start", "2026-03-12T09:00:00Z"),
			asstMsgAt("elig-fb-valid", 1, "work", "2026-03-12T09:03:00Z"),
		)

		insertSession(t, d, "elig-fb-reversed", "project-elig-fb", func(s *Session) {
			s.StartedAt = Ptr("2026-03-12T10:00:00Z")
			s.EndedAt = Ptr("2026-03-12T09:00:00Z")
			s.MessageCount = 2
		})
		insertMessages(
			t, d,
			userMsgAt("elig-fb-reversed", 0, "start", "2026-03-12T09:00:00Z"),
			asstMsgAt("elig-fb-reversed", 1, "work", "2026-03-12T09:04:00Z"),
		)

		insertSession(t, d, "elig-fb-empty", "project-elig-fb", func(s *Session) {
			s.StartedAt = Ptr("")
			s.EndedAt = Ptr("")
			s.MessageCount = 2
		})
		insertMessages(
			t, d,
			userMsgAt("elig-fb-empty", 0, "start", "2026-03-12T09:00:00Z"),
			asstMsgAt("elig-fb-empty", 1, "work", "2026-03-12T09:04:00Z"),
		)
		// created_at is not bound by UpsertSession and otherwise defaults
		// to insertion time, which the date filter would drop before the
		// eligibility guard is reached. Pin it into the filter window so
		// this row genuinely exercises the empty-timestamp NULLIF guard
		// rather than being excluded by the date range.
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			"2026-03-12T00:00:00Z", "elig-fb-empty",
		)
		require.NoError(t, err, "pin elig-fb-empty created_at")

		resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
			Project:  "project-elig-fb",
			From:     "2026-03-01",
			To:       "2026-03-31",
			Timezone: "America/New_York",
		}, "duration")
		require.NoError(t, err, "GetAnalyticsTopSessions")
		var ids []string
		for _, s := range resp.Sessions {
			ids = append(ids, s.ID)
		}
		assert.Equal(t, []string{"elig-fb-valid"}, ids,
			"reversed and empty duration rows must be excluded in fallback")
	})

	t.Run("DefaultMetric", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, baseFilter(), "",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		assert.Equal(t, "messages", resp.Metric, "Metric")
	})

	t.Run("ProjectFilter", func(t *testing.T) {
		f := baseFilter()
		f.Project = "project-alpha"
		resp, err := d.GetAnalyticsTopSessions(
			ctx, f, "messages",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		assert.Len(t, resp.Sessions, 3, "len(Sessions)")
		for _, s := range resp.Sessions {
			assert.Equal(t, "project-alpha", s.Project, "session project")
		}
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsTopSessions(
			ctx, emptyFilter(), "messages",
		)
		require.NoError(t, err, "GetAnalyticsTopSessions")
		assert.Len(t, resp.Sessions, 0, "len(Sessions)")
	})
}

func TestGetAnalyticsTopSessionsOutputTokensUseFilteredModelTotals(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "top-output-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
		s.TotalOutputTokens = 510
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "top-output-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "gpt-4o", OutputTokens: 10, HasOutputTokens: true,
		},
		Message{
			SessionID: "top-output-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp:    "2024-06-01T09:05:00Z",
			Model:        "claude-3-5-sonnet",
			OutputTokens: 500, HasOutputTokens: true,
		},
	)

	insertSession(t, d, "top-output-gpt", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T11:00:00Z")
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.MessageCount = 1
		s.Agent = "gpt"
		s.TotalOutputTokens = 30
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d, Message{
		SessionID: "top-output-gpt", Ordinal: 0, Role: "assistant",
		Content: "gpt", ContentLength: 3,
		Timestamp: "2024-06-01T11:00:00Z",
		Model:     "gpt-4o", OutputTokens: 30, HasOutputTokens: true,
	})

	insertSession(t, d, "top-output-uncovered", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T13:00:00Z")
		s.EndedAt = new("2024-06-01T14:00:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
		s.TotalOutputTokens = 900
		s.HasTotalOutputTokens = true
	})
	insertMessages(t, d,
		Message{
			SessionID: "top-output-uncovered", Ordinal: 0,
			Role:    "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T13:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "top-output-uncovered", Ordinal: 1,
			Role:    "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp:    "2024-06-01T13:05:00Z",
			Model:        "claude-3-5-sonnet",
			OutputTokens: 900, HasOutputTokens: true,
		},
	)

	resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "output_tokens")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 2, "len(Sessions)")
	assert.Equal(t, "top-output-gpt", resp.Sessions[0].ID, "top session")
	assert.Equal(t, 30, resp.Sessions[0].OutputTokens,
		"top OutputTokens")
	assert.Equal(t, "top-output-mixed", resp.Sessions[1].ID,
		"second session")
	assert.Equal(t, 10, resp.Sessions[1].OutputTokens,
		"second OutputTokens")
}

func TestGetAnalyticsTopSessionsDisplayName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	rawFirst := "raw first user message"
	sessionName := "Agent generated title"
	insertSession(t, d, "session-name", "project-alpha", func(s *Session) {
		s.FirstMessage = &rawFirst
		s.SessionName = &sessionName
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 10
		s.UserMessageCount = 2
	})

	customName := "User renamed title"
	customSessionName := "Generated title hidden by rename"
	insertSession(t, d, "custom-name", "project-alpha", func(s *Session) {
		s.FirstMessage = &rawFirst
		s.SessionName = &customSessionName
		s.StartedAt = new("2024-06-01T11:00:00Z")
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.MessageCount = 9
		s.UserMessageCount = 2
	})
	require.NoError(t, d.RenameSession("custom-name", &customName),
		"RenameSession")

	resp, err := d.GetAnalyticsTopSessions(
		ctx, baseFilter(), "messages",
	)
	require.NoError(t, err, "GetAnalyticsTopSessions")

	byID := map[string]TopSession{}
	for _, session := range resp.Sessions {
		byID[session.ID] = session
	}

	named, ok := byID["session-name"]
	require.True(t, ok, "session-name missing from top sessions")
	require.NotNil(t, named.DisplayName,
		"session_name should be exposed as display_name")
	assert.Equal(t, sessionName, *named.DisplayName)

	custom, ok := byID["custom-name"]
	require.True(t, ok, "custom-name missing from top sessions")
	require.NotNil(t, custom.DisplayName,
		"custom display_name should be exposed")
	assert.Equal(t, customName, *custom.DisplayName)
}

func TestGetAnalyticsTopSessionsMessagesUseFilteredModelCounts(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "top-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 3
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "top-mixed", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "top-mixed", Ordinal: 1, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp: "2024-06-01T09:05:00Z",
			Model:     "claude-3-5-sonnet",
		},
		Message{
			SessionID: "top-mixed", Ordinal: 2, Role: "assistant",
			Content: "claude", ContentLength: 6,
			Timestamp: "2024-06-01T09:06:00Z",
			Model:     "claude-3-5-sonnet",
		},
	)

	insertSession(t, d, "top-gpt", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T11:00:00Z")
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.MessageCount = 2
		s.Agent = "gpt"
	})
	insertMessages(t, d,
		Message{
			SessionID: "top-gpt", Ordinal: 0, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T11:00:00Z",
			Model:     "gpt-4o",
		},
		Message{
			SessionID: "top-gpt", Ordinal: 1, Role: "assistant",
			Content: "gpt", ContentLength: 3,
			Timestamp: "2024-06-01T11:05:00Z",
			Model:     "gpt-4o",
		},
	)

	resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "messages")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 2, "len(Sessions)")
	assert.Equal(t, "top-gpt", resp.Sessions[0].ID, "top session")
	assert.Equal(t, 2, resp.Sessions[0].MessageCount, "top MessageCount")
	assert.Equal(t, "top-mixed", resp.Sessions[1].ID, "second session")
	assert.Equal(t, 1, resp.Sessions[1].MessageCount, "second MessageCount")
}

func TestGetAnalyticsTopSessionsModelFilterCapsAtTen(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Twelve sessions all match the gpt-4o filter, so the model-scoped
	// re-sort drops the SQL LIMIT and ranks every matching session. The
	// caller must still cap the result at the top ten. Session i carries
	// i+1 gpt-4o assistant messages of ten output tokens each, so
	// "top-cap-11" has the most messages and the most tokens.
	for i := range 12 {
		id := fmt.Sprintf("top-cap-%d", i)
		count := i + 1
		hour := fmt.Sprintf("%02d", 8+i)
		insertSession(t, d, id, "alpha", func(s *Session) {
			s.StartedAt = new("2024-06-01T" + hour + ":00:00Z")
			s.EndedAt = new("2024-06-01T" + hour + ":30:00Z")
			s.MessageCount = count
			s.Agent = "gpt"
			s.TotalOutputTokens = count * 10
			s.HasTotalOutputTokens = true
		})
		msgs := make([]Message, count)
		for j := range msgs {
			msgs[j] = Message{
				SessionID: id, Ordinal: j, Role: "assistant",
				Content: "gpt", ContentLength: 3,
				Timestamp:    "2024-06-01T" + hour + ":00:00Z",
				Model:        "gpt-4o",
				OutputTokens: 10, HasOutputTokens: true,
			}
		}
		insertMessages(t, d, msgs...)
	}

	for _, metric := range []string{"messages", "output_tokens"} {
		t.Run(metric, func(t *testing.T) {
			resp, err := d.GetAnalyticsTopSessions(ctx, AnalyticsFilter{
				From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
				Model: "gpt-4o",
			}, metric)
			require.NoError(t, err, "GetAnalyticsTopSessions")
			require.Len(t, resp.Sessions, 10,
				"model-filtered top sessions capped at ten")
			assert.Equal(t, "top-cap-11", resp.Sessions[0].ID,
				"highest-count session ranks first")
		})
	}
}

func TestBuildWhereProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	seedAnalyticsData(t, d)

	t.Run("SummaryWithProject", func(t *testing.T) {
		f := baseFilter()
		f.Project = "project-alpha"
		s := mustSummary(t, d, ctx, f)
		assert.Equal(t, 3, s.TotalSessions, "TotalSessions")
	})

	t.Run("SummaryWithNonexistentProject", func(t *testing.T) {
		f := baseFilter()
		f.Project = "nonexistent"
		s := mustSummary(t, d, ctx, f)
		assert.Equal(t, 0, s.TotalSessions, "TotalSessions")
	})
}

func TestAnalyticsTerminationFilter(t *testing.T) {
	t.Run("BuildWherePredicate", func(t *testing.T) {
		cases := []struct {
			name        string
			termination string
			want        string
		}{
			{"empty adds nothing", "", ""},
			{
				"clean", "clean",
				"termination_status = 'clean'",
			},
			{
				"unclean", "unclean",
				"termination_status IN ('tool_call_pending', 'truncated')",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := baseFilter()
				f.Termination = tc.termination
				where, _ := f.buildWhere(
					"COALESCE(NULLIF(started_at, ''), created_at)",
				)
				if tc.want == "" {
					assert.NotContains(t, where, "termination_status",
						"empty termination produced predicate")
					return
				}
				assert.Contains(t, where, tc.want,
					"buildWhere(%q)", tc.termination)
			})
		}
	})

	t.Run("RoundTripQueries", func(t *testing.T) {
		d := testDB(t)
		ctx := context.Background()

		clean := "clean"
		pending := "tool_call_pending"
		truncated := "truncated"

		insert := func(id string, term *string) {
			insertSession(t, d, id, "p", func(s *Session) {
				s.StartedAt = new(string("2024-06-02T09:00:00Z"))
				s.EndedAt = new(string("2024-06-02T10:00:00Z"))
				s.MessageCount = 5
				s.UserMessageCount = 2
				s.TerminationStatus = term
			})
			msgs := make([]Message, 5)
			for i := range msgs {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				msgs[i] = Message{
					SessionID:     id,
					Ordinal:       i,
					Role:          role,
					Content:       "msg",
					ContentLength: 3,
					Timestamp:     "2024-06-02T09:00:00Z",
				}
			}
			insertMessages(t, d, msgs...)
		}

		insert("c1", &clean)
		insert("c2", &clean)
		insert("p1", &pending)
		insert("t1", &truncated)
		insert("n1", nil)

		collect := func(termination string) []string {
			f := baseFilter()
			f.Termination = termination
			resp, err := d.GetAnalyticsTopSessions(
				ctx, f, "messages",
			)
			require.NoError(t, err,
				"GetAnalyticsTopSessions(%q)", termination)
			ids := make([]string, len(resp.Sessions))
			for i, s := range resp.Sessions {
				ids[i] = s.ID
			}
			return ids
		}

		cases := []struct {
			name        string
			termination string
			want        []string
		}{
			{"all", "", []string{"c1", "c2", "p1", "t1", "n1"}},
			{"clean", "clean", []string{"c1", "c2"}},
			{"unclean", "unclean", []string{"p1", "t1"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := collect(tc.termination)
				assertStringSetsEqual(t, got, tc.want)
			})
		}

		t.Run("TopSessionsCarriesTerminationStatus", func(t *testing.T) {
			f := baseFilter()
			f.Termination = "clean"
			resp, err := d.GetAnalyticsTopSessions(
				ctx, f, "messages",
			)
			require.NoError(t, err, "GetAnalyticsTopSessions")
			require.NotEmpty(t, resp.Sessions,
				"expected sessions, got 0")
			for _, s := range resp.Sessions {
				if !assert.NotNil(t, s.TerminationStatus,
					"session %s TerminationStatus want clean",
					s.ID) {
					continue
				}
				assert.Equal(t, "clean", *s.TerminationStatus,
					"session %s TerminationStatus", s.ID)
			}
		})

		t.Run("SummaryFilteredByTermination", func(t *testing.T) {
			f := baseFilter()
			f.Termination = "unclean"
			s := mustSummary(t, d, ctx, f)
			assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
		})
	})
}

// TestTerminationFilterEmptyEndedAt guards the shared SQLite termination
// activity expression. A flagged session whose ended_at was persisted as the
// empty string must still classify by its started_at fallback. Before the fix
// strftime('%s', ”) returned NULL, silently dropping such sessions from the
// active, stale, and unclean scopes that analytics and session selection share
// (usage already used the NULLIF form, so the two diverged).
func TestTerminationFilterEmptyEndedAt(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	pending := "tool_call_pending"
	const oldStart = "2024-06-02T09:00:00Z"

	// Two flagged sessions old enough to be "unclean": one with a normal
	// ended_at (control) and one whose ended_at is the empty string.
	seed := func(id, ended string) {
		insertSession(t, d, id, "p", func(s *Session) {
			s.StartedAt = new(oldStart)
			s.EndedAt = new(ended)
			s.MessageCount = 4
			s.UserMessageCount = 2
			s.TerminationStatus = &pending
		})
		insertMessages(t, d, Message{
			SessionID: id, Ordinal: 0, Role: "assistant",
			Content: "m", ContentLength: 1, Timestamp: oldStart,
		})
	}
	seed("ended", "2024-06-02T10:00:00Z")
	seed("untimed", "")

	// Analytics termination filter.
	f := baseFilter()
	f.Termination = "unclean"
	resp, err := d.GetAnalyticsTopSessions(ctx, f, "messages")
	require.NoError(t, err, "GetAnalyticsTopSessions unclean")
	analyticsIDs := make([]string, len(resp.Sessions))
	for i, s := range resp.Sessions {
		analyticsIDs[i] = s.ID
	}
	assert.ElementsMatch(t, []string{"ended", "untimed"}, analyticsIDs,
		"analytics unclean must include the empty-ended_at session")

	// Session-selection termination filter shares the same expression.
	page, err := d.ListSessions(ctx, SessionFilter{
		Termination: "unclean", Limit: 50,
	})
	require.NoError(t, err, "ListSessions unclean")
	sessionIDs := make([]string, len(page.Sessions))
	for i, s := range page.Sessions {
		sessionIDs[i] = s.ID
	}
	assert.ElementsMatch(t, []string{"ended", "untimed"}, sessionIDs,
		"session list unclean must include the empty-ended_at session")
}

func TestTimeFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Create sessions with messages at known day/hour combos.
	// 2024-06-01 = Saturday (ISO dow 5)
	// 2024-06-03 = Monday   (ISO dow 0)
	insertSession(t, d, "tf1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 2
		s.Agent = "claude"
	})
	insertMessages(t, d,
		Message{
			SessionID: "tf1", Ordinal: 0, Role: "user",
			Timestamp: "2024-06-01T09:05:00Z",
			Content:   "hello", ContentLength: 5,
		},
		Message{
			SessionID: "tf1", Ordinal: 1, Role: "assistant",
			Timestamp: "2024-06-01T09:10:00Z",
			Content:   "hi", ContentLength: 2,
		},
	)

	insertSession(t, d, "tf2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.EndedAt = new("2024-06-01T15:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d, Message{
		SessionID: "tf2", Ordinal: 0, Role: "user",
		Timestamp: "2024-06-01T14:05:00Z",
		Content:   "world", ContentLength: 5,
	})

	insertSession(t, d, "tf3", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-03T09:00:00Z")
		s.EndedAt = new("2024-06-03T10:00:00Z")
		s.MessageCount = 1
		s.Agent = "claude"
	})
	insertMessages(t, d, Message{
		SessionID: "tf3", Ordinal: 0, Role: "user",
		Timestamp: "2024-06-03T09:30:00Z",
		Content:   "test", ContentLength: 4,
	})

	f := AnalyticsFilter{
		From:     "2024-06-01",
		To:       "2024-06-03",
		Timezone: "UTC",
	}

	t.Run("FilterByHour", func(t *testing.T) {
		ff := f
		hour := 9
		ff.Hour = &hour
		s := mustSummary(t, d, ctx, ff)
		// tf1 and tf3 have messages at hour 9
		assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
	})

	t.Run("FilterByDow", func(t *testing.T) {
		ff := f
		dow := 5 // Saturday
		ff.DayOfWeek = &dow
		s := mustSummary(t, d, ctx, ff)
		// tf1 and tf2 are on Saturday
		assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
	})

	t.Run("FilterByDowAndHour", func(t *testing.T) {
		ff := f
		dow := 5
		hour := 14
		ff.DayOfWeek = &dow
		ff.Hour = &hour
		s := mustSummary(t, d, ctx, ff)
		// Only tf2 has messages on Saturday at hour 14
		assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
	})

	t.Run("NoTimeFilter", func(t *testing.T) {
		s := mustSummary(t, d, ctx, f)
		// All 3 sessions
		assert.Equal(t, 3, s.TotalSessions, "TotalSessions")
	})
}

func TestAnalyticsFilterAgentAndMinUserMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "c1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 10
		s.UserMessageCount = 5
		s.Agent = "claude"
	})
	insertSession(t, d, "c2", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T11:00:00Z")
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.MessageCount = 4
		s.UserMessageCount = 1
		s.Agent = "claude"
	})
	insertSession(t, d, "x1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.EndedAt = new("2024-06-01T15:00:00Z")
		s.MessageCount = 20
		s.UserMessageCount = 8
		s.Agent = "codex"
	})

	f := baseFilter()

	t.Run("NoFilters", func(t *testing.T) {
		s := mustSummary(t, d, ctx, f)
		assert.Equal(t, 3, s.TotalSessions, "TotalSessions")
	})

	t.Run("AgentOnly", func(t *testing.T) {
		af := f
		af.Agent = "claude"
		s := mustSummary(t, d, ctx, af)
		assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
	})

	t.Run("MinUserMessagesOnly", func(t *testing.T) {
		af := f
		af.MinUserMessages = 5
		s := mustSummary(t, d, ctx, af)
		assert.Equal(t, 2, s.TotalSessions, "TotalSessions")
	})

	t.Run("AgentAndMinUserMessages", func(t *testing.T) {
		af := f
		af.Agent = "claude"
		af.MinUserMessages = 2
		s := mustSummary(t, d, ctx, af)
		assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
	})

	t.Run("ActiveSince", func(t *testing.T) {
		af := f
		af.ActiveSince = "2024-06-01T13:00:00Z"
		s := mustSummary(t, d, ctx, af)
		assert.Equal(t, 1, s.TotalSessions, "TotalSessions")
	})
}

func TestAutonomyExcludesSystemMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "zencoder"
		s.StartedAt = new(tsMidYear)
		s.EndedAt = new(tsMidYear)
		s.MessageCount = 4
	})

	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "system banner", ContentLength: 13,
			Timestamp: tsMidYear, IsSystem: true},
		{SessionID: "s1", Ordinal: 1, Role: "user",
			Content: "real question", ContentLength: 13,
			Timestamp: tsMidYear},
		{SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: "answer", ContentLength: 6,
			Timestamp: tsMidYear, HasToolUse: true},
		{SessionID: "s1", Ordinal: 3, Role: "user",
			Content: "finish marker", ContentLength: 13,
			Timestamp: tsMidYear, IsSystem: true},
	}
	insertMessages(t, d, msgs...)

	resp, err := d.GetAnalyticsSessionShape(
		context.Background(),
		AnalyticsFilter{
			From: "2024-01-01", To: "2024-12-31",
		},
	)
	requireNoError(t, err, "GetAnalyticsSessionShape")

	// The autonomy ratio should be based on 1 real user message
	// (not 3), yielding ratio = 1/1 = 1.0 -> bucket "1-2".
	for _, b := range resp.AutonomyDistribution {
		if b.Label == "1-2" && b.Count == 1 {
			return // found the expected bucket
		}
	}
	assert.Failf(t, "missing autonomy bucket",
		"expected autonomy bucket '1-2' with count 1, got %v",
		resp.AutonomyDistribution)
}

func TestActivityExcludesSystemUserMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "proj", func(s *Session) {
		s.Agent = "zencoder"
		s.StartedAt = new(tsMidYear)
		s.EndedAt = new(tsMidYear)
		s.MessageCount = 3
	})

	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "system banner", ContentLength: 13,
			Timestamp: tsMidYear, IsSystem: true},
		{SessionID: "s1", Ordinal: 1, Role: "user",
			Content: "real question", ContentLength: 13,
			Timestamp: tsMidYear},
		{SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: "answer", ContentLength: 6,
			Timestamp: tsMidYear},
	}
	insertMessages(t, d, msgs...)

	resp, err := d.GetAnalyticsActivity(
		context.Background(),
		AnalyticsFilter{
			From: "2024-01-01", To: "2024-12-31",
		},
		"day",
	)
	requireNoError(t, err, "GetAnalyticsActivity")

	require.Len(t, resp.Series, 1, "len")
	entry := resp.Series[0]
	// 3 total messages but only 1 real user message
	assert.Equal(t, 3, entry.Messages, "Messages")
	assert.Equal(t, 1, entry.UserMessages, "UserMessages")
	assert.Equal(t, 1, entry.AssistantMessages, "AssistantMessages")
}

func TestGetAnalyticsSignals(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("EmptyDB", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 0)
		assertEq(t, "UnscoredSessions",
			resp.UnscoredSessions, 0)
		assertEq(t, "len(Trend)", len(resp.Trend), 0)
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 0)
		assertEq(t, "len(ByProject)", len(resp.ByProject), 0)
	})

	// Seed sessions with signal data.
	// UpsertSession only writes core fields; signal columns
	// are written by UpdateSessionSignals.
	insertSession(t, d, "sig1", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.MessageCount = 10
		s.Agent = "claude"
	})
	cp1 := 0.6
	updateSignals(t, d, "sig1", SessionSignalUpdate{
		HealthScore:            new(85),
		HealthGrade:            new("B"),
		Outcome:                "completed",
		OutcomeConfidence:      "high",
		ToolFailureSignalCount: 2,
		ToolRetryCount:         1,
		CompactionCount:        1,
		ContextPressureMax:     &cp1,
		QualitySignals: QualitySignals{
			Version:                     CurrentQualitySignalVersion,
			ShortPromptCount:            2,
			UnstructuredStart:           true,
			MissingSuccessCriteriaCount: 1,
			DuplicatePromptCount:        1,
		},
	})
	insertSession(t, d, "sig2", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.MessageCount = 5
		s.Agent = "codex"
	})
	cp2 := 0.9
	updateSignals(t, d, "sig2", SessionSignalUpdate{
		HealthScore:            new(45),
		HealthGrade:            new("D"),
		Outcome:                "errored",
		OutcomeConfidence:      "medium",
		ToolFailureSignalCount: 5,
		ToolRetryCount:         3,
		EditChurnCount:         2,
		ContextPressureMax:     &cp2,
		QualitySignals: QualitySignals{
			Version:                  CurrentQualitySignalVersion,
			MissingVerificationCount: 1,
			NoCodeContextCount:       1,
			RunawayToolLoopCount:     1,
		},
	})
	insertSession(t, d, "sig3", "beta", func(s *Session) {
		s.StartedAt = new("2024-06-02T10:00:00Z")
		s.MessageCount = 8
		s.Agent = "claude"
	})
	updateSignals(t, d, "sig3", SessionSignalUpdate{
		Outcome:           "abandoned",
		OutcomeConfidence: "low",
		CompactionCount:   3,
		// No health score (unscored)
	})

	t.Run("ScoredVsUnscored", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 2)
		assertEq(t, "UnscoredSessions",
			resp.UnscoredSessions, 1)
	})

	t.Run("GradeDistribution", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		assertEq(t, "grade B", resp.GradeDistribution["B"], 1)
		assertEq(t, "grade D", resp.GradeDistribution["D"], 1)
	})

	t.Run("AvgHealthScore", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		require.NotNil(t, resp.AvgHealthScore, "AvgHealthScore")
		// (85 + 45) / 2 = 65.0
		assertEq(t, "AvgHealthScore",
			*resp.AvgHealthScore, 65.0)
	})

	t.Run("QualityHealth", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		if err != nil {
			t.Fatalf("GetAnalyticsSignals: %v", err)
		}
		assertEq(t, "ComputedSessions",
			resp.QualityHealth.ComputedSessions, 2)
		assertEq(t, "ShortPromptCount total",
			resp.QualityHealth.Totals.ShortPromptCount, 2)
		assertEq(t, "ShortPromptCount sessions",
			resp.QualityHealth.SessionsWithSignal.ShortPromptCount, 1)
		assertEq(t, "UnstructuredStart total",
			resp.QualityHealth.Totals.UnstructuredStart, 1)
		assertEq(t, "MissingSuccessCriteriaCount total",
			resp.QualityHealth.Totals.MissingSuccessCriteriaCount, 1)
		assertEq(t, "MissingVerificationCount total",
			resp.QualityHealth.Totals.MissingVerificationCount, 1)
		assertEq(t, "DuplicatePromptCount total",
			resp.QualityHealth.Totals.DuplicatePromptCount, 1)
		assertEq(t, "NoCodeContextCount total",
			resp.QualityHealth.Totals.NoCodeContextCount, 1)
		assertEq(t, "RunawayToolLoopCount total",
			resp.QualityHealth.Totals.RunawayToolLoopCount, 1)
	})

	t.Run("OutcomeDistribution", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		assertEq(t, "completed",
			resp.OutcomeDistribution["completed"], 1)
		assertEq(t, "errored",
			resp.OutcomeDistribution["errored"], 1)
		assertEq(t, "abandoned",
			resp.OutcomeDistribution["abandoned"], 1)
	})

	t.Run("ToolHealth", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		// 2 + 5 + 0 = 7
		assertEq(t, "TotalFailureSignals",
			resp.ToolHealth.TotalFailureSignals, 7)
		// 1 + 3 + 0 = 4
		assertEq(t, "TotalRetries",
			resp.ToolHealth.TotalRetries, 4)
		// 0 + 2 + 0 = 2
		assertEq(t, "TotalEditChurn",
			resp.ToolHealth.TotalEditChurn, 2)
		// sig1 and sig2 have failures
		assertEq(t, "SessionsWithFailures",
			resp.ToolHealth.SessionsWithFailures, 2)
	})

	t.Run("ContextHealth", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		// sig1 (1) and sig3 (3) have compaction > 0
		assertEq(t, "SessionsWithCompaction",
			resp.ContextHealth.SessionsWithCompaction, 2)
		// sig1 and sig2 have context_pressure_max
		assertEq(t, "SessionsWithContextData",
			resp.ContextHealth.SessionsWithContextData, 2)
		// sig2 has pressure >= 0.8
		assertEq(t, "HighPressureSessions",
			resp.ContextHealth.HighPressureSessions, 1)
	})

	t.Run("Trend", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		// 2 dates: 2024-06-01 (2 sessions), 2024-06-02 (1)
		assertEq(t, "len(Trend)", len(resp.Trend), 2)
		// Sorted by date
		assertEq(t, "Trend[0].Date",
			resp.Trend[0].Date, "2024-06-01")
		assertEq(t, "Trend[0].SessionCount",
			resp.Trend[0].SessionCount, 2)
		assertEq(t, "Trend[0].Completed",
			resp.Trend[0].Completed, 1)
		assertEq(t, "Trend[0].Errored",
			resp.Trend[0].Errored, 1)
		assertEq(t, "Trend[1].Date",
			resp.Trend[1].Date, "2024-06-02")
		assertEq(t, "Trend[1].Abandoned",
			resp.Trend[1].Abandoned, 1)
	})

	t.Run("ByAgent", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		// 2 agents: claude (2 sessions), codex (1)
		assertEq(t, "len(ByAgent)", len(resp.ByAgent), 2)
		// Alphabetical: claude first
		assertEq(t, "ByAgent[0].Agent",
			resp.ByAgent[0].Agent, "claude")
		assertEq(t, "ByAgent[0].SessionCount",
			resp.ByAgent[0].SessionCount, 2)
		assertEq(t, "ByAgent[1].Agent",
			resp.ByAgent[1].Agent, "codex")
	})

	t.Run("ByProject", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(ctx, baseFilter())
		require.NoError(t, err, "GetAnalyticsSignals")
		// 2 projects: alpha (2), beta (1)
		// Sorted by session count desc
		assertEq(t, "len(ByProject)", len(resp.ByProject), 2)
		assertEq(t, "ByProject[0].Project",
			resp.ByProject[0].Project, "alpha")
		assertEq(t, "ByProject[0].SessionCount",
			resp.ByProject[0].SessionCount, 2)
	})

	t.Run("ProjectFilter", func(t *testing.T) {
		f := baseFilter()
		f.Project = "beta"
		resp, err := d.GetAnalyticsSignals(ctx, f)
		require.NoError(t, err, "GetAnalyticsSignals")
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 0)
		assertEq(t, "UnscoredSessions",
			resp.UnscoredSessions, 1)
		assertEq(t, "len(ByProject)", len(resp.ByProject), 1)
	})

	t.Run("EmptyDateRange", func(t *testing.T) {
		resp, err := d.GetAnalyticsSignals(
			ctx, emptyFilter(),
		)
		require.NoError(t, err, "GetAnalyticsSignals")
		assertEq(t, "ScoredSessions", resp.ScoredSessions, 0)
	})
}

func TestBuildSignalExamplesUsesObservedOrdinal(t *testing.T) {
	tests := []struct {
		name   string
		signal string
		row    SignalRow
		msgs   []SignalMessage
		want   int
	}{
		{
			name:   "short prompts skip controls",
			signal: "short_prompt_count",
			row: SignalRow{
				ID:               "short",
				ShortPromptCount: 1,
			},
			msgs: []SignalMessage{
				{SessionID: "short", Ordinal: 0, Role: "user", Content: "yes"},
				{SessionID: "short", Ordinal: 3, Role: "user", Content: "fix bug"},
			},
			want: 3,
		},
		{
			name:   "repeated prompts point at repeat",
			signal: "duplicate_prompt_count",
			row: SignalRow{
				ID:                   "repeat",
				DuplicatePromptCount: 1,
			},
			msgs: []SignalMessage{
				{SessionID: "repeat", Ordinal: 0, Role: "user", Content: "Fix the backend test."},
				{SessionID: "repeat", Ordinal: 2, Role: "user", Content: "Fix the backend test."},
			},
			want: 2,
		},
		{
			name:   "tool signals point at tool turn",
			signal: "tool_failure_signals",
			row: SignalRow{
				ID:                     "tool",
				ToolFailureSignalCount: 1,
			},
			msgs: []SignalMessage{
				{SessionID: "tool", Ordinal: 0, Role: "user", Content: "run tests"},
				{SessionID: "tool", Ordinal: 4, Role: "assistant", HasToolUse: true},
			},
			want: 4,
		},
		{
			name:   "outcomes point at last observed turn",
			signal: "outcome_errored",
			row: SignalRow{
				ID:      "outcome",
				Outcome: "errored",
			},
			msgs: []SignalMessage{
				{SessionID: "outcome", Ordinal: 0, Role: "user", Content: "start"},
				{SessionID: "outcome", Ordinal: 6, Role: "assistant", Content: "failed"},
			},
			want: 6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			examples := BuildSignalExamples(
				[]SignalRow{tt.row},
				map[string][]SignalMessage{tt.row.ID: tt.msgs},
				tt.signal,
			)
			if len(examples) != 1 {
				t.Fatalf("len(examples) = %d, want 1",
					len(examples))
			}
			if examples[0].MessageOrdinal == nil {
				t.Fatal("MessageOrdinal is nil")
			}
			if *examples[0].MessageOrdinal != tt.want {
				t.Fatalf("MessageOrdinal = %d, want %d",
					*examples[0].MessageOrdinal, tt.want)
			}
		})
	}
}

func TestGetAnalyticsSignalSessionsRejectsUnsupportedSignal(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	_, err := d.GetAnalyticsSignalSessions(
		ctx,
		baseFilter(),
		"not_a_signal",
		10,
	)
	if !errors.Is(err, ErrUnsupportedAnalyticsSignal) {
		t.Fatalf("err = %v, want ErrUnsupportedAnalyticsSignal", err)
	}
}

func TestGetAnalyticsSignalSessionsModelFilterUsesMatchingMessages(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "signal-mixed", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:10:00Z")
		s.MessageCount = 2
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "signal-mixed", Ordinal: 0, Role: "assistant",
			Content: "claude tool evidence", ContentLength: 20,
			Timestamp:  "2024-06-01T09:05:00Z",
			Model:      "claude-3-5-sonnet",
			HasToolUse: true,
		},
		Message{
			SessionID: "signal-mixed", Ordinal: 1, Role: "assistant",
			Content: "gpt tool evidence", ContentLength: 17,
			Timestamp:  "2024-06-01T09:06:00Z",
			Model:      "gpt-4o",
			HasToolUse: true,
		},
	)
	require.NoError(t, d.UpdateSessionSignals(
		"signal-mixed",
		SessionSignalUpdate{ToolFailureSignalCount: 1},
	))

	resp, err := d.GetAnalyticsSignalSessions(
		ctx,
		AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
			Model:    "gpt-4o",
		},
		"tool_failure_signals",
		10,
	)
	require.NoError(t, err, "GetAnalyticsSignalSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "gpt tool evidence", resp.Sessions[0].Excerpt)
	require.NotNil(t, resp.Sessions[0].MessageOrdinal)
	assert.Equal(t, 1, *resp.Sessions[0].MessageOrdinal)
}

func TestGetAnalyticsSignalSessionsModelFilterKeepsParserUserEvidence(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "signal-parser-user", "alpha", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T09:10:00Z")
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.Agent = "mixed"
	})
	insertMessages(t, d,
		Message{
			SessionID: "signal-parser-user", Ordinal: 0, Role: "user",
			Content: "help", ContentLength: 4,
			Timestamp: "2024-06-01T09:00:00Z",
			Model:     "",
		},
		Message{
			SessionID: "signal-parser-user", Ordinal: 1, Role: "assistant",
			Content: "reply", ContentLength: 5,
			Timestamp:  "2024-06-01T09:01:00Z",
			Model:      "gpt-4o",
			HasToolUse: true,
		},
	)
	updateSignals(t, d, "signal-parser-user", SessionSignalUpdate{
		QualitySignals: QualitySignals{
			Version:          1,
			ShortPromptCount: 1,
		},
	})

	resp, err := d.GetAnalyticsSignalSessions(
		ctx,
		AnalyticsFilter{
			From:     "2024-06-01",
			To:       "2024-06-01",
			Timezone: "UTC",
			Model:    "gpt-4o",
		},
		"short_prompt_count",
		10,
	)
	require.NoError(t, err, "GetAnalyticsSignalSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "help", resp.Sessions[0].Excerpt)
	require.NotNil(t, resp.Sessions[0].MessageOrdinal)
	assert.Equal(t, 0, *resp.Sessions[0].MessageOrdinal)
}

func TestParseEvidenceTimeAcceptsPostgresUTCFormat(t *testing.T) {
	got, ok := parseEvidenceTime("2024-06-01T12:34:56.123456Z")
	if !ok {
		t.Fatal("parseEvidenceTime did not accept PG UTC format")
	}
	if got.UTC().Format(time.RFC3339Nano) !=
		"2024-06-01T12:34:56.123456Z" {
		t.Fatalf("got %s", got.UTC().Format(time.RFC3339Nano))
	}
}

func TestLocalTime(t *testing.T) {
	tests := []struct {
		name  string
		ts    string
		valid bool
	}{
		{"RFC3339", "2024-06-01T15:00:00Z", true},
		{"RFC3339Nano", "2024-06-01T15:00:00.123Z", true},
		{"NoFraction", "2024-06-01T15:00:00Z", true},
		{"BadFormat", "2024-06-01", false},
		{"Empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := localTime(tt.ts, time.UTC)
			assert.Equal(t, tt.valid, ok,
				"localTime(%q) ok", tt.ts)
		})
	}
}

func TestSQLiteTimeModifier(t *testing.T) {
	t.Run("UTC", func(t *testing.T) {
		modifier, ok := AnalyticsFilter{
			From:     "2026-06-01",
			To:       "2026-06-07",
			Timezone: "UTC",
		}.sqliteTimeModifier()
		require.True(t, ok)
		assert.Equal(t, "", modifier)
	})

	t.Run("StableOffset", func(t *testing.T) {
		modifier, ok := AnalyticsFilter{
			From:     "2026-06-01",
			To:       "2026-06-07",
			Timezone: "America/New_York",
		}.sqliteTimeModifier()
		require.True(t, ok)
		assert.Equal(t, "-04:00", modifier)
	})

	t.Run("DSTCrossingFallsBack", func(t *testing.T) {
		_, ok := AnalyticsFilter{
			From:     "2026-03-01",
			To:       "2026-03-31",
			Timezone: "America/New_York",
		}.sqliteTimeModifier()
		assert.False(t, ok)
	})
}
