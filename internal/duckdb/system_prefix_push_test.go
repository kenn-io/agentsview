//go:build !(windows && arm64)

package duckdb

import (
	"crypto/sha256"
	"fmt"
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
		{"", "user", false}, {"ordinary", "user", false},
		{"This session is being continued", "user", true},
		{"[Request interrupted", "user", true},
		{" \ufeff<task-notification>task", "user", true},
		{"<command-message>command", "user", true},
		{"<command-name>command", "user", true},
		{"<local-command-output>output", "user", true},
		{"Stop hook feedback: output", "user", true},
		{"<TASK-NOTIFICATION>task", "user", false},
		{"<system-reminder>a</system-reminder>", "user", true},
		{"<system-reminder>a</system-reminder><task-notification>task", "user", true},
		{"<system-reminder>a</system-reminder>ordinary", "user", false},
		{"<system-reminder>broken", "user", false},
		{"<goal_context>goal</goal_context>", "user", true},
		{"<codex_internal_context source=\"goal\">goal", "user", true},
		{"<task-notification>task", "assistant", false},
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
			var flag, visible bool
			err := conn.QueryRowContext(ctx, `SELECT is_system_prefixed, `+db.DuckDBSystemPrefixSQL("content", "role")+` FROM messages WHERE session_id = 'prefix' AND ordinal = ?`, i).Scan(&flag, &visible)
			require.NoError(t, err)
			require.Equal(t, c.want, flag, "case %d", i)
			require.Equal(t, !visible, flag, "SQL parity case %d", i)
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

func TestSystemPrefixRuleVersion(t *testing.T) {
	got := fmt.Sprintf("%x", sha256.Sum256([]byte(db.DuckDBSystemPrefixSQL("c", "r"))))
	require.Equal(t, "5ea07257a66f4605c2be0cc001efe601b44ff590aa74d1f6156a1d7b4b4ac78b", got, "system-prefix rules changed; bump SchemaVersion")
}

func TestSystemPrefixSearchAndTrends(t *testing.T) {
	text := "<task-notification>uniqueterm"
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session: unitSession("prefix-search", 2),
		Messages: []db.Message{
			unitMsg("prefix-search", 0, "user", text, false, false),
			unitMsg("prefix-search", 1, "assistant", text, false, false),
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
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
