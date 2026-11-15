//go:build !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func TestSystemPrefixPush(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	cases := []struct {
		content, role string
		want          bool
	}{
		{"ordinary", "user", false},
		{"<task-notification>task", "user", true},
		{"<task-notification>task", "assistant", false},
		{`<codex_internal_context source="goal"/other>goal`, "user", false},
	}
	msgs := make([]db.Message, len(cases))
	for i, c := range cases {
		msgs[i] = syncMessage("prefix", i, c.role, c.content, "2026-01-01T12:00:00Z")
	}
	session := syncSession("prefix", "project", "prefix", "2026-01-01T12:00:00Z", len(msgs))
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: session, Messages: msgs, DataVersion: 1, ReplaceMessages: true}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	check := func() {
		conn, err := Open(ctx, path)
		require.NoError(t, err)
		defer conn.Close()
		for i, c := range cases {
			var flag bool
			err := conn.QueryRowContext(ctx, `SELECT is_system_prefixed FROM messages WHERE session_id = 'prefix' AND ordinal = ?`, i).Scan(&flag)
			require.NoError(t, err)
			require.Equal(t, c.want, flag, "case %d", i)
		}
	}
	check()
	cases[0].content, cases[0].want = "<task-notification>changed", true
	msgs[0].Content = cases[0].content
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: session, Messages: msgs, DataVersion: 2, ReplaceMessages: true}})
	require.NoError(t, err)
	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.False(t, result.Diagnostics.Full)
	check()
}

func TestSystemPrefixSQLParity(t *testing.T) {
	ctx := t.Context()
	inputs := []string{
		"", "ordinary", "<task-notification>", "<TASK-NOTIFICATION>",
		"<system-reminder>x</system-reminder>",
		"<system-reminder>x</system-reminder>ordinary",
		"<system-reminder>x</system-reminder><task-notification>",
		"<system-reminder>x</system-reminder><system-reminder>y</system-reminder>",
		"<system-reminder>broken", "<goal_context>goal",
		`<codex_internal_context source="goal">`,
		`<codex_internal_context source="goal"/>`,
		`<codex_internal_context source="goal"/other>`,
		`<codex_internal_context data-source="goal">`,
		`<codex_internal_context source="goal"`,
		"<codex_internal_context\vsource=\"goal\">",
		"<codex_internal_context source=\"goal\"\vother>",
	}
	for _, prefix := range db.SystemMsgPrefixes {
		inputs = append(inputs, prefix)
	}
	for _, space := range " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff" {
		inputs = append(inputs, string(space)+"<task-notification>")
	}
	conn := openTestDuckDB(t)
	for _, content := range inputs {
		for _, role := range []string{"user", "assistant"} {
			var excluded bool
			err := conn.QueryRowContext(ctx, `SELECT NOT (`+db.DuckDBSystemPrefixSQL("content", "role")+`) FROM (SELECT ? AS content, ? AS role)`, content, role).Scan(&excluded)
			require.NoError(t, err)
			require.Equal(t, excluded, db.IsDuckDBSystemPrefixed(content, role), "%q, %s", content, role)
		}
	}
}

func TestSystemPrefixSearchAndTrends(t *testing.T) {
	text := "uniqueterm"
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("prefix-search", 2),
		Messages: []db.Message{
			unitMsg("prefix-search", 0, "user", text, false, false),
			unitMsg("prefix-search", 1, "assistant", text, false, false),
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	// Deliberately differ from the content-derived verdict to exercise the stored column.
	_, err := store.DB().ExecContext(t.Context(), `UPDATE messages SET is_system_prefixed = TRUE WHERE session_id = 'prefix-search' AND ordinal = 0`)
	require.NoError(t, err)
	search, err := store.Search(t.Context(), db.SearchFilter{Query: "uniqueterm", Limit: 10})
	require.NoError(t, err)
	require.Len(t, search.Results, 1)
	require.Equal(t, 1, search.Results[0].Ordinal)
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "uniqueterm", Mode: "substring", Sources: []string{"messages"}, IncludeOneShot: true, ExcludeSystem: true, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	require.Equal(t, 1, page.Matches[0].Ordinal)
	terms, err := db.ParseTrendTerms([]string{"uniqueterm"})
	require.NoError(t, err)
	trends, err := store.GetTrendsTerms(t.Context(), db.AnalyticsFilter{From: "2026-05-01", To: "2026-05-01", Timezone: "UTC"}, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 1, trends.Series[0].Total)
}
