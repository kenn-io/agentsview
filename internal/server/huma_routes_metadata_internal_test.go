package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

type statsSpyService struct {
	service.SessionService
	got service.StatsFilter
}

func newArtifactHandlerTestServer(
	t *testing.T, database *db.DB, cfg config.Config,
) *Server {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	repository, err := artifact.OpenRepository(t.Context(), cfg.DataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	store := repository.Content()
	return &Server{
		db:            database,
		cfg:           cfg,
		artifactStore: store,
		artifactOps:   artifactOperationLifetime{store: store},
	}
}

func (s *statsSpyService) Stats(
	_ context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	s.got = f
	return &service.SessionStats{}, nil
}

func TestHumaGetSessionStatsUsesServerGitHubToken(t *testing.T) {
	spy := &statsSpyService{}
	srv := &Server{
		cfg:      config.Config{GithubToken: "server-token"},
		sessions: spy,
	}

	_, err := srv.humaGetSessionStats(context.Background(), &sessionStatsInput{
		IncludeGitHubOutcomes: true,
	})

	require.NoError(t, err)
	assert.Equal(t, "server-token", spy.got.GHToken)
}

func TestHumaBatchDeleteRestoresOnlyUnpublishedNewDeletions(t *testing.T) {
	tests := []struct {
		name        string
		failure     error
		wantDeleted []string
	}{
		{
			name:        "ordinary append failure restores current and later",
			failure:     errors.New("artifact write failed"),
			wantDeleted: []string{"already-trashed", "s1"},
		},
		{
			name: "published failure keeps current and restores later",
			failure: &artifact.MetadataPublishedError{
				Err: errors.New("replay bookkeeping failed"),
			},
			wantDeleted: []string{"already-trashed", "s1", "s2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			for _, id := range []string{"already-trashed", "s1", "s2", "s3"} {
				dbtest.SeedSession(t, database, id, "alpha")
			}
			require.NoError(t, database.SoftDeleteSession("already-trashed"))

			var appended []string
			srv := &Server{
				db: database,
				metadataAppend: func(
					_ context.Context, input artifact.MetadataEventInput,
				) error {
					appended = append(appended, input.SessionID)
					if input.SessionID == "s2" {
						return tt.failure
					}
					return nil
				},
			}
			in := &batchDeleteInput{}
			in.Body.SessionIDs = []string{"already-trashed", "s1", "s2", "s3"}

			_, err := srv.humaBatchDeleteSessions(context.Background(), in)

			require.Error(t, err)
			assert.Equal(t, []string{"s1", "s2"}, appended,
				"publication must stop at the first failed event")
			for _, id := range []string{"already-trashed", "s1", "s2", "s3"} {
				session, getErr := database.GetSessionFull(context.Background(), id)
				require.NoError(t, getErr)
				require.NotNil(t, session)
				assert.Equal(t, containsString(tt.wantDeleted, id), session.DeletedAt != nil,
					"unexpected trash state for %s", id)
			}
		})
	}
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

type batchRestoreStore struct {
	db.Store
	restored []string
	failID   string
	failErr  error
}

func (s *batchRestoreStore) RestoreSession(id string) (int64, error) {
	s.restored = append(s.restored, id)
	if id == s.failID {
		return 0, s.failErr
	}
	return 1, nil
}

func TestRestoreBatchDeletedSessionsJoinsFailuresAndContinues(t *testing.T) {
	cause := errors.New("metadata append failed")
	restoreErr := errors.New("restore failed")
	store := &batchRestoreStore{failID: "s2", failErr: restoreErr}
	srv := &Server{db: store}

	err := srv.restoreBatchDeletedSessions([]string{"s2", "s3"}, cause)

	assert.ErrorIs(t, err, cause)
	assert.ErrorIs(t, err, restoreErr)
	assert.Equal(t, []string{"s2", "s3"}, store.restored,
		"a restoration failure must not prevent later sessions from being restored")
}

func TestRestoreLifecycleWaitsForFailedRestoreCompensation(t *testing.T) {
	const origin = "desk-a1b2c3"
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "alpha")
	require.NoError(t, database.SoftDeleteSession("s1"))
	recordSoftDeleteReplayState(t, database, origin, "s1")

	firstAppendStarted := make(chan struct{})
	secondAppendStarted := make(chan struct{})
	secondLockAttempted := make(chan struct{})
	releaseFirstAppend := make(chan struct{})
	var appendCalls atomic.Int32
	var lockAttempts atomic.Int32
	srv := &Server{
		db:  database,
		cfg: config.Config{ArtifactOriginID: origin},
		beforeSessionLifecycleLock: func() {
			if lockAttempts.Add(1) == 2 {
				close(secondLockAttempted)
			}
		},
		metadataAppend: func(
			_ context.Context, input artifact.MetadataEventInput,
		) error {
			if input.Op != artifact.MetadataOpRestore {
				return errors.New("unexpected metadata op")
			}
			switch appendCalls.Add(1) {
			case 1:
				close(firstAppendStarted)
				<-releaseFirstAppend
				return errors.New("artifact write failed")
			case 2:
				close(secondAppendStarted)
				return nil
			default:
				return errors.New("unexpected extra metadata append")
			}
		},
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := srv.humaRestoreSession(context.Background(), &idPathInput{ID: "s1"})
		firstResult <- err
	}()
	<-firstAppendStarted

	secondResult := make(chan error, 1)
	go func() {
		_, err := srv.humaRestoreSession(context.Background(), &idPathInput{ID: "s1"})
		secondResult <- err
	}()
	<-secondLockAttempted

	interleaved := false
	select {
	case <-secondAppendStarted:
		interleaved = true
	case <-time.After(500 * time.Millisecond):
	}
	close(releaseFirstAppend)
	firstErr := <-firstResult
	secondErr := <-secondResult

	assert.False(t, interleaved,
		"a second restore must not publish before the first restore compensates")
	require.Error(t, firstErr)
	require.NoError(t, secondErr)
	assert.Equal(t, int32(2), appendCalls.Load())
	session, err := database.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Nil(t, session.DeletedAt,
		"the later successful restore must remain visible locally")
}

func TestPermanentDeleteLifecycleExcludesConcurrentRestore(t *testing.T) {
	const origin = "desk-a1b2c3"
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "alpha")
	require.NoError(t, database.SoftDeleteSession("s1"))
	recordSoftDeleteReplayState(t, database, origin, "s1")

	firstAppendStarted := make(chan struct{})
	secondAppendStarted := make(chan struct{})
	secondLockAttempted := make(chan struct{})
	releaseFirstAppend := make(chan struct{})
	var appendCalls atomic.Int32
	var lockAttempts atomic.Int32
	srv := &Server{
		db:  database,
		cfg: config.Config{ArtifactOriginID: origin},
		beforeSessionLifecycleLock: func() {
			if lockAttempts.Add(1) == 2 {
				close(secondLockAttempted)
			}
		},
		metadataAppend: func(
			_ context.Context, input artifact.MetadataEventInput,
		) error {
			switch appendCalls.Add(1) {
			case 1:
				if input.Op != artifact.MetadataOpPurge {
					return errors.New("unexpected first metadata op")
				}
				close(firstAppendStarted)
				<-releaseFirstAppend
				return nil
			case 2:
				if input.Op != artifact.MetadataOpRestore {
					return errors.New("unexpected second metadata op")
				}
				close(secondAppendStarted)
				return nil
			default:
				return errors.New("unexpected extra metadata append")
			}
		},
	}

	purgeResult := make(chan error, 1)
	go func() {
		_, err := srv.humaPermanentDeleteSession(
			context.Background(), &idPathInput{ID: "s1"},
		)
		purgeResult <- err
	}()
	<-firstAppendStarted

	restoreResult := make(chan error, 1)
	go func() {
		_, err := srv.humaRestoreSession(context.Background(), &idPathInput{ID: "s1"})
		restoreResult <- err
	}()
	<-secondLockAttempted

	interleaved := false
	select {
	case <-secondAppendStarted:
		interleaved = true
	case <-time.After(500 * time.Millisecond):
	}
	close(releaseFirstAppend)
	purgeErr := <-purgeResult
	restoreErr := <-restoreResult

	assert.False(t, interleaved,
		"restore must not publish while a purge has reserved the trashed session")
	require.NoError(t, purgeErr)
	require.Error(t, restoreErr)
	assert.Equal(t, int32(1), appendCalls.Load())
	session, err := database.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	assert.Nil(t, session,
		"a durable purge must delete the local session before restore can run")
}

func TestPermanentDeleteLifecycleExcludesConcurrentPeerRestore(t *testing.T) {
	const (
		localOrigin = "desk-a1b2c3"
		peerOrigin  = "peer-b2c3d4"
	)
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "alpha")
	require.NoError(t, database.SoftDeleteSession("s1"))
	recordSoftDeleteReplayState(t, database, localOrigin, "s1")

	firstAppendStarted := make(chan struct{})
	secondLockAttempted := make(chan struct{})
	releaseFirstAppend := make(chan struct{})
	var appendCalls atomic.Int32
	var lockAttempts atomic.Int32
	srv := newArtifactHandlerTestServer(t, database, config.Config{
		ArtifactOriginID: localOrigin,
	})
	srv.beforeSessionLifecycleLock = func() {
		if lockAttempts.Add(1) == 2 {
			close(secondLockAttempted)
		}
	}
	srv.metadataAppend = func(
		_ context.Context, input artifact.MetadataEventInput,
	) error {
		if appendCalls.Add(1) != 1 || input.Op != artifact.MetadataOpPurge {
			return errors.New("unexpected metadata append")
		}
		close(firstAppendStarted)
		<-releaseFirstAppend
		return nil
	}

	purgeResult := make(chan error, 1)
	go func() {
		_, err := srv.humaPermanentDeleteSession(
			context.Background(), &idPathInput{ID: "s1"},
		)
		purgeResult <- err
	}()
	<-firstAppendStarted

	body, name := peerRestoreArtifact(
		peerOrigin, artifact.MetadataSessionGID(localOrigin, "s1"),
	)
	peerResult := make(chan error, 1)
	go func() {
		_, err := srv.humaPostArtifact(context.Background(), &artifactPostInput{
			Origin: peerOrigin,
			Kind:   artifact.KindMeta,
			Name:   name,
			Body:   bytes.NewReader(body),
		})
		peerResult <- err
	}()

	peerCompletedBeforeRelease := false
	var peerErr error
	select {
	case <-secondLockAttempted:
		select {
		case peerErr = <-peerResult:
			peerCompletedBeforeRelease = true
		case <-time.After(500 * time.Millisecond):
		}
	case peerErr = <-peerResult:
		peerCompletedBeforeRelease = true
	case <-time.After(2 * time.Second):
		require.FailNow(t, "peer restore reached neither lifecycle lock nor completion")
	}
	close(releaseFirstAppend)
	purgeErr := <-purgeResult
	if !peerCompletedBeforeRelease {
		peerErr = <-peerResult
	}

	assert.False(t, peerCompletedBeforeRelease,
		"peer restore import must wait for the durable purge to delete locally")
	require.NoError(t, purgeErr)
	require.NoError(t, peerErr)
	assert.Equal(t, int32(1), appendCalls.Load())
	session, err := database.GetSessionFull(context.Background(), "s1")
	require.NoError(t, err)
	assert.Nil(t, session,
		"peer restore must not interleave between purge publication and deletion")
}

func TestArtifactContentPostDefersDatabaseImport(t *testing.T) {
	ctx := context.Background()
	const peerOrigin = "peer-b2c3d4"
	peerDB := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, peerDB, "s1", "alpha")
	peerStore := exportHumaArtifactFixture(t, ctx, peerDB, peerOrigin)
	segmentRef := oneHumaArtifactRef(t, peerStore, peerOrigin, artifact.KindSegments)
	segmentWire, segmentData := humaWireArtifact(t, peerStore, segmentRef)
	require.NotEmpty(t, segmentData)

	local := dbtest.OpenTestDB(t)
	server := newArtifactHandlerTestServer(t, local, config.Config{})
	_, err := server.humaPostArtifact(ctx, &artifactPostInput{
		Origin: peerOrigin,
		Kind:   artifact.KindSegments,
		Name:   segmentWire.Name,
		Body:   bytes.NewReader(segmentData),
	})
	require.NoError(t, err)

	localOrigin, err := artifact.StoredOrigin(local)
	require.NoError(t, err)
	assert.Empty(t, localOrigin,
		"content-only upload must not trigger a full import or enroll the receiver")
}

func exportHumaArtifactFixture(
	t *testing.T, ctx context.Context, database *db.DB, origin string,
) artifact.ArtifactStore {
	t.Helper()
	repository, err := artifact.OpenRepository(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	_, err = artifact.ExportToStore(ctx, database, repository.Content(), artifact.ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)
	return repository.Content()
}

func oneHumaArtifactRef(
	t *testing.T, store artifact.ArtifactStore, origin string, kind artifact.Kind,
) artifact.Ref {
	t.Helper()
	entries := collectArtifactEntries(t, store, origin, kind, 10)
	require.Len(t, entries, 1)
	return entries[0].Ref
}

func humaWireArtifact(
	t *testing.T, store artifact.ArtifactStore, ref artifact.Ref,
) (artifact.WireRef, []byte) {
	t.Helper()
	_, reader, err := store.Open(t.Context(), ref)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	wire, err := artifact.ToWireRef(ref)
	require.NoError(t, err)
	var body bytes.Buffer
	require.NoError(t, artifact.EncodeWire(t.Context(), ref, reader, &body))
	require.NoError(t, reader.Verify())
	return wire, body.Bytes()
}

func TestArtifactDependencyPostRetriesDeferredCheckpointImport(t *testing.T) {
	ctx := context.Background()
	const peerOrigin = "peer-b2c3d4"
	peerDB := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, peerDB, "s1", "alpha")
	peerStore := exportHumaArtifactFixture(t, ctx, peerDB, peerOrigin)
	checkpoint := oneHumaArtifactRef(t, peerStore, peerOrigin, artifact.KindCheckpoints)
	manifest := oneHumaArtifactRef(t, peerStore, peerOrigin, artifact.KindManifests)
	segment := oneHumaArtifactRef(t, peerStore, peerOrigin, artifact.KindSegments)

	local := dbtest.OpenTestDB(t)
	server := newArtifactHandlerTestServer(t, local, config.Config{})
	post := func(ref artifact.Ref) {
		t.Helper()
		wire, data := humaWireArtifact(t, peerStore, ref)
		require.NotEmpty(t, data)
		_, postErr := server.humaPostArtifact(ctx, &artifactPostInput{
			Origin: peerOrigin, Kind: string(ref.Kind), Name: wire.Name, Body: bytes.NewReader(data),
		})
		require.NoError(t, postErr)
	}

	post(checkpoint)
	post(manifest)
	got, err := local.GetSession(ctx, peerOrigin+"~s1")
	require.NoError(t, err)
	assert.Nil(t, got)

	post(segment)
	got, err = local.GetSession(ctx, peerOrigin+"~s1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "alpha", got.Project)
}

func peerRestoreArtifact(origin, sessionGID string) ([]byte, string) {
	const hlc = "2026-07-10T010203.000000001Z-00000000000000000000"
	body := []byte(`{"hlc":"` + hlc + `","op":"restore","origin":"` + origin +
		`","session_gid":"` + sessionGID + `","v":1}` + "\n")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	return body, hlc + "-" + hash + ".json"
}

func recordSoftDeleteReplayState(t *testing.T, database *db.DB, origin, sessionID string) {
	t.Helper()
	_, err := database.RecordLocalMetadataProjection(context.Background(), db.MetadataProjection{
		EventOrigin:    origin,
		OrderKey:       "0001-soft-delete",
		HLC:            "0001",
		ArtifactHash:   "soft-delete",
		SessionGID:     artifact.MetadataSessionGID(origin, sessionID),
		LocalSessionID: sessionID,
		Field:          "deleted_at",
		Op:             artifact.MetadataOpSoftDelete,
		Value:          artifact.MetadataOpSoftDelete,
	})
	require.NoError(t, err)
}

type curationAppendGate struct {
	firstStarted  chan struct{}
	secondStarted chan struct{}
	releaseFirst  chan struct{}
	releaseOnce   sync.Once
	calls         atomic.Int32
	inputs        [2]artifact.MetadataEventInput
}

func newCurationAppendGate() *curationAppendGate {
	return &curationAppendGate{
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
}

func (g *curationAppendGate) append(
	_ context.Context, input artifact.MetadataEventInput,
) error {
	call := g.calls.Add(1)
	if call <= int32(len(g.inputs)) {
		g.inputs[call-1] = input
	}
	switch call {
	case 1:
		close(g.firstStarted)
		<-g.releaseFirst
		return errors.New("artifact write failed")
	case 2:
		close(g.secondStarted)
		return nil
	default:
		return errors.New("unexpected extra metadata append")
	}
}

func (g *curationAppendGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseFirst) })
}

func curationMetadataRecorder(t *testing.T, database *db.DB) *artifact.MetadataRecorder {
	t.Helper()
	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	return artifact.NewMetadataRecorder(database, artifact.MetadataRecorderOptions{
		Store:  repository.Content(),
		Origin: "desk-a1b2c3",
	})
}

func waitForSecondCurationRequest(
	t *testing.T,
	secondLockAttempted <-chan struct{},
	secondAppendStarted <-chan struct{},
) bool {
	t.Helper()
	select {
	case <-secondLockAttempted:
		select {
		case <-secondAppendStarted:
			return true
		case <-time.After(500 * time.Millisecond):
			return false
		}
	case <-secondAppendStarted:
		return true
	case <-time.After(2 * time.Second):
		require.FailNow(t, "second curation request reached neither lifecycle lock nor metadata append")
		return false
	}
}

func TestRenameLifecycleWaitsForFailedAppendCompensation(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	original := "original"
	dbtest.SeedSession(t, database, "s1", "alpha", func(session *db.Session) {
		session.DisplayName = &original
	})

	gate := newCurationAppendGate()
	t.Cleanup(gate.release)
	secondLockAttempted := make(chan struct{})
	var lockAttempts atomic.Int32
	srv := &Server{
		db: database,
		beforeSessionLifecycleLock: func() {
			if lockAttempts.Add(1) == 2 {
				close(secondLockAttempted)
			}
		},
		metadataAppend: gate.append,
	}

	firstName := "first"
	firstResult := make(chan error, 1)
	go func() {
		_, err := srv.humaRenameSession(context.Background(), &renameSessionInput{
			ID:   "s1",
			Body: renameRequest{DisplayName: &firstName},
		})
		firstResult <- err
	}()
	<-gate.firstStarted

	secondName := "second"
	secondResult := make(chan error, 1)
	go func() {
		_, err := srv.humaRenameSession(context.Background(), &renameSessionInput{
			ID:   "s1",
			Body: renameRequest{DisplayName: &secondName},
		})
		secondResult <- err
	}()

	interleaved := waitForSecondCurationRequest(
		t, secondLockAttempted, gate.secondStarted,
	)
	gate.release()
	firstErr := <-firstResult
	secondErr := <-secondResult

	assert.False(t, interleaved,
		"a later rename must not publish before the failed rename compensates")
	require.Error(t, firstErr)
	require.NoError(t, secondErr)
	assert.Equal(t, int32(2), gate.calls.Load())
	assert.Equal(t, artifact.MetadataOpRename, gate.inputs[0].Op)
	assert.Equal(t, artifact.MetadataOpRename, gate.inputs[1].Op)
	assert.JSONEq(t, `{"display_name":"second"}`, string(gate.inputs[1].Value))
	session, err := database.GetSession(context.Background(), "s1")
	require.NoError(t, err)
	require.NotNil(t, session)
	require.NotNil(t, session.DisplayName)
	assert.Equal(t, "second", *session.DisplayName,
		"SQLite must retain the value represented by the later durable rename")
}

func TestStarLifecycleWaitsForFailedAppendCompensation(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "alpha")

	gate := newCurationAppendGate()
	t.Cleanup(gate.release)
	secondLockAttempted := make(chan struct{})
	var lockAttempts atomic.Int32
	srv := &Server{
		db:       database,
		metadata: curationMetadataRecorder(t, database),
		beforeSessionLifecycleLock: func() {
			if lockAttempts.Add(1) == 2 {
				close(secondLockAttempted)
			}
		},
		metadataAppend: gate.append,
	}

	firstResult := make(chan error, 1)
	go func() {
		_, err := srv.humaStarSession(context.Background(), &idPathInput{ID: "s1"})
		firstResult <- err
	}()
	<-gate.firstStarted

	secondResult := make(chan error, 1)
	go func() {
		_, err := srv.humaStarSession(context.Background(), &idPathInput{ID: "s1"})
		secondResult <- err
	}()

	interleaved := waitForSecondCurationRequest(
		t, secondLockAttempted, gate.secondStarted,
	)
	gate.release()
	firstErr := <-firstResult
	secondErr := <-secondResult

	assert.False(t, interleaved,
		"a later star must not publish before the failed star compensates")
	require.Error(t, firstErr)
	require.NoError(t, secondErr)
	assert.Equal(t, int32(2), gate.calls.Load())
	assert.Equal(t, artifact.MetadataOpStar, gate.inputs[0].Op)
	assert.Equal(t, artifact.MetadataOpStar, gate.inputs[1].Op)
	starred, err := database.ListStarredSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"s1"}, starred,
		"SQLite must retain the star represented by the later durable event")
}

func seedCurationPinMessage(t *testing.T, database *db.DB) int64 {
	t.Helper()
	dbtest.SeedSession(t, database, "s1", "alpha")
	message := dbtest.UserMsg("s1", 0, "investigate")
	message.SourceUUID = "message-a1b2c3"
	dbtest.SeedMessages(t, database, message)
	messages, err := database.GetAllMessages(context.Background(), "s1")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	return messages[0].ID
}

type metadataPinPointLookupStore struct {
	db.Store
	message         *db.Message
	fullReads       int
	pointReads      int
	lookupSessionID string
	lookupMessageID int64
}

func (s *metadataPinPointLookupStore) GetAllMessages(
	_ context.Context, _ string,
) ([]db.Message, error) {
	s.fullReads++
	return nil, errors.New("full transcript read must not be used for pin metadata")
}

func (s *metadataPinPointLookupStore) GetMessageForMetadataPin(
	_ context.Context, sessionID string, messageID int64,
) (*db.Message, error) {
	s.pointReads++
	s.lookupSessionID = sessionID
	s.lookupMessageID = messageID
	return s.message, nil
}

func TestMetadataPinUsesPointMessageLookup(t *testing.T) {
	store := &metadataPinPointLookupStore{message: &db.Message{
		ID:         42,
		SessionID:  "s1",
		Ordinal:    7,
		SourceUUID: "message-a1b2c3",
	}}
	srv := &Server{db: store}
	note := "remember"

	pin, err := srv.metadataPinForMessage(
		context.Background(), "s1", 42, &note,
	)
	require.NoError(t, err)
	require.NotNil(t, pin)
	assert.Equal(t, "message-a1b2c3", pin.SourceUUID)
	assert.Equal(t, 7, pin.Ordinal)
	require.NotNil(t, pin.Note)
	assert.Equal(t, "remember", *pin.Note)
	assert.Equal(t, 1, store.pointReads)
	assert.Zero(t, store.fullReads)
	assert.Equal(t, "s1", store.lookupSessionID)
	assert.Equal(t, int64(42), store.lookupMessageID)
}

func TestPinLifecycleWaitsForFailedAppendCompensation(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	messageID := seedCurationPinMessage(t, database)

	gate := newCurationAppendGate()
	t.Cleanup(gate.release)
	secondLockAttempted := make(chan struct{})
	var lockAttempts atomic.Int32
	srv := &Server{
		db:       database,
		metadata: curationMetadataRecorder(t, database),
		beforeSessionLifecycleLock: func() {
			if lockAttempts.Add(1) == 2 {
				close(secondLockAttempted)
			}
		},
		metadataAppend: gate.append,
	}

	firstNote := "first"
	firstResult := make(chan error, 1)
	go func() {
		_, err := srv.humaPinMessage(context.Background(), &pinMessageInput{
			ID:        "s1",
			MessageID: messageID,
			Body:      pinRequest{Note: &firstNote},
		})
		firstResult <- err
	}()
	<-gate.firstStarted

	secondNote := "second"
	secondResult := make(chan error, 1)
	go func() {
		_, err := srv.humaPinMessage(context.Background(), &pinMessageInput{
			ID:        "s1",
			MessageID: messageID,
			Body:      pinRequest{Note: &secondNote},
		})
		secondResult <- err
	}()

	interleaved := waitForSecondCurationRequest(
		t, secondLockAttempted, gate.secondStarted,
	)
	gate.release()
	firstErr := <-firstResult
	secondErr := <-secondResult

	assert.False(t, interleaved,
		"a later pin must not publish before the failed pin compensates")
	require.Error(t, firstErr)
	require.NoError(t, secondErr)
	assert.Equal(t, int32(2), gate.calls.Load())
	assert.Equal(t, artifact.MetadataOpPin, gate.inputs[0].Op)
	assert.Equal(t, artifact.MetadataOpPin, gate.inputs[1].Op)
	require.NotNil(t, gate.inputs[1].Pin)
	require.NotNil(t, gate.inputs[1].Pin.Note)
	assert.Equal(t, "second", *gate.inputs[1].Pin.Note)
	pins, err := database.ListPinnedMessages(context.Background(), "s1", "")
	require.NoError(t, err)
	require.Len(t, pins, 1,
		"SQLite must retain the pin represented by the later durable event")
	require.NotNil(t, pins[0].Note)
	assert.Equal(t, "second", *pins[0].Note)
}

func assertCurationMutationWaitsForLifecycle(
	t *testing.T,
	srv *Server,
	wantOp string,
	mutate func() error,
	assertBefore func(),
	assertAfter func(),
) {
	t.Helper()
	lockAttempted := make(chan struct{})
	appended := make(chan artifact.MetadataEventInput, 2)
	var lockAttempts atomic.Int32
	srv.beforeSessionLifecycleLock = func() {
		if lockAttempts.Add(1) == 1 {
			close(lockAttempted)
		}
	}
	srv.metadataAppend = func(
		_ context.Context, input artifact.MetadataEventInput,
	) error {
		appended <- input
		return nil
	}

	srv.sessionLifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.sessionLifecycleMu.Unlock()
		}
	}()

	result := make(chan error, 1)
	go func() { result <- mutate() }()

	completedBeforeLock := false
	var mutationErr error
	select {
	case <-lockAttempted:
	case mutationErr = <-result:
		completedBeforeLock = true
	case <-time.After(2 * time.Second):
		require.FailNow(t, "curation mutation reached neither lifecycle lock nor completion")
	}
	assert.False(t, completedBeforeLock,
		"curation mutation must wait for the shared lifecycle boundary")
	assertBefore()

	srv.sessionLifecycleMu.Unlock()
	locked = false
	if !completedBeforeLock {
		mutationErr = <-result
	}
	require.NoError(t, mutationErr)
	assertAfter()
	assert.Equal(t, int32(1), lockAttempts.Load())
	select {
	case input := <-appended:
		assert.Equal(t, wantOp, input.Op)
	case <-time.After(2 * time.Second):
		require.FailNow(t, "curation mutation did not append metadata")
	}
	select {
	case input := <-appended:
		assert.Fail(t, "curation mutation appended extra metadata", "op: %s", input.Op)
	default:
	}
}

func TestUnstarMutationWaitsForSessionLifecycle(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "alpha")
	starred, err := database.StarSession("s1")
	require.NoError(t, err)
	require.True(t, starred)
	srv := &Server{db: database}

	assertStarred := func(want []string) func() {
		return func() {
			ids, listErr := database.ListStarredSessionIDs(context.Background())
			require.NoError(t, listErr)
			assert.ElementsMatch(t, want, ids)
		}
	}
	assertCurationMutationWaitsForLifecycle(
		t,
		srv,
		artifact.MetadataOpUnstar,
		func() error {
			_, handlerErr := srv.humaUnstarSession(
				context.Background(), &idPathInput{ID: "s1"},
			)
			return handlerErr
		},
		assertStarred([]string{"s1"}),
		assertStarred([]string{}),
	)
}

func TestBulkStarMutationWaitsForSessionLifecycle(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "s1", "alpha")
	srv := &Server{db: database}
	in := &bulkStarInput{}
	in.Body.SessionIDs = []string{"s1"}

	assertStarred := func(want []string) func() {
		return func() {
			ids, err := database.ListStarredSessionIDs(context.Background())
			require.NoError(t, err)
			assert.ElementsMatch(t, want, ids)
		}
	}
	assertCurationMutationWaitsForLifecycle(
		t,
		srv,
		artifact.MetadataOpStar,
		func() error {
			_, err := srv.humaBulkStar(context.Background(), in)
			return err
		},
		assertStarred([]string{}),
		assertStarred([]string{"s1"}),
	)
}

func TestBulkStarEmptyInputDoesNotWaitForSessionLifecycle(t *testing.T) {
	lockAttempted := make(chan struct{})
	var appendCalls atomic.Int32
	srv := &Server{
		beforeSessionLifecycleLock: func() { close(lockAttempted) },
		metadataAppend: func(
			_ context.Context, _ artifact.MetadataEventInput,
		) error {
			appendCalls.Add(1)
			return nil
		},
	}
	srv.sessionLifecycleMu.Lock()
	locked := true
	defer func() {
		if locked {
			srv.sessionLifecycleMu.Unlock()
		}
	}()

	result := make(chan error, 1)
	go func() {
		_, err := srv.humaBulkStar(context.Background(), &bulkStarInput{})
		result <- err
	}()

	select {
	case err := <-result:
		require.NoError(t, err)
	case <-lockAttempted:
		srv.sessionLifecycleMu.Unlock()
		locked = false
		<-result
		require.FailNow(t, "empty bulk star waited for the lifecycle boundary")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "empty bulk star did not complete")
	}
	assert.Equal(t, int32(0), appendCalls.Load())
}

func TestUnpinMutationWaitsForSessionLifecycle(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	messageID := seedCurationPinMessage(t, database)
	note := "keep"
	_, err := database.PinMessage("s1", messageID, &note)
	require.NoError(t, err)
	srv := &Server{
		db:       database,
		metadata: curationMetadataRecorder(t, database),
	}

	assertPinned := func(want bool) func() {
		return func() {
			pins, listErr := database.ListPinnedMessages(context.Background(), "s1", "")
			require.NoError(t, listErr)
			if !want {
				assert.Empty(t, pins)
				return
			}
			require.Len(t, pins, 1)
			require.NotNil(t, pins[0].Note)
			assert.Equal(t, "keep", *pins[0].Note)
		}
	}
	assertCurationMutationWaitsForLifecycle(
		t,
		srv,
		artifact.MetadataOpUnpin,
		func() error {
			_, handlerErr := srv.humaUnpinMessage(context.Background(), &messagePathInput{
				ID:        "s1",
				MessageID: messageID,
			})
			return handlerErr
		},
		assertPinned(true),
		assertPinned(false),
	)
}
