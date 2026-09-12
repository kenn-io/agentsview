package rawcheckpoint

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestBackfillSelectionAndSealRequireDurablePass(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	spec := BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: root.ID}}}
	progress, err := store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	require.Equal(t, "run-a", progress.RunID)
	require.Equal(t, "open", progress.Discovery)
	_, err = store.SealBackfill(t.Context(), spec.RunID)
	require.ErrorIs(t, err, ErrBackfillIncomplete)
	spec.Roots = append(spec.Roots, spec.Roots[0])
	spec.Providers = append(spec.Providers, parser.AgentClaude)
	_, err = store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	otherRoot, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, t.TempDir())
	require.NoError(t, err)
	changedRootSpec := spec
	changedRootSpec.Roots = []BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: otherRoot.ID}}
	_, err = store.BeginBackfill(t.Context(), changedRootSpec)
	require.ErrorIs(t, err, ErrBackfillConflict)
	spec.Destination = "https://different.example"
	_, err = store.BeginBackfill(t.Context(), spec)
	require.ErrorIs(t, err, ErrBackfillConflict)
	require.NoError(t, store.FinishBackfillProvider(t.Context(), "run-a", parser.AgentClaude, BackfillPassResult{Complete: true}))
	progress, err = store.SealBackfill(t.Context(), "run-a")
	require.NoError(t, err)
	require.False(t, progress.Complete)
	progress, err = store.CompleteBackfill(t.Context(), "run-a")
	require.NoError(t, err)
	require.True(t, progress.Complete)
}

func TestBackfillPublicationReceiptAndLostGeneration(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			store, root := openOutboxTestStore(t, 1<<20)
			require.NoError(t, store.SetDevice(t.Context(), "device-a"))
			_, err := store.BeginBackfill(t.Context(), BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []BackfillSelection{{parser.AgentClaude, root.ID}}})
			require.NoError(t, err)
			ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(10), Length: 1}
			installOutboxTestObject(t, store, ref, []byte{0})
			generation := testCapturedGeneration(1, root, "", ref)
			reservation, err := store.ReserveSourceCapture(t.Context(), generation.Source, 1793)
			require.NoError(t, err)
			require.NoError(t, store.CommitCaptureForBackfill(t.Context(), reservation.ID, generation, "run-a"))
			member, found, err := store.BackfillSource(t.Context(), "run-a", generation.Source)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, generation.CaptureID, member.CaptureID)
			require.Equal(t, int64(1), member.Ordinal)
			require.NoError(t, store.FinishBackfillProvider(t.Context(), "run-a", parser.AgentClaude, BackfillPassResult{Complete: true}))
			_, err = store.SealBackfill(t.Context(), "run-a")
			require.NoError(t, err)
			_, err = store.CompleteBackfill(t.Context(), "run-a")
			require.ErrorIs(t, err, ErrBackfillIncomplete)
			if lost {
				_, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, store.RecordGenerationFailure(t.Context(), "device-a", generation.CaptureID, GenerationFailurePermanent, time.Time{}))
				_, err = store.DiscardRejectedGeneration(t.Context(), "device-a", generation.CaptureID)
				require.NoError(t, err)
				p, err := store.BackfillProgress(t.Context(), "run-a")
				require.NoError(t, err)
				require.Equal(t, int64(1), p.Pending)
				require.Equal(t, int64(1), p.Failures["capture_lost"])
				_, err = store.CompleteBackfill(t.Context(), "run-a")
				require.ErrorIs(t, err, ErrBackfillIncomplete)
			} else {
				manifest, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
				require.NoError(t, err)
				require.True(t, found)
				commit := rawsync.CommitResult{ManifestID: validCheckpointDigest(1), Receipt: validCheckpointDigest(2), Generation: 1}
				require.NoError(t, store.BindFinalizedCommit(t.Context(), "device-a", manifest.CaptureID, commit))
				_, err = store.AcknowledgeGeneration(t.Context(), "device-a", manifest.CaptureID, commit)
				require.NoError(t, err)
				p, err := store.CompleteBackfill(t.Context(), "run-a")
				require.NoError(t, err)
				require.True(t, p.Complete)
				require.Equal(t, int64(1), p.Acknowledged)
				member, found, err = store.BackfillSource(t.Context(), "run-a", generation.Source)
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, commit.Receipt, member.Receipt)
				require.Equal(t, commit.ManifestID, member.ManifestID)
				_, found, err = store.NextGeneration(t.Context())
				require.NoError(t, err)
				require.False(t, found)
			}
		})
	}
}

func TestBackfillSelectorIncludesOnlyMembersAndPredecessors(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	_, err := store.BeginBackfill(t.Context(), BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []BackfillSelection{{parser.AgentClaude, root.ID}}})
	require.NoError(t, err)
	var chain []CapturedGeneration
	for i := 1; i <= 4; i++ {
		ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(byte(i)), Length: 1}
		installOutboxTestObject(t, store, ref, []byte{byte(i)})
		predecessor := ""
		if i >= 3 {
			predecessor = chain[i-2].CaptureID
		}
		gen := testCapturedGeneration(i, root, predecessor, ref)
		if i == 1 {
			gen.Source.SourceKey = "unrelated"
		}
		r, err := store.ReserveSourceCapture(t.Context(), gen.Source, 1793)
		require.NoError(t, err)
		if i == 3 {
			err = store.CommitCaptureForBackfill(t.Context(), r.ID, gen, "run-a")
		} else {
			err = store.CommitCapture(t.Context(), r.ID, gen)
		}
		require.NoError(t, err)
		chain = append(chain, gen)
	}
	for i := 1; i <= 2; i++ {
		m, found, err := store.FinalizeNextManifestForBackfill(t.Context(), "device-a", "run-a")
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, chain[i].CaptureID, m.CaptureID)
		commit := rawsync.CommitResult{ManifestID: validCheckpointDigest(byte(i + 5)), Receipt: validCheckpointDigest(byte(i + 7)), Generation: int64(i)}
		require.NoError(t, store.BindFinalizedCommit(t.Context(), "device-a", m.CaptureID, commit))
		_, err = store.AcknowledgeGeneration(t.Context(), "device-a", m.CaptureID, commit)
		require.NoError(t, err)
	}
	_, found, err := store.FinalizeNextManifestForBackfill(t.Context(), "device-a", "run-a")
	require.NoError(t, err)
	require.False(t, found, "later watch append and unrelated source remain outside run")
}

func TestBackfillRefusesEmptyProviderPassAndChangedDevice(t *testing.T) {
	store, _ := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	spec := BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}}
	_, err := store.BeginBackfill(t.Context(), spec)
	require.NoError(t, err)
	err = store.FinishBackfillProvider(t.Context(), "run-a", parser.AgentClaude, BackfillPassResult{Complete: true})
	require.ErrorIs(t, err, ErrBackfillIncomplete)
	require.NoError(t, store.SetDevice(t.Context(), "device-b"))
	_, err = store.BeginBackfill(t.Context(), spec)
	require.ErrorIs(t, err, ErrDeviceMismatch)
}

func TestBackfillPublicationOutsideSelectionRollsBack(t *testing.T) {
	store, root := openOutboxTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	_, err := store.BeginBackfill(t.Context(), BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}})
	require.NoError(t, err)
	ref := rawsync.ObjectRef{SHA256: validCheckpointDigest(4), Length: 1}
	installOutboxTestObject(t, store, ref, []byte{4})
	generation := testCapturedGeneration(1, root, "", ref)
	reservation, err := store.ReserveSourceCapture(t.Context(), generation.Source, 1793)
	require.NoError(t, err)
	require.ErrorIs(t, store.CommitCaptureForBackfill(t.Context(), reservation.ID, generation, "run-a"), ErrBackfillConflict)
	_, found, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.False(t, found)
	p, err := store.BackfillProgress(t.Context(), "run-a")
	require.NoError(t, err)
	require.Zero(t, p.Captured)
	usage, err := store.OutboxUsage(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1793), usage.ReservedBytes)
}

func TestBackfillVersionEightMigrationPreservesQueueAndReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, err := Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, t.TempDir())
	require.NoError(t, err)
	ref := rawsync.ObjectRef{SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("x"))), Length: 1}
	installOutboxTestObject(t, store, ref, []byte("x"))
	gen := testCapturedGeneration(1, root, "", ref)
	r, err := store.ReserveSourceCapture(t.Context(), gen.Source, 1793)
	require.NoError(t, err)
	require.NoError(t, store.CommitCapture(t.Context(), r.ID, gen))
	_, _, err = store.FinalizeNextManifest(t.Context(), "device-a")
	require.NoError(t, err)
	receipt := rawsync.CommitResult{ManifestID: validCheckpointDigest(7), Receipt: validCheckpointDigest(8), Generation: 1}
	require.NoError(t, store.BindFinalizedCommit(t.Context(), "device-a", gen.CaptureID, receipt))
	_, err = store.AcknowledgeGeneration(t.Context(), "device-a", gen.CaptureID, receipt)
	require.NoError(t, err)
	// A queued tombstone retains its acknowledged predecessor independently of
	// the compacted receipt. Strip only new, empty tables to obtain a v8 fixture.
	queued, found, err := store.QueueTombstone(t.Context(), gen.Source)
	require.NoError(t, err)
	require.True(t, found)
	for _, statement := range []string{`DROP TRIGGER backfill_generation_deleted`, `DROP TABLE backfill_members`, `DROP TABLE backfill_roots`, `DROP TABLE backfill_providers`, `DROP TABLE backfill_runs`, `PRAGMA user_version=8`} {
		_, err = store.db.Exec(statement)
		require.NoError(t, err)
	}
	require.NoError(t, store.Close())
	store, err = Open(t.Context(), path)
	require.NoError(t, err)
	defer store.Close()
	next, found, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, queued, next.CaptureID)
	head, found, err := store.SourceHead(t.Context(), gen.Source.Provider, root.ID, gen.Source.SourceKey)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, receipt.Receipt, head.Receipt)
	require.Equal(t, receipt.Generation, head.Generation)
	var version int
	require.NoError(t, store.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, 9, version)
}
