package duckdb

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

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
