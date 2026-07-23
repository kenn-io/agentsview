package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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
)

const artifactLifecycleOrigin = "lifecycle-a1b2c3"

type lifecycleArtifactStore struct {
	mu            sync.Mutex
	entries       map[artifact.Ref][]byte
	corrupt       map[artifact.Ref]bool
	openStarted   chan struct{}
	openRelease   chan struct{}
	createStarted chan struct{}
	createRelease chan struct{}
	closeCalls    atomic.Int32
	closed        atomic.Bool
	closeErr      error
}

type repairGateArtifactStore struct {
	artifact.ArtifactStore
	started chan struct{}
	err     error
}

func (s *lifecycleArtifactStore) RepairContent(
	context.Context, artifact.Identity, io.Reader,
) error {
	return artifact.ErrArtifactUnsupported
}

func (s *repairGateArtifactStore) RepairContent(
	ctx context.Context, _ artifact.Identity, _ io.Reader,
) error {
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}
	if s.err != nil {
		return s.err
	}
	<-ctx.Done()
	return ctx.Err()
}

func newLifecycleArtifactStore() *lifecycleArtifactStore {
	return &lifecycleArtifactStore{
		entries: make(map[artifact.Ref][]byte), corrupt: make(map[artifact.Ref]bool),
	}
}

func (s *lifecycleArtifactStore) Create(
	ctx context.Context,
	ref artifact.Ref,
	identity artifact.Identity,
	_ string,
	body io.Reader,
) (artifact.CreateResult, error) {
	if s.createStarted != nil {
		select {
		case s.createStarted <- struct{}{}:
		default:
		}
		select {
		case <-s.createRelease:
		case <-ctx.Done():
			return artifact.CreateResult{}, ctx.Err()
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return artifact.CreateResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return artifact.CreateResult{}, err
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != identity.SHA256 || int64(len(data)) != identity.Size {
		return artifact.CreateResult{}, artifact.ErrArtifactInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.entries[ref]; ok {
		if !bytes.Equal(existing, data) {
			return artifact.CreateResult{}, artifact.ErrArtifactConflict
		}
		return artifact.CreateResult{Entry: lifecycleEntry(ref, data)}, nil
	}
	s.entries[ref] = append([]byte(nil), data...)
	return artifact.CreateResult{Entry: lifecycleEntry(ref, data), Created: true}, nil
}

func (s *lifecycleArtifactStore) Stat(
	ctx context.Context, ref artifact.Ref,
) (artifact.Entry, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.entries[ref]
	if !ok {
		return artifact.Entry{}, artifact.ErrArtifactNotFound
	}
	return lifecycleEntry(ref, data), nil
}

func (s *lifecycleArtifactStore) Open(
	ctx context.Context, ref artifact.Ref,
) (artifact.Entry, artifact.VerifiedReader, error) {
	if s.openStarted != nil {
		select {
		case s.openStarted <- struct{}{}:
		default:
		}
		select {
		case <-s.openRelease:
		case <-ctx.Done():
			return artifact.Entry{}, nil, ctx.Err()
		}
	}
	entry, err := s.Stat(ctx, ref)
	if err != nil {
		return artifact.Entry{}, nil, err
	}
	s.mu.Lock()
	data := append([]byte(nil), s.entries[ref]...)
	corrupt := s.corrupt[ref]
	s.mu.Unlock()
	return entry, &lifecycleVerifiedReader{
		Reader: bytes.NewReader(data), corrupt: corrupt,
	}, nil
}

func (s *lifecycleArtifactStore) ListOrigins(
	ctx context.Context, cursor artifact.Cursor, limit int,
) ([]string, artifact.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if cursor != "" || limit < 1 {
		return nil, "", artifact.ErrArtifactInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ref := range s.entries {
		return []string{ref.Origin}, "", nil
	}
	return []string{}, "", nil
}

func (s *lifecycleArtifactStore) List(
	ctx context.Context,
	origin string,
	kind artifact.Kind,
	cursor artifact.Cursor,
	limit int,
) (artifact.Page, error) {
	if err := ctx.Err(); err != nil {
		return artifact.Page{}, err
	}
	if cursor != "" || limit < 1 {
		return artifact.Page{}, artifact.ErrArtifactInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	page := artifact.Page{}
	for ref, data := range s.entries {
		if ref.Origin == origin && ref.Kind == kind {
			page.Items = append(page.Items, lifecycleEntry(ref, data))
		}
	}
	return page, nil
}

func (s *lifecycleArtifactStore) Origins(context.Context) (artifact.OriginIterator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{})
	for ref := range s.entries {
		seen[ref.Origin] = struct{}{}
	}
	items := make([]string, 0, len(seen))
	for origin := range seen {
		items = append(items, origin)
	}
	sort.Strings(items)
	return &lifecycleOriginIterator{items: items}, nil
}

func (s *lifecycleArtifactStore) Entries(
	_ context.Context, origin string, kind artifact.Kind,
) (artifact.EntryIterator, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]artifact.Entry, 0)
	for ref, data := range s.entries {
		if ref.Origin == origin && ref.Kind == kind {
			items = append(items, lifecycleEntry(ref, data))
		}
	}
	sort.Slice(items, func(left, right int) bool {
		return items[left].Ref.Name < items[right].Ref.Name
	})
	return &lifecycleEntryIterator{items: items}, nil
}

type lifecycleOriginIterator struct {
	items  []string
	offset int
	closed bool
}

func (i *lifecycleOriginIterator) Next(ctx context.Context, limit int) ([]string, error) {
	if i.closed {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, artifact.ErrArtifactInvalid
	}
	end := min(i.offset+limit, len(i.items))
	page := append([]string(nil), i.items[i.offset:end]...)
	i.offset = end
	if end == len(i.items) {
		return page, io.EOF
	}
	return page, nil
}

func (i *lifecycleOriginIterator) Close() error {
	i.closed = true
	return nil
}

type lifecycleEntryIterator struct {
	items  []artifact.Entry
	offset int
	closed bool
}

func (i *lifecycleEntryIterator) Next(
	ctx context.Context, limit int,
) ([]artifact.Entry, error) {
	if i.closed {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, artifact.ErrArtifactInvalid
	}
	end := min(i.offset+limit, len(i.items))
	page := append([]artifact.Entry(nil), i.items[i.offset:end]...)
	i.offset = end
	if end == len(i.items) {
		return page, io.EOF
	}
	return page, nil
}

func (i *lifecycleEntryIterator) Close() error {
	i.closed = true
	return nil
}

func (*lifecycleArtifactStore) Quarantine(context.Context, artifact.Ref, string) error {
	return nil
}

func (*lifecycleArtifactStore) Trash(context.Context, artifact.Ref) error { return nil }

func (*lifecycleArtifactStore) Pack(context.Context, int64) (artifact.PackResult, error) {
	return artifact.PackResult{}, nil
}

func (*lifecycleArtifactStore) LooseBacklog(context.Context) (artifact.LooseBacklog, error) {
	return artifact.LooseBacklog{}, nil
}

func (s *lifecycleArtifactStore) Close() error {
	s.closeCalls.Add(1)
	s.closed.Store(true)
	return s.closeErr
}

func lifecycleEntry(ref artifact.Ref, data []byte) artifact.Entry {
	hash := sha256.Sum256(data)
	return artifact.Entry{Ref: ref, Identity: artifact.Identity{
		SHA256: hex.EncodeToString(hash[:]),
		Size:   int64(len(data)),
	}}
}

type lifecycleVerifiedReader struct {
	*bytes.Reader
	closed  bool
	corrupt bool
}

func (r *lifecycleVerifiedReader) Verify() error {
	if r.closed {
		return errors.New("reader closed")
	}
	_, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if r.corrupt {
		return artifact.ErrArtifactCorrupt
	}
	return nil
}

func (r *lifecycleVerifiedReader) Close() error {
	r.closed = true
	return nil
}

func TestArtifactResetDrainsActiveOperationsBeforeStoreSwap(t *testing.T) {
	oldStore := newLifecycleArtifactStore()
	newStore := newLifecycleArtifactStore()
	lifetime := artifactOperationLifetime{store: oldStore}
	store, release, err := lifetime.acquire()
	require.NoError(t, err)
	assert.Same(t, oldStore, store)
	type resetDrainResult struct {
		store artifact.ArtifactStore
		err   error
	}
	drained := make(chan resetDrainResult, 1)
	go func() {
		owned, _, drainErr := lifetime.beginReset(t.Context())
		drained <- resetDrainResult{store: owned, err: drainErr}
	}()

	require.Eventually(t, func() bool {
		_, probeRelease, probeErr := lifetime.acquire()
		if probeErr != nil {
			return true
		}
		probeRelease()
		return false
	}, time.Second, time.Millisecond, "reset never entered its drain state")
	select {
	case <-drained:
		t.Fatal("reset passed an active artifact operation")
	default:
	}
	release()
	drainResult := <-drained
	require.NoError(t, drainResult.err)
	assert.Same(t, oldStore, drainResult.store)
	require.NoError(t, lifetime.finishReset(newStore))

	store, release, err = lifetime.acquire()
	require.NoError(t, err)
	assert.Same(t, newStore, store)
	release()
}

func TestArtifactResetCanceledWhileDrainingUnwindsResetState(t *testing.T) {
	store := newLifecycleArtifactStore()
	lifetime := artifactOperationLifetime{store: store}
	_, release, err := lifetime.acquire()
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	drained := make(chan error, 1)
	go func() {
		_, _, drainErr := lifetime.beginReset(ctx)
		drained <- drainErr
	}()

	require.Eventually(t, func() bool {
		_, probeRelease, probeErr := lifetime.acquire()
		if probeErr != nil {
			return true
		}
		probeRelease()
		return false
	}, time.Second, time.Millisecond, "reset never entered its drain state")
	cancel()
	require.ErrorIs(t, <-drained, context.Canceled)
	release()

	acquired, releaseAgain, err := lifetime.acquire()
	require.NoError(t, err)
	assert.Same(t, store, acquired)
	releaseAgain()
}

func TestArtifactResetShutdownWhileDrainingPreventsLateReset(t *testing.T) {
	store := newLifecycleArtifactStore()
	lifetime := artifactOperationLifetime{store: store}
	_, release, err := lifetime.acquire()
	require.NoError(t, err)
	type resetDrainResult struct {
		store artifact.ArtifactStore
		err   error
	}
	drained := make(chan resetDrainResult, 1)
	go func() {
		owned, _, drainErr := lifetime.beginReset(t.Context())
		drained <- resetDrainResult{store: owned, err: drainErr}
	}()

	require.Eventually(t, func() bool {
		_, probeRelease, probeErr := lifetime.acquire()
		if probeErr != nil {
			return true
		}
		probeRelease()
		return false
	}, time.Second, time.Millisecond, "reset never entered its drain state")
	require.NoError(t, lifetime.closeWhenIdle())
	release()
	drainResult := <-drained
	require.ErrorContains(t, drainResult.err, "closing")
	assert.Nil(t, drainResult.store)
	assert.Equal(t, int32(1), store.closeCalls.Load())
	_, _, err = lifetime.acquire()
	require.Error(t, err)
}

func TestArtifactResetShutdownAfterAdmissionBeforeMutationDoesNotMoveVault(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, WriteTimeout: time.Second,
		AuthToken: "daemon-secret", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactRepository(repository))
	locked := true
	srv.sessionLifecycleMu.Lock()
	t.Cleanup(func() {
		if locked {
			srv.sessionLifecycleMu.Unlock()
		}
		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, repository.Close())
	})
	admitted := make(chan struct{})
	var admittedOnce sync.Once
	srv.beforeSessionLifecycleLock = func() {
		admittedOnce.Do(func() { close(admitted) })
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/reset", http.NoBody)
	request.Host = "127.0.0.1:0"
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer daemon-secret")
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-admitted:
	case <-time.After(time.Second):
		require.FailNow(t, "reset did not reach the admitted session-lifecycle boundary")
	}
	require.NoError(t, srv.closeArtifactResources())
	srv.sessionLifecycleMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "reset did not unwind after artifact shutdown")
	}

	assert.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
	assert.DirExists(t, filepath.Join(dataDir, "artifacts"))
	moved, err := filepath.Glob(filepath.Join(dataDir, "artifacts.reset-*"))
	require.NoError(t, err)
	assert.Empty(t, moved, "shutdown must prevent a reset admitted before session drain from moving the vault")
}

func TestArtifactResetShutdownDeadlineIsBoundedDuringPostMoveRepublish(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, WriteTimeout: time.Second,
		AuthToken: "daemon-secret", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactRepository(repository))
	republishStarted := make(chan struct{})
	releaseRepublish := make(chan struct{})
	srv.republishArtifactRepositoryReset = func(
		ctx context.Context,
		dataDir string,
		database *db.DB,
		_ string,
		_ *artifact.Repository,
		result artifact.RepositoryResetResult,
	) (artifact.RepositoryResetResult, error) {
		close(republishStarted)
		<-releaseRepublish
		return result, nil
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv.SetPort(listener.Addr().(*net.TCPAddr).Port)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, repository.Close())
	})

	requestDone := make(chan error, 1)
	go func() {
		request, requestErr := http.NewRequest(
			http.MethodPost,
			"http://"+listener.Addr().String()+"/api/v1/artifacts/reset",
			http.NoBody,
		)
		if requestErr != nil {
			requestDone <- requestErr
			return
		}
		request.Header.Set("Authorization", "Bearer daemon-secret")
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()
	select {
	case <-republishStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "reset did not reach post-move republish")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown(shutdownCtx) }()
	var shutdownErr error
	bounded := false
	select {
	case shutdownErr = <-shutdownDone:
		bounded = true
	case <-time.After(250 * time.Millisecond):
		assert.Fail(t, "shutdown exceeded its deadline while reset republish was blocked")
	}
	close(releaseRepublish)
	if !bounded {
		shutdownErr = <-shutdownDone
	}
	assert.ErrorIs(t, shutdownErr, context.DeadlineExceeded)
	require.NoError(t, <-requestDone)
	assert.ErrorIs(t, <-serveDone, http.ErrServerClosed)
	assert.DirExists(t, filepath.Join(dataDir, "artifacts"))
	moved, err := filepath.Glob(filepath.Join(dataDir, "artifacts.reset-*"))
	require.NoError(t, err)
	assert.Len(t, moved, 1, "post-admission reset must preserve its moved-aside diagnostic vault")
}

func TestArtifactResetRequestCancellationAfterMoveKeepsFreshRepositoryUsable(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, WriteTimeout: time.Second,
		AuthToken: "daemon-secret", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactRepository(repository))
	republishStarted := make(chan struct{})
	releaseRepublish := make(chan struct{})
	var freshRepository *artifact.Repository
	srv.republishArtifactRepositoryReset = func(
		ctx context.Context,
		dataDir string,
		database *db.DB,
		_ string,
		fresh *artifact.Repository,
		result artifact.RepositoryResetResult,
	) (artifact.RepositoryResetResult, error) {
		freshRepository = fresh
		close(republishStarted)
		<-releaseRepublish
		return result, ctx.Err()
	}
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, repository.Close())
		if freshRepository != nil {
			require.NoError(t, freshRepository.Close())
		}
	})

	ctx, cancel := context.WithCancel(t.Context())
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/reset", http.NoBody).WithContext(ctx)
	request.Host = "127.0.0.1:0"
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Authorization", "Bearer daemon-secret")
	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(response, request)
		close(done)
	}()
	select {
	case <-republishStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "reset did not reach post-move republish")
	}
	cancel()
	close(releaseRepublish)
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "canceled reset request did not return")
	}
	assert.Equal(t, http.StatusInternalServerError, response.Code, response.Body.String())

	store, release, err := srv.acquireArtifactStore()
	require.NoError(t, err, "the fresh post-move repository must remain owned by the server")
	defer release()
	assert.Same(t, freshRepository, srv.artifactRepository)
	assert.False(t, srv.artifactRepository.Closed())
	_, err = store.Origins(t.Context())
	require.NoError(t, err)
	_, pending, err := database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.True(t, pending, "canceled republish must retain durable recovery authority")
	require.NoError(t, srv.publishLocalArtifacts(t.Context(), store),
		"the retained fresh repository must support a later full publication")
	assert.True(t, srv.artifactBaselineDone)
	_, pending, err = database.ArtifactResetRepublishPending(t.Context())
	require.NoError(t, err)
	assert.False(t, pending, "ordinary publication must finish reset recovery")
}

func TestArtifactResetDrainBlocksCursorReleaseRegistryAccess(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := newLifecycleArtifactStore()
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })
	owned, _, err := srv.artifactOps.beginReset(t.Context())
	require.NoError(t, err)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/v1/artifacts/cursors/stale", nil)
	request.Host = "127.0.0.1:0"
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Origin", "http://127.0.0.1:0")
	srv.Handler().ServeHTTP(response, request)
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.NoError(t, srv.artifactOps.finishReset(owned))
}

func TestArtifactLifecycleRoutesUseInjectedStoreAndCloseItOnce(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := newLifecycleArtifactStore()
	body := []byte("injected artifact bytes")
	hash := sha256.Sum256(body)
	name := hex.EncodeToString(hash[:])
	ref, err := artifact.NewRef(artifactLifecycleOrigin, artifact.KindRaw, name)
	require.NoError(t, err)
	store.entries[ref] = body

	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: t.TempDir(), WriteTimeout: time.Second,
	}, database, nil, WithArtifactStore(store))

	for _, path := range []string{
		"/api/v1/artifacts/origins",
		"/api/v1/artifacts/" + artifactLifecycleOrigin + "/index",
		"/api/v1/artifacts/" + artifactLifecycleOrigin + "/raw/" + name,
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Host = "127.0.0.1:0"
		srv.Handler().ServeHTTP(response, request)
		assert.Equal(t, http.StatusOK, response.Code, path)
	}

	require.NoError(t, srv.Shutdown(t.Context()))
	require.NoError(t, srv.Shutdown(t.Context()))
	assert.Equal(t, int32(1), store.closeCalls.Load())
}

func TestArtifactLifecycleReadOnlyAndRemoteServersOmitMutationRoutes(t *testing.T) {
	dir := t.TempDir()
	writable, err := db.Open(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, writable.Close())
	readonly, err := db.OpenReadOnly(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })

	for name, database := range map[string]db.Store{
		"read-only SQLite": readonly,
		"remote":           lifecycleRemoteStore{},
	} {
		t.Run(name, func(t *testing.T) {
			store := newLifecycleArtifactStore()
			srv := New(config.Config{Host: "127.0.0.1", DataDir: dir}, database, nil,
				WithArtifactStore(store))
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

			for _, path := range []string{
				"/api/v1/artifacts/finalize",
				"/api/v1/artifacts/exchange",
				"/api/v1/artifacts/maintenance",
				"/api/v1/artifacts/reset",
				"/api/v1/artifacts/" + artifactLifecycleOrigin + "/raw/" + strings.Repeat("0", 64),
			} {
				response := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(nil))
				request.Host = "127.0.0.1:0"
				request.Header.Set("Origin", "http://127.0.0.1:0")
				srv.Handler().ServeHTTP(response, request)
				assert.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, response.Code, path)
			}
		})
	}
}

func TestArtifactResetRequiresAuthenticatedDirectLoopback(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dataDir := t.TempDir()
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, WriteTimeout: time.Second,
		AuthToken: "daemon-secret", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactRepository(repository))
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, repository.Close())
	})
	request := func(remoteAddr string, authenticated, forwarded bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/reset", http.NoBody)
		req.Host = "127.0.0.1:0"
		req.RemoteAddr = remoteAddr
		if authenticated {
			req.Header.Set("Authorization", "Bearer daemon-secret")
		}
		if forwarded {
			req.Header.Set("X-Forwarded-For", "203.0.113.10")
		}
		return req
	}

	unauthorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorized, request("127.0.0.1:1234", false, false))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	proxied := httptest.NewRecorder()
	srv.Handler().ServeHTTP(proxied, request("127.0.0.1:1234", true, true))
	assert.Equal(t, http.StatusForbidden, proxied.Code)
	remote := httptest.NewRecorder()
	srv.Handler().ServeHTTP(remote, request("203.0.113.10:1234", true, false))
	assert.Equal(t, http.StatusForbidden, remote.Code)
	local := httptest.NewRecorder()
	srv.Handler().ServeHTTP(local, request("127.0.0.1:1234", true, false))
	assert.Equal(t, http.StatusOK, local.Code, local.Body.String())
}

func TestArtifactLifecycleExchangeRequiresAuthenticatedDirectLoopback(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dataDir := t.TempDir()
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, WriteTimeout: time.Second,
		AuthToken: "daemon-secret", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactRepository(repository))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	target := t.TempDir()
	body, err := json.Marshal(artifactExchangeRequest{Target: target})
	require.NoError(t, err)
	request := func(remoteAddr string, authenticated, forwarded bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/exchange", bytes.NewReader(body))
		req.Host = "127.0.0.1:0"
		req.RemoteAddr = remoteAddr
		if authenticated {
			req.Header.Set("Authorization", "Bearer daemon-secret")
		}
		if forwarded {
			req.Header.Set("X-Forwarded-For", "203.0.113.10")
		}
		return req
	}

	unauthorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorized, request("127.0.0.1:1234", false, false))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	proxied := httptest.NewRecorder()
	srv.Handler().ServeHTTP(proxied, request("127.0.0.1:1234", true, true))
	assert.Equal(t, http.StatusForbidden, proxied.Code)

	remote := httptest.NewRecorder()
	srv.Handler().ServeHTTP(remote, request("203.0.113.10:1234", true, false))
	assert.Equal(t, http.StatusForbidden, remote.Code)

	local := httptest.NewRecorder()
	srv.Handler().ServeHTTP(local, request("127.0.0.1:1234", true, false))
	require.Equal(t, http.StatusOK, local.Code, local.Body.String())
	var response artifactExchangeResponse
	require.NoError(t, json.Unmarshal(local.Body.Bytes(), &response))
	assert.Equal(t, artifactLifecycleOrigin, response.Origin)
}

func TestArtifactLifecycleMaintenanceRequiresAuthenticatedDirectLoopback(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dataDir := t.TempDir()
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, WriteTimeout: time.Second,
		AuthToken: "daemon-secret", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactRepository(repository))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	request := func(remoteAddr string, authenticated, forwarded bool) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/maintenance",
			strings.NewReader(`{"dry_run":true}`))
		req.Host = "127.0.0.1:0"
		req.RemoteAddr = remoteAddr
		if authenticated {
			req.Header.Set("Authorization", "Bearer daemon-secret")
		}
		if forwarded {
			req.Header.Set("X-Forwarded-For", "203.0.113.10")
		}
		return req
	}

	unauthorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorized, request("127.0.0.1:1234", false, false))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)

	proxied := httptest.NewRecorder()
	srv.Handler().ServeHTTP(proxied, request("127.0.0.1:1234", true, true))
	assert.Equal(t, http.StatusForbidden, proxied.Code)

	remote := httptest.NewRecorder()
	srv.Handler().ServeHTTP(remote, request("203.0.113.10:1234", true, false))
	assert.Equal(t, http.StatusForbidden, remote.Code)

	local := httptest.NewRecorder()
	srv.Handler().ServeHTTP(local, request("127.0.0.1:1234", true, false))
	assert.Equal(t, http.StatusOK, local.Code, local.Body.String())
}

func TestArtifactLifecycleShutdownTimeoutDefersStoreCloseUntilRouteCompletes(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := newLifecycleArtifactStore()
	store.openStarted = make(chan struct{}, 1)
	store.openRelease = make(chan struct{})
	body := []byte("held artifact")
	entry := lifecycleEntry(artifact.Ref{}, body)
	ref, err := artifact.NewRef(artifactLifecycleOrigin, artifact.KindRaw, entry.Identity.SHA256)
	require.NoError(t, err)
	store.entries[ref] = body

	srv := New(config.Config{Host: "127.0.0.1", WriteTimeout: time.Second}, database, nil,
		WithArtifactStore(store))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv.SetPort(listener.Addr().(*net.TCPAddr).Port)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()

	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String() +
			"/api/v1/artifacts/" + artifactLifecycleOrigin + "/raw/" + ref.Name)
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-store.openStarted:
	case <-time.After(time.Second):
		t.Fatal("artifact read did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, srv.Shutdown(shutdownCtx), context.DeadlineExceeded)
	assert.False(t, store.closed.Load(), "timed-out shutdown must not close a store in active use")

	close(store.openRelease)
	require.NoError(t, <-requestDone)
	assert.Eventually(t, store.closed.Load, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), store.closeCalls.Load())
	assert.ErrorIs(t, <-serveDone, http.ErrServerClosed)
}

func TestArtifactLifecycleVerifiedReadFailsClosedAfterProducingBytes(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := newLifecycleArtifactStore()
	body := []byte("content that must not be served")
	entry := lifecycleEntry(artifact.Ref{}, body)
	ref, err := artifact.NewRef(artifactLifecycleOrigin, artifact.KindRaw, entry.Identity.SHA256)
	require.NoError(t, err)
	store.entries[ref] = body
	store.corrupt[ref] = true
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/"+artifactLifecycleOrigin+"/raw/"+ref.Name, nil)
	request.Host = "127.0.0.1:0"
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.NotContains(t, response.Body.String(), string(body))
}

func TestArtifactPostRepairsExactQueuedDocbankContent(t *testing.T) {
	database, repository, ref, _, body := seedQueuedCorruptArtifact(t)

	srv := New(config.Config{Host: "127.0.0.1"}, database, nil,
		WithArtifactRepository(repository))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })
	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/artifacts/"+ref.Origin+"/raw/"+ref.Name, bytes.NewReader(body))
	request.Host = "127.0.0.1:0"
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Origin", "http://127.0.0.1:0")
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, "body: %s", response.Body.String())

	pending, err := database.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	_, reader, err := repository.Content().Open(t.Context(), ref)
	require.NoError(t, err)
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	assert.Equal(t, body, got)
}

func TestArtifactPostPreservesExactRepairClaimWhenRepairFails(t *testing.T) {
	database, repository, ref, identity, body := seedQueuedCorruptArtifact(t)
	store := &repairGateArtifactStore{
		ArtifactStore: repository.Content(), err: errors.New("forced repair failure"),
	}
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	_, err := srv.humaPostArtifact(t.Context(), &artifactPostInput{
		Origin: ref.Origin, Kind: string(ref.Kind), Name: ref.Name,
		ImportMode: "deferred", Body: bytes.NewReader(body),
	})
	require.Error(t, err)

	pending, pendingErr := database.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, pendingErr)
	require.Len(t, pending, 1)
	assert.Equal(t, identity.SHA256, pending[0].SHA256)
	assert.Equal(t, identity.Size, pending[0].Size)
	require.NoError(t, repository.Close())
}

func TestArtifactPostPreservesExactRepairClaimWhenCanceled(t *testing.T) {
	database, repository, ref, identity, body := seedQueuedCorruptArtifact(t)
	store := &repairGateArtifactStore{
		ArtifactStore: repository.Content(), started: make(chan struct{}, 1),
	}
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := srv.humaPostArtifact(ctx, &artifactPostInput{
			Origin: ref.Origin, Kind: string(ref.Kind), Name: ref.Name,
			ImportMode: "deferred", Body: bytes.NewReader(body),
		})
		done <- err
	}()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("repair did not start")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)

	pending, err := database.PendingArtifactRepairs(t.Context(), 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, identity.SHA256, pending[0].SHA256)
	assert.Equal(t, identity.Size, pending[0].Size)
	require.NoError(t, repository.Close())
}

func TestArtifactExchangeRejectsSecretURLWithoutResponseOrLogDisclosure(t *testing.T) {
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	srv := New(config.Config{
		Host: "127.0.0.1", DataDir: dataDir, AuthToken: "server-auth",
	}, database, nil,
		WithArtifactRepository(repository))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	const secret = "exchange-secret-value"
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/exchange",
		strings.NewReader(`{"target":"https://user:`+secret+
			`@example.invalid/archive?token=`+secret+`#`+secret+`","token":"peer-`+secret+`"}`))
	request.Host = "127.0.0.1:0"
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Origin", "http://127.0.0.1:0")
	request.Header.Set("Authorization", "Bearer server-auth")
	var logs bytes.Buffer
	previousLogOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(previousLogOutput)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)

	assert.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	assert.NotContains(t, response.Body.String(), secret)
	assert.NotContains(t, logs.String(), secret)
}

func seedQueuedCorruptArtifact(
	t *testing.T,
) (*db.DB, *artifact.Repository, artifact.Ref, artifact.Identity, []byte) {
	t.Helper()
	dataDir := t.TempDir()
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	repository, err := artifact.OpenRepository(t.Context(), dataDir)
	require.NoError(t, err)
	body := []byte("trusted peer artifact content")
	identity := lifecycleEntry(artifact.Ref{}, body).Identity
	ref, err := artifact.NewRef(artifactLifecycleOrigin, artifact.KindRaw, identity.SHA256)
	require.NoError(t, err)
	_, err = repository.Content().Create(t.Context(), ref, identity,
		"application/octet-stream", bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, database.EnqueueArtifactRepair(t.Context(), db.ArtifactRepair{
		Origin: ref.Origin, Kind: string(ref.Kind), Name: ref.Name,
		SHA256: identity.SHA256, Size: identity.Size,
	}))
	blobPath := filepath.Join(dataDir, "artifacts", "blobs", identity.SHA256[:2], identity.SHA256)
	require.NoError(t, os.WriteFile(blobPath, []byte("corrupt"), 0o600))
	return database, repository, ref, identity, body
}

func TestArtifactLifecycleMetadataCreateHoldsStoreThroughTimedOutShutdown(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	require.NoError(t, artifact.AdoptOrigin(database, artifactLifecycleOrigin))
	store := newLifecycleArtifactStore()
	store.createStarted = make(chan struct{}, 1)
	store.createRelease = make(chan struct{})
	srv := New(config.Config{
		Host: "127.0.0.1", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactStore(store))

	appendDone := make(chan error, 1)
	go func() {
		appendDone <- srv.appendMetadataEvent(t.Context(), artifact.MetadataEventInput{
			SessionID: "session-1", Op: artifact.MetadataOpStar,
		})
	}()
	select {
	case <-store.createStarted:
	case <-time.After(time.Second):
		t.Fatal("metadata create did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, srv.Shutdown(shutdownCtx))
	assert.False(t, store.closed.Load(),
		"timed-out shutdown must not close a store during metadata create")

	close(store.createRelease)
	require.NoError(t, <-appendDone)
	assert.Eventually(t, store.closed.Load, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), store.closeCalls.Load())
}

func TestArtifactLifecycleBulkMetadataCreateHoldsStoreThroughTimedOutShutdown(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dbtest.SeedSession(t, database, "session-1", "bulk metadata lifecycle")
	dbtest.SeedSession(t, database, "session-2", "bulk metadata lifecycle")
	require.NoError(t, artifact.AdoptOrigin(database, artifactLifecycleOrigin))
	store := newLifecycleArtifactStore()
	store.createStarted = make(chan struct{}, 1)
	store.createRelease = make(chan struct{})
	srv := New(config.Config{
		Host: "127.0.0.1", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactStore(store))

	requestDone := make(chan error, 1)
	go func() {
		input := &bulkStarInput{}
		input.Body.SessionIDs = []string{"session-1", "session-2"}
		_, err := srv.humaBulkStar(t.Context(), input)
		requestDone <- err
	}()
	select {
	case <-store.createStarted:
	case <-time.After(time.Second):
		t.Fatal("bulk metadata create did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, srv.Shutdown(shutdownCtx))
	assert.False(t, store.closed.Load(),
		"timed-out shutdown must not close a store during bulk metadata create")

	close(store.createRelease)
	require.NoError(t, <-requestDone)
	assert.Eventually(t, store.closed.Load, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), store.closeCalls.Load())
}

func TestArtifactLifecycleMetadataRepairAndAppendShareShutdownLease(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dbtest.SeedSession(t, database, "session-1", "metadata repair lifecycle")
	require.NoError(t, artifact.AdoptOrigin(database, artifactLifecycleOrigin))
	store := newLifecycleArtifactStore()
	recorder := artifact.NewMetadataRecorder(database, artifact.MetadataRecorderOptions{
		Origin: artifactLifecycleOrigin, Store: store,
	})
	_, err := recorder.Append(t.Context(), artifact.MetadataEventInput{
		SessionID: "session-1", Op: artifact.MetadataOpStar,
	})
	require.NoError(t, err)
	_, err = recorder.Append(t.Context(), artifact.MetadataEventInput{
		SessionID: "session-1", Op: artifact.MetadataOpUnstar,
	})
	require.NoError(t, err)
	store.openStarted = make(chan struct{}, 1)
	store.openRelease = make(chan struct{})
	srv := New(config.Config{
		Host: "127.0.0.1", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactStore(store))

	ensureDone := make(chan error, 1)
	go func() {
		ensureDone <- srv.ensureLocalMetadataEvent(t.Context(), artifact.MetadataEventInput{
			SessionID: "session-1", Op: artifact.MetadataOpStar,
		}, "starred", artifact.MetadataOpStar)
	}()
	select {
	case <-store.openStarted:
	case <-time.After(time.Second):
		t.Fatal("metadata repair did not open its provenance event")
	}

	require.NoError(t, srv.Shutdown(t.Context()))
	assert.False(t, store.closed.Load(),
		"shutdown must retain the store through the repair and append transaction")

	close(store.openRelease)
	require.NoError(t, <-ensureDone)
	assert.Eventually(t, store.closed.Load, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), store.closeCalls.Load())
	assert.Len(t, store.entries, 3,
		"a new star event must publish after repair observes the newer unstar state")
}

func TestArtifactLifecycleSpontaneousServeExitClosesOwnedStoreOnce(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := newLifecycleArtifactStore()
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil,
		WithArtifactStore(store))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()

	require.NoError(t, listener.Close())
	require.Error(t, <-serveDone)
	assert.Eventually(t, store.closed.Load, time.Second, time.Millisecond)
	assert.Equal(t, int32(1), store.closeCalls.Load())
	require.NoError(t, srv.Shutdown(t.Context()))
	assert.Equal(t, int32(1), store.closeCalls.Load())
}

func TestArtifactLifecycleServePreservesServerClosedIdentity(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := newLifecycleArtifactStore()
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil,
		WithArtifactStore(store))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	require.Eventually(t, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		return srv.httpSrv != nil
	}, time.Second, time.Millisecond)

	require.NoError(t, srv.Shutdown(t.Context()))
	serveErr := <-serveDone
	assert.True(t, serveErr == http.ErrServerClosed,
		"clean artifact cleanup must preserve the exact HTTP sentinel")
	assert.Equal(t, int32(1), store.closeCalls.Load())
}

func TestArtifactLifecycleServeJoinsStoreCloseFailure(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	closeErr := errors.New("closing artifact store")
	store := newLifecycleArtifactStore()
	store.closeErr = closeErr
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil,
		WithArtifactStore(store))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	require.Eventually(t, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		return srv.httpSrv != nil
	}, time.Second, time.Millisecond)

	assert.ErrorIs(t, srv.Shutdown(t.Context()), closeErr)
	serveErr := <-serveDone
	assert.ErrorIs(t, serveErr, http.ErrServerClosed)
	assert.ErrorIs(t, serveErr, closeErr)
	assert.Equal(t, int32(1), store.closeCalls.Load())
}

func TestArtifactLifecyclePeerStatusReportsCorruptExactHeadWithoutFallback(t *testing.T) {
	dir := t.TempDir()
	writable, err := db.Open(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	store := newLifecycleArtifactStore()
	checkpoint := func(sequence int, body []byte, corrupt bool) {
		t.Helper()
		ref, err := artifact.NewRef(artifactLifecycleOrigin, artifact.KindCheckpoints,
			fmt.Sprintf("cp-%010d.json", sequence))
		require.NoError(t, err)
		store.entries[ref] = body
		store.corrupt[ref] = corrupt
	}
	checkpoint(1, []byte(`{"origin":"lifecycle-a1b2c3","seq":1,"sessions":{},"v":1}`+"\n"), false)
	corruptHead := []byte(`{"origin":"lifecycle-a1b2c3","seq":2,"sessions":{},"v":1}` + "\n")
	checkpoint(2, corruptHead, true)
	headIdentity := lifecycleEntry(artifact.Ref{}, corruptHead).Identity
	require.NoError(t, writable.RecordArtifactCheckpointHead(t.Context(), db.ArtifactCheckpointHead{
		Origin: artifactLifecycleOrigin, Sequence: 2,
		SessionMapSHA256: strings.Repeat("0", 64),
		CheckpointSHA256: headIdentity.SHA256, CheckpointSize: headIdentity.Size,
	}, nil))
	require.NoError(t, writable.Close())
	readonly, err := db.OpenReadOnly(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	srv := New(config.Config{
		Host: "127.0.0.1", ArtifactOriginID: artifactLifecycleOrigin,
	}, readonly, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/peers", nil)
	request.Host = "127.0.0.1:0"
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var peers artifactPeersResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &peers))
	require.Len(t, peers.Peers, 1)
	assert.Equal(t, 2, peers.Peers[0].CheckpointSeq)
	assert.Equal(t, "error", peers.Peers[0].Status)
}

type lifecycleRemoteStore struct{ db.Store }

func (lifecycleRemoteStore) ReadOnly() bool { return true }

type pagedLifecycleStore struct {
	*lifecycleArtifactStore
	originCount     int
	entryCount      int
	originCalls     atomic.Int32
	listCalls       atomic.Int32
	statCalls       atomic.Int32
	openCalls       atomic.Int32
	releaseCalls    atomic.Int32
	entryCloseCalls atomic.Int32
	originErr       error
}

func (s *pagedLifecycleStore) Stat(
	ctx context.Context, ref artifact.Ref,
) (artifact.Entry, error) {
	s.statCalls.Add(1)
	return s.lifecycleArtifactStore.Stat(ctx, ref)
}

func (s *pagedLifecycleStore) Open(
	ctx context.Context, ref artifact.Ref,
) (artifact.Entry, artifact.VerifiedReader, error) {
	s.openCalls.Add(1)
	return s.lifecycleArtifactStore.Open(ctx, ref)
}

func (s *pagedLifecycleStore) Origins(context.Context) (artifact.OriginIterator, error) {
	return &pagedOriginIterator{store: s}, nil
}

func (s *pagedLifecycleStore) Entries(
	_ context.Context, origin string, kind artifact.Kind,
) (artifact.EntryIterator, error) {
	return &pagedEntryIterator{store: s, origin: origin, kind: kind}, nil
}

type pagedOriginIterator struct {
	store  *pagedLifecycleStore
	offset int
	closed atomic.Bool
}

func (i *pagedOriginIterator) Next(ctx context.Context, limit int) ([]string, error) {
	if i.closed.Load() {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.store.originErr != nil {
		return nil, i.store.originErr
	}
	i.store.originCalls.Add(1)
	end := min(i.offset+limit, i.store.originCount)
	origins := make([]string, 0, end-i.offset)
	for index := i.offset; index < end; index++ {
		origins = append(origins, fmt.Sprintf("origin-%05d-a1b2c3", index))
	}
	i.offset = end
	if end == i.store.originCount {
		return origins, io.EOF
	}
	return origins, nil
}

func (i *pagedOriginIterator) Close() error {
	if i.closed.CompareAndSwap(false, true) {
		i.store.releaseCalls.Add(1)
	}
	return nil
}

type pagedEntryIterator struct {
	store  *pagedLifecycleStore
	origin string
	kind   artifact.Kind
	offset int
	closed atomic.Bool
}

func (i *pagedEntryIterator) Next(
	ctx context.Context, limit int,
) ([]artifact.Entry, error) {
	if i.closed.Load() {
		return nil, os.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if i.kind != artifact.KindSegments {
		return []artifact.Entry{}, io.EOF
	}
	i.store.listCalls.Add(1)
	end := min(i.offset+limit, i.store.entryCount)
	items := make([]artifact.Entry, 0, end-i.offset)
	for index := i.offset; index < end; index++ {
		name := fmt.Sprintf("%064x.ndjson", index+1)
		ref, err := artifact.NewRef(i.origin, artifact.KindSegments, name)
		if err != nil {
			return nil, err
		}
		items = append(items, artifact.Entry{Ref: ref})
	}
	i.offset = end
	if end == i.store.entryCount {
		return items, io.EOF
	}
	return items, nil
}

func (i *pagedEntryIterator) Close() error {
	if i.closed.CompareAndSwap(false, true) {
		i.store.entryCloseCalls.Add(1)
	}
	return nil
}

func (s *pagedLifecycleStore) ListOrigins(
	ctx context.Context, cursor artifact.Cursor, limit int,
) ([]string, artifact.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if s.originErr != nil {
		return nil, "", s.originErr
	}
	s.originCalls.Add(1)
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(string(cursor))
		if err != nil {
			return nil, "", artifact.ErrArtifactInvalid
		}
	}
	end := min(start+limit, s.originCount)
	origins := make([]string, 0, end-start)
	for index := start; index < end; index++ {
		origins = append(origins, fmt.Sprintf("origin-%05d-a1b2c3", index))
	}
	if end == s.originCount {
		return origins, "", nil
	}
	return origins, artifact.Cursor(strconv.Itoa(end)), nil
}

func TestArtifactPeersUsesBoundedScopedOriginCursor(t *testing.T) {
	dir := t.TempDir()
	writable, err := db.Open(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	require.NoError(t, writable.Close())
	readonly, err := db.OpenReadOnly(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 10_000,
	}
	srv := New(config.Config{Host: "127.0.0.1"}, readonly, nil,
		WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/peers?limit=2", nil)
	request.Host = "127.0.0.1:0"
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var first artifactPeersResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &first))
	require.Len(t, first.Peers, 2)
	assert.NotEmpty(t, first.NextCursor)
	assert.Equal(t, int32(1), store.originCalls.Load(),
		"peer work must be bounded by the requested page")
	assert.Zero(t, store.listCalls.Load(),
		"peer status must not scan checkpoint history")

	wrongScope := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/origins?limit=2&cursor="+first.NextCursor, nil)
	wrongScope.Host = "127.0.0.1:0"
	wrongScopeResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(wrongScopeResponse, wrongScope)
	assert.Equal(t, http.StatusBadRequest, wrongScopeResponse.Code,
		"peer cursors must not be accepted by origin enumeration")

	request = httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/peers?limit=2&cursor="+first.NextCursor, nil)
	request.Host = "127.0.0.1:0"
	response = httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var second artifactPeersResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &second))
	require.Len(t, second.Peers, 2)
	assert.NotEqual(t, first.Peers[0].Origin, second.Peers[0].Origin)
	assert.Equal(t, int32(2), store.originCalls.Load())
}

func TestArtifactPeersPointOpensOnlyTheProvenanceSelectedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	origin := "origin-00000-a1b2c3"
	body := []byte(`{"origin":"origin-00000-a1b2c3","seq":73,"sessions":{},"v":1}` + "\n")
	identity := lifecycleEntry(artifact.Ref{}, body).Identity
	require.NoError(t, database.RecordArtifactPeerCheckpointHead(t.Context(),
		db.ArtifactPeerCheckpointHead{
			Origin: origin, Sequence: 73,
			CheckpointSHA256: identity.SHA256, CheckpointSize: identity.Size,
		}))
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(), originCount: 10_000,
		entryCount: 10_000,
	}
	ref, err := artifact.NewRef(origin, artifact.KindCheckpoints, "cp-0000000073.json")
	require.NoError(t, err)
	store.entries[ref] = body
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil,
		WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	request := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/peers?limit=1", nil)
	request.Host = "127.0.0.1:0"
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var peers artifactPeersResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &peers))
	require.Len(t, peers.Peers, 1)
	assert.Equal(t, 73, peers.Peers[0].CheckpointSeq)
	assert.Equal(t, "in_sync", peers.Peers[0].Status)
	assert.Zero(t, store.listCalls.Load(),
		"checkpoint status must not enumerate the 10k unrelated entries")
	assert.Equal(t, int32(1), store.statCalls.Load())
	assert.Equal(t, int32(1), store.openCalls.Load())
}

func (s *pagedLifecycleStore) List(
	ctx context.Context,
	origin string,
	kind artifact.Kind,
	cursor artifact.Cursor,
	limit int,
) (artifact.Page, error) {
	if kind != artifact.KindSegments {
		return artifact.Page{}, nil
	}
	if err := ctx.Err(); err != nil {
		return artifact.Page{}, err
	}
	s.listCalls.Add(1)
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(string(cursor))
		if err != nil {
			return artifact.Page{}, artifact.ErrArtifactInvalid
		}
	}
	end := min(start+limit, s.entryCount)
	items := make([]artifact.Entry, 0, end-start)
	for index := start; index < end; index++ {
		name := fmt.Sprintf("%064x.ndjson", index+1)
		ref, err := artifact.NewRef(origin, artifact.KindSegments, name)
		if err != nil {
			return artifact.Page{}, err
		}
		items = append(items, artifact.Entry{Ref: ref})
	}
	page := artifact.Page{Items: items}
	if end < s.entryCount {
		page.Next = artifact.Cursor(strconv.Itoa(end))
	}
	return page, nil
}

func TestArtifactLifecycleEnumerationOnlyConsumesRequestedStorePage(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(),
		originCount:            10_000,
		entryCount:             10_000,
	}
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	origins := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/origins?limit=10", nil)
	origins.Host = "127.0.0.1:0"
	originsResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(originsResponse, origins)
	require.Equal(t, http.StatusOK, originsResponse.Code)
	assert.Equal(t, int32(1), store.originCalls.Load(),
		"the first HTTP page must not enumerate the remaining archive")

	index := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/origin-00000-a1b2c3/index?limit=10", nil)
	index.Host = "127.0.0.1:0"
	indexResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(indexResponse, index)
	require.Equal(t, http.StatusOK, indexResponse.Code)
	assert.Equal(t, int32(1), store.listCalls.Load(),
		"the first HTTP index page must not enumerate the remaining collection")
}

func TestArtifactLifecycleConcurrentEnumerationCoalescesDirtyPublicationAndBaseline(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dbtest.SeedSession(t, database, "local-1", "project")
	displayName := "curated before publication"
	require.NoError(t, database.RenameSession("local-1", &displayName))
	store := newLifecycleArtifactStore()
	srv := New(config.Config{
		Host: "127.0.0.1", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	const callers = 8
	var group sync.WaitGroup
	group.Add(callers)
	statuses := make(chan int, callers)
	for range callers {
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/origins", nil)
			request.Host = "127.0.0.1:0"
			response := httptest.NewRecorder()
			srv.Handler().ServeHTTP(response, request)
			statuses <- response.Code
		}()
	}
	group.Wait()
	close(statuses)
	for status := range statuses {
		assert.Equal(t, http.StatusOK, status)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	kindCounts := make(map[artifact.Kind]int)
	for ref := range store.entries {
		if ref.Origin == artifactLifecycleOrigin {
			kindCounts[ref.Kind]++
		}
	}
	assert.Equal(t, 1, kindCounts[artifact.KindCheckpoints],
		"concurrent enumeration must converge on one unchanged checkpoint")
	assert.Equal(t, 1, kindCounts[artifact.KindMeta],
		"metadata baseline must run once for concurrent enumeration")
	assert.Equal(t, 1, kindCounts[artifact.KindManifests])
	assert.Equal(t, 1, kindCounts[artifact.KindSegments])
}

func TestArtifactLifecycleIndividualReadDoesNotFlushDirtyPublication(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	dbtest.SeedSession(t, database, "local-1", "project")
	store := newLifecycleArtifactStore()
	body := []byte("already stored peer content")
	entry := lifecycleEntry(artifact.Ref{}, body)
	ref, err := artifact.NewRef("peer-a1b2c3", artifact.KindRaw, entry.Identity.SHA256)
	require.NoError(t, err)
	store.entries[ref] = body
	srv := New(config.Config{
		Host: "127.0.0.1", ArtifactOriginID: artifactLifecycleOrigin,
	}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/peer-a1b2c3/raw/"+ref.Name, nil)
	request.Host = "127.0.0.1:0"
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, body, response.Body.Bytes())

	store.mu.Lock()
	defer store.mu.Unlock()
	assert.Len(t, store.entries, 1,
		"an individual artifact read must not publish the dirty local queue")
}

func TestArtifactLifecycleCursorReleaseClosesUnderlyingStoreCursor(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(),
		originCount:            100,
	}
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	firstRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/origins?limit=10", nil)
	firstRequest.Host = "127.0.0.1:0"
	firstResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(firstResponse, firstRequest)
	require.Equal(t, http.StatusOK, firstResponse.Code)
	var first artifactOriginsResponse
	require.NoError(t, json.Unmarshal(firstResponse.Body.Bytes(), &first))
	require.NotEmpty(t, first.NextCursor)

	releaseRequest := httptest.NewRequest(http.MethodDelete,
		"/api/v1/artifacts/cursors/"+first.NextCursor, nil)
	releaseRequest.Host = "127.0.0.1:0"
	releaseRequest.Header.Set("Origin", "http://127.0.0.1:0")
	releaseResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(releaseResponse, releaseRequest)
	require.Equal(t, http.StatusNoContent, releaseResponse.Code)
	assert.Equal(t, int32(1), store.releaseCalls.Load())

	staleRequest := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/origins?limit=10&cursor="+first.NextCursor, nil)
	staleRequest.Host = "127.0.0.1:0"
	staleResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(staleResponse, staleRequest)
	assert.Equal(t, http.StatusBadRequest, staleResponse.Code)
}

func TestArtifactLifecycleCanceledContinuationClosesUnderlyingStoreCursor(t *testing.T) {
	database := dbtest.OpenTestDBAt(t, filepath.Join(t.TempDir(), "sessions.db"))
	store := &pagedLifecycleStore{
		lifecycleArtifactStore: newLifecycleArtifactStore(),
		originCount:            100,
	}
	srv := New(config.Config{Host: "127.0.0.1"}, database, nil, WithArtifactStore(store))
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(t.Context())) })

	firstRequest := httptest.NewRequest(http.MethodGet, "/api/v1/artifacts/origins?limit=10", nil)
	firstRequest.Host = "127.0.0.1:0"
	firstResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(firstResponse, firstRequest)
	require.Equal(t, http.StatusOK, firstResponse.Code)
	var first artifactOriginsResponse
	require.NoError(t, json.Unmarshal(firstResponse.Body.Bytes(), &first))
	require.NotEmpty(t, first.NextCursor)

	store.originErr = context.Canceled
	continuation := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/origins?limit=10&cursor="+first.NextCursor, nil)
	continuation.Host = "127.0.0.1:0"
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, continuation)
	assert.Equal(t, int32(1), store.releaseCalls.Load())
}
