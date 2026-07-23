package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
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

// mockS3 is an in-memory, path-style S3-compatible server backing a single
// bucket. It implements just enough of ListObjectsV2, GetObject, PutObject, and
// DeleteObject to exercise the object-store transport, and verifies that
// requests arrive signed (Authorization plus x-amz-date) without re-validating
// the signature.
type mockS3 struct {
	t        *testing.T
	bucket   string
	pageSize int

	mu      sync.Mutex
	objects map[string][]byte
	deletes int
}

func newMockS3(t *testing.T, bucket string, pageSize int) *mockS3 {
	return &mockS3{
		t:        t,
		bucket:   bucket,
		pageSize: pageSize,
		objects:  map[string][]byte{},
	}
}

func (m *mockS3) put(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = append([]byte(nil), data...)
}

func (m *mockS3) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[key]
	return ok
}

func (m *mockS3) deleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deletes
}

func (m *mockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	assert.True(m.t, strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256"),
		"request must carry a SigV4 Authorization header")
	assert.NotEmpty(m.t, r.Header.Get("X-Amz-Date"), "request must carry an x-amz-date header")

	bucketPath := "/" + m.bucket
	if r.Method == http.MethodGet && r.URL.Path == bucketPath && r.URL.Query().Get("list-type") == "2" {
		m.list(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, bucketPath+"/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, bucketPath+"/")
	switch r.Method {
	case http.MethodGet:
		m.mu.Lock()
		data, ok := m.objects[key]
		m.mu.Unlock()
		if !ok {
			http.Error(w, "no such key", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
	case http.MethodPut:
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		// Honor the write-once conditional: reject when the key already exists.
		if r.Header.Get("If-None-Match") == "*" && m.has(key) {
			http.Error(w, "precondition failed", http.StatusPreconditionFailed)
			return
		}
		m.put(key, body)
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		m.mu.Lock()
		m.deletes++
		delete(m.objects, key)
		m.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	delimiter := r.URL.Query().Get("delimiter")
	token := r.URL.Query().Get("continuation-token")

	m.mu.Lock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	m.mu.Unlock()
	sort.Strings(keys)
	if delimiter != "" {
		seen := make(map[string]struct{})
		prefixes := make([]string, 0, len(keys))
		for _, key := range keys {
			remainder := strings.TrimPrefix(key, prefix)
			component, _, found := strings.Cut(remainder, delimiter)
			if !found {
				continue
			}
			common := prefix + component + delimiter
			if _, ok := seen[common]; ok {
				continue
			}
			seen[common] = struct{}{}
			prefixes = append(prefixes, common)
		}
		keys = prefixes
	}

	start := 0
	if token != "" {
		start, _ = strconv.Atoi(token)
	}
	pageSize := m.pageSize
	if pageSize <= 0 {
		pageSize = 1000
	}
	end := start + pageSize
	truncated := end < len(keys)
	if end > len(keys) {
		end = len(keys)
	}

	type contentsXML struct {
		Key string `xml:"Key"`
	}
	type resultXML struct {
		XMLName        xml.Name      `xml:"ListBucketResult"`
		IsTruncated    bool          `xml:"IsTruncated"`
		Contents       []contentsXML `xml:"Contents"`
		CommonPrefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
		NextContinuationToken string `xml:"NextContinuationToken,omitempty"`
	}
	out := resultXML{IsTruncated: truncated}
	for _, k := range keys[start:end] {
		if delimiter == "" {
			out.Contents = append(out.Contents, contentsXML{Key: k})
		} else {
			out.CommonPrefixes = append(out.CommonPrefixes, struct {
				Prefix string `xml:"Prefix"`
			}{Prefix: k})
		}
	}
	if truncated {
		out.NextContinuationToken = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/xml")
	require.NoError(m.t, xml.NewEncoder(w).Encode(out))
}

func testObjectOptions(endpoint string) ObjectStoreOptions {
	return ObjectStoreOptions{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		PathStyle:       true,
	}
}

func TestS3TransportPrepareHonorsCanceledSync(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("<ListBucketResult></ListBucketResult>"))
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = syncWithTransport(ctx, testDB(t), SyncOptions{
		DataDir: t.TempDir(),
		Target:  "s3://bucket/arts",
		Origin:  "laptop-a1b2c3",
	}, transport)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load(), "canceled preparation must not contact the object store")
}

func TestS3ListPageRejectsTruncatedPageWithoutTokenAndOversizedPage(t *testing.T) {
	tests := []struct {
		name string
		xml  string
		max  int
	}{
		{
			name: "missing continuation token",
			xml:  `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`,
			max:  1,
		},
		{
			name: "more keys than requested",
			xml: `<ListBucketResult><IsTruncated>false</IsTruncated>` +
				`<Contents><Key>a</Key></Contents><Contents><Key>b</Key></Contents></ListBucketResult>`,
			max: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tt.xml)
			}))
			t.Cleanup(server.Close)
			transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
			require.NoError(t, err)

			_, err = transport.listPage(t.Context(), "arts/", "", "", tt.max)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}
}

func TestS3OriginIteratorDetectsArbitraryContinuationCycle(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		next := map[string]string{"": "a", "a": "b", "b": "c", "c": "b"}[r.URL.Query().Get("continuation-token")]
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
			`<NextContinuationToken>`+next+`</NextContinuationToken></ListBucketResult>`)
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)
	iterator := &s3OriginIterator{transport: transport}

	_, _, err = iterator.Next(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "continuation token cycle")
	assert.LessOrEqual(t, requests.Load(), int32(8))
}

func TestS3WireIteratorDetectsArbitraryContinuationCycle(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		next := map[string]string{"": "a", "a": "b", "b": "c", "c": "b"}[r.URL.Query().Get("continuation-token")]
		_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated>`+
			`<NextContinuationToken>`+next+`</NextContinuationToken></ListBucketResult>`)
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)
	iterator := &s3WireIterator{transport: transport, origin: "peer-a1b2c3", kind: KindRaw}

	_, _, err = iterator.Next(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "continuation token cycle")
	assert.LessOrEqual(t, requests.Load(), int32(8))
}

func TestS3OriginIteratorRejectsDuplicateAcrossPageBoundary(t *testing.T) {
	const prefix = "arts/peer-a1b2c3/"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.URL.Query().Get("continuation-token")
		truncated := token == ""
		_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>%t</IsTruncated>`+
			`<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>`, truncated, prefix)
		if truncated {
			_, _ = io.WriteString(w, `<NextContinuationToken>next</NextContinuationToken>`)
		}
		_, _ = io.WriteString(w, `</ListBucketResult>`)
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)
	iterator := &s3OriginIterator{transport: transport}

	got, ok, err := iterator.Next(t.Context())
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "peer-a1b2c3", got)
	_, _, err = iterator.Next(t.Context())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrArtifactInvalid)
}

func TestS3WireIteratorRejectsDuplicateAndMalformedKeys(t *testing.T) {
	const origin = "peer-a1b2c3"
	validKey := "arts/" + origin + "/raw/" + strings64("a")
	tests := []struct {
		name      string
		secondKey string
	}{
		{name: "duplicate across pages", secondKey: validKey},
		{name: "malformed nested key", secondKey: "arts/" + origin + "/raw/nested/name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				token := r.URL.Query().Get("continuation-token")
				key := validKey
				truncated := token == ""
				if !truncated {
					key = tt.secondKey
				}
				_, _ = fmt.Fprintf(w, `<ListBucketResult><IsTruncated>%t</IsTruncated>`+
					`<Contents><Key>%s</Key></Contents>`, truncated, key)
				if truncated {
					_, _ = io.WriteString(w, `<NextContinuationToken>next</NextContinuationToken>`)
				}
				_, _ = io.WriteString(w, `</ListBucketResult>`)
			}))
			t.Cleanup(server.Close)
			transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
			require.NoError(t, err)
			iterator := &s3WireIterator{transport: transport, origin: origin, kind: KindRaw}

			_, ok, err := iterator.Next(t.Context())
			require.NoError(t, err)
			assert.True(t, ok)
			_, _, err = iterator.Next(t.Context())
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrArtifactInvalid)
		})
	}
}

// exportStore exports one origin's sessions into a fresh artifact store.
func exportStore(t *testing.T, origin string, seed func(*db.DB)) ArtifactStore {
	t.Helper()
	database := testDB(t)
	seed(database)
	store := openTransportStore(t, filepath.Join(t.TempDir(), "artifacts"))
	_, err := ExportToStore(t.Context(), database, store, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)
	return store
}

func TestS3TransportPushRoundTrip(t *testing.T) {
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	dataDir := t.TempDir()
	localRoot := filepath.Join(dataDir, "artifacts")
	require.NoError(t, os.MkdirAll(localRoot, 0o755))
	localStore, err := newProtocolTestStore(localRoot)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, localStore.Close()) })
	_, err = ExportToStore(t.Context(), database, localStore, ExportOptions{
		Origin: origin,
		Full:   true,
	})
	require.NoError(t, err)

	// Append a metadata event so a meta artifact is part of the push.
	rec := NewMetadataRecorder(database, MetadataRecorderOptions{
		Origin: origin,
		Store:  localStore,
		Now:    func() time.Time { return fixedHLCTime() },
	})
	_, err = database.StarSession("sess-1")
	require.NoError(t, err)
	_, err = rec.Append(context.Background(), MetadataEventInput{
		SessionID: "sess-1",
		Op:        MetadataOpStar,
	})
	require.NoError(t, err)

	mock := newMockS3(t, "bucket", 0)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Prepare(context.Background(), localStore))
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	idx, err := listTransportArtifacts(context.Background(), localStore, origin)
	require.NoError(t, err)
	items := indexItems(idx)
	require.NotEmpty(t, items)
	assert.NotEmpty(t, idx.Meta, "the star event should have produced a meta artifact")
	for _, item := range items {
		key := "arts/" + origin + "/" + item.kind + "/" + item.name
		assert.True(t, mock.has(key), "expected object %q in bucket", key)
	}
}

func TestS3TransportPullRoundTrip(t *testing.T) {
	origin := "desktop-d4e5f6"

	// Produce a populated store for the origin and upload it into the bucket so
	// the transport must pull it down into an empty local store.
	remoteStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
		seedSession(t, database, "sess-8", "beta")
	})
	remoteIdx, err := listTransportArtifacts(context.Background(), remoteStore, origin)
	require.NoError(t, err)
	uploaded := indexItems(remoteIdx)
	require.NotEmpty(t, uploaded)

	// Use a small page size and more than one object to exercise the
	// continuation-token pagination path.
	mock := newMockS3(t, "bucket", 2)
	for _, item := range uploaded {
		art, err := readTransportArtifact(context.Background(), remoteStore, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art)
	}
	require.Greater(t, len(uploaded), 2, "need multiple pages to test pagination")

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	localStore := openTransportStore(t, filepath.Join(t.TempDir(), "artifacts"))
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	gotIdx, err := listTransportArtifacts(context.Background(), localStore, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(remoteIdx), indexItems(gotIdx))
}

func TestS3TransportPullRetainsCorruptRemoteArtifactOverHTTP(t *testing.T) {
	origin := "desktop-d4e5f6"
	remoteStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
	})
	remoteIdx, err := listTransportArtifacts(context.Background(), remoteStore, origin)
	require.NoError(t, err)
	uploaded := indexItems(remoteIdx)
	mock := newMockS3(t, "bucket", 0)
	for _, item := range uploaded {
		art, err := readTransportArtifact(context.Background(), remoteStore, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art)
	}
	corruptName := hashHex([]byte("corrupt")) + segmentExtension
	mock.put("arts/"+origin+"/segments/"+corruptName, []byte("garbage"))

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	localStore := openTransportStore(t, filepath.Join(t.TempDir(), "artifacts"))
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	gotIdx, err := listTransportArtifacts(context.Background(), localStore, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, uploaded, indexItems(gotIdx))
	corruptRef, err := FromWireRef(origin, KindSegments, corruptName)
	require.NoError(t, err)
	_, err = localStore.Stat(t.Context(), corruptRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound)
	assert.True(t, mock.has("arts/"+origin+"/segments/"+corruptName))
	assert.Zero(t, mock.deleteCount())
}

func TestS3TransportPullDeletesCorruptRemoteObjectSoPushHeals(t *testing.T) {
	origin := "desktop-d4e5f6"
	ownerStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
	})
	ownerIdx, err := listTransportArtifacts(context.Background(), ownerStore, origin)
	require.NoError(t, err)
	require.Len(t, ownerIdx.Segments, 1)
	segKey := "arts/" + origin + "/segments/" + ownerIdx.Segments[0]

	mock := newMockS3(t, "bucket", 0)
	for _, item := range indexItems(ownerIdx) {
		art, err := readTransportArtifact(context.Background(), ownerStore, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art)
	}
	// The bucket copy is corrupted in place: its name still lists, so pushes
	// from valid holders would otherwise skip it forever.
	mock.put(segKey, []byte("garbage"))

	srv := httptest.NewTLSServer(mock)
	t.Cleanup(srv.Close)

	// An empty peer's pull fails to validate the object and deletes it, so
	// the name stops masking the valid copy.
	emptyStore := openTransportStore(t, filepath.Join(t.TempDir(), "artifacts"))
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	tr.client.Transport = srv.Client().Transport
	require.NoError(t, tr.Exchange(context.Background(), emptyStore))
	assert.False(t, mock.has(segKey), "corrupt object deleted from the bucket")
	assert.Equal(t, 1, mock.deleteCount(), "expected one DELETE for the corrupt object")

	// The owner's next exchange re-uploads its valid copy, and the empty
	// peer's next pull completes its store.
	ownerTr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	ownerTr.client.Transport = srv.Client().Transport
	require.NoError(t, ownerTr.Exchange(context.Background(), ownerStore))
	assert.True(t, mock.has(segKey), "valid copy re-uploaded")

	require.NoError(t, tr.Exchange(context.Background(), emptyStore))
	gotIdx, err := listTransportArtifacts(context.Background(), emptyStore, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(ownerIdx), indexItems(gotIdx))
}

func TestS3TransportRejectsRedirect(t *testing.T) {
	var sourceReached atomic.Bool
	var destinationReached atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		destinationReached.Store(true)
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
	}))
	t.Cleanup(destination.Close)

	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceReached.Store(true)
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(source.Close)

	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(source.URL))
	require.NoError(t, err)
	tr.client.Transport = source.Client().Transport

	_, err = tr.listPage(context.Background(), tr.prefixWithSlash(), "", "", 0)
	require.Error(t, err)
	assert.True(t, sourceReached.Load())
	assert.False(t, destinationReached.Load())
}

func TestS3TransportIgnoresUncatalogedCorruptLocalArtifact(t *testing.T) {
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

	mock := newMockS3(t, "bucket", 0)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	for _, item := range indexItems(validIdx) {
		assert.True(t, mock.has("arts/"+origin+"/"+item.kind+"/"+item.name),
			"expected object %s/%s in bucket", item.kind, item.name)
	}
	assert.False(t, mock.has("arts/"+origin+"/segments/"+corruptName))
	_, err = localStore.Stat(t.Context(), corruptRef)
	assert.ErrorIs(t, err, ErrArtifactNotFound,
		"transport enumeration must not invent uncataloged logical artifacts")
}

func TestS3TransportExchangeDetectsDivergentCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	localStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpointRef := onlyTransportRef(t, localStore, origin, KindCheckpoints)
	wire, err := ToWireRef(checkpointRef)
	require.NoError(t, err)

	divergent, err := canonicalJSON(checkpoint{
		Version: formatVersion, Origin: origin, Sequence: 1,
		Sessions: map[string]string{origin + "~other": hashHex([]byte("other"))},
	})
	require.NoError(t, err)
	mock := newMockS3(t, "bucket", 0)
	mock.put("arts/"+origin+"/"+KindCheckpoints+"/"+wire.Name, divergent)

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)

	err = tr.Exchange(context.Background(), localStore)
	require.Error(t, err)
	assert.ErrorIs(t, err, errArtifactPathConflict)
}

func TestS3TransportExchangeRepairsCorruptLocalCheckpoint(t *testing.T) {
	origin := "laptop-a1b2c3"
	baseStore := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-1", "alpha")
	})
	checkpointRef := onlyTransportRef(t, baseStore, origin, KindCheckpoints)
	wire, valid := wireTransportArtifact(t, baseStore, checkpointRef)
	localStore := &corruptUntilRepairedStore{ArtifactStore: baseStore, corruptRef: checkpointRef}

	mock := newMockS3(t, "bucket", 0)
	mock.put("arts/"+origin+"/"+KindCheckpoints+"/"+wire.Name, valid)

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localStore))

	assert.True(t, localStore.repaired, "corrupt local checkpoint should be re-fetched from the bucket")
	_, got := wireTransportArtifact(t, localStore, checkpointRef)
	assert.Equal(t, valid, got)
}

func TestS3TransportWriteOnceRejectsDivergentContent(t *testing.T) {
	mock := newMockS3(t, "bucket", 0)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	ctx := context.Background()
	key := "arts/laptop-a1b2c3/raw/deadbeef"

	// First write creates the object.
	one := []byte("one")
	require.NoError(t, tr.putObject(ctx, key, bytes.NewReader(one), int64(len(one)), hashHex(one)))
	// An identical re-write is an accepted duplicate, not an error.
	require.NoError(t, tr.putObject(ctx, key, bytes.NewReader(one), int64(len(one)), hashHex(one)))
	// Divergent content at the same key is a conflict, never a silent overwrite.
	two := []byte("two")
	err = tr.putObject(ctx, key, bytes.NewReader(two), int64(len(two)), hashHex(two))
	require.Error(t, err)
	assert.ErrorIs(t, err, errObjectStore)
	// The original content is preserved.
	var got []byte
	err = tr.withObject(ctx, key, func(body io.Reader) error {
		var readErr error
		got, readErr = io.ReadAll(body)
		return readErr
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("one"), got)
}

func TestIsObjectTarget(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   bool
	}{
		{"s3 url", "s3://bucket/prefix", true},
		{"s3 bucket only", "s3://bucket", true},
		{"http peer", "http://example.com", false},
		{"https peer", "https://example.com", false},
		{"folder path", "/var/data/share", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsObjectTarget(tt.target))
		})
	}
}

func TestNewObjectTransport(t *testing.T) {
	creds := ObjectStoreOptions{
		Region:          "us-east-1",
		AccessKeyID:     "AK",
		SecretAccessKey: "SK",
	}

	t.Run("missing bucket", func(t *testing.T) {
		_, err := newObjectTransport("s3://", creds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bucket")
	})

	t.Run("missing credentials", func(t *testing.T) {
		_, err := newObjectTransport("s3://bucket/prefix", ObjectStoreOptions{Region: "us-east-1"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "AWS_ACCESS_KEY_ID")
	})

	t.Run("not an object target", func(t *testing.T) {
		_, err := newObjectTransport("https://example.com", creds)
		require.Error(t, err)
	})

	t.Run("parses bucket and prefix", func(t *testing.T) {
		tr, err := newObjectTransport("s3://bucket/some/prefix/", creds)
		require.NoError(t, err)
		assert.Equal(t, "bucket", tr.bucket)
		assert.Equal(t, "some/prefix", tr.prefix)
		assert.Equal(t, "s3.us-east-1.amazonaws.com", tr.endpoint.Host)
		assert.False(t, tr.pathStyle, "real AWS defaults to virtual-host addressing")
	})

	t.Run("custom endpoint forces path style", func(t *testing.T) {
		tr, err := newObjectTransport("s3://bucket", ObjectStoreOptions{
			Endpoint:        "http://localhost:9000",
			Region:          "us-east-1",
			AccessKeyID:     "AK",
			SecretAccessKey: "SK",
		})
		require.NoError(t, err)
		assert.True(t, tr.pathStyle)
		assert.Equal(t, "localhost:9000", tr.endpoint.Host)
		assert.Empty(t, tr.prefix)
	})

	t.Run("rejects insecure remote endpoint by default", func(t *testing.T) {
		options := creds
		options.Endpoint = "http://minio.lan:9000"

		_, err := newObjectTransport("s3://bucket", options)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "insecure S3 endpoint")
		assert.Contains(t, err.Error(), "AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT")
	})

	t.Run("allows opted-in insecure remote endpoint", func(t *testing.T) {
		options := creds
		options.Endpoint = "http://minio.lan:9000"
		options.AllowInsecureEndpoint = true

		tr, err := newObjectTransport("s3://bucket", options)
		require.NoError(t, err)
		assert.Equal(t, "http", tr.endpoint.Scheme)
	})

	for _, endpoint := range []string{
		"http://localhost:9000",
		"http://LOCALHOST:9000",
		"http://127.0.0.1:9000",
		"http://[::1]:9000",
	} {
		t.Run("allows loopback endpoint "+endpoint, func(t *testing.T) {
			options := creds
			options.Endpoint = endpoint

			tr, err := newObjectTransport("s3://bucket", options)
			require.NoError(t, err)
			assert.Equal(t, "http", tr.endpoint.Scheme)
		})
	}

	t.Run("bare host defaults to HTTPS", func(t *testing.T) {
		options := creds
		options.Endpoint = "minio.lan:9000"

		tr, err := newObjectTransport("s3://bucket", options)
		require.NoError(t, err)
		assert.Equal(t, "https", tr.endpoint.Scheme)
	})

	t.Run("rejects unsupported endpoint scheme", func(t *testing.T) {
		options := creds
		options.Endpoint = "ftp://minio.lan"

		_, err := newObjectTransport("s3://bucket", options)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ftp")
	})
}

func TestObjectStoreOptionsFromEnvAllowsInsecureEndpoint(t *testing.T) {
	for _, value := range []string{"1", "true", "yes", "YES"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "AK")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")
			t.Setenv("AGENTSVIEW_S3_ENDPOINT", "http://minio.lan:9000")
			t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", value)

			tr, err := newObjectTransport("s3://bucket", ObjectStoreOptionsFromEnv())
			require.NoError(t, err)
			assert.Equal(t, "http", tr.endpoint.Scheme)
		})
	}
}

func TestObjectStoreOptionsFromEnvRejectsInvalidInsecureEndpointOverride(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "zero", value: "0"},
		{name: "false", value: "false"},
		{name: "no", value: "no"},
		{name: "typo", value: "treu"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AWS_ACCESS_KEY_ID", "AK")
			t.Setenv("AWS_SECRET_ACCESS_KEY", "SK")
			t.Setenv("AGENTSVIEW_S3_ENDPOINT", "http://minio.lan:9000")
			t.Setenv("AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT", tt.value)

			_, err := newObjectTransport("s3://bucket", ObjectStoreOptionsFromEnv())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "insecure S3 endpoint")
			assert.Contains(t, err.Error(), "AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT")
		})
	}
}

func TestS3TransportUploadMemoryRemainsBoundedByBufferSize(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		assert.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)

	measure := func(size int) uint64 {
		body := bytes.Repeat([]byte{'s'}, size)
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
		require.NoError(t, transport.putEntry(t.Context(), store, result.Entry))
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	small := measure(1 << 20)
	large := measure(24 << 20)
	assert.Less(t, large, small+(4<<20),
		"SigV4 upload allocation growth must not scale with artifact bytes")
	assert.Equal(t, int64(2), requests.Load())
}

func TestS3TransportRejectsOversizedMalformedPageWithBoundedMemory(t *testing.T) {
	measure := func(size int64) uint64 {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<ListBucketResult><Contents><Key>`)
			_, _ = io.CopyN(w, repeatedByteReader('x'), size)
		}))
		t.Cleanup(server.Close)
		transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
		require.NoError(t, err)

		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err = transport.listPage(t.Context(), "arts/", "/", "", transportPageSize)
		runtime.ReadMemStats(&after)
		require.ErrorIs(t, err, ErrArtifactInvalid)
		assert.Contains(t, err.Error(), "response exceeds")
		return after.TotalAlloc - before.TotalAlloc
	}

	small := measure(2 << 20)
	large := measure(24 << 20)
	assert.Less(t, large, small+(4<<20),
		"malformed object listing allocation growth must remain bounded")
}

func TestS3TransportExchangeMemoryRemainsBoundedByArtifactSize(t *testing.T) {
	identityForSize := func(size int64) Identity {
		hasher := sha256.New()
		_, err := io.CopyN(hasher, repeatedByteReader('x'), size)
		require.NoError(t, err)
		identity, err := NewIdentity(hex.EncodeToString(hasher.Sum(nil)), size)
		require.NoError(t, err)
		return identity
	}
	newServer := func(t *testing.T, identity Identity, inbound bool) *httptest.Server {
		t.Helper()
		const origin = "peer-a1b2c3"
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
				prefix := r.URL.Query().Get("prefix")
				switch {
				case inbound && r.URL.Query().Get("delimiter") == "/":
					_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
						`<CommonPrefixes><Prefix>arts/`+origin+`/</Prefix></CommonPrefixes></ListBucketResult>`)
				case inbound && prefix == "arts/"+origin+"/raw/":
					_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`+
						`<Contents><Key>`+prefix+identity.SHA256+`</Key></Contents></ListBucketResult>`)
				default:
					_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>`)
				}
				return
			}
			if inbound && r.Method == http.MethodGet {
				w.Header().Set("Content-Length", strconv.FormatInt(identity.Size, 10))
				_, err := io.CopyN(w, repeatedByteReader('x'), identity.Size)
				assert.NoError(t, err)
				return
			}
			if !inbound && r.Method == http.MethodPut {
				_, err := io.Copy(io.Discard, r.Body)
				assert.NoError(t, err)
				w.WriteHeader(http.StatusOK)
				return
			}
			http.NotFound(w, r)
		}))
	}
	measure := func(size int64, inbound bool) uint64 {
		identity := identityForSize(size)
		server := newServer(t, identity, inbound)
		t.Cleanup(server.Close)
		transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
		require.NoError(t, err)
		local := openTransportStore(t, t.TempDir())
		ref, err := NewRef("peer-a1b2c3", KindRaw, identity.SHA256)
		require.NoError(t, err)
		if !inbound {
			_, err = local.Create(t.Context(), ref, identity,
				canonicalArtifactMediaType(ref.Kind), io.LimitReader(repeatedByteReader('x'), size))
			require.NoError(t, err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		require.NoError(t, transport.Exchange(t.Context(), local))
		runtime.ReadMemStats(&after)
		if inbound {
			_, err = local.Stat(t.Context(), ref)
			require.NoError(t, err)
		}
		return after.TotalAlloc - before.TotalAlloc
	}

	smallOut, smallIn := measure(1<<20, false), measure(1<<20, true)
	largeOut, largeIn := measure(24<<20, false), measure(24<<20, true)
	assert.Less(t, largeOut, smallOut+(4<<20),
		"real S3 Exchange outbound allocation growth must remain bounded")
	assert.Less(t, largeIn, smallIn+(4<<20),
		"real S3 Exchange inbound CreateFromWire allocation growth must remain bounded")
}

func TestS3TransportCancellationDuringVerificationSendsNoRequest(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)

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

	err = transport.putEntry(ctx, store, result.Entry)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load(), "verification and hashing must finish before SigV4 request headers are sent")
}

func TestS3TransportCancellationStopsDownloadWithoutPublishing(t *testing.T) {
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
	transport, err := newObjectTransport("s3://bucket/arts", testObjectOptions(server.URL))
	require.NoError(t, err)
	ref, err := NewRef("peer-a1b2c3", KindRaw, strings64("a"))
	require.NoError(t, err)
	wire, err := ToWireRef(ref)
	require.NoError(t, err)
	store := openTransportStore(t, t.TempDir())

	err = transport.receiveObject(ctx, store, wire)
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.Stat(t.Context(), ref)
	require.ErrorIs(t, err, ErrArtifactNotFound)
}
