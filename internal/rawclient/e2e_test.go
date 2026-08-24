package rawclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/server"
)

// TestRawClientEndToEnd drives the full client cycle — enrollment, token
// exchange, missing-object negotiation, resumable multi-chunk uploads, and
// manifest commits — against the real huma routes mounted with real
// rawsync services. Only the persistence seams are in-memory fakes defined
// in this file; every HTTP hop, validation layer, and custody boundary is
// production code.
func TestRawClientEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	uploads := newE2EUploadSessionStore()
	httpServer, auth := newE2ERawSyncServer(t, uploads)

	// Step 1: enroll a device through the real DeviceAuthService.
	// Server-side enrollment has no HTTP route by design; the issued
	// credential is exactly what a laptop would be provisioned with.
	enrollment, err := auth.EnrollDevice(ctx, "tenant-e2e", "laptop-e2e")
	require.NoError(t, err)
	assert.Equal(t, "tenant-e2e", enrollment.Identity.TenantID)
	assert.NotEmpty(t, enrollment.Identity.DeviceID)
	assert.NotEmpty(t, enrollment.Credential)

	// The laptop's checkpoint store records its device identity once.
	checkpointPath := t.TempDir() + "/rawcheckpoint.db"
	checkpoint, err := rawcheckpoint.Open(ctx, checkpointPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, checkpoint.Close()) })
	require.NoError(t, checkpoint.SetDevice(ctx, enrollment.Identity.DeviceID))
	storedDevice, ok, err := checkpoint.Device(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, enrollment.Identity.DeviceID, storedDevice)

	// A small ChunkBytes forces every object through multiple PATCHes.
	client, err := NewClient(Config{
		BaseURL:     httpServer.URL,
		DeviceID:    enrollment.Identity.DeviceID,
		Credential:  enrollment.Credential,
		HTTPClient:  httpServer.Client(),
		ChunkBytes:  8,
		TokenMargin: time.Minute,
	})
	require.NoError(t, err)

	// Step 2: build a two-object generation from fixture bytes with real
	// SHA-256 digests. Both bodies exceed two chunks at 8 bytes each.
	firstBody := []byte("first source object body, chunked")
	secondBody := []byte("second source object body")
	first := e2EObjectFor(t, firstBody)
	second := e2EObjectFor(t, secondBody)
	require.Greater(t, first.Length, int64(16))
	require.Greater(t, second.Length, int64(16))

	const (
		rootID    = "root-e2e"
		sourceKey = "projects/demo/session.jsonl"
	)
	capturedAt := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	entries := []rawsync.Entry{
		{
			Path: "session.jsonl", Type: "file",
			Length: first.Length, Objects: []rawsync.ObjectRef{first},
		},
		{
			Path: "meta/sidecar.json", Type: "file",
			Length: second.Length, Objects: []rawsync.ObjectRef{second},
		},
	}
	manifestFor := func(captureID string, captured time.Time, parent string) rawsync.Manifest {
		return rawsync.Manifest{
			SchemaVersion:         rawsync.ManifestSchemaVersion,
			Provider:              parser.AgentClaude,
			ConfiguredRootID:      rootID,
			SourceKey:             sourceKey,
			ExpectedParentReceipt: parent,
			CaptureID:             captureID,
			CapturedAt:            captured,
			Kind:                  rawsync.ManifestSnapshot,
			Entries:               entries,
		}
	}

	// Step 3: negotiate missing objects, then upload both through the
	// resumable data plane.
	objects := []rawsync.ObjectRef{first, second}
	missing, err := client.MissingObjects(ctx, parser.AgentClaude, objects)
	require.NoError(t, err)
	require.Len(t, missing, 2)
	assert.Equal(t, objects, missing, "both objects missing in request order")

	bodies := map[string][]byte{first.SHA256: firstBody, second.SHA256: secondBody}
	for _, object := range missing {
		require.NoError(t, client.UploadObject(
			ctx, parser.AgentClaude, object, bytes.NewReader(bodies[object.SHA256]),
		))
	}
	// Multi-chunk transfers actually happened: every session needed more
	// than one PATCH at the 8-byte chunk size.
	for uploadID, appends := range uploads.appendCounts() {
		assert.GreaterOrEqual(t, appends, 2, "upload %s used multiple chunks", uploadID)
	}

	// After custody the server holds both objects; renegotiation is empty.
	missing, err = client.MissingObjects(ctx, parser.AgentClaude, objects)
	require.NoError(t, err)
	assert.Empty(t, missing)

	// Step 4: commit the first generation with an empty expected parent
	// receipt, then advance the laptop's checkpoint from the durable result.
	firstCommit, err := client.CommitManifest(ctx, manifestFor(
		"capture-e2e-1", capturedAt, "",
	))
	require.NoError(t, err)
	assert.NotEmpty(t, firstCommit.ManifestID)
	assert.NotEmpty(t, firstCommit.Receipt)
	assert.Equal(t, int64(1), firstCommit.Generation)
	assert.True(t, firstCommit.Created)
	require.NoError(t, checkpoint.AdvanceHead(
		ctx, enrollment.Identity.DeviceID, parser.AgentClaude,
		rootID, sourceKey, "", firstCommit,
	))
	head, ok, err := checkpoint.SourceHead(ctx, parser.AgentClaude, rootID, sourceKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, firstCommit.Receipt, head.Receipt)
	assert.Equal(t, firstCommit.ManifestID, head.ManifestID)
	assert.Equal(t, int64(1), head.Generation)

	// The same capture_id replays idempotently: same receipt and
	// generation, never a second generation.
	replayed, err := client.CommitManifest(ctx, manifestFor(
		"capture-e2e-1", capturedAt, "",
	))
	require.NoError(t, err)
	assert.Equal(t, firstCommit.Receipt, replayed.Receipt)
	assert.Equal(t, firstCommit.Generation, replayed.Generation)
	assert.False(t, replayed.Created)

	// Step 5: a second generation chained onto the first receipt advances
	// the server head to generation 2.
	secondCommit, err := client.CommitManifest(ctx, manifestFor(
		"capture-e2e-2", capturedAt.Add(time.Minute), firstCommit.Receipt,
	))
	require.NoError(t, err)
	assert.NotEqual(t, firstCommit.Receipt, secondCommit.Receipt)
	assert.Equal(t, int64(2), secondCommit.Generation)
	assert.True(t, secondCommit.Created)
	require.NoError(t, checkpoint.AdvanceHead(
		ctx, enrollment.Identity.DeviceID, parser.AgentClaude,
		rootID, sourceKey, firstCommit.Receipt, secondCommit,
	))

	// Step 6: a stale parent receipt is rejected as a typed head conflict
	// carrying the current head, not a generic failure.
	_, err = client.CommitManifest(ctx, manifestFor(
		"capture-e2e-3", capturedAt.Add(2*time.Minute), firstCommit.Receipt,
	))
	require.Error(t, err)
	var apiErr APIError
	require.True(t, AsAPIError(err, &apiErr))
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Equal(t, CodeHeadConflict, apiErr.Code)
	assert.Equal(t, secondCommit.ManifestID, apiErr.CurrentManifestID)
	assert.Equal(t, secondCommit.Receipt, apiErr.CurrentReceipt)
	assert.Equal(t, int64(2), apiErr.CurrentGeneration)

	// Step 7: the checkpoint reflects only the last acknowledged head —
	// generation 2 — and the rejected stale commit advanced nothing.
	head, ok, err = checkpoint.SourceHead(ctx, parser.AgentClaude, rootID, sourceKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, secondCommit.Receipt, head.Receipt)
	assert.Equal(t, secondCommit.ManifestID, head.ManifestID)
	assert.Equal(t, int64(2), head.Generation)
}

// e2EObjectFor builds a validated ObjectRef carrying body's real digest.
func e2EObjectFor(t *testing.T, body []byte) rawsync.ObjectRef {
	t.Helper()
	digest := sha256.Sum256(body)
	object, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), int64(len(body)))
	require.NoError(t, err)
	return object
}

// newE2ERawSyncServer mounts the real huma server with real rawsync
// services: device authentication, custody, resumable uploads, and the
// verified artifact ledger backed by a throwaway repository.
func newE2ERawSyncServer(
	t *testing.T,
	uploads *e2EUploadSessionStore,
) (*httptest.Server, *rawsync.DeviceAuthService) {
	t.Helper()
	repository, err := artifact.OpenRepository(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)

	metadata := newE2EMetadataStore()
	custody, err := rawsync.NewService(
		objects, metadata, rawsync.DefaultManifestLimits(), "parser-data-17",
	)
	require.NoError(t, err)

	uploadService, err := rawsync.NewUploadService(
		uploads, custody, rawsync.DefaultUploadSessionTTL,
	)
	require.NoError(t, err)

	auth, err := rawsync.NewDeviceAuthService(
		newE2EDeviceAuthStore(), time.Hour,
	)
	require.NoError(t, err)

	srv := server.New(config.Config{
		Host: "127.0.0.1", Port: 8080,
		AuthToken: "legacy-shared-token", RequireAuth: true,
		WriteTimeout: 30 * time.Second,
	}, nil, nil,
		server.WithRawSyncServices(auth, custody),
		server.WithRawSyncUploads(uploadService),
	)
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer, auth
}

// e2EDeviceAuthStore is an in-memory DeviceAuthStore backed by maps. It
// mirrors the PostgreSQL store's semantics: enrollments persist once,
// credentials authenticate only active devices, tokens are digest-only and
// scope-checked, and revocation invalidates the device and its tokens.
type e2EDeviceAuthStore struct {
	mu      sync.Mutex
	devices map[string]rawsync.DeviceEnrollmentRecord
	tokens  map[rawsync.TokenDigest]rawsync.DeviceTokenRecord
}

func newE2EDeviceAuthStore() *e2EDeviceAuthStore {
	return &e2EDeviceAuthStore{
		devices: make(map[string]rawsync.DeviceEnrollmentRecord),
		tokens:  make(map[rawsync.TokenDigest]rawsync.DeviceTokenRecord),
	}
}

func (s *e2EDeviceAuthStore) EnrollDevice(
	_ context.Context,
	record rawsync.DeviceEnrollmentRecord,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.devices[record.Identity.DeviceID]; exists {
		return fmt.Errorf("enrolling raw sync device: %w", rawsync.ErrConflict)
	}
	s.devices[record.Identity.DeviceID] = record
	return nil
}

func (s *e2EDeviceAuthStore) activeIdentity(
	deviceID string,
	credential rawsync.CredentialDigest,
) (rawsync.AuthIdentity, bool) {
	record, exists := s.devices[deviceID]
	if !exists || record.CredentialDigest != credential || record.RevokedAt != nil {
		return rawsync.AuthIdentity{}, false
	}
	return record.Identity, true
}

func (s *e2EDeviceAuthStore) AuthenticateCredential(
	_ context.Context,
	deviceID string,
	credential rawsync.CredentialDigest,
) (rawsync.AuthIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	identity, ok := s.activeIdentity(deviceID, credential)
	if !ok {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	return identity, nil
}

func (s *e2EDeviceAuthStore) IssueToken(
	_ context.Context,
	deviceID string,
	credential rawsync.CredentialDigest,
	token rawsync.DeviceTokenRecord,
) (rawsync.AuthIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	identity, ok := s.activeIdentity(deviceID, credential)
	if !ok {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	token.Identity = identity
	s.tokens[token.Digest] = token
	return identity, nil
}

func (s *e2EDeviceAuthStore) AuthenticateToken(
	_ context.Context,
	digest rawsync.TokenDigest,
	required rawsync.DeviceTokenScope,
	now time.Time,
) (rawsync.AuthIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, exists := s.tokens[digest]
	if !exists || !token.ExpiresAt.After(now) || !token.Scopes.Allows(required) {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	device, exists := s.devices[token.Identity.DeviceID]
	if !exists || device.RevokedAt != nil || device.Identity != token.Identity {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	return token.Identity, nil
}

func (s *e2EDeviceAuthStore) RevokeDevice(
	_ context.Context,
	identity rawsync.AuthIdentity,
	revokedAt time.Time,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.devices[identity.DeviceID]
	if !exists || record.Identity != identity || record.RevokedAt != nil {
		return false, nil
	}
	record.RevokedAt = &revokedAt
	s.devices[identity.DeviceID] = record
	return true, nil
}

// e2EHeadKey identifies one source head exactly as the PostgreSQL store
// keys it: tenant, device, provider, root, and source key.
type e2EHeadKey struct {
	tenant   string
	device   string
	provider parser.AgentType
	root     string
	source   string
}

// e2ECaptureKey pairs one head with a capture_id for replay detection.
type e2ECaptureKey struct {
	head      e2EHeadKey
	captureID string
}

// e2EMetadataStore is an in-memory MetadataStore with the real head
// compare-and-swap semantics: manifests are accepted only when the expected
// parent receipt matches the current head, the first head carries an empty
// receipt at generation 0, commits increment the generation, and the same
// capture_id replays its original result idempotently.
type e2EMetadataStore struct {
	mu       sync.Mutex
	verified map[string]map[rawsync.ObjectRef]bool
	heads    map[e2EHeadKey]*e2ESourceHead
	captures map[e2ECaptureKey]rawsync.CommitResult
}

type e2ESourceHead struct {
	manifestID string
	receipt    string
	generation int64
}

func newE2EMetadataStore() *e2EMetadataStore {
	return &e2EMetadataStore{
		verified: make(map[string]map[rawsync.ObjectRef]bool),
		heads:    make(map[e2EHeadKey]*e2ESourceHead),
		captures: make(map[e2ECaptureKey]rawsync.CommitResult),
	}
}

func (s *e2EMetadataStore) RecordVerifiedObject(
	ctx context.Context,
	identity rawsync.AuthIdentity,
	object rawsync.ObjectRef,
) error {
	return s.RecordVerifiedObjects(ctx, identity, []rawsync.ObjectRef{object})
}

func (s *e2EMetadataStore) RecordVerifiedObjects(
	_ context.Context,
	identity rawsync.AuthIdentity,
	objects []rawsync.ObjectRef,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	verified := s.verified[identity.TenantID]
	if verified == nil {
		verified = make(map[rawsync.ObjectRef]bool)
		s.verified[identity.TenantID] = verified
	}
	for _, object := range objects {
		verified[object] = true
	}
	return nil
}

func (s *e2EMetadataStore) MissingObjects(
	_ context.Context,
	identity rawsync.AuthIdentity,
	objects []rawsync.ObjectRef,
) ([]rawsync.ObjectRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	missing := make([]rawsync.ObjectRef, 0, len(objects))
	for _, object := range objects {
		if !s.verified[identity.TenantID][object] {
			missing = append(missing, object)
		}
	}
	return missing, nil
}

func (s *e2EMetadataStore) CommitManifest(
	_ context.Context,
	manifest rawsync.CanonicalManifest,
	_ string,
) (rawsync.CommitResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := e2EHeadKey{
		tenant:   manifest.Identity.TenantID,
		device:   manifest.Identity.DeviceID,
		provider: manifest.Manifest.Provider,
		root:     manifest.Manifest.ConfiguredRootID,
		source:   manifest.Manifest.SourceKey,
	}
	capture := e2ECaptureKey{head: head, captureID: manifest.Manifest.CaptureID}
	if prior, exists := s.captures[capture]; exists {
		if prior.ManifestID != manifest.ManifestID {
			return rawsync.CommitResult{}, fmt.Errorf(
				"raw capture identifier reused: %w", rawsync.ErrConflict)
		}
		prior.Created = false
		return prior, nil
	}
	current := s.heads[head]
	if current == nil {
		current = &e2ESourceHead{}
		s.heads[head] = current
	}
	if manifest.Manifest.ExpectedParentReceipt != current.receipt {
		return rawsync.CommitResult{}, &rawsync.HeadConflictError{
			CurrentManifestID: current.manifestID,
			CurrentReceipt:    current.receipt,
			CurrentGeneration: current.generation,
		}
	}
	for _, object := range manifest.Objects {
		if !s.verified[manifest.Identity.TenantID][object] {
			return rawsync.CommitResult{}, fmt.Errorf(
				"%w: %s", rawsync.ErrMissingObject, object.SHA256)
		}
	}
	receipt, err := e2EReceipt()
	if err != nil {
		return rawsync.CommitResult{}, err
	}
	current.manifestID = manifest.ManifestID
	current.receipt = receipt
	current.generation++
	result := rawsync.CommitResult{
		ManifestID: manifest.ManifestID,
		Receipt:    receipt,
		Generation: current.generation,
		Created:    true,
	}
	s.captures[capture] = result
	return result, nil
}

// e2EReceipt mints a lowercase-hex receipt matching the parent-receipt
// format the manifest validator accepts.
func e2EReceipt() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generating e2e receipt: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// e2EUploadRecord is one staged transfer: its durable session metadata and
// the bytes appended so far.
type e2EUploadRecord struct {
	session rawsync.UploadSession
	data    []byte
}

// e2EUploadSessionStore is an in-memory multi-session UploadSessionStore
// staging bytes per upload ID at exact offsets. It mirrors
// internal/rawsync/upload_test.go's memoryUploadSessionStore semantics —
// offset conflicts report the authoritative offset, resets bump the session
// generation — but keeps every concurrent session alive, keyed by upload ID.
type e2EUploadSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*e2EUploadRecord
	appends  map[string]int
}

func newE2EUploadSessionStore() *e2EUploadSessionStore {
	return &e2EUploadSessionStore{
		sessions: make(map[string]*e2EUploadRecord),
		appends:  make(map[string]int),
	}
}

// appendCounts reports how many PATCHes each upload received.
func (s *e2EUploadSessionStore) appendCounts() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int, len(s.appends))
	maps.Copy(counts, s.appends)
	return counts
}

func (s *e2EUploadSessionStore) lookup(
	identity rawsync.AuthIdentity,
	uploadID string,
) (*e2EUploadRecord, error) {
	record, exists := s.sessions[uploadID]
	if !exists || record.session.Identity != identity {
		return nil, rawsync.ErrNotFound
	}
	return record, nil
}

func (s *e2EUploadSessionStore) Create(
	_ context.Context,
	record rawsync.UploadSession,
) (rawsync.UploadSession, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, exists := s.sessions[record.ID]; exists {
		return existing.session, false, nil
	}
	s.sessions[record.ID] = &e2EUploadRecord{session: record}
	return record, true, nil
}

func (s *e2EUploadSessionStore) Status(
	_ context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	_ time.Time,
) (rawsync.UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lookup(identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	return record.session, nil
}

func (s *e2EUploadSessionStore) Append(
	_ context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	expectedOffset int64,
	chunk []byte,
	_ time.Time,
) (rawsync.UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lookup(identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	s.appends[uploadID]++
	if record.session.Offset != expectedOffset {
		return rawsync.UploadSession{}, &rawsync.UploadOffsetConflictError{
			CurrentOffset: record.session.Offset,
		}
	}
	if int64(len(chunk)) > record.session.Object.Length-record.session.Offset {
		return rawsync.UploadSession{}, rawsync.ErrInvalid
	}
	record.data = append(record.data, chunk...)
	record.session.Offset += int64(len(chunk))
	return record.session, nil
}

func (s *e2EUploadSessionStore) Open(
	_ context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	_ time.Time,
) (rawsync.UploadSession, io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lookup(identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, nil, err
	}
	data := append([]byte(nil), record.data...)
	return record.session, io.NopCloser(bytes.NewReader(data)), nil
}

func (s *e2EUploadSessionStore) Reset(
	_ context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	expectedGeneration int64,
	_ time.Time,
) (rawsync.UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lookup(identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	if record.session.Generation != expectedGeneration {
		return rawsync.UploadSession{}, rawsync.ErrConflict
	}
	record.data = nil
	record.session.Offset = 0
	record.session.Generation++
	return record.session, nil
}

func (s *e2EUploadSessionStore) Complete(
	_ context.Context,
	identity rawsync.AuthIdentity,
	uploadID string,
	_ time.Time,
) (rawsync.UploadSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.lookup(identity, uploadID)
	if err != nil {
		return rawsync.UploadSession{}, err
	}
	record.session.Complete = true
	return record.session, nil
}

var (
	_ rawsync.DeviceAuthStore    = (*e2EDeviceAuthStore)(nil)
	_ rawsync.MetadataStore      = (*e2EMetadataStore)(nil)
	_ rawsync.UploadSessionStore = (*e2EUploadSessionStore)(nil)
)
