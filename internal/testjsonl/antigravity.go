package testjsonl

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
)

const AntigravityTitleSchema = `CREATE TABLE IF NOT EXISTS conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT)`

func WriteAntigravityBrainTranscript(t *testing.T, root, id, body string) string {
	t.Helper()
	path := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// AntigravitySummary is one conversation's row in the summary store, in the
// shape Antigravity records it: WorkspaceURIs is a JSON array of folder URIs.
type AntigravitySummary struct {
	Title         string
	WorkspaceURIs string
	ParentID      string
}

// WriteAntigravitySummaries upserts rows, keyed by conversation ID, into
// root's conversation_summaries.db with the columns AgentsView reads.
func WriteAntigravitySummaries(t *testing.T, root string, rows map[string]AntigravitySummary) string {
	t.Helper()
	path := filepath.Join(root, "conversation_summaries.db")
	conn, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS conversation_summaries (
		conversation_id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
		workspace_uris TEXT NOT NULL DEFAULT '[]', parent_conversation_id TEXT NOT NULL DEFAULT '')`)
	require.NoError(t, err)
	for id, row := range rows {
		uris := row.WorkspaceURIs
		if uris == "" {
			uris = "[]"
		}
		_, err = conn.ExecContext(t.Context(), `INSERT INTO conversation_summaries
			(conversation_id, title, workspace_uris, parent_conversation_id) VALUES (?, ?, ?, ?)
			ON CONFLICT(conversation_id) DO UPDATE SET title = excluded.title,
			workspace_uris = excluded.workspace_uris, parent_conversation_id = excluded.parent_conversation_id`,
			id, row.Title, uris, row.ParentID)
		require.NoError(t, err)
	}
	return path
}
