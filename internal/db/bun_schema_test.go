package db

import (
	"database/sql"
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

func TestCheckCommonSchemaRejectsEmptyRequiredSessionProvenance(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)
	_, err = store.NewInsert().Model(&bunmodel.Session{
		ID: "missing-provenance", Project: "project", Machine: "machine",
		Agent: "agent", CreatedAt: bunmodel.NewTimestamp(
			time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		),
	}).Exec(t.Context())
	require.NoError(t, err)

	err = CheckCommonSchema(t.Context(), store)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session provenance")
}
