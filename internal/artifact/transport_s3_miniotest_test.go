//go:build miniotest

package artifact

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestS3TransportMinIORoundTrip exercises the object-store transport against a
// real MinIO server in a container, validating that the hand-rolled SigV4
// signing is accepted by a genuine S3 implementation end to end: create bucket,
// push a producer's artifacts, list them, and pull them into a fresh store.
func TestS3TransportMinIORoundTrip(t *testing.T) {
	ctx := context.Background()
	endpoint, accessKey, secretKey := startMinIO(t, ctx)

	tr, err := newObjectTransport("s3://agentsview/sync", ObjectStoreOptions{
		Endpoint:        endpoint,
		Region:          "us-east-1",
		AccessKeyID:     accessKey,
		SecretAccessKey: secretKey,
		PathStyle:       true,
	})
	require.NoError(t, err)
	requireCreateBucket(t, ctx, tr)

	// Producer store: one exported session plus a star metadata event.
	origin := "laptop-a1b2c3"
	prod := testDB(t)
	seedSession(t, prod, "sess-1", "alpha")
	prodDir := t.TempDir()
	prodRoot := filepath.Join(prodDir, "artifacts")
	_, err = Export(ctx, prod, prodRoot, origin)
	require.NoError(t, err)
	recorder := NewMetadataRecorder(prod, MetadataRecorderOptions{DataDir: prodDir, Origin: origin})
	_, err = recorder.Append(ctx, MetadataEventInput{SessionID: "sess-1", Op: MetadataOpStar})
	require.NoError(t, err)

	want, err := ListArtifacts(prodRoot, origin)
	require.NoError(t, err)
	require.NotEmpty(t, want.Manifests)
	require.NotEmpty(t, want.Meta)

	// Push to MinIO, then confirm the bucket lists exactly the producer's set.
	require.NoError(t, tr.Prepare(prodRoot))
	require.NoError(t, tr.Exchange(ctx, prodRoot))

	remote, err := tr.listRemote(ctx)
	require.NoError(t, err)
	require.Contains(t, remote, origin)
	assertIndexEqual(t, want, remote[origin])

	// A fresh consumer store pulls every artifact back.
	consDir := t.TempDir()
	consRoot := filepath.Join(consDir, "artifacts")
	require.NoError(t, os.MkdirAll(consRoot, 0o755))
	require.NoError(t, tr.Exchange(ctx, consRoot))

	got, err := ListArtifacts(consRoot, origin)
	require.NoError(t, err)
	assertIndexEqual(t, want, got)

	// Re-running is a no-op set-union: nothing new to fetch or upload.
	require.NoError(t, tr.Exchange(ctx, consRoot))
	got, err = ListArtifacts(consRoot, origin)
	require.NoError(t, err)
	assertIndexEqual(t, want, got)
}

func assertIndexEqual(t *testing.T, want, got OriginArtifactIndex) {
	t.Helper()
	assert.ElementsMatch(t, want.Checkpoints, got.Checkpoints, "checkpoints")
	assert.ElementsMatch(t, want.Manifests, got.Manifests, "manifests")
	assert.ElementsMatch(t, want.Segments, got.Segments, "segments")
	assert.ElementsMatch(t, want.Meta, got.Meta, "meta")
	assert.ElementsMatch(t, want.Raw, got.Raw, "raw")
}

// requireCreateBucket issues a signed CreateBucket request through the transport,
// reusing the same SigV4 path under test. An already-owned bucket is fine.
func requireCreateBucket(t *testing.T, ctx context.Context, tr *s3Transport) {
	t.Helper()
	var lastStatus string
	for attempt := 0; attempt < 10; attempt++ {
		req, err := tr.newRequest(ctx, http.MethodPut, "", nil, nil)
		require.NoError(t, err)
		resp, err := tr.client.Do(req)
		require.NoError(t, err)
		lastStatus = resp.Status
		status := resp.StatusCode
		resp.Body.Close()
		// 200 = created, 409 = already owned. 503 can still occur briefly while
		// MinIO finishes initializing; retry those.
		if status == http.StatusOK || status == http.StatusConflict {
			return
		}
		if status != http.StatusServiceUnavailable {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.FailNowf(t, "create bucket failed", "last status: %s", lastStatus)
}

func startMinIO(t *testing.T, ctx context.Context) (endpoint, accessKey, secretKey string) {
	t.Helper()
	const user, pass = "minioadmin", "minioadmin"
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "minio/minio:RELEASE.2025-07-23T15-54-02Z",
			ExposedPorts: []string{"9000/tcp"},
			Env: map[string]string{
				"MINIO_ROOT_USER":     user,
				"MINIO_ROOT_PASSWORD": pass,
			},
			Cmd: []string{"server", "/data"},
			// "ready" (not "live") signals MinIO can actually serve S3 requests;
			// "live" only means the process is up and races CreateBucket to 503.
			WaitingFor: wait.ForHTTP("/minio/health/ready").
				WithPort("9000/tcp").
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Skipf("could not start MinIO container (is Docker available?): %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(stopCtx)
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "9000/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, port.Port()), user, pass
}
