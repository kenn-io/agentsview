package readbase

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/db"
)

func TestSecretFindingsSQL(t *testing.T) {
	for _, dialect := range []struct {
		name, dateExpr, dateParam, tieBreak string
	}{
		{"duckdb", "CAST(COALESCE(s.started_at, s.created_at) AS DATE)", "CAST(? AS DATE)", "sf.id"},
		{"clickhouse", "toDate(COALESCE(s.started_at, s.created_at))", "toDate(?)", "sf.finding_index"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			for _, tt := range []struct {
				name   string
				filter db.SecretFindingFilter
				where  string
				args   []any
			}{
				{"unfiltered", db.SecretFindingFilter{Limit: 10}, "s.deleted_at IS NULL", []any{11, 0}},
				{"empty_versions_and_all_confidence", db.SecretFindingFilter{Confidence: "all", RulesVersions: []string{"", ""}, Limit: 2}, "s.deleted_at IS NULL", []any{3, 0}},
				{
					"combined_filters",
					db.SecretFindingFilter{Project: "project-a", Agent: "claude", Rule: "rule-a", Confidence: "high", RulesVersions: []string{"v1", "", "v2"}, DateFrom: "2026-01-01", DateTo: "2026-01-31", Limit: 3, Cursor: 6},
					"s.deleted_at IS NULL AND s.project = ? AND s.agent = ? AND sf.rule_name = ? AND sf.confidence = ? AND sf.rules_version IN (?,?) AND " + dialect.dateExpr + " >= " + dialect.dateParam + " AND " + dialect.dateExpr + " <= " + dialect.dateParam,
					[]any{"project-a", "claude", "rule-a", "high", "v1", "v2", "2026-01-01", "2026-01-31", 4, 6},
				},
			} {
				t.Run(tt.name, func(t *testing.T) {
					query, args := SecretFindingsSQL(tt.filter, dialect.dateExpr, dialect.dateParam, dialect.tieBreak)
					query = strings.Join(strings.Fields(query), " ")
					assert.Contains(t, query, "WHERE "+tt.where+" ORDER BY")
					assert.Contains(t, query, "sf.match_index, "+dialect.tieBreak+" LIMIT ? OFFSET ?")
					assert.Equal(t, tt.args, args)
				})
			}
		})
	}
}

func TestSidebarSQL(t *testing.T) {
	for _, tt := range []struct {
		name     string
		dialect  db.QueryDialect
		position string
		count    string
	}{
		{"duckdb", db.DuckDBQueryDialect(), "position('<teammate-message' in COALESCE(first_message, ''))", "COUNT(*)"},
		{"clickhouse", db.ClickHouseQueryDialect(), "position(COALESCE(first_message, ''), '<teammate-message')", "toInt64(COUNT(*))"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			filter := db.SessionFilter{Project: "project", IncludeChildren: true, IncludeOrphans: true}
			query, args := (SessionQueries{}).SidebarSQL(filter, tt.dialect)
			where, wantArgs := db.BuildSessionFilterSQL(filter, tt.dialect)
			assert.Equal(t, wantArgs, args)
			assert.Contains(t, query, "FROM sessions WHERE "+where)
			assert.Contains(t, query, "is_automated, "+tt.position+" > 0")
			assert.Contains(t, query, "ORDER BY COALESCE(ended_at, started_at, created_at) DESC, id DESC")
			assert.Equal(t, tt.count, tt.dialect.SignedAggregate("COUNT(*)"))
		})
	}
}
