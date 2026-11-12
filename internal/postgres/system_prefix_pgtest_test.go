//go:build pgtest

package postgres

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestSystemPrefixSQLClassification(t *testing.T) {
	conn, err := sql.Open("pgx", testPGURL(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	for name, predicate := range map[string]func(string, string) string{
		"guarded": db.PostgresSystemPrefixSQL,
		"scoped":  db.PostgresSystemPrefixSQLWithoutGuard,
	} {
		t.Run(name, func(t *testing.T) {
			query := "SELECT " + predicate("content", "role") + " FROM (SELECT $1::text AS content, $2::text AS role) AS candidate"
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
			var encoding string
			require.NoError(t, conn.QueryRowContext(t.Context(), "SHOW server_encoding").Scan(&encoding))
			if encoding == "SQL_ASCII" {
				// In SQL_ASCII, ltrim treats every byte of these characters as a
				// separate trim character, including UTF-8 continuation bytes.
				for _, b := range []byte("\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff") {
					var visible bool
					require.NoError(t, conn.QueryRowContext(t.Context(), query, string([]byte{b})+"<goal_context>state", "user").Scan(&visible))
					assert.False(t, visible, "leading byte %x", b)
				}
			}
		})
	}
}
