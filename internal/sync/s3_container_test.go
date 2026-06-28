//go:build s3test

// Package sync's S3 container integration test. It exercises the real S3
// discovery+sync path -- listS3Objects -> s3Client -> a live S3-compatible
// object store -- rather than stubbing the seam, so a regression that silently
// drops s3:// discovery (as the provider migration once did) cannot pass.
//
// Gated behind the s3test build tag and Docker, mirroring the pgtest setup. Run
// with:
//
//	CGO_ENABLED=1 go test -tags "fts5,s3test" ./internal/sync/ -run TestS3 -v
//
// The container image is rustfs, an actively maintained S3-compatible object
// store; any S3-compatible image works by swapping s3ContainerImage and its
// credential env vars.
package sync

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	s3ContainerImage = "rustfs/rustfs:latest"
	s3TestAccessKey  = "rustfsadmin"
	s3TestSecretKey  = "rustfsadmin"
	s3TestBucket     = "agentsview"
	// s3TestMachine is the machine segment of the /<machine>/raw/<provider>
	// layout; discovery derives the session machine namespace from it.
	s3TestMachine = "laptop"
)

// startS3Container boots a throwaway S3-compatible object store and returns its
// host:port endpoint. The container is terminated on test cleanup.
func startS3Container(ctx context.Context, t *testing.T) string {
	t.Helper()
	req := testcontainers.ContainerRequest{
		Image:        s3ContainerImage,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"RUSTFS_ACCESS_KEY": s3TestAccessKey,
			"RUSTFS_SECRET_KEY": s3TestSecretKey,
		},
		// rustfs serves the S3 API on :9000 and answers /health with 200 once
		// the object store is ready; the image's default entrypoint starts the
		// server, so no Cmd override is needed.
		WaitingFor: wait.ForHTTP("/health").
			WithPort("9000/tcp").
			WithStartupTimeout(90 * time.Second),
	}
	container, err := testcontainers.GenericContainer(
		ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: req,
			Started:          true,
		},
	)
	require.NoError(t, err, "start S3 container")
	t.Cleanup(func() {
		// Use a fresh context so cleanup runs even if the test ctx is done.
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	require.NoError(t, err, "container host")
	port, err := container.MappedPort(ctx, "9000")
	require.NoError(t, err, "container mapped port")
	return fmt.Sprintf("%s:%s", host, port.Port())
}

// putS3Object uploads body to s3://<bucket>/<key> using a direct client.
func putS3Object(
	ctx context.Context, t *testing.T, cl *minio.Client, key, body string,
) {
	t.Helper()
	_, err := cl.PutObject(
		ctx, s3TestBucket, key,
		bytes.NewReader([]byte(body)), int64(len(body)),
		minio.PutObjectOptions{ContentType: "application/jsonl"},
	)
	require.NoError(t, err, "put object %s", key)
}

// TestS3DiscoverySyncAgainstContainer uploads a Claude session and a Codex
// rollout to a live S3-compatible store, points the production s3Client at it
// via the standard AWS_* env vars, and runs a full provider-authoritative
// SyncAll against s3:// roots. It asserts both remote sessions are discovered,
// fetched, parsed, and persisted machine-namespaced under the s3 root's machine
// segment -- the end-to-end path that has no other real-store coverage.
func TestS3DiscoverySyncAgainstContainer(t *testing.T) {
	ctx := context.Background()
	endpoint := startS3Container(ctx, t)

	// Point the production s3Client (env-driven) at the container. The endpoint
	// is loopback (127.0.0.1), so http is allowed without the insecure override.
	t.Setenv("AWS_S3_ENDPOINT", "http://"+endpoint)
	t.Setenv("AWS_ACCESS_KEY_ID", s3TestAccessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", s3TestSecretKey)
	t.Setenv("AWS_REGION", "us-east-1")

	uploadClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3TestAccessKey, s3TestSecretKey, ""),
		Secure: false,
	})
	require.NoError(t, err, "build upload client")
	require.NoError(t, uploadClient.MakeBucket(
		ctx, s3TestBucket, minio.MakeBucketOptions{},
	), "make bucket")

	const (
		claudeID = "11111111-1111-4111-8111-111111111111"
		codexID  = "22222222-2222-4222-8222-222222222222"
	)
	claudeKey := fmt.Sprintf(
		"%s/raw/claude/myproj/%s.jsonl", s3TestMachine, claudeID,
	)
	claudeBody := testjsonl.NewSessionBuilder().
		AddClaudeUser("2024-01-01T00:00:00Z", "hello from claude s3").
		AddClaudeAssistant("2024-01-01T00:00:05Z", "hi there").
		String()
	putS3Object(ctx, t, uploadClient, claudeKey, claudeBody)

	codexKey := fmt.Sprintf(
		"%s/raw/codex/2026/06/24/rollout-2026-06-24T00-00-00-%s.jsonl",
		s3TestMachine, codexID,
	)
	codexBody := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(
			codexID, "/home/coder/project", "user", "2026-06-24T00:00:00Z",
		),
		testjsonl.CodexMsgJSON("user", "review this from s3", "2026-06-24T00:00:01Z"),
	)
	putS3Object(ctx, t, uploadClient, codexKey, codexBody)

	database := openTestDB(t)
	engine := NewEngine(database, EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {"s3://" + s3TestBucket + "/" + s3TestMachine + "/raw/claude"},
			parser.AgentCodex:  {"s3://" + s3TestBucket + "/" + s3TestMachine + "/raw/codex"},
		},
		Machine: "central",
	})

	stats := engine.SyncAll(ctx, nil)
	require.GreaterOrEqual(t, stats.Synced, 2,
		"both s3 sessions discovered and synced")

	// The Claude session ID is the object's filename stem, namespaced by the s3
	// root's machine ("laptop"), not the host machine ("central").
	claudeSess, err := database.GetSessionFull(ctx, s3TestMachine+"~"+claudeID)
	require.NoError(t, err)
	require.NotNil(t, claudeSess, "claude s3 session persisted")
	assert.Equal(t, s3TestMachine, claudeSess.Machine)
	assert.Equal(t, "s3://"+s3TestBucket+"/"+claudeKey, derefString(claudeSess.FilePath))

	// Codex namespaces its ID differently (machine~codex:<id>); assert via the
	// persisted set so the exact format is not hard-coded.
	page, err := database.ListSessions(ctx, db.SessionFilter{Limit: 100})
	require.NoError(t, err)
	agents := map[string]bool{}
	for _, s := range page.Sessions {
		assert.Equal(t, s3TestMachine, s.Machine,
			"every synced s3 session is namespaced under the s3 root machine")
		agents[s.Agent] = true
	}
	assert.True(t, agents["claude"], "claude s3 session discovered")
	assert.True(t, agents["codex"], "codex s3 session discovered")
}
