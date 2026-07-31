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
	t.Parallel()

	target := t.TempDir()
	databaseA := testDB(t)
	databaseB := testDB(t)
	repositoryA, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repositoryA.Close()) })
	repositoryB, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repositoryB.Close()) })

	originA := "laptop-a1b2c3"
	originB := "desktop-d4e5f6"
	seedSession(t, databaseA, "one", "alpha")

	published, err := SyncWithRepository(
		t.Context(),
		databaseA,
		repositoryA,
		SyncOptions{Target: target, Origin: originA},
	)
	require.NoError(t, err)
	assert.Equal(t, originA, published.Origin)
	assert.Equal(t, 1, published.ExportedSessions)
	assert.Positive(t, published.PublishedArtifacts)
	assert.False(t, published.More)

	imported, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(t, err)
	assert.Equal(t, originB, imported.Origin)
	assert.Equal(t, 1, imported.ImportedSessions)
	assert.Equal(t, 2, imported.ImportedMessages)
	assert.Positive(t, imported.ReceivedArtifacts)
	assert.False(t, imported.More)

	session, err := databaseB.GetSessionFull(t.Context(), originA+"~one")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, originA, session.Machine)
	messages, err := databaseB.GetMessages(
		t.Context(),
		originA+"~one",
		0,
		10,
		true,
	)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, "world", messages[1].Content)
	journalSequenceBeforeReplay := readTestFolderJournalSequence(t, target)

	replay, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(t, err)
	assert.Zero(t, replay.ImportedSessions)
	assert.Zero(t, replay.ImportedMessages)
	assert.False(t, replay.More)
	assert.Equal(
		t,
		journalSequenceBeforeReplay,
		readTestFolderJournalSequence(t, target),
		"an unchanged authoritative head must not replay its closure",
	)

	require.NoError(t, databaseA.ReplaceSessionMessages("one", []db.Message{
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
	require.NoError(t, err)
	assert.Equal(t, 1, updatedPublish.ExportedSessions)
	updatedImport, err := SyncWithRepository(
		t.Context(),
		databaseB,
		repositoryB,
		SyncOptions{Target: target, Origin: originB},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, updatedImport.ImportedSessions)
	updatedMessages, err := databaseB.GetMessages(
		t.Context(),
		originA+"~one",
		0,
		10,
		true,
	)
	require.NoError(t, err)
	require.Len(t, updatedMessages, 2)
	assert.Equal(t, "updated response", updatedMessages[1].Content)
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
	t.Parallel()

	targetA := t.TempDir()
	transportA, err := OpenFolderTransport(targetA, FolderTransportOptions{})
	require.NoError(t, err)
	require.NoError(t, transportA.Close())

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
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	opts := SyncOptions{Origin: localOrigin}

	opts.Target = targetA
	_, err = SyncWithRepository(t.Context(), database, repository, opts)
	require.NoError(t, err)
	_, err = repository.Content().Stat(t.Context(), spoofedRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound)

	targetB := t.TempDir()
	opts.Target = targetB
	_, err = SyncWithRepository(t.Context(), database, repository, opts)
	require.NoError(t, err)

	spoofedWire, err := ToWireRef(spoofedRef)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(
		targetB,
		spoofedWire.Origin,
		string(spoofedWire.Kind),
		spoofedWire.Name,
	))
}

func TestArtifactSyncDoesNotPublishUnrecordedLocalOriginObjects(t *testing.T) {
	t.Parallel()

	origin := "local-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "owned", "alpha")
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

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

	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.Positive(t, result.PublishedArtifacts)
	unrecordedWire, err := ToWireRef(unrecordedRef)
	require.NoError(t, err)
	assert.NoFileExists(t, filepath.Join(
		target,
		unrecordedWire.Origin,
		string(unrecordedWire.Kind),
		unrecordedWire.Name,
	))
}

func TestArtifactSyncValidatesBeforeCreatingOwnedStorage(t *testing.T) {
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
		target := filepath.Join(t.TempDir(), "target")
		repository, err := OpenRepository(t.Context(), t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, repository.Close()) })

		_, err = SyncWithRepository(
			t.Context(),
			testDB(t),
			repository,
			SyncOptions{Target: target, Origin: "BAD"},
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid artifact origin")
		assert.NoDirExists(t, target)
	})
}

func TestArtifactSyncQuarantinesInvalidCheckpointInFolderAndDocbank(
	t *testing.T,
) {
	t.Parallel()

	target := t.TempDir()
	transport, err := OpenFolderTransport(target, FolderTransportOptions{})
	require.NoError(t, err)
	require.NoError(t, transport.Close())

	origin := "peer-a1b2c3"
	ref, err := NewRef(origin, KindCheckpoints, "cp-0000000001.json")
	require.NoError(t, err)
	writeFolderWire(t, target, ref, []byte(`{"v":1}`))
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	wirePath := filepath.Join(target, origin, string(KindCheckpoints), wire.Name)

	database := testDB(t)
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	result, err := SyncWithRepository(
		t.Context(),
		database,
		repository,
		SyncOptions{Target: target, Origin: "local-d4e5f6"},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Quarantined)
	assert.NoFileExists(t, wirePath)
	quarantined, err := filepath.Glob(wirePath + folderCorruptSeparator + "*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	_, err = repository.Content().Stat(t.Context(), ref)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
}

func TestCoordinatedQuarantineDropsStaleLocalAfterRemoteReplacement(
	t *testing.T,
) {
	t.Parallel()

	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	ref, err := NewRef(
		"peer-a1b2c3",
		KindCheckpoints,
		"cp-0000000001.json",
	)
	require.NoError(t, err)
	body := []byte(`{"v":1}`)
	_, err = repository.Content().Create(
		t.Context(),
		ref,
		identityForBytes(t, body),
		canonicalArtifactMediaType(ref.Kind),
		bytes.NewReader(body),
	)
	require.NoError(t, err)
	store := &coordinatedTransportStore{
		ArtifactStore: repository.Content(),
		quarantine:    replacementQuarantineTransport{},
	}

	err = store.Quarantine(t.Context(), ref, "invalid checkpoint")

	require.NoError(t, err)
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
	database := testDB(t)
	origin := "local-a1b2c3"
	require.NoError(t, AdoptOrigin(database, origin))
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
	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, artifactExportBatchSize, result.ExportedSessions)
	pending, err := database.CountPendingArtifactExports(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}

func TestDrainArtifactSyncFullExportReturnsMoreWhenQueueDoesNotSettle(
	t *testing.T,
) {
	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	store := newTestArtifactStore(t)
	concurrent := &reEnqueueOnBoundaryStore{DB: database}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
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
	assert.True(t, more)
	assert.Positive(t, result.ExportedSessions)
	assert.Positive(t, concurrent.round)
}

type repeatingImportFinalizer struct {
	calls int
}

func (f *repeatingImportFinalizer) Finalize(context.Context) (ImportResult, error) {
	f.calls++
	return ImportResult{Sessions: 1, Messages: 2, More: true}, nil
}

func TestDrainArtifactSyncImportsReturnsMoreAtRoundBudget(t *testing.T) {
	finalizer := &repeatingImportFinalizer{}
	var result SyncResult

	more, err := drainArtifactSyncImportsWithRounds(
		t.Context(),
		finalizer,
		&result,
		3,
	)
	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, 3, finalizer.calls)
	assert.Equal(t, 3, result.ImportedSessions)
	assert.Equal(t, 6, result.ImportedMessages)
}
