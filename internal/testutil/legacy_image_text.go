package testutil

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

const imageTextOccurrencesA080 = `CREATE TABLE image_text_occurrences (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, call_id INTEGER REFERENCES tool_calls(id) ON DELETE CASCADE, event_id INTEGER REFERENCES tool_result_events(id) ON DELETE CASCADE, image_index INTEGER NOT NULL, image_hash TEXT NOT NULL, CHECK ((call_id IS NULL) <> (event_id IS NULL)), PRIMARY KEY (session_id, image_hash));`

const imageTextOccurrencesB831 = `CREATE TABLE image_text_occurrences (session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE, call_id INTEGER, event_id INTEGER, image_index INTEGER NOT NULL, image_hash TEXT NOT NULL, media_type TEXT NOT NULL, CHECK ((call_id IS NULL) <> (event_id IS NULL)));`

// LegacyImageTextSchema installs the image-text tables and AFTER DELETE trigger
// used by unreleased builds a08089649 and b831f99cc.
func LegacyImageTextSchema(t testing.TB, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, later bool) {
	t.Helper()
	occurrences := imageTextOccurrencesA080
	if later {
		occurrences = imageTextOccurrencesB831
	}
	_, err := db.ExecContext(t.Context(), `
		DROP TRIGGER image_text_session_delete;
		DROP TABLE image_text_occurrences;
		DROP TABLE image_text_cache;
		DROP VIEW message_search_content;
		CREATE VIEW message_search_content AS SELECT id, session_id, content AS rendered, COALESCE(dialogue_text, content) AS content FROM messages;
		CREATE TABLE image_text_cache (image_hash TEXT NOT NULL, processor TEXT NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL, revision TEXT NOT NULL, text TEXT NOT NULL DEFAULT '', succeeded INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0, retry_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '', PRIMARY KEY (image_hash, processor));
		`+occurrences+`
		CREATE VIRTUAL TABLE image_text_fts USING fts5(text, content='image_text_cache', content_rowid='rowid', tokenize='porter unicode61');
		CREATE TRIGGER image_text_session_delete AFTER DELETE ON sessions BEGIN DELETE FROM image_text_cache WHERE NOT EXISTS (SELECT 1 FROM image_text_occurrences o WHERE o.image_hash=image_text_cache.image_hash); END;
	`)
	require.NoError(t, err)
}
