package artifact

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

const importLocalOrigin = "local-b2c3d4"

func TestArtifactImportEndToEndAndReplay(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestStoreImportCoordinatorRejectsOutOfRangeCheckpointWithoutAdvancingHead(
	t *testing.T,
) {
	t.Parallel()

	store := newTestArtifactStore(t)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	outOfRange := Entry{
		Ref: Ref{
			Origin: contractOrigin, Kind: KindCheckpoints,
			Name: "cp-2147483648.json",
		},
		Identity: identityForBytes(t, []byte("out-of-range")),
	}

	err := coordinator.RecordChanged(t.Context(), outOfRange)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
	_, found, err := destination.GetArtifactPeerCheckpointHead(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	assert.False(t, found)

	valid := createImportTestCheckpoint(
		t, store, contractOrigin, 1, map[string]string{},
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), valid))
	head, found, err := destination.GetArtifactPeerCheckpointHead(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, head.Sequence)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestStoreImportCoordinatorRetriesMissingSegmentAfterArrival(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
					[]byte(`{"origin":"contract-a1b2c3","v":5}`),
				)
			},
			wantManifest: manifestFormatVersion + 1,
			wantSegment:  messageSegmentFormatVersion,
			understood: db.ArtifactImportVersions{
				Checkpoint: checkpointFormatVersion,
				Manifest:   manifestFormatVersion + 1,
				Segment:    messageSegmentFormatVersion,
			},
		},
		{
			name: "future segment",
			prepare: func(t *testing.T, store ArtifactStore) string {
				segment := []byte(
					"{\"content\":\"future\",\"ordinal\":0,\"role\":\"user\",\"v\":4}\n",
				)
				segmentHash := createHashedImportArtifact(
					t, store, KindSegments, ".ndjson", segment,
				)
				m := importTestManifest("session")
				m.Segments = []string{segmentHash}
				return createImportTestManifest(t, store, m, false)
			},
			wantManifest: manifestFormatVersion,
			wantSegment:  messageSegmentFormatVersion + 1,
			understood: db.ArtifactImportVersions{
				Checkpoint: checkpointFormatVersion,
				Manifest:   manifestFormatVersion,
				Segment:    messageSegmentFormatVersion + 1,
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

func TestStoreImportCoordinatorDefersLargeFutureCheckpointBeforeValidClaim(
	t *testing.T,
) {
	t.Parallel()

	const futureSessionCount = artifactImportDrainLimit*2 + 1
	store := newTestArtifactStore(t)
	futureSessions := make(map[string]string, futureSessionCount)
	for i := range futureSessionCount {
		futureSessions[fmt.Sprintf("%s~future-%03d", contractOrigin, i)] =
			fmt.Sprintf("%064x", i+1)
	}
	futureBody, err := canonicalJSON(checkpoint{
		Version: checkpointFormatVersion + 1,
		Origin:  contractOrigin, Sequence: 1, Sessions: futureSessions,
	})
	require.NoError(t, err)
	futureRef := requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	futureEntry := createContractArtifact(
		t, store, futureRef, futureBody,
	).Entry

	const supportedOrigin = "contract-d4e5f6"
	supportedEntry := createImportTestCheckpoint(
		t, store, supportedOrigin, 1, map[string]string{},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), futureEntry))
	require.NoError(t, coordinator.RecordChanged(t.Context(), supportedEntry))

	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Deferred)
	landing, _, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), supportedOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, landing.Sequence)

	_, err = destination.ArtifactCheckpointStageProgress(
		t.Context(),
		db.ArtifactCheckpointLanding{
			Origin: contractOrigin, Sequence: 1,
			CheckpointSHA256: futureEntry.Identity.SHA256,
			CheckpointSize:   futureEntry.Identity.Size,
		},
	)
	require.ErrorIs(t, err, db.ErrArtifactImportConflict)
}

func TestStoreImportCoordinatorFinishesSupportedSessionsBeforeFutureGate(
	t *testing.T,
) {
	t.Parallel()

	store := newTestArtifactStore(t)
	sessionMap := map[string]string{
		contractOrigin + "~000-future": createHashedImportArtifact(
			t, store, KindManifests, ".json",
			[]byte(`{"origin":"contract-a1b2c3","v":5}`),
		),
	}
	const supportedSessions = artifactImportDrainLimit + 1
	for i := range supportedSessions {
		nativeID := fmt.Sprintf("supported-%03d", i)
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
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	first, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	require.True(t, first.More)
	second, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	require.True(t, second.More)
	coordinator = NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	imported := first.Sessions + second.Sessions
	deferred := first.Deferred + second.Deferred
	for rounds := 0; ; rounds++ {
		require.Less(t, rounds, 10)
		result, err := coordinator.Finalize(t.Context())
		require.NoError(t, err)
		imported += result.Sessions
		deferred += result.Deferred
		if !result.More {
			break
		}
	}
	assert.Equal(t, supportedSessions, imported)
	assert.Positive(t, deferred)
	session, err := destination.GetSessionFull(
		t.Context(), contractOrigin+"~supported-128",
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "project", session.Project)

	attempt, err := destination.ReserveArtifactImportAttemptGeneration(
		t.Context(),
	)
	require.NoError(t, err)
	pending, err := destination.PendingArtifactImports(
		t.Context(),
		db.ArtifactImportVersions{
			Checkpoint: checkpointFormatVersion,
			Manifest:   manifestFormatVersion + 1,
			Segment:    messageSegmentFormatVersion,
		},
		attempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, manifestFormatVersion+1, pending[0].RequiredManifestVersion)
}

func TestStoreImportCoordinatorQuarantinesInvalidCheckpointAndContinues(
	t *testing.T,
) {
	t.Parallel()

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

func TestStoreImportCoordinatorRecoversCrashAfterCheckpointQuarantine(
	t *testing.T,
) {
	t.Parallel()

	base := newTestArtifactStore(t)
	invalidOrigin := "alpha-a1b2c3"
	invalidRef := requireContractRef(
		t, invalidOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	invalid := createContractArtifact(
		t, base, invalidRef,
		[]byte(`{"origin":"alpha-a1b2c3","seq":1,"v":1}`),
	)
	valid := createImportTestCheckpoint(
		t, base, contractOrigin, 1, map[string]string{},
	)
	injected := errors.New("crash after quarantine")
	store := &failAfterQuarantineImportStore{
		ArtifactStore: base,
		err:           injected,
	}
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), invalid.Entry))
	require.NoError(t, coordinator.RecordChanged(t.Context(), valid))

	_, err := coordinator.Finalize(t.Context())
	require.ErrorIs(t, err, injected)
	coordinator = NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Quarantined)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count)
	_, _, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	assert.True(t, found)
}

func TestStoreImportCoordinatorDiscardsPartialStageAfterQuarantineCrash(
	t *testing.T,
) {
	t.Parallel()

	base := newTestArtifactStore(t)
	sessionMap := make(map[string]string, artifactImportDrainLimit+1)
	for i := range artifactImportDrainLimit {
		sessionMap[fmt.Sprintf("%s~valid-%03d", contractOrigin, i)] =
			fmt.Sprintf("%064x", i+1)
	}
	sessionMap[contractOrigin+"~zzz-invalid"] = "invalid"
	checkpointEntry := createImportTestCheckpoint(
		t, base, contractOrigin, 1, sessionMap,
	)
	injected := errors.New("crash after partial checkpoint quarantine")
	store := &failAfterQuarantineImportStore{
		ArtifactStore: base,
		err:           injected,
	}
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	first, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	require.True(t, first.More)
	landing := db.ArtifactCheckpointLanding{
		Origin:           contractOrigin,
		Sequence:         1,
		CheckpointSHA256: checkpointEntry.Identity.SHA256,
		CheckpointSize:   checkpointEntry.Identity.Size,
	}
	progress, err := destination.ArtifactCheckpointStageProgress(
		t.Context(), landing,
	)
	require.NoError(t, err)
	assert.Equal(t, artifactImportDrainLimit, progress.DecodedCount)

	_, err = coordinator.Finalize(t.Context())
	require.ErrorIs(t, err, injected)
	coordinator = NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Quarantined)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count)
	_, err = destination.ArtifactCheckpointStageProgress(
		t.Context(), landing,
	)
	require.ErrorIs(t, err, db.ErrArtifactImportConflict)
}

func TestStoreImportCoordinatorRetainsClaimOnOperationalStoreError(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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

func TestStoreImportCoordinatorRetriesTrashedManifestAfterRestore(t *testing.T) {
	t.Parallel()

	store := newTestArtifactStore(t)
	gid := contractOrigin + "~session"
	firstManifest := importTestManifest("session")
	firstHash := createImportTestClosure(
		t, store, &firstManifest, []db.Message{{
			Ordinal: 0, Role: "user", Content: "version A",
		}},
	)
	first := createImportTestCheckpoint(
		t, store, contractOrigin, 1, map[string]string{gid: firstHash},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), first))
	_, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	require.NoError(t, destination.SoftDeleteSession(gid))

	secondManifest := importTestManifest("session")
	secondHash := createImportTestClosure(
		t, store, &secondManifest, []db.Message{{
			Ordinal: 0, Role: "user", Content: "version B",
		}},
	)
	second := createImportTestCheckpoint(
		t, store, contractOrigin, 2, map[string]string{gid: secondHash},
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), second))
	deferred, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, deferred.Deferred)
	assert.Zero(t, deferred.Sessions)

	provenance, err := destination.ArtifactImportedManifestHashes(
		t.Context(), contractOrigin, []string{gid},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{gid: firstHash}, provenance)
	landing, landedMap, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, landing.Sequence)
	assert.Equal(t, map[string]string{gid: firstHash}, landedMap)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	coordinator = NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	restored, err := destination.RestoreSession(gid)
	require.NoError(t, err)
	require.EqualValues(t, 1, restored)
	require.NoError(t, coordinator.RecordChanged(t.Context(), second))
	applied, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, applied.Sessions)
	assert.Zero(t, applied.Deferred)

	messages, err := destination.GetAllMessages(t.Context(), gid)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "version B", messages[0].Content)
	provenance, err = destination.ArtifactImportedManifestHashes(
		t.Context(), contractOrigin, []string{gid},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{gid: secondHash}, provenance)
	landing, landedMap, found, err = destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 2, landing.Sequence)
	assert.Equal(t, map[string]string{gid: secondHash}, landedMap)
	count, _, err = destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestStoreImportCoordinatorContinuesAfterConcurrentCheckpointSupersession(
	t *testing.T,
) {
	t.Parallel()

	store := newTestArtifactStore(t)
	gid := contractOrigin + "~session"
	firstManifest := importTestManifest("session")
	firstHash := createImportTestClosure(
		t, store, &firstManifest, []db.Message{{
			Ordinal: 0, Role: "user", Content: "version A",
		}},
	)
	first := createImportTestCheckpoint(
		t, store, contractOrigin, 1, map[string]string{gid: firstHash},
	)
	secondManifest := importTestManifest("session")
	secondHash := createImportTestClosure(
		t, store, &secondManifest, []db.Message{{
			Ordinal: 0, Role: "user", Content: "version B",
		}},
	)
	second := createImportTestCheckpoint(
		t, store, contractOrigin, 2, map[string]string{gid: secondHash},
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	coordinator.hooks = &importCoordinatorHooks{
		afterProvenance: func() error {
			coordinator.hooks.afterProvenance = nil
			return coordinator.RecordChanged(t.Context(), second)
		},
	}
	require.NoError(t, coordinator.RecordChanged(t.Context(), first))

	superseded, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	require.True(t, superseded.More)
	for rounds := 0; ; rounds++ {
		require.Less(t, rounds, 5)
		result, err := coordinator.Finalize(t.Context())
		require.NoError(t, err)
		if !result.More {
			break
		}
	}

	messages, err := destination.GetAllMessages(t.Context(), gid)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "version B", messages[0].Content)
	landing, landedMap, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 2, landing.Sequence)
	assert.Equal(t, map[string]string{gid: secondHash}, landedMap)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestStoreImportCoordinatorSuppressesLocalSessionIDCollision(t *testing.T) {
	t.Parallel()

	store := newTestArtifactStore(t)
	m := importTestManifest("session")
	manifestHash := createImportTestClosure(t, store, &m, []db.Message{{
		Ordinal: 0, Role: "user", Content: "peer content",
	}})
	checkpointEntry := createImportTestCheckpoint(
		t, store, contractOrigin, 1,
		map[string]string{contractOrigin + "~session": manifestHash},
	)
	destination := testDB(t)
	collidingID := contractOrigin + "~session"
	seedSession(t, destination, collidingID, "local-project")

	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))
	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, result.Sessions)

	session, err := destination.GetSessionFull(t.Context(), collidingID)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "local-project", session.Project)
	assert.Equal(t, "local", session.Machine)
	messages, err := destination.GetMessages(
		t.Context(), collidingID, 0, 10, true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.NotEqual(t, "peer content", messages[0].Content)
	provenance, err := destination.ArtifactImportedManifestHashes(
		t.Context(), contractOrigin, []string{collidingID},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{collidingID: manifestHash}, provenance)
}

func TestStoreImportCoordinatorKeepsCheckpointPendingAfterInvalidDependency(
	t *testing.T,
) {
	t.Parallel()

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
	t.Parallel()

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

func TestStoreImportCoordinatorCrashWindowsConverge(t *testing.T) {
	t.Parallel()

	injected := errors.New("injected crash")
	tests := []struct {
		name      string
		recordErr bool
		install   func(*importCoordinatorHooks)
	}{
		{
			name:      "after peer head before queue",
			recordErr: true,
			install: func(hooks *importCoordinatorHooks) {
				hooks.afterPeerHead = func() error { return injected }
			},
		},
		{
			name: "after session write before provenance",
			install: func(hooks *importCoordinatorHooks) {
				hooks.afterSessionWrite = func() error { return injected }
			},
		},
		{
			name: "after provenance before landing",
			install: func(hooks *importCoordinatorHooks) {
				hooks.afterProvenance = func() error { return injected }
			},
		},
		{
			name: "after landing before acknowledgement",
			install: func(hooks *importCoordinatorHooks) {
				hooks.afterLanding = func() error { return injected }
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			databasePath := filepath.Join(root, "archive.db")
			storeRoot := filepath.Join(root, "artifacts")
			store, err := newProtocolTestStore(storeRoot)
			require.NoError(t, err)
			database, err := db.Open(databasePath)
			require.NoError(t, err)

			ordinal := 0
			m := importTestManifest("session")
			m.UsageEvents = []artifactUsageEvent{{
				MessageOrdinal: &ordinal,
				Source:         "provider",
				Model:          "model",
				DedupKey:       "usage",
			}}
			manifestHash := createImportTestClosure(
				t, store, &m, []db.Message{{
					Ordinal: 0, Role: "user", Content: "once",
				}},
			)
			checkpointEntry := createImportTestCheckpoint(
				t, store, contractOrigin, 1,
				map[string]string{
					contractOrigin + "~session": manifestHash,
				},
			)
			coordinator := NewStoreImportCoordinator(
				database, store, importLocalOrigin,
			)
			coordinator.hooks = &importCoordinatorHooks{}
			tc.install(coordinator.hooks)

			if tc.recordErr {
				err = coordinator.RecordChanged(t.Context(), checkpointEntry)
			} else {
				require.NoError(t, coordinator.RecordChanged(
					t.Context(), checkpointEntry,
				))
				_, err = coordinator.Finalize(t.Context())
			}
			require.ErrorIs(t, err, injected)
			require.NoError(t, database.Close())
			require.NoError(t, store.Close())

			database, err = db.Open(databasePath)
			require.NoError(t, err)
			store, err = newProtocolTestStore(storeRoot)
			require.NoError(t, err)
			coordinator = NewStoreImportCoordinator(
				database, store, importLocalOrigin,
			)
			require.NoError(t, coordinator.RecordChanged(
				t.Context(), checkpointEntry,
			))
			_, err = coordinator.Finalize(t.Context())
			require.NoError(t, err)

			head, found, err := database.GetArtifactPeerCheckpointHead(
				t.Context(), contractOrigin,
			)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, 1, head.Sequence)
			landing, _, found, err :=
				database.GetArtifactCheckpointLanding(
					t.Context(), contractOrigin,
				)
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, 1, landing.Sequence)
			count, _, err := database.ArtifactImportQueueStats(t.Context())
			require.NoError(t, err)
			assert.Zero(t, count)
			messages, err := database.GetMessages(
				t.Context(), contractOrigin+"~session", 0, 10, true,
			)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "once", messages[0].Content)
			usage, err := database.GetUsageEvents(
				t.Context(), contractOrigin+"~session",
			)
			require.NoError(t, err)
			require.Len(t, usage, 1)
			assert.Equal(t, "usage", usage[0].DedupKey)
			require.NoError(t, database.Close())
			require.NoError(t, store.Close())
		})
	}
}

func TestStoreImportCoordinatorBoundsUnchangedCheckpointWork(t *testing.T) {
	t.Parallel()

	const unchangedSessions = 10_000
	base := newTestArtifactStore(t)
	m := importTestManifest("changed")
	changedHash := createImportTestClosure(t, base, &m, []db.Message{{
		Ordinal: 0, Role: "user", Content: "changed",
	}})
	sessionMap := make(map[string]string, unchangedSessions+1)
	destination := testDB(t)
	for i := range unchangedSessions {
		gid := fmt.Sprintf("%s~unchanged-%05d", contractOrigin, i)
		hash := fmt.Sprintf("%064x", i+1)
		sessionMap[gid] = hash
		require.NoError(t, destination.RecordArtifactImportedSession(
			t.Context(),
			db.ArtifactImportedSession{
				Origin:            contractOrigin,
				GID:               gid,
				ManifestHash:      hash,
				ImportedSessionID: gid,
			},
		))
	}
	sessionMap[contractOrigin+"~changed"] = changedHash
	checkpointEntry := createImportTestCheckpoint(
		t, base, contractOrigin, 1, sessionMap,
	)
	store := &countingImportStore{ArtifactStore: base}
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	var pendingLimits, pendingCounts, provenancePages, stagePages []int
	coordinator.hooks = &importCoordinatorHooks{
		observePending: func(limit, count int) {
			pendingLimits = append(pendingLimits, limit)
			pendingCounts = append(pendingCounts, count)
		},
		observeProvenance: func(count int) {
			provenancePages = append(provenancePages, count)
		},
		observeStage: func(count int) {
			stagePages = append(stagePages, count)
		},
	}
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))
	totalSessions := 0
	for rounds := 0; ; rounds++ {
		require.Less(t, rounds, 100)
		result, err := coordinator.Finalize(t.Context())
		require.NoError(t, err)
		totalSessions += result.Sessions
		if !result.More {
			break
		}
	}
	assert.Equal(t, 1, totalSessions)
	require.Len(t, pendingLimits, 79)
	for _, limit := range pendingLimits {
		assert.Equal(t, artifactImportDrainLimit, limit)
	}
	require.Len(t, pendingCounts, 79)
	for _, count := range pendingCounts {
		assert.Equal(t, 1, count)
	}
	for _, size := range append(stagePages, provenancePages...) {
		assert.LessOrEqual(t, size, artifactImportDrainLimit)
	}
	require.Len(t, stagePages, 79)
	assert.Equal(t, 17, stagePages[len(stagePages)-1])
	assert.Equal(t, []int{1}, provenancePages)
	assert.Equal(t, 1, store.opens[KindCheckpoints])
	assert.Equal(t, 1, store.opens[KindManifests])
	assert.Equal(t, 1, store.opens[KindSegments])
	assert.Equal(t, 1, store.stats[KindManifests])
	assert.Equal(t, 1, store.stats[KindSegments])
	assert.Equal(t, 3, store.openOrigins[contractOrigin])
}

func TestStoreImportCoordinatorPagesLargeChangedCheckpointAcrossDrains(
	t *testing.T,
) {
	t.Parallel()

	const changedSessions = 300
	base := newTestArtifactStore(t)
	sessionMap := make(map[string]string, changedSessions)
	for i := range changedSessions {
		sessionMap[fmt.Sprintf("%s~missing-%03d", contractOrigin, i)] =
			fmt.Sprintf("%064x", i+1)
	}
	checkpointEntry := createImportTestCheckpoint(
		t, base, contractOrigin, 1, sessionMap,
	)
	store := &countingImportStore{ArtifactStore: base}
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	var attempts []int
	previousStats := 0
	for i := range 5 {
		result, err := coordinator.Finalize(t.Context())
		require.NoError(t, err)
		assert.Equal(t, i < 4, result.More)
		currentStats := store.stats[KindManifests]
		attempts = append(attempts, currentStats-previousStats)
		previousStats = currentStats
	}
	assert.Equal(t, []int{0, 0, 84, 128, 88}, attempts)
	assert.Equal(t, 1, store.opens[KindCheckpoints])
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	_, _, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	assert.False(t, found)
}

func TestStoreImportCoordinatorPreservesSignalsDuringActiveAttempt(
	t *testing.T,
) {
	t.Parallel()

	const sessionCount = 300
	store := newTestArtifactStore(t)
	segmentBody, err := encodeSegment([]db.Message{{
		Ordinal: 0, Role: "user", Content: "arrived",
	}})
	require.NoError(t, err)
	segmentHash := createHashedImportArtifact(
		t, store, KindSegments, ".ndjson", segmentBody,
	)
	arrivedManifest := importTestManifest("000-arrived")
	arrivedManifest.Session.MessageCount = 1
	arrivedManifest.Session.UserMessageCount = 1
	arrivedManifest.Segments = []string{segmentHash}
	arrivedBody, err := canonicalJSON(arrivedManifest)
	require.NoError(t, err)
	arrivedIdentity := identityForBytes(t, arrivedBody)
	sessionMap := make(map[string]string, sessionCount)
	sessionMap[contractOrigin+"~000-arrived"] = arrivedIdentity.SHA256
	for i := 1; i < sessionCount; i++ {
		sessionMap[fmt.Sprintf("%s~missing-%03d", contractOrigin, i)] =
			fmt.Sprintf("%064x", i+1)
	}
	checkpointEntry := createImportTestCheckpoint(
		t, store, contractOrigin, 1, sessionMap,
	)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(
		t.Context(), checkpointEntry,
	))

	for range 3 {
		result, err := coordinator.Finalize(t.Context())
		require.NoError(t, err)
		require.True(t, result.More)
	}
	arrivedRef := requireContractRef(
		t, contractOrigin, KindManifests,
		arrivedIdentity.SHA256+".json",
	)
	arrived := createContractArtifact(t, store, arrivedRef, arrivedBody)
	require.NoError(t, coordinator.RecordChanged(t.Context(), arrived.Entry))

	imported := 0
	for rounds := 0; ; rounds++ {
		require.Less(t, rounds, 10)
		result, err := coordinator.Finalize(t.Context())
		require.NoError(t, err)
		imported += result.Sessions
		if !result.More {
			break
		}
	}
	assert.Equal(t, 1, imported)
	session, err := destination.GetSessionFull(
		t.Context(), contractOrigin+"~000-arrived",
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, "project", session.Project)
}

func TestStoreImportCoordinatorPreservesSignalDuringCompletedPrune(
	t *testing.T,
) {
	t.Parallel()

	store := newTestArtifactStore(t)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	_, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	manifest := importTestManifest("signal")
	manifestHash := createImportTestManifest(t, store, manifest, false)
	manifestRef := requireContractRef(
		t, contractOrigin, KindManifests, manifestHash+".json",
	)
	manifestEntry, err := store.Stat(t.Context(), manifestRef)
	require.NoError(t, err)
	coordinator.hooks = &importCoordinatorHooks{
		afterPrune: func() error {
			coordinator.hooks.afterPrune = nil
			return coordinator.RecordChanged(t.Context(), manifestEntry)
		},
	}

	result, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.True(t, result.More)
}

func TestStoreImportCoordinatorRereadsCheckpointOnceAfterRestart(t *testing.T) {
	t.Parallel()

	const changedSessions = 300
	base := newTestArtifactStore(t)
	sessionMap := make(map[string]string, changedSessions)
	for i := range changedSessions {
		sessionMap[fmt.Sprintf("%s~missing-%03d", contractOrigin, i)] =
			fmt.Sprintf("%064x", i+1)
	}
	checkpointEntry := createImportTestCheckpoint(
		t, base, contractOrigin, 1, sessionMap,
	)
	store := &countingImportStore{ArtifactStore: base}
	destination := testDB(t)
	first := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, first.RecordChanged(t.Context(), checkpointEntry))

	result, err := first.Finalize(t.Context())
	require.NoError(t, err)
	require.True(t, result.More)
	require.Equal(t, 1, store.opens[KindCheckpoints])

	restarted := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	for rounds := 0; ; rounds++ {
		require.Less(t, rounds, 10)
		result, err = restarted.Finalize(t.Context())
		require.NoError(t, err)
		if !result.More {
			break
		}
	}
	assert.Equal(t, 2, store.opens[KindCheckpoints],
		"a restarted coordinator rereads once, then retains the verified body")
}

func TestStoreImportCoordinatorRecoversTerminalCheckpointPage(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	databasePath := filepath.Join(root, "archive.db")
	storeRoot := filepath.Join(root, "artifacts")
	store, err := newProtocolTestStore(storeRoot)
	require.NoError(t, err)
	destination, err := db.Open(databasePath)
	require.NoError(t, err)
	body, err := canonicalJSON(checkpoint{
		Version:  checkpointFormatVersion,
		Origin:   contractOrigin,
		Sequence: 1,
		Sessions: map[string]string{},
	})
	require.NoError(t, err)
	ref := requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	entry := createContractArtifact(t, store, ref, body).Entry
	coordinator := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), entry))

	_, sessions, err := decodeImportCheckpointHeader(
		body, contractOrigin, entry.Ref.Name,
	)
	require.NoError(t, err)
	page, nextOffset, done, err := decodeImportCheckpointSessionPage(
		sessions, contractOrigin, 0, artifactImportDrainLimit,
	)
	require.NoError(t, err)
	require.Empty(t, page)
	require.True(t, done)
	stage := db.ArtifactCheckpointLanding{
		Origin:           contractOrigin,
		Sequence:         1,
		CheckpointSHA256: entry.Identity.SHA256,
		CheckpointSize:   entry.Identity.Size,
	}
	require.NoError(t, destination.BeginArtifactCheckpointStage(
		t.Context(), stage, checkpointFormatVersion,
	))
	require.NoError(t, destination.StageArtifactCheckpointSessionPage(
		t.Context(), stage, nil, 0, nextOffset,
	))
	require.NoError(t, destination.Close())
	require.NoError(t, store.Close())

	destination, err = db.Open(databasePath)
	require.NoError(t, err)
	defer destination.Close()
	store, err = newProtocolTestStore(storeRoot)
	require.NoError(t, err)
	defer store.Close()
	restarted := NewStoreImportCoordinator(
		destination, store, importLocalOrigin,
	)
	result, err := restarted.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, result.Quarantined)
	complete, err := destination.ArtifactCheckpointStageComplete(
		t.Context(), stage,
	)
	require.NoError(t, err)
	assert.True(t, complete)
	landing, _, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 1, landing.Sequence)
	_, err = store.Stat(t.Context(), ref)
	require.NoError(t, err)
}

func TestStoreImportCoordinatorDoesNotDoubleImportSignals(t *testing.T) {
	t.Parallel()

	base := newTestArtifactStore(t)
	ordinal := 0
	m := importTestManifest("session")
	m.UsageEvents = []artifactUsageEvent{{
		MessageOrdinal: &ordinal,
		Source:         "provider",
		Model:          "model",
		DedupKey:       "usage",
	}}
	manifestHash := createImportTestClosure(t, base, &m, []db.Message{{
		Ordinal: 0, Role: "user", Content: "once",
	}})
	checkpoint := createImportTestCheckpoint(
		t, base, contractOrigin, 1,
		map[string]string{contractOrigin + "~session": manifestHash},
	)
	manifestRef := requireContractRef(
		t, contractOrigin, KindManifests, manifestHash+".json",
	)
	manifestEntry, err := base.Stat(t.Context(), manifestRef)
	require.NoError(t, err)
	segmentRef := requireContractRef(
		t, contractOrigin, KindSegments, m.Segments[0]+".ndjson",
	)
	segmentEntry, err := base.Stat(t.Context(), segmentRef)
	require.NoError(t, err)
	destination := testDB(t)
	coordinator := NewStoreImportCoordinator(
		destination, base, importLocalOrigin,
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), checkpoint))
	initialGeneration := coordinator.generation
	require.NoError(t, coordinator.RecordChanged(t.Context(), manifestEntry))
	require.NoError(t, coordinator.RecordChanged(t.Context(), segmentEntry))
	assert.Equal(t, initialGeneration+2, coordinator.generation)
	count, _, err := destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	require.NoError(t, coordinator.RecordChanged(t.Context(), checkpoint))
	count, _, err = destination.ArtifactImportQueueStats(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	first, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, first.Sessions)
	require.NoError(t, coordinator.RecordChanged(t.Context(), checkpoint))
	replay, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, replay.Sessions)

	higher := createImportTestCheckpoint(
		t, base, contractOrigin, 2,
		map[string]string{contractOrigin + "~session": manifestHash},
	)
	require.NoError(t, coordinator.RecordChanged(t.Context(), higher))
	unchanged, err := coordinator.Finalize(t.Context())
	require.NoError(t, err)
	assert.Zero(t, unchanged.Sessions)
	messages, err := destination.GetMessages(
		t.Context(), contractOrigin+"~session", 0, 10, true,
	)
	require.NoError(t, err)
	assert.Len(t, messages, 1)
	usage, err := destination.GetUsageEvents(
		t.Context(), contractOrigin+"~session",
	)
	require.NoError(t, err)
	assert.Len(t, usage, 1)
	landing, _, found, err := destination.GetArtifactCheckpointLanding(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, 2, landing.Sequence)
}

type countingImportStore struct {
	ArtifactStore
	opens       map[Kind]int
	stats       map[Kind]int
	openOrigins map[string]int
}

type failAfterQuarantineImportStore struct {
	ArtifactStore
	err    error
	failed bool
}

func (s *failAfterQuarantineImportStore) Quarantine(
	ctx context.Context, ref Ref, reason string,
) error {
	if err := s.ArtifactStore.Quarantine(ctx, ref, reason); err != nil {
		return err
	}
	if !s.failed {
		s.failed = true
		return s.err
	}
	return nil
}

func (s *countingImportStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if s.opens == nil {
		s.opens = make(map[Kind]int)
		s.openOrigins = make(map[string]int)
	}
	s.opens[ref.Kind]++
	s.openOrigins[ref.Origin]++
	return s.ArtifactStore.Open(ctx, ref)
}

func (s *countingImportStore) Stat(
	ctx context.Context, ref Ref,
) (Entry, error) {
	if s.stats == nil {
		s.stats = make(map[Kind]int)
	}
	s.stats[ref.Kind]++
	return s.ArtifactStore.Stat(ctx, ref)
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
