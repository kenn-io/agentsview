package db

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

type recordingBunBackend struct {
	readOnly            bool
	capabilities        BackendCapabilities
	insideGuard         bool
	viewCalls           int
	consistentViewCalls int
	updateCalls         int
}

func (*recordingBunBackend) Name() string { return "recording" }

func (b *recordingBunBackend) ReadOnly() bool { return b.readOnly }

func (b *recordingBunBackend) Capabilities() BackendCapabilities {
	return b.capabilities
}

func (*recordingBunBackend) TimestampOrderExpr(column string) string { return column }

func (*recordingBunBackend) SessionVersion(
	context.Context, bun.IDB, string,
) (int, int64, error) {
	return 0, 0, sql.ErrNoRows
}

func (b *recordingBunBackend) View(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.viewCalls++
	b.insideGuard = true
	defer func() { b.insideGuard = false }()
	return fn(nil)
}

func (b *recordingBunBackend) ConsistentView(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.consistentViewCalls++
	b.insideGuard = true
	defer func() { b.insideGuard = false }()
	return fn(nil)
}

func (b *recordingBunBackend) Update(
	_ context.Context, fn func(bun.IDB) error,
) error {
	b.updateCalls++
	b.insideGuard = true
	defer func() { b.insideGuard = false }()
	return fn(nil)
}

func TestBunStoreViewRunsCallbackInsideBackendGuard(t *testing.T) {
	backend := &recordingBunBackend{}
	store := NewBunStore(backend)

	err := store.view(t.Context(), func(bun.IDB) error {
		assert.True(t, backend.insideGuard)
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 1, backend.viewCalls)
	assert.False(t, backend.insideGuard)
}

func TestBunStoreUpdateUsesOperationCapability(t *testing.T) {
	backend := &recordingBunBackend{
		readOnly: true,
		capabilities: BackendCapabilities{
			Writes: map[WriteOperation]bool{WriteCuration: true},
		},
	}
	store := NewBunStore(backend)

	err := store.update(t.Context(), WriteArchive, func(bun.IDB) error {
		require.Fail(t, "unauthorized archive callback ran")
		return nil
	})
	assert.ErrorIs(t, err, ErrReadOnly)
	assert.Equal(t, 0, backend.updateCalls)

	err = store.update(t.Context(), WriteCuration, func(bun.IDB) error {
		assert.True(t, backend.insideGuard)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, backend.updateCalls)
}

func TestBunStoreOwnsReadOnlyArchiveWriteDefaults(t *testing.T) {
	backend := &recordingBunBackend{readOnly: true}
	store := NewBunStore(backend)

	assert.True(t, store.ReadOnly())
	assert.ErrorIs(t, store.UpsertSession(Session{}), ErrReadOnly)
	assert.ErrorIs(t, store.ReplaceSessionMessages("session", nil), ErrReadOnly)
	result, err := store.WriteSessionBatchAtomic(nil)
	assert.Equal(t, SessionBatchResult{}, result)
	assert.ErrorIs(t, err, ErrReadOnly)
	result, err = store.WriteSessionAtomic(SessionBatchWrite{})
	assert.Equal(t, SessionBatchResult{}, result)
	assert.ErrorIs(t, err, ErrReadOnly)
	assert.Zero(t, backend.updateCalls)
}

func TestBunStoreUpsertSessionUsesWritableArchiveCapability(t *testing.T) {
	database := testDB(t)
	store := database.BunStore

	require.NoError(t, store.UpsertSession(Session{
		ID: "bun-store-upload", Agent: "codex", Project: "alpha",
	}))
	session, err := store.GetSessionFull(t.Context(), "bun-store-upload")
	require.NoError(t, err)
	assert.Equal(t, "alpha", session.Project)
	assert.NotEmpty(t, session.SourceArchiveID)
	assert.NotEmpty(t, session.SourceDatabaseGeneration)
}

func TestBunStoreReplaceSessionMessagesUsesWritableArchiveCapability(t *testing.T) {
	database := testDB(t)
	store := database.BunStore
	_, err := database.upsertSession(Session{
		ID: "bun-store-messages", Agent: "codex",
	})
	require.NoError(t, err)

	require.NoError(t, store.ReplaceSessionMessages(
		"bun-store-messages", []Message{{
			SessionID: "bun-store-messages", Ordinal: 0,
			Role: "user", Content: "shared upload", ContentLength: 13,
		}},
	))
	messages, err := store.GetAllMessages(
		t.Context(), "bun-store-messages",
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "shared upload", messages[0].Content)
}

func TestBunStoreWriteSessionBatchAtomicUsesWritableArchiveCapability(t *testing.T) {
	database := testDB(t)
	store := database.BunStore

	result, err := store.WriteSessionBatchAtomic([]SessionBatchWrite{{
		Session: Session{ID: "bun-store-batch", Agent: "codex"},
	}})
	require.NoError(t, err)
	assert.Equal(t, 1, result.WrittenSessions)
	session, err := store.GetSessionFull(t.Context(), "bun-store-batch")
	require.NoError(t, err)
	assert.Equal(t, "bun-store-batch", session.ID)
}

func TestBunStoreWriteSessionAtomicUsesOneBatchTransaction(t *testing.T) {
	database := testDB(t)
	store := database.BunStore

	result, err := store.WriteSessionAtomic(
		SessionBatchWrite{
			Session: Session{ID: "bun-store-single", Agent: "codex"},
			Messages: []Message{{
				SessionID: "bun-store-single", Ordinal: 0,
				Role: "user", Content: "single atomic write",
			}},
		},
		func() error { return assert.AnError },
	)
	require.ErrorIs(t, err, assert.AnError)
	assert.Zero(t, result.WrittenSessions)
	session, readErr := store.GetSessionFull(t.Context(), "bun-store-single")
	require.NoError(t, readErr)
	assert.Nil(t, session, "late failure must roll back the complete session")
}

func TestGuardedSQLFacadesExecuteThroughBun(t *testing.T) {
	database := testDB(t)

	readerHook := new(countingQueryHook)
	database.bunReader = database.bunReader.WithQueryHook(readerHook)
	var value int
	require.NoError(t, database.getReader().QueryRow("SELECT 1").Scan(&value))
	assert.Equal(t, 1, value)
	assert.Len(t, readerHook.queries, 1)

	writerHook := new(countingQueryHook)
	database.bunWriter = database.bunWriter.WithQueryHook(writerHook)
	value = 0
	require.NoError(t, database.getWriter().QueryRow("SELECT 1").Scan(&value))
	assert.Equal(t, 1, value)
	assert.Len(t, writerHook.queries, 1)
}
