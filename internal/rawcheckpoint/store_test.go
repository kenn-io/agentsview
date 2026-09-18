package rawcheckpoint

import (
	"errors"
	"fmt"
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

func testCommitResult(sequence int, generation int64) rawsync.CommitResult {
	return rawsync.CommitResult{
		ManifestID: fmt.Sprintf("%064x", sequence*2),
		Receipt:    fmt.Sprintf("%064x", sequence*2+1),
		Generation: generation,
	}
}

func TestStoreDeviceRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	_, ok, err := store.Device(t.Context())
	require.NoError(err)
	assert.False(ok)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	device, ok, err := store.Device(t.Context())
	require.NoError(err)
	assert.True(ok)
	assert.Equal("dev_1", device)
}

func TestStoreAdvanceHeadRequiresReceipt(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	commit := testCommitResult(1, 1)
	commit.Receipt = ""
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1",
		"src-1", "", commit)
	require.ErrorIs(t, err, ErrMissingReceipt)
}

func TestStoreAdvanceHeadRejectsInvalidCommitResult(t *testing.T) {
	t.Parallel()
	valid := testCommitResult(1, 1)
	tests := []struct {
		name   string
		commit rawsync.CommitResult
	}{
		{
			name: "noncanonical manifest ID",
			commit: rawsync.CommitResult{
				ManifestID: "rm_1", Receipt: valid.Receipt, Generation: 1,
			},
		},
		{
			name: "uppercase receipt",
			commit: rawsync.CommitResult{
				ManifestID: valid.ManifestID,
				Receipt:    "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
				Generation: 1,
			},
		},
		{
			name: "zero generation",
			commit: rawsync.CommitResult{
				ManifestID: valid.ManifestID, Receipt: valid.Receipt, Generation: 0,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)

			t.Parallel()
			store := openTestStore(t)
			require.NoError(store.SetDevice(t.Context(), "dev_1"))

			err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
				"root-1", "src-1", "", tt.commit)
			require.ErrorIs(err, rawsync.ErrInvalid)
			_, ok, readErr := store.SourceHead(t.Context(), parser.AgentClaude,
				"root-1", "src-1")
			require.NoError(readErr)
			assert.False(t, ok, "invalid commit result must not persist a source head")
		})
	}
}

func TestStoreAdvanceHeadRequiresFirstGeneration(t *testing.T) {
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))

	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
		"root-1", "src-1", "", testCommitResult(1, 2))
	require.ErrorIs(err, ErrHeadConflict)
	_, ok, readErr := store.SourceHead(t.Context(), parser.AgentClaude,
		"root-1", "src-1")
	require.NoError(readErr)
	assert.False(t, ok, "a source chain may not begin after generation one")
}

func TestStoreAdvanceHeadRejectsGenerationGap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
		"root-1", "src-1", "", first))

	gap := testCommitResult(3, 3)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
		"root-1", "src-1", first.Receipt, gap)
	require.ErrorIs(err, ErrHeadConflict)
	head, ok, readErr := store.SourceHead(t.Context(), parser.AgentClaude,
		"root-1", "src-1")
	require.NoError(readErr)
	require.True(ok)
	assert.Equal(first.ManifestID, head.ManifestID)
	assert.Equal(first.Receipt, head.Receipt)
	assert.Equal(first.Generation, head.Generation)
}

func TestStoreHeadLifecycle(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	_, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	assert.False(ok)

	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(first.Receipt, head.Receipt)
	assert.Equal(int64(1), head.Generation)
	assert.Equal(first.ManifestID, head.ManifestID)

	second := testCommitResult(2, 2)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", first.Receipt, second))
	head, ok, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(second.Receipt, head.Receipt)
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	require := require.New(t)

	t.Parallel()
	path := t.TempDir() + "/rawcheckpoint.db"
	store, err := Open(t.Context(), path)
	require.NoError(err)
	require.NoError(store.SetDevice(t.Context(), "dev_persist"))
	commit := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_persist", parser.AgentClaude, "r", "s", "", commit))
	require.NoError(store.Close())

	reopened, err := Open(t.Context(), path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	head, ok, err := reopened.SourceHead(t.Context(), parser.AgentClaude, "r", "s")
	require.NoError(err)
	require.True(ok)
	assert.Equal(t, commit.Receipt, head.Receipt)
}

func TestStoreAdvanceHeadRejectsStaleParent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// A delayed second-generation result carrying the stale parent receipt
	// must never overwrite the newer acknowledged head.
	stale := testCommitResult(2, 2)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", stale)
	require.ErrorIs(err, ErrHeadConflict)

	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(first.Receipt, head.Receipt)
	assert.Equal(first.ManifestID, head.ManifestID)
	assert.Equal(int64(1), head.Generation)
}

func TestStoreAdvanceHeadIdempotentReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// A retried manifest carries the same parent it used for the original
	// commit. Recognize the identical committed result without requiring the
	// now-current receipt as a fabricated parent.
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(first.Receipt, head.Receipt)
	assert.Equal(first.ManifestID, head.ManifestID)
	assert.Equal(int64(1), head.Generation)
}

func TestStoreAdvanceHeadRejectsLowerGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
		"root-1", "src-1", "", first))
	current := testCommitResult(2, 2)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
		"root-1", "src-1", first.Receipt, current))

	older := testCommitResult(3, 1)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
		"root-1", "src-1", current.Receipt, older)
	require.ErrorIs(err, ErrHeadConflict)
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(current.ManifestID, head.ManifestID)
	assert.Equal(current.Receipt, head.Receipt)
	assert.Equal(current.Generation, head.Generation)
}

func TestStoreConcurrentAdvanceHeadReturnsCASResults(t *testing.T) {
	assert := assert.New(t)

	store := openTestStore(t)
	require.NoError(t, store.SetDevice(t.Context(), "dev_1"))

	const writers = 16
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := range writers {
		commit := testCommitResult(i, 1)
		go func() {
			<-start
			results <- store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude,
				"root-1", "src-1", "", commit)
		}()
	}
	close(start)

	var successes, conflicts int
	for range writers {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrHeadConflict):
			conflicts++
		default:
			assert.NoError(err, "concurrent writers must return a CAS result")
		}
	}
	assert.Equal(1, successes)
	assert.Equal(writers-1, conflicts)
}

func TestStoreSetDeviceChangeClearsHeads(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// Re-provisioning under a different device identity clears per-source
	// heads: the server chains heads per device, so the new device starts
	// from an empty chain.
	require.NoError(store.SetDevice(t.Context(), "dev_2"))
	device, ok, err := store.Device(t.Context())
	require.NoError(err)
	require.True(ok)
	assert.Equal("dev_2", device)
	_, ok, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	assert.False(ok)

	// The new chain advances from empty, and recording the same device
	// again is a no-op that keeps both the head and the provisioning time.
	fresh := testCommitResult(2, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_2", parser.AgentClaude, "root-1", "src-1", "", fresh))
	var createdAt string
	require.NoError(store.db.QueryRowContext(t.Context(),
		`SELECT created_at FROM device_config WHERE id = 1`).Scan(&createdAt))
	require.NoError(store.SetDevice(t.Context(), "dev_2"))
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(fresh.Receipt, head.Receipt)
	var createdAtAfter string
	require.NoError(store.db.QueryRowContext(t.Context(),
		`SELECT created_at FROM device_config WHERE id = 1`).Scan(&createdAtAfter))
	assert.Equal(createdAt, createdAtAfter)
}

func TestStoreEnsureDeviceRejectsReprovisioningWithoutMutatingState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(
		t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first,
	))

	err := store.EnsureDevice(t.Context(), "dev_2")

	require.ErrorIs(err, ErrDeviceMismatch)
	device, ok, readErr := store.Device(t.Context())
	require.NoError(readErr)
	require.True(ok)
	assert.Equal("dev_1", device)
	head, ok, readErr := store.SourceHead(
		t.Context(), parser.AgentClaude, "root-1", "src-1",
	)
	require.NoError(readErr)
	require.True(ok)
	assert.Equal(first.Receipt, head.Receipt)
}

// TestStoreAdvanceHeadRequiresConfiguredDevice: advancement before any
// device identity is recorded is refused, not defaulted.
func TestStoreAdvanceHeadRequiresConfiguredDevice(t *testing.T) {
	t.Parallel()
	store := openTestStore(t)
	commit := testCommitResult(1, 1)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "",
		commit)
	require.ErrorIs(t, err, ErrDeviceNotConfigured)
}

// TestStoreAdvanceHeadRejectsForeignDevice: only the configured device may
// advance heads — including after re-provisioning cleared them, when a
// stale in-flight ack from the previous device must not repopulate heads.
func TestStoreAdvanceHeadRejectsForeignDevice(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	first := testCommitResult(1, 1)
	require.NoError(store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first))

	// A different device may not advance dev_1's chain even with the
	// correct expected parent receipt.
	second := testCommitResult(2, 2)
	err := store.AdvanceHead(t.Context(), "dev_2", parser.AgentClaude, "root-1", "src-1", first.Receipt, second)
	require.ErrorIs(err, ErrDeviceMismatch)
	head, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	require.True(ok)
	assert.Equal(first.Receipt, head.Receipt)

	// Re-provisioning clears heads; a delayed first-generation ack from
	// dev_1 (empty expected parent) must not repopulate dev_2's chain.
	require.NoError(store.SetDevice(t.Context(), "dev_2"))
	err = store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1", "", first)
	require.ErrorIs(err, ErrDeviceMismatch)
	_, ok, err = store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	assert.False(ok)
}

// TestStoreAdvanceHeadAbsentSourceRequiresEmptyParent: the absent-row insert
// is gated on an empty expected parent receipt, exactly like the
// conflict-update path.
func TestStoreAdvanceHeadAbsentSourceRequiresEmptyParent(t *testing.T) {
	require := require.New(t)

	t.Parallel()
	store := openTestStore(t)
	require.NoError(store.SetDevice(t.Context(), "dev_1"))
	commit := testCommitResult(1, 1)
	err := store.AdvanceHead(t.Context(), "dev_1", parser.AgentClaude, "root-1", "src-1",
		testCommitResult(2, 1).Receipt, commit)
	require.ErrorIs(err, ErrHeadConflict)
	_, ok, err := store.SourceHead(t.Context(), parser.AgentClaude, "root-1", "src-1")
	require.NoError(err)
	assert.False(t, ok, "no row may be created for a nonempty expected parent")
}
