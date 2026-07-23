package db

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func encodeBranchFilterTokensForTest(branches ...BranchInfo) string {
	tokens := make([]string, 0, len(branches))
	for _, branch := range branches {
		tokens = append(tokens,
			EncodeBranchFilterToken(branch.Project, branch.Branch))
	}
	return strings.Join(tokens, branchListSep)
}

func TestBuildSessionFilterSQLRendersEquivalentDialectFilters(t *testing.T) {
	minToolFailures := 2
	filter := SessionFilter{
		Project:              "proj-a",
		ExcludeProject:       "unknown",
		Machine:              "laptop,server",
		Agent:                "claude,codex",
		Date:                 "2026-06-08",
		DateFrom:             "2026-06-01",
		DateTo:               "2026-06-30",
		Timezone:             "America/New_York",
		ActiveSince:          "2026-06-08T12:00:00Z",
		MinMessages:          3,
		MaxMessages:          100,
		MinUserMessages:      2,
		ExcludeOneShot:       true,
		ExcludeAutomated:     false,
		Outcome:              []string{"success", "failed"},
		HealthGrade:          []string{"A", "C"},
		MinToolFailures:      &minToolFailures,
		HasSecret:            true,
		SecretsRulesVersions: []string{"v1", "", "v2"},
		Termination:          "clean,awaiting_user",
	}

	tests := []struct {
		name      string
		dialect   QueryDialect
		wantParts []string
		wantArgs  []any
	}{
		{
			name:    "sqlite",
			dialect: SQLiteQueryDialect(),
			wantParts: []string{
				"message_count > 0",
				"deleted_at IS NULL",
				"relationship_type NOT IN ('subagent', 'fork')",
				"project = ?",
				"project != ?",
				"machine IN (?,?)",
				"agent IN (?,?)",
				"julianday(COALESCE(NULLIF(ended_at, ''), (SELECT m.timestamp FROM messages m WHERE m.session_id = sessions.id AND m.timestamp != '' ORDER BY julianday(m.timestamp) DESC, m.timestamp DESC LIMIT 1), NULLIF(started_at, ''), created_at)) >= julianday(?)",
				"julianday(COALESCE(NULLIF(started_at, ''), created_at)) < julianday(?)",
				"julianday(COALESCE(NULLIF(ended_at, ''), (SELECT m.timestamp FROM messages m WHERE m.session_id = sessions.id AND m.timestamp != '' ORDER BY julianday(m.timestamp) DESC, m.timestamp DESC LIMIT 1), NULLIF(started_at, ''), created_at)) >= julianday(?)",
				"julianday(COALESCE(NULLIF(started_at, ''), created_at)) < julianday(?)",
				"julianday(COALESCE(NULLIF(ended_at, ''), (SELECT m.timestamp FROM messages m WHERE m.session_id = sessions.id AND m.timestamp != '' ORDER BY julianday(m.timestamp) DESC, m.timestamp DESC LIMIT 1), NULLIF(started_at, ''), created_at)) >= julianday(?)",
				"message_count >= ?",
				"message_count <= ?",
				"user_message_count >= ?",
				"(termination_status = 'clean' OR termination_status = 'awaiting_user')",
				"(user_message_count > 1 OR is_automated = 1)",
				"outcome IN (?,?)",
				"health_grade IN (?,?)",
				"tool_failure_signal_count >= ?",
				"secret_leak_count > 0 AND secrets_rules_version IN (?,?)",
			},
		},
		{
			name:    "postgres",
			dialect: PostgresQueryDialect(),
			wantParts: []string{
				"message_count > 0",
				"deleted_at IS NULL",
				"relationship_type NOT IN ('subagent', 'fork')",
				"project = $1",
				"project != $2",
				"machine IN ($3,$4)",
				"agent IN ($5,$6)",
				"COALESCE(ended_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id AND m.timestamp IS NOT NULL), started_at, created_at) >= $7::timestamptz",
				"COALESCE(started_at, created_at) < $8::timestamptz",
				"COALESCE(ended_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id AND m.timestamp IS NOT NULL), started_at, created_at) >= $9::timestamptz",
				"COALESCE(started_at, created_at) < $10::timestamptz",
				"COALESCE(ended_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id AND m.timestamp IS NOT NULL), started_at, created_at) >= $11::timestamptz",
				"message_count >= $12",
				"message_count <= $13",
				"user_message_count >= $14",
				"(termination_status = 'clean' OR termination_status = 'awaiting_user')",
				"(user_message_count > 1 OR is_automated = TRUE)",
				"outcome IN ($15,$16)",
				"health_grade IN ($17,$18)",
				"tool_failure_signal_count >= $19",
				"secret_leak_count > 0 AND secrets_rules_version IN ($20,$21)",
			},
		},
		{
			name:    "duckdb",
			dialect: DuckDBQueryDialect(),
			wantParts: []string{
				"message_count > 0",
				"deleted_at IS NULL",
				"relationship_type NOT IN ('subagent', 'fork')",
				"project = ?",
				"project != ?",
				"machine IN (?,?)",
				"agent IN (?,?)",
				"CAST(COALESCE(ended_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id AND m.timestamp IS NOT NULL), started_at, created_at) AS TIMESTAMP) >= CAST(? AS TIMESTAMP)",
				"CAST(COALESCE(started_at, created_at) AS TIMESTAMP) < CAST(? AS TIMESTAMP)",
				"CAST(COALESCE(ended_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id AND m.timestamp IS NOT NULL), started_at, created_at) AS TIMESTAMP) >= CAST(? AS TIMESTAMP)",
				"CAST(COALESCE(started_at, created_at) AS TIMESTAMP) < CAST(? AS TIMESTAMP)",
				"CAST(COALESCE(ended_at, (SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = sessions.id AND m.timestamp IS NOT NULL), started_at, created_at) AS TIMESTAMP) >= CAST(? AS TIMESTAMP)",
				"message_count >= ?",
				"message_count <= ?",
				"user_message_count >= ?",
				"(termination_status = 'clean' OR termination_status = 'awaiting_user')",
				"(user_message_count > 1 OR is_automated = TRUE)",
				"outcome IN (?,?)",
				"health_grade IN (?,?)",
				"tool_failure_signal_count >= ?",
				"secret_leak_count > 0 AND secrets_rules_version IN (?,?)",
			},
		},
	}
	wantArgs := []any{
		"proj-a", "unknown", "laptop", "server", "claude", "codex",
		"2026-06-08T04:00:00Z", "2026-06-09T04:00:00Z",
		"2026-06-01T04:00:00Z", "2026-07-01T04:00:00Z",
		"2026-06-08T12:00:00Z", 3, 100, 2,
		"success", "failed", "A", "C", 2, "v1", "v2",
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, args := BuildSessionFilterSQL(filter, tt.dialect)
			normalized := normalizeSQL(got)
			for _, part := range tt.wantParts {
				assert.Contains(t, normalized, normalizeSQL(part))
			}
			assert.Equal(t, wantArgs, args)

			for _, value := range []string{
				"proj-a", "unknown", "laptop", "server", "claude",
				"codex", "success", "failed", "v1", "v2",
			} {
				assert.NotContains(t, normalized, "'"+value+"'")
			}
		})
	}
}

func TestBuildSessionFilterSQLResolvesDSTDateBounds(t *testing.T) {
	filter := SessionFilter{
		DateFrom: "2024-03-10",
		DateTo:   "2024-03-10",
		Timezone: "America/New_York",
	}

	tests := []struct {
		name    string
		dialect QueryDialect
	}{
		{name: "sqlite", dialect: SQLiteQueryDialect()},
		{name: "postgres", dialect: PostgresQueryDialect()},
		{name: "duckdb", dialect: DuckDBQueryDialect()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, args := BuildSessionFilterSQL(filter, tt.dialect)
			assert.Equal(t, []any{
				"2024-03-10T05:00:00Z",
				"2024-03-11T04:00:00Z",
			}, args)
		})
	}
}

func TestBuildSessionFilterSQLRendersIncludeChildrenCTE(t *testing.T) {
	filter := SessionFilter{
		IncludeChildren:  true,
		Machine:          "laptop,server",
		Agent:            "claude",
		ExcludeOneShot:   true,
		ExcludeAutomated: true,
	}

	tests := []struct {
		name     string
		dialect  QueryDialect
		wantArgs []any
	}{
		{"sqlite", SQLiteQueryDialect(), []any{"laptop", "server", "claude"}},
		{"postgres", PostgresQueryDialect(), []any{"laptop", "server", "claude"}},
		{"duckdb", DuckDBQueryDialect(), []any{"laptop", "server", "claude"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, args := BuildSessionFilterSQL(filter, tt.dialect)
			normalized := normalizeSQL(got)
			assert.Contains(t, normalized, "WITH RECURSIVE tree(id) AS")
			assert.Contains(t, normalized, "JOIN tree t ON s.parent_session_id = t.id")
			assert.Contains(t, normalized, "id IN (WITH RECURSIVE tree(id) AS")
			assert.Contains(t, normalized,
				"NOT (root_session.relationship_type IN ('subagent', 'fork', 'continuation'))")
			assert.NotContains(t, normalized,
				"relationship_type NOT IN ('subagent', 'fork') AND id IN")
			assert.Equal(t, tt.wantArgs, args)
		})
	}
}

func TestBuildSessionFilterSQLHandlesEmptyCSVFilters(t *testing.T) {
	got, args := BuildSessionFilterSQL(SessionFilter{
		Machine: ",,",
		Agent:   "  ",
	}, SQLiteQueryDialect())

	assert.Contains(t, normalizeSQL(got), "1 = 0")
	assert.Empty(t, args)
}

func TestBuildSessionFilterSQLRendersBranchPairs(t *testing.T) {
	filter := SessionFilter{
		Machine: "laptop",
		GitBranch: encodeBranchFilterTokensForTest(
			BranchInfo{Project: "alpha", Branch: ""},
			BranchInfo{Project: "alpha", Branch: "unknown"},
		),
		Agent: "claude",
	}

	tests := []struct {
		name      string
		dialect   QueryDialect
		wantParts []string
	}{
		{
			name:    "sqlite",
			dialect: SQLiteQueryDialect(),
			wantParts: []string{
				"machine = ?",
				"((project = ? AND git_branch = ?) OR (project = ? AND git_branch = ?))",
				"agent = ?",
			},
		},
		{
			name:    "postgres",
			dialect: PostgresQueryDialect(),
			wantParts: []string{
				"machine = $1",
				"((project = $2 AND git_branch = $3) OR (project = $4 AND git_branch = $5))",
				"agent = $6",
			},
		},
		{
			name:    "duckdb",
			dialect: DuckDBQueryDialect(),
			wantParts: []string{
				"machine = ?",
				"((project = ? AND git_branch = ?) OR (project = ? AND git_branch = ?))",
				"agent = ?",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, args := BuildSessionFilterSQL(filter, tt.dialect)
			normalized := normalizeSQL(got)

			for _, part := range tt.wantParts {
				assert.Contains(t, normalized, normalizeSQL(part))
			}
			assert.Equal(t, []any{"laptop", "alpha", "", "alpha", "unknown", "claude"}, args)
		})
	}
}

func TestBranchPairClauseArgsKeepsEmptyBranchDistinct(t *testing.T) {
	tokens := encodeBranchFilterTokensForTest(
		BranchInfo{Project: "alpha", Branch: ""},
		BranchInfo{Project: "alpha", Branch: "unknown"},
	)

	got, args := BranchPairClauseArgs("project", "git_branch", tokens, nil)

	assert.Equal(t,
		"((project = ? AND git_branch = ?) OR (project = ? AND git_branch = ?))",
		normalizeSQL(got))
	assert.Equal(t, []any{"alpha", "", "alpha", "unknown"}, args)
}

func TestSessionCursorFragmentsAreParameterized(t *testing.T) {
	cursor := SessionCursor{
		EndedAt: "2026-06-08T12:00:00Z",
		ID:      "sess-123",
	}

	tests := []struct {
		name       string
		dialect    QueryDialect
		startIndex int
		wantWhere  string
		wantLimit  string
		wantArgs   []any
	}{
		{
			name:       "sqlite",
			dialect:    SQLiteQueryDialect(),
			startIndex: 0,
			wantWhere:  `(COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at), id) < (?, ?)`,
			wantLimit:  "LIMIT ? OFFSET ?",
		},
		{
			name:       "postgres",
			dialect:    PostgresQueryDialect(),
			startIndex: 20,
			wantWhere:  "(COALESCE(ended_at, started_at, created_at), id) < ($21::timestamptz, $22)",
			wantLimit:  "LIMIT $23 OFFSET $24",
		},
		{
			name:       "duckdb",
			dialect:    DuckDBQueryDialect(),
			startIndex: 0,
			wantWhere:  `(COALESCE(ended_at, started_at, created_at), id) < (CAST(? AS TIMESTAMP), ?)`,
			wantLimit:  "LIMIT ? OFFSET ?",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewQueryBuilder(tt.dialect, tt.startIndex)
			where := b.CursorBeforePredicate(cursor)
			limit := b.LimitOffset(51, 200)

			assert.Equal(t, normalizeSQL(tt.wantWhere), normalizeSQL(where))
			assert.Equal(t, normalizeSQL(tt.wantLimit), normalizeSQL(limit))
			assert.Equal(t, []any{cursor.EndedAt, cursor.ID, 51, 200}, b.Args())
		})
	}
}

func TestQueryDialectPredicatesKeepUserValuesParameterized(t *testing.T) {
	userPattern := "secret%' OR 1=1 --"
	userRegex := "(?i)secret.*token"

	tests := []struct {
		name      string
		dialect   QueryDialect
		wantLike  string
		wantRegex string
		wantArgs  []any
	}{
		{
			name:      "sqlite",
			dialect:   SQLiteQueryDialect(),
			wantLike:  "body LIKE ? ESCAPE '\\'",
			wantRegex: "body REGEXP ?",
			wantArgs:  []any{"%" + EscapeLikePattern(userPattern) + "%", userRegex},
		},
		{
			name:      "postgres",
			dialect:   PostgresQueryDialect(),
			wantLike:  "body ILIKE $1 ESCAPE E'\\\\'",
			wantRegex: "body ~* $2",
			wantArgs:  []any{"%" + EscapeLikePattern(userPattern) + "%", userRegex},
		},
		{
			name:      "duckdb",
			dialect:   DuckDBQueryDialect(),
			wantLike:  "body ILIKE ? ESCAPE '\\'",
			wantRegex: "regexp_matches(body, ?)",
			wantArgs:  []any{"%" + EscapeLikePattern(userPattern) + "%", userRegex},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := NewQueryBuilder(tt.dialect, 0)
			like := b.ContainsPredicate("body", userPattern)
			regex := b.RegexPredicate("body", userRegex)

			assert.Equal(t, normalizeSQL(tt.wantLike), normalizeSQL(like))
			assert.Equal(t, normalizeSQL(tt.wantRegex), normalizeSQL(regex))
			assert.Equal(t, tt.wantArgs, b.Args())
			assert.NotContains(t, like, userPattern)
			assert.NotContains(t, regex, userRegex)
		})
	}
}

func TestQueryDialectQualifyIdentifier(t *testing.T) {
	assert.Equal(t, `"catalog"."schema"."sessions"`,
		PostgresQueryDialect().Qualify("catalog", "schema", "sessions"))
	assert.Equal(t, `"safe_name"`,
		DuckDBQueryDialect().Qualify("", "", "safe_name"))

	require.Panics(t, func() {
		SQLiteQueryDialect().Qualify("", "", `sessions"; DROP TABLE sessions; --`)
	})
}

func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func TestNormalizeSQLHelperDoesNotMaskPlaceholders(t *testing.T) {
	require.True(t, regexp.MustCompile(`\?\)|\$1`).MatchString(
		normalizeSQL("value IN (?,?) OR value = $1")))
}
