package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const legacyMessagesAndToolCallsSchema = `
CREATE TABLE messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);
CREATE TABLE tool_calls (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL
);`

const preParentLegacySchema = `
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);` + legacyMessagesAndToolCallsSchema

const singleDateLegacySchema = `
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    parent_session_id TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);` + legacyMessagesAndToolCallsSchema + `
CREATE TABLE insights (
    id          INTEGER PRIMARY KEY,
    type        TEXT NOT NULL,
    date        TEXT NOT NULL,
    project     TEXT,
    agent       TEXT NOT NULL,
    model       TEXT,
    prompt      TEXT,
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);`

const legacyArchiveRows = `
INSERT INTO sessions (
    id, project, machine, agent, first_message, message_count,
    file_path, file_size, file_mtime, file_hash
) VALUES (
    'legacy-session', 'project-a', 'local', 'claude',
    'archived prompt', 1, '/archive/session.jsonl', 128, 42, 'legacy-hash'
);
INSERT INTO messages (
    id, session_id, ordinal, role, content, has_tool_use, content_length
) VALUES (
    1, 'legacy-session', 0, 'assistant', 'archived answer', 1, 15
);
INSERT INTO tool_calls (
    id, message_id, session_id, tool_name, category
) VALUES (
    1, 1, 'legacy-session', 'Read', 'Read'
);`

func TestOpenLegacySchemasPreservesArchiveAndRequestsResync(t *testing.T) {
	tests := []struct {
		name            string
		schema          string
		wantInsightDate string
	}{
		{
			name:   "pre-parent-link archive",
			schema: preParentLegacySchema,
		},
		{
			name:            "single-date insight and base tool calls",
			schema:          singleDateLegacySchema,
			wantInsightDate: "2026-02-23",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			conn, err := sql.Open("sqlite3", makeDSN(path, false))
			require.NoError(t, err)
			conn.SetMaxOpenConns(1)

			_, err = conn.Exec(tc.schema)
			require.NoError(t, err)
			_, err = conn.Exec(legacyArchiveRows)
			require.NoError(t, err)
			if tc.wantInsightDate != "" {
				_, err = conn.Exec(`
					INSERT INTO insights (
						id, type, date, project, agent, model, prompt, content
					) VALUES (
						1, 'daily', '2026-02-23', 'project-a', 'claude',
						'model-a', 'summarize', 'archived insight'
					)`)
				require.NoError(t, err)
			}
			_, err = conn.Exec(fmt.Sprintf(
				"PRAGMA user_version = %d", dataVersion,
			))
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			d, err := Open(path)
			require.NoError(t, err)
			require.True(t, d.NeedsResync())

			session := requireSessionExists(t, d, "legacy-session")
			assert.Equal(t, "project-a", session.Project)
			require.NotNil(t, session.FirstMessage)
			assert.Equal(t, "archived prompt", *session.FirstMessage)

			messages, err := d.GetMessages(
				context.Background(), "legacy-session", 0, 10, true,
			)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, "Read", messages[0].ToolCalls[0].ToolName)

			if tc.wantInsightDate != "" {
				var dateFrom, dateTo string
				err = d.getReader().QueryRow(`
					SELECT date_from, date_to FROM insights WHERE id = 1
				`).Scan(&dateFrom, &dateTo)
				require.NoError(t, err)
				assert.Equal(t, tc.wantInsightDate, dateFrom)
				assert.Equal(t, tc.wantInsightDate, dateTo)

				id, err := d.InsertInsight(Insight{
					Type:     "daily",
					DateFrom: "2026-07-14",
					DateTo:   "2026-07-14",
					Agent:    "claude",
					Content:  "new insight",
				})
				require.NoError(t, err)
				inserted, err := d.GetInsight(context.Background(), id)
				require.NoError(t, err)
				require.NotNil(t, inserted)
				assert.Equal(t, "2026-07-14", inserted.DateFrom)
				assert.Equal(t, "2026-07-14", inserted.DateTo)
				assert.Equal(t, "new insight", inserted.Content)

				requireIndexColumns(t, d, "idx_insights_lookup", []string{
					"type", "date_from", "date_to", "project",
				})
			}

			requireLegacyRepairIndexes(t, d)
			require.NoError(t, d.Close())

			reopened, err := Open(path)
			require.NoError(t, err)
			defer reopened.Close()
			require.True(t, reopened.NeedsResync())
		})
	}
}

func requireIndexColumns(
	t *testing.T, d *DB, index string, want []string,
) {
	t.Helper()
	rows, err := d.getReader().Query(`
		SELECT name FROM pragma_index_info(?) ORDER BY seqno
	`, index)
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		got = append(got, column)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, want, got)
}

func requireLegacyRepairIndexes(t *testing.T, d *DB) {
	t.Helper()
	for _, name := range []string{
		"idx_sessions_parent",
		"idx_sessions_user_message_count",
		"idx_tool_calls_skill",
		"idx_tool_calls_subagent",
		"idx_insights_lookup",
	} {
		var count int
		err := d.getReader().QueryRow(`
			SELECT count(*) FROM sqlite_master
			WHERE type = 'index' AND name = ?
		`, name).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "index %s", name)
	}
}
