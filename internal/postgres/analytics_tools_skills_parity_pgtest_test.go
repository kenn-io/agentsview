//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// TestAnalyticsToolsSkillsSQLiteParity pushes one SQLite fixture to
// PostgreSQL and requires both stores to return identical tool and skill
// analytics. The fixture crosses the America/New_York spring-forward
// boundary, a local-versus-UTC month edge, and a session whose calls span
// several days, and it carries blank tool names, timestamp-less messages,
// a session with no timestamp at all, and sessions each default filter
// must exclude.
func TestAnalyticsToolsSkillsSQLiteParity(t *testing.T) {
	const schema = "agentsview_tools_skills_parity_test"
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })

	local := testDB(t)
	seedToolsSkillsParityFixture(t, local)

	syncer, err := New(
		pgURL, schema, local, "parity-machine", true, storage.PusherOptions{},
	)
	require.NoError(t, err, "create PostgreSQL sync")
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err, "push parity fixture")

	remote, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "open PostgreSQL store")
	t.Cleanup(func() { require.NoError(t, remote.Close()) })

	// Mark the pushed copy deleted so PostgreSQL's own deleted-session
	// exclusion is exercised however push mirrors soft deletion.
	res, err := remote.DB().ExecContext(t.Context(),
		`UPDATE sessions SET deleted_at = NOW() WHERE id = 'deleted'`)
	require.NoError(t, err, "soft-delete remote session")
	deleted, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted, "deleted session was pushed")

	// Push requires a created_at, so clear the no-time session's timestamps
	// in both stores after the push. SQLite stores a missing created_at as
	// an empty string and PostgreSQL as NULL.
	require.NoError(t, local.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE sessions SET started_at = NULL, created_at = '' WHERE id = 'no-time'`)
		return err
	}), "clear SQLite session timestamps")
	res, err = remote.DB().ExecContext(t.Context(),
		`UPDATE sessions SET started_at = NULL, created_at = NULL WHERE id = 'no-time'`)
	require.NoError(t, err, "clear PostgreSQL session timestamps")
	cleared, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, cleared, "no-time session was pushed")

	hour3, monday := 3, 0
	march := func(tz string) db.AnalyticsFilter {
		return db.AnalyticsFilter{
			From: "2024-03-01", To: "2024-03-31", Timezone: tz,
			ExcludeOneShot: true, ExcludeAutomated: true,
		}
	}
	cases := []struct {
		name      string
		wantCalls int
		filter    func() db.AnalyticsFilter
	}{
		{"new-york-march", 9, func() db.AnalyticsFilter { return march("America/New_York") }},
		{"utc-march", 8, func() db.AnalyticsFilter { return march("UTC") }},
		{"tokyo-march", 8, func() db.AnalyticsFilter { return march("Asia/Tokyo") }},
		{"new-york-spring-forward-day", 3, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.From, f.To = "2024-03-10", "2024-03-10"
			return f
		}},
		{"model", 6, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.Model = "model-a"
			return f
		}},
		{"multi-model", 9, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.Model = "model-a, model-b"
			return f
		}},
		{"hour-after-dst", 1, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.Hour = &hour3
			return f
		}},
		{"monday-model", 2, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.DayOfWeek = &monday
			f.Model = "model-a"
			return f
		}},
		{"project", 2, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.Project = "beta"
			return f
		}},
		{"subagents-and-automated", 11, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.IncludeSubagents = true
			f.AutomatedScope = "all"
			return f
		}},
		{"automated-only", 1, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.AutomatedScope = "automated"
			return f
		}},
		{"include-one-shot", 10, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.ExcludeOneShot = false
			return f
		}},
		// The no-time session's call has an empty date. SQLite keeps it
		// under an upper bound alone and drops it under a lower bound.
		{"upper-bound-only", 11, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.From = ""
			return f
		}},
		{"lower-bound-only", 9, func() db.AnalyticsFilter {
			f := march("America/New_York")
			f.To = ""
			return f
		}},
		{"all-time", 13, func() db.AnalyticsFilter {
			return db.AnalyticsFilter{Timezone: "UTC"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.filter()
			wantTools, err := local.GetAnalyticsTools(t.Context(), f)
			require.NoError(t, err, "SQLite tools")
			gotTools, err := remote.GetAnalyticsTools(t.Context(), f)
			require.NoError(t, err, "PostgreSQL tools")
			assert.Equal(t, wantTools, gotTools, "tools parity")
			assert.Equal(t, tc.wantCalls, gotTools.TotalCalls, "total calls")
			for _, granularity := range []string{"day", "week", "month", ""} {
				wantSkills, err := local.GetAnalyticsSkills(t.Context(), f, granularity)
				require.NoError(t, err, "SQLite skills %q", granularity)
				gotSkills, err := remote.GetAnalyticsSkills(t.Context(), f, granularity)
				require.NoError(t, err, "PostgreSQL skills %q", granularity)
				assert.Equal(t, wantSkills, gotSkills, "skills parity %q", granularity)
			}
		})
	}

	t.Run("new-york-literal", func(t *testing.T) {
		f := march("America/New_York")
		tools, err := remote.GetAnalyticsTools(t.Context(), f)
		require.NoError(t, err)
		assert.Equal(t, 9, tools.TotalCalls)
		assert.Equal(t, []db.ToolUsageAnalysis{
			{ToolName: "Read", Category: "Read", CallCount: 5, SessionCount: 3, Pct: 55.6},
			{ToolName: "Bash", Category: "Bash", CallCount: 1, SessionCount: 1, Pct: 11.1},
			{ToolName: "Edit", Category: "Edit", CallCount: 1, SessionCount: 1, Pct: 11.1},
			{ToolName: "Grep", Category: "Grep", CallCount: 1, SessionCount: 1, Pct: 11.1},
			{ToolName: "Unknown", Category: "Other", CallCount: 1, SessionCount: 1, Pct: 11.1},
		}, tools.ByTool)
		assert.Equal(t, []db.ToolTrendEntry{
			{Date: "2024-03-04", ByCat: map[string]int{"Read": 3, "Bash": 1, "Other": 1, "Grep": 1}},
			{Date: "2024-03-11", ByCat: map[string]int{"Read": 2}},
			{Date: "2024-03-25", ByCat: map[string]int{"Edit": 1}},
		}, tools.Trend)

		skills, err := remote.GetAnalyticsSkills(t.Context(), f, "month")
		require.NoError(t, err)
		assert.Equal(t, 6, skills.TotalSkillCalls)
		assert.Equal(t, []db.SkillUsage{
			{
				SkillName: "review-code", CallCount: 4, SessionCount: 2,
				AgentBreakdown: []db.SkillAgentBreakdown{
					{Agent: "claude", Count: 3}, {Agent: "codex", Count: 1},
				},
				ProjectBreakdown: []db.SkillProjectBreakdown{
					{Project: "alpha", Count: 3}, {Project: "beta", Count: 1},
				},
				LastUsedAt: "2024-03-11T13:00:00Z", Pct: 66.7,
			},
			{
				SkillName: "write-tests", CallCount: 2, SessionCount: 2,
				AgentBreakdown: []db.SkillAgentBreakdown{
					{Agent: "claude", Count: 1}, {Agent: "codex", Count: 1},
				},
				ProjectBreakdown: []db.SkillProjectBreakdown{
					{Project: "alpha", Count: 1}, {Project: "beta", Count: 1},
				},
				LastUsedAt: "2024-04-01T03:30:00Z", Pct: 33.3,
			},
		}, skills.BySkill)
		assert.Equal(t, []db.SkillTrendEntry{
			{Date: "2024-03-01", BySkill: map[string]int{"review-code": 4, "write-tests": 2}},
		}, skills.Trend)
	})

	t.Run("local-timezone", func(t *testing.T) {
		newYork, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		t.Setenv("TZ", "America/New_York")
		origLocal := time.Local                      //nolint:forbidigo // "Local" analytics follow the process timezone.
		t.Cleanup(func() { time.Local = origLocal }) //nolint:forbidigo // "Local" analytics follow the process timezone.
		time.Local = newYork                         //nolint:forbidigo // "Local" analytics follow the process timezone.

		f := march("Local")
		wantTools, err := local.GetAnalyticsTools(t.Context(), f)
		require.NoError(t, err, "SQLite tools")
		gotTools, err := remote.GetAnalyticsTools(t.Context(), f)
		require.NoError(t, err, "PostgreSQL tools")
		assert.Equal(t, wantTools, gotTools, "tools parity")
		assert.Equal(t, 9, gotTools.TotalCalls,
			"New York places the 23:30 EST March 9 calls in March; UTC has 8")
		wantSkills, err := local.GetAnalyticsSkills(t.Context(), f, "day")
		require.NoError(t, err, "SQLite skills")
		gotSkills, err := remote.GetAnalyticsSkills(t.Context(), f, "day")
		require.NoError(t, err, "PostgreSQL skills")
		assert.Equal(t, wantSkills, gotSkills, "skills parity")
	})

	t.Run("spring-forward-literal", func(t *testing.T) {
		f := march("America/New_York")
		f.From, f.To = "2024-03-10", "2024-03-10"
		tools, err := remote.GetAnalyticsTools(t.Context(), f)
		require.NoError(t, err)
		assert.Equal(t, 3, tools.TotalCalls,
			"00:30 EST, 03:30 EDT and the late-start call; the 23:30 EST "+
				"call and the timestamp-less fallback fall on March 9")

		f.From, f.To = "2024-03-01", "2024-03-31"
		f.Hour = &hour3
		tools, err = remote.GetAnalyticsTools(t.Context(), f)
		require.NoError(t, err)
		assert.Equal(t, []db.ToolUsageAnalysis{
			{ToolName: "Unknown", Category: "Other", CallCount: 1, SessionCount: 1, Pct: 100},
		}, tools.ByTool, "07:30Z is 03:30 EDT after the spring-forward jump")
	})
}

func seedToolsSkillsParityFixture(t *testing.T, local *db.DB) {
	t.Helper()
	type call struct{ tool, category, skill string }
	type msg struct {
		ts, model string
		calls     []call
	}
	type session struct {
		id, agent, project, startedAt, relationship string
		automated                                   bool
		// userMessages defaults to 3, which every one-shot filter keeps.
		userMessages int
		msgs         []msg
	}
	read := call{tool: "Read", category: "Read"}
	sessions := []session{
		{
			id: "dst-span", agent: "claude", project: "alpha",
			startedAt: "2024-03-09T23:00:00Z",
			msgs: []msg{
				// 23:30 EST on Saturday March 9; already March 10 in UTC.
				{ts: "2024-03-10T04:30:00Z", model: "model-a", calls: []call{
					read, {tool: "Read", category: "Read", skill: "review-code"},
				}},
				{ts: "2024-03-10T05:30:00Z", model: "model-a", calls: []call{
					{tool: "Bash", category: "Bash"},
				}},
				// 03:30 EDT, just after the 02:00 spring-forward jump.
				{ts: "2024-03-10T07:30:00Z", model: "model-b", calls: []call{
					{tool: "  ", category: "Other", skill: "write-tests"},
				}},
				// Monday in the next ISO week.
				{ts: "2024-03-11T13:00:00Z", model: "model-a", calls: []call{
					{tool: "Read", category: "Read", skill: "review-code"},
				}},
				// No timestamp: falls back to the session start.
				{model: "model-b", calls: []call{
					{tool: "Grep", category: "Grep", skill: "review-code"},
				}},
			},
		},
		{
			id: "second", agent: "codex", project: "beta",
			startedAt: "2024-03-11T12:00:00Z",
			msgs: []msg{
				{ts: "2024-03-11T12:05:00Z", model: "model-a", calls: []call{
					{tool: "Read", category: "Read", skill: "review-code"},
				}},
				// 23:30 EDT on March 31; April in UTC.
				{ts: "2024-04-01T03:30:00Z", model: "model-b", calls: []call{
					{tool: "Edit", category: "Edit", skill: "write-tests"},
				}},
			},
		},
		{
			// Started before the range; its call is inside it.
			id: "late-start", agent: "claude", project: "alpha",
			startedAt: "2024-02-28T12:00:00Z",
			msgs:      []msg{{ts: "2024-03-10T14:00:00Z", model: "model-a", calls: []call{read}}},
		},
		{
			id: "subagent", agent: "claude", project: "alpha",
			startedAt: "2024-03-10T15:00:00Z", relationship: "subagent",
			msgs: []msg{{ts: "2024-03-10T15:01:00Z", model: "model-a", calls: []call{
				{tool: "Read", category: "Read", skill: "review-code"},
			}}},
		},
		{
			id: "automated", agent: "codex", project: "beta",
			startedAt: "2024-03-12T15:00:00Z", automated: true,
			msgs: []msg{{ts: "2024-03-12T15:01:00Z", model: "model-b", calls: []call{
				{tool: "Write", category: "Write", skill: "write-tests"},
			}}},
		},
		{
			id: "deleted", agent: "claude", project: "alpha",
			startedAt: "2024-03-12T16:00:00Z",
			msgs: []msg{{ts: "2024-03-12T16:01:00Z", model: "model-a", calls: []call{
				{tool: "Read", category: "Read", skill: "review-code"},
			}}},
		},
		{
			id: "one-shot", agent: "claude", project: "alpha",
			startedAt: "2024-03-13T15:00:00Z", userMessages: 1,
			msgs: []msg{{ts: "2024-03-13T15:01:00Z", model: "model-a", calls: []call{
				{tool: "Glob", category: "Glob", skill: "review-code"},
			}}},
		},
		{
			// Neither the session nor its message has a timestamp; the
			// test clears the session's timestamps after the push.
			id: "no-time", agent: "codex", project: "beta",
			startedAt: "2024-03-14T15:00:00Z",
			msgs: []msg{{model: "model-b", calls: []call{
				{tool: "Task", category: "Task", skill: "write-tests"},
			}}},
		},
		{
			id: "february", agent: "claude", project: "alpha",
			startedAt: "2024-02-01T12:00:00Z",
			msgs: []msg{{ts: "2024-02-01T12:01:00Z", model: "model-a", calls: []call{
				{tool: "Read", category: "Read", skill: "review-code"},
			}}},
		},
	}
	for _, s := range sessions {
		startedAt := s.startedAt
		userMessages := s.userMessages
		if userMessages == 0 {
			userMessages = 3
		}
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: s.id, Project: s.project, Machine: "parity-machine",
			Agent: s.agent, StartedAt: &startedAt,
			MessageCount: len(s.msgs), UserMessageCount: userMessages,
			RelationshipType: s.relationship, IsAutomated: s.automated,
		}), "seed session %s", s.id)
		msgs := make([]db.Message, len(s.msgs))
		for i, m := range s.msgs {
			calls := make([]db.ToolCall, len(m.calls))
			for j, c := range m.calls {
				calls[j] = db.ToolCall{
					SessionID: s.id, ToolName: c.tool, Category: c.category,
					SkillName: c.skill, CallIndex: j,
				}
			}
			msgs[i] = db.Message{
				SessionID: s.id, Ordinal: i, Role: "assistant",
				Content: "call", Timestamp: m.ts, Model: m.model,
				HasToolUse: true, ToolCalls: calls,
			}
		}
		require.NoError(t, local.InsertMessages(t.Context(), msgs),
			"seed messages %s", s.id)
	}
	require.NoError(t, local.SoftDeleteSession(t.Context(), "deleted"))
}
