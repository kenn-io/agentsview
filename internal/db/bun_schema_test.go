package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestBunSchemaCreatesAndChecksCanonicalSQLiteSchema(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	_, err = raw.ExecContext(t.Context(), `PRAGMA foreign_keys = ON`)
	require.NoError(t, err)
	store := bun.NewDB(raw, sqlitedialect.New())

	require.NoError(t, CreateCommonSchema(t.Context(), store))
	require.NoError(t, CheckCommonSchema(t.Context(), store))

	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive-1", SourceArchiveSalt: "salt-1",
	}).Exec(t.Context())
	require.NoError(t, err)
	want := bunmodel.Session{
		ID: "canonical-session", Project: "project", Machine: "machine",
		Agent: "agent", CreatedAt: bunmodel.NewTimestamp(
			time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		),
		SourceArchiveID:          "archive-1",
		SourceDatabaseGeneration: "generation-1",
	}
	_, err = store.NewInsert().Model(&want).Exec(t.Context())
	require.NoError(t, err)

	var got bunmodel.Session
	require.NoError(t, store.NewSelect().Model(&got).
		Where("id = ?", want.ID).Scan(t.Context()))
	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, want.SourceArchiveID, got.SourceArchiveID)
	assert.Equal(t, want.SourceDatabaseGeneration, got.SourceDatabaseGeneration)
}

func TestBunSchemaNormalSQLiteOpenAcceptsCanonicalSession(t *testing.T) {
	database := testDB(t)
	archiveID, err := database.GetArchiveID(t.Context())
	require.NoError(t, err)
	generation, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	want := bunmodel.Session{
		ID: "normal-open-session", Project: "project", Machine: "machine",
		Agent: "agent", CreatedAt: bunmodel.NewTimestamp(
			time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		),
		SourceArchiveID:          archiveID,
		SourceDatabaseGeneration: generation,
	}

	err = database.update(t.Context(), WriteArchive, func(store bun.IDB) error {
		_, insertErr := store.NewInsert().Model(&want).Exec(t.Context())
		return insertErr
	})
	require.NoError(t, err)

	var got bunmodel.Session
	err = database.view(t.Context(), func(store bun.IDB) error {
		return store.NewSelect().Model(&got).
			Where("id = ?", want.ID).Scan(t.Context())
	})
	require.NoError(t, err)
	assert.Equal(t, want.ID, got.ID)
	assert.Equal(t, archiveID, got.SourceArchiveID)
	assert.Equal(t, generation, got.SourceDatabaseGeneration)
}

func TestBunSchemaNormalSQLiteOpenUsesAcceptedRelationshipMatrix(t *testing.T) {
	database := testDB(t)
	type foreignKey struct {
		Parent   string
		From     string
		To       string
		OnDelete string
	}
	wantByTable := map[string][]foreignKey{
		"messages": {{
			Parent: "sessions", From: "session_id", To: "id", OnDelete: "CASCADE",
		}},
		"tool_calls": {
			{Parent: "messages", From: "message_id", To: "id", OnDelete: "CASCADE"},
			{Parent: "sessions", From: "session_id", To: "id", OnDelete: "CASCADE"},
		},
		"pinned_messages": {
			{Parent: "messages", From: "message_id", To: "id", OnDelete: "CASCADE"},
			{Parent: "sessions", From: "session_id", To: "id", OnDelete: "CASCADE"},
		},
		"tool_result_events": {{
			Parent: "sessions", From: "session_id", To: "id", OnDelete: "CASCADE",
		}},
	}

	for table, want := range wantByTable {
		rows, err := database.getReader().QueryContext(t.Context(), `
			SELECT "table", "from", "to", on_delete
			FROM pragma_foreign_key_list(?)`, table)
		require.NoError(t, err, table)
		var got []foreignKey
		for rows.Next() {
			var item foreignKey
			require.NoError(t, rows.Scan(
				&item.Parent, &item.From, &item.To, &item.OnDelete,
			), table)
			got = append(got, item)
		}
		require.NoError(t, rows.Err(), table)
		require.NoError(t, rows.Close(), table)
		assert.ElementsMatch(t, want, got, table)
	}
}

func TestBunSchemaRawSQLiteToolCallInsertResolvesMessageOrdinal(t *testing.T) {
	database := testDB(t)
	insertSession(t, database, "raw-tool-session", "project")
	insertMessages(t, database, userMsg("raw-tool-session", 5, "hello"))

	var messageID int64
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT id FROM messages
		WHERE session_id = 'raw-tool-session' AND ordinal = 5`,
	).Scan(&messageID))
	_, err := database.getWriter().ExecContext(t.Context(), `
		INSERT INTO tool_calls (
			message_id, session_id, tool_name, category, call_index
		) VALUES (?, 'raw-tool-session', 'Read', 'Read', 0)`, messageID)
	require.NoError(t, err)

	var ordinal int
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT message_ordinal FROM tool_calls
		WHERE session_id = 'raw-tool-session' AND call_index = 0`,
	).Scan(&ordinal))
	assert.Equal(t, 5, ordinal)
}

func TestBunSchemaSessionBatchStampsArchiveProvenance(t *testing.T) {
	database := testDB(t)
	createdAt := "2026-08-02T12:00:00Z"
	_, err := database.WriteSessionBatchAtomic([]SessionBatchWrite{{
		Session: Session{
			ID: "stamped-session", Project: "project", Machine: "machine",
			Agent: "agent", CreatedAt: createdAt,
		},
		Messages: []Message{{
			SessionID: "stamped-session", Ordinal: 0, Role: "user",
			Content: "hello", Timestamp: createdAt,
		}},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	var archiveID, generation string
	require.NoError(t, database.getReader().QueryRowContext(t.Context(), `
		SELECT source_archive_id, source_database_generation
		FROM sessions WHERE id = 'stamped-session'`,
	).Scan(&archiveID, &generation))
	assert.NotEmpty(t, archiveID)
	assert.NotEmpty(t, generation)
}

func TestBunSchemaStampedReopenRejectsTriggerDriftWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		DROP TRIGGER IF EXISTS trg_sessions_delete_source_project_identity_snapshot;
		CREATE TRIGGER trg_sessions_delete_source_project_identity_snapshot
		AFTER DELETE ON sessions BEGIN
			SELECT 'drifted trigger';
		END;`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	reopened, err := Open(path)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.Error(t, err)

	raw, err = sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var triggerSQL string
	require.NoError(t, raw.QueryRowContext(t.Context(), `
		SELECT sql FROM sqlite_schema
		WHERE type = 'trigger'
		  AND name = 'trg_sessions_delete_source_project_identity_snapshot'`,
	).Scan(&triggerSQL))
	assert.Contains(t, triggerSQL, "drifted trigger")
}

func TestBunSchemaStampedReopenRejectsIndexDriftWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		DROP INDEX idx_sessions_project;
		CREATE INDEX idx_sessions_project ON sessions(machine);`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	reopened, err := Open(path)
	if reopened != nil {
		require.NoError(t, reopened.Close())
	}
	require.Error(t, err)

	raw, err = sql.Open("sqlite3", path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var indexedColumn string
	require.NoError(t, raw.QueryRowContext(t.Context(), `
		SELECT name FROM pragma_index_info('idx_sessions_project')
		ORDER BY seqno LIMIT 1`,
	).Scan(&indexedColumn))
	assert.Equal(t, "machine", indexedColumn)
}

func TestBunSchemaStampedReopenSkipsRowInvariantScans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := Open(path)
	require.NoError(t, err)
	archiveID, err := database.GetArchiveID(t.Context())
	require.NoError(t, err)
	databaseID, err := database.GetDatabaseID(t.Context())
	require.NoError(t, err)
	_, err = database.getWriter().ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, project, machine, agent, created_at,
			source_archive_id, source_database_generation
		) VALUES (?, 'project', 'machine', 'agent', ?, ?, ?);
		INSERT INTO messages (session_id, ordinal, role, content)
		VALUES ('invalid-tool-session', 0, 'assistant', 'done');
		INSERT INTO tool_calls (
			message_id, session_id, message_ordinal,
			tool_name, category, call_index
		) SELECT id, session_id, ordinal, 'Read', 'Read', 0
		  FROM messages WHERE session_id = 'invalid-tool-session';
		UPDATE tool_calls SET message_ordinal = NULL
		WHERE session_id = 'invalid-tool-session';`,
		"invalid-tool-session", "2026-08-02T12:00:00Z", archiveID, databaseID,
	)
	require.NoError(t, err)
	require.NoError(t, database.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })

	var invalidRows int
	require.NoError(t, reopened.getReader().QueryRowContext(t.Context(), `
		SELECT count(*) FROM tool_calls WHERE message_ordinal IS NULL`,
	).Scan(&invalidRows))
	assert.Equal(t, 1, invalidRows)
}
