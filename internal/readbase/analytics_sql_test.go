package readbase

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/db"
)

func TestAnalyticsSQLDialects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect db.QueryDialect
		signed  func(string) string
		truth   string
	}{
		{"duckdb", db.DuckDBQueryDialect(), func(s string) string { return s }, "TRUE"},
		{"clickhouse", db.ClickHouseQueryDialect(), func(s string) string { return "toInt64(" + s + ")" }, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []any{"project", "agent"}
			query, gotArgs := AnalyticsSummaryAgentsSQL("s.project = ? AND s.agent = ?", args, tc.dialect)
			assert.Contains(t, query, "SELECT agent, "+tc.signed("COUNT(*)")+", "+tc.signed("COALESCE(SUM(message_count), 0)"))
			assert.Contains(t, query, "WHERE s.project = ? AND s.agent = ?")
			assert.Equal(t, args, gotArgs)
			query, gotArgs = AnalyticsActivityAgentsSQL("s.project = ? AND s.agent = ?", args, "local_day(?, started_at)", []any{"UTC"}, "week(local_date)", "WHERE m.model = ?", []any{"selected"}, tc.dialect)
			assert.Contains(t, query, "SELECT week(local_date) AS bucket, fs.agent, "+tc.signed("COUNT(*)")+" AS messages")
			assert.Contains(t, query, "WHERE m.model = ?")
			assert.Equal(t, []any{"UTC", "project", "agent", "selected"}, gotArgs)
			for _, metric := range []struct{ name, expression string }{
				{"sessions", "COUNT(*)"},
				{"messages", "COALESCE(SUM(s.message_count), 0)"},
				{"output_tokens", "COALESCE(SUM(s.total_output_tokens), 0)"},
			} {
				t.Run(metric.name, func(t *testing.T) {
					query, gotArgs := AnalyticsHeatmapSQL("s.project = ? AND s.agent = ?", args, "local_day(?, started_at)", []any{"UTC"}, metric.name, tc.dialect)
					assert.Contains(t, query, "SELECT local_day(?, started_at) AS local_date, "+tc.signed(metric.expression)+" AS value")
					assert.Contains(t, query, "WHERE s.project = ? AND s.agent = ?")
					if metric.name == "output_tokens" {
						assert.Contains(t, query, "AND s.has_total_output_tokens = "+tc.truth)
					} else {
						assert.NotContains(t, query, "has_total_output_tokens")
					}
					assert.Equal(t, []any{"UTC", "project", "agent"}, gotArgs)
				})
			}
		})
	}
}
