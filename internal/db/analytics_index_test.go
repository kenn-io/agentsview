package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyticsMetadataIndexLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"reopen", "upgrade", "bulk import"} {
		t.Run(lifecycle, func(t *testing.T) {
			d := testDB(t)
			if lifecycle == "bulk import" {
				require.NoError(t, d.DropBulkImportIndexes(t.Context()))
			}
			for _, id := range []string{"a", "b"} {
				insertSession(t, d, id, "project", func(s *Session) {
					s.StartedAt = new("2024-06-01T09:00:00Z")
					s.MessageCount = 20
				})
				var messages []Message
				for ordinal := range 20 {
					messages = append(messages, Message{
						SessionID: id, Ordinal: ordinal, Role: "assistant",
						Content:   strings.Repeat("message body ", 1000),
						Timestamp: "2024-06-01T09:00:00Z", Model: "model-" + id,
						ToolCalls: []ToolCall{{SessionID: id, ToolName: "Read", Category: "Read"}},
					})
				}
				insertMessages(t, d, messages...)
			}
			if lifecycle == "upgrade" {
				// Model an archive whose metadata index predates tool evidence ordinals.
				_, err := d.getWriter().Exec(t.Context(), `
					DROP INDEX idx_messages_analytics_metadata;
					CREATE INDEX idx_messages_analytics_metadata ON messages(session_id, id, timestamp, model);
					ANALYZE;
					DELETE FROM sqlite_stat1`)
				require.NoError(t, err)
			}
			if lifecycle == "bulk import" {
				require.NoError(t, d.RebuildBulkImportIndexes(t.Context()))
			} else {
				path := d.Path()
				require.NoError(t, d.Close())
				var err error
				d, err = OpenIsolated(t.Context(), path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, d.Close()) })
			}

			// Statistics must cover the competing index too. Analyzing only
			// the new index caused SQLite to keep choosing body-page reads.
			for _, index := range []string{
				"idx_messages_analytics_metadata", "idx_messages_session_role",
				"idx_tool_calls_session", "sqlite_autoindex_sessions_1",
			} {
				var stat string
				require.NoError(t, d.getReader().QueryRow(t.Context(),
					`SELECT stat FROM sqlite_stat1 WHERE idx = ?`, index).Scan(&stat))
				assert.NotEmpty(t, stat)
			}

			filter := baseFilter()
			summary, err := d.GetAnalyticsSummary(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 2, summary.TotalSessions)
			assert.Equal(t, 40, summary.TotalMessages)
			assert.Equal(t, []string{"model-a", "model-b"}, summary.Models)
			toolSummary, err := d.GetAnalyticsTools(t.Context(), filter)
			require.NoError(t, err)
			assert.Equal(t, 40, toolSummary.TotalCalls)

			// Protect the index-only access used for summary model discovery
			// and the production tool-query builder's timestamp join.
			filter.IncludeSubagents = true
			where, args := sqliteAnalyticsWhereSQL(filter,
				"COALESCE(NULLIF(started_at, ''), created_at)", "s.id", true)
			modelsPlan := queryPlanOf(t, d, `SELECT DISTINCT m.model
				FROM sessions s JOIN messages m ON m.session_id = s.id
				WHERE `+where+` AND COALESCE(m.model, '') <> '' ORDER BY m.model`, args...)
			assert.Contains(t, modelsPlan, "USING COVERING INDEX idx_messages_analytics_metadata")
			toolsPlan := queryPlanOf(t, d, analyticsToolsQuery("(?)", "", "", true), "a")
			assert.Contains(t, toolsPlan, "USING COVERING INDEX idx_messages_analytics_metadata")
		})
	}
}
