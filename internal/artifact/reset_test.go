package artifact

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/docbank"
	docsqlite "go.kenn.io/docbank/pkg/sqlite"
	"go.kenn.io/docbank/pkg/sqlite/modernc"
)

type cancelAfterMetadataCreateStore struct {
	ArtifactStore
	cancel    context.CancelFunc
	remaining int
}

type resetRecoveryObservingTransport struct {
	database  *db.DB
	origin    string
	exchanges int
}

func (t *resetRecoveryObservingTransport) Prepare(context.Context, ArtifactStore) error {
	return nil
}

func (t *resetRecoveryObservingTransport) Exchange(ctx context.Context, store ArtifactStore) error {
	t.exchanges++
	_, pending, err := t.database.ArtifactResetRepublishPending(ctx)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("transport exchange observed pending reset recovery")
	}
	page, err := firstStoreEntryPage(ctx, store, t.origin, KindMeta, 10)
	if err != nil {
		return err
	}
	if len(page.Items) != 1 {
		return fmt.Errorf("transport exchange observed %d metadata events, want 1", len(page.Items))
	}
	return nil
}

func (s *cancelAfterMetadataCreateStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	result, err := s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
	if err == nil && ref.Kind == KindMeta && s.remaining > 0 {
		s.remaining--
		if s.remaining == 0 {
			s.cancel()
		}
	}
	return result, err
}

func TestArtifactResetCorruptCatalogNormalStartupFailsClosed(t *testing.T) {
	dataDir := t.TempDir()
	database, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	seedSession(t, database, "local-session", "project-a")
	database.Close()

	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	require.NoError(t, repository.Close())
	catalog := filepath.Join(dataDir, repositoryDirectory, "docbank.db")
	require.NoError(t, os.WriteFile(catalog, []byte("corrupt docbank catalog"), 0o600))
	before := snapshotDirectory(t, dataDir)

	reopened, err := openRepository(t.Context(), dataDir, modernc.Driver{})

	assert.Nil(t, reopened)
	require.Error(t, err)
	assert.Equal(t, before, snapshotDirectory(t, dataDir))
}

func TestArtifactResetStoppedDaemonMovesAsideAndRepublishesFromSQLiteFloor(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	seedSession(t, database, "local-session", "project-a")
	localDisplayName := "Recovered local title"
	require.NoError(t, database.RenameSession("local-session", &localDisplayName))
	origin := "desktop-d4e5f6"
	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	first, err := ExportToStore(t.Context(), database, repository.Content(), ExportOptions{
		Origin: origin, Full: true,
	})
	require.NoError(t, err)
	foreignBody := []byte("foreign relay bytes")
	foreignRef := requireContractRef(t, "peer-a1b2c3", KindRaw, hashHex(foreignBody))
	_, err = repository.Content().Create(t.Context(), foreignRef, identityForBytes(t, foreignBody),
		canonicalArtifactMediaType(KindRaw), bytes.NewReader(foreignBody))
	require.NoError(t, err)
	require.NoError(t, repository.Close())

	fresh, result, err := resetRepositoryWith(
		t.Context(), dataDir, database, origin, nil,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })

	wantRoot, err := canonicalRepositoryRoot(dataDir)
	require.NoError(t, err)
	assert.Equal(t, wantRoot, result.VaultRoot)
	assert.Equal(t, result.VaultRoot+".reset-20260722T123456.123456789Z", result.DiagnosticRoot)
	assert.DirExists(t, result.DiagnosticRoot)
	assert.GreaterOrEqual(t, result.Export.CheckpointSequence, first.CheckpointSequence)
	assert.Equal(t, []string{origin}, listAllContractOrigins(t, fresh.Content(), 10))
	assert.NotEmpty(t, listAllContractEntries(t, fresh.Content(), origin, KindMeta, 10),
		"reset must publish local curation that existed only in SQLite")
	_, err = fresh.Content().Stat(t.Context(), foreignRef)
	require.ErrorIs(t, err, ErrArtifactNotFound)
	replayDB := testDB(t)
	replay, err := importResultFromTestStore(t.Context(), replayDB, fresh.Content(), "receiver-a1b2c3")
	require.NoError(t, err)
	assert.Positive(t, replay.Metadata)
	replayed, err := replayDB.GetSession(t.Context(), origin+"~local-session")
	require.NoError(t, err)
	require.NotNil(t, replayed)
	require.NotNil(t, replayed.DisplayName)
	assert.Equal(t, localDisplayName, *replayed.DisplayName)

	diagnostic, err := docbank.New(t.Context(), docbank.Config{
		Root: result.DiagnosticRoot, SQLite: modernc.Driver{},
	})
	require.NoError(t, err)
	diagnosticStore := newDocbankContent(diagnostic)
	t.Cleanup(func() { require.NoError(t, diagnosticStore.Close()) })
	_, err = diagnosticStore.Stat(t.Context(), foreignRef)
	require.NoError(t, err)
}

func TestArtifactResetRematerializesCoveredLocalCurationBeforeFreshCheckpoint(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	origin := "desktop-d4e5f6"
	seedSession(t, database, "local-session", "project-a")
	require.NoError(t, database.ReplaceSessionMessages("local-session", []db.Message{
		{SessionID: "local-session", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, SourceUUID: "uuid-question"},
		{SessionID: "local-session", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5, SourceUUID: "uuid-answer"},
	}))

	bootstrap, err := openRepository(ctx, t.TempDir(), modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bootstrap.Close()) })
	_, err = ExportToStore(ctx, database, bootstrap.Content(), ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	displayName := "Recovered curated title"
	require.NoError(t, database.RenameSession("local-session", &displayName))
	starred, err := database.StarSession("local-session")
	require.NoError(t, err)
	assert.True(t, starred)
	messages, err := database.GetAllMessages(ctx, "local-session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	note := "keep this answer"
	_, err = database.PinMessage("local-session", messages[1].ID, &note)
	require.NoError(t, err)
	require.NoError(t, database.SoftDeleteSession("local-session"))

	current, err := openRepository(ctx, dataDir, modernc.Driver{})
	require.NoError(t, err)
	recorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: origin,
		Store:  current.Content(),
	})
	written, err := recorder.AppendBaseline(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, written)

	materializedBeforeExport := false
	fresh, _, err := resetRepositoryWith(
		ctx, dataDir, database, origin, current,
		repositoryResetHooks{
			now: fixedArtifactResetTime,
			export: func(
				ctx context.Context, database *db.DB, store ArtifactStore, opts ExportOptions,
			) (ExportResult, error) {
				materializedBeforeExport = len(listAllContractEntries(
					t, store, origin, KindMeta, 10,
				)) == 4
				return ExportToStore(ctx, database, store, opts)
			},
		},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })
	assert.True(t, materializedBeforeExport,
		"fresh metadata must exist before ExportToStore creates the checkpoint")

	replayDB := testDB(t)
	_, err = importResultFromTestStore(ctx, replayDB, bootstrap.Content(), "receiver-a1b2c3")
	require.NoError(t, err)
	replayed, err := replayDB.GetSessionFull(ctx, origin+"~local-session")
	require.NoError(t, err)
	require.NotNil(t, replayed)
	require.NotNil(t, replayed.DisplayName)
	assert.NotEqual(t, displayName, *replayed.DisplayName)

	replayedResult, err := importResultFromTestStore(
		ctx, replayDB, fresh.Content(), "receiver-a1b2c3",
	)
	require.NoError(t, err)
	assert.Equal(t, 4, replayedResult.Metadata)
	replayed, err = replayDB.GetSessionFull(ctx, origin+"~local-session")
	require.NoError(t, err)
	require.NotNil(t, replayed)
	require.NotNil(t, replayed.DisplayName)
	assert.Equal(t, displayName, *replayed.DisplayName)
	require.NotNil(t, replayed.DeletedAt)
	stars, err := replayDB.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{origin + "~local-session"}, stars)
	pins, err := replayDB.ListPinnedMessages(ctx, origin+"~local-session", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, 1, pins[0].Ordinal)
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, note, *pins[0].Note)
}

func TestArtifactResetRematerializesLocalNegativeWinnersWithoutReattributingForeign(t *testing.T) {
	ctx := t.Context()
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	seedSession(t, database, "cleared-session", "project-a")
	seedSession(t, database, "foreign-session", "project-a")
	require.NoError(t, database.ReplaceSessionMessages("cleared-session", []db.Message{
		{SessionID: "cleared-session", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5, SourceUUID: "uuid-question"},
		{SessionID: "cleared-session", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5, SourceUUID: "uuid-answer"},
	}))

	bootstrap, err := openRepository(ctx, t.TempDir(), modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bootstrap.Close()) })
	_, err = ExportToStore(ctx, database, bootstrap.Content(), ExportOptions{
		Origin: localOrigin,
		Full:   true,
	})
	require.NoError(t, err)

	current, err := openRepository(ctx, dataDir, modernc.Driver{})
	require.NoError(t, err)
	localRecorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  current.Content(),
		Now:    fixedHLCTime,
	})
	staleName := "stale local title"
	require.NoError(t, database.RenameSession("cleared-session", &staleName))
	renameValue, err := metadataRenameValue(&staleName)
	require.NoError(t, err)
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpRename, Value: renameValue,
	})
	require.NoError(t, err)
	_, err = database.StarSession("cleared-session")
	require.NoError(t, err)
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpStar,
	})
	require.NoError(t, err)
	messages, err := database.GetAllMessages(ctx, "cleared-session")
	require.NoError(t, err)
	require.Len(t, messages, 2)
	note := "stale pin"
	_, err = database.PinMessage("cleared-session", messages[1].ID, &note)
	require.NoError(t, err)
	pin := &MetadataPin{SourceUUID: "uuid-answer", Ordinal: 1, Note: &note}
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpPin, Pin: pin,
	})
	require.NoError(t, err)
	require.NoError(t, database.SoftDeleteSession("cleared-session"))
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpSoftDelete,
	})
	require.NoError(t, err)

	require.NoError(t, database.RenameSession("cleared-session", nil))
	renameValue, err = metadataRenameValue(nil)
	require.NoError(t, err)
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpRename, Value: renameValue,
	})
	require.NoError(t, err)
	_, err = database.UnstarSession("cleared-session")
	require.NoError(t, err)
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpUnstar,
	})
	require.NoError(t, err)
	require.NoError(t, database.UnpinMessage("cleared-session", messages[1].ID))
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpUnpin, Pin: pin,
	})
	require.NoError(t, err)
	_, err = database.RestoreSession("cleared-session")
	require.NoError(t, err)
	_, err = localRecorder.Append(ctx, MetadataEventInput{
		SessionID: "cleared-session", Op: MetadataOpRestore,
	})
	require.NoError(t, err)

	peerDB := testDB(t)
	peerRepository, err := openRepository(ctx, t.TempDir(), modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, peerRepository.Close()) })
	peerRecorder := NewMetadataRecorder(peerDB, MetadataRecorderOptions{
		Origin: peerOrigin,
		Store:  peerRepository.Content(),
		Now:    func() time.Time { return fixedHLCTime().Add(time.Hour) },
	})
	peerName := "peer-authored winner"
	peerValue, err := metadataRenameValue(&peerName)
	require.NoError(t, err)
	_, err = peerRecorder.Append(ctx, MetadataEventInput{
		SessionID: localOrigin + "~foreign-session", Op: MetadataOpRename, Value: peerValue,
	})
	require.NoError(t, err)
	_, err = importResultFromTestStore(ctx, database, peerRepository.Content(), localOrigin)
	require.NoError(t, err)

	fresh, _, err := resetRepositoryWith(
		ctx, dataDir, database, localOrigin, current,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })

	metadataEntries := listAllContractEntries(t, fresh.Content(), localOrigin, KindMeta, 10)
	require.Len(t, metadataEntries, 4)
	wantOps := map[string]bool{
		MetadataOpRename:  true,
		MetadataOpUnstar:  true,
		MetadataOpUnpin:   true,
		MetadataOpRestore: true,
	}
	for _, entry := range metadataEntries {
		data := readContractArtifact(t, fresh.Content(), entry.Ref)
		var event metadataEvent
		require.NoError(t, json.Unmarshal(data, &event))
		assert.Equal(t, localOrigin+"~cleared-session", event.SessionGID)
		assert.True(t, wantOps[event.Op], "unexpected materialized op %q", event.Op)
		delete(wantOps, event.Op)
	}
	assert.Empty(t, wantOps)

	replayDB := testDB(t)
	_, err = importResultFromTestStore(ctx, replayDB, bootstrap.Content(), "receiver-a1b2c3")
	require.NoError(t, err)
	require.NoError(t, replayDB.RenameSession(localOrigin+"~cleared-session", &staleName))
	_, err = replayDB.StarSession(localOrigin + "~cleared-session")
	require.NoError(t, err)
	replayMessages, err := replayDB.GetAllMessages(ctx, localOrigin+"~cleared-session")
	require.NoError(t, err)
	require.Len(t, replayMessages, 2)
	_, err = replayDB.PinMessage(localOrigin+"~cleared-session", replayMessages[1].ID, &note)
	require.NoError(t, err)
	require.NoError(t, replayDB.SoftDeleteSession(localOrigin+"~cleared-session"))

	result, err := importResultFromTestStore(ctx, replayDB, fresh.Content(), "receiver-a1b2c3")
	require.NoError(t, err)
	assert.Equal(t, 4, result.Metadata)
	replayed, err := replayDB.GetSessionFull(ctx, localOrigin+"~cleared-session")
	require.NoError(t, err)
	require.NotNil(t, replayed)
	assert.Nil(t, replayed.DeletedAt)
	require.NotNil(t, replayed.DisplayName)
	assert.NotEqual(t, staleName, *replayed.DisplayName)
	stars, err := replayDB.ListStarredSessionIDs(ctx)
	require.NoError(t, err)
	assert.Empty(t, stars)
	pins, err := replayDB.ListPinnedMessages(ctx, localOrigin+"~cleared-session", "")
	require.NoError(t, err)
	assert.Empty(t, pins)
}

func TestArtifactResetRepublishCrashAcrossWinnerPageIsIdempotent(t *testing.T) {
	const winnerPageSize = 128
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	localOrigin := "desktop-d4e5f6"
	peerOrigin := "laptop-a1b2c3"
	for index := range winnerPageSize + 1 {
		recordResetMetadataWinner(
			t, database, localOrigin,
			fmt.Sprintf("%s~session-%03d", localOrigin, index),
			MetadataOpUnstar, index,
		)
	}
	recordResetMetadataWinner(
		t, database, peerOrigin, localOrigin+"~foreign-session",
		MetadataOpRestore, winnerPageSize+2,
	)

	current, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	fresh, _, err := beginRepositoryResetWith(
		t.Context(), dataDir, localOrigin, current,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	require.NoError(t, err)

	firstCtx, cancel := context.WithCancel(t.Context())
	interruptingStore := &cancelAfterMetadataCreateStore{
		ArtifactStore: fresh.Content(),
		cancel:        cancel,
		remaining:     winnerPageSize + 1,
	}
	firstRecorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  interruptingStore,
	})
	baselineHLC := HLCTimestamp{WallTime: fixedHLCTime()}.String()
	_, err = firstRecorder.materializeCurrentStateAtHLC(firstCtx, baselineHLC)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, fresh.Close())

	reopened, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	retryRecorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  reopened.Content(),
	})
	_, err = retryRecorder.materializeCurrentStateAtHLC(t.Context(), baselineHLC)
	require.NoError(t, err)

	entries := listAllContractEntries(t, reopened.Content(), localOrigin, KindMeta, 64)
	assert.Len(t, entries, winnerPageSize+1,
		"retry must immutable-create the same winner artifacts instead of appending replacements")
	assert.Empty(t, listAllContractEntries(t, reopened.Content(), peerOrigin, KindMeta, 64),
		"reset recovery must not reauthor foreign winners")
}

func TestArtifactResetRepublishCrashAfterBaselineCreateDoesNotGrowEvents(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	seedSession(t, database, "local-session", "project-a")
	_, err := database.StarSession("local-session")
	require.NoError(t, err)
	localOrigin := "desktop-d4e5f6"
	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)

	firstCtx, cancel := context.WithCancel(t.Context())
	interruptingStore := &cancelAfterMetadataCreateStore{
		ArtifactStore: repository.Content(), cancel: cancel, remaining: 1,
	}
	firstRecorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  interruptingStore,
		Now:    fixedHLCTime,
	})
	baselineHLC := HLCTimestamp{WallTime: fixedHLCTime()}.String()
	_, err = firstRecorder.materializeCurrentStateAtHLC(firstCtx, baselineHLC)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, repository.Close())

	reopened, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	retryRecorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: localOrigin,
		Store:  reopened.Content(),
		Now:    fixedHLCTime,
	})
	_, err = retryRecorder.materializeCurrentStateAtHLC(t.Context(), baselineHLC)
	require.NoError(t, err)

	entries := listAllContractEntries(t, reopened.Content(), localOrigin, KindMeta, 10)
	assert.Len(t, entries, 1,
		"retry after Create-before-projection crash must reuse one deterministic baseline event")
}

func TestArtifactResetRepublishCrashAcrossUncoveredBaselinePageIsIdempotent(t *testing.T) {
	const baselineRows = 129
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	origin := "desktop-d4e5f6"
	for index := range baselineRows {
		sessionID := fmt.Sprintf("session-%03d", index)
		seedSession(t, database, sessionID, "project-a")
		_, err := database.StarSession(sessionID)
		require.NoError(t, err)
	}
	pending, err := PrepareRepositoryResetRepublish(t.Context(), database, dataDir, origin)
	require.NoError(t, err)
	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)

	firstCtx, cancel := context.WithCancel(t.Context())
	interruptingStore := &cancelAfterMetadataCreateStore{
		ArtifactStore: repository.Content(), cancel: cancel, remaining: 128,
	}
	firstRecorder := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: origin,
		Store:  interruptingStore,
	})
	_, err = firstRecorder.materializeCurrentStateAtHLC(firstCtx, pending.BaselineHLC)
	require.ErrorIs(t, err, context.Canceled)
	_, markerFound, err := database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.True(t, markerFound)
	require.NoError(t, repository.Close())

	reopened, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	_, recovered, err := RecoverRepositoryResetRepublish(
		t.Context(), database, reopened, origin,
	)
	require.NoError(t, err)
	assert.True(t, recovered)
	assert.Len(t, listAllContractEntries(t, reopened.Content(), origin, KindMeta, 64), baselineRows,
		"retry must complete uncovered baseline pages without duplicate event growth")
	_, markerFound, err = database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.False(t, markerFound)
}

func TestRecoverRepositoryResetRepublishClearsMarkerAfterCheckpoint(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	origin := "desktop-d4e5f6"
	recordResetMetadataWinner(
		t, database, origin, origin+"~local-session", MetadataOpUnstar, 1,
	)
	_, err := PrepareRepositoryResetRepublish(t.Context(), database, dataDir, origin)
	require.NoError(t, err)

	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })

	result, recovered, err := RecoverRepositoryResetRepublish(
		t.Context(), database, repository, origin,
	)
	require.NoError(t, err)
	assert.True(t, recovered)
	assert.Positive(t, result.CheckpointSequence)
	assert.Len(t, listAllContractEntries(t, repository.Content(), origin, KindMeta, 10), 1)
	_, found, err := database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.False(t, found, "checkpoint completion must CAS-clear the durable marker")

	second, recovered, err := RecoverRepositoryResetRepublish(
		t.Context(), database, repository, origin,
	)
	require.NoError(t, err)
	assert.False(t, recovered)
	assert.Zero(t, second.CheckpointSequence)
	assert.Len(t, listAllContractEntries(t, repository.Content(), origin, KindMeta, 10), 1)
}

func TestRepublishRepositoryResetNotifiesOwnedPacking(t *testing.T) {
	repository, err := OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	repository.packer.Close()
	packer := newBlockingPacker()
	repository.packer = newPackScheduler(packer, packSchedulerOptions{RetryDelay: time.Hour})

	_, err = republishRepositoryResetWith(
		t.Context(), t.TempDir(), testDB(t), "desktop-d4e5f6", repository,
		RepositoryResetResult{}, repositoryResetHooks{
			export: func(
				context.Context, *db.DB, ArtifactStore, ExportOptions,
			) (ExportResult, error) {
				return ExportResult{}, nil
			},
		},
	)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return packer.calls.Load() == 1 },
		time.Second, time.Millisecond)
}

func TestSyncRepositoryRecoversResetBeforeFirstExchange(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	origin := "desktop-d4e5f6"
	recordResetMetadataWinner(
		t, database, origin, origin+"~local-session", MetadataOpUnstar, 1,
	)
	_, err := PrepareRepositoryResetRepublish(t.Context(), database, dataDir, origin)
	require.NoError(t, err)
	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	transport := &resetRecoveryObservingTransport{database: database, origin: origin}

	_, err = syncRepositoryWithTransport(
		t.Context(), database, repository, SyncOptions{Origin: origin}, transport,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, transport.exchanges)
}

func recordResetMetadataWinner(
	t *testing.T,
	database *db.DB,
	origin string,
	sessionGID string,
	op string,
	index int,
) {
	t.Helper()
	stamp := HLCTimestamp{
		WallTime: fixedHLCTime().Add(time.Duration(index) * time.Nanosecond),
	}
	event := metadataEvent{
		Version:    formatVersion,
		HLC:        stamp.String(),
		Origin:     origin,
		SessionGID: sessionGID,
		Op:         op,
	}
	data, err := canonicalJSON(event)
	require.NoError(t, err)
	hash := hashHex(data)
	projection, err := metadataProjection(metadataArtifact{
		orderKey: stamp.OrderingKey(hash),
		hash:     hash,
		hlc:      event.HLC,
		event:    event,
	}, origin)
	require.NoError(t, err)
	_, err = database.RecordLocalMetadataProjection(t.Context(), projection)
	require.NoError(t, err)
}

func TestArtifactResetDaemonOwnerTransfersRepositoryReservation(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	seedSession(t, database, "local-session", "project-a")
	current, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	oldStore := current.Content()

	fresh, result, err := resetRepositoryWith(
		t.Context(), dataDir, database, "desktop-d4e5f6", current,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fresh.Close()) })

	assert.DirExists(t, result.DiagnosticRoot)
	_, err = firstStoreOriginPage(t.Context(), oldStore, 10)
	require.Error(t, err)
	contender, err := OpenRepository(t.Context(), dataDir)
	assert.Nil(t, contender)
	require.ErrorContains(t, err, "vault is locked")
	require.NoError(t, current.Close(), "the transferred owner must be inert")
}

func TestArtifactResetLockConflictDoesNotMutateVault(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	current, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, current.Close()) })
	before := snapshotDirectory(t, filepath.Join(dataDir, repositoryDirectory))

	fresh, result, err := resetRepositoryWith(
		t.Context(), dataDir, database, "desktop-d4e5f6", nil,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	wantRoot, rootErr := canonicalRepositoryRoot(dataDir)
	require.NoError(t, rootErr)

	assert.Nil(t, fresh)
	assert.Equal(t, wantRoot, result.VaultRoot)
	assert.NoDirExists(t, result.DiagnosticRoot)
	require.ErrorContains(t, err, "vault is locked")
	assert.Equal(t, before, snapshotDirectory(t, filepath.Join(dataDir, repositoryDirectory)))
}

func TestArtifactResetRejectsInvalidOriginBeforeReleasingCurrentVault(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	current, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, current.Close()) })

	fresh, result, err := resetRepositoryWith(
		t.Context(), dataDir, database, "invalid origin", current,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)

	assert.Nil(t, fresh)
	assert.Empty(t, result)
	require.ErrorContains(t, err, "invalid artifact origin")
	assert.False(t, current.Closed())
	assert.NoDirExists(t, filepath.Join(dataDir, repositoryDirectory)+".reset-20260722T123456.123456789Z")
}

func TestArtifactResetRepeatedTimestampAccumulatesDiagnosticVaults(t *testing.T) {
	dataDir := t.TempDir()
	database := openArtifactResetDB(t, dataDir)
	repository, err := openRepository(t.Context(), dataDir, modernc.Driver{})
	require.NoError(t, err)
	require.NoError(t, repository.Close())

	first, firstResult, err := resetRepositoryWith(
		t.Context(), dataDir, database, "", nil,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	require.NoError(t, err)
	require.NoError(t, first.Close())
	second, secondResult, err := resetRepositoryWith(
		t.Context(), dataDir, database, "", nil,
		repositoryResetHooks{now: fixedArtifactResetTime},
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })

	assert.NotEqual(t, firstResult.DiagnosticRoot, secondResult.DiagnosticRoot)
	assert.Equal(t, firstResult.DiagnosticRoot+".1", secondResult.DiagnosticRoot)
	assert.DirExists(t, firstResult.DiagnosticRoot)
	assert.DirExists(t, secondResult.DiagnosticRoot)
}

func TestArtifactResetFailureBoundariesPreserveRecoveryPaths(t *testing.T) {
	for _, tt := range []struct {
		name       string
		hooks      func(*testing.T, error) repositoryResetHooks
		wantMoved  bool
		wantSource bool
	}{
		{
			name: "move",
			hooks: func(t *testing.T, injected error) repositoryResetHooks {
				return repositoryResetHooks{
					now: fixedArtifactResetTime,
					resetVault: func(_ context.Context, _ docbank.Config, opts docbank.ResetOptions) (*docbank.Vault, error) {
						require.NoError(t, opts.ReleaseCurrent())
						return nil, injected
					},
				}
			},
			wantSource: true,
		},
		{
			name: "fresh initialization",
			hooks: func(_ *testing.T, injected error) repositoryResetHooks {
				return repositoryResetHooks{now: fixedArtifactResetTime, driver: artifactResetFailDriver{err: injected}}
			},
			wantMoved: true, wantSource: true,
		},
		{
			name: "local republish",
			hooks: func(_ *testing.T, injected error) repositoryResetHooks {
				return repositoryResetHooks{
					now: fixedArtifactResetTime,
					export: func(context.Context, *db.DB, ArtifactStore, ExportOptions) (ExportResult, error) {
						return ExportResult{}, injected
					},
				}
			},
			wantMoved: true, wantSource: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			database := openArtifactResetDB(t, dataDir)
			seedSession(t, database, "local-session", "project-a")
			beforeSession, err := database.GetSession(t.Context(), "local-session")
			require.NoError(t, err)
			_, beforeFloorFound, err := database.GetArtifactCheckpointFloor(
				t.Context(), "desktop-d4e5f6",
			)
			require.NoError(t, err)
			assert.False(t, beforeFloorFound)
			current, err := openRepository(t.Context(), dataDir, modernc.Driver{})
			require.NoError(t, err)
			vaultRoot := filepath.Join(dataDir, repositoryDirectory)
			injected := errors.New("injected " + tt.name + " failure")

			fresh, result, err := resetRepositoryWith(
				t.Context(), dataDir, database, "desktop-d4e5f6", current, tt.hooks(t, injected),
			)

			assert.Nil(t, fresh)
			require.ErrorIs(t, err, injected)
			if tt.wantMoved {
				require.ErrorContains(t, err, result.VaultRoot)
				require.ErrorContains(t, err, result.DiagnosticRoot)
				assert.DirExists(t, result.DiagnosticRoot)
			} else {
				assert.NoDirExists(t, result.DiagnosticRoot)
				recovered, openErr := openRepository(t.Context(), dataDir, modernc.Driver{})
				require.NoError(t, openErr)
				require.NoError(t, recovered.Close())
			}
			if tt.wantSource {
				assert.DirExists(t, vaultRoot)
			}
			afterSession, dbErr := database.GetSession(t.Context(), "local-session")
			require.NoError(t, dbErr)
			assert.Equal(t, beforeSession, afterSession)
			_, afterFloorFound, dbErr := database.GetArtifactCheckpointFloor(
				t.Context(), "desktop-d4e5f6",
			)
			require.NoError(t, dbErr)
			assert.False(t, afterFloorFound, "failed reset must not advance SQLite publication floor")
		})
	}
}

func fixedArtifactResetTime() time.Time {
	return time.Date(2026, 7, 22, 12, 34, 56, 123456789, time.UTC)
}

func openArtifactResetDB(t *testing.T, dataDir string) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })
	return database
}

func snapshotDirectory(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			snapshot[relative+string(filepath.Separator)] = nil
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		snapshot[relative] = content
		return nil
	}))
	return snapshot
}

type artifactResetFailDriver struct{ err error }

func (d artifactResetFailDriver) Name() string { return "artifact reset failing driver" }
func (d artifactResetFailDriver) Open(string, docsqlite.OpenOptions) (*sql.DB, error) {
	return nil, d.err
}
func (artifactResetFailDriver) IsBusy(error) bool            { return false }
func (artifactResetFailDriver) IsUniqueViolation(error) bool { return false }
