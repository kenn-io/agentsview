package artifact

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// exportOnlyKinds mirrors folderExchangeKinds, which are also the only kinds
// ExportToStore creates.
var exportOnlyKinds = []Kind{KindSegments, KindManifests, KindCheckpoints}

func assertNoPublishedArtifacts(t *testing.T, store ArtifactStore, origin string) {
	t.Helper()
	for _, kind := range exportOnlyKinds {
		page, err := firstStoreEntryPage(t.Context(), store, origin, kind, maxArtifactListPageSize)
		require.NoError(t, err)
		assert.Empty(t, page.Items, "unexpected published %s artifact", kind)
		require.Empty(t, page.Next)
	}
}

// testExportDB returns a database with an artifact origin already
// established (plan deviation 1: the queue triggers and enqueue hooks only
// fire once an origin row exists) so sessions seeded afterward populate the
// export queue as export tests expect. Origin-gating itself is exercised
// directly by origin_test.go's own tests against a bare testDB.
func testExportDB(t *testing.T) *db.DB {
	t.Helper()
	database := testDB(t)
	require.NoError(t, database.SetSyncState(originStateKey, contractOrigin))
	return database
}

// seedBareExportSessions inserts the minimal rows these export cardinality
// tests need in one transaction. The real sessions-table export triggers still
// enqueue every local row; parser validation and message replacement are not
// part of the behavior under test here.
func seedBareExportSessions(
	t *testing.T,
	database *db.DB,
	count int,
	idFormat string,
	machine string,
) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, database.Update(func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO sessions
			(id, project, machine, agent, created_at)
			VALUES (?, 'project', ?, 'claude', '2026-06-14T01:02:03Z')`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for i := range count {
			if _, err := stmt.ExecContext(
				ctx, fmt.Sprintf(idFormat, i), machine,
			); err != nil {
				return err
			}
		}
		return nil
	}), "seed bare export sessions")
}

// latestStoreCheckpointForTest locates the highest-sequence checkpoint for
// origin and decodes its full session map. It stands in for peer.go's
// latestStoreCheckpointSummary (out of PR2 scope; see plan hazard 6) so
// export tests can assert on published manifest hashes without pulling in
// peer.go.
func latestStoreCheckpointForTest(t *testing.T, store ArtifactStore, origin string) *checkpoint {
	t.Helper()
	ctx := t.Context()
	page, err := firstStoreEntryPage(ctx, store, origin, KindCheckpoints, maxArtifactListPageSize)
	require.NoError(t, err)
	require.NotEmpty(t, page.Items, "no checkpoints published for %s", origin)

	var winner Ref
	winnerSeq := 0
	for _, entry := range page.Items {
		_, reader, err := store.Open(ctx, entry.Ref)
		require.NoError(t, err)
		head, decodeErr := decodeCanonicalCheckpointHead(reader, origin, entry.Ref.Name, entry.Identity)
		require.NoError(t, reader.Verify())
		require.NoError(t, reader.Close())
		require.NoError(t, decodeErr)
		if head.Sequence > winnerSeq {
			winnerSeq = head.Sequence
			winner = entry.Ref
		}
	}
	require.Positive(t, winnerSeq)

	var cp checkpoint
	require.NoError(t, json.Unmarshal(readContractArtifact(t, store, winner), &cp))
	return &cp
}

type countingQueuedExportStore struct {
	database     *db.DB
	queueQueries int
	sessionLoads int
	messageLoads int
	usageLoads   int
}

func (s *countingQueuedExportStore) PendingArtifactExports(
	ctx context.Context, limit int,
) ([]db.ArtifactExportQueueItem, error) {
	s.queueQueries++
	return s.database.PendingArtifactExports(ctx, limit)
}

func (s *countingQueuedExportStore) GetSessionFull(
	ctx context.Context, id string,
) (*db.Session, error) {
	s.sessionLoads++
	return s.database.GetSessionFull(ctx, id)
}

func (s *countingQueuedExportStore) GetAllMessages(
	ctx context.Context, id string,
) ([]db.Message, error) {
	s.messageLoads++
	return s.database.GetAllMessages(ctx, id)
}

func (s *countingQueuedExportStore) GetUsageEvents(
	ctx context.Context, id string,
) ([]db.UsageEvent, error) {
	s.usageLoads++
	return s.database.GetUsageEvents(ctx, id)
}

type countingCanonicalExportDB struct {
	*db.DB
	sessionLoads int
	boundedLoads int
	messageLoads int
	usageLoads   int
}

func (s *countingCanonicalExportDB) GetArtifactExportSession(
	ctx context.Context, id string,
) (*db.Session, error) {
	s.sessionLoads++
	return s.DB.GetArtifactExportSession(ctx, id)
}

func (s *countingCanonicalExportDB) LoadArtifactExportData(
	ctx context.Context, id string, limits db.ArtifactExportLoadLimits,
) (db.ArtifactExportData, error) {
	s.boundedLoads++
	return s.DB.LoadArtifactExportData(ctx, id, limits)
}

func (s *countingCanonicalExportDB) GetAllMessages(
	ctx context.Context, id string,
) ([]db.Message, error) {
	s.messageLoads++
	return s.DB.GetAllMessages(ctx, id)
}

func (s *countingCanonicalExportDB) GetUsageEvents(
	ctx context.Context, id string,
) ([]db.UsageEvent, error) {
	s.usageLoads++
	return s.DB.GetUsageEvents(ctx, id)
}

type recordedArtifactCreate struct {
	Ref      Ref
	Identity Identity
	Created  bool
}

type recordingArtifactStore struct {
	ArtifactStore
	creates []recordedArtifactCreate
}

func (s *recordingArtifactStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	result, err := s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
	if err == nil {
		s.creates = append(s.creates, recordedArtifactCreate{
			Ref: ref, Identity: identity, Created: result.Created,
		})
	}
	return result, err
}

type checkpointReadCountingStore struct {
	ArtifactStore
	lists int
	opens int
}

func (s *checkpointReadCountingStore) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	if kind == Kind(KindCheckpoints) {
		s.lists++
	}
	return s.ArtifactStore.Entries(ctx, origin, kind)
}

func (s *checkpointReadCountingStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref.Kind == Kind(KindCheckpoints) {
		s.opens++
	}
	return s.ArtifactStore.Open(ctx, ref)
}

type catalogCheckpointStore struct {
	ArtifactStore
	entry     Entry
	statCalls int
	openCalls int
}

func (s *catalogCheckpointStore) Stat(context.Context, Ref) (Entry, error) {
	s.statCalls++
	return s.entry, nil
}

func (s *catalogCheckpointStore) Open(
	context.Context, Ref,
) (Entry, VerifiedReader, error) {
	s.openCalls++
	return Entry{}, nil, errors.New("checkpoint body must not be opened")
}

type checkpointStatOverrideStore struct {
	ArtifactStore
	identity *Identity
	err      error
}

func (s *checkpointStatOverrideStore) Stat(ctx context.Context, ref Ref) (Entry, error) {
	if ref.Kind == Kind(KindCheckpoints) {
		if s.err != nil {
			return Entry{}, s.err
		}
		if s.identity != nil {
			return Entry{Ref: ref, Identity: *s.identity}, nil
		}
	}
	return s.ArtifactStore.Stat(ctx, ref)
}

type hiddenCheckpointHeadDB struct {
	*db.DB
}

func (s *hiddenCheckpointHeadDB) GetArtifactCheckpointHead(
	context.Context, string,
) (db.ArtifactCheckpointHead, bool, error) {
	return db.ArtifactCheckpointHead{}, false, nil
}

type closeErrorVerifiedReader struct {
	VerifiedReader
	err error
}

func (r *closeErrorVerifiedReader) Close() error {
	return errors.Join(r.VerifiedReader.Close(), r.err)
}

type checkpointCloseErrorStore struct {
	ArtifactStore
	err error
}

func (s *checkpointCloseErrorStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	entry, reader, err := s.ArtifactStore.Open(ctx, ref)
	if err != nil || ref.Kind != Kind(KindCheckpoints) {
		return entry, reader, err
	}
	return entry, &closeErrorVerifiedReader{VerifiedReader: reader, err: s.err}, nil
}

type checkpointOpenFailureStore struct {
	ArtifactStore
	err error
}

func (s *checkpointOpenFailureStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	if ref.Kind == Kind(KindCheckpoints) {
		return Entry{}, nil, s.err
	}
	return s.ArtifactStore.Open(ctx, ref)
}

type failingArtifactStore struct {
	ArtifactStore
	failKind Kind
	failErr  error
	failed   bool
	calls    []Kind
}

func (s *failingArtifactStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	s.calls = append(s.calls, ref.Kind)
	if !s.failed && ref.Kind == s.failKind {
		s.failed = true
		return CreateResult{}, s.failErr
	}
	return s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
}

type staleClaimExportDB struct {
	*db.DB
	once sync.Once
}

func (s *staleClaimExportDB) ApplyArtifactPublicationChanges(
	ctx context.Context,
	origin string,
	changes []db.ArtifactPublicationChange,
) (int64, bool, error) {
	s.once.Do(func() {
		_ = s.ReplaceSessionMessages("sess-1", []db.Message{{
			SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "newer",
		}})
	})
	return s.DB.ApplyArtifactPublicationChanges(ctx, origin, changes)
}

type mutateAfterCheckpointStore struct {
	ArtifactStore
	database *db.DB
	session  string
	once     sync.Once
}

func (s *mutateAfterCheckpointStore) Create(
	ctx context.Context,
	ref Ref,
	identity Identity,
	mediaType string,
	body io.Reader,
) (CreateResult, error) {
	result, err := s.ArtifactStore.Create(ctx, ref, identity, mediaType, body)
	if err == nil && ref.Kind == Kind(KindCheckpoints) {
		s.once.Do(func() {
			sessionID := s.session
			if sessionID == "" {
				sessionID = "sess-1"
			}
			_ = s.database.ReplaceSessionMessages(sessionID, []db.Message{{
				SessionID: sessionID, Ordinal: 0, Role: "user", Content: "newest",
			}})
		})
	}
	return result, err
}

func deterministicRejectionBatch(
	t *testing.T,
) (*db.DB, ArtifactStore, artifactLimits) {
	t.Helper()
	database := testExportDB(t)
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	seedSession(t, database, "rejected", "alpha")
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin, SessionIDs: []string{"rejected"},
	})
	require.NoError(t, err)
	require.NoError(t, database.ReplaceSessionMessages("rejected", []db.Message{
		{SessionID: "rejected", Ordinal: 0, Role: "user", Content: "one"},
		{SessionID: "rejected", Ordinal: 1, Role: "assistant", Content: "two"},
		{SessionID: "rejected", Ordinal: 2, Role: "user", Content: "three"},
	}))
	seedSession(t, database, "valid", "alpha")
	limits := productionArtifactLimits()
	limits.sessionMessages = 2
	return database, filesystem, limits
}

func TestExportContinuesPastDeterministicRejection(t *testing.T) {
	t.Parallel()

	database, store, limits := deterministicRejectionBatch(t)

	result, err := exportToStoreWithLimits(
		t.Context(), database, store,
		ExportOptions{Origin: contractOrigin}, limits,
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.Equal(t, 1, result.RejectedSessions)
	checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
	assert.NotContains(t, checkpoint.Sessions, contractOrigin+"~rejected")
	assert.Contains(t, checkpoint.Sessions, contractOrigin+"~valid")
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	rejection, ok, err := database.GetArtifactExportRejection(
		t.Context(), "rejected",
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, rejection.Error, "message count exceeds 2")
}

func TestExportContinuesPastInvalidTokenUsage(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	seedSession(t, database, "invalid", "alpha")
	require.NoError(t, database.ReplaceSessionMessages("invalid", []db.Message{{
		SessionID: "invalid", Ordinal: 0, Role: "assistant",
		Content: "bad usage", TokenUsage: jsontext.Value(`{"input_tokens":`),
	}}))
	seedSession(t, database, "valid", "alpha")

	result, err := ExportToStore(
		t.Context(), database, store, ExportOptions{Origin: contractOrigin},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.Equal(t, 1, result.RejectedSessions)
	checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
	assert.NotContains(t, checkpoint.Sessions, contractOrigin+"~invalid")
	assert.Contains(t, checkpoint.Sessions, contractOrigin+"~valid")
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	rejection, ok, err := database.GetArtifactExportRejection(t.Context(), "invalid")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, rejection.Error, "encoding message segment at ordinal 0")
}

func TestExportContinuesPastInvalidManifestValue(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	seedSession(t, database, "invalid", "alpha")
	require.NoError(t, database.UpdateSessionSignals(
		"invalid", db.SessionSignalUpdate{
			ContextPressureMax: new(math.Inf(1)),
		},
	))
	stored, err := database.GetSessionFull(t.Context(), "invalid")
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.NotNil(t, stored.ContextPressureMax)
	require.True(t, math.IsInf(*stored.ContextPressureMax, 1))
	seedSession(t, database, "valid", "alpha")

	result, err := ExportToStore(
		t.Context(), database, store, ExportOptions{Origin: contractOrigin},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.Equal(t, 1, result.RejectedSessions)
	checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
	assert.NotContains(t, checkpoint.Sessions, contractOrigin+"~invalid")
	assert.Contains(t, checkpoint.Sessions, contractOrigin+"~valid")
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	rejection, ok, err := database.GetArtifactExportRejection(t.Context(), "invalid")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, rejection.Error, "encoding manifest for invalid")
}

func TestFullExportSkipsPreviouslyRejectedNativeSessionID(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	seedSession(t, database, "invalid~native", "alpha")

	rejected, err := ExportToStore(
		t.Context(), database, store, ExportOptions{Origin: contractOrigin},
	)
	require.NoError(t, err)
	require.Equal(t, 1, rejected.RejectedSessions)

	result, err := ExportToStore(
		t.Context(), database, store, ExportOptions{
			Origin: contractOrigin,
			Full:   true,
		},
	)
	require.NoError(t, err)
	assert.Zero(t, result.ExportedSessions)
	assert.Zero(t, result.RejectedSessions)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestFullExportRemovesPublishedNativeSessionIDRejectedByCurrentWire(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	databasePath := filepath.Join(t.TempDir(), "archive.db")
	database, err := db.Open(databasePath)
	require.NoError(t, err)
	require.NoError(t, database.SetSyncState(originStateKey, contractOrigin))
	store := newTestArtifactStore(t)
	seedSession(t, database, "legacy~native", "alpha")

	claims, err := database.ArtifactExportClaims(ctx, []string{"legacy~native"})
	require.NoError(t, err)
	require.Len(t, claims, 1)
	manifestHash := strings.Repeat("a", 64)
	revision, changed, err := database.ApplyArtifactPublicationChanges(
		ctx, contractOrigin, []db.ArtifactPublicationChange{{
			SessionID:         "legacy~native",
			Generation:        claims[0].Generation,
			ManifestHash:      manifestHash,
			SourceFingerprint: manifestHash,
		}},
	)
	require.NoError(t, err)
	require.True(t, changed)
	publications, mapDigest, mapRevision, err := spoolArtifactPublicationMap(
		ctx, database, contractOrigin,
	)
	require.NoError(t, err)
	require.Equal(t, revision, mapRevision)
	checkpointBody, checkpointIdentity, err := spoolArtifactCheckpoint(
		ctx, publications, contractOrigin, 1,
	)
	require.NoError(t, err)
	checkpointRef := requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000001.json",
	)
	_, err = store.Create(
		ctx, checkpointRef, checkpointIdentity,
		canonicalArtifactMediaType(KindCheckpoints), checkpointBody,
	)
	require.NoError(t, err)
	require.NoError(t, closeAndRemoveExportSpool(publications))
	require.NoError(t, closeAndRemoveExportSpool(checkpointBody))
	require.NoError(t, database.RecordArtifactCheckpointHead(
		ctx, db.ArtifactCheckpointHead{
			Origin:              contractOrigin,
			Sequence:            1,
			PublicationRevision: revision,
			SessionMapSHA256:    mapDigest,
			CheckpointSHA256:    checkpointIdentity.SHA256,
			CheckpointSize:      checkpointIdentity.Size,
		},
		claims,
	))
	require.NoError(t, database.Close())

	database, err = db.Open(databasePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	result, err := ExportToStore(ctx, database, store, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.RejectedSessions)
	published := latestStoreCheckpointForTest(t, store, contractOrigin)
	assert.NotContains(t, published.Sessions, contractOrigin+"~legacy~native")
	pending, err := database.PendingArtifactExports(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	rejection, ok, err := database.GetArtifactExportRejection(
		ctx, "legacy~native",
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Contains(t, rejection.Error, "native session ID")
}

func TestExportTransientFailureDoesNotReject(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "rejected", "alpha")
	seedSession(t, database, "valid", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	failure := errors.New("injected transient store failure")
	store := &failingArtifactStore{
		ArtifactStore: filesystem, failKind: Kind(KindSegments), failErr: failure,
	}

	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, failure)
	for _, sessionID := range []string{"rejected", "valid"} {
		_, ok, rejectionErr := database.GetArtifactExportRejection(
			t.Context(), sessionID,
		)
		require.NoError(t, rejectionErr)
		assert.False(t, ok)
	}
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestExportRejectionWaitsForCheckpoint(t *testing.T) {
	t.Parallel()

	database, filesystem, limits := deterministicRejectionBatch(t)
	failure := errors.New("injected checkpoint failure")
	store := &failingArtifactStore{
		ArtifactStore: filesystem, failKind: Kind(KindCheckpoints), failErr: failure,
	}

	_, err := exportToStoreWithLimits(
		t.Context(), database, store,
		ExportOptions{Origin: contractOrigin}, limits,
	)
	require.ErrorIs(t, err, failure)
	checkpoint := latestStoreCheckpointForTest(t, filesystem, contractOrigin)
	assert.Contains(t, checkpoint.Sessions, contractOrigin+"~rejected")
	assert.NotContains(t, checkpoint.Sessions, contractOrigin+"~valid")
	_, ok, err := database.GetArtifactExportRejection(t.Context(), "rejected")
	require.NoError(t, err)
	assert.False(t, ok)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestExportStaleRejectionCannotConsumeMutation(t *testing.T) {
	t.Parallel()

	database, filesystem, limits := deterministicRejectionBatch(t)
	store := &mutateAfterCheckpointStore{
		ArtifactStore: filesystem, database: database, session: "rejected",
	}

	_, err := exportToStoreWithLimits(
		t.Context(), database, store,
		ExportOptions{Origin: contractOrigin}, limits,
	)
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactExportRejection(t.Context(), "rejected")
	require.NoError(t, err)
	assert.False(t, ok)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	assert.Equal(t, "rejected", pending[0].SessionID)
	assert.Greater(t, pending[0].Generation, int64(2))
}

func TestExportToStorePublishesDependenciesBeforeCheckpointAndSkipsUnchanged(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &recordingArtifactStore{ArtifactStore: filesystem}

	result, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	assert.True(t, result.CheckpointCreated)
	require.Len(t, store.creates, 3)
	assert.Equal(t, Kind(KindSegments), store.creates[0].Ref.Kind)
	assert.Equal(t, Kind(KindManifests), store.creates[1].Ref.Kind)
	assert.Equal(t, Kind(KindCheckpoints), store.creates[2].Ref.Kind)
	assert.True(t, store.creates[0].Created)
	assert.True(t, store.creates[1].Created)
	assert.True(t, store.creates[2].Created)
	checkpointBytes := readContractArtifact(t, store, store.creates[2].Ref)
	var published checkpoint
	require.NoError(t, json.Unmarshal(checkpointBytes, &published))
	canonicalCheckpoint, err := canonicalJSON(published)
	require.NoError(t, err)
	assert.Equal(t, canonicalCheckpoint, checkpointBytes,
		"streaming checkpoint encoding must remain byte-compatible")

	store.creates = nil
	result, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.Zero(t, result.ExportedSessions)
	assert.False(t, result.CheckpointCreated)
	require.Len(t, store.creates, 2,
		"full export verifies immutable dependencies without minting a version")
	assert.Equal(t, Kind(KindSegments), store.creates[0].Ref.Kind)
	assert.Equal(t, Kind(KindManifests), store.creates[1].Ref.Kind)
	assert.False(t, store.creates[0].Created)
	assert.False(t, store.creates[1].Created)
}

func TestExportKeepsAgentSessionNameSeparateFromUserDisplayName(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	store := newTestArtifactStore(t)
	agentName := "Agent-provided title"
	seedSession(t, database, "sess-1", "alpha", func(sess *db.Session) {
		sess.SessionName = &agentName
	})
	var storedDisplayName, storedSessionName *string
	require.NoError(t, database.Reader().QueryRow(
		`SELECT display_name, session_name FROM sessions WHERE id = ?`, "sess-1",
	).Scan(&storedDisplayName, &storedSessionName))
	assert.Nil(t, storedDisplayName)
	require.NotNil(t, storedSessionName)
	assert.Equal(t, agentName, *storedSessionName)

	result, err := ExportToStore(
		t.Context(), database, store, ExportOptions{Origin: contractOrigin},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)

	checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
	manifestHash := checkpoint.Sessions[contractOrigin+"~sess-1"]
	require.NotEmpty(t, manifestHash)
	ref, err := NewRef(contractOrigin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	published, err := decodeManifestWithLimits(
		readContractArtifact(t, store, ref), productionArtifactLimits(),
	)
	require.NoError(t, err)
	assert.Nil(t, published.Session.DisplayName)
	require.NotNil(t, published.SessionName)
	assert.Equal(t, agentName, *published.SessionName)
}

func TestExportToStorePublishesConfiguredHostnameSession(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	require.NoError(t, database.SetSyncState(
		"artifact_local_machine_name", "workstation.example",
	))
	seedSession(t, database, "sess-hostname", "alpha", func(sess *db.Session) {
		sess.Machine = "workstation.example"
	})
	store := newTestArtifactStore(t)

	result, err := ExportToStore(
		t.Context(), database, store,
		ExportOptions{Origin: contractOrigin, Full: true},
	)
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	checkpoint := latestStoreCheckpointForTest(t, store, contractOrigin)
	assert.Contains(t, checkpoint.Sessions, contractOrigin+"~sess-hostname")
}

func TestExportToStoreFullRepairsMissingDependencyWithoutNewCheckpoint(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	first, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	require.True(t, first.CheckpointCreated)
	segments, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindSegments, 10)
	require.NoError(t, err)
	require.Len(t, segments.Items, 1)
	require.NoError(t, filesystem.Trash(t.Context(), segments.Items[0].Ref))
	_, err = filesystem.Stat(t.Context(), segments.Items[0].Ref)
	require.ErrorIs(t, err, ErrArtifactNotFound)
	headBefore, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin, Full: true,
	})
	require.NoError(t, err)
	assert.False(t, result.CheckpointCreated)
	_, err = filesystem.Stat(t.Context(), segments.Items[0].Ref)
	require.NoError(t, err, "full export recreates a missing dependency even when its manifest survives")
	headAfter, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, headBefore, headAfter)
}

func TestExportToStoreRecreatesMissingRecordedCheckpointAfterVaultReset(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	first, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	_, err = ExportToStore(t.Context(), database, first, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	require.NoError(t, first.Close())

	replacement, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, replacement.Close()) })
	result, err := ExportToStore(t.Context(), database, replacement, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 1, result.CheckpointSequence,
		"vault reset recreates the recorded immutable checkpoint without consuming a new sequence")
	checkpointRef := requireContractRef(t, contractOrigin, KindCheckpoints,
		"cp-0000000001.json")
	checkpointBytes := readContractArtifact(t, replacement, checkpointRef)
	var published checkpoint
	require.NoError(t, json.Unmarshal(checkpointBytes, &published))
	assert.Equal(t, map[string]string{
		contractOrigin + "~sess-1": published.Sessions[contractOrigin+"~sess-1"],
	}, published.Sessions)
}

func TestExportToStoreChangedBatchDoesNotScanCheckpointHistory(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &checkpointReadCountingStore{ArtifactStore: filesystem}
	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	store.lists = 0
	store.opens = 0
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "changed",
	}}))

	result, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 2, result.CheckpointSequence)
	assert.Zero(t, store.lists,
		"a recorded pre-apply head avoids checkpoint-history traversal")
	assert.Equal(t, 1, store.opens,
		"normal changed export validates only the recorded checkpoint body")
}

func TestExportToStoreDirtyBatchDefersRecordedFutureCheckpoint(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	currentHead, ok, err := database.GetArtifactCheckpointHead(
		t.Context(), contractOrigin,
	)
	require.NoError(t, err)
	require.True(t, ok)

	futureSequence, err := database.ReserveArtifactCheckpointSequence(
		t.Context(), contractOrigin, currentHead.Sequence,
	)
	require.NoError(t, err)
	require.Equal(t, 2, futureSequence)
	futureBody := []byte(
		`{"origin":"contract-a1b2c3","seq":2,"sessions":[` +
			`{"gid":"contract-a1b2c3~session","hash":"sha512:future"}],"v":2}` + "\n",
	)
	futureIdentity := identityForBytes(t, futureBody)
	createCheckpointBody(t, filesystem, futureSequence, futureBody)
	require.NoError(t, database.RecordArtifactCheckpointHead(
		t.Context(), db.ArtifactCheckpointHead{
			Origin: contractOrigin, Sequence: futureSequence,
			PublicationRevision: currentHead.PublicationRevision,
			SessionMapSHA256:    strings64("b"),
			CheckpointSHA256:    futureIdentity.SHA256,
			CheckpointSize:      futureIdentity.Size,
		}, nil,
	))
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "changed",
	}}))

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, errFutureArtifactVersion)
	assert.False(t, result.CheckpointCreated)
	result, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, errFutureArtifactVersion)
	assert.False(t, result.CheckpointCreated)
	head, ok, headErr := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, headErr)
	require.True(t, ok)
	assert.Equal(t, futureSequence, head.Sequence)
	page, listErr := firstStoreEntryPage(
		t.Context(), filesystem, contractOrigin, KindCheckpoints, 10,
	)
	require.NoError(t, listErr)
	assert.Len(t, page.Items, 2)
}

func TestExportToStoreUnchangedCheckpointUsesCatalogIdentityOnly(t *testing.T) {
	t.Parallel()

	for _, size := range []int64{128, 64 << 20} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			database := testExportDB(t)
			ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json")
			identity := Identity{SHA256: strings64("a"), Size: size}
			require.NoError(t, database.RecordArtifactCheckpointHead(t.Context(), db.ArtifactCheckpointHead{
				Origin: contractOrigin, Sequence: 1,
				SessionMapSHA256: strings64("b"), CheckpointSHA256: identity.SHA256,
				CheckpointSize: identity.Size,
			}, nil))
			store := &catalogCheckpointStore{entry: Entry{Ref: ref, Identity: identity}}

			result, err := ExportToStore(t.Context(), database, store, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)
			assert.False(t, result.CheckpointCreated)
			assert.Equal(t, 1, store.statCalls)
			assert.Zero(t, store.openCalls,
				"unchanged periodic export cannot drain the checkpoint body")
		})
	}
}

func TestExportToStoreRecordedCheckpointStatRecovery(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)

	operationalFailure := errors.New("injected checkpoint stat failure")
	_, err = ExportToStore(t.Context(), database, &checkpointStatOverrideStore{
		ArtifactStore: filesystem, err: operationalFailure,
	}, ExportOptions{Origin: contractOrigin})
	require.ErrorIs(t, err, operationalFailure)

	mismatch := Identity{SHA256: strings64("c"), Size: 1}
	result, err := ExportToStore(t.Context(), database, &checkpointStatOverrideStore{
		ArtifactStore: filesystem, identity: &mismatch,
	}, ExportOptions{Origin: contractOrigin})
	require.NoError(t, err)
	assert.True(t, result.CheckpointCreated)
	assert.Equal(t, 1, result.CheckpointSequence,
		"catalog identity mismatch quarantines and reconstructs the recorded checkpoint")
}

func TestExportToStoreBootstrapsMissingDatabaseHeadFromLatestCheckpoint(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	store := &checkpointReadCountingStore{ArtifactStore: filesystem}

	result, err := ExportToStore(t.Context(), &hiddenCheckpointHeadDB{DB: database},
		store, ExportOptions{Origin: contractOrigin})
	require.NoError(t, err)
	assert.False(t, result.CheckpointCreated)
	assert.Equal(t, 1, result.CheckpointSequence)
	assert.Positive(t, store.lists)
	assert.Equal(t, 1, store.opens)
	page, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindCheckpoints, 10)
	require.NoError(t, err)
	assert.Len(t, page.Items, 1)
}

func TestExportCheckpointBootstrapPropagatesOperationalOpenErrors(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "canceled", err: context.Canceled},
		{name: "unavailable", err: errors.New("checkpoint store unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := testExportDB(t)
			seedSession(t, database, "sess-1", "alpha")
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
			_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)

			_, err = ExportToStore(t.Context(), &hiddenCheckpointHeadDB{DB: database},
				&checkpointOpenFailureStore{ArtifactStore: filesystem, err: test.err},
				ExportOptions{Origin: contractOrigin})
			require.ErrorIs(t, err, test.err)
		})
	}
}

func TestLatestValidCheckpointFallsBackPastSemanticCandidateAndDefersFuture(t *testing.T) {
	t.Parallel()

	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	for _, candidate := range []struct {
		name string
		body string
	}{
		{name: "cp-0000000001.json", body: `{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n"},
		{name: "cp-0000000002.json", body: `{"origin":"contract-a1b2c3","seq":99,"sessions":{},"v":1}` + "\n"},
	} {
		body := []byte(candidate.body)
		ref := requireContractRef(t, contractOrigin, KindCheckpoints, candidate.name)
		_, err := filesystem.Create(t.Context(), ref, identityForBytes(t, body),
			canonicalArtifactMediaType(KindCheckpoints), strings.NewReader(candidate.body))
		require.NoError(t, err)
	}

	head, ok, err := latestValidCheckpointHead(t.Context(), filesystem, contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, head.Sequence)

	future := []byte(`{"origin":"contract-a1b2c3","seq":3,"sessions":{},"v":2}` + "\n")
	ref := requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000003.json")
	_, err = filesystem.Create(t.Context(), ref, identityForBytes(t, future),
		canonicalArtifactMediaType(KindCheckpoints), strings.NewReader(string(future)))
	require.NoError(t, err)
	_, _, err = latestValidCheckpointHead(t.Context(), filesystem, contractOrigin)
	require.ErrorIs(t, err, errFutureArtifactVersion)
}

func TestExportSpoolConstructionJoinsCleanupFailures(t *testing.T) {
	// Serial: overrides package-level spool filesystem hooks.
	chmodFailure := errors.New("injected spool setup failure")
	cleanupFailure := errors.New("injected spool cleanup failure")
	previousChmod := exportSpoolChmod
	previousCleanup := exportSpoolCleanup
	t.Cleanup(func() {
		exportSpoolChmod = previousChmod
		exportSpoolCleanup = previousCleanup
	})
	exportSpoolChmod = func(*os.File) error { return chmodFailure }
	exportSpoolCleanup = func(file *os.File) error {
		return errors.Join(closeAndRemoveExportSpool(file), cleanupFailure)
	}

	_, _, _, err := spoolArtifactPublicationMap(
		t.Context(), testExportDB(t), contractOrigin,
	)
	require.ErrorIs(t, err, chmodFailure)
	require.ErrorIs(t, err, cleanupFailure)

	mapSpool, err := os.CreateTemp("", "agentsview-artifact-test-map-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeAndRemoveExportSpool(mapSpool) })
	_, err = io.WriteString(mapSpool, "{}\n")
	require.NoError(t, err)
	_, _, err = spoolArtifactCheckpoint(t.Context(), mapSpool, contractOrigin, 1)
	require.ErrorIs(t, err, chmodFailure)
	require.ErrorIs(t, err, cleanupFailure)
}

func TestSpoolArtifactCheckpointEnforcesDecodedSizeBoundary(t *testing.T) {
	t.Parallel()

	mapSpool, err := os.CreateTemp("", "agentsview-artifact-test-map-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeAndRemoveExportSpool(mapSpool) })
	_, err = io.WriteString(mapSpool, "{}\n")
	require.NoError(t, err)

	baseline, identity, err := spoolArtifactCheckpointWithLimit(
		t.Context(), mapSpool, contractOrigin, 1, checkpointDecodedLimit,
	)
	require.NoError(t, err)
	require.NoError(t, closeAndRemoveExportSpool(baseline))

	atLimit, atLimitIdentity, err := spoolArtifactCheckpointWithLimit(
		t.Context(), mapSpool, contractOrigin, 1, identity.Size,
	)
	require.NoError(t, err)
	require.Equal(t, identity, atLimitIdentity)
	require.NoError(t, closeAndRemoveExportSpool(atLimit))

	checkpoint, _, err := spoolArtifactCheckpointWithLimit(
		t.Context(), mapSpool, contractOrigin, 1, identity.Size-1,
	)
	require.Error(t, err)
	assert.Nil(t, checkpoint)
	assert.Contains(t, err.Error(), "checkpoint exceeds decoded size limit")
}

func TestExportToStorePropagatesCheckpointCloseErrorWithoutAcknowledging(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	require.NoError(t, database.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "changed",
	}}))
	closeFailure := errors.New("injected checkpoint close failure")

	_, err = ExportToStore(t.Context(), &hiddenCheckpointHeadDB{DB: database}, &checkpointCloseErrorStore{
		ArtifactStore: filesystem, err: closeFailure,
	}, ExportOptions{Origin: contractOrigin})
	require.ErrorIs(t, err, closeFailure)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)
}

func TestExportToStoreCancellationLeavesQueuePending(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = ExportToStore(ctx, database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, context.Canceled)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Len(t, pending, 1)
}

func TestExportToStoreFailureKeepsClaimAndCheckpointLast(t *testing.T) {
	t.Parallel()

	failure := errors.New("injected artifact create failure")
	tests := []struct {
		name         string
		failKind     Kind
		wantCalls    []Kind
		wantRetrySeq int
	}{
		{name: "dependency", failKind: Kind(KindSegments),
			wantCalls: []Kind{Kind(KindSegments)}, wantRetrySeq: 1},
		{name: "manifest", failKind: Kind(KindManifests),
			wantCalls: []Kind{Kind(KindSegments), Kind(KindManifests)}, wantRetrySeq: 1},
		{name: "checkpoint", failKind: Kind(KindCheckpoints),
			wantCalls: []Kind{Kind(KindSegments), Kind(KindManifests), Kind(KindCheckpoints)}, wantRetrySeq: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := testExportDB(t)
			seedSession(t, database, "sess-1", "alpha")
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
			store := &failingArtifactStore{
				ArtifactStore: filesystem, failKind: tt.failKind, failErr: failure,
			}

			_, err = ExportToStore(t.Context(), database, store, ExportOptions{
				Origin: contractOrigin,
			})
			require.ErrorIs(t, err, failure)
			assert.Equal(t, tt.wantCalls, store.calls)
			pending, err := database.PendingArtifactExports(t.Context(), 10)
			require.NoError(t, err)
			require.Len(t, pending, 1, "failed export must retain its exact claim")
			page, err := firstStoreEntryPage(t.Context(), store, contractOrigin, KindCheckpoints, 10)
			require.NoError(t, err)
			assert.Empty(t, page.Items, "checkpoint cannot precede a failed dependency")

			store.calls = nil
			result, err := ExportToStore(t.Context(), database, store, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)
			assert.True(t, result.CheckpointCreated)
			assert.Equal(t, tt.wantRetrySeq, result.CheckpointSequence)
			pending, err = database.PendingArtifactExports(t.Context(), 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
		})
	}
}

func TestExportToStoreStaleGenerationDoesNotPublishOrAcknowledge(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	stale := &staleClaimExportDB{DB: database}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	_, err = ExportToStore(t.Context(), stale, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	assert.False(t, ok)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Greater(t, pending[0].Generation, int64(1))
	page, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindCheckpoints, 10)
	require.NoError(t, err)
	assert.Empty(t, page.Items)
}

func TestExportToStoreStaleGenerationAfterCheckpointDoesNotAdvanceHead(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &mutateAfterCheckpointStore{ArtifactStore: filesystem, database: database}

	_, err = ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	assert.False(t, ok)
	pending, err := database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	firstPage, err := firstStoreEntryPage(t.Context(), filesystem, contractOrigin, KindCheckpoints, 10)
	require.NoError(t, err)
	require.Len(t, firstPage.Items, 1,
		"the immutable stale checkpoint is harmless while no head references it")

	result, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, 2, result.CheckpointSequence)
	head, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, head.Sequence)
	pending, err = database.PendingArtifactExports(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestExportToStorePublicationRevisionRejectsPhysicallyCreatedStaleCheckpoint(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := db.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, first.Close()) })
	require.NoError(t, first.SetSyncState(originStateKey, contractOrigin))
	second, err := db.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	ctx := t.Context()
	seedSession(t, first, "session-a", "project")
	claimA, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	require.Len(t, claimA, 1)
	sessionA, err := first.GetArtifactExportSession(ctx, "session-a")
	require.NoError(t, err)
	messagesA, err := first.GetAllMessages(ctx, "session-a")
	require.NoError(t, err)
	usageA, err := first.GetUsageEvents(ctx, "session-a")
	require.NoError(t, err)
	manifestA, _, err := exportLoadedSessionToStore(
		ctx, filesystem, contractOrigin, sessionA, messagesA, usageA, productionArtifactLimits(),
	)
	require.NoError(t, err)
	revisionA, changed, err := first.ApplyArtifactPublicationChanges(ctx, contractOrigin,
		[]db.ArtifactPublicationChange{{
			SessionID: "session-a", Generation: claimA[0].Generation,
			ManifestHash: manifestA, SourceFingerprint: manifestA,
		}})
	require.NoError(t, err)
	require.True(t, changed)
	mapA, digestA, snapshotA, err := spoolArtifactPublicationMap(ctx, first, contractOrigin)
	require.NoError(t, err)
	require.Equal(t, revisionA, snapshotA)
	t.Cleanup(func() { _ = closeAndRemoveExportSpool(mapA) })

	seedSession(t, second, "session-b", "project")
	claimB, err := second.ArtifactExportClaims(ctx, []string{"session-b"})
	require.NoError(t, err)
	require.Len(t, claimB, 1)
	sessionB, err := second.GetArtifactExportSession(ctx, "session-b")
	require.NoError(t, err)
	messagesB, err := second.GetAllMessages(ctx, "session-b")
	require.NoError(t, err)
	usageB, err := second.GetUsageEvents(ctx, "session-b")
	require.NoError(t, err)
	manifestB, _, err := exportLoadedSessionToStore(
		ctx, filesystem, contractOrigin, sessionB, messagesB, usageB, productionArtifactLimits(),
	)
	require.NoError(t, err)
	revisionB, changed, err := second.ApplyArtifactPublicationChanges(ctx, contractOrigin,
		[]db.ArtifactPublicationChange{{
			SessionID: "session-b", Generation: claimB[0].Generation,
			ManifestHash: manifestB, SourceFingerprint: manifestB,
		}})
	require.NoError(t, err)
	require.True(t, changed)
	mapB, digestB, snapshotB, err := spoolArtifactPublicationMap(ctx, second, contractOrigin)
	require.NoError(t, err)
	require.Equal(t, revisionB, snapshotB)
	sequenceB, err := reserveCheckpointSequenceFromStore(ctx, second, filesystem, contractOrigin)
	require.NoError(t, err)
	checkpointB, identityB, err := spoolArtifactCheckpoint(ctx, mapB, contractOrigin, sequenceB)
	require.NoError(t, err)
	refB := requireContractRef(t, contractOrigin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequenceB))
	_, err = filesystem.Create(ctx, refB, identityB,
		canonicalArtifactMediaType(KindCheckpoints), checkpointB)
	require.NoError(t, err)
	require.NoError(t, closeAndRemoveExportSpool(mapB))
	require.NoError(t, closeAndRemoveExportSpool(checkpointB))
	require.NoError(t, second.RecordArtifactCheckpointHead(ctx, db.ArtifactCheckpointHead{
		Origin: contractOrigin, Sequence: sequenceB, PublicationRevision: snapshotB,
		SessionMapSHA256: digestB, CheckpointSHA256: identityB.SHA256,
		CheckpointSize: identityB.Size,
	}, claimB))

	sequenceA, err := reserveCheckpointSequenceFromStore(ctx, first, filesystem, contractOrigin)
	require.NoError(t, err)
	require.Greater(t, sequenceA, sequenceB)
	checkpointA, identityA, err := spoolArtifactCheckpoint(ctx, mapA, contractOrigin, sequenceA)
	require.NoError(t, err)
	refA := requireContractRef(t, contractOrigin, KindCheckpoints,
		fmt.Sprintf("cp-%010d.json", sequenceA))
	_, err = filesystem.Create(ctx, refA, identityA,
		canonicalArtifactMediaType(KindCheckpoints), checkpointA)
	require.NoError(t, err)
	require.NoError(t, closeAndRemoveExportSpool(checkpointA))
	err = first.RecordArtifactCheckpointHead(ctx, db.ArtifactCheckpointHead{
		Origin: contractOrigin, Sequence: sequenceA, PublicationRevision: snapshotA,
		SessionMapSHA256: digestA, CheckpointSHA256: identityA.SHA256,
		CheckpointSize: identityA.Size,
	}, claimA)
	require.ErrorIs(t, err, db.ErrArtifactExportClaimStale)
	_, err = filesystem.Stat(ctx, refA)
	require.NoError(t, err, "the stale immutable checkpoint may exist without becoming authoritative")
	head, ok, err := first.GetArtifactCheckpointHead(ctx, contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sequenceB, head.Sequence)
	pending, err := first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	require.Equal(t, claimA, pending)

	result, err := ExportToStore(ctx, first, filesystem, ExportOptions{Origin: contractOrigin})
	require.NoError(t, err)
	assert.False(t, result.CheckpointCreated)
	pending, err = first.ArtifactExportClaims(ctx, []string{"session-a"})
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestArtifactExportCardinalityLoadsOnlyDirtyBatch(t *testing.T) {
	t.Parallel()

	for _, archiveSize := range []int{20, 2000} {
		t.Run(fmt.Sprintf("archive-%d", archiveSize), func(t *testing.T) {
			database := testExportDB(t)
			seedBareExportSessions(
				t, database, archiveSize, "peer-%04d", "peer-a1b2c3",
			)
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "dirty", Project: "project", Machine: "local", Agent: "claude",
			}))
			require.NoError(t, database.ReplaceSessionMessages("dirty", []db.Message{{
				SessionID: "dirty", Ordinal: 0, Role: "user", Content: "changed",
			}}))
			require.NoError(t, database.ReplaceSessionUsageEvents("dirty", []db.UsageEvent{{
				SessionID: "dirty", Source: "event", Model: "model", DedupKey: "one",
			}}))

			store := &countingQueuedExportStore{database: database}
			visited := 0
			err := forEachQueuedArtifactExport(t.Context(), store, 1, func(work queuedArtifactExport) error {
				visited++
				assert.Equal(t, "dirty", work.Item.SessionID)
				require.NotNil(t, work.Session)
				assert.Len(t, work.Messages, 1)
				assert.Len(t, work.UsageEvents, 1)
				return nil
			})
			require.NoError(t, err)
			assert.Equal(t, 1, visited)
			assert.Equal(t, 1, store.queueQueries)
			assert.Equal(t, 1, store.sessionLoads)
			assert.Equal(t, 1, store.messageLoads)
			assert.Equal(t, 1, store.usageLoads)
		})
	}
}

func TestExportToStoreCardinalityIgnoresUnrelatedArchiveBodies(t *testing.T) {
	t.Parallel()

	for _, archiveSize := range []int{20, 2000} {
		t.Run(fmt.Sprintf("archive-%d", archiveSize), func(t *testing.T) {
			database := testExportDB(t)
			seedBareExportSessions(
				t, database, archiveSize, "peer-%04d", "peer-a1b2c3",
			)
			seedSession(t, database, "dirty", "project")
			counted := &countingCanonicalExportDB{DB: database}
			filesystem, err := newProtocolTestStore(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

			result, err := ExportToStore(t.Context(), counted, filesystem, ExportOptions{
				Origin: contractOrigin,
			})
			require.NoError(t, err)
			assert.Equal(t, 1, result.ExportedSessions)
			assert.Equal(t, 1, counted.sessionLoads)
			assert.Equal(t, 1, counted.boundedLoads)
			assert.Zero(t, counted.messageLoads)
			assert.Zero(t, counted.usageLoads)
		})
	}
}

func TestExportToStoreIncrementalBatchIsBoundedAndFullStreamsAllBodies(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	const total = artifactExportBatchSize + 5
	for i := range total {
		seedSession(t, database, fmt.Sprintf("session-%03d", i), "project")
	}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
	})
	require.NoError(t, err)
	assert.Equal(t, artifactExportBatchSize, result.ExportedSessions)
	pending, err := database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Len(t, pending, 5)
	firstBytes := readContractArtifact(t, filesystem,
		requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json"))
	var first checkpoint
	require.NoError(t, json.Unmarshal(firstBytes, &first))
	assert.Len(t, first.Sessions, artifactExportBatchSize)

	result, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin,
		Full:   true,
	})
	require.NoError(t, err)
	assert.Equal(t, 5, result.ExportedSessions,
		"full pass idempotently recreates clean dependencies and creates only missing manifests")
	pending, err = database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Empty(t, pending)
	secondBytes := readContractArtifact(t, filesystem,
		requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000002.json"))
	var second checkpoint
	require.NoError(t, json.Unmarshal(secondBytes, &second))
	assert.Len(t, second.Sessions, total)
}

func TestExportToStoreFullDrainsMoreThanOneClaimPage(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	const total = 1025
	seedBareExportSessions(
		t, database, total, "session-%04d", "local",
	)
	counted := &countingCanonicalExportDB{DB: database}
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	result, err := ExportToStore(t.Context(), counted, filesystem, ExportOptions{
		Origin: contractOrigin, Full: true,
	})
	require.NoError(t, err)
	assert.Equal(t, total, result.ExportedSessions)
	assert.Equal(t, total, counted.sessionLoads,
		"full export loads each body once instead of reloading the archive per page")
	assert.Equal(t, total, counted.boundedLoads)
	assert.Zero(t, counted.messageLoads)
	assert.Zero(t, counted.usageLoads)
	pending, err := database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Empty(t, pending)
	head, ok, err := database.GetArtifactCheckpointHead(t.Context(), contractOrigin)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, head.Sequence)
	body := readContractArtifact(t, filesystem, requireContractRef(
		t, contractOrigin, KindCheckpoints, "cp-0000000002.json",
	))
	var published checkpoint
	require.NoError(t, json.Unmarshal(body, &published))
	assert.Len(t, published.Sessions, total)
}

func TestExportToStoreExplicitSessionIDsClaimBeyondOldestQueuePage(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	const total = 1025
	seedBareExportSessions(t, database, total, "session-%04d", "local")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

	result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
		Origin: contractOrigin, SessionIDs: []string{"session-1024"},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	pending, err := database.PendingArtifactExports(t.Context(), 1024)
	require.NoError(t, err)
	assert.Len(t, pending, 1024)
	body := readContractArtifact(t, filesystem,
		requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json"))
	var published checkpoint
	require.NoError(t, json.Unmarshal(body, &published))
	assert.Equal(t, map[string]string{
		contractOrigin + "~session-1024": published.Sessions[contractOrigin+"~session-1024"],
	}, published.Sessions)
}

func TestExportToStorePublishesEmptyAndDeletionSets(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		database := testExportDB(t)
		filesystem, err := newProtocolTestStore(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, filesystem.Close()) })

		result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
			Origin: contractOrigin, Full: true,
		})
		require.NoError(t, err)
		assert.True(t, result.CheckpointCreated)
		body := readContractArtifact(t, filesystem,
			requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000001.json"))
		assert.Equal(t,
			`{"origin":"contract-a1b2c3","seq":1,"sessions":{},"v":1}`+"\n",
			string(body))
	})

	t.Run("deletion", func(t *testing.T) {
		database := testExportDB(t)
		seedSession(t, database, "sess-1", "project")
		filesystem, err := newProtocolTestStore(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
		_, err = ExportToStore(t.Context(), database, filesystem, ExportOptions{
			Origin: contractOrigin,
		})
		require.NoError(t, err)
		require.NoError(t, database.SoftDeleteSession("sess-1"))

		result, err := ExportToStore(t.Context(), database, filesystem, ExportOptions{
			Origin: contractOrigin,
		})
		require.NoError(t, err)
		assert.True(t, result.CheckpointCreated)
		body := readContractArtifact(t, filesystem,
			requireContractRef(t, contractOrigin, KindCheckpoints, "cp-0000000002.json"))
		assert.Contains(t, string(body), `"sessions":{}`)
	})
}

func TestQueuedArtifactExportTreatsOwnershipLossAsDeletion(t *testing.T) {
	t.Parallel()

	database := testExportDB(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "moved", Project: "project", Machine: "local", Agent: "claude",
	}))
	require.NoError(t, database.ReplaceSessionMessages("moved", []db.Message{{
		SessionID: "moved", Ordinal: 0, Role: "user", Content: "local content",
	}}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "moved", Project: "project", Machine: "peer-a1b2c3", Agent: "claude",
	}))

	store := &countingQueuedExportStore{database: database}
	visited := 0
	err := forEachQueuedArtifactExport(t.Context(), store, 1, func(work queuedArtifactExport) error {
		visited++
		assert.Nil(t, work.Session)
		assert.Empty(t, work.Messages)
		assert.Empty(t, work.UsageEvents)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 1, visited)
	assert.Equal(t, 1, store.sessionLoads)
	assert.Zero(t, store.messageLoads)
	assert.Zero(t, store.usageLoads)
}

func TestExportEmitsNewManifestAfterDataVersionChange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := contractOrigin
	seedSession(t, database, "sess-1", "alpha")

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	require.Equal(t, 1, result.ExportedSessions)
	cp := latestStoreCheckpointForTest(t, store, origin)
	firstHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, firstHash)

	require.NoError(t, database.SetSessionDataVersion("sess-1", 42))
	result, err = ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)
	cp = latestStoreCheckpointForTest(t, store, origin)
	nextHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, nextHash)
	assert.NotEqual(t, firstHash, nextHash)

	ref, err := NewRef(origin, KindManifests, nextHash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)
	assert.Equal(t, 42, m.DataVersion)
}

func TestExportIncludesLocalOwnedSessionClasses(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := contractOrigin
	seedSession(t, database, "file-sess", "alpha")
	seedSession(t, database, "claude-ai-sess", "bravo", func(s *db.Session) {
		s.Agent = "claude-ai"
	})
	seedSession(t, database, "upload-sess", "charlie", func(s *db.Session) {
		s.Agent = "upload"
	})
	seedSession(t, database, "orphan-sess", "delta", func(s *db.Session) {
		s.SourceSessionID = "missing-source"
	})

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 4, result.ExportedSessions)

	cp := latestStoreCheckpointForTest(t, store, origin)
	assert.Contains(t, cp.Sessions, origin+"~file-sess")
	assert.Contains(t, cp.Sessions, origin+"~claude-ai-sess")
	assert.Contains(t, cp.Sessions, origin+"~upload-sess")
	assert.Contains(t, cp.Sessions, origin+"~orphan-sess")
}

func TestExportScrubsUnstableArtifactIDs(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := contractOrigin
	seedSession(t, database, "sess-1", "alpha")
	require.NoError(t, database.ReplaceSessionUsageEvents("sess-1", []db.UsageEvent{
		{
			SessionID:   "sess-1",
			Source:      "fixture",
			Model:       "claude-test",
			InputTokens: 1,
			OccurredAt:  "2026-06-14T01:02:04Z",
			DedupKey:    "usage-1",
		},
	}))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	cp := latestStoreCheckpointForTest(t, store, origin)
	manifestHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, manifestHash)

	ref, err := NewRef(origin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)
	require.Len(t, m.UsageEvents, 1)
	manifestData, err := canonicalJSON(m)
	require.NoError(t, err)
	assert.NotContains(t, string(manifestData), `"ID"`)
	assert.NotContains(t, string(manifestData), `"SessionID"`)

	msgs := testStoreManifestMessages(t, store, origin, m)
	require.Len(t, msgs, 2)
	for _, msg := range msgs {
		assert.Zero(t, msg.ID)
		assert.Empty(t, msg.SessionID)
	}
}

func TestExportRejectsInvalidOriginBeforeCreatingPaths(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	seedSession(t, database, "sess-1", "alpha")

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: "../outside", Full: true})
	require.Error(t, err)
	assert.Zero(t, result.ExportedSessions)
	assert.Contains(t, err.Error(), "invalid artifact origin")
	assertNoPublishedArtifacts(t, store, "outside-a1b2c3")
}

func TestExportSkipsDeletedAndForeignSessions(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := newTestArtifactStore(t)
	origin := contractOrigin
	database := testExportDB(t)

	seedSession(t, database, "owned", "alpha")
	// A soft-deleted owned session and a foreign-owned session are both excluded.
	seedSession(t, database, "trashed", "alpha")
	require.NoError(t, database.SoftDeleteSession("trashed"))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:        "foreign",
		Project:   "alpha",
		Machine:   "desktop-d4e5f6",
		Agent:     "claude",
		CreatedAt: "2026-06-14T01:02:03Z",
	}))

	_, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)

	cp := latestStoreCheckpointForTest(t, store, origin)
	assert.Contains(t, cp.Sessions, origin+"~owned")
	assert.NotContains(t, cp.Sessions, origin+"~trashed")
	assert.NotContains(t, cp.Sessions, origin+"~foreign")
}

// reEnqueueOnBoundaryStore re-enqueues its one session only at the outer
// round-boundary check exportFullToStore makes with limit 1
// (database.PendingArtifactExports(ctx, 1) just after each round's
// ExportToStore call), never on an ordinary acknowledgement. Deviation 5
// requires this ordering: a fake that re-enqueues on every acknowledgement
// keeps drain()'s own unbounded internal loop non-empty forever and the test
// hangs instead of exercising the outer cap.
type reEnqueueOnBoundaryStore struct {
	*db.DB
	round int
}

func (s *reEnqueueOnBoundaryStore) PendingArtifactExports(
	ctx context.Context, limit int,
) ([]db.ArtifactExportQueueItem, error) {
	if limit == 1 {
		s.round++
		if err := s.ReplaceSessionMessages("sess-1", []db.Message{{
			SessionID: "sess-1", Ordinal: 0, Role: "user",
			Content: fmt.Sprintf("round %d", s.round),
		}}); err != nil {
			return nil, err
		}
	}
	return s.DB.PendingArtifactExports(ctx, limit)
}

// TestExportFullDrainCapReturnsAccumulatedResultAfterUnsettledQueue covers
// plan deviation 5: exportFullToStore's terminal drain must not spin forever
// when a concurrent writer keeps the queue perpetually non-empty. It must
// give up after maxExportDrainRounds and still return whatever it exported.
func TestExportFullDrainCapReturnsAccumulatedResultAfterUnsettledQueue(t *testing.T) {
	t.Parallel()

	const drainRounds = 3
	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	store := &reEnqueueOnBoundaryStore{DB: database}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	result, err := exportFullToStoreWithDrainRounds(
		ctx, store, filesystem, contractOrigin, drainRounds,
	)
	require.EqualError(t, err, "artifact export queue did not settle after 3 drain rounds")
	assert.ErrorIs(t, err, ErrArtifactExportUnsettled)
	assert.Positive(t, result.ExportedSessions,
		"the accumulated result must still be returned alongside the drain-cap error")
}

type reEnqueueAfterAcknowledgeStore struct {
	*db.DB
	requeues int
}

func (s *reEnqueueAfterAcknowledgeStore) FinalizeArtifactExports(
	ctx context.Context, outcomes []db.ArtifactExportOutcome,
) error {
	if err := s.DB.FinalizeArtifactExports(ctx, outcomes); err != nil {
		return err
	}
	return s.requeue(outcomes)
}

func (s *reEnqueueAfterAcknowledgeStore) RecordArtifactCheckpointHeadOutcomes(
	ctx context.Context,
	head db.ArtifactCheckpointHead,
	outcomes []db.ArtifactExportOutcome,
) error {
	if err := s.DB.RecordArtifactCheckpointHeadOutcomes(ctx, head, outcomes); err != nil {
		return err
	}
	return s.requeue(outcomes)
}

func (s *reEnqueueAfterAcknowledgeStore) requeue(
	outcomes []db.ArtifactExportOutcome,
) error {
	if len(outcomes) == 0 {
		return nil
	}
	s.requeues++
	return s.ReplaceSessionMessages("sess-1", []db.Message{{
		SessionID: "sess-1", Ordinal: 0, Role: "user",
		Content: fmt.Sprintf("concurrent write %d", s.requeues),
	}})
}

func TestExportFullDrainCapAppliesToWritesArrivingDuringDrain(t *testing.T) {
	t.Parallel()

	const drainRounds = 3
	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	filesystem, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	concurrent := &reEnqueueAfterAcknowledgeStore{DB: database}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	result, err := exportFullToStoreWithDrainRounds(
		ctx, concurrent, filesystem, contractOrigin, drainRounds,
	)
	require.EqualError(t, err, "artifact export queue did not settle after 3 drain rounds")
	assert.ErrorIs(t, err, ErrArtifactExportUnsettled)
	assert.Positive(t, result.ExportedSessions)
	assert.GreaterOrEqual(t, concurrent.requeues, drainRounds)
}

type finiteReEnqueueBoundaryStore struct {
	*db.DB
	remaining int
}

func (s *finiteReEnqueueBoundaryStore) PendingArtifactExports(
	ctx context.Context,
	limit int,
) ([]db.ArtifactExportQueueItem, error) {
	if limit == 1 && s.remaining > 0 {
		s.remaining--
		if err := s.ReplaceSessionMessages("sess-1", []db.Message{{
			SessionID: "sess-1", Ordinal: 0, Role: "user",
			Content: "one final concurrent write",
		}}); err != nil {
			return nil, err
		}
	}
	return s.DB.PendingArtifactExports(ctx, limit)
}

func TestExportFullDrainCapDoesNotReportMoreAfterFinalDrainSettles(
	t *testing.T,
) {
	t.Parallel()

	database := testExportDB(t)
	seedSession(t, database, "sess-1", "alpha")
	store := newTestArtifactStore(t)
	concurrent := &finiteReEnqueueBoundaryStore{
		DB: database, remaining: 1,
	}

	result, err := exportFullToStoreWithDrainRounds(
		t.Context(),
		concurrent,
		store,
		contractOrigin,
		1,
	)
	require.NoError(t, err)
	assert.Positive(t, result.ExportedSessions)
	pending, err := database.CountPendingArtifactExports(t.Context())
	require.NoError(t, err)
	assert.Zero(t, pending)
}

// TestExportRoundTripsSessionQualitySignals pins the PR1 invariant that the
// manifest-level session_quality_signals field is the sole canonical carrier
// for a session's quality signals (manifest_session.go): the inner session
// DTO must never gain a quality-signals field of its own, and every seeded
// signal value -- not just non-nil presence -- must survive the encode then
// decode round trip.
func TestExportRoundTripsSessionQualitySignals(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	database := testExportDB(t)
	store := newTestArtifactStore(t)
	origin := contractOrigin
	seedSession(t, database, "sess-1", "alpha")

	want := db.QualitySignals{
		Version:                     2,
		ShortPromptCount:            3,
		UnstructuredStart:           true,
		MissingSuccessCriteriaCount: 4,
		MissingVerificationCount:    5,
		DuplicatePromptCount:        6,
		NoCodeContextCount:          7,
		RunawayToolLoopCount:        8,
	}
	require.NoError(t, database.UpdateSessionSignals("sess-1", db.SessionSignalUpdate{
		QualitySignals: want,
	}))
	seeded, err := database.GetSessionFull(ctx, "sess-1")
	require.NoError(t, err)
	require.NotNil(t, seeded)
	require.Equal(t, &want, seeded.StoredQualitySignals(),
		"UpdateSessionSignals must persist through the scalar columns export reads")

	result, err := ExportToStore(ctx, database, store, ExportOptions{Origin: origin, Full: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.ExportedSessions)

	cp := latestStoreCheckpointForTest(t, store, origin)
	manifestHash := cp.Sessions[origin+"~sess-1"]
	require.NotEmpty(t, manifestHash)
	ref, err := NewRef(origin, KindManifests, manifestHash+".json")
	require.NoError(t, err)
	m, err := decodeManifestWithLimits(readContractArtifact(t, store, ref), productionArtifactLimits())
	require.NoError(t, err)

	require.NotNil(t, m.SessionQualitySignals)
	assert.Equal(t, &manifestQualitySignals{
		Version:                     want.Version,
		ShortPromptCount:            want.ShortPromptCount,
		UnstructuredStart:           want.UnstructuredStart,
		MissingSuccessCriteriaCount: want.MissingSuccessCriteriaCount,
		MissingVerificationCount:    want.MissingVerificationCount,
		DuplicatePromptCount:        want.DuplicatePromptCount,
		NoCodeContextCount:          want.NoCodeContextCount,
		RunawayToolLoopCount:        want.RunawayToolLoopCount,
	}, m.SessionQualitySignals, "the manifest-level field must round-trip every seeded value")

	sessionData, err := json.Marshal(m.Session)
	require.NoError(t, err)
	assert.NotContains(t, string(sessionData), "short_prompt_count",
		"the inner session DTO must carry no quality-signals content of its own")
	assert.NotContains(t, string(sessionData), `"quality_signals"`,
		"the inner session DTO must carry no quality-signals content of its own")
}
