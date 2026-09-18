package rawcheckpoint

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/rawsync"
)

func TestFinalizeAndAcknowledgeOfflineChainInOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	refs := []rawsync.ObjectRef{
		{SHA256: validCheckpointDigest(10), Length: 1},
		{SHA256: validCheckpointDigest(11), Length: 1},
		{SHA256: validCheckpointDigest(12), Length: 1},
	}
	predecessor := ""
	var generations []CapturedGeneration
	for i, ref := range refs {
		installOutboxTestObject(t, store, ref, []byte{byte(i)})
		reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
		require.NoError(err)
		generation := testCapturedGeneration(i+1, root, predecessor, ref)
		require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
		generations = append(generations, generation)
		predecessor = generation.CaptureID
	}

	first, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(generations[0].CaptureID, first.CaptureID)
	assert.Empty(first.ExpectedParentReceipt)
	assert.Equal(generations[0].Entries[0].Objects, first.Entries[0].Objects)

	firstCommit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1),
		Receipt:    validCheckpointDigest(2),
		Generation: 1,
		Created:    true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generations[0].CaptureID, firstCommit,
	))
	ack, err := store.AcknowledgeGeneration(
		t.Context(), "device-a", generations[0].CaptureID, firstCommit,
	)
	require.NoError(err)
	assert.False(ack.Replayed)
	assert.Equal(1, ack.Garbage.Objects)
	assert.NoFileExists(store.ObjectPath(refs[0]))

	second, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(generations[1].CaptureID, second.CaptureID)
	assert.Equal(firstCommit.Receipt, second.ExpectedParentReceipt)
	secondCommit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(3),
		Receipt:    validCheckpointDigest(4),
		Generation: 2,
		Created:    true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generations[1].CaptureID, secondCommit,
	))
	_, err = store.AcknowledgeGeneration(
		t.Context(), "device-a", generations[1].CaptureID, secondCommit,
	)
	require.NoError(err)
	third, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(generations[2].CaptureID, third.CaptureID)
	assert.Equal(secondCommit.Receipt, third.ExpectedParentReceipt)

	replayed, err := store.AcknowledgeGeneration(
		t.Context(), "device-a", generations[1].CaptureID, secondCommit,
	)
	require.NoError(err)
	assert.True(replayed.Replayed)
}

func TestFinalizeNextManifestCarriesEntryModTime(t *testing.T) {
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(21), Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	generation := testCapturedGeneration(4, root, "", ref)
	require.Equal(int64(4), generation.Entries[0].ModTimeNS)
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))

	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")

	require.NoError(err)
	require.True(found)
	require.Len(manifest.Entries, 1)
	assert.Equal(t, int64(4), manifest.Entries[0].ModTimeNS,
		"finalized manifests must retain each captured file's mod time")
}

func TestFinalizeNextManifestOrdersExactSecondBeforeLaterFraction(t *testing.T) {
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	baseTime := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for sequence, capturedAt := range []time.Time{
		baseTime,
		baseTime.Add(100 * time.Millisecond),
	} {
		ref := rawsync.ObjectRef{
			SHA256: validCheckpointDigest(byte(sequence + 10)), Length: 1,
		}
		installOutboxTestObject(t, store, ref, []byte{byte(sequence)})
		generation := testCapturedGeneration(sequence+1, root, "", ref)
		generation.Source.SourceKey = fmt.Sprintf("source-%d", sequence)
		generation.CapturedAt = capturedAt
		reservation, err := store.ReserveSourceCapture(
			t.Context(), generation.Source, 1793,
		)
		require.NoError(err)
		require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	}

	manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")

	require.NoError(err)
	require.True(found)
	assert.Equal(t, fmt.Sprintf("%032x", 1), manifest.CaptureID)
}

func TestBindFinalizedCommitPersistsResultForRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1),
		Receipt:    validCheckpointDigest(2),
		Generation: 1,
		Created:    true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))
	stored, found, err := store.FinalizedCommit(t.Context(), "device-a", generation.CaptureID)

	require.NoError(err)
	require.True(found)
	assert.Equal(commit.ManifestID, stored.ManifestID)
	assert.Equal(commit.Receipt, stored.Receipt)
	assert.Equal(commit.Generation, stored.Generation)
}

func TestQueueTombstoneWaitsForSnapshotAndIsIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	snapshot := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, snapshot))

	tombstoneID, queued, err := store.QueueTombstone(t.Context(), snapshot.Source)
	require.NoError(err)
	require.True(queued)
	assert.NotEmpty(tombstoneID)
	_, queued, err = store.QueueTombstone(t.Context(), snapshot.Source)
	require.NoError(err)
	assert.False(queued)

	first, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(snapshot.CaptureID, first.CaptureID)
	retry, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(snapshot.CaptureID, retry.CaptureID,
		"tombstone must wait for the snapshot receipt")

	firstCommit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1),
		Receipt:    validCheckpointDigest(2),
		Generation: 1,
		Created:    true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", snapshot.CaptureID, firstCommit,
	))
	_, err = store.AcknowledgeGeneration(
		t.Context(), "device-a", snapshot.CaptureID, firstCommit,
	)
	require.NoError(err)

	deleted, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(tombstoneID, deleted.CaptureID)
	assert.Equal(rawsync.ManifestTombstone, deleted.Kind)
	assert.Equal(firstCommit.Receipt, deleted.ExpectedParentReceipt)
	assert.Empty(deleted.Entries)

	base, ok, err := store.CaptureBase(t.Context(), snapshot.Source)
	require.NoError(err)
	require.True(ok)
	assert.Equal(tombstoneID, base.CaptureID)
	assert.Equal(rawsync.ManifestTombstone, base.Kind)
	assert.Empty(base.Entries)
}

func TestQueueTombstoneReplacesPermanentlyRejectedTombstone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	snapshot := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, snapshot))
	manifest, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	snapshotCommit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1),
		Receipt:    validCheckpointDigest(2),
		Generation: 1,
		Created:    true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", manifest.CaptureID, snapshotCommit,
	))
	_, err = store.AcknowledgeGeneration(
		t.Context(), "device-a", manifest.CaptureID, snapshotCommit,
	)
	require.NoError(err)
	rejectedID, queued, err := store.QueueTombstone(t.Context(), snapshot.Source)
	require.NoError(err)
	require.True(queued)
	rejected, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(rejectedID, rejected.CaptureID)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", rejectedID,
		GenerationFailurePermanent, time.Time{},
	))
	status, err := store.ClientStatus(t.Context())
	require.NoError(err)
	require.Equal(1, status.PermanentFailures)

	replacementID, queued, err := store.QueueTombstone(t.Context(), snapshot.Source)

	require.NoError(err)
	require.True(queued)
	assert.NotEqual(rejectedID, replacementID)
	replacement, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(replacementID, replacement.CaptureID)
	assert.Equal(rawsync.ManifestTombstone, replacement.Kind)
	assert.Equal(snapshotCommit.Receipt, replacement.ExpectedParentReceipt)
	var rejectedRows int
	require.NoError(store.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM outbox_generations WHERE capture_id = ?`, rejectedID,
	).Scan(&rejectedRows))
	assert.Zero(rejectedRows)
	status, err = store.ClientStatus(t.Context())
	require.NoError(err)
	assert.Zero(status.PermanentFailures)
	_, queued, err = store.QueueTombstone(t.Context(), snapshot.Source)
	require.NoError(err)
	assert.False(queued, "the healthy replacement remains idempotent")
}

func TestAcknowledgeGenerationRejectsStaleInputsWithoutAdvancing(t *testing.T) {
	parentRequire := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	parentRequire.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	parentRequire.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	parentRequire.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	parentRequire.NoError(err)
	parentRequire.True(ok)
	valid := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2),
		Generation: 1, Created: true,
	}
	parentRequire.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, valid,
	))
	parentRequire.ErrorIs(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, rawsync.CommitResult{
			ManifestID: validCheckpointDigest(9), Receipt: valid.Receipt,
			Generation: valid.Generation,
		},
	), ErrAcknowledgeConflict)

	for _, tc := range []struct {
		name      string
		deviceID  string
		captureID string
		commit    rawsync.CommitResult
	}{
		{name: "foreign device", deviceID: "device-b", captureID: generation.CaptureID, commit: valid},
		{name: "foreign capture", deviceID: "device-a", captureID: "missing", commit: valid},
		{
			name: "wrong generation", deviceID: "device-a", captureID: generation.CaptureID,
			commit: rawsync.CommitResult{ManifestID: valid.ManifestID, Receipt: valid.Receipt, Generation: 2},
		},
		{
			name: "wrong manifest", deviceID: "device-a", captureID: generation.CaptureID,
			commit: rawsync.CommitResult{ManifestID: validCheckpointDigest(9), Receipt: valid.Receipt, Generation: 1},
		},
		{
			name: "invalid receipt", deviceID: "device-a", captureID: generation.CaptureID,
			commit: rawsync.CommitResult{ManifestID: valid.ManifestID, Receipt: "bad", Generation: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)

			_, err := store.AcknowledgeGeneration(
				t.Context(), tc.deviceID, tc.captureID, tc.commit,
			)
			require.Error(err)
			head, ok, readErr := store.SourceHead(
				t.Context(), root.Provider, root.ID, generation.Source.SourceKey,
			)
			require.NoError(readErr)
			require.True(ok)
			assert.Zero(t, head.Generation)
		})
	}
}

func TestAcknowledgedCaptureBaseRetainsObjectReferencesAfterLocalGC(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2),
		Generation: 1, Created: true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", generation.CaptureID, commit)
	require.NoError(err)

	base, ok, err := store.CaptureBase(t.Context(), generation.Source)

	require.NoError(err)
	require.True(ok)
	assert.Equal(generation.CaptureID, base.CaptureID)
	assert.Equal(commit.Receipt, base.Head.Receipt)
	assert.Equal(generation.Entries, base.Entries)
	assert.NoFileExists(store.ObjectPath(ref))
}

func TestCaptureBaseSnapshotRemainsCoherentDuringAcknowledgement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2),
		Generation: 1, Created: true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))

	tx, err := store.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(err)
	t.Cleanup(func() { _ = tx.Rollback() })
	var captureID string
	require.NoError(tx.QueryRowContext(t.Context(), `SELECT latest_capture_id
		FROM raw_sources WHERE provider = ? AND configured_root_id = ? AND source_key = ?`,
		string(generation.Source.Provider), generation.Source.ConfiguredRootID,
		generation.Source.SourceKey,
	).Scan(&captureID))
	require.Equal(generation.CaptureID, captureID)

	_, err = store.AcknowledgeGeneration(
		t.Context(), "device-a", generation.CaptureID, commit,
	)
	require.NoError(err)
	base, ok, err := captureBaseSnapshot(t.Context(), tx, generation.Source)

	require.NoError(err)
	require.True(ok)
	assert.Equal(generation.CaptureID, base.CaptureID)
	assert.Empty(base.Head.Receipt)
	assert.Zero(base.Head.Generation)
	assert.Equal(generation.Entries, base.Entries)
}

func TestAcknowledgedGenerationCompactsToBoundedHeadReplayState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2),
		Generation: 1, Created: true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", generation.CaptureID, commit)
	require.NoError(err)

	usage, err := store.OutboxUsage(t.Context())

	require.NoError(err)
	assert.Zero(usage.UsedBytes)
	var generations int
	require.NoError(store.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM outbox_generations`,
	).Scan(&generations))
	assert.Zero(generations)
	base, ok, err := store.CaptureBase(t.Context(), generation.Source)
	require.NoError(err)
	require.True(ok)
	assert.Equal(generation.CaptureID, base.CaptureID)
	replayed, err := store.AcknowledgeGeneration(
		t.Context(), "device-a", generation.CaptureID, commit,
	)
	require.NoError(err)
	assert.True(replayed.Replayed)
}

func TestAcknowledgementReplayFinishesPendingGarbageCollection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
	installOutboxTestObject(t, store, ref, []byte("abc"))
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1795)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	commit := rawsync.CommitResult{
		ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2),
		Generation: 1, Created: true,
	}
	require.NoError(store.BindFinalizedCommit(
		t.Context(), "device-a", generation.CaptureID, commit,
	))
	objectPath := store.ObjectPath(ref)
	require.NoError(os.Remove(objectPath))
	require.NoError(os.MkdirAll(objectPath, 0o700))
	require.NoError(os.WriteFile(filepath.Join(objectPath, "busy"), []byte("x"), 0o600))

	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", generation.CaptureID, commit)
	require.Error(err)
	usage, readErr := store.OutboxUsage(t.Context())
	require.NoError(readErr)
	assert.Equal(ref.Length, usage.UsedBytes)
	require.NoError(os.RemoveAll(objectPath))

	replayed, err := store.AcknowledgeGeneration(
		t.Context(), "device-a", generation.CaptureID, commit,
	)

	require.NoError(err)
	assert.True(replayed.Replayed)
	assert.Equal(1, replayed.Garbage.Objects)
	usage, err = store.OutboxUsage(t.Context())
	require.NoError(err)
	assert.Zero(usage.UsedBytes)
}

func TestParentConflictBlocksOnlyItsSourceChain(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	firstRef := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	secondRef := rawsync.ObjectRef{SHA256: validCheckpointDigest(11), Length: 1}
	installOutboxTestObject(t, store, firstRef, []byte{1})
	installOutboxTestObject(t, store, secondRef, []byte{2})
	firstReservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	first := testCapturedGeneration(1, root, "", firstRef)
	require.NoError(store.CommitCapture(t.Context(), firstReservation.ID, first))
	secondReservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	second := testCapturedGeneration(2, root, "", secondRef)
	second.Source.SourceKey = "source-2"
	require.NoError(store.CommitCapture(t.Context(), secondReservation.ID, second))
	manifest, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	require.Equal(first.CaptureID, manifest.CaptureID)

	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", first.CaptureID,
		GenerationFailureParentReceiptConflict, time.Time{},
	))
	next, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")

	require.NoError(err)
	require.True(ok)
	assert.Equal(second.CaptureID, next.CaptureID)
	var errorClass string
	var blocked int
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT error_class, blocked
		FROM outbox_generations WHERE capture_id = ?`, first.CaptureID,
	).Scan(&errorClass, &blocked))
	assert.Equal(string(GenerationFailureParentReceiptConflict), errorClass)
	assert.Equal(1, blocked)
}

func TestPermanentReplacementRecyclesCapacityAndCollectsRejectedObjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1793)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	rejectedRef := rawsync.ObjectRef{SHA256: validCheckpointDigest(20), Length: 1}
	installOutboxTestObject(t, store, rejectedRef, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	rejected := testCapturedGeneration(1, root, "", rejectedRef)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, rejected))
	_, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(found)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", rejected.CaptureID,
		GenerationFailurePermanent, time.Time{},
	))

	replacementRef := rawsync.ObjectRef{SHA256: validCheckpointDigest(21), Length: 1}
	installOutboxTestObject(t, store, replacementRef, []byte{2})
	replacementReservation, err := store.ReserveSourceCapture(
		t.Context(), rejected.Source, 1793,
	)
	require.NoError(err)
	_, err = store.ReserveSourceCapture(t.Context(), rejected.Source, 1)
	require.ErrorIs(err, ErrOutboxFull,
		"recyclable capacity must be credited to only one active reservation")
	replacement := testCapturedGeneration(2, root, rejected.CaptureID, replacementRef)
	require.NoError(store.CommitCapture(
		t.Context(), replacementReservation.ID, replacement,
	))
	_, err = store.CollectGarbage(t.Context())
	require.NoError(err)

	var rejectedRows int
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM outbox_objects
		WHERE sha256 = ? AND length = ?`, rejectedRef.SHA256, rejectedRef.Length,
	).Scan(&rejectedRows))
	assert.Zero(rejectedRows)
	assert.NoFileExists(store.ObjectPath(rejectedRef))
	var replacementRefs int
	var replacementState string
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT ref_count, state FROM outbox_objects
		WHERE sha256 = ? AND length = ?`, replacementRef.SHA256, replacementRef.Length,
	).Scan(&replacementRefs, &replacementState))
	assert.Equal(1, replacementRefs)
	assert.Equal("live", replacementState)
	assert.FileExists(store.ObjectPath(replacementRef))
	var generations int
	require.NoError(store.db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM outbox_generations`,
	).Scan(&generations))
	assert.Equal(1, generations)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(err)
	assert.Equal(int64(1793), usage.UsedBytes)
}

func TestTransientFailureIsNotFinalizableBeforeRetryTime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	retryAt := time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureTransient, retryAt,
	))

	_, ok, err = store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	assert.False(ok)
	store.now = func() time.Time { return retryAt.Add(500 * time.Millisecond) }
	retried, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	assert.Equal(generation.CaptureID, retried.CaptureID)
}

func TestParentConflictCannotBeClearedByLateTransientFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureParentReceiptConflict, time.Time{},
	))

	err = store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureTransient, time.Now().Add(time.Minute),
	)

	require.ErrorIs(err, ErrGenerationFailureConflict)
	var class string
	var blocked int
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT error_class, blocked
		FROM outbox_generations WHERE capture_id = ?`, generation.CaptureID,
	).Scan(&class, &blocked))
	assert.Equal(string(GenerationFailureParentReceiptConflict), class)
	assert.Equal(1, blocked)
}

func TestResumeGenerationRequeuesAgainstReconciledServerHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureParentReceiptConflict, time.Time{},
	))
	reconciled := SourceHead{
		ManifestID: validCheckpointDigest(4), Receipt: validCheckpointDigest(5),
		Generation: 1,
	}

	require.NoError(store.ResumeGeneration(
		t.Context(), "device-a", generation.CaptureID, reconciled,
	))
	retried, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")

	require.NoError(err)
	require.True(ok)
	assert.Equal(generation.CaptureID, retried.CaptureID)
	assert.Equal(reconciled.Receipt, retried.ExpectedParentReceipt)
	var state, expectedParent, manifestID string
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT state, expected_parent_receipt,
		manifest_id FROM outbox_generations WHERE capture_id = ?`, generation.CaptureID,
	).Scan(&state, &expectedParent, &manifestID))
	assert.Equal("finalized", state)
	assert.Equal(expectedParent, reconciled.Receipt)
	assert.Empty(manifestID)
}

func TestResumeGenerationAtomicallyReconcilesUnblockedConflict(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	reconciled := SourceHead{
		ManifestID: validCheckpointDigest(4), Receipt: validCheckpointDigest(5),
		Generation: 1,
	}

	require.NoError(store.ResumeGeneration(
		t.Context(), "device-a", generation.CaptureID, reconciled,
	))
	retried, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")

	require.NoError(err)
	require.True(ok)
	assert.Equal(reconciled.Receipt, retried.ExpectedParentReceipt)
	var failureClass string
	var blocked int
	require.NoError(store.db.QueryRowContext(t.Context(), `SELECT error_class, blocked
		FROM outbox_generations WHERE capture_id = ?`, generation.CaptureID,
	).Scan(&failureClass, &blocked))
	assert.Empty(failureClass)
	assert.Zero(blocked)
}

func TestResumeGenerationRequeuesAgainstEmptyServerHead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{1})
	reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
	require.NoError(err)
	generation := testCapturedGeneration(1, root, "", ref)
	require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
	_, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(err)
	require.True(ok)
	require.NoError(store.RecordGenerationFailure(
		t.Context(), "device-a", generation.CaptureID,
		GenerationFailureParentReceiptConflict, time.Time{},
	))

	require.NoError(store.ResumeGeneration(
		t.Context(), "device-a", generation.CaptureID, SourceHead{},
	))
	retried, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")

	require.NoError(err)
	require.True(ok)
	assert.Empty(retried.ExpectedParentReceipt)
	head, ok, err := store.SourceHead(
		t.Context(), root.Provider, root.ID, generation.Source.SourceKey,
	)
	require.NoError(err)
	require.True(ok)
	assert.Empty(head.ManifestID)
	assert.Empty(head.Receipt)
	assert.Zero(head.Generation)
}

func TestRepeatedAcknowledgementsReuseBoundedOutboxCapacity(t *testing.T) {
	require := require.New(t)

	store, root := openOutboxTestStore(t, 1793)
	require.NoError(store.SetDevice(t.Context(), "device-a"))
	predecessor := ""
	for sequence := 1; sequence <= 3; sequence++ {
		ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(byte(9 + sequence)), Length: 1}
		installOutboxTestObject(t, store, ref, []byte{byte(sequence)})
		reservation, err := store.ReserveCapture(t.Context(), root.ID, 1793)
		require.NoError(err)
		generation := testCapturedGeneration(sequence, root, predecessor, ref)
		require.NoError(store.CommitCapture(t.Context(), reservation.ID, generation))
		manifest, ok, err := store.FinalizeNextManifest(t.Context(), "device-a")
		require.NoError(err)
		require.True(ok)
		commit := rawsync.CommitResult{
			ManifestID: validCheckpointDigest(byte(sequence)),
			Receipt:    validCheckpointDigest(byte(sequence + 3)),
			Generation: int64(sequence),
			Created:    true,
		}
		require.NoError(store.BindFinalizedCommit(
			t.Context(), "device-a", manifest.CaptureID, commit,
		))
		_, err = store.AcknowledgeGeneration(
			t.Context(), "device-a", manifest.CaptureID, commit,
		)
		require.NoError(err)
		usage, err := store.OutboxUsage(t.Context())
		require.NoError(err)
		assert.Zero(t, usage.UsedBytes)
		predecessor = generation.CaptureID
	}
}
