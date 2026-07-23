package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/docbank"
)

type artifactOriginsBody struct {
	Origins    []string `json:"origins"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type artifactPostBody struct {
	Origin    string `json:"origin"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Hash      string `json:"hash,omitempty"`
	Size      int64  `json:"size"`
	Duplicate bool   `json:"duplicate"`
}

type artifactIndexPageBody struct {
	artifact.OriginArtifactIndex
	NextCursor string `json:"next_cursor,omitempty"`
}

type repeatedByteReader struct{ value byte }

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for index := range p {
		p[index] = r.value
	}
	return len(p), nil
}

type cancelingRequestReader struct {
	cancel context.CancelFunc
	value  byte
	sent   bool
}

func (r *cancelingRequestReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, context.Canceled
	}
	if len(p) > 64<<10 {
		p = p[:64<<10]
	}
	for index := range p {
		p[index] = r.value
	}
	r.sent = true
	r.cancel()
	return len(p), nil
}

type cancelAfterChecksContext struct {
	context.Context
	checks   atomic.Int64
	cancelAt int64
	cancel   context.CancelFunc
}

func (c *cancelAfterChecksContext) Err() error {
	if c.checks.Add(1) >= c.cancelAt {
		if c.cancel != nil {
			c.cancel()
			return c.Context.Err()
		}
		return context.Canceled
	}
	return nil
}

type countingHTTPResponse struct {
	header http.Header
	status int
	bytes  int64
}

func seedArtifactStore(
	t *testing.T, store artifact.ArtifactStore, origin string, kind artifact.Kind, body []byte,
) artifact.Ref {
	t.Helper()
	hash := sha256.Sum256(body)
	name := hex.EncodeToString(hash[:])
	if kind == artifact.KindSegments {
		name += ".ndjson"
	}
	ref, err := artifact.NewRef(origin, kind, name)
	require.NoError(t, err)
	result, err := store.Create(t.Context(), ref, artifact.Identity{
		SHA256: hex.EncodeToString(hash[:]), Size: int64(len(body)),
	}, "application/octet-stream", bytes.NewReader(body))
	require.NoError(t, err)
	assert.Equal(t, ref, result.Entry.Ref)
	return ref
}

func exportArtifactFixture(
	t *testing.T, ctx context.Context, database *db.DB, origin string,
) (artifact.ArtifactStore, artifact.ExportResult) {
	t.Helper()
	repository, err := artifact.OpenRepository(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	result, err := artifact.ExportToStore(ctx, database, repository.Content(), artifact.ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)
	return repository.Content(), result
}

func oneArtifactRef(
	t *testing.T, store artifact.ArtifactStore, origin string, kind artifact.Kind,
) artifact.Ref {
	t.Helper()
	entries := collectArtifactEntries(t, store, origin, kind, 10)
	require.Len(t, entries, 1)
	return entries[0].Ref
}

func wireArtifact(
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

func TestArtifactMaintenanceRouteUsesDaemonOwnedStore(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"))
	body := strings.NewReader(`{"grace_seconds":3600,"quarantine_grace_seconds":7200,"max_objects":8,"max_bytes":1048576}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/maintenance", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	te.handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Logical struct {
			Origins int `json:"origins"`
		} `json:"logical"`
		Physical struct {
			Supported bool `json:"supported"`
		} `json:"physical"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.Zero(t, response.Logical.Origins)
	assert.True(t, response.Physical.Supported,
		"the daemon-owned Docbank store must service physical maintenance")
}

func TestArtifactMaintenanceRouteRejectsOverflowingGrace(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"))
	body := strings.NewReader(`{"grace_seconds":9223372036854775807}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/maintenance", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	te.handler.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
}

func TestArtifactMaintenanceRouteRejectsCursorFromAnotherStage(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"))
	body := strings.NewReader(`{"max_objects":1,"gc_cursor":"agentsview-artifact-maintenance:v1:empty-trash"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/maintenance", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()

	te.handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
}

func TestArtifactMaintenanceRouteRejectsOversizedBudgetBeforeLogicalRetention(t *testing.T) {
	store := &maintenanceBudgetProbeStore{}
	te := setupWithServerOpts(t, []server.Option{server.WithArtifactStore(store)}, withAuth("secret"))
	body := strings.NewReader(fmt.Sprintf(`{"max_objects":%d}`, docbank.MaxMaintenanceObjects+1))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/maintenance", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()

	te.handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadRequest, recorder.Code, recorder.Body.String())
	assert.Zero(t, store.listOrigins,
		"invalid physical limits must be rejected before logical retention reads the store")
}

func TestArtifactMaintenanceRoutePreservesExplicitZeroBudgets(t *testing.T) {
	store := &maintenanceBudgetProbeStore{}
	te := setupWithServerOpts(t, []server.Option{server.WithArtifactStore(store)}, withAuth("secret"))
	body := strings.NewReader(`{"max_objects":0,"max_bytes":0}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/maintenance", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()

	te.handler.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, artifact.WorkBudget{}, store.emptyTrash)
	assert.Equal(t, artifact.WorkBudget{}, store.gc)
	assert.Equal(t, artifact.WorkBudget{}, store.repack)
}

type maintenanceBudgetProbeStore struct {
	artifact.ArtifactStore
	listOrigins int
	emptyTrash  artifact.WorkBudget
	gc          artifact.WorkBudget
	repack      artifact.WorkBudget
}

func (s *maintenanceBudgetProbeStore) Origins(context.Context) (artifact.OriginIterator, error) {
	s.listOrigins++
	return emptyArtifactOriginIterator{}, nil
}

type emptyArtifactOriginIterator struct{}

func (emptyArtifactOriginIterator) Next(context.Context, int) ([]string, error) {
	return nil, io.EOF
}

func (emptyArtifactOriginIterator) Close() error { return nil }

func (s *maintenanceBudgetProbeStore) Verify(
	context.Context, artifact.WorkBudget,
) (artifact.MaintenanceResult, error) {
	return artifact.MaintenanceResult{}, nil
}

func (s *maintenanceBudgetProbeStore) EmptyTrash(
	_ context.Context, _ time.Duration, budget artifact.WorkBudget,
) (artifact.MaintenanceResult, error) {
	s.emptyTrash = budget
	return artifact.MaintenanceResult{}, nil
}

func (s *maintenanceBudgetProbeStore) GarbageCollect(
	_ context.Context, budget artifact.WorkBudget,
) (artifact.MaintenanceResult, error) {
	s.gc = budget
	return artifact.MaintenanceResult{}, nil
}

func (s *maintenanceBudgetProbeStore) Repack(
	_ context.Context, budget artifact.WorkBudget,
) (artifact.MaintenanceResult, error) {
	s.repack = budget
	return artifact.MaintenanceResult{}, nil
}

func (w *countingHTTPResponse) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *countingHTTPResponse) WriteHeader(status int) { w.status = status }

func (w *countingHTTPResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.bytes += int64(len(p))
	return len(p), nil
}

func repeatedByteHash(t *testing.T, value byte, size int64) string {
	t.Helper()
	hasher := sha256.New()
	_, err := io.CopyN(hasher, repeatedByteReader{value: value}, size)
	require.NoError(t, err)
	return hex.EncodeToString(hasher.Sum(nil))
}

func measureArtifactRouteAlloc(t *testing.T, run func()) uint64 {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	run()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func TestArtifactPeerPostRouteMemoryRemainsBounded(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	const origin = "peer-a1b2c3"
	measure := func(size int64) uint64 {
		name := repeatedByteHash(t, 'p', size)
		request := httptest.NewRequest(http.MethodPost,
			"/api/v1/artifacts/"+origin+"/raw/"+name,
			io.LimitReader(repeatedByteReader{value: 'p'}, size),
		)
		request.ContentLength = size
		request.Header.Set("Content-Type", "application/octet-stream")
		request.Header.Set("X-Agentsview-Artifact-Import", "deferred")
		response := &countingHTTPResponse{}
		allocated := measureArtifactRouteAlloc(t, func() {
			te.handler.ServeHTTP(response, request)
		})
		assert.Equal(t, http.StatusOK, response.status)
		return allocated
	}

	small := measure(1 << 20)
	large := measure(24 << 20)
	assert.Less(t, large, small+(4<<20),
		"production POST route allocation must not scale with request bytes")
}

func TestArtifactPeerGetRouteMemoryRemainsBounded(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	const origin = "peer-a1b2c3"
	measure := func(size int64) uint64 {
		name := repeatedByteHash(t, 'g', size)
		post := httptest.NewRequest(http.MethodPost,
			"/api/v1/artifacts/"+origin+"/raw/"+name,
			io.LimitReader(repeatedByteReader{value: 'g'}, size),
		)
		post.ContentLength = size
		post.Header.Set("Content-Type", "application/octet-stream")
		post.Header.Set("X-Agentsview-Artifact-Import", "deferred")
		postResponse := &countingHTTPResponse{}
		te.handler.ServeHTTP(postResponse, post)
		require.Equal(t, http.StatusOK, postResponse.status)

		request := httptest.NewRequest(http.MethodGet,
			"/api/v1/artifacts/"+origin+"/raw/"+name, nil,
		)
		response := &countingHTTPResponse{}
		allocated := measureArtifactRouteAlloc(t, func() {
			te.handler.ServeHTTP(response, request)
		})
		assert.Equal(t, http.StatusOK, response.status)
		assert.Equal(t, size, response.bytes)
		return allocated
	}

	small := measure(1 << 20)
	large := measure(24 << 20)
	assert.Less(t, large, small+(4<<20),
		"production GET route allocation must not scale with response bytes")
}

func TestArtifactPeerRoutesHonorCancellationWithoutSuccessOrPublication(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	const origin = "peer-a1b2c3"

	postCtx, cancelPost := context.WithCancel(t.Context())
	postName := repeatedByteHash(t, 'c', 2<<20)
	post := httptest.NewRequest(http.MethodPost,
		"/api/v1/artifacts/"+origin+"/raw/"+postName,
		&cancelingRequestReader{cancel: cancelPost, value: 'c'},
	).WithContext(postCtx)
	post.Header.Set("Content-Type", "application/octet-stream")
	postResponse := &countingHTTPResponse{}
	te.handler.ServeHTTP(postResponse, post)
	assert.NotEqual(t, http.StatusOK, postResponse.status)
	assert.NoFileExists(t, filepath.Join(te.dataDir, "artifacts", origin, "raw", postName))

	getSize := int64(2 << 20)
	getName := repeatedByteHash(t, 'd', getSize)
	seed := httptest.NewRequest(http.MethodPost,
		"/api/v1/artifacts/"+origin+"/raw/"+getName,
		io.LimitReader(repeatedByteReader{value: 'd'}, getSize),
	)
	seed.Header.Set("Content-Type", "application/octet-stream")
	seed.Header.Set("X-Agentsview-Artifact-Import", "deferred")
	seedResponse := &countingHTTPResponse{}
	te.handler.ServeHTTP(seedResponse, seed)
	require.Equal(t, http.StatusOK, seedResponse.status)

	getContext := &cancelAfterChecksContext{Context: t.Context(), cancelAt: 8}
	get := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/"+origin+"/raw/"+getName, nil,
	).WithContext(getContext)
	getResponse := &countingHTTPResponse{}
	te.handler.ServeHTTP(getResponse, get)
	assert.NotEqual(t, http.StatusOK, getResponse.status)
	assert.Zero(t, getResponse.bytes)
	assert.GreaterOrEqual(t, getContext.checks.Load(), getContext.cancelAt)
}

func TestArtifactPeerIndexPagesWireNamesWithOpaqueCursor(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	const origin = "peer-a1b2c3"
	refs := make(map[string]artifact.Ref, 513)
	for index := range 513 {
		body := fmt.Appendf(nil, "raw-%04d", index)
		ref := seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw, body)
		refs[ref.Name] = ref
	}

	firstResponse := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/index", nil, "")
	assertStatus(t, firstResponse, http.StatusOK)
	var first artifactIndexPageBody
	require.NoError(t, json.Unmarshal(firstResponse.Body.Bytes(), &first))
	assert.Len(t, first.Raw, 512)
	require.NotEmpty(t, first.NextCursor)
	assert.NotContains(t, first.NextCursor, first.Raw[len(first.Raw)-1],
		"cursor must be opaque rather than a raw filename")
	firstNames := make(map[string]struct{}, len(first.Raw))
	for _, name := range first.Raw {
		firstNames[name] = struct{}{}
	}
	remaining := ""
	for name := range refs {
		if _, found := firstNames[name]; !found {
			remaining = name
			break
		}
	}
	require.NotEmpty(t, remaining)
	require.NoError(t, te.artifactStore.Trash(t.Context(), refs[remaining]))

	secondResponse := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/index?cursor="+url.QueryEscape(first.NextCursor), nil, "")
	assertStatus(t, secondResponse, http.StatusOK)
	var second artifactIndexPageBody
	require.NoError(t, json.Unmarshal(secondResponse.Body.Bytes(), &second))
	assert.Len(t, second.Raw, 1)
	assert.Empty(t, second.NextCursor)
	assert.Equal(t, remaining, second.Raw[0],
		"later pages must come from the initial snapshot")
}

func TestArtifactPeerOriginsUseBoundedSnapshotCursor(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	refs := make(map[string]artifact.Ref, 512)
	for index := range 512 {
		origin := fmt.Sprintf("peer-%04d-a1b2c3", index)
		refs[origin] = seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw,
			fmt.Appendf(nil, "origin-%04d", index))
	}

	firstResponse := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/origins?limit=512", nil, "")
	assertStatus(t, firstResponse, http.StatusOK)
	var first artifactOriginsBody
	require.NoError(t, json.Unmarshal(firstResponse.Body.Bytes(), &first))
	assert.Len(t, first.Origins, 512)
	require.NotEmpty(t, first.NextCursor)

	removed := "peer-0511-a1b2c3"
	require.NoError(t, te.artifactStore.Trash(t.Context(), refs[removed]))
	secondResponse := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/origins?cursor="+url.QueryEscape(first.NextCursor), nil, "")
	assertStatus(t, secondResponse, http.StatusOK)
	var second artifactOriginsBody
	require.NoError(t, json.Unmarshal(secondResponse.Body.Bytes(), &second))
	assert.Equal(t, []string{removed}, second.Origins,
		"later pages must advance a stable snapshot instead of rescanning")
	assert.Empty(t, second.NextCursor)
}

func TestArtifactPeerCursorReleaseInvalidatesContinuation(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	for index := range 513 {
		origin := fmt.Sprintf("peer-%04d-a1b2c3", index)
		seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw,
			fmt.Appendf(nil, "origin-%04d", index))
	}

	firstResponse := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/origins?limit=512", nil, "")
	assertStatus(t, firstResponse, http.StatusOK)
	var first artifactOriginsBody
	require.NoError(t, json.Unmarshal(firstResponse.Body.Bytes(), &first))
	require.NotEmpty(t, first.NextCursor)

	releaseResponse := artifactPeerRequest(t, te, http.MethodDelete,
		"/api/v1/artifacts/cursors/"+url.PathEscape(first.NextCursor), nil, "")
	assertStatus(t, releaseResponse, http.StatusNoContent)

	staleResponse := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/origins?cursor="+url.QueryEscape(first.NextCursor), nil, "")
	assertStatus(t, staleResponse, http.StatusBadRequest)
}

func TestArtifactPeerHighCardinalityOriginsRetainOneBoundedCursor(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	for index := range 513 {
		origin := fmt.Sprintf("peer-%04d-a1b2c3", index)
		seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw,
			fmt.Appendf(nil, "origin-%04d", index))
	}

	response := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/origins?limit=512", nil, "")
	assertStatus(t, response, http.StatusOK)
	var page artifactOriginsBody
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	assert.Len(t, page.Origins, 512)
	require.NotEmpty(t, page.NextCursor)
	release := artifactPeerRequest(t, te, http.MethodDelete,
		"/api/v1/artifacts/cursors/"+url.PathEscape(page.NextCursor), nil, "")
	assertStatus(t, release, http.StatusNoContent)
	stale := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/origins?cursor="+url.QueryEscape(page.NextCursor), nil, "")
	assertStatus(t, stale, http.StatusBadRequest)
}

func TestArtifactPeerOriginSnapshotCancellationReleasesSpool(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	for index := range 513 {
		origin := fmt.Sprintf("peer-%04d-a1b2c3", index)
		seedArtifactStore(t, te.artifactStore, origin, artifact.KindRaw,
			fmt.Appendf(nil, "origin-%04d", index))
	}
	base, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ctx := &cancelAfterChecksContext{Context: base, cancelAt: 2, cancel: cancel}
	request := httptest.NewRequest(http.MethodGet,
		"/api/v1/artifacts/origins?limit=512", nil).WithContext(ctx)
	response := httptest.NewRecorder()

	te.handler.ServeHTTP(response, request)

	assert.NotEqual(t, http.StatusOK, response.Code)
	assert.GreaterOrEqual(t, ctx.checks.Load(), ctx.cancelAt)
}

type artifactRemoteStore struct {
	db.Store
}

func (artifactRemoteStore) ReadOnly() bool { return true }

func (artifactRemoteStore) MachineSessionCounts(
	context.Context,
) (map[string]int, error) {
	return map[string]int{}, nil
}

func (artifactRemoteStore) CountMetadataConflicts(context.Context) (int, error) {
	return 0, nil
}

func TestArtifactPeerRoutesRequireBearerAuthWhenConfigured(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"))

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "")
	assertStatus(t, w, http.StatusUnauthorized)

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "secret")
	assertStatus(t, w, http.StatusOK)
}

func TestArtifactPeerRoutesPostDuplicateAndFetch(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"))
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	posted := decode[artifactPostBody](t, w)
	assert.False(t, posted.Duplicate)
	assert.Equal(t, "meta", posted.Kind)
	assert.Equal(t, metadataName, posted.Name)

	w = artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	posted = decode[artifactPostBody](t, w)
	assert.True(t, posted.Duplicate)

	w = artifactPeerRequest(
		t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		nil, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, metadataBody, w.Body.Bytes())

	checkpoint := []byte(`{"origin":"peer-a1b2c3","seq":1,"sessions":{},"v":1}` + "\n")
	w = artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/checkpoints/cp-0000000001.json",
		checkpoint, "secret",
	)
	assertStatus(t, w, http.StatusOK)

	w = artifactPeerRequest(
		t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/checkpoint",
		nil, "secret",
	)
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, checkpoint, w.Body.Bytes())

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "secret")
	assertStatus(t, w, http.StatusOK)
	origins := decode[artifactOriginsBody](t, w)
	assert.Contains(t, origins.Origins, origin)
}

func TestArtifactPeerRawAndZstdFetchesUseBinaryMediaTypeAndExactBytes(t *testing.T) {
	te := setupArtifact(t)
	origin := "peer-a1b2c3"
	rawBody := []byte{0x00, 0xff, 0x80, 0x7f, 0x01}
	rawHash := sha256.Sum256(rawBody)
	rawName := hex.EncodeToString(rawHash[:])

	request := httptest.NewRequest(http.MethodPost,
		"/api/v1/artifacts/"+origin+"/raw/"+rawName, bytes.NewReader(rawBody))
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Agentsview-Artifact-Import", "deferred")
	response := httptest.NewRecorder()
	te.handler.ServeHTTP(response, request)
	assertStatus(t, response, http.StatusOK)

	w := artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/raw/"+rawName, nil, "")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, rawBody, w.Body.Bytes())

	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })
	first := "binary contract"
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(session *db.Session) {
		session.FirstMessage = &first
	})
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	peerStore, _ := exportArtifactFixture(t, t.Context(), peerDB, origin)
	segmentRef := oneArtifactRef(t, peerStore, origin, artifact.KindSegments)
	segmentWire, segmentBody := wireArtifact(t, peerStore, segmentRef)
	postArtifactBodyDeferred(t, te, segmentWire, segmentBody)

	w = artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/"+origin+"/segments/"+url.PathEscape(segmentWire.Name),
		nil, "")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, "application/octet-stream", w.Header().Get("Content-Type"))
	assert.Equal(t, segmentBody, w.Body.Bytes())
}

func TestArtifactPeerMutationRoutesAreAbsentInRemoteMode(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		WriteTimeout: 30 * time.Second,
	}
	srv := server.New(cfg, artifactRemoteStore{}, nil)
	te := &testEnv{
		srv:     srv,
		handler: wrapTestHandler(cfg, srv.Handler()),
		dataDir: dir,
	}
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "",
	)

	assertStatus(t, w, http.StatusNotFound)
}

func TestArtifactPeerReadRoutesServeInjectedStoreInRemoteMode(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	cfg := config.Config{
		Host:         "127.0.0.1",
		Port:         0,
		DataDir:      dir,
		WriteTimeout: 30 * time.Second,
	}
	repository, err := artifact.OpenRepository(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	origin := "peer-a1b2c3"
	raw := []byte("remote artifact")
	ref := seedArtifactStore(t, repository.Content(), origin, artifact.KindRaw, raw)

	srv := server.New(cfg, artifactRemoteStore{}, nil, server.WithArtifactRepository(repository))
	te := &testEnv{
		srv:     srv,
		handler: wrapTestHandler(cfg, srv.Handler()),
		dataDir: dir,
	}

	for _, path := range []string{
		"/api/v1/artifacts/origins",
		"/api/v1/artifacts/" + origin + "/index",
		"/api/v1/artifacts/" + origin + "/raw/" + ref.Name,
	} {
		w := artifactPeerRequest(t, te, http.MethodGet, path, nil, "")
		assertStatus(t, w, http.StatusOK)
	}
}

func TestArtifactPeerMutationRoutesAreAbsentForReadOnlySQLite(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	dbPath := filepath.Join(dir, "test.db")
	writable, err := db.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, writable.Close())
	readonly, err := db.OpenReadOnly(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { readonly.Close() })

	cfg := config.Config{
		Host:             "127.0.0.1",
		Port:             0,
		DataDir:          dir,
		DBPath:           dbPath,
		ArtifactOriginID: "desktop-d4e5f6",
		WriteTimeout:     30 * time.Second,
	}
	srv := server.New(cfg, readonly, nil)
	te := &testEnv{
		srv:     srv,
		handler: wrapTestHandler(cfg, srv.Handler()),
		db:      readonly,
		dataDir: dir,
	}
	origin := "peer-a1b2c3"
	metadataBody, metadataName := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(metadataName),
		metadataBody, "",
	)

	assertStatus(t, w, http.StatusNotFound)
}

func TestArtifactResetDaemonRouteMovesAsideAndReportsManualRecovery(t *testing.T) {
	dir := tempDirWithRetryCleanup(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dir, "test.db"))
	origin := "desktop-d4e5f6"
	startedAt := "2026-06-14T01:02:03Z"
	require.NoError(t, database.UpsertSession(db.Session{
		ID: "local-session", Machine: "local", Agent: "codex", Project: "project-a",
		StartedAt: &startedAt, CreatedAt: startedAt,
	}))
	repository, err := artifact.OpenRepository(t.Context(), dir)
	require.NoError(t, err)
	foreign := seedArtifactStore(t, repository.Content(), "peer-a1b2c3", artifact.KindRaw,
		[]byte("foreign relay bytes"))
	cfg := config.Config{
		Host: "127.0.0.1", DataDir: dir, DBPath: filepath.Join(dir, "test.db"),
		ArtifactOriginID: origin, WriteTimeout: 30 * time.Second,
		RequireAuth: true, AuthToken: "daemon-secret",
	}
	srv := server.New(cfg, database, nil, server.WithArtifactRepository(repository))
	t.Cleanup(func() {
		require.NoError(t, srv.Shutdown(context.Background()))
		require.NoError(t, repository.Close())
	})
	te := &testEnv{srv: srv, handler: wrapTestHandler(cfg, srv.Handler()), db: database, dataDir: dir}

	w := artifactPeerRequest(t, te, http.MethodPost, "/api/v1/artifacts/reset", nil, "daemon-secret")
	assertStatus(t, w, http.StatusOK)
	var response struct {
		VaultRoot        string                `json:"vault_root"`
		DiagnosticRoot   string                `json:"diagnostic_root"`
		Export           artifact.ExportResult `json:"export"`
		ManualCleanup    string                `json:"manual_cleanup"`
		ForeignArtifacts string                `json:"foreign_artifacts"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	assert.NotEmpty(t, response.VaultRoot)
	assert.DirExists(t, response.VaultRoot)
	assert.DirExists(t, response.DiagnosticRoot)
	assert.Equal(t, artifact.ArtifactResetManualCleanupWarning, response.ManualCleanup)
	assert.Equal(t, artifact.ArtifactResetForeignRelayWarning, response.ForeignArtifacts)
	assert.NotZero(t, response.Export.CheckpointSequence)

	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/origins", nil, "daemon-secret")
	assertStatus(t, w, http.StatusOK)
	origins := decode[artifactOriginsBody](t, w)
	assert.Equal(t, []string{origin}, origins.Origins)
	w = artifactPeerRequest(t, te, http.MethodGet,
		"/api/v1/artifacts/peer-a1b2c3/raw/"+foreign.Name, nil, "daemon-secret")
	assertStatus(t, w, http.StatusNotFound)
	session, err := database.GetSession(t.Context(), "local-session")
	require.NoError(t, err)
	assert.NotNil(t, session)
}

type artifactPeerBody struct {
	Origin            string `json:"origin"`
	IsLocal           bool   `json:"is_local"`
	CheckpointSeq     int    `json:"checkpoint_seq"`
	PublishedSessions int    `json:"published_sessions"`
	LocalSessions     int    `json:"local_sessions"`
	LastPublished     string `json:"last_published"`
}

type artifactPeersBody struct {
	LocalOrigin    string             `json:"local_origin"`
	Peers          []artifactPeerBody `json:"peers"`
	ConflictCount  int                `json:"conflict_count"`
	PendingImports int                `json:"pending_imports"`
	OldestPending  string             `json:"oldest_pending_at"`
}

func TestArtifactPeersStatus(t *testing.T) {
	local := "desktop-d4e5f6"
	te := setupArtifact(t, withArtifactOrigin(local))
	ctx := context.Background()
	first := "hi"

	// Two owned sessions, exported so the local origin gets a checkpoint.
	dbtest.SeedSession(t, te.db, "local-1", "proj", func(s *db.Session) { s.FirstMessage = &first })
	dbtest.SeedSession(t, te.db, "local-2", "proj", func(s *db.Session) { s.FirstMessage = &first })
	exported, err := artifact.ExportToStore(ctx, te.db, te.artifactStore, artifact.ExportOptions{
		Origin: local,
		Full:   true,
	})
	require.NoError(t, err)
	require.Equal(t, 2, exported.ExportedSessions)

	// A foreign peer publishes one session that the server imports.
	origin := "peer-a1b2c3"
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) { s.FirstMessage = &first })
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
	}))
	peerStore, _ := exportArtifactFixture(t, ctx, peerDB, origin)
	postArtifactRef(t, te, peerStore, oneArtifactRef(t, peerStore, origin, artifact.KindSegments))
	postArtifactRef(t, te, peerStore, oneArtifactRef(t, peerStore, origin, artifact.KindManifests))
	postArtifactRef(t, te, peerStore, oneArtifactRef(t, peerStore, origin, artifact.KindCheckpoints))

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)

	assert.Equal(t, local, body.LocalOrigin)
	assert.Equal(t, 0, body.ConflictCount)
	require.Len(t, body.Peers, 2)

	byOrigin := map[string]artifactPeerBody{}
	for _, p := range body.Peers {
		byOrigin[p.Origin] = p
	}

	localPeer, ok := byOrigin[local]
	require.True(t, ok, "local origin present in peers")
	assert.True(t, localPeer.IsLocal)
	assert.Equal(t, 2, localPeer.PublishedSessions)
	assert.Equal(t, 2, localPeer.LocalSessions)
	assert.NotEmpty(t, localPeer.LastPublished)

	peer, ok := byOrigin[origin]
	require.True(t, ok, "foreign origin present in peers")
	assert.False(t, peer.IsLocal)
	assert.Equal(t, 1, peer.PublishedSessions)
	assert.Equal(t, 1, peer.LocalSessions)
	assert.Equal(t, 1, peer.CheckpointSeq)

	// Landing is checkpoint provenance, not visibility in the session list.
	// A locally trashed replica is still fully imported from this checkpoint.
	require.NoError(t, te.db.SoftDeleteSession(origin+"~sess-1"))
	w = artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body = decode[artifactPeersBody](t, w)
	for _, p := range body.Peers {
		if p.Origin == origin {
			assert.Equal(t, 1, p.LocalSessions)
		}
	}
}

func TestArtifactPeersStatusDoesNotCountUnrelatedMachineRows(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	origin := "peer-a1b2c3"
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })
	first := "published"
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) {
		s.FirstMessage = &first
	})
	peerStore, _ := exportArtifactFixture(t, context.Background(), peerDB, origin)

	// Publish only the checkpoint, leaving its manifest unresolved, then add a
	// stale row for the same machine that is not a member of that checkpoint.
	postArtifactRef(t, te, peerStore,
		oneArtifactRef(t, peerStore, origin, artifact.KindCheckpoints))
	dbtest.SeedSession(t, te.db, origin+"~stale", "old", func(s *db.Session) {
		s.Machine = origin
	})

	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)
	assert.Equal(t, 1, body.PendingImports)
	assert.NotEmpty(t, body.OldestPending)
	for _, p := range body.Peers {
		if p.Origin == origin {
			assert.Equal(t, 1, p.PublishedSessions)
			assert.Zero(t, p.LocalSessions,
				"rows outside the latest checkpoint must not satisfy peer status")
			return
		}
	}
	t.Fatal("foreign peer missing from status")
}

func TestArtifactPeersStatusPublishesEmptyLocalOrigin(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	// Discovery publishes an explicit empty checkpoint for a configured origin.
	w := artifactPeerRequest(t, te, http.MethodGet, "/api/v1/artifacts/peers", nil, "")
	assertStatus(t, w, http.StatusOK)
	body := decode[artifactPeersBody](t, w)
	assert.Equal(t, "desktop-d4e5f6", body.LocalOrigin)
	require.Len(t, body.Peers, 1)
	assert.True(t, body.Peers[0].IsLocal)
	assert.Equal(t, 0, body.Peers[0].PublishedSessions)
	assert.Equal(t, 1, body.Peers[0].CheckpointSeq)
	assert.NotEmpty(t, body.Peers[0].LastPublished)
}

func TestArtifactPeerPostRejectsHashMismatch(t *testing.T) {
	te := setupArtifact(t, withAuth("secret"))
	origin := "peer-a1b2c3"
	metadataBody, _ := peerMetadataArtifact(
		origin,
		"2026-06-14T010203.000000001Z-00000000000000000000",
	)
	badName := "2026-06-14T010203.000000001Z-peer-a1b2c3-" + strings.Repeat("0", 64)

	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+origin+"/meta/"+url.PathEscape(badName),
		metadataBody, "secret",
	)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestArtifactPeerPostImportsAndEmitsDataChanged(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	origin := "peer-a1b2c3"
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })

	first := "hello"
	started := "2026-06-14T01:02:03Z"
	ended := "2026-06-14T01:03:03Z"
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) {
		s.MessageCount = 2
		s.UserMessageCount = 1
		s.FirstMessage = &first
		s.StartedAt = &started
		s.EndedAt = &ended
	})
	require.NoError(t, peerDB.ReplaceSessionMessages("sess-1", []db.Message{
		{SessionID: "sess-1", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "sess-1", Ordinal: 1, Role: "assistant", Content: "world", ContentLength: 5},
	}))
	peerStore, _ := exportArtifactFixture(t, context.Background(), peerDB, origin)

	postArtifactRef(t, te, peerStore, oneArtifactRef(t, peerStore, origin, artifact.KindSegments))
	postArtifactRef(t, te, peerStore, oneArtifactRef(t, peerStore, origin, artifact.KindManifests))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	stream := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		te.handler.ServeHTTP(stream, req)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)

	postArtifactRef(t, te, peerStore, oneArtifactRef(t, peerStore, origin, artifact.KindCheckpoints))
	te.waitForSSEEvent(t, stream, "data_changed", 3*time.Second)

	// Live clients only refresh the session index on the "sessions"
	// scope and only invalidate hydrated session details on the
	// "messages" scope; an import needs both.
	assert.Eventually(t, func() bool {
		scopes := dataChangedScopes(stream)
		return scopes["messages"] && scopes["sessions"]
	}, 3*time.Second, 10*time.Millisecond,
		"import must emit data_changed with both messages and sessions scopes")

	got, err := te.db.GetSession(context.Background(), origin+"~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, origin, got.Machine)
	assert.Equal(t, "alpha", got.Project)

	cancel()
	<-done
}

func TestArtifactPeerDeferredBatchImportsOnlyAtFinalize(t *testing.T) {
	te := setupArtifact(t, withArtifactOrigin("desktop-d4e5f6"))
	const origin = "peer-a1b2c3"
	peerDB, err := db.Open(filepath.Join(t.TempDir(), "peer.db"))
	require.NoError(t, err)
	t.Cleanup(func() { peerDB.Close() })
	first := "batched"
	dbtest.SeedSession(t, peerDB, "sess-1", "alpha", func(s *db.Session) {
		s.FirstMessage = &first
	})
	peerStore, _ := exportArtifactFixture(t, context.Background(), peerDB, origin)

	for _, kind := range []artifact.Kind{
		artifact.KindSegments, artifact.KindManifests, artifact.KindCheckpoints,
	} {
		postArtifactRefDeferred(t, te, peerStore, oneArtifactRef(t, peerStore, origin, kind))
	}
	got, err := te.db.GetSession(context.Background(), origin+"~sess-1")
	require.NoError(t, err)
	assert.Nil(t, got, "deferred uploads must not repeatedly import partial batches")

	w := artifactPeerRequest(
		t, te, http.MethodPost, "/api/v1/artifacts/finalize", nil, "",
	)
	assertStatus(t, w, http.StatusOK)
	got, err = te.db.GetSession(context.Background(), origin+"~sess-1")
	require.NoError(t, err)
	require.NotNil(t, got, "finalize must import the completed artifact batch")
	assert.Equal(t, "alpha", got.Project)
}

// dataChangedScopes collects the scope payloads of every data_changed
// event written to the SSE stream so far.
func dataChangedScopes(w *flushRecorder) map[string]bool {
	scopes := make(map[string]bool)
	for _, e := range parseSSE(w.BodyString()) {
		if e.Event != "data_changed" {
			continue
		}
		var payload struct {
			Scope string `json:"scope"`
		}
		if json.Unmarshal([]byte(e.Data), &payload) == nil {
			scopes[payload.Scope] = true
		}
	}
	return scopes
}

func artifactPeerRequest(
	t *testing.T,
	te *testEnv,
	method string,
	path string,
	body []byte,
	token string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	return w
}

func postArtifactRef(
	t *testing.T, te *testEnv, store artifact.ArtifactStore, ref artifact.Ref,
) {
	t.Helper()
	wire, body := wireArtifact(t, store, ref)
	w := artifactPeerRequest(
		t, te, http.MethodPost,
		"/api/v1/artifacts/"+ref.Origin+"/"+string(ref.Kind)+"/"+url.PathEscape(wire.Name),
		body, "",
	)
	assertStatus(t, w, http.StatusOK)
}

func postArtifactBodyDeferred(
	t *testing.T, te *testEnv, wire artifact.WireRef, body []byte,
) {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/artifacts/"+wire.Origin+"/"+string(wire.Kind)+"/"+url.PathEscape(wire.Name),
		bytes.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Agentsview-Artifact-Import", "deferred")
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assertStatus(t, w, http.StatusOK)
}

func postArtifactRefDeferred(
	t *testing.T, te *testEnv, store artifact.ArtifactStore, ref artifact.Ref,
) {
	t.Helper()
	wire, body := wireArtifact(t, store, ref)
	postArtifactBodyDeferred(t, te, wire, body)
}

func peerMetadataArtifact(origin, hlc string) ([]byte, string) {
	body := []byte(`{"hlc":"` + hlc + `","op":"rename","origin":"` + origin + `","session_gid":"` + origin + `~sess-1","v":1,"value":{"display_name":"Remote"}}` + "\n")
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	return body, hlc + "-" + hash + ".json"
}
