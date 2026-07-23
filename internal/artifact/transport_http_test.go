package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// fakeArtifactPeer is an in-memory peer implementing the artifact API surface
// the HTTP transport exchanges against: origin listing, per-origin index, and
// artifact get/post.
type fakeArtifactPeer struct {
	mu               sync.Mutex
	arts             map[string][]byte // "origin/kind/name" -> bytes
	posts            []string
	deferredPosts    int
	finalizeCalls    int
	supportsFinalize bool
}

type pagedArtifactStoreObservations struct {
	ArtifactStore
	mu                  sync.Mutex
	originPages         int
	listPages           map[Kind]int
	maxRequestedEntries int
	maxReturnedEntries  int
}

func (s *pagedArtifactStoreObservations) Origins(ctx context.Context) (OriginIterator, error) {
	iterator, err := s.ArtifactStore.Origins(ctx)
	if err != nil {
		return nil, err
	}
	return &testOriginIterator{
		next: func(ctx context.Context, limit int) ([]string, error) {
			origins, err := iterator.Next(ctx, limit)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.originPages++
			s.maxRequestedEntries = max(s.maxRequestedEntries, limit)
			s.maxReturnedEntries = max(s.maxReturnedEntries, len(origins))
			return origins, err
		},
		close: iterator.Close,
	}, nil
}

func (s *pagedArtifactStoreObservations) Entries(
	ctx context.Context, origin string, kind Kind,
) (EntryIterator, error) {
	iterator, err := s.ArtifactStore.Entries(ctx, origin, kind)
	if err != nil {
		return nil, err
	}
	return &testEntryIterator{
		next: func(ctx context.Context, limit int) ([]Entry, error) {
			entries, err := iterator.Next(ctx, limit)
			s.mu.Lock()
			defer s.mu.Unlock()
			s.listPages[kind]++
			s.maxRequestedEntries = max(s.maxRequestedEntries, limit)
			s.maxReturnedEntries = max(s.maxReturnedEntries, len(entries))
			return entries, err
		},
		close: iterator.Close,
	}, nil
}

type queuedTransportRepairStore struct {
	ArtifactStore
	pending Entry
	repairs int
}

type cleanupFailingRepairStore struct {
	ArtifactStore
	pending Entry
}

func (s *cleanupFailingRepairStore) PendingTransportRepair(
	context.Context, Ref,
) (Entry, bool, error) {
	return s.pending, true, nil
}

func (s *cleanupFailingRepairStore) RepairTransportArtifact(
	_ context.Context, _ Entry, trusted io.Reader,
) error {
	file, ok := trusted.(*os.File)
	if !ok {
		return errors.New("repair spool is not a file")
	}
	return file.Close()
}

func (s *cleanupFailingRepairStore) AcknowledgeTransportRepair(
	context.Context, Entry,
) error {
	return nil
}

type cancelDuringOpenStore struct {
	ArtifactStore
	cancel context.CancelFunc
	after  int
}

func (s *cancelDuringOpenStore) Open(
	ctx context.Context, ref Ref,
) (Entry, VerifiedReader, error) {
	entry, reader, err := s.ArtifactStore.Open(ctx, ref)
	if err != nil {
		return Entry{}, nil, err
	}
	return entry, &cancelDuringVerifiedRead{
		VerifiedReader: reader,
		cancel:         s.cancel,
		remaining:      s.after,
	}, nil
}

type cancelDuringVerifiedRead struct {
	VerifiedReader
	cancel    context.CancelFunc
	remaining int
	canceled  bool
}

func (r *cancelDuringVerifiedRead) Read(p []byte) (int, error) {
	if r.canceled {
		return 0, context.Canceled
	}
	if len(p) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.VerifiedReader.Read(p)
	r.remaining -= n
	if r.remaining <= 0 {
		r.cancel()
		r.canceled = true
	}
	return n, err
}

func (s *queuedTransportRepairStore) PendingTransportRepair(
	ctx context.Context, ref Ref,
) (Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, false, err
	}
	return s.pending, s.pending.Ref == ref, nil
}

func (s *queuedTransportRepairStore) RepairTransportArtifact(
	ctx context.Context, entry Entry, trusted io.Reader,
) error {
	if entry != s.pending {
		return errors.New("unexpected repair identity")
	}
	if err := s.Quarantine(ctx, entry.Ref, "test repair"); err != nil {
		return err
	}
	if _, err := s.Create(ctx, entry.Ref, entry.Identity,
		canonicalArtifactMediaType(entry.Ref.Kind), trusted); err != nil {
		return err
	}
	s.repairs++
	return nil
}

func (s *queuedTransportRepairStore) AcknowledgeTransportRepair(
	ctx context.Context, entry Entry,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry != s.pending {
		return errors.New("unexpected repair acknowledgement")
	}
	s.pending = Entry{}
	return nil
}

func newFakeArtifactPeer() *fakeArtifactPeer {
	return &fakeArtifactPeer{arts: map[string][]byte{}, supportsFinalize: true}
}

func (p *fakeArtifactPeer) put(origin, kind, name string, data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.arts[origin+"/"+kind+"/"+name] = append([]byte(nil), data...)
}

func (p *fakeArtifactPeer) has(origin, kind, name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.arts[origin+"/"+kind+"/"+name]
	return ok
}

func (p *fakeArtifactPeer) postedKinds() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	kinds := make([]string, 0, len(p.posts))
	for _, key := range p.posts {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) == 3 {
			kinds = append(kinds, parts[1])
		}
	}
	return kinds
}

func (p *fakeArtifactPeer) batchCounts() (deferredPosts, finalizeCalls int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deferredPosts, p.finalizeCalls
}

func (p *fakeArtifactPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, artifactAPIPath+"/")
	p.mu.Lock()
	defer p.mu.Unlock()
	if rest == "origins" {
		seen := map[string]bool{}
		for key := range p.arts {
			seen[strings.SplitN(key, "/", 2)[0]] = true
		}
		origins := make([]string, 0, len(seen))
		for origin := range seen {
			origins = append(origins, origin)
		}
		sort.Strings(origins)
		_ = json.NewEncoder(w).Encode(map[string]any{"origins": origins})
		return
	}
	if rest == "finalize" && r.Method == http.MethodPost {
		if !p.supportsFinalize {
			http.NotFound(w, r)
			return
		}
		p.finalizeCalls++
		w.WriteHeader(http.StatusOK)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[1] == "index" {
		idx := OriginArtifactIndex{Origin: parts[0]}
		for key := range p.arts {
			kp := strings.SplitN(key, "/", 3)
			if kp[0] != parts[0] {
				continue
			}
			switch kp[1] {
			case KindCheckpoints:
				idx.Checkpoints = append(idx.Checkpoints, kp[2])
			case KindManifests:
				idx.Manifests = append(idx.Manifests, kp[2])
			case KindSegments:
				idx.Segments = append(idx.Segments, kp[2])
			case KindMeta:
				idx.Meta = append(idx.Meta, kp[2])
			case KindRaw:
				idx.Raw = append(idx.Raw, kp[2])
			}
		}
		sort.Strings(idx.Checkpoints)
		sort.Strings(idx.Manifests)
		sort.Strings(idx.Segments)
		sort.Strings(idx.Meta)
		sort.Strings(idx.Raw)
		_ = json.NewEncoder(w).Encode(idx)
		return
	}
	if len(parts) == 3 {
		switch r.Method {
		case http.MethodGet:
			data, ok := p.arts[rest]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		case http.MethodPost:
			data, _ := io.ReadAll(r.Body)
			p.arts[rest] = data
			p.posts = append(p.posts, rest)
			if r.Header.Get("X-Agentsview-Artifact-Import") == "deferred" {
				p.deferredPosts++
			}
			w.WriteHeader(http.StatusCreated)
		}
		return
	}
	http.NotFound(w, r)
}

func TestHTTPTransportRequiresTLSForNonLoopbackPeers(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "public HTTP", target: "http://203.0.113.10:8080", wantErr: true},
		{name: "hostname HTTP", target: "http://peer.example.test:8080", wantErr: true},
		{name: "public HTTPS", target: "https://peer.example.test:8443"},
		{name: "localhost HTTP", target: "http://localhost:8080"},
		{name: "IPv4 loopback HTTP", target: "http://127.0.0.1:8080"},
		{name: "IPv6 loopback HTTP", target: "http://[::1]:8080"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := newHTTPTransport(tt.target, "", false)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "requires HTTPS")
				assert.Nil(t, tr)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, tr)
		})
	}
}

func TestHTTPTransportAllowsExplicitRemotePlaintextOptIn(t *testing.T) {
	const target = "http://peer.example.test:8080"
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	tr, err := newHTTPTransport(target, "", true)

	require.NoError(t, err)
	require.NotNil(t, tr)
	assert.Equal(t, "http://peer.example.test:8080"+artifactAPIPath, tr.base)
	assert.Contains(t, logs.String(), "warning")
	assert.Contains(t, logs.String(), "plaintext HTTP")
	assert.NotContains(t, logs.String(), target)
	assert.NotContains(t, logs.String(), "peer.example.test")
}

func TestHTTPTransportPrepareHonorsCanceledSync(t *testing.T) {
	var requests atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"origins": []string{}})
	}))
	t.Cleanup(peer.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Sync(ctx, testDB(t), SyncOptions{
		DataDir: t.TempDir(),
		Target:  peer.URL,
		Origin:  "laptop-a1b2c3",
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load(), "canceled preparation must not contact the peer")
}

func TestHTTPOriginIteratorDetectsArbitraryCursorCycleInConstantState(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		requests.Add(1)
		next := map[string]string{"": "a", "a": "b", "b": "c", "c": "b"}[r.URL.Query().Get("cursor")]
		_ = json.NewEncoder(w).Encode(httpOriginsPage{NextCursor: next})
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)
	iterator := &httpOriginIterator{transport: transport}
	t.Cleanup(func() { require.NoError(t, iterator.Close(context.Background())) })

	_, _, err = iterator.Next(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cursor cycle")
	assert.LessOrEqual(t, requests.Load(), int32(8))
}

func TestHTTPOriginIteratorRejectsDuplicateAcrossPageBoundary(t *testing.T) {
	const origin = "peer-a1b2c3"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		page := httpOriginsPage{Origins: []string{origin}}
		if r.URL.Query().Get("cursor") == "" {
			page.NextCursor = "next"
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)
	iterator := &httpOriginIterator{transport: transport}
	t.Cleanup(func() { require.NoError(t, iterator.Close(context.Background())) })

	got, ok, err := iterator.Next(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, origin, got)
	_, _, err = iterator.Next(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestHTTPWireIteratorRejectsDuplicateAcrossPageBoundary(t *testing.T) {
	const origin = "peer-a1b2c3"
	name := strings64("a")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		page := httpIndexPage{OriginArtifactIndex: OriginArtifactIndex{
			Origin: origin,
			Raw:    []string{name},
		}}
		if r.URL.Query().Get("cursor") == "" {
			page.NextCursor = "next"
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)
	iterator := &httpWireIterator{transport: transport, origin: origin}
	t.Cleanup(func() { require.NoError(t, iterator.Close()) })

	_, ok, err := iterator.Next(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	_, _, err = iterator.Next(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestHTTPSyncReusesPreparedOriginsForFirstExchange(t *testing.T) {
	peer := newFakeArtifactPeer()
	var originRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/origins") {
			originRequests.Add(1)
		}
		peer.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	_, err := Sync(context.Background(), testDB(t), SyncOptions{
		DataDir: t.TempDir(),
		Target:  server.URL,
		Origin:  "laptop-a1b2c3",
	})
	require.NoError(t, err)
	assert.Equal(t, int32(1), originRequests.Load(),
		"prepare and the first pull must share one peer snapshot")
}

func TestHTTPTransportPullSkipsCorruptRemoteArtifact(t *testing.T) {
	origin := "desktop-d4e5f6"
	remoteStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
	})
	remoteIdx, err := listTransportArtifacts(context.Background(), remoteStore, origin)
	require.NoError(t, err)
	peer := newFakeArtifactPeer()
	for _, item := range indexItems(remoteIdx) {
		art, err := readTransportArtifact(context.Background(), remoteStore, origin, item.kind, item.name)
		require.NoError(t, err)
		peer.put(origin, item.kind, item.name, art)
	}
	corruptName := hashHex([]byte("corrupt")) + segmentExtension
	peer.put(origin, KindSegments, corruptName, []byte("garbage"))

	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)
	localStore := openTransportStore(t, filepath.Join(t.TempDir(), "artifacts"))
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	gotIdx, err := listTransportArtifacts(context.Background(), localStore, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(remoteIdx), indexItems(gotIdx))
	corruptRef, err := FromWireRef(origin, KindSegments, corruptName)
	require.NoError(t, err)
	_, err = localStore.Stat(t.Context(), corruptRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
}

func TestHTTPTransportIgnoresUncatalogedCorruptLocalArtifact(t *testing.T) {
	origin := "laptop-a1b2c3"
	localStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	validIdx, err := listTransportArtifacts(context.Background(), localStore, origin)
	require.NoError(t, err)
	corruptName := hashHex([]byte("junk")) + segmentExtension
	corruptRef, err := FromWireRef(origin, KindSegments, corruptName)
	require.NoError(t, err)
	_, err = localStore.Stat(t.Context(), corruptRef)
	require.ErrorIs(t, err, ErrArtifactNotFound)

	peer := newFakeArtifactPeer()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	for _, item := range indexItems(validIdx) {
		assert.True(t, peer.has(origin, item.kind, item.name),
			"expected %s/%s on the peer", item.kind, item.name)
	}
	assert.False(t, peer.has(origin, KindSegments, corruptName))
	_, err = localStore.Stat(t.Context(), corruptRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound,
		"transport enumeration must not invent uncataloged logical artifacts")
}

func TestHTTPTransportPushPublishesDependenciesBeforeCheckpoint(t *testing.T) {
	localStore := exportStore(t, "laptop-a1b2c3", func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	peer := newFakeArtifactPeer()
	server := httptest.NewServer(peer)
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)

	require.NoError(t, transport.Exchange(context.Background(), localStore))

	assert.Equal(t,
		[]string{KindSegments, KindManifests, KindCheckpoints},
		peer.postedKinds(),
	)
}

func TestHTTPTransportExchangeDetectsDivergentCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	localStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpointRef := onlyTransportRef(t, localStore, origin, KindCheckpoints)
	wire, err := ToWireRef(checkpointRef)
	require.NoError(t, err)

	// The peer holds a different, equally valid checkpoint under the same
	// sequence name, as a rebuilt store under a reused origin id would.
	divergent, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~other": hashHex([]byte("other"))},
	})
	require.NoError(t, err)
	peer := newFakeArtifactPeer()
	peer.put(origin, KindCheckpoints, wire.Name, divergent)

	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)

	err = tr.Exchange(context.Background(), localStore)
	require.Error(t, err)
	assert.ErrorIs(t, err, errArtifactPathConflict)
}

func TestHTTPTransportExchangeRepairsCorruptLocalCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	baseStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpointRef := onlyTransportRef(t, baseStore, origin, KindCheckpoints)
	wire, valid := wireTransportArtifact(t, baseStore, checkpointRef)
	localStore := &corruptUntilRepairedStore{ArtifactStore: baseStore, corruptRef: checkpointRef}
	_, reader, err := localStore.Open(t.Context(), checkpointRef)
	require.NoError(t, err)
	require.Error(t, reader.Verify())
	require.NoError(t, reader.Close())

	peer := newFakeArtifactPeer()
	peer.put(origin, KindCheckpoints, wire.Name, valid)

	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	assert.True(t, localStore.repaired, "corrupt local checkpoint should be re-fetched from the peer")
	_, got := wireTransportArtifact(t, localStore, checkpointRef)
	assert.Equal(t, valid, got)
}

func TestHTTPTransportPostArtifactSetsPeerOrigin(t *testing.T) {
	var gotOrigin, gotImportMode string
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrigin = r.Header.Get("Origin")
		gotImportMode = r.Header.Get("X-Agentsview-Artifact-Import")
		w.WriteHeader(http.StatusCreated)
	}))
	defer peer.Close()

	tr, err := newHTTPTransport(peer.URL+"/api/v1/artifacts", "", false)
	require.NoError(t, err)

	body := bytes.NewReader([]byte("artifact"))
	err = tr.postArtifact(context.Background(), "peer-a1b2c3", KindSegments, strings64("a"), body, int64(body.Len()))
	require.NoError(t, err)

	assert.Equal(t, peer.URL, gotOrigin)
	assert.Equal(t, "deferred", gotImportMode)
}

func TestHTTPTransportFinalizesEveryPushOnce(t *testing.T) {
	origin := "laptop-a1b2c3"
	localStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	peer := newFakeArtifactPeer()
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)

	require.NoError(t, tr.Exchange(context.Background(), localStore))
	deferred, finalized := peer.batchCounts()
	assert.Positive(t, deferred, "every artifact in the batch must defer import")
	assert.Equal(t, 1, finalized, "one push must trigger one import finalization")

	// A retry with no missing artifacts must still finalize a batch interrupted
	// after its uploads were stored but before the earlier finalize request.
	require.NoError(t, tr.Exchange(context.Background(), localStore))
	deferredAfterRetry, finalizedAfterRetry := peer.batchCounts()
	assert.Equal(t, deferred, deferredAfterRetry)
	assert.Equal(t, 2, finalizedAfterRetry)
}

func TestHTTPTransportUploadCardinalityUsesBoundedStorePagesAndOneFinalize(t *testing.T) {
	if testing.Short() {
		t.Skip("513-object real-Docbank cardinality regression")
	}
	type observation struct {
		uploads             int
		finalizes           int
		originPages         int
		rawPages            int
		maxRequestedEntries int
		maxReturnedEntries  int
	}
	measure := func(t *testing.T, artifacts int) observation {
		t.Helper()
		ctx := SuppressArtifactMaintenance(t.Context())
		base, err := newProtocolTestStore(t.TempDir())
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, base.Close()) })
		for index := range artifacts {
			body := []byte("logical artifact " + strconv.Itoa(index))
			identity := identityForBytes(t, body)
			ref := requireContractRef(t, contractOrigin, KindRaw, identity.SHA256)
			_, err = base.Create(ctx, ref, identity,
				canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
			require.NoError(t, err)
		}
		observed := &pagedArtifactStoreObservations{
			ArtifactStore: base,
			listPages:     make(map[Kind]int),
		}
		peer := newFakeArtifactPeer()
		server := httptest.NewServer(peer)
		t.Cleanup(server.Close)
		transport, err := newHTTPTransport(server.URL, "", false)
		require.NoError(t, err)
		require.NoError(t, transport.Exchange(ctx, observed))

		uploads, finalizes := peer.batchCounts()
		observed.mu.Lock()
		defer observed.mu.Unlock()
		return observation{
			uploads:             uploads,
			finalizes:           finalizes,
			originPages:         observed.originPages,
			rawPages:            observed.listPages[KindRaw],
			maxRequestedEntries: observed.maxRequestedEntries,
			maxReturnedEntries:  observed.maxReturnedEntries,
		}
	}

	small := measure(t, 1)
	large := measure(t, transportPageSize+1)

	assert.Equal(t, observation{
		uploads:             1,
		finalizes:           1,
		originPages:         1,
		rawPages:            1,
		maxRequestedEntries: transportPageSize,
		maxReturnedEntries:  1,
	}, small)
	assert.Equal(t, transportPageSize+1, large.uploads)
	assert.Equal(t, 1, large.finalizes,
		"crossing the store page boundary must not split the peer import batch")
	assert.Equal(t, 1, large.originPages)
	assert.Equal(t, 2, large.rawPages)
	assert.Equal(t, transportPageSize, large.maxRequestedEntries)
	assert.Equal(t, transportPageSize, large.maxReturnedEntries)
}

func TestHTTPTransportFetchesQueuedRepairWhenBothIndexesContainName(t *testing.T) {
	body := []byte("trusted repair body")
	ref := requireContractRef(t, contractOrigin, KindRaw, hashHex(body))
	base, err := newProtocolTestStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	created, err := base.Create(t.Context(), ref, identityForBytes(t, body),
		canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
	require.NoError(t, err)
	local := &queuedTransportRepairStore{ArtifactStore: base, pending: created.Entry}
	peer := newFakeArtifactPeer()
	peer.put(ref.Origin, string(ref.Kind), ref.Name, body)
	server := httptest.NewServer(peer)
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)

	require.NoError(t, transport.Exchange(t.Context(), local))

	assert.Equal(t, 1, local.repairs)
	assert.Empty(t, local.pending.Ref)
	assert.Equal(t, body, readContractArtifact(t, local, ref))
}

func TestQueuedTransportRepairReturnsSpoolCleanupFailure(t *testing.T) {
	body := []byte("trusted repair body")
	identity := identityForBytes(t, body)
	ref := requireContractRef(t, contractOrigin, KindRaw, identity.SHA256)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	store := &cleanupFailingRepairStore{pending: Entry{Ref: ref, Identity: identity}}

	repaired, err := repairQueuedTransportArtifact(t.Context(), store, wire,
		func(consume func(io.Reader) error) error {
			return consume(bytes.NewReader(body))
		})

	assert.False(t, repaired)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "file already closed")
}

func TestHTTPTransportAcceptsLegacyPeerWithoutFinalize(t *testing.T) {
	localStore := exportStore(t, "laptop-a1b2c3", func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	peer := newFakeArtifactPeer()
	peer.supportsFinalize = false
	srv := httptest.NewServer(peer)
	t.Cleanup(srv.Close)
	tr, err := newHTTPTransport(srv.URL, "", false)
	require.NoError(t, err)

	require.NoError(t, tr.Exchange(context.Background(), localStore),
		"legacy peers import each POST and return 404 for the new finalize route")
}

func TestHTTPTransportRejectsRedirectedArtifactPost(t *testing.T) {
	type requestCapture struct {
		authorization string
		body          []byte
		readErr       error
	}

	var destinationReached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationReached.Store(true)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(destination.Close)

	captured := make(chan requestCapture, 1)
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		captured <- requestCapture{
			authorization: r.Header.Get("Authorization"),
			body:          body,
			readErr:       err,
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	tr, err := newHTTPTransport(source.URL, "peer-secret", false)
	require.NoError(t, err)
	tr.client.Transport = source.Client().Transport

	body := bytes.NewReader([]byte("artifact-secret"))
	err = tr.postArtifact(context.Background(), "peer-a1b2c3", KindSegments, strings64("a"), body, int64(body.Len()))
	require.Error(t, err)
	assert.ErrorIs(t, err, errHTTPPeer)
	var got requestCapture
	select {
	case got = <-captured:
	case <-time.After(time.Second):
		require.FailNow(t, "redirect source was not reached", "timed out waiting for the artifact POST")
	}
	require.NoError(t, got.readErr)
	assert.Equal(t, "Bearer peer-secret", got.authorization)
	assert.Equal(t, []byte("artifact-secret"), got.body)
	assert.False(t, destinationReached.Load())
}

func TestHTTPTransportUploadMemoryRemainsBoundedByBufferSize(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)
	transport.origin = "sender-a1b2c3"

	measure := func(size int) uint64 {
		body := bytes.Repeat([]byte{'x'}, size)
		identity := identityForBytes(t, body)
		ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
		require.NoError(t, err)
		store := openTransportStore(t, t.TempDir())
		result, err := store.Create(t.Context(), ref, identity,
			canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
		require.NoError(t, err)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		require.NoError(t, transport.postEntry(t.Context(), store, result.Entry))
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	small := measure(1 << 20)
	large := measure(24 << 20)
	assert.Less(t, large, small+(4<<20),
		"upload allocation growth must not scale with artifact bytes")
	assert.Equal(t, int64(2), requests.Load())
}

func TestHTTPTransportRejectsOversizedMalformedPageWithBoundedMemory(t *testing.T) {
	measure := func(size int64) uint64 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"origins":["`)
			_, _ = io.CopyN(w, repeatedByteReader('x'), size)
		}))
		t.Cleanup(server.Close)
		transport, err := newHTTPTransport(server.URL, "", false)
		require.NoError(t, err)

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err = transport.getOriginsPage(t.Context(), "")
		runtime.ReadMemStats(&after)
		require.ErrorIs(t, err, ErrArtifactInvalid)
		assert.Contains(t, err.Error(), "response exceeds")
		return after.TotalAlloc - before.TotalAlloc
	}

	small := measure(2 << 20)
	large := measure(24 << 20)
	assert.Less(t, large, small+(4<<20),
		"malformed peer page allocation growth must remain bounded")
}

func TestHTTPTransportExchangeMemoryRemainsBoundedByArtifactSize(t *testing.T) {
	identityForSize := func(size int64) Identity {
		hasher := sha256.New()
		_, err := io.CopyN(hasher, repeatedByteReader('x'), size)
		require.NoError(t, err)
		identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), size)
		require.NoError(t, err)
		return identity
	}
	measureOutbound := func(size int64) uint64 {
		identity := identityForSize(size)
		ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
		require.NoError(t, err)
		local := openTransportStore(t, t.TempDir())
		_, err = local.Create(t.Context(), ref, identity,
			canonicalArtifactMediaType(ref.Kind), io.LimitReader(repeatedByteReader('x'), size))
		require.NoError(t, err)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/origins"):
				_ = json.NewEncoder(w).Encode(httpOriginsPage{})
			case strings.HasSuffix(r.URL.Path, "/index"):
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				_ = json.NewEncoder(w).Encode(OriginArtifactIndex{Origin: parts[len(parts)-2]})
			case strings.HasSuffix(r.URL.Path, "/finalize"):
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodPost:
				_, copyErr := io.Copy(io.Discard, r.Body)
				assert.NoError(t, copyErr)
				w.WriteHeader(http.StatusCreated)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		transport, err := newHTTPTransport(server.URL, "", false)
		require.NoError(t, err)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		require.NoError(t, transport.Exchange(t.Context(), local))
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	measureInbound := func(size int64) uint64 {
		identity := identityForSize(size)
		const origin = "peer-a1b2c3"
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/origins"):
				_ = json.NewEncoder(w).Encode(httpOriginsPage{Origins: []string{origin}})
			case strings.HasSuffix(r.URL.Path, "/index"):
				_ = json.NewEncoder(w).Encode(OriginArtifactIndex{
					Origin: origin,
					Raw:    []string{identity.SHA256},
				})
			case strings.HasSuffix(r.URL.Path, "/finalize"):
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet:
				w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
				_, copyErr := io.CopyN(w, repeatedByteReader('x'), size)
				assert.NoError(t, copyErr)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		transport, err := newHTTPTransport(server.URL, "", false)
		require.NoError(t, err)
		local := openTransportStore(t, t.TempDir())
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		require.NoError(t, transport.Exchange(t.Context(), local))
		runtime.ReadMemStats(&after)
		_, err = local.Stat(t.Context(), Ref{Origin: origin, Kind: KindRaw, Name: identity.SHA256})
		require.NoError(t, err)
		return after.TotalAlloc - before.TotalAlloc
	}

	smallOut, smallIn := measureOutbound(1<<20), measureInbound(1<<20)
	largeOut, largeIn := measureOutbound(24<<20), measureInbound(24<<20)
	assert.Less(t, largeOut, smallOut+(4<<20),
		"real HTTP Exchange outbound allocation growth must remain bounded")
	assert.Less(t, largeIn, smallIn+(4<<20),
		"real HTTP Exchange inbound allocation growth must remain bounded")
}

func TestHTTPTransportCancellationDuringVerificationSendsNoRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)
	transport.origin = "sender-a1b2c3"

	body := bytes.Repeat([]byte{'c'}, 2<<20)
	identity := identityForBytes(t, body)
	ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
	require.NoError(t, err)
	base := openTransportStore(t, t.TempDir())
	result, err := base.Create(t.Context(), ref, identity,
		canonicalArtifactMediaType(ref.Kind), bytes.NewReader(body))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	store := &cancelDuringOpenStore{ArtifactStore: base, cancel: cancel, after: 64 << 10}

	err = transport.postEntry(ctx, store, result.Entry)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load(), "verification must finish before request headers are sent")
}

func TestHTTPTransportCancellationStopsDownloadWithoutPublishing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(bytes.Repeat([]byte{'d'}, 64<<10))
		if flush, ok := w.(http.Flusher); ok {
			flush.Flush()
		}
		cancel()
	}))
	t.Cleanup(server.Close)
	transport, err := newHTTPTransport(server.URL, "", false)
	require.NoError(t, err)
	ref, err := NewRef("peer-a1b2c3", KindRaw, strings64("a"))
	require.NoError(t, err)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	store := openTransportStore(t, t.TempDir())

	err = transport.receiveArtifact(ctx, store, wire)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.Stat(t.Context(), ref)
	require.ErrorIs(t, err, ErrArtifactNotFound)
}
