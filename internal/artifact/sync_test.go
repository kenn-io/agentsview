package artifact

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestArtifactSyncTwoNodeFolderRoundTripAndReplay(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	target := t.TempDir()
	databaseA := testDB(t)
	databaseB := testDB(t)
	repositoryA, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repositoryA.Close()) })
	repositoryB, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repositoryB.Close()) })

	originA := "laptop-a1b2c3"
	originB := "desktop-d4e5f6"
	seedSession(t, databaseA, "one", "alpha")

	published, err := SyncWithRepository(
		t.Context(),
		databaseA,
		repositoryA,
		SyncOptions{Target: target, Origin: originA},
	)
	require.NoError(err)
	assert.Equal(originA, published.Origin)
	assert.Equal(1, published.ExportedSessions)
	assert.Positive(published.PublishedArtifacts)
	assert.False(published.More)

	imported, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(err)
	assert.Equal(originB, imported.Origin)
	assert.Equal(1, imported.ImportedSessions)
	assert.Equal(2, imported.ImportedMessages)
	assert.Positive(imported.ReceivedArtifacts)
	assert.False(imported.More)

	session, err := databaseB.GetSessionFull(t.Context(), originA+"~one")
	require.NoError(err)
	require.NotNil(session)
	assert.Equal(originA, session.Machine)
	messages, err := databaseB.GetMessages(
		t.Context(),
		originA+"~one",
		0,
		10,
		true,
	)
	require.NoError(err)
	require.Len(messages, 2)
	assert.Equal("world", messages[1].Content)
	journalSequenceBeforeReplay := readTestFolderJournalSequence(t, target)

	replay, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(err)
	assert.Zero(replay.ImportedSessions)
	assert.Zero(replay.ImportedMessages)
	assert.False(replay.More)
	assert.Equal(
		journalSequenceBeforeReplay,
		readTestFolderJournalSequence(t, target),
		"an unchanged authoritative head must not replay its closure",
	)

	require.NoError(databaseA.ReplaceSessionMessages("one", []db.Message{
		{
			SessionID: "one", Ordinal: 0, Role: "user",
			Content: "updated prompt", ContentLength: 14,
		},
		{
			SessionID: "one", Ordinal: 1, Role: "assistant",
			Content: "updated response", ContentLength: 16,
		},
	}))
	updatedPublish, err := SyncWithRepository(
		t.Context(),
		databaseA,
		repositoryA,
		SyncOptions{Target: target, Origin: originA},
	)
	require.NoError(err)
	assert.Equal(1, updatedPublish.ExportedSessions)
	updatedImport, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(err)
	assert.Equal(1, updatedImport.ImportedSessions)
	updatedMessages, err := databaseB.GetMessages(
		t.Context(),
		originA+"~one",
		0,
		10,
		true,
	)
	require.NoError(err)
	require.Len(updatedMessages, 2)
	assert.Equal("updated response", updatedMessages[1].Content)
}

func TestArtifactSyncFullRepairRejournalsMissingObjectForAdvancedPeer(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	target := t.TempDir()
	databaseA := testDB(t)
	databaseB := testDB(t)
	repositoryA, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repositoryA.Close()) })
	repositoryB, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repositoryB.Close()) })
	originA := "laptop-a1b2c3"
	originB := "desktop-d4e5f6"
	seedSession(t, databaseA, "one", "alpha")
	opts := SyncOptions{Target: target, Origin: originA}

	initial, err := SyncWithRepository(t.Context(), databaseA, repositoryA, opts)
	require.NoError(err)
	assert.Positive(initial.PublishedArtifacts)
	segmentPaths, err := filepath.Glob(filepath.Join(
		target,
		originA,
		string(KindSegments),
		"*"+segmentExtension,
	))
	require.NoError(err)
	require.Len(segmentPaths, 1)
	segmentPath := segmentPaths[0]
	require.NoError(os.WriteFile(segmentPath, []byte("not zstd"), 0o600))

	advanced, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(err)
	assert.Zero(advanced.ImportedSessions)
	assert.NoFileExists(segmentPath)
	journalSequence := readTestFolderJournalSequence(t, target)

	noOp, err := SyncWithRepository(t.Context(), databaseA, repositoryA, opts)
	require.NoError(err)
	assert.Zero(noOp.PublishedArtifacts)
	assert.NoFileExists(segmentPath)

	opts.Full = true
	repaired, err := SyncWithRepository(t.Context(), databaseA, repositoryA, opts)
	require.NoError(err)
	assert.Equal(1, repaired.PublishedArtifacts)
	assert.FileExists(segmentPath)
	assert.Equal(journalSequence+1, readTestFolderJournalSequence(t, target))

	imported, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(err)
	assert.Equal(1, imported.ImportedSessions)
	assert.Equal(2, imported.ImportedMessages)
}

func readTestFolderJournalSequence(t *testing.T, target string) int64 {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join(target, folderJournalDirectory))
	require.NoError(t, err)
	head, err := readFolderJournalHead(root)
	require.NoError(t, err)
	require.NoError(t, root.Close())
	return head.Sequence
}

func TestArtifactSyncDoesNotIngestOrRepublishSpoofedLocalOrigin(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	targetA := t.TempDir()
	transportA, err := OpenFolderTransport(targetA, FolderTransportOptions{})
	require.NoError(err)
	require.NoError(transportA.Close())

	localOrigin := "local-d4e5f6"
	spoofedBody := []byte("{\"content\":\"spoofed local session\"}\n")
	spoofedRef := testContentRef(
		t,
		localOrigin,
		KindSegments,
		spoofedBody,
		".ndjson",
	)
	writeFolderWire(t, targetA, spoofedRef, spoofedBody)

	database := testDB(t)
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repository.Close()) })
	opts := SyncOptions{Origin: localOrigin}

	opts.Target = targetA
	_, err = SyncWithRepository(t.Context(), database, repository, opts)
	require.NoError(err)
	_, err = repository.Content().Stat(t.Context(), spoofedRef)
	assert.ErrorIs(err, ErrArtifactNotFound)

	targetB := t.TempDir()
	opts.Target = targetB
	_, err = SyncWithRepository(t.Context(), database, repository, opts)
	require.NoError(err)

	spoofedWire, err := ToWireRef(spoofedRef)
	require.NoError(err)
	assert.NoFileExists(filepath.Join(
		targetB,
		spoofedWire.Origin,
		string(spoofedWire.Kind),
		spoofedWire.Name,
	))
}

func TestArtifactSyncDoesNotPublishUnrecordedLocalOriginObjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	origin := "local-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "owned", "alpha")
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repository.Close()) })

	unrecordedBody := []byte("{\"content\":\"unrecorded local object\"}\n")
	unrecordedRef := testContentRef(
		t,
		origin,
		KindSegments,
		unrecordedBody,
		".ndjson",
	)
	createTestStoreArtifact(
		t,
		repository.Content(),
		unrecordedRef,
		unrecordedBody,
	)

	target := t.TempDir()
	result, err := SyncWithRepository(
		t.Context(),
		database,
		repository,
		SyncOptions{Target: target, Origin: origin},
	)

	require.NoError(err)
	assert.Equal(1, result.ExportedSessions)
	assert.Positive(result.PublishedArtifacts)
	unrecordedWire, err := ToWireRef(unrecordedRef)
	require.NoError(err)
	assert.NoFileExists(filepath.Join(
		target,
		unrecordedWire.Origin,
		string(unrecordedWire.Kind),
		unrecordedWire.Name,
	))
}

func TestArtifactSyncValidatesBeforeCreatingOwnedStorage(t *testing.T) {
	t.Parallel()

	t.Run("missing target", func(t *testing.T) {
		dataDir := t.TempDir()
		_, err := Sync(
			t.Context(),
			testDB(t),
			SyncOptions{DataDir: dataDir, Origin: "local-a1b2c3"},
		)
		require.ErrorIs(t, err, ErrArtifactInvalid)
		assert.NoDirExists(t, filepath.Join(dataDir, repositoryDirectory))
	})

	t.Run("invalid origin", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)

		target := filepath.Join(t.TempDir(), "target")
		repository, err := OpenRepository(t.Context(), t.TempDir())
		require.NoError(err)
		t.Cleanup(func() { require.NoError(repository.Close()) })

		_, err = SyncWithRepository(
			t.Context(),
			testDB(t),
			repository,
			SyncOptions{Target: target, Origin: "BAD"},
		)
		require.Error(err)
		assert.Contains(err.Error(), "invalid artifact origin")
		assert.NoDirExists(target)
	})
}

func TestArtifactSyncQuarantinesInvalidCheckpointInFolderAndDocbank(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(err)
	require.NoError(transport.Close())

	origin := "peer-a1b2c3"
	ref, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(err)
	writeFolderWire(t, target, ref, []byte(`{"v":1}`))
	wire, err := ToWireRef(ref)
	require.NoError(err)
	wirePath := filepath.Join(target, origin, string(KindCheckpoints), wire.Name)

	database := testDB(t)
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repository.Close()) })
	result, err := SyncWithRepository(
		t.Context(),
		database,
		repository,
		SyncOptions{Target: target, Origin: "local-d4e5f6"},
	)
	require.NoError(err)
	assert.Equal(1, result.Quarantined)
	assert.NoFileExists(wirePath)
	quarantined, err := filepath.Glob(wirePath + folderCorruptSeparator + "*")
	require.NoError(err)
	require.Len(quarantined, 1)
	_, err = repository.Content().Stat(t.Context(), ref)
	assert.ErrorIs(err, ErrArtifactNotFound)
}

func TestArtifactSyncCountsTransportQuarantine(t *testing.T) {
	require := require.New(t)

	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(err)
	require.NoError(transport.Close())

	origin := "peer-a1b2c3"
	body := []byte(`{"v":2}`)
	ref := testContentRef(t, origin, KindManifests, body, ".json")
	wire, err := ToWireRef(ref)
	require.NoError(err)
	wireDirectory := filepath.Join(target, origin, string(KindManifests))
	require.NoError(os.MkdirAll(wireDirectory, 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(wireDirectory, wire.Name),
		[]byte("not zstd"),
		0o600,
	))
	appendFolderJournalTestEntry(t, target, Entry{
		Ref: ref, Identity: identityForBytes(t, body),
	})

	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repository.Close()) })
	result, err := SyncWithRepository(
		t.Context(),
		testDB(t),
		repository,
		SyncOptions{Target: target, Origin: "local-d4e5f6"},
	)
	require.NoError(err)
	assert.Equal(t, 1, result.Quarantined)
}

func TestCoordinatedQuarantineDropsStaleLocalAfterRemoteReplacement(
	t *testing.T,
) {
	require := require.New(t)

	t.Parallel()

	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(repository.Close()) })
	ref, err := NewRef(
		"peer-a1b2c3",
		KindCheckpoints,
		"cp-0000000001.json",
	)
	require.NoError(err)
	body := []byte(`{"v":1}`)
	_, err = repository.Content().Create(
		t.Context(),
		ref,
		identityForBytes(t, body),
		canonicalArtifactMediaType(ref.Kind),
		bytes.NewReader(body),
	)
	require.NoError(err)
	store := &coordinatedTransportStore{
		ArtifactStore: repository.Content(),
		quarantine:    replacementQuarantineTransport{},
	}

	err = store.Quarantine(t.Context(), ref, "invalid checkpoint")

	require.NoError(err)
	_, err = repository.Content().Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
}

type replacementQuarantineTransport struct{}

func (replacementQuarantineTransport) Prepare(
	context.Context,
	ArtifactStore,
) error {
	return nil
}

func (replacementQuarantineTransport) Exchange(
	context.Context,
	ArtifactStore,
	string,
) (ExchangeResult, error) {
	return ExchangeResult{}, nil
}

func (replacementQuarantineTransport) Close() error {
	return nil
}

func (replacementQuarantineTransport) QuarantineTransportArtifact(
	context.Context,
	Ref,
	Identity,
) error {
	return ErrArtifactConflict
}

func TestDrainArtifactSyncExportsReturnsMoreAtRoundBudget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Parallel()

	database := testDB(t)
	origin := "local-a1b2c3"
	require.NoError(AdoptOrigin(database, origin))
	for index := range artifactExportBatchSize + 1 {
		seedSession(t, database, fmt.Sprintf("session-%03d", index), "alpha")
	}
	store := newTestArtifactStore(t)

	result, more, err := drainArtifactSyncExportsWithRounds(
		t.Context(),
		database,
		store,
		origin,
		false,
		1,
	)
	require.NoError(err)
	assert.True(more)
	assert.Equal(artifactExportBatchSize, result.ExportedSessions)
	pending, err := database.CountPendingArtifactExports(t.Context())
	require.NoError(err)
	assert.Equal(1, pending)
}

func TestDrainArtifactSyncFullExportReturnsMoreWhenQueueDoesNotSettle(
	t *testing.T,
) {
	assert := assert.New(t)

	// Serial: this exercises the full drain cap through real SQLite and
	// Docbank writes under a fixed deadline. The race job runs other packages
	// concurrently, so leave enough headroom for cross-package I/O contention
	// while retaining a guard against a stuck drain.

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	store := newTestArtifactStore(t)
	concurrent := &reEnqueueOnBoundaryStore{DB: database}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	result, more, err := drainArtifactSyncExportsWithRounds(
		ctx,
		concurrent,
		store,
		contractOrigin,
		true,
		1,
	)
	require.NoError(t, err)
	assert.True(more)
	assert.Positive(result.ExportedSessions)
	assert.Positive(concurrent.round)
}

type repeatingImportFinalizer struct {
	calls int
}

func (f *repeatingImportFinalizer) Finalize(context.Context) (ImportResult, error) {
	f.calls++
	return ImportResult{Sessions: 1, Messages: 2, More: true}, nil
}

func TestDrainArtifactSyncImportsReturnsMoreAtRoundBudget(t *testing.T) {
	assert := assert.New(t)

	t.Parallel()

	finalizer := &repeatingImportFinalizer{}
	var result SyncResult

	more, err := drainArtifactSyncImportsWithRounds(
		t.Context(),
		finalizer,
		&result,
		3,
	)
	require.NoError(t, err)
	assert.True(more)
	assert.Equal(3, finalizer.calls)
	assert.Equal(3, result.ImportedSessions)
	assert.Equal(6, result.ImportedMessages)
}
