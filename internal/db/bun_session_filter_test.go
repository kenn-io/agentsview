package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func normalizeSQL(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func TestBuildBunSessionFilterPreservesParameterOrderAndDSTBounds(t *testing.T) {
	minToolFailures := 2
	filter := SessionFilter{
		Project: "proj-a", ExcludeProject: "unknown",
		Machine: "laptop,server", Agent: "claude,codex",
		DateFrom: "2024-03-10", DateTo: "2024-03-10",
		Timezone: "America/New_York", MinMessages: 3,
		Outcome:         []string{"success", "failed"},
		MinToolFailures: &minToolFailures,
		HasSecret:       true, SecretsRulesVersions: []string{"v1", "", "v2"},
	}

	query, args := buildBunSessionFilter(filter, nil)

	assert.Contains(t, normalizeSQL(query), "project = ? AND project != ?")
	assert.Contains(t, normalizeSQL(query), "machine IN (?,?)")
	assert.Contains(t, normalizeSQL(query), "agent IN (?,?)")
	assert.Contains(t, normalizeSQL(query), "outcome IN (?,?)")
	assert.Contains(t, normalizeSQL(query),
		"secret_leak_count > 0 AND secrets_rules_version IN (?,?)")
	assert.Equal(t, []any{
		"proj-a", "unknown", "laptop", "server", "claude", "codex",
		"2024-03-10T05:00:00Z", "2024-03-11T04:00:00Z",
		3, "success", "failed", 2, "v1", "v2",
	}, args)
}

func TestBuildBunSessionFilterKeepsRecursiveChildScope(t *testing.T) {
	query, args := buildBunSessionFilter(SessionFilter{
		IncludeChildren: true, Machine: "laptop,server", Agent: "claude",
		ExcludeOneShot: true, ExcludeAutomated: true,
	}, nil)
	normalized := normalizeSQL(query)

	assert.Contains(t, normalized, "WITH RECURSIVE tree(id) AS")
	assert.Contains(t, normalized, "JOIN tree t ON s.parent_session_id = t.id")
	assert.Contains(t, normalized, "id IN (WITH RECURSIVE tree(id) AS")
	assert.Contains(t, normalized,
		"NOT (root_session.relationship_type IN ('subagent', 'fork', 'continuation'))")
	assert.Equal(t, []any{"laptop", "server", "claude"}, args)
}

func TestBuildBunSessionFilterFailsClosedForEmptyCSVValues(t *testing.T) {
	query, args := buildBunSessionFilter(SessionFilter{
		Machine: ",,", Agent: "  ",
	}, nil)

	assert.Contains(t, normalizeSQL(query), "1 = 0")
	assert.Empty(t, args)
}
