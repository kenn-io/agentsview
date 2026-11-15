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
		{"<codex_internal_context source=\"goal\"/other>goal", "user", false},
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
	// Changing a stored verdict needs a mirror rebuild and fresh Trends parts,
	// even if the SQL oracle changes at the same time.
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
	hash := sha256.New()
	for _, content := range inputs {
		for _, role := range []string{"user", "assistant"} {
			fmt.Fprintf(hash, "%q %q %t\n", content, role, db.IsDuckDBSystemPrefixed(content, role))
		}
	}
	require.Equal(t, "67e2f7c0294c8def56f41870585d4735e0aac75d4ae0663c62ad941145e1cf1e", fmt.Sprintf("%x", hash.Sum(nil)), "system-prefix rules changed; bump SchemaVersion and update this digest")
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

func TestSearchRetainsMalformedGoalContext(t *testing.T) {
	store := newUnitsStore(t, []db.SessionBatchWrite{{
		Session:     unitSession("malformed-goal", 1),
		Messages:    []db.Message{unitMsg("malformed-goal", 0, "user", `<codex_internal_context source="goal"/other> uniqueparityterm`, false, false)},
		DataVersion: 1, ReplaceMessages: true,
	}})
	page, err := store.Search(t.Context(), db.SearchFilter{Query: "uniqueparityterm", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Results, 1)
	require.Equal(t, "malformed-goal", page.Results[0].SessionID)
}

func TestDuckDBGoalContextWhitespaceParity(t *testing.T) {
	conn := openTestDuckDB(t)
	for _, content := range []string{
		"<codex_internal_context\vsource=\"goal\"> text",
		"<codex_internal_context source=\"goal\"\vother> text",
	} {
		var excluded bool
		err := conn.QueryRowContext(t.Context(), `SELECT NOT (`+db.DuckDBSystemPrefixSQL("content", "role")+`) FROM (SELECT ? AS content, 'user' AS role)`, content).Scan(&excluded)
		require.NoError(t, err)
		require.True(t, excluded, "%q", content)
		require.Equal(t, excluded, db.IsDuckDBSystemPrefixed(content, "user"), "%q", content)
	}
}
