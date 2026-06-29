//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// TestDuckBuildAnalyticsWhereSubagents verifies that the DuckDB
// analytics WHERE builder mirrors the SQLite rule: when subagents are
// counted, the relationship filter stops excluding them and the
// one-shot exclusion exempts subagent rows (workflow subagents are
// inherently one-shot). The exemption is hand-written DuckDB SQL, so
// this guards its column qualification across prefixed/unprefixed
// callers.
func TestDuckBuildAnalyticsWhereSubagents(t *testing.T) {
	t.Run("root-only default", func(t *testing.T) {
		f := db.AnalyticsFilter{ExcludeOneShot: true}
		where, _ := duckBuildAnalyticsWhere(f, "started_at", "", false, false)
		assert.Contains(t, where,
			"relationship_type NOT IN ('subagent', 'fork')")
		assert.NotContains(t, where, "OR relationship_type = 'subagent'")
	})

	t.Run("include subagents, unprefixed", func(t *testing.T) {
		f := db.AnalyticsFilter{ExcludeOneShot: true, IncludeSubagents: true}
		where, _ := duckBuildAnalyticsWhere(f, "started_at", "", false, false)
		assert.Contains(t, where, "relationship_type NOT IN ('fork')")
		assert.NotContains(t, where, "NOT IN ('subagent'")
		// One-shot exclusion exempts subagents.
		assert.Contains(t, where, "OR relationship_type = 'subagent')")
	})

	t.Run("include subagents, prefixed columns stay qualified", func(t *testing.T) {
		f := db.AnalyticsFilter{ExcludeOneShot: true, IncludeSubagents: true}
		where, _ := duckBuildAnalyticsWhere(f, "s.started_at", "s.", false, false)
		assert.Contains(t, where, "s.relationship_type NOT IN ('fork')")
		assert.Contains(t, where, "OR s.relationship_type = 'subagent')")
		// No unqualified relationship_type leaks through.
		assert.False(t,
			strings.Contains(where, " relationship_type") ||
				strings.HasPrefix(where, "relationship_type"),
			"relationship_type must be table-qualified: %s", where)
	})
}

func TestDuckAnalyticsAutomatedScopePredicates(t *testing.T) {
	tests := []struct {
		name    string
		filter  db.AnalyticsFilter
		want    string
		notWant string
	}{
		{
			name: "legacy exclude automated normalizes to human",
			filter: db.AnalyticsFilter{
				ExcludeAutomated: true,
			},
			want: "s.is_automated = FALSE",
		},
		{
			name: "all scope suppresses legacy human filter",
			filter: db.AnalyticsFilter{
				AutomatedScope:   "all",
				ExcludeAutomated: true,
			},
			notWant: "s.is_automated = FALSE",
		},
		{
			name: "automated scope selects automated sessions",
			filter: db.AnalyticsFilter{
				AutomatedScope: "automated",
			},
			want:    "s.is_automated = TRUE",
			notWant: "s.is_automated = FALSE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, _ := duckBuildAnalyticsWhere(
				tt.filter,
				"COALESCE(s.started_at, s.created_at)",
				"s.",
				false,
				false,
			)
			if tt.want != "" {
				assert.Contains(t, sql, tt.want,
					"DuckDB analytics SQL missing expected predicate")
			}
			if tt.notWant != "" {
				assert.NotContains(t, sql, tt.notWant,
					"DuckDB analytics SQL has unexpected predicate")
			}
		})
	}
}

func TestDuckAnalyticsAutomatedScopeOneShotExemption(t *testing.T) {
	sql, _ := duckBuildAnalyticsWhere(
		db.AnalyticsFilter{
			AutomatedScope: "automated",
			ExcludeOneShot: true,
		},
		"COALESCE(s.started_at, s.created_at)",
		"s.",
		false,
		false,
	)
	want := "(s.user_message_count > 1 OR s.is_automated = TRUE)"
	assert.Contains(t, sql, want,
		"DuckDB analytics SQL missing one-shot exemption")
}

func TestDuckAnalyticsModelFilterPredicates(t *testing.T) {
	sql, _ := duckBuildAnalyticsWhere(
		db.AnalyticsFilter{Model: "gpt-4o"},
		"COALESCE(s.started_at, s.created_at)",
		"s.",
		false,
		false,
	)
	assert.Contains(t, sql,
		"EXISTS (SELECT 1 FROM messages m WHERE m.session_id = s.id AND m.model = ?)")
}

func TestDuckAnalyticsModelAndHourUseSameMessagePredicate(t *testing.T) {
	hour := 10
	sql, _ := duckBuildAnalyticsWhere(
		db.AnalyticsFilter{
			Model: "gpt-4o",
			Hour:  &hour,
		},
		"COALESCE(s.started_at, s.created_at)",
		"s.",
		false,
		true,
	)
	assert.Contains(t, sql, "m.session_id = s.id")
	assert.Contains(t, sql, "m.model = ?")
	assert.Contains(t, sql, "CAST(strftime(")
}

func TestDuckUsageTerminationPredicate(t *testing.T) {
	where, args := appendDuckUsageSessionFilterClauses(
		"WHERE true",
		nil,
		db.UsageFilter{Termination: "clean,unclean"},
		"",
	)

	assert.Contains(t, where, "s.termination_status = 'clean'")
	assert.Contains(t, where, "s.termination_status IN ('tool_call_pending', 'truncated')")
	assert.Contains(t, where, "COALESCE(s.ended_at, s.started_at, s.created_at) <= CAST(? AS TIMESTAMP)")
	require.Len(t, args, 1)
	_, ok := args[0].(string)
	assert.True(t, ok, "termination cutoff should be bound as a timestamp string")
}

func TestDuckUsageAutomatedScopePredicates(t *testing.T) {
	tests := []struct {
		name    string
		filter  db.UsageFilter
		want    string
		notWant string
	}{
		{
			name:   "legacy exclude automated normalizes to human",
			filter: db.UsageFilter{ExcludeAutomated: true},
			want:   "COALESCE(s.is_automated, FALSE) = FALSE",
		},
		{
			name: "all scope suppresses legacy human filter",
			filter: db.UsageFilter{
				AutomatedScope:   "all",
				ExcludeAutomated: true,
			},
			notWant: "COALESCE(s.is_automated, FALSE) = FALSE",
		},
		{
			name:    "automated scope selects automated sessions",
			filter:  db.UsageFilter{AutomatedScope: "automated"},
			want:    "COALESCE(s.is_automated, FALSE) = TRUE",
			notWant: "COALESCE(s.is_automated, FALSE) = FALSE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			where, _ := appendDuckUsageSessionFilterClauses(
				"WHERE true", nil, tt.filter, "")
			if tt.want != "" {
				assert.Contains(t, where, tt.want,
					"DuckDB usage SQL missing expected predicate")
			}
			if tt.notWant != "" {
				assert.NotContains(t, where, tt.notWant,
					"DuckDB usage SQL has unexpected predicate")
			}
		})
	}
}

func TestDuckUsageAutomatedScopeOneShotExemption(t *testing.T) {
	where, _ := appendDuckUsageSessionFilterClauses(
		"WHERE true",
		nil,
		db.UsageFilter{AutomatedScope: "automated", ExcludeOneShot: true},
		"",
	)
	assert.Contains(t, where,
		"(s.user_message_count > 1 OR COALESCE(s.is_automated, FALSE) = TRUE)",
		"DuckDB usage SQL missing non-human one-shot exemption")

	humanWhere, _ := appendDuckUsageSessionFilterClauses(
		"WHERE true",
		nil,
		db.UsageFilter{AutomatedScope: "human", ExcludeOneShot: true},
		"",
	)
	assert.Contains(t, humanWhere, "AND s.user_message_count > 1",
		"DuckDB usage human one-shot must be a plain threshold")
	assert.NotContains(t, humanWhere,
		"OR COALESCE(s.is_automated, FALSE) = TRUE",
		"DuckDB usage human one-shot must not exempt automated sessions")
}

func TestDuckSignalMessagesFormatsTimestampValues(t *testing.T) {
	ctx := context.Background()
	store, _ := newSyncedStore(t)

	_, err := store.duck.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp,
			is_system, has_tool_use
		) VALUES
			(9101, 'signal-time', 0, 'user', 'with timestamp',
			 CAST('2026-01-20T12:34:56Z' AS TIMESTAMP), FALSE, FALSE),
			(9102, 'signal-time', 1, 'assistant', 'without timestamp',
			 NULL, FALSE, FALSE)`)
	require.NoError(t, err)

	got, err := store.duckSignalMessages(
		ctx,
		[]db.SignalRow{{ID: "signal-time"}},
		db.AnalyticsFilter{},
	)
	require.NoError(t, err)
	require.Len(t, got["signal-time"], 2)
	assert.Equal(t, "2026-01-20T12:34:56Z", got["signal-time"][0].Timestamp)
	assert.Empty(t, got["signal-time"][1].Timestamp)
}

func TestDuckAnalyticsSignalSessionsModelFilterUsesMatchingMessages(
	t *testing.T,
) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-signal-mixed", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-signal-mixed", 0, "assistant",
					"claude tool evidence",
					"2024-06-01T09:05:00Z",
					"claude-3-5-sonnet",
					db.ToolCall{ToolName: "Grep", Category: "Grep"},
				),
				duckModelMessage(
					"duck-signal-mixed", 1, "assistant",
					"gpt tool evidence",
					"2024-06-01T09:06:00Z",
					"gpt-4o",
					db.ToolCall{ToolName: "Read", Category: "Read"},
				),
			},
			Signals: db.SessionSignalUpdate{
				ToolFailureSignalCount: 1,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "tool_failure_signals", 10)
	require.NoError(t, err, "GetAnalyticsSignalSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "gpt tool evidence", resp.Sessions[0].Excerpt)
	require.NotNil(t, resp.Sessions[0].MessageOrdinal)
	assert.Equal(t, 1, *resp.Sessions[0].MessageOrdinal)
}

func TestDuckAnalyticsSignalSessionsModelFilterKeepsParserUserEvidence(
	t *testing.T,
) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-signal-parser-user", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-signal-parser-user", 0, "user",
					"help", "2024-06-01T09:00:00Z", "",
				),
				duckModelMessage(
					"duck-signal-parser-user", 1, "assistant",
					"reply", "2024-06-01T09:01:00Z", "gpt-4o",
					db.ToolCall{ToolName: "Read", Category: "Read"},
				),
			},
			Signals: db.SessionSignalUpdate{
				QualitySignals: db.QualitySignals{
					Version:          1,
					ShortPromptCount: 1,
				},
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsSignalSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "short_prompt_count", 10)
	require.NoError(t, err, "GetAnalyticsSignalSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "help", resp.Sessions[0].Excerpt)
	require.NotNil(t, resp.Sessions[0].MessageOrdinal)
	assert.Equal(t, 0, *resp.Sessions[0].MessageOrdinal)
}

func TestDuckAnalyticsSummaryModelFilterPopulatesModels(t *testing.T) {
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession("duck-model-a", "alpha", "gpt", start, 1),
			Messages: []db.Message{
				duckModelMessage("duck-model-a", 0, "assistant", "gpt", start, "gpt-4o"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession("duck-model-b", "alpha", "claude", start, 1),
			Messages: []db.Message{
				duckModelMessage("duck-model-b", 0, "assistant", "claude", start, "claude-3-5-sonnet"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
}

func TestDuckAnalyticsMixedModelFilters(t *testing.T) {
	ctx := context.Background()
	store := newDuckMixedModelAnalyticsStore(t)

	t.Run("summary counts only matching messages", func(t *testing.T) {
		assertDuckAnalyticsSummaryModelFilterCountsOnlyMatchingMessages(t, ctx, store)
	})
	t.Run("activity counts only matching messages", func(t *testing.T) {
		assertDuckAnalyticsActivityModelFilterCountsOnlyMatchingMessages(t, ctx, store)
	})
	t.Run("hour of week counts only matching messages", func(t *testing.T) {
		assertDuckAnalyticsHourOfWeekModelFilterCountsOnlyMatchingMessages(t, ctx, store)
	})
	t.Run("tools count only matching calls", func(t *testing.T) {
		assertDuckAnalyticsToolsModelFilterCountsOnlyMatchingToolCalls(t, ctx, store)
	})
	t.Run("skills count only matching calls", func(t *testing.T) {
		assertDuckAnalyticsSkillsModelFilterCountsOnlyMatchingSkillCalls(t, ctx, store)
	})
	t.Run("projects count only matching messages", func(t *testing.T) {
		assertDuckAnalyticsProjectsModelFilterCountsOnlyMatchingMessages(t, ctx, store)
	})
	t.Run("heatmap counts only matching messages", func(t *testing.T) {
		assertDuckAnalyticsHeatmapModelFilterCountsOnlyMatchingMessages(t, ctx, store)
	})
}

func assertDuckAnalyticsSummaryModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, 1, resp.TotalMessages, "TotalMessages")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
	assert.Equal(t, 1.0, resp.AvgMessages, "AvgMessages")
	assert.Equal(t, 1, resp.MedianMessages, "MedianMessages")
	assert.Equal(t, 1, resp.P90Messages, "P90Messages")
	require.Len(t, resp.Agents, 1, "len(Agents)")
	for _, summary := range resp.Agents {
		assert.Equal(t, 1, summary.Messages, "AgentMessages")
	}
}

func TestDuckAnalyticsSummaryModelsUseMatchingHourRowsOnly(t *testing.T) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-summary-hour-mixed", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-summary-hour-mixed", 0, "assistant", "gpt",
					"2024-06-01T09:05:00Z", "gpt-4o",
				),
				duckModelMessage(
					"duck-summary-hour-mixed", 1, "assistant", "claude",
					"2024-06-01T10:05:00Z", "claude-3-5-sonnet",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	hour := 9
	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 1, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
}

func TestDuckAnalyticsSummaryModelFilterUsesFilteredOutputTokens(
	t *testing.T,
) {
	ctx := context.Background()

	mixedSession := syncSession(
		"duck-summary-output-mixed", "alpha", "mixed",
		"2024-06-01T10:00:00Z", 2,
	)
	mixedSession.TotalOutputTokens = 111
	mixedSession.HasTotalOutputTokens = true
	mixedGpt := duckModelMessage(
		"duck-summary-output-mixed", 0, "assistant", "gpt",
		"2024-06-01T10:00:00Z", "gpt-4o",
	)
	mixedGpt.TokenUsage = []byte(`{"output_tokens":11}`)
	mixedGpt.OutputTokens = 11
	mixedGpt.HasOutputTokens = true
	mixedClaude := duckModelMessage(
		"duck-summary-output-mixed", 1, "assistant", "claude",
		"2024-06-01T10:05:00Z", "claude-3-5-sonnet",
	)
	mixedClaude.TokenUsage = []byte(`{"output_tokens":100}`)
	mixedClaude.OutputTokens = 100
	mixedClaude.HasOutputTokens = true

	uncoveredSession := syncSession(
		"duck-summary-output-uncovered", "alpha", "mixed",
		"2024-06-01T10:40:00Z", 2,
	)
	uncoveredSession.TotalOutputTokens = 90
	uncoveredSession.HasTotalOutputTokens = true
	uncoveredGpt := duckModelMessage(
		"duck-summary-output-uncovered", 0, "assistant", "gpt",
		"2024-06-01T10:40:00Z", "gpt-4o",
	)
	uncoveredGpt.TokenUsage = nil
	uncoveredGpt.ContextTokens = 0
	uncoveredGpt.OutputTokens = 0
	uncoveredGpt.HasContextTokens = false
	uncoveredGpt.HasOutputTokens = false
	uncoveredClaude := duckModelMessage(
		"duck-summary-output-uncovered", 1, "assistant", "claude",
		"2024-06-01T10:45:00Z", "claude-3-5-sonnet",
	)
	uncoveredClaude.TokenUsage = []byte(`{"output_tokens":90}`)
	uncoveredClaude.OutputTokens = 90
	uncoveredClaude.HasOutputTokens = true

	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: mixedSession,
			Messages: []db.Message{
				mixedGpt,
				mixedClaude,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: uncoveredSession,
			Messages: []db.Message{
				uncoveredGpt,
				uncoveredClaude,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	hour := 10
	resp, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsSummary")
	assert.Equal(t, 2, resp.TotalSessions, "TotalSessions")
	assert.Equal(t, 2, resp.TotalMessages, "TotalMessages")
	assert.Equal(t, []string{"gpt-4o"}, resp.Models, "Models")
	assert.Equal(t, 11, resp.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 1, resp.TokenReportingSessions, "TokenReportingSessions")
}

func assertDuckAnalyticsActivityModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].AssistantMessages,
		"AssistantMessages")
	assert.Equal(t, 2, resp.Series[0].ToolCalls, "ToolCalls")
}

func TestDuckAnalyticsActivityModelAndHourFilterCountsOnlyMatchingHourRows(
	t *testing.T,
) {
	ctx := context.Background()

	readMsg := duckModelMessage(
		"duck-activity-hour-gpt", 0, "assistant", "read",
		"2024-06-01T09:00:00Z", "gpt-4o",
		db.ToolCall{
			ToolName: "Read", Category: "Read",
		},
		db.ToolCall{
			ToolName: "Bash", Category: "Bash",
		},
	)
	grepMsg := duckModelMessage(
		"duck-activity-hour-gpt", 1, "assistant", "grep",
		"2024-06-01T10:00:00Z", "gpt-4o",
		db.ToolCall{
			ToolName: "Grep", Category: "Grep",
		},
	)
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-activity-hour-gpt", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages: []db.Message{
				readMsg,
				grepMsg,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	hour := 10
	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].AssistantMessages,
		"AssistantMessages")
	assert.Equal(t, 1, resp.Series[0].ToolCalls, "ToolCalls")
}

func assertDuckAnalyticsHourOfWeekModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 1, duckHOWMessages(resp.Cells, 5, 9), "Sat 09:00")
	assert.Equal(t, 0, duckHOWMessages(resp.Cells, 5, 10), "Sat 10:00")
}

func TestDuckAnalyticsHourOfWeekModelFilterIncludesPairedUserTurns(
	t *testing.T,
) {
	ctx := context.Background()
	start := "2024-06-01T09:00:00Z"
	reply := "2024-06-01T10:00:00Z"

	userMsg := syncMessage("duck-paired-model", 0, "user", "q", start)
	userMsg.Model = ""
	assistantMsg := duckModelMessage(
		"duck-paired-model", 1, "assistant", "a", reply, "gpt-4o",
	)
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-paired-model", "alpha", "q", start, 2,
			),
			Messages:        []db.Message{userMsg, assistantMsg},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsHourOfWeek(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsHourOfWeek")
	assert.Equal(t, 1, duckHOWMessages(resp.Cells, 5, 9),
		"paired empty-model user turn at Sat 09:00")
	assert.Equal(t, 1, duckHOWMessages(resp.Cells, 5, 10),
		"selected-model assistant at Sat 10:00")
}

func TestDuckAnalyticsActivityModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	ctx := context.Background()
	userMsg := syncMessage(
		"duck-activity-paired-hour", 0, "user", "q", "2024-06-01T09:00:00Z",
	)
	userMsg.Model = ""
	assistantMsg := duckModelMessage(
		"duck-activity-paired-hour", 1, "assistant", "a",
		"2024-06-01T10:00:00Z", "gpt-4o",
	)
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-activity-paired-hour", "alpha", "q",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages:        []db.Message{userMsg, assistantMsg},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	// Paired empty-model user turn at hour 9; gpt-4o assistant at hour 10.
	// Filtering by hour 9 must keep the session via the paired user turn.
	hour := 9
	resp, err := store.GetAnalyticsActivity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "day")
	require.NoError(t, err, "GetAnalyticsActivity")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Sessions, "Sessions")
	assert.Equal(t, 1, resp.Series[0].Messages, "Messages")
	assert.Equal(t, 1, resp.Series[0].UserMessages, "UserMessages")
	assert.Equal(t, 0, resp.Series[0].AssistantMessages, "AssistantMessages")
}

func TestDuckAnalyticsHeatmapSessionsModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	ctx := context.Background()
	userMsg := syncMessage(
		"duck-heatmap-sessions-paired", 0, "user", "q",
		"2024-06-01T09:00:00Z",
	)
	userMsg.Model = ""
	assistantMsg := duckModelMessage(
		"duck-heatmap-sessions-paired", 1, "assistant", "a",
		"2024-06-01T10:00:00Z", "gpt-4o",
	)
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-heatmap-sessions-paired", "alpha", "q",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages:        []db.Message{userMsg, assistantMsg},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	// Empty-model user turn at hour 9 paired with a gpt-4o assistant at hour
	// 10. The sessions heatmap must keep the session via the paired user turn
	// instead of requiring a gpt-4o message at hour 9.
	hour := 9
	resp, err := store.GetAnalyticsHeatmap(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "sessions")
	require.NoError(t, err, "GetAnalyticsHeatmap")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 1, resp.Entries[0].Value, "Value")
}

func TestDuckAnalyticsTopSessionsDurationModelAndHourFilterKeepsPairedUserTurn(
	t *testing.T,
) {
	ctx := context.Background()
	session := syncSession(
		"duck-top-duration-paired", "alpha", "q",
		"2024-06-01T09:00:00Z", 2,
	)
	endedAt := "2024-06-01T10:00:00Z"
	session.EndedAt = &endedAt
	userMsg := syncMessage(
		"duck-top-duration-paired", 0, "user", "q",
		"2024-06-01T09:00:00Z",
	)
	userMsg.Model = ""
	assistantMsg := duckModelMessage(
		"duck-top-duration-paired", 1, "assistant", "a",
		"2024-06-01T10:00:00Z", "gpt-4o",
	)
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session:         session,
			Messages:        []db.Message{userMsg, assistantMsg},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	// Empty-model user turn at hour 9 paired with a gpt-4o assistant at hour
	// 10. Ranking sessions by duration under the gpt-4o + hour-9 filter must
	// keep the session via the paired user turn.
	hour := 9
	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "duration")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 1, "len(Sessions)")
	assert.Equal(t, "duck-top-duration-paired", resp.Sessions[0].ID, "ID")
}

func TestDuckAnalyticsTopSessionsDurationModelFilterRanksAndLimitsScopedSet(
	t *testing.T,
) {
	ctx := context.Background()
	// 12 gpt-4o sessions in hour 9 with distinct active durations (message gap
	// k*20s, under the 5-minute cap). The model+hour duration path filters the
	// scoped set and limits in Go without binding every ID into one IN list, so
	// it must return the 10 longest by active duration, not all 12.
	var writes []db.SessionBatchWrite
	for k := 1; k <= 12; k++ {
		id := fmt.Sprintf("duck-top-dur-rank-%02d", k)
		start := "2024-06-01T09:00:00Z"
		gap := k * 20
		end := fmt.Sprintf("2024-06-01T09:%02d:%02dZ", gap/60, gap%60)
		session := syncSession(id, "alpha", "q", start, 2)
		session.EndedAt = &end
		userMsg := syncMessage(id, 0, "user", "q", start)
		userMsg.Model = ""
		writes = append(writes, db.SessionBatchWrite{
			Session: session,
			Messages: []db.Message{
				userMsg,
				duckModelMessage(id, 1, "assistant", "a", end, "gpt-4o"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	store := newDuckAnalyticsStore(t, writes)

	hour := 9
	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "duration")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 10, "top sessions capped at 10")
	// Longest active duration first: k=12 (240s) down to k=3 (60s).
	assert.Equal(t, "duck-top-dur-rank-12", resp.Sessions[0].ID, "longest")
	assert.Equal(t, "duck-top-dur-rank-03", resp.Sessions[9].ID, "tenth")
	ids := map[string]bool{}
	for _, session := range resp.Sessions {
		ids[session.ID] = true
	}
	assert.False(t, ids["duck-top-dur-rank-01"], "shortest excluded")
	assert.False(t, ids["duck-top-dur-rank-02"], "second shortest excluded")
}

func assertDuckAnalyticsToolsModelFilterCountsOnlyMatchingToolCalls(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsTools(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsTools")
	assert.Equal(t, 2, resp.TotalCalls, "TotalCalls")

	byCategory := map[string]int{}
	for _, row := range resp.ByCategory {
		byCategory[row.Category] = row.Count
	}
	assert.Equal(t, 1, byCategory["Read"], "Read")
	assert.Equal(t, 1, byCategory["Skill"], "Skill")
	assert.Zero(t, byCategory["Grep"], "Grep")
}

func TestDuckAnalyticsToolsModelAndHourFilterCountsOnlyMatchingHourToolCalls(
	t *testing.T,
) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-tool-model-hour", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 2,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-tool-model-hour", 0, "assistant",
					"read", "2024-06-01T09:00:00Z", "gpt-4o",
					db.ToolCall{ToolName: "Read", Category: "Read"},
					db.ToolCall{ToolName: "Bash", Category: "Bash"},
				),
				duckModelMessage(
					"duck-tool-model-hour", 1, "assistant",
					"grep", "2024-06-01T10:00:00Z", "gpt-4o",
					db.ToolCall{ToolName: "Grep", Category: "Grep"},
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	hour := 10
	resp, err := store.GetAnalyticsTools(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	})
	require.NoError(t, err, "GetAnalyticsTools")
	assert.Equal(t, 1, resp.TotalCalls, "TotalCalls")
	require.Len(t, resp.ByCategory, 1, "len(ByCategory)")
	assert.Equal(t, "Grep", resp.ByCategory[0].Category, "Category")
	assert.Equal(t, 1, resp.ByCategory[0].Count, "Count")
}

func assertDuckAnalyticsSkillsModelFilterCountsOnlyMatchingSkillCalls(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsSkills(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSkills")
	assert.Equal(t, 1, resp.TotalSkillCalls, "TotalSkillCalls")
	assert.Equal(t, 1, resp.DistinctSkills, "DistinctSkills")
	require.Len(t, resp.BySkill, 1, "len(BySkill)")
	assert.Equal(t, "review-code", resp.BySkill[0].SkillName, "SkillName")
	assert.Equal(t, 1, resp.BySkill[0].CallCount, "CallCount")
}

func assertDuckAnalyticsProjectsModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsProjects(ctx, db.AnalyticsFilter{
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

func assertDuckAnalyticsHeatmapModelFilterCountsOnlyMatchingMessages(
	t *testing.T,
	ctx context.Context,
	store *Store,
) {
	t.Helper()

	resp, err := store.GetAnalyticsHeatmap(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "messages")
	require.NoError(t, err, "GetAnalyticsHeatmap")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 1, resp.Entries[0].Value, "Value")
}

func TestDuckAnalyticsHeatmapModelFilterUsesFilteredOutputTokens(
	t *testing.T,
) {
	ctx := context.Background()

	mixedSession := syncSession(
		"duck-heatmap-output-mixed", "alpha", "mixed",
		"2024-06-01T10:00:00Z", 2,
	)
	mixedSession.TotalOutputTokens = 111
	mixedSession.HasTotalOutputTokens = true
	mixedGpt := duckModelMessage(
		"duck-heatmap-output-mixed", 0, "assistant", "gpt",
		"2024-06-01T10:00:00Z", "gpt-4o",
	)
	mixedGpt.TokenUsage = []byte(`{"output_tokens":11}`)
	mixedGpt.OutputTokens = 11
	mixedGpt.HasOutputTokens = true
	mixedClaude := duckModelMessage(
		"duck-heatmap-output-mixed", 1, "assistant", "claude",
		"2024-06-01T10:05:00Z", "claude-3-5-sonnet",
	)
	mixedClaude.TokenUsage = []byte(`{"output_tokens":100}`)
	mixedClaude.OutputTokens = 100
	mixedClaude.HasOutputTokens = true

	uncoveredSession := syncSession(
		"duck-heatmap-output-uncovered", "alpha", "mixed",
		"2024-06-01T10:40:00Z", 2,
	)
	uncoveredSession.TotalOutputTokens = 90
	uncoveredSession.HasTotalOutputTokens = true
	uncoveredGpt := duckModelMessage(
		"duck-heatmap-output-uncovered", 0, "assistant", "gpt",
		"2024-06-01T10:40:00Z", "gpt-4o",
	)
	uncoveredGpt.TokenUsage = nil
	uncoveredGpt.ContextTokens = 0
	uncoveredGpt.OutputTokens = 0
	uncoveredGpt.HasContextTokens = false
	uncoveredGpt.HasOutputTokens = false
	uncoveredClaude := duckModelMessage(
		"duck-heatmap-output-uncovered", 1, "assistant", "claude",
		"2024-06-01T10:45:00Z", "claude-3-5-sonnet",
	)
	uncoveredClaude.TokenUsage = []byte(`{"output_tokens":90}`)
	uncoveredClaude.OutputTokens = 90
	uncoveredClaude.HasOutputTokens = true

	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: mixedSession,
			Messages: []db.Message{
				mixedGpt,
				mixedClaude,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: uncoveredSession,
			Messages: []db.Message{
				uncoveredGpt,
				uncoveredClaude,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	hour := 10
	resp, err := store.GetAnalyticsHeatmap(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o", Hour: &hour,
	}, "output_tokens")
	require.NoError(t, err, "GetAnalyticsHeatmap")
	require.Len(t, resp.Entries, 1, "len(Entries)")
	assert.Equal(t, 11, resp.Entries[0].Value, "Value")
}

func TestDuckAnalyticsTopSessionsMessagesUseFilteredModelCounts(
	t *testing.T,
) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-top-mixed", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 3,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-top-mixed", 0, "assistant", "gpt",
					"2024-06-01T09:00:00Z", "gpt-4o",
				),
				duckModelMessage(
					"duck-top-mixed", 1, "assistant", "claude",
					"2024-06-01T09:05:00Z", "claude-3-5-sonnet",
				),
				duckModelMessage(
					"duck-top-mixed", 2, "assistant", "claude",
					"2024-06-01T09:06:00Z", "claude-3-5-sonnet",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: syncSession(
				"duck-top-gpt", "alpha", "gpt",
				"2024-06-01T11:00:00Z", 2,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-top-gpt", 0, "assistant", "gpt",
					"2024-06-01T11:00:00Z", "gpt-4o",
				),
				duckModelMessage(
					"duck-top-gpt", 1, "assistant", "gpt",
					"2024-06-01T11:05:00Z", "gpt-4o",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "messages")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 2, "len(Sessions)")
	assert.Equal(t, "duck-top-gpt", resp.Sessions[0].ID, "top session")
	assert.Equal(t, 2, resp.Sessions[0].MessageCount, "top MessageCount")
	assert.Equal(t, "duck-top-mixed", resp.Sessions[1].ID, "second session")
	assert.Equal(t, 1, resp.Sessions[1].MessageCount, "second MessageCount")
}

func TestDuckAnalyticsTopSessionsOutputTokensUseFilteredModelTotals(
	t *testing.T,
) {
	ctx := context.Background()

	mixedSession := syncSession(
		"duck-top-output-mixed", "alpha", "mixed",
		"2024-06-01T09:00:00Z", 2,
	)
	mixedSession.TotalOutputTokens = 510
	mixedSession.HasTotalOutputTokens = true
	mixedGpt := duckModelMessage(
		"duck-top-output-mixed", 0, "assistant", "gpt",
		"2024-06-01T09:00:00Z", "gpt-4o",
	)
	mixedGpt.TokenUsage = []byte(`{"output_tokens":10}`)
	mixedGpt.OutputTokens = 10
	mixedGpt.HasOutputTokens = true
	mixedClaude := duckModelMessage(
		"duck-top-output-mixed", 1, "assistant", "claude",
		"2024-06-01T09:05:00Z", "claude-3-5-sonnet",
	)
	mixedClaude.TokenUsage = []byte(`{"output_tokens":500}`)
	mixedClaude.OutputTokens = 500
	mixedClaude.HasOutputTokens = true

	gptSession := syncSession(
		"duck-top-output-gpt", "alpha", "gpt",
		"2024-06-01T11:00:00Z", 1,
	)
	gptSession.TotalOutputTokens = 30
	gptSession.HasTotalOutputTokens = true
	gptMsg := duckModelMessage(
		"duck-top-output-gpt", 0, "assistant", "gpt",
		"2024-06-01T11:00:00Z", "gpt-4o",
	)
	gptMsg.TokenUsage = []byte(`{"output_tokens":30}`)
	gptMsg.OutputTokens = 30
	gptMsg.HasOutputTokens = true

	uncoveredSession := syncSession(
		"duck-top-output-uncovered", "alpha", "mixed",
		"2024-06-01T13:00:00Z", 2,
	)
	uncoveredSession.TotalOutputTokens = 900
	uncoveredSession.HasTotalOutputTokens = true
	uncoveredGpt := duckModelMessage(
		"duck-top-output-uncovered", 0, "assistant", "gpt",
		"2024-06-01T13:00:00Z", "gpt-4o",
	)
	uncoveredGpt.TokenUsage = nil
	uncoveredGpt.ContextTokens = 0
	uncoveredGpt.OutputTokens = 0
	uncoveredGpt.HasContextTokens = false
	uncoveredGpt.HasOutputTokens = false
	uncoveredClaude := duckModelMessage(
		"duck-top-output-uncovered", 1, "assistant", "claude",
		"2024-06-01T13:05:00Z", "claude-3-5-sonnet",
	)
	uncoveredClaude.TokenUsage = []byte(`{"output_tokens":900}`)
	uncoveredClaude.OutputTokens = 900
	uncoveredClaude.HasOutputTokens = true

	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: mixedSession,
			Messages: []db.Message{
				mixedGpt,
				mixedClaude,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: gptSession,
			Messages: []db.Message{
				gptMsg,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: uncoveredSession,
			Messages: []db.Message{
				uncoveredGpt,
				uncoveredClaude,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsTopSessions(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, "output_tokens")
	require.NoError(t, err, "GetAnalyticsTopSessions")
	require.Len(t, resp.Sessions, 2, "len(Sessions)")
	assert.Equal(t, "duck-top-output-gpt", resp.Sessions[0].ID,
		"top session")
	assert.Equal(t, 30, resp.Sessions[0].OutputTokens,
		"top OutputTokens")
	assert.Equal(t, "duck-top-output-mixed", resp.Sessions[1].ID,
		"second session")
	assert.Equal(t, 10, resp.Sessions[1].OutputTokens,
		"second OutputTokens")
}

func TestDuckAnalyticsVelocityModelFilterUsesMatchingRowsOnly(t *testing.T) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-velocity-model", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 4,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-velocity-model", 0, "user", "claude q",
					"2024-06-01T09:00:00Z", "claude-3-5-sonnet",
				),
				duckModelMessage(
					"duck-velocity-model", 1, "assistant", "offscope-offscope-xx",
					"2024-06-01T09:00:10Z", "claude-3-5-sonnet",
					db.ToolCall{ToolName: "Read", Category: "Read"},
					db.ToolCall{ToolName: "Bash", Category: "Bash"},
					db.ToolCall{ToolName: "Grep", Category: "Grep"},
				),
				duckModelMessage(
					"duck-velocity-model", 2, "user", "gpt q",
					"2024-06-01T09:10:00Z", "",
				),
				duckModelMessage(
					"duck-velocity-model", 3, "assistant", "reply",
					"2024-06-01T09:11:00Z", "gpt-4o",
					db.ToolCall{ToolName: "Edit", Category: "Edit"},
					db.ToolCall{ToolName: "Write", Category: "Write"},
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
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

func TestDuckAnalyticsVelocityModelFilterCountsNullTimestampToolCallsWithoutTimeFilter(
	t *testing.T,
) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-velocity-null-ts", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 4,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-velocity-null-ts", 0, "user", "claude q",
					"2024-06-01T09:00:00Z", "claude-3-5-sonnet",
				),
				duckModelMessage(
					"duck-velocity-null-ts", 1, "assistant", "offscope-offscope-xx",
					"2024-06-01T09:00:10Z", "claude-3-5-sonnet",
					db.ToolCall{ToolName: "Read", Category: "Read"},
					db.ToolCall{ToolName: "Bash", Category: "Bash"},
					db.ToolCall{ToolName: "Grep", Category: "Grep"},
				),
				duckModelMessage(
					"duck-velocity-null-ts", 2, "user", "gpt q",
					"2024-06-01T09:10:00Z", "",
				),
				duckModelMessage(
					"duck-velocity-null-ts", 3, "assistant", "reply",
					"2024-06-01T09:11:00Z", "gpt-4o",
					db.ToolCall{ToolName: "Edit", Category: "Edit"},
					db.ToolCall{ToolName: "Write", Category: "Write"},
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	_, err := store.duck.ExecContext(ctx, `
		UPDATE sessions
		SET message_count = 5
		WHERE id = 'duck-velocity-null-ts'`)
	require.NoError(t, err, "update session message_count")
	_, err = store.duck.ExecContext(ctx, `
		INSERT INTO messages (
			id, session_id, ordinal, role, content, timestamp,
			has_tool_use, content_length, is_system, model
		) VALUES
			(9103, 'duck-velocity-null-ts', 4, 'assistant', 'extra',
			 NULL, TRUE, 5, FALSE, 'gpt-4o')`)
	require.NoError(t, err, "insert null-timestamp message")
	_, err = store.duck.ExecContext(ctx, `
		INSERT INTO tool_calls (
			id, message_id, session_id, tool_name, category, call_index
		) VALUES
			(9203, 9103, 'duck-velocity-null-ts', 'Search', 'Search', 0)`)
	require.NoError(t, err, "insert tool call")

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	assert.Equal(t, 60.0, resp.Overall.FirstResponseSec.P50,
		"FirstResponse P50")
	assert.Equal(t, 3.0, resp.Overall.ToolCallsPerActiveMin,
		"ToolCallsPerActiveMin")
}

func TestDuckAnalyticsSessionShapeModelFilterUsesMatchingRowsOnly(t *testing.T) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-shape-model", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 6,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-shape-model", 0, "user", "gpt q",
					"2024-06-01T09:00:00Z", "",
				),
				duckModelMessage(
					"duck-shape-model", 1, "assistant", "gpt tool",
					"2024-06-01T09:01:00Z", "gpt-4o",
					db.ToolCall{ToolName: "Read", Category: "Read"},
				),
				duckModelMessage(
					"duck-shape-model", 2, "user", "claude q1",
					"2024-06-01T09:02:00Z", "claude-3-5-sonnet",
				),
				duckModelMessage(
					"duck-shape-model", 3, "user", "claude q2",
					"2024-06-01T09:03:00Z", "claude-3-5-sonnet",
				),
				duckModelMessage(
					"duck-shape-model", 4, "user", "claude q3",
					"2024-06-01T09:04:00Z", "claude-3-5-sonnet",
				),
				duckModelMessage(
					"duck-shape-model", 5, "assistant", "claude reply",
					"2024-06-01T09:05:00Z", "claude-3-5-sonnet",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsSessionShape(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsSessionShape")
	assert.Equal(t, 1, resp.Count, "Count")

	lenMap := map[string]int{}
	for _, bucket := range resp.LengthDistribution {
		lenMap[bucket.Label] = bucket.Count
	}
	assert.Equal(t, 1, lenMap["1-5"], "filtered length bucket")
	assert.Equal(t, 0, lenMap["6-15"], "full-session count must not leak")

	autoMap := map[string]int{}
	for _, bucket := range resp.AutonomyDistribution {
		autoMap[bucket.Label] = bucket.Count
	}
	assert.Equal(t, 1, autoMap["1-2"], "filtered autonomy bucket")
	assert.Equal(t, 0, autoMap["<0.5"], "off-model user turns must not leak")
}

func TestDuckAnalyticsVelocityModelFilterUsesMatchingComplexityBucket(t *testing.T) {
	ctx := context.Background()
	msgs := []db.Message{
		duckModelMessage(
			"duck-velocity-complexity", 0, "user", "gpt q",
			"2024-06-01T09:00:00Z", "",
		),
		duckModelMessage(
			"duck-velocity-complexity", 1, "assistant", "reply",
			"2024-06-01T09:01:00Z", "gpt-4o",
		),
	}
	for i := 2; i < 16; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, duckModelMessage(
			"duck-velocity-complexity", i, role, "claude",
			"2024-06-01T09:10:00Z", "claude-3-5-sonnet",
		))
	}
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-velocity-complexity", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 16,
			),
			Messages:        msgs,
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})

	resp, err := store.GetAnalyticsVelocity(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	})
	require.NoError(t, err, "GetAnalyticsVelocity")
	require.Len(t, resp.ByComplexity, 1, "len(ByComplexity)")
	assert.Equal(t, "1-15", resp.ByComplexity[0].Label,
		"complexity bucket should use filtered message count")
	assert.Equal(t, 1, resp.ByComplexity[0].Sessions, "Sessions")
}

func TestDuckTrendsTermsModelFilterStaysOnMatchingMessages(t *testing.T) {
	ctx := context.Background()
	store := newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-trends-model", "alpha", "mixed",
				"2024-06-01T09:00:00Z", 3,
			),
			Messages: []db.Message{
				duckModelMessage(
					"duck-trends-model", 0, "user", "seam",
					"2024-06-01T09:00:00Z", "",
				),
				duckModelMessage(
					"duck-trends-model", 1, "assistant", "ready",
					"2024-06-01T09:01:00Z", "gpt-4o",
				),
				duckModelMessage(
					"duck-trends-model", 2, "assistant", "seam seam",
					"2024-06-01T10:00:00Z", "claude-3-5-sonnet",
				),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	terms, err := db.ParseTrendTerms([]string{"seam"})
	require.NoError(t, err)

	resp, err := store.GetTrendsTerms(ctx, db.AnalyticsFilter{
		From: "2024-06-01", To: "2024-06-01", Timezone: "UTC",
		Model: "gpt-4o",
	}, terms, "day")
	require.NoError(t, err, "GetTrendsTerms")
	assert.Equal(t, 2, resp.MessageCount, "MessageCount")
	require.Len(t, resp.Series, 1, "len(Series)")
	assert.Equal(t, 1, resp.Series[0].Total, "Total")
}

func newDuckAnalyticsStore(
	t *testing.T, writes []db.SessionBatchWrite,
) *Store {
	t.Helper()
	ctx := context.Background()
	local := newLocalDB(t)
	_, err := local.WriteSessionBatchAtomic(writes)
	require.NoError(t, err)
	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	_, err = syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	return NewStoreFromDB(syncer.DB())
}

func newDuckMixedModelAnalyticsStore(t *testing.T) *Store {
	t.Helper()
	startA := "2024-06-01T09:00:00Z"
	startB := "2024-06-01T10:00:00Z"

	gptMsg := duckModelMessage(
		"duck-mixed-model", 0, "assistant", "seam", startA, "gpt-4o",
		db.ToolCall{
			ToolName: "Read", Category: "Read",
		},
		db.ToolCall{
			ToolName: "Skill", Category: "Skill", SkillName: "review-code",
		},
	)
	claudeMsg := duckModelMessage(
		"duck-mixed-model", 1, "assistant", "seam seam", startB,
		"claude-3-5-sonnet",
		db.ToolCall{
			ToolName: "Grep", Category: "Grep",
		},
		db.ToolCall{
			ToolName: "Skill", Category: "Skill", SkillName: "write-tests",
		},
	)

	return newDuckAnalyticsStore(t, []db.SessionBatchWrite{
		{
			Session: syncSession(
				"duck-mixed-model", "alpha", "mixed", startA, 2,
			),
			Messages: []db.Message{
				gptMsg,
				claudeMsg,
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
}

func duckModelMessage(
	sessionID string,
	ordinal int,
	role, content, ts, model string,
	calls ...db.ToolCall,
) db.Message {
	msg := syncMessage(sessionID, ordinal, role, content, ts, calls...)
	msg.Model = model
	return msg
}

func duckHOWMessages(cells []db.HourOfWeekCell, dow, hour int) int {
	for _, cell := range cells {
		if cell.DayOfWeek == dow && cell.Hour == hour {
			return cell.Messages
		}
	}
	return -1
}
