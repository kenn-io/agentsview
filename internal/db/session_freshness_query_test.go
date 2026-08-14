package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentScopedFreshnessQueriesSeekByFilePath(t *testing.T) {
	database := testDB(t)

	tests := []struct {
		name  string
		query string
	}{
		{name: "file metadata", query: getFileInfoByAgentPathQuery},
		{name: "file hash", query: getFileHashByAgentPathQuery},
		{name: "data version", query: getDataVersionByAgentPathQuery},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := queryPlanOf(
				t, database, test.query, "/sessions/example.jsonl", "codex",
			)

			assert.Contains(t, plan,
				"USING INDEX idx_sessions_file_path (file_path=?)",
				"freshness work must be bounded by one source path\n%s", plan,
			)
			assert.NotContains(t, plan,
				"idx_sessions_agent (agent=?)",
				"agent-wide scans make reconciliation quadratic\n%s", plan,
			)
		})
	}
}
