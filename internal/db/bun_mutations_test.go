package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBunStoreSessionMutationsRejectBeforeBackendUpdate(t *testing.T) {
	backend := &recordingBunBackend{}
	store := NewBunStore(backend)
	name := "forbidden"

	require.ErrorIs(t, store.RenameSession("session", &name), ErrReadOnly)
	require.ErrorIs(t, store.SoftDeleteSession("session"), ErrReadOnly)
	count, err := store.SoftDeleteSessions([]string{"session"})
	assert.Zero(t, count)
	require.ErrorIs(t, err, ErrReadOnly)
	restored, err := store.RestoreSession("session")
	assert.Zero(t, restored)
	require.ErrorIs(t, err, ErrReadOnly)
	deleted, err := store.DeleteSessionIfTrashed("session")
	assert.Zero(t, deleted)
	require.ErrorIs(t, err, ErrReadOnly)
	emptied, err := store.EmptyTrash()
	assert.Zero(t, emptied)
	require.ErrorIs(t, err, ErrReadOnly)

	assert.Zero(t, backend.updateCalls)
}
