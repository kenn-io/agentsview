package artifact

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

const importLocalOrigin = "local-b2c3d4"

func TestArtifactImportEndToEndAndReplay(t *testing.T) {
	source := testExportDB(t)
	seedSession(t, source, "one", "project")
	seedSession(t, source, "two", "project")
	cost := &money.Money{Microdollars: 12_345}
	require.NoError(t, source.ReplaceSessionUsageEvents("one", []db.UsageEvent{{
		SessionID: "one", Source: "provider", Model: "model",
		Cost: cost, CostStatus: "known", CostSource: "provider",
		DedupKey: "usage-one",
	}}))
	store := newTestArtifactStore(t)
	exported, err := ExportToStore(
		t.Context(), source, store,
		ExportOptions{Origin: contractOrigin, Full: true},
	)
	require.NoError(t, err)
	require.True(t, exported.CheckpointCreated)

	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	recordAllImportEntries(t, coordinator, store, contractOrigin)
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, result.Sessions)
	assert.Equal(t, 4, result.Messages)
	assert.Zero(t, result.Deferred)
	assert.Zero(t, result.Quarantined)
	assert.False(t, result.More)

	for _, nativeID := range []string{"one", "two"} {
		importedID := contractOrigin + "~" + nativeID
		session, err := destination.GetSessionFull(t.Context(), importedID)
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, contractOrigin, session.Machine)
		assert.Nil(t, session.FilePath)
		messages, err := destination.GetMessages(
			t.Context(), importedID, 0, 10, true,
		)
		require.NoError(t, err)
		assert.Len(t, messages, 2)
	}
	usage, err := destination.GetUsageEvents(
		t.Context(), contractOrigin+"~one",
	)
	require.NoError(t, err)
	require.Len(t, usage, 1)
	assert.Equal(t, cost, usage[0].Cost)

	checkpointEntry := latestImportCheckpointEntry(
		t, store, contractOrigin,
	)
	sequence, err := checkpointSequence(checkpointEntry.Ref.Name)
	require.NoError(t, err)
	landing, sessionMap, found, err :=
		destination.GetArtifactCheckpointLanding(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, sequence, landing.Sequence)
	assert.Len(t, sessionMap, 2)
	provenance, err := destination.ArtifactImportedManifestHashes(
		t.Context(), contractOrigin,
		[]string{contractOrigin + "~one", contractOrigin + "~two"},
	)
	require.NoError(t, err)
	assert.Equal(t, sessionMap, provenance)

	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))
	replay, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, replay.Sessions)
	assert.Zero(t, replay.Messages)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestStoreImportCoordinatorIgnoresLocalOrigin(t *testing.T) {
	store := newTestArtifactStore(t)
	entry := createImportTestCheckpoint(
		t, store, contractOrigin, 1, map[string]string{},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, contractOrigin,
	)

	require.NoError(t, coordinator.RecordChanged(t.Context(), entry))
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, result.Sessions)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestStoreImportCoordinatorRetriesMissingSegmentAfterArrival(t *testing.T) {
	store := newTestArtifactStore(t)
	segmentBody, err := encodeSegment([]db.Message{{
		Ordinal: 0, Role: "user", Content: "arrived",
	}})
	require.NoError(t, err)
	segmentIdentity := identityForBytes(t, segmentBody)
	m := importTestManifest("session")
	m.Session.MessageCount = 1
	m.Session.UserMessageCount = 1
	m.Segments = []string{segmentIdentity.SHA256}
	manifestHash := createImportTestManifest(t, store, m, false)
	checkpointEntry := createImportTestCheckpoint(
		t, store, contractOrigin, 1,
		map[string]string{contractOrigin + "~session": manifestHash},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	first, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, first.Deferred)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	segmentRef := requireContractRef(
		t, contractOrigin, KindSegments,
		segmentIdentity.SHA256+".ndjson",
	)
	created := createContractArtifact(t, store, segmentRef, segmentBody)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), created.Entry,
	))
	second, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, second.Sessions)
	assert.Zero(t, second.Deferred)
	session, err := destination.GetSessionFull(
		t.Context(), contractOrigin+"~session",
	)
	require.NoError(t, err)
	require.NotNil(t, session)
}

func TestStoreImportCoordinatorTracksIndependentFutureRequirements(t *testing.T) {
	tests := []struct {
		name         string
		prepare      func(*testing.T, ArtifactStore) string
		wantManifest int
		wantSegment  int
		understood   db.ArtifactImportVersions
	}{
		{
			name: "future manifest",
			prepare: func(t *testing.T, store ArtifactStore) string {
				return createHashedImportArtifact(
					t, store, KindManifests, ".json",
					[]byte(`{"origin":"contract-a1b2c3","v":3}`),
				)
			},
			wantManifest: 3, wantSegment: messageSegmentFormatVersion,
			understood: db.ArtifactImportVersions{
				Checkpoint: checkpointFormatVersion,
				Manifest:   3,
				Segment:    messageSegmentFormatVersion,
			},
		},
		{
			name: "future segment",
			prepare: func(t *testing.T, store ArtifactStore) string {
				segment := []byte(
					"{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":2}\n",
				)
				segmentHash := createHashedImportArtifact(
					t, store, KindSegments, ".ndjson", segment,
				)
				m := importTestManifest("session")
				m.Segments = []string{segmentHash}
				return createImportTestManifest(t, store, m, false)
			},
			wantManifest: manifestFormatVersion, wantSegment: 2,
			understood: db.ArtifactImportVersions{
				Checkpoint: checkpointFormatVersion,
				Manifest:   manifestFormatVersion,
				Segment:    2,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestArtifactStore(t)
			manifestHash := tc.prepare(t, store)
			checkpointEntry := createImportTestCheckpoint(
				t, store, contractOrigin, 1,
				map[string]string{contractOrigin + "~session": manifestHash},
			)
			destination := testDB(t)
			coordinator := NewStoreImportCoordinator(
				destination, store, importLocalOrigin,
			)
			require.NoError(t, coordinator.RecordChanged(
				t.Context(), checkpointEntry,
			))

			result, err := coordinator.Finalize(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, result.Deferred)
			attempt, err := destination.ReserveArtifactImportAttemptGeneration(
				t.Context(),
			)
			require.NoError(t, err)
			pending, err := destination.PendingArtifactImports(
				t.Context(), tc.understood, attempt, 10,
			)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, tc.wantManifest, pending[0].RequiredManifestVersion)
			assert.Equal(t, tc.wantSegment, pending[0].RequiredSegmentVersion)
		})
	}
}

func TestStoreImportCoordinatorQuarantinesInvalidCheckpointAndContinues(
	t *testing.T,
) {
	store := newTestArtifactStore(t)
	invalidOrigin := "alpha-a1b2c3"
	invalidRef := requireContractRef(
		t, invalidOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	invalid := createContractArtifact(
		t, store, invalidRef,
		[]byte(`{"origin":"alpha-a1b2c3","seq":1,"v":1}`),
	)

	source := testExportDB(t)
	seedSession(t, source, "valid", "project")
	_, err := ExportToStore(
		t.Context(), source, store,
		ExportOptions{Origin: contractOrigin, Full: true},
	)
	require.NoError(t, err)
	valid := latestImportCheckpointEntry(t, store, contractOrigin)

	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), invalid.Entry))
	require.NoError(t, coordinator.RecordChanged(t.Context(), valid))
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Quarantined)
	assert.Equal(t, 1, result.Sessions)
	_, err = store.Stat(t.Context(), invalidRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
	session, err := destination.GetSessionFull(
		t.Context(), contractOrigin+"~valid",
	)
	require.NoError(t, err)
	require.NotNil(t, session)
}

func TestStoreImportCoordinatorRetainsClaimOnOperationalStoreError(t *testing.T) {
	base := newTestArtifactStore(t)
	checkpointEntry := createImportTestCheckpoint(
		t, base, contractOrigin, 1, map[string]string{},
	)
	operational := errors.New("archive unavailable")
	store := &failingImportOpenStore{
		ArtifactStore: base, failRef: checkpointEntry.Ref, err: operational,
	}
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	_, err := coordinator.Finalize(t.Context())
	require.ErrorIs(t, err, operational)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestStoreImportCoordinatorSuppressesExcludedAndTrashedSessions(t *testing.T) {
	store := newTestArtifactStore(t)
	sessionMap := make(map[string]string)
	for _, nativeID := range []string{"excluded", "trashed"} {
		m := importTestManifest(nativeID)
		sessionMap[contractOrigin+"~"+nativeID] = createImportTestClosure(
			t, store, &m, []db.Message{{
				Ordinal: 0, Role: "user", Content: nativeID,
			}},
		)
	}
	checkpointEntry := createImportTestCheckpoint(
		t, store, contractOrigin, 1, sessionMap,
	)
	destination := testDB(t)
	excludedID := contractOrigin + "~excluded"
	trashedID := contractOrigin + "~trashed"
	seedSession(t, destination, excludedID, "local")
	require.NoError(t, destination.DeleteSession(excludedID))
	seedSession(t, destination, trashedID, "local")
	require.NoError(t, destination.SoftDeleteSession(trashedID))

	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, result.Sessions)
	assert.Zero(t, result.Deferred)

	excluded, err := destination.GetSessionFull(t.Context(), excludedID)
	require.NoError(t, err)
	assert.Nil(t, excluded)
	trashed, err := destination.GetSessionFull(t.Context(), trashedID)
	require.NoError(t, err)
	require.NotNil(t, trashed)
	assert.NotNil(t, trashed.DeletedAt)
	provenance, err := destination.ArtifactImportedManifestHashes(
		t.Context(), contractOrigin, []string{excludedID, trashedID},
	)
	require.NoError(t, err)
	assert.Equal(t, sessionMap, provenance)
	_, landedMap, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, sessionMap, landedMap)
}

func TestStoreImportCoordinatorKeepsCheckpointPendingAfterInvalidDependency(
	t *testing.T,
) {
	store := newTestArtifactStore(t)
	segment := []byte("{not-json}\n")
	segmentHash := createHashedImportArtifact(
		t, store, KindSegments, ".ndjson", segment,
	)
	m := importTestManifest("session")
	m.Segments = []string{segmentHash}
	manifestHash := createImportTestManifest(t, store, m, false)
	checkpointEntry := createImportTestCheckpoint(
		t, store, contractOrigin, 1,
		map[string]string{contractOrigin + "~session": manifestHash},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Quarantined)
	assert.Equal(t, 1, result.Deferred)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	_, _, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestStoreImportCoordinatorDoesNotDeleteSessionOmittedByNewCheckpoint(
	t *testing.T,
) {
	store := newTestArtifactStore(t)
	m := importTestManifest("session")
	manifestHash := createImportTestClosure(t, store, &m, []db.Message{{
		Ordinal: 0, Role: "user", Content: "kept",
	}})
	first := createImportTestCheckpoint(
		t, store, contractOrigin, 1,
		map[string]string{contractOrigin + "~session": manifestHash},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), first))
	_, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)

	second := createImportTestCheckpoint(
		t, store, contractOrigin, 2, map[string]string{},
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), second))
	_, err = coordinator.Finalize(t.Context())
	require.NoError(t, err)
	session, err := destination.GetSessionFull(
		t.Context(), contractOrigin+"~session",
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	messages, err := destination.GetMessages(
		t.Context(), session.ID, 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "kept", messages[0].Content)
}

func createImportTestCheckpoint(
	t *testing.T,
	store ArtifactStore,
	origin string,
	sequence int,
	sessions map[string]string,
) Entry {
	t.Helper()
	body, err := canonicalJSON(checkpoint{
		Version: checkpointFormatVersion, Origin: origin,
		Sequence: sequence, Sessions: sessions,
	})
	require.NoError(t, err)
	ref := requireContractRef(
		t, origin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequence),
	)
	return createContractArtifact(t, store, ref, body).Entry
}

func latestImportCheckpointEntry(
	t *testing.T, store ArtifactStore, origin string,
) Entry {
	t.Helper()
	entries := listAllContractEntries(
		t, store, origin, KindCheckpoints, maxArtifactListPageSize,
	)
	require.NotEmpty(t, entries)
	winner := entries[0]
	winnerSequence, err := checkpointSequence(winner.Ref.Name)
	require.NoError(t, err)
	for _, entry := range entries[1:] {
		sequence, err := checkpointSequence(entry.Ref.Name)
		require.NoError(t, err)
		if sequence > winnerSequence {
			winner = entry
			winnerSequence = sequence
		}
	}
	return winner
}

func recordAllImportEntries(
	t *testing.T,
	coordinator *StoreImportCoordinator,
	store ArtifactStore,
	origin string,
) {
	t.Helper()
	for _, kind := range []Kind{KindSegments, KindManifests, KindCheckpoints} {
		for _, entry := range listAllContractEntries(
			t, store, origin, kind, maxArtifactListPageSize,
		) {
			require.NoError(t, coordinator.RecordChanged(t.Context(), entry))
		}
	}
}
