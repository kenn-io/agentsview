package rawcheckpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.Context(), t.TempDir()+"/rawcheckpoint.db")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestStoreDeviceRoundTrip(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	_, ok, err := store.Device(t.Context())
	require.NoError(t, err)
	assert.False(t, ok)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	device, ok, err := store.Device(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "dev_1", device)
}

func TestStoreAdvanceHeadRequiresReceipt(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1",
		"src-1", "", rawsync.CommitResult{ManifestID: "rm_1"})
	require.ErrorIs(t, err, ErrMissingReceipt)
}

func TestStoreHeadLifecycle(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	_, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	assert.False(t, ok)

	first := rawsync.CommitResult{ManifestID: "rm_1", Receipt: "rr_1", Generation: 4}
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr_1", head.Receipt)
	assert.Equal(t, int64(4), head.Generation)
	assert.Equal(t, "rm_1", head.ManifestID)

	second := rawsync.CommitResult{ManifestID: "rm_2", Receipt: "rr_2", Generation: 5}
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "rr_1", second))
	head, ok, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr_2", head.Receipt)
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/rawcheckpoint.db"
	store, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "dev_persist"))
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_persist", parser.AgentClaude, "r", "s", "",
		rawsync.CommitResult{ManifestID: "rm", Receipt: "rr", Generation: 1}))
	require.NoError(t, store.Close())

	reopened, err := Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	head, ok, err := reopened.SourceHead(t.Context(), parser.AgentClaude, "r", "s")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr", head.Receipt)
}

func TestStoreAdvanceHeadRejectsStaleParent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	first := rawsync.CommitResult{ManifestID: "rm_1", Receipt: "rr_1", Generation: 4}
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// A delayed second-generation result carrying the stale parent receipt
	// must never overwrite the newer acknowledged head.
	stale := rawsync.CommitResult{ManifestID: "rm_2", Receipt: "rr_2", Generation: 5}
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", stale)
	require.ErrorIs(t, err, ErrHeadConflict)

	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr_1", head.Receipt)
	assert.Equal(t, "rm_1", head.ManifestID)
	assert.Equal(t, int64(4), head.Generation)
}

func TestStoreAdvanceHeadIdempotentReplay(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	first := rawsync.CommitResult{ManifestID: "rm_1", Receipt: "rr_1", Generation: 4}
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// Re-acknowledging the stored head with its own receipt passes the
	// compare-and-swap and rewrites identical values.
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "rr_1", first))
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr_1", head.Receipt)
	assert.Equal(t, "rm_1", head.ManifestID)
	assert.Equal(t, int64(4), head.Generation)
}

func TestStoreSetDeviceChangeClearsHeads(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "",
		rawsync.CommitResult{ManifestID: "rm_1", Receipt: "rr_1", Generation: 1}))

	// Re-provisioning under a different device identity clears per-source
	// heads: the server chains heads per device, so the new device starts
	// from an empty chain.
	require.NoError(t, store.SetDevice(t.Context(), "dev_2"))
	device, ok, err := store.Device(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "dev_2", device)
	_, ok, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	assert.False(t, ok)

	// The new chain advances from empty, and recording the same device
	// again is a no-op that keeps both the head and the provisioning time.
	fresh := rawsync.CommitResult{ManifestID: "rm_2", Receipt: "rr_2", Generation: 1}
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_2", parser.AgentClaude, "root-1", "src-1", "", fresh))
	var createdAt string
	require.NoError(t, store.db.QueryRowContext(t.Context(),
		`SELECT created_at FROM device_config WHERE id = 1`).Scan(&createdAt))
	require.NoError(t, store.SetDevice(t.Context(), "dev_2"))
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr_2", head.Receipt)
	var createdAtAfter string
	require.NoError(t, store.db.QueryRowContext(t.Context(),
		`SELECT created_at FROM device_config WHERE id = 1`).Scan(&createdAtAfter))
	assert.Equal(t, createdAt, createdAtAfter)
}

// TestStoreAdvanceHeadRequiresConfiguredDevice: advancement before any
// device identity is recorded is refused, not defaulted.
func TestStoreAdvanceHeadRequiresConfiguredDevice(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "",
		rawsync.CommitResult{ManifestID: "rm_1", Receipt: "rr_1", Generation: 1})
	require.ErrorIs(t, err, ErrDeviceNotConfigured)
}

// TestStoreAdvanceHeadRejectsForeignDevice: only the configured device may
// advance heads — including after re-provisioning cleared them, when a
// stale in-flight ack from the previous device must not repopulate heads.
func TestStoreAdvanceHeadRejectsForeignDevice(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	first := rawsync.CommitResult{ManifestID: "rm_1", Receipt: "rr_1", Generation: 1}
	require.NoError(t, store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// A different device may not advance dev_1's chain even with the
	// correct expected parent receipt.
	second := rawsync.CommitResult{ManifestID: "rm_2", Receipt: "rr_2", Generation: 2}
	err := store.AdvanceHead(t.Context(), "dev_2", parser.AgentClaude, "root-1", "src-1", "rr_1", second)
	require.ErrorIs(t, err, ErrDeviceMismatch)
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "rr_1", head.Receipt)

	// Re-provisioning clears heads; a delayed first-generation ack from
	// dev_1 (empty expected parent) must not repopulate dev_2's chain.
	require.NoError(t, store.SetDevice(t.Context(), "dev_2"))
	err = store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first)
	require.ErrorIs(t, err, ErrDeviceMismatch)
	_, ok, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	assert.False(t, ok)
}

// TestStoreAdvanceHeadAbsentSourceRequiresEmptyParent: the absent-row insert
// is gated on an empty expected parent receipt, exactly like the
// conflict-update path.
func TestStoreAdvanceHeadAbsentSourceRequiresEmptyParent(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))
	commit := rawsync.CommitResult{ManifestID: "rm_ghost", Receipt: "rr_ghost_child", Generation: 1}
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "rr_ghost", commit)
	require.ErrorIs(t, err, ErrHeadConflict)
	_, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(t, err)
	assert.False(t, ok, "no row may be created for a nonempty expected parent")
}
