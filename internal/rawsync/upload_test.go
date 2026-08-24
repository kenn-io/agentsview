package rawsync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestUploadServiceStartsDurableDeviceBoundSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	object := objectRefForBytes(t, []byte("hello"))

	session, created, err := service.Start(
		t.Context(), identity, parser.AgentCodex, object,
	)

	require.NoError(t, err)
	assert.True(t, created)
	assert.Equal(t, "upl_AQEBAQEBAQEBAQEBAQEBAQ", session.ID)
	assert.Equal(t, identity, session.Identity)
	assert.Equal(t, parser.AgentCodex, session.Provider)
	assert.Equal(t, object, session.Object)
	assert.Zero(t, session.Offset)
	assert.Equal(t, now, session.CreatedAt)
	assert.Equal(t, now.Add(DefaultUploadSessionTTL), session.ExpiresAt)
	assert.False(t, session.Complete)
	assert.Equal(t, 1, store.createCalls)
}

func TestUploadServiceSkipsSessionWhenObjectAlreadyExists(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	custody.objectPresent = true
	service := newUploadServiceForTest(t, store, custody, now)
	object := objectRefForBytes(t, []byte("already present"))

	session, created, err := service.Start(
		t.Context(), AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"},
		parser.AgentCodex, object,
	)

	require.NoError(t, err)
	assert.False(t, created)
	assert.Empty(t, session.ID)
	assert.Equal(t, object.Length, session.Offset)
	assert.True(t, session.Complete)
	assert.Zero(t, store.createCalls)
}

func TestUploadServiceAppendsAndIndependentlyFinalizesExactObject(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	events := make([]string, 0)
	store := newMemoryUploadSessionStore()
	store.events = &events
	custody := newMemoryUploadCustody()
	custody.events = &events
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	body := []byte("complete object")
	object := objectRefForBytes(t, body)
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)

	partial, err := service.Append(
		t.Context(), identity, session.ID, 0, body[:5],
	)
	require.NoError(t, err)
	assert.Equal(t, int64(5), partial.Offset)
	assert.False(t, partial.Complete)
	assert.Zero(t, custody.finalizeCalls)

	completed, err := service.Append(
		t.Context(), identity, session.ID, 5, body[5:],
	)

	require.NoError(t, err)
	assert.True(t, completed.Complete)
	assert.Equal(t, object.Length, completed.Offset)
	assert.Equal(t, body, custody.finalizedBody)
	assert.Equal(t, 1, custody.finalizeCalls)
	assert.Equal(t, []string{
		"append", "append", "open", "open", "finalize", "complete",
	}, events)
}

func TestUploadServiceReturnsCurrentOffsetWithoutWritingOnConflict(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	object := objectRefForBytes(t, []byte("offset"))
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)

	_, err = service.Append(t.Context(), identity, session.ID, 3, []byte("bad"))

	var conflict *UploadOffsetConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Zero(t, conflict.CurrentOffset)
	assert.Empty(t, store.data)
	assert.Zero(t, custody.finalizeCalls)
}

func TestUploadServiceChecksumMismatchResetsSession(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	object := objectRefForBytes(t, []byte("expected"))
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)

	_, err = service.Append(
		t.Context(), identity, session.ID, 0, []byte("corrupt!"),
	)

	var mismatch *UploadChecksumMismatchError
	require.ErrorAs(t, err, &mismatch)
	assert.Zero(t, mismatch.CurrentOffset)
	assert.Zero(t, store.session.Offset)
	assert.Empty(t, store.data)
	assert.Equal(t, 1, store.resetCalls)
	assert.Zero(t, custody.finalizeCalls)
}

func TestUploadServiceDoesNotReportResetOffsetWhenResetFails(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	store.resetErr = errors.New("database unavailable")
	service := newUploadServiceForTest(t, store, newMemoryUploadCustody(), now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	object := objectRefForBytes(t, []byte("expected"))
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)

	_, err = service.Append(t.Context(), identity, session.ID, 0, []byte("corrupt!"))

	var mismatch *UploadChecksumMismatchError
	assert.NotErrorAs(t, err, &mismatch)
	assert.ErrorContains(t, err, "database unavailable")
	assert.Equal(t, object.Length, store.session.Offset)
}

func TestUploadServiceStopsHashingWhenRequestIsCanceled(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	service := newUploadServiceForTest(t, store, newMemoryUploadCustody(), now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	body := bytes.Repeat([]byte("cancel"), 1024)
	object := objectRefForBytes(t, body)
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	store.openReader = func(data []byte) io.ReadCloser {
		return io.NopCloser(&cancelAfterFirstRead{reader: bytes.NewReader(data), cancel: cancel})
	}

	_, err = service.Append(ctx, identity, session.ID, 0, body)

	assert.ErrorIs(t, err, context.Canceled)
	assert.False(t, store.session.Complete)
}

func TestUploadServiceSerializesDuplicateFinalization(t *testing.T) {
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	custody.finalizeStarted = make(chan struct{}, 2)
	custody.finalizeRelease = make(chan struct{})
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	body := []byte("duplicate final patch")
	object := objectRefForBytes(t, body)
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)
	store.session.Offset = object.Length
	store.data = append([]byte(nil), body...)

	results := make(chan error, 2)
	go func() {
		_, appendErr := service.Append(t.Context(), identity, session.ID, object.Length, nil)
		results <- appendErr
	}()
	select {
	case <-custody.finalizeStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "first finalization did not start")
	}
	go func() {
		_, appendErr := service.Append(t.Context(), identity, session.ID, object.Length, nil)
		results <- appendErr
	}()
	select {
	case <-custody.finalizeStarted:
		require.FailNow(t, "duplicate finalization reached custody concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	close(custody.finalizeRelease)

	require.NoError(t, <-results)
	require.NoError(t, <-results)
	assert.Equal(t, 1, custody.finalizeCalls)
	assert.True(t, store.session.Complete)
}

func TestUploadServiceAcceptsCompletionByAnotherProcess(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	body := []byte("completed elsewhere")
	object := objectRefForBytes(t, body)
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)
	store.session.Offset = object.Length
	store.data = append([]byte(nil), body...)
	store.completeOnOpen = true

	completed, err := service.Append(
		t.Context(), identity, session.ID, object.Length, nil,
	)

	require.NoError(t, err)
	assert.True(t, completed.Complete)
	assert.Equal(t, object.Length, completed.Offset)
	assert.Zero(t, custody.finalizeCalls)
}

func TestUploadServiceRetriesFinalizationWithoutAnotherChunk(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	store := newMemoryUploadSessionStore()
	custody := newMemoryUploadCustody()
	custody.finalizeErr = errors.New("custody unavailable")
	service := newUploadServiceForTest(t, store, custody, now)
	identity := AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"}
	body := []byte("retry")
	object := objectRefForBytes(t, body)
	session, _, err := service.Start(t.Context(), identity, parser.AgentCodex, object)
	require.NoError(t, err)

	_, err = service.Append(t.Context(), identity, session.ID, 0, body)
	require.ErrorContains(t, err, "custody unavailable")
	assert.Equal(t, object.Length, store.session.Offset)
	assert.False(t, store.session.Complete)

	custody.finalizeErr = nil
	completed, err := service.Append(
		t.Context(), identity, session.ID, object.Length, nil,
	)

	require.NoError(t, err)
	assert.True(t, completed.Complete)
	assert.Equal(t, 2, custody.finalizeCalls)
}

func TestUploadServiceRejectsOversizedChunkBeforeStore(t *testing.T) {
	t.Parallel()

	service := newUploadServiceForTest(
		t, newMemoryUploadSessionStore(), newMemoryUploadCustody(), time.Now().UTC(),
	)

	_, err := service.Append(
		t.Context(),
		AuthIdentity{TenantID: "tenant-a", DeviceID: "dev-a"},
		"upl_AQEBAQEBAQEBAQEBAQEBAQ",
		0,
		make([]byte, DefaultUploadChunkBytes+1),
	)

	assert.ErrorIs(t, err, ErrInvalid)
}

func newUploadServiceForTest(
	t *testing.T,
	store UploadSessionStore,
	custody UploadCustody,
	now time.Time,
) *UploadService {
	t.Helper()
	service, err := NewUploadService(store, custody, DefaultUploadSessionTTL)
	require.NoError(t, err)
	service.random = bytes.NewReader(bytes.Repeat([]byte{1}, uploadIDRandomSize))
	service.now = func() time.Time { return now }
	return service
}

type memoryUploadSessionStore struct {
	mu             sync.Mutex
	session        UploadSession
	data           []byte
	events         *[]string
	openReader     func([]byte) io.ReadCloser
	completeOnOpen bool
	resetErr       error
	createCalls    int
	resetCalls     int
	completeCalls  int
}

func newMemoryUploadSessionStore() *memoryUploadSessionStore {
	return new(memoryUploadSessionStore)
}

func (s *memoryUploadSessionStore) Create(
	_ context.Context,
	record UploadSession,
) (UploadSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.session.ID != "" {
		return s.session, false, nil
	}
	s.session = record
	return record, true, nil
}

func (s *memoryUploadSessionStore) Status(
	_ context.Context,
	identity AuthIdentity,
	uploadID string,
	_ time.Time,
) (UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session.ID != uploadID || s.session.Identity != identity {
		return UploadSession{}, ErrNotFound
	}
	return s.session, nil
}

func (s *memoryUploadSessionStore) Append(
	_ context.Context,
	identity AuthIdentity,
	uploadID string,
	expectedOffset int64,
	chunk []byte,
	_ time.Time,
) (UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		*s.events = append(*s.events, "append")
	}
	if s.session.ID != uploadID || s.session.Identity != identity {
		return UploadSession{}, ErrNotFound
	}
	if s.session.Offset != expectedOffset {
		return UploadSession{}, &UploadOffsetConflictError{
			CurrentOffset: s.session.Offset,
		}
	}
	if int64(len(chunk)) > s.session.Object.Length-s.session.Offset {
		return UploadSession{}, ErrInvalid
	}
	s.data = append(s.data, chunk...)
	s.session.Offset += int64(len(chunk))
	return s.session, nil
}

func (s *memoryUploadSessionStore) Open(
	_ context.Context,
	identity AuthIdentity,
	uploadID string,
	_ time.Time,
) (UploadSession, io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		*s.events = append(*s.events, "open")
	}
	if s.session.ID != uploadID || s.session.Identity != identity {
		return UploadSession{}, nil, ErrNotFound
	}
	if s.completeOnOpen {
		s.completeOnOpen = false
		s.session.Complete = true
		return UploadSession{}, nil, ErrNotFound
	}
	data := append([]byte(nil), s.data...)
	if s.openReader != nil {
		return s.session, s.openReader(data), nil
	}
	return s.session, io.NopCloser(bytes.NewReader(data)), nil
}

func (s *memoryUploadSessionStore) Reset(
	_ context.Context,
	identity AuthIdentity,
	uploadID string,
	expectedGeneration int64,
	_ time.Time,
) (UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session.ID != uploadID || s.session.Identity != identity {
		return UploadSession{}, ErrNotFound
	}
	if s.session.Generation != expectedGeneration {
		return UploadSession{}, ErrConflict
	}
	s.resetCalls++
	if s.resetErr != nil {
		return UploadSession{}, s.resetErr
	}
	s.data = nil
	s.session.Offset = 0
	s.session.Generation++
	return s.session, nil
}

func (s *memoryUploadSessionStore) Complete(
	_ context.Context,
	identity AuthIdentity,
	uploadID string,
	_ time.Time,
) (UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		*s.events = append(*s.events, "complete")
	}
	if s.session.ID != uploadID || s.session.Identity != identity {
		return UploadSession{}, ErrNotFound
	}
	s.completeCalls++
	s.session.Complete = true
	return s.session, nil
}

type memoryUploadCustody struct {
	mu              sync.Mutex
	objectPresent   bool
	finalizeErr     error
	finalizedBody   []byte
	finalizeCalls   int
	events          *[]string
	finalizeStarted chan struct{}
	finalizeRelease chan struct{}
}

func newMemoryUploadCustody() *memoryUploadCustody {
	return new(memoryUploadCustody)
}

func (s *memoryUploadCustody) MissingObjects(
	_ context.Context,
	_ AuthIdentity,
	_ parser.AgentType,
	objects []ObjectRef,
) ([]ObjectRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.objectPresent {
		return nil, nil
	}
	return objects, nil
}

func (s *memoryUploadCustody) FinalizeObject(
	_ context.Context,
	_ AuthIdentity,
	_ parser.AgentType,
	object ObjectRef,
	body io.Reader,
) (PutResult, error) {
	s.mu.Lock()
	if s.events != nil {
		*s.events = append(*s.events, "finalize")
	}
	s.finalizeCalls++
	started := s.finalizeStarted
	release := s.finalizeRelease
	finalizeErr := s.finalizeErr
	s.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return PutResult{}, err
	}
	s.mu.Lock()
	s.finalizedBody = data
	s.mu.Unlock()
	if finalizeErr != nil {
		return PutResult{}, finalizeErr
	}
	return PutResult{Info: ObjectInfo{Ref: object}, Created: true}, nil
}

type cancelAfterFirstRead struct {
	reader io.Reader
	cancel context.CancelFunc
	once   sync.Once
}

func (r *cancelAfterFirstRead) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.once.Do(r.cancel)
	return read, err
}

var _ UploadSessionStore = (*memoryUploadSessionStore)(nil)
var _ UploadCustody = (*memoryUploadCustody)(nil)
