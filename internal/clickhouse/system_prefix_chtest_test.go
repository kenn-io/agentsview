//go:build chtest

package clickhouse

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
)

func TestSystemPrefixSQLClassification(t *testing.T) {
	conn, err := sql.Open("clickhouse", chtest.ServerURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	query := "SELECT " + db.ClickHouseSystemPrefixSQL("content", "role") + " FROM (SELECT ? AS content, ? AS role)" + systemPrefixSettings
	cases := []struct {
		content string
		role    string
		visible bool
	}{
		{"", "user", true},
		{"ordinary message", "user", true},
		{"\ufeff<task-notification>done", "user", false},
		{"\u2003<goal_context>state", "user", false},
		{"\u2003ordinary", "user", true},
		{"<system-reminder>a</system-reminder><system-reminder>b</system-reminder>", "user", false},
		{"<system-reminder>a</system-reminder>ordinary", "user", true},
		{"<system-reminder>unfinished", "user", true},
		{"<goal_context>state", "assistant", true},
	}
	for _, tc := range cases {
		var visible bool
		require.NoError(t, conn.QueryRowContext(t.Context(), query, tc.content, tc.role).Scan(&visible))
		assert.Equal(t, tc.visible, visible, "%q", tc.content)
	}

	args := make([]any, 0, len(cases)*3)
	for i, tc := range cases {
		args = append(args, i, tc.content, tc.role)
	}
	tuples := strings.TrimSuffix(strings.Repeat("tuple(?, ?, ?),", len(cases)), ",")
	query = "SELECT i, " + db.ClickHouseSystemPrefixSQL("content", "role") + " FROM (SELECT item.1 AS i, item.2 AS content, item.3 AS role FROM (SELECT arrayJoin([" + tuples + "]) AS item)) ORDER BY i" + systemPrefixSettings
	rows, err := conn.QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	for i, tc := range cases {
		require.True(t, rows.Next())
		var index uint64
		var visible bool
		require.NoError(t, rows.Scan(&index, &visible))
		assert.Equal(t, uint64(i), index)
		assert.Equal(t, tc.visible, visible, "%q", tc.content)
	}
	assert.False(t, rows.Next())
	require.NoError(t, rows.Err())
}
