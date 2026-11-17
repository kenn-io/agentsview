//go:build duckdb && !(windows && arm64)

package duckdb

import (
	"path/filepath"
	"testing"
	"time"

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
	check := func(i int) {
		conn, err := Open(ctx, path)
		require.NoError(t, err)
		defer conn.Close()
		var flag bool
		err = conn.QueryRowContext(ctx, `SELECT is_system_prefixed FROM messages WHERE session_id = 'prefix' AND ordinal = ?`, i).Scan(&flag)
		require.NoError(t, err)
		require.Equal(t, cases[i].want, flag, "case %d", i)
	}
	check(0)
	check(1)
	cases[0].content, cases[0].want = "<task-notification>changed", true
	msgs[0].Content = cases[0].content
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{Session: session, Messages: msgs, DataVersion: 2, ReplaceMessages: true}})
	require.NoError(t, err)
	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.False(t, result.Diagnostics.Full)
	check(0)
}

func TestIncrementalPushReclassifiesOldMirrorFlags(t *testing.T) {
	ctx := t.Context()
	local := newLocalDB(t)
	content := "<task-notification>staleclassifierterm"
	session := syncSession("old-prefix", "project", "old prefix", "2026-01-01T12:00:00Z", 1)
	message := syncMessage("old-prefix", 0, "user", content, "2026-01-01T12:00:00Z")
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: session, Messages: []db.Message{message}, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "old-mirror.duckdb")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	// This is an old classifier's saved verdict for a message the current rule hides.
	writeUsageMirror(t, path, `UPDATE messages SET is_system_prefixed = FALSE WHERE session_id = 'old-prefix' AND ordinal = 0`)
	writeUsageMirror(t, path, `UPDATE sync_metadata SET value = 'old' WHERE key = '`+systemPrefixRulesVersionMetadataKey+`'`)
	require.True(t, db.IsSystemPrefixed(content, "user"))

	otherTime := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	other := syncSession("new-session", "project", "unrelated", otherTime, 1)
	otherMessage := syncMessage("new-session", 0, "user", "unrelated", otherTime)
	_, err = local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: other, Messages: []db.Message{otherMessage}, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	result, err := Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)
	require.True(t, result.Diagnostics.Full)
	require.Equal(t, "system message filters changed", result.Diagnostics.RebuildReason)

	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	search, err := store.Search(ctx, db.SearchFilter{Query: "staleclassifierterm", Limit: 10})
	require.NoError(t, err)
	require.Empty(t, search.Results)
}

func TestSystemPrefixSQLParity(t *testing.T) {
	ctx := t.Context()
	inputs := []string{
		"", "ordinary", "　hello", "<task-notification>", "<TASK-NOTIFICATION>",
		"<system-reminder>x</system-reminder>",
		"<system-reminder>x</system-reminder>ordinary",
		"<system-reminder>x</system-reminder><task-notification>",
		"<system-reminder>x</system-reminder><system-reminder>y</system-reminder>",
		"<system-reminder>broken", "<goal_context>goal",
		`<codex_internal_context source="goal">`,
		`<codex_internal_context source="goal"/>`,
		`<codex_internal_context source="goal"/unexpected>`,
		`<codex_internal_context source="goal"/other>`,
		"<codex_internal_context\vsource=\"goal\">",
		"<codex_internal_context source=\"goal\"\vother>",
		`<codex_internal_context data-source="goal">`,
	}
	inputs = append(inputs, db.SystemMsgPrefixes...)
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

func TestSystemPrefixSearchAndContentExcludePrefixedMessages(t *testing.T) {
	text := "uniqueterm"
	store := newUnitsStore(t, []db.SessionBatchWrite{
		{
			Session: unitSession("prefix-search", 2),
			Messages: []db.Message{
				unitMsg("prefix-search", 0, "user", "<task-notification>"+text, false, false),
				unitMsg("prefix-search", 1, "user", "ordinary message before "+text, false, false),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
	})
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
}

func TestSearchRetainsMalformedGoalContext(t *testing.T) {
	ctx := t.Context()
	const content = `<codex_internal_context source="goal"/unexpected>needle`
	require.True(t, db.IsSystemPrefixed(content, "user"))
	require.False(t, db.IsDuckDBSystemPrefixed(content, "user"))

	local := newLocalDB(t)
	session := syncSession("malformed-goal", "project", "malformed goal", "2026-01-01T12:00:00Z", 1)
	message := syncMessage("malformed-goal", 0, "user", content, "2026-01-01T12:00:00Z")
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: session, Messages: []db.Message{message}, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	_, err = Push(ctx, path, local, "m", storage.MirrorPushOptions{}, false, nil)
	require.NoError(t, err)

	conn, err := Open(ctx, path)
	require.NoError(t, err)
	var ordinal int
	err = conn.QueryRowContext(ctx,
		`SELECT ordinal FROM messages WHERE content ILIKE ? AND `+db.DuckDBSystemPrefixSQL("content", "role"),
		"%needle%",
	).Scan(&ordinal)
	require.NoError(t, err, "the prior DuckDB Search predicate keeps this row")
	require.Equal(t, 0, ordinal)
	require.NoError(t, conn.Close())

	store := openPushedStore(t, path)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	search, err := store.Search(ctx, db.SearchFilter{Query: "needle", Limit: 10})
	require.NoError(t, err)
	require.Len(t, search.Results, 1, "Search must retain the row accepted by the prior DuckDB predicate")
	require.Equal(t, 0, search.Results[0].Ordinal)
}
