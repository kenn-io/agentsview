//go:build chtest

package clickhouse

import (
	"database/sql"
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
	for _, tc := range []struct {
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
	} {
		var visible bool
		require.NoError(t, conn.QueryRowContext(t.Context(), query, tc.content, tc.role).Scan(&visible))
		assert.Equal(t, tc.visible, visible, "%q", tc.content)
	}
}
