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

func (*recordingBunBackend) SessionQueryDialect() QueryDialect {
	return PortableBunSessionQueryDialect()
}

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
