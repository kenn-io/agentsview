//go:build !(windows && arm64)

package duckdb

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	inputs = append(inputs, db.SystemMsgPrefixes...)
	for _, space := range " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff" {
		inputs = append(inputs, string(space)+"<task-notification>")
	}
	conn := openTestDuckDB(t)
	type classifierCase struct {
		Content string
		Role    string
		System  bool
	}
	results := make([]classifierCase, 0, len(inputs)*2)
	for _, content := range inputs {
		for _, role := range []string{"user", "assistant"} {
			var excluded bool
			err := conn.QueryRowContext(ctx, `SELECT NOT (`+db.DuckDBSystemPrefixSQL("content", "role")+`) FROM (SELECT ? AS content, ? AS role)`, content, role).Scan(&excluded)
			require.NoError(t, err)
			require.Equal(t, excluded, db.IsDuckDBSystemPrefixed(content, role), "%q, %s", content, role)
			results = append(results, classifierCase{Content: content, Role: role, System: excluded})
		}
	}
	fingerprintJSON, err := json.Marshal(results)
	require.NoError(t, err)
	fingerprint := sha256.Sum256(fingerprintJSON)
	wantBySchemaVersion := map[int]string{
		31: "139cb85920832ad5723b30ec66b584c145cc39be69098a42c5d6738500191cfb",
	}
	want, ok := wantBySchemaVersion[SchemaVersion]
	require.Truef(t, ok, "add a frozen system-prefix fingerprint for schema version %d", SchemaVersion)
	require.Equal(t, want, hex.EncodeToString(fingerprint[:]), "system-prefix rules changed; bump SchemaVersion and record the new fingerprint")
}

func TestSystemPrefixSearchAndContentExcludePrefixedMessages(t *testing.T) {
	text := "uniqueterm"
	store := newUnitsStore(t, []db.SessionBatchWrite{
		{
			Session: unitSession("prefix-search", 2),
			Messages: []db.Message{
				unitMsg("prefix-search", 0, "user", "<task-notification>"+text, false, false),
				unitMsg("prefix-search", 1, "assistant", text, false, false),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
		{
			Session: unitSession("prefix-malformed", 1),
			Messages: []db.Message{
				unitMsg("prefix-malformed", 0, "user", `<codex_internal_context source="goal"/other> `+text, false, false),
			},
			DataVersion: 1, ReplaceMessages: true,
		},
	})
	search, err := store.Search(t.Context(), db.SearchFilter{Query: "uniqueterm", Limit: 10})
	require.NoError(t, err)
	require.Len(t, search.Results, 2)
	require.ElementsMatch(t, []string{"prefix-search", "prefix-malformed"}, []string{search.Results[0].SessionID, search.Results[1].SessionID})
	page, err := store.SearchContent(t.Context(), db.ContentSearchFilter{
		Pattern: "uniqueterm", Mode: "substring", Sources: []string{"messages"}, IncludeOneShot: true, ExcludeSystem: true, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 2)
	require.ElementsMatch(t, []string{"prefix-search", "prefix-malformed"}, []string{page.Matches[0].SessionID, page.Matches[1].SessionID})
}
