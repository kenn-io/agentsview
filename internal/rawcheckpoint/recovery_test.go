package rawcheckpoint

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestRecoverInvalidatesBrokenOfflineSuffixAndResetsAcknowledgedBase(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	firstRef := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, firstRef, []byte("abc"))
	firstReservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	first := testCapturedGeneration(1, root, "", firstRef)
	require.NoError(store.CommitCapture(t.Context(), firstReservation.ID, first))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2),
		Generation: 1, Created: true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", first.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", first.CaptureID, commit)
	require.NoError(err)

	secondRef := rawsync.ObjectRef{SHA256: validCheckpointDigest(12), Length: 3}
	thirdRef := rawsync.ObjectRef{SHA256: validCheckpointDigest(13), Length: 3}
	installOutboxTestObject(t, store, secondRef, []byte("def"))
	secondReservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	second := testCapturedGeneration(2, root, first.CaptureID, secondRef)
	require.NoError(store.CommitCapture(t.Context(), secondReservation.ID, second))
	installOutboxTestObject(t, store, thirdRef, []byte("ghi"))
	thirdReservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	third := testCapturedGeneration(3, root, second.CaptureID, thirdRef)
	require.NoError(store.CommitCapture(t.Context(), thirdReservation.ID, third))

	require.NoError(os.WriteFile(store.ObjectPath(secondRef), []byte("broken"), 0o600))
	temporary := filepath.Join(store.CaptureTempDir(), "stale.tmp")
	require.NoError(os.WriteFile(temporary, []byte("stale"), 0o600))
	orphan := filepath.Join(store.spoolDir, "objects", "sha256", "ff", validCheckpointDigest(15))
	require.NoError(os.MkdirAll(filepath.Dir(orphan), 0o700))
	require.NoError(os.WriteFile(orphan, []byte("orphan"), 0o600))
	_, err = store.ReserveCapture(t.Context(), root.ID, 128)
	require.NoError(err)

	report, err := store.Recover(t.Context())

	require.NoError(err)
	assert.Equal(1, report.TemporaryFiles)
	assert.Equal(1, report.UnreferencedFiles)
	assert.Equal(1, report.Reservations)
	assert.Equal(2, report.InvalidGenerations)
	assert.Equal(2, report.Garbage.Objects)
	assert.NoFileExists(temporary)
	assert.NoFileExists(orphan)
	assert.NoFileExists(store.ObjectPath(secondRef))
	assert.NoFileExists(store.ObjectPath(thirdRef))

	base, ok, err := store.CaptureBase(t.Context(), first.Source)
	require.NoError(err)
	require.True(ok)
	assert.Equal(first.CaptureID, base.CaptureID)
	assert.Equal(first.Entries, base.Entries)
	assert.Equal(commit.Receipt, base.Head.Receipt)
	_, ok, err = store.NextGeneration(t.Context())
	require.NoError(err)
	assert.False(ok)
	head, ok, err := store.SourceHead(
		t.Context(), first.Source.Provider, first.Source.ConfiguredRootID, first.Source.SourceKey,
	)
	require.NoError(err)
	require.True(ok)
	assert.Equal(commit.Receipt, head.Receipt)
	coverage, ok, err := store.Coverage(t.Context(), root.Provider, root.ID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(CoverageDegraded, coverage.State)
	assert.Equal("missing_object", coverage.Reason)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(err)
	assert.Zero(usage.UsedBytes)
	assert.Zero(usage.ReservedBytes)
	var generations int
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM outbox_generations`).Scan(
		&generations,
	))
	assert.Zero(generations)
}

func TestOpenRecoversInterruptedSourceReservationAsCoverageGap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := t.TempDir()
	checkpointPath := filepath.Join(base, "checkpoint.db")
	spoolDir := filepath.Join(base, "spool")
	sourceRoot := filepath.Join(base, "sources")
	require.NoError(os.MkdirAll(sourceRoot, 0o755))
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	options := Options{
		SpoolDir: spoolDir, MaxOutboxBytes: 1 << 20,
		Now: func() time.Time { return now },
	}
	store, err := OpenWithOptions(t.Context(), checkpointPath, options)
	require.NoError(err)
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, sourceRoot)
	require.NoError(err)
	source := SourceIdentity{
		Provider: root.Provider, ConfiguredRootID: root.ID, SourceKey: "source-a",
	}
	_, err = store.ReserveSourceCapture(t.Context(), source, 128)
	require.NoError(err)
	require.NoError(store.Close())

	reopened, err := OpenWithOptions(t.Context(), checkpointPath, options)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(reopened.Close()) })
	usage, err := reopened.OutboxUsage(t.Context())
	require.NoError(err)
	assert.Zero(usage.ReservedBytes)
	coverage, ok, err := reopened.Coverage(t.Context(), root.Provider, root.ID)
	require.NoError(err)
	require.True(ok)
	assert.Equal(CoverageDegraded, coverage.State)
	assert.Equal("capture_interrupted", coverage.Reason)
	var failureReason string
	require.NoError(reopened.db.QueryRowContext(t.Context(), `SELECT reason FROM raw_coverage_failures
		WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(source.Provider), source.ConfiguredRootID, source.SourceKey,
	).Scan(&failureReason))
	assert.Equal("capture_interrupted", failureReason)
}

func TestVerifyObjectPerformsExplicitFullDigestCheck(t *testing.T) {
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	require.NoError(store.VerifyObject(t.Context(), ref))
	require.NoError(os.WriteFile(store.ObjectPath(ref), []byte("xyz"), 0o600))

	err = store.VerifyObject(t.Context(), ref)

	require.ErrorIs(err, rawsync.ErrInvalid)
}

func TestRecoverWaitsForActiveObjectPublication(t *testing.T) {
	store, _ := openOutboxTestStore(t, 1<<20)
	finishPublication := store.BeginObjectPublication()
	result := make(chan error, 1)
	go func() {
		_, err := store.Recover(t.Context())
		result <- err
	}()

	select {
	case err := <-result:
		require.Failf(t, "recovery returned during publication", "error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	finishPublication()

	require.NoError(t, <-result)
}
