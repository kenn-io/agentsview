package artifact

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// mockS3 is an in-memory, path-style S3-compatible server backing a single
// bucket. It implements just enough of ListObjectsV2, GetObject, and PutObject
// to exercise the object-store transport, and verifies that requests arrive
// signed (Authorization plus x-amz-date) without re-validating the signature.
type mockS3 struct {
	t        *testing.T
	bucket   string
	pageSize int

	mu      sync.Mutex
	objects map[string][]byte
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
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *mockS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
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
		XMLName               xml.Name      `xml:"ListBucketResult"`
		IsTruncated           bool          `xml:"IsTruncated"`
		Contents              []contentsXML `xml:"Contents"`
		NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
	}
	out := resultXML{IsTruncated: truncated}
	for _, k := range keys[start:end] {
		out.Contents = append(out.Contents, contentsXML{Key: k})
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

// exportStore exports one origin's sessions into a fresh artifact store and
// returns the artifact root.
func exportStore(t *testing.T, origin string, seed func(*db.DB)) string {
	t.Helper()
	database := testDB(t)
	seed(database)
	root := filepath.Join(t.TempDir(), "artifacts")
	_, err := Export(context.Background(), database, root, origin)
	require.NoError(t, err)
	return root
}

func TestS3TransportPushRoundTrip(t *testing.T) {
	origin := "laptop-a1b2c3"
	database := testDB(t)
	seedSession(t, database, "sess-1", "alpha")

	dataDir := t.TempDir()
	localRoot := filepath.Join(dataDir, "artifacts")
	_, err := Export(context.Background(), database, localRoot, origin)
	require.NoError(t, err)

	// Append a metadata event so a meta artifact is part of the push.
	rec := NewMetadataRecorder(database, MetadataRecorderOptions{
		DataDir: dataDir,
		Origin:  origin,
		Now:     func() time.Time { return fixedHLCTime() },
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
	require.NoError(t, tr.Prepare(localRoot))
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	idx, err := ListArtifacts(localRoot, origin)
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
	remoteRoot := exportStore(t, origin, func(database *db.DB) {
		seedSession(t, database, "sess-7", "beta")
		seedSession(t, database, "sess-8", "beta")
	})
	remoteIdx, err := ListArtifacts(remoteRoot, origin)
	require.NoError(t, err)
	uploaded := indexItems(remoteIdx)
	require.NotEmpty(t, uploaded)

	// Use a small page size and more than one object to exercise the
	// continuation-token pagination path.
	mock := newMockS3(t, "bucket", 2)
	for _, item := range uploaded {
		art, err := ReadArtifact(remoteRoot, origin, item.kind, item.name)
		require.NoError(t, err)
		mock.put("arts/"+origin+"/"+item.kind+"/"+item.name, art.Data)
	}
	require.Greater(t, len(uploaded), 2, "need multiple pages to test pagination")

	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)

	localRoot := filepath.Join(t.TempDir(), "artifacts")
	tr, err := newObjectTransport("s3://bucket/arts", testObjectOptions(srv.URL))
	require.NoError(t, err)
	require.NoError(t, tr.Exchange(context.Background(), localRoot))

	gotIdx, err := ListArtifacts(localRoot, origin)
	require.NoError(t, err)
	assert.ElementsMatch(t, indexItems(remoteIdx), indexItems(gotIdx))
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
	require.NoError(t, tr.putObject(ctx, key, []byte("one")))
	// An identical re-write is an accepted duplicate, not an error.
	require.NoError(t, tr.putObject(ctx, key, []byte("one")))
	// Divergent content at the same key is a conflict, never a silent overwrite.
	err = tr.putObject(ctx, key, []byte("two"))
	require.Error(t, err)
	assert.ErrorIs(t, err, errObjectStore)
	// The original content is preserved.
	got, err := tr.getObject(ctx, key)
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
}
