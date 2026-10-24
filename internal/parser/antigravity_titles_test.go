package parser

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestAntigravityTitleCacheExpiresMatchingMarkers(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	priorNow := antigravityTitleNow
	antigravityTitleNow = func() time.Time { return now }
	t.Cleanup(func() { antigravityTitleNow = priorNow })
	store := filepath.Join(t.TempDir(), AntigravitySummariesDBName)
	conn, err := sql.Open("sqlite3", store)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	_, err = conn.ExecContext(t.Context(), testjsonl.AntigravityTitleSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `PRAGMA journal_mode=WAL; INSERT INTO conversation_summaries VALUES ('conversation-a', 'First')`)
	require.NoError(t, err)
	checkpoint := func() {
		_, err := conn.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
		require.NoError(t, err)
		for _, path := range []string{store, store + "-wal"} {
			require.NoError(t, os.Chtimes(path, now, now))
		}
	}
	checkpoint()
	summaries, _, err := AntigravityStoreSummaries(t.Context(), store)
	require.NoError(t, err)
	require.Equal(t, "First", summaries["conversation-a"].Title)
	before, ok := StatSQLiteContainerState(store)
	require.True(t, ok)
	_, err = conn.ExecContext(t.Context(), "UPDATE conversation_summaries SET title = 'Later'")
	require.NoError(t, err)
	checkpoint()
	after, ok := StatSQLiteContainerState(store)
	require.True(t, ok)
	require.Equal(t, before, after)
	summaries, _, err = AntigravityStoreSummaries(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "First", summaries["conversation-a"].Title)
	now = now.Add(SQLiteContainerDigestVerifyInterval)
	summaries, _, err = AntigravityStoreSummaries(t.Context(), store)
	require.NoError(t, err)
	assert.Equal(t, "Later", summaries["conversation-a"].Title)
}

func TestAntigravityWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "empty column", raw: "", want: ""},
		{name: "empty array", raw: "[]", want: ""},
		{name: "json null", raw: "null", want: ""},
		{name: "json empty string", raw: `""`, want: ""},
		{name: "array skips blank uris", raw: `["", "file:///srv/synthetic"]`, want: filepath.Clean("/srv/synthetic")},
		{name: "json string uri", raw: `"file:///srv/synthetic"`, want: filepath.Clean("/srv/synthetic")},
		{name: "bare uri", raw: "file:///srv/synthetic", want: filepath.Clean("/srv/synthetic")},
		{name: "bare path", raw: "/srv/raw-synthetic", want: filepath.Clean("/srv/raw-synthetic")},
		{name: "encoded windows drive", raw: `["file:///d%3A/synthetic-workspace"]`, want: filepath.Clean(filepath.FromSlash("d:/synthetic-workspace"))},
		{name: "plain windows drive", raw: `["file:///D:/synthetic/app"]`, want: filepath.Clean(filepath.FromSlash("D:/synthetic/app"))},
		{name: "localhost authority", raw: `["file://localhost/d:/local/repo"]`, want: filepath.Clean(filepath.FromSlash("d:/local/repo"))},
		{name: "unc share", raw: `["file://server/share/repo"]`, want: filepath.Clean(filepath.FromSlash("//server/share/repo"))},
		{name: "encoded unicode", raw: `["file:///c%3A/%E6%96%B0%E5%BB%BA"]`, want: filepath.Clean(filepath.FromSlash("c:/新建"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, antigravityWorkspace(tc.raw))
		})
	}
}

func writeAntigravitySummaryStore(t *testing.T, root, schema string, values ...any) {
	t.Helper()
	conn, err := sql.Open("sqlite3", filepath.Join(root, AntigravitySummariesDBName))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), schema)
	require.NoError(t, err)
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(values)), ", ")
	_, err = conn.ExecContext(t.Context(), "INSERT INTO conversation_summaries VALUES ("+placeholders+")", values...)
	require.NoError(t, err)
}

func TestApplyAntigravityTitleFillsWorkspaceAndParent(t *testing.T) {
	const id = "synthetic-conv"
	const fullSchema = `CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY,
		title TEXT, workspace_uris TEXT NOT NULL DEFAULT '', parent_conversation_id TEXT NOT NULL DEFAULT '')`
	workspace := filepath.Clean(filepath.FromSlash("d:/synthetic-workspace"))
	for _, tc := range []struct {
		name        string
		schema      string
		values      []any
		session     ParsedSession
		wantCwd     string
		wantProject string
		wantParent  string
	}{
		{
			name:   "fills an empty session",
			schema: fullSchema, values: []any{id, "Title", `["file:///d%3A/synthetic-workspace"]`, "parent"},
			wantCwd: workspace, wantProject: "synthetic_workspace", wantParent: "antigravity:parent",
		},
		{
			name:   "keeps transcript values and a project hint",
			schema: fullSchema, values: []any{id, "Title", `["file:///d%3A/synthetic-workspace"]`, "parent"},
			session: ParsedSession{Project: "hint", ParentSessionID: "antigravity:own"},
			wantCwd: workspace, wantProject: "hint", wantParent: "antigravity:own",
		},
		{
			name:   "store without the optional columns",
			schema: testjsonl.AntigravityTitleSchema, values: []any{id, "Title"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeAntigravitySummaryStore(t, root, tc.schema, tc.values...)
			sess := tc.session
			applyAntigravityTitle(t.Context(), &sess, antigravitySource{Root: root, ID: id})
			assert.Equal(t, "Title", sess.SessionName)
			assert.Equal(t, tc.wantCwd, sess.Cwd)
			assert.Equal(t, tc.wantProject, sess.Project)
			assert.Equal(t, tc.wantParent, sess.ParentSessionID)
			assert.True(t, sess.KeepStoredWorkspace, "the transcript supplied no working directory")
			assert.Equal(t, tc.session.ParentSessionID == "", sess.KeepStoredParent)
			if tc.wantParent == "antigravity:parent" {
				assert.Equal(t, RelSubagent, sess.RelationshipType)
			}
		})
	}
}
