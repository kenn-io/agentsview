package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

const seededPinNote = "seeded curation pin"

// CurationStore is the Task 6 star and pin method family.
type CurationStore interface {
	StarSession(string) (bool, error)
	UnstarSession(string) error
	ListStarredSessionIDs(context.Context) ([]string, error)
	BulkStarSessions([]string) error
	PinMessage(string, int64, *string) (int64, error)
	UnpinMessage(string, int64) error
	ListPinnedMessages(context.Context, string, string) ([]db.PinnedMessage, error)
}

// CurationFixture identifies independently seeded star and pin rows.
type CurationFixture struct {
	Core            Fixture
	PinnedMessageID int64
	InitialPinRowID int64
	WriteMessageID  int64
}

// CurationBackend registers one embedded BunStore and its fixture setup.
type CurationBackend struct {
	Name     string
	Open     func(*testing.T) (CurationStore, CurationFixture)
	Writable bool
}

// RunCurationContract verifies shared star and pin reads plus operation-scoped
// write policy against one embedded BunStore.
func RunCurationContract(t *testing.T, backend CurationBackend) {
	t.Helper()
	t.Run(backend.Name, func(t *testing.T) {
		store, fixture := backend.Open(t)
		ctx := t.Context()

		stars, err := store.ListStarredSessionIDs(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{fixture.Core.RootOldID}, stars)

		pins, err := store.ListPinnedMessages(ctx, fixture.Core.RootNewID, "")
		require.NoError(t, err)
		require.Len(t, pins, 1)
		assert.Equal(t, fixture.InitialPinRowID, pins[0].ID)
		assert.Equal(t, fixture.PinnedMessageID, pins[0].MessageID)
		assert.Equal(t, 1, pins[0].Ordinal)
		require.NotNil(t, pins[0].Note)
		assert.Equal(t, seededPinNote, *pins[0].Note)

		allPins, err := store.ListPinnedMessages(ctx, "", "alpha")
		require.NoError(t, err)
		require.Len(t, allPins, 1)
		require.NotNil(t, allPins[0].Content)
		assert.Equal(t, "working", *allPins[0].Content)
		require.NotNil(t, allPins[0].Role)
		assert.Equal(t, "assistant", *allPins[0].Role)
		require.NotNil(t, allPins[0].SessionProject)
		assert.Equal(t, "alpha", *allPins[0].SessionProject)

		if !backend.Writable {
			assertReadOnlyCurationWrites(t, store, fixture)
			return
		}
		assertWritableCuration(t, store, fixture)
	})
}

func assertReadOnlyCurationWrites(
	t *testing.T, store CurationStore, fixture CurationFixture,
) {
	t.Helper()
	starred, err := store.StarSession(fixture.Core.RootNewID)
	assert.False(t, starred)
	require.ErrorIs(t, err, db.ErrReadOnly)
	require.ErrorIs(t, store.BulkStarSessions([]string{fixture.Core.ChildID}), db.ErrReadOnly)
	require.ErrorIs(t, store.UnstarSession(fixture.Core.RootOldID), db.ErrReadOnly)
	pinID, err := store.PinMessage(
		fixture.Core.RootNewID, fixture.WriteMessageID, nil,
	)
	assert.Zero(t, pinID)
	require.ErrorIs(t, err, db.ErrReadOnly)
	require.ErrorIs(t,
		store.UnpinMessage(fixture.Core.RootNewID, fixture.PinnedMessageID),
		db.ErrReadOnly,
	)
}

func assertWritableCuration(
	t *testing.T, store CurationStore, fixture CurationFixture,
) {
	t.Helper()
	ctx := t.Context()
	starred, err := store.StarSession(fixture.Core.RootNewID)
	require.NoError(t, err)
	assert.True(t, starred)
	starred, err = store.StarSession(fixture.Core.RootNewID)
	require.NoError(t, err)
	assert.True(t, starred, "starring is idempotent for an existing session")
	starred, err = store.StarSession("missing-curation-session")
	require.NoError(t, err)
	assert.False(t, starred)
	require.NoError(t, store.BulkStarSessions([]string{
		fixture.Core.ChildID, "missing-curation-session",
	}))
	stars, err := store.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		fixture.Core.RootOldID, fixture.Core.RootNewID, fixture.Core.ChildID,
	}, stars)
	require.NoError(t, store.UnstarSession(fixture.Core.RootOldID))

	updatedNote := "updated common pin"
	pinID, err := store.PinMessage(
		fixture.Core.RootNewID, fixture.PinnedMessageID, &updatedNote,
	)
	require.NoError(t, err)
	assert.Equal(t, fixture.InitialPinRowID, pinID)
	pinID, err = store.PinMessage(
		fixture.Core.ChildID, fixture.PinnedMessageID, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, pinID, "a message cannot be pinned through another session")
	pinID, err = store.PinMessage(
		fixture.Core.RootNewID, 999_999, nil,
	)
	require.NoError(t, err)
	assert.Zero(t, pinID)
	pins, err := store.ListPinnedMessages(ctx, fixture.Core.RootNewID, "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, updatedNote, *pins[0].Note)

	require.NoError(t,
		store.UnpinMessage(fixture.Core.RootNewID, fixture.PinnedMessageID),
	)
	pins, err = store.ListPinnedMessages(ctx, fixture.Core.RootNewID, "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

// InsertBunCurationFixture inserts canonical rows for generated PostgreSQL and
// DuckDB schemas.
func InsertBunCurationFixture(
	ctx context.Context, store bun.IDB, archiveID, generation string,
) (CurationFixture, error) {
	if err := InsertBunCoreFixture(ctx, store, archiveID, generation); err != nil {
		return CurationFixture{}, err
	}
	created := bunmodel.NewTimestamp(time.Date(2026, 8, 3, 13, 0, 0, 0, time.UTC))
	if _, err := store.NewInsert().Model(&bunmodel.StarredSession{
		SessionID: rootOldID, CreatedAt: created,
	}).Exec(ctx); err != nil {
		return CurationFixture{}, fmt.Errorf("inserting curation star: %w", err)
	}
	messageID := int64(802)
	note := seededPinNote
	pin := bunmodel.PinnedMessage{
		ID: 1201, SessionID: rootNewID, MessageID: &messageID, Ordinal: 1,
		Note: &note, CreatedAt: created,
	}
	if _, err := store.NewInsert().Model(&pin).Exec(ctx); err != nil {
		return CurationFixture{}, fmt.Errorf("inserting curation pin: %w", err)
	}
	return CurationFixture{
		Core: CoreFixture(), PinnedMessageID: messageID,
		InitialPinRowID: pin.ID, WriteMessageID: 803,
	}, nil
}

// InsertSQLiteCurationFixture inserts the same contract through SQLite's
// shipped message-row ID alias.
func InsertSQLiteCurationFixture(
	ctx context.Context, tx *sql.Tx, archiveID, generation string,
) (CurationFixture, error) {
	if err := InsertSQLiteCoreFixture(ctx, tx, archiveID, generation); err != nil {
		return CurationFixture{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO starred_sessions (session_id, created_at)
		VALUES (?, ?)`, rootOldID, "2026-08-03T13:00:00Z"); err != nil {
		return CurationFixture{}, fmt.Errorf("inserting SQLite curation star: %w", err)
	}
	var pinnedMessageID, writeMessageID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM messages WHERE session_id = ? AND ordinal = 1`,
		rootNewID,
	).Scan(&pinnedMessageID); err != nil {
		return CurationFixture{}, fmt.Errorf("selecting SQLite pinned message: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM messages WHERE session_id = ? AND ordinal = 2`,
		rootNewID,
	).Scan(&writeMessageID); err != nil {
		return CurationFixture{}, fmt.Errorf("selecting SQLite write message: %w", err)
	}
	note := seededPinNote
	result, err := tx.ExecContext(ctx, `
		INSERT INTO pinned_messages (
			session_id, message_id, ordinal, note, created_at
		) VALUES (?, ?, 1, ?, ?)`,
		rootNewID, pinnedMessageID, note, "2026-08-03T13:00:00Z",
	)
	if err != nil {
		return CurationFixture{}, fmt.Errorf("inserting SQLite curation pin: %w", err)
	}
	pinRowID, err := result.LastInsertId()
	if err != nil {
		return CurationFixture{}, fmt.Errorf("reading SQLite curation pin id: %w", err)
	}
	return CurationFixture{
		Core: CoreFixture(), PinnedMessageID: pinnedMessageID,
		InitialPinRowID: pinRowID, WriteMessageID: writeMessageID,
	}, nil
}
