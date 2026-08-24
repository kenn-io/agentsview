//go:build pgtest

// Package postgres_test carries the raw-client end-to-end proof as an
// external test package: internal/server imports internal/postgres, so an
// internal test file cannot import the server package without an import
// cycle. Everything the exercise needs from the store layer is exported;
// only the tiny pgtest conveniences (schema wipe, digest helper, table
// counts, spool paths) are restated here.
package postgres_test

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawclient"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/server"
)

// pgE2ESchema mirrors the internal pgtest tests' shared schema. The test
// must stay serial with them: each wipes this schema on entry and exit.
const pgE2ESchema = "agentsview_schema_test"

// pgE2ESpoolDirectory and pgE2EStageName mirror the RawUploadStore's private
// spool layout below the data directory.
const (
	pgE2ESpoolDirectory = "raw-upload-spool"
	pgE2EStageSuffix    = ".part"
)

func pgE2EStageName(uploadID string) string {
	return uploadID + pgE2EStageSuffix
}

// newPGE2ETestDatabase opens the shared test schema wiped clean, with every
// raw-sync table provisioned, and returns a throwaway data directory.
func newPGE2ETestDatabase(t *testing.T) (*sql.DB, string) {
	t.Helper()
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	pgE2ECleanSchema(t, pgURL)
	t.Cleanup(func() { pgE2ECleanSchema(t, pgURL) })
	pg, err := postgres.Open(pgURL, pgE2ESchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, postgres.EnsureSchema(t.Context(), pg, pgE2ESchema))
	return pg, t.TempDir()
}

func pgE2ECleanSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + pgE2ESchema + " CASCADE")
	require.NoError(t, err, "dropping test schema")
}

// TestRawClientPostgresEndToEnd drives the full rawclient cycle — enrollment,
// token exchange, missing-object negotiation, resumable multi-chunk uploads,
// manifest commits, interrupted-upload resume, and stale-parent head
// conflicts — against the real huma routes with every rawsync persistence
// seam durable in PostgreSQL: upload sessions and offsets in
// raw_upload_sessions plus the staging spool, custody metadata in the raw
// ingest tables, and device identities and tokens in raw_devices and
// raw_device_tokens. Only the artifact repository uses a throwaway directory.
func TestRawClientPostgresEndToEnd(t *testing.T) {
	pg, dataDir := newPGE2ETestDatabase(t)
	ctx := t.Context()

	repository, err := artifact.OpenRepository(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, repository.Close()) })
	objects, err := rawsync.NewArtifactObjectStore(repository.Content())
	require.NoError(t, err)

	ingest, err := postgres.NewRawIngestStore(pg)
	require.NoError(t, err)
	authStore, err := postgres.NewRawDeviceAuthStore(pg)
	require.NoError(t, err)
	sessions, err := postgres.NewRawUploadStore(pg, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sessions.Close()) })

	custody, err := rawsync.NewService(
		objects, ingest, rawsync.DefaultManifestLimits(), "parser-data-17",
	)
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, 15*time.Minute)
	require.NoError(t, err)
	uploads, err := rawsync.NewUploadService(
		sessions, custody, rawsync.DefaultUploadSessionTTL,
	)
	require.NoError(t, err)

	srv := server.New(config.Config{
		Host: "127.0.0.1", Port: 0, WriteTimeout: 30 * time.Second,
	}, nil, nil,
		server.WithRawSyncServices(auth, custody),
		server.WithRawSyncUploads(uploads),
	)
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)

	wire := newPGE2EPatchTransport(http.DefaultTransport)
	httpClient := &http.Client{Transport: wire}

	// Server-side enrollment has no HTTP route by design; the issued
	// credential is exactly what a laptop would be provisioned with, and it
	// lands durably in raw_devices.
	enrollment, err := auth.EnrollDevice(ctx, "tenant-pg-e2e", "laptop-pg-e2e")
	require.NoError(t, err)
	assert.Equal(t, "tenant-pg-e2e", enrollment.Identity.TenantID)
	assert.NotEmpty(t, enrollment.Identity.DeviceID)
	assert.NotEmpty(t, enrollment.Credential)

	var enrolledDevices int
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT count(*) FROM raw_devices
		WHERE tenant_id = $1 AND device_id = $2`,
		enrollment.Identity.TenantID, enrollment.Identity.DeviceID,
	).Scan(&enrolledDevices))
	assert.Equal(t, 1, enrolledDevices, "enrollment persisted in PostgreSQL")

	// A small ChunkBytes forces every object through multiple PATCHes.
	newLaptop := func() *rawclient.Client {
		client, err := rawclient.NewClient(rawclient.Config{
			BaseURL:     httpServer.URL,
			DeviceID:    enrollment.Identity.DeviceID,
			Credential:  enrollment.Credential,
			HTTPClient:  httpClient,
			ChunkBytes:  8,
			TokenMargin: time.Minute,
		})
		require.NoError(t, err)
		return client
	}
	client := newLaptop()

	// Step 1: negotiate missing objects, then upload both through the
	// resumable data plane backed by the PostgreSQL session store.
	firstBody := []byte("first source object body, chunked")
	secondBody := []byte("second source object body")
	first := pgE2EObjectFor(t, firstBody)
	second := pgE2EObjectFor(t, secondBody)
	require.Greater(t, first.Length, int64(16))
	require.Greater(t, second.Length, int64(16))

	objs := []rawsync.ObjectRef{first, second}
	missing, err := client.MissingObjects(ctx, parser.AgentClaude, objs)
	require.NoError(t, err)
	require.Len(t, missing, 2)
	assert.Equal(t, objs, missing, "both objects missing in request order")

	bodies := map[string][]byte{first.SHA256: firstBody, second.SHA256: secondBody}
	for _, object := range missing {
		require.NoError(t, client.UploadObject(
			ctx, parser.AgentClaude, object, bytes.NewReader(bodies[object.SHA256]),
		))
	}

	// Multi-chunk transfers actually happened over the wire: every resumable
	// session needed more than one PATCH at the 8-byte chunk size.
	patches, _ := wire.snapshot()
	require.Len(t, patches, 2, "one resumable session per object")
	for uploadID, count := range patches {
		assert.GreaterOrEqual(t, count, int64(2),
			"upload %s used multiple chunks", uploadID)
	}

	// After custody the durable ledger holds both objects; renegotiation
	// against the PostgreSQL metadata store is empty.
	missing, err = client.MissingObjects(ctx, parser.AgentClaude, objs)
	require.NoError(t, err)
	assert.Empty(t, missing)
	assert.Equal(t, 2, pgE2ETableCount(t, pg, "raw_objects"))

	var issuedTokens int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT count(*) FROM raw_device_tokens`,
	).Scan(&issuedTokens))
	assert.GreaterOrEqual(t, issuedTokens, 1,
		"the token exchange persisted digest-only tokens")

	// Step 2: commit the first generation with an empty expected parent
	// receipt, replay it idempotently, then chain a second generation.
	const (
		rootID    = "root-pg-e2e"
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

	firstCommit, err := client.CommitManifest(ctx, manifestFor(
		"capture-pg-e2e-1", capturedAt, "",
	))
	require.NoError(t, err)
	assert.NotEmpty(t, firstCommit.ManifestID)
	assert.NotEmpty(t, firstCommit.Receipt)
	assert.Equal(t, int64(1), firstCommit.Generation)
	assert.True(t, firstCommit.Created)
	assert.Equal(t, pgE2EIngestCounts{
		Manifests: 1, Entries: 2, Objects: 2, Heads: 1, Jobs: 1,
	}, pgE2EReadIngestCounts(t, pg), "first generation is durable in PostgreSQL")

	// The same capture_id replays idempotently: same receipt and
	// generation, never a second generation or a third metadata row set.
	replayed, err := client.CommitManifest(ctx, manifestFor(
		"capture-pg-e2e-1", capturedAt, "",
	))
	require.NoError(t, err)
	assert.Equal(t, firstCommit.Receipt, replayed.Receipt)
	assert.Equal(t, firstCommit.Generation, replayed.Generation)
	assert.False(t, replayed.Created)
	assert.Equal(t, pgE2EIngestCounts{
		Manifests: 1, Entries: 2, Objects: 2, Heads: 1, Jobs: 1,
	}, pgE2EReadIngestCounts(t, pg), "replay wrote nothing new")

	secondCommit, err := client.CommitManifest(ctx, manifestFor(
		"capture-pg-e2e-2", capturedAt.Add(time.Minute), firstCommit.Receipt,
	))
	require.NoError(t, err)
	assert.NotEqual(t, firstCommit.Receipt, secondCommit.Receipt)
	assert.Equal(t, int64(2), secondCommit.Generation)
	assert.True(t, secondCommit.Created)

	durableHead := func() (manifestID, receipt string, generation int64) {
		require.NoError(t, pg.QueryRowContext(ctx, `
			SELECT manifest_id, receipt, generation FROM raw_source_heads`,
		).Scan(&manifestID, &receipt, &generation))
		return manifestID, receipt, generation
	}
	headManifest, headReceipt, headGeneration := durableHead()
	assert.Equal(t, secondCommit.ManifestID, headManifest)
	assert.Equal(t, secondCommit.Receipt, headReceipt)
	assert.Equal(t, int64(2), headGeneration)
	assert.Equal(t, pgE2EIngestCounts{
		Manifests: 2, Entries: 4, Objects: 4, Heads: 1, Jobs: 2,
	}, pgE2EReadIngestCounts(t, pg), "second generation is durable in PostgreSQL")

	// Step 3: simulate an interrupted upload. The laptop dies after two
	// 8-byte chunks; the durable session and its spooled bytes survive in
	// PostgreSQL and below dataDir while the object stays incomplete.
	resumeBody := bytes.Repeat([]byte("raw-pg-e2e"), 4) // 40 bytes, 5 chunks
	resumeObject := pgE2EObjectFor(t, resumeBody)

	crashed := &pgE2ECrashReader{data: resumeBody, failAt: 16}
	err = client.UploadObject(ctx, parser.AgentClaude, resumeObject, crashed)
	require.Error(t, err)
	assert.ErrorContains(t, err, "simulated laptop crash")

	var durableOffset int64
	var openUploadID string
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT upload_id, offset_bytes FROM raw_upload_sessions
		WHERE state = 'open' AND sha256 = $1 AND size_bytes = $2`,
		resumeObject.SHA256, resumeObject.Length,
	).Scan(&openUploadID, &durableOffset))
	assert.Equal(t, int64(16), durableOffset,
		"the two durable chunks survived the crash in PostgreSQL")
	require.Greater(t, durableOffset, int64(0))
	require.Less(t, durableOffset, resumeObject.Length)

	staged, err := os.Stat(filepath.Join(
		dataDir, pgE2ESpoolDirectory, pgE2EStageName(openUploadID),
	))
	require.NoError(t, err)
	assert.Equal(t, durableOffset, staged.Size(),
		"the spool holds exactly the durable prefix")

	// A fresh client — new token cache, no in-memory session state —
	// re-uploads the same object. It must resume the durable PostgreSQL
	// session, not restart it: the start-response offset it acts on equals
	// the durable offset, it reads only the missing tail, and only that
	// tail travels over the wire.
	beforePatches, beforeSent := wire.snapshot()
	resumeClient := newLaptop()
	resumed := &pgE2ERecordingReader{data: resumeBody}
	require.NoError(t, resumeClient.UploadObject(
		ctx, parser.AgentClaude, resumeObject, resumed,
	))
	assert.Equal(t, durableOffset, resumed.firstOffset,
		"the fresh client's first read sat at the server's resumed offset")
	assert.Equal(t, resumeObject.Length-durableOffset, resumed.bytesRead,
		"the fresh client read only the missing tail")

	afterPatches, afterSent := wire.snapshot()
	var resumeUploadID string
	var resumeWireBytes, resumePatchCount int64
	for uploadID, sent := range afterSent {
		delta := sent - beforeSent[uploadID]
		if delta == 0 {
			continue
		}
		require.Empty(t, resumeUploadID,
			"only the resumed upload transferred new bytes")
		resumeUploadID = uploadID
		resumeWireBytes = delta
		resumePatchCount = afterPatches[uploadID] - beforePatches[uploadID]
	}
	require.NotEmpty(t, resumeUploadID)
	assert.Equal(t, openUploadID, resumeUploadID,
		"the fresh client reused the interrupted session's upload ID")
	assert.Equal(t, resumeObject.Length-durableOffset, resumeWireBytes,
		"the resume re-sent only the missing bytes")
	assert.GreaterOrEqual(t, resumePatchCount, int64(3),
		"the resumed tail itself is multi-chunk")

	// Completion is durable: custody recorded the third object, the open
	// session is gone, and the spooled stage file was cleaned up.
	missing, err = resumeClient.MissingObjects(
		ctx, parser.AgentClaude, []rawsync.ObjectRef{resumeObject},
	)
	require.NoError(t, err)
	assert.Empty(t, missing)
	assert.Equal(t, 3, pgE2ETableCount(t, pg, "raw_objects"))

	var openSessions int
	require.NoError(t, pg.QueryRowContext(ctx,
		`SELECT count(*) FROM raw_upload_sessions WHERE state = 'open'`,
	).Scan(&openSessions))
	assert.Zero(t, openSessions)

	_, err = os.Stat(filepath.Join(
		dataDir, pgE2ESpoolDirectory, pgE2EStageName(openUploadID),
	))
	assert.True(t, os.IsNotExist(err), "the completed session's spool was removed")

	// Step 4: a stale parent receipt is rejected as a typed head conflict
	// carrying the current durable head, and the conflict advances nothing.
	_, err = client.CommitManifest(ctx, manifestFor(
		"capture-pg-e2e-3", capturedAt.Add(2*time.Minute), firstCommit.Receipt,
	))
	require.Error(t, err)
	var apiErr rawclient.APIError
	require.True(t, rawclient.AsAPIError(err, &apiErr))
	assert.Equal(t, http.StatusConflict, apiErr.Status)
	assert.Equal(t, rawclient.CodeHeadConflict, apiErr.Code)
	assert.Equal(t, secondCommit.ManifestID, apiErr.CurrentManifestID)
	assert.Equal(t, secondCommit.Receipt, apiErr.CurrentReceipt)
	assert.Equal(t, int64(2), apiErr.CurrentGeneration)

	headManifest, headReceipt, headGeneration = durableHead()
	assert.Equal(t, secondCommit.ManifestID, headManifest)
	assert.Equal(t, secondCommit.Receipt, headReceipt)
	assert.Equal(t, int64(2), headGeneration)
	assert.Equal(t, pgE2EIngestCounts{
		Manifests: 2, Entries: 4, Objects: 4, Heads: 1, Jobs: 2,
	}, pgE2EReadIngestCounts(t, pg), "the rejected stale commit wrote no metadata")
}

// pgE2EObjectFor builds a validated ObjectRef carrying body's real digest.
func pgE2EObjectFor(t *testing.T, body []byte) rawsync.ObjectRef {
	t.Helper()
	digest := sha256.Sum256(body)
	object, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), int64(len(body)))
	require.NoError(t, err)
	return object
}

type pgE2EIngestCounts struct {
	Manifests int
	Entries   int
	Objects   int
	Heads     int
	Jobs      int
}

// pgE2EReadIngestCounts reads the raw ingest metadata row counts.
func pgE2EReadIngestCounts(t *testing.T, pg *sql.DB) pgE2EIngestCounts {
	t.Helper()
	var counts pgE2EIngestCounts
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT
			(SELECT count(*) FROM raw_manifests),
			(SELECT count(*) FROM raw_manifest_entries),
			(SELECT count(*) FROM raw_manifest_objects),
			(SELECT count(*) FROM raw_source_heads),
			(SELECT count(*) FROM raw_ingest_jobs)`,
	).Scan(&counts.Manifests, &counts.Entries, &counts.Objects,
		&counts.Heads, &counts.Jobs))
	return counts
}

// pgE2ETableCount counts rows in one raw-sync table.
func pgE2ETableCount(t *testing.T, pg *sql.DB, table string) int {
	t.Helper()
	var count int
	require.NoError(t, pg.QueryRowContext(t.Context(),
		"SELECT count(*) FROM "+table,
	).Scan(&count))
	return count
}

const pgE2EUploadsPathPrefix = "/api/v1/raw-sync/uploads/"

// pgE2EPatchTransport counts resumable-upload PATCHes per upload ID and the
// body bytes each carried, so the test can observe multi-chunk transfers and
// prove a resumed session never re-sends bytes the server already holds.
type pgE2EPatchTransport struct {
	mu      sync.Mutex
	base    http.RoundTripper
	patches map[string]int64
	sent    map[string]int64
}

func newPGE2EPatchTransport(base http.RoundTripper) *pgE2EPatchTransport {
	return &pgE2EPatchTransport{
		base:    base,
		patches: make(map[string]int64),
		sent:    make(map[string]int64),
	}
}

func (t *pgE2EPatchTransport) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	if req.Method == http.MethodPatch &&
		strings.HasPrefix(req.URL.Path, pgE2EUploadsPathPrefix) {
		uploadID := path.Base(req.URL.Path)
		t.mu.Lock()
		t.patches[uploadID]++
		t.sent[uploadID] += req.ContentLength
		t.mu.Unlock()
	}
	return t.base.RoundTrip(req)
}

func (t *pgE2EPatchTransport) snapshot() (patches, sent map[string]int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	patches = make(map[string]int64, len(t.patches))
	sent = make(map[string]int64, len(t.sent))
	for uploadID, count := range t.patches {
		patches[uploadID] = count
	}
	for uploadID, sentBytes := range t.sent {
		sent[uploadID] = sentBytes
	}
	return patches, sent
}

// pgE2ECrashReader stands in for a laptop that died mid-transfer: reads at or
// beyond failAt fail, exactly as if the process vanished before it could
// hand the client the next chunk.
type pgE2ECrashReader struct {
	data   []byte
	failAt int64
}

func (r *pgE2ECrashReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.failAt {
		return 0, fmt.Errorf("pg e2e: simulated laptop crash before offset %d", off)
	}
	n := copy(p, r.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// pgE2ERecordingReader records the first offset the client asked to read and
// the total bytes it read — the client-side view of the server's
// start-response offset for a resumed session.
type pgE2ERecordingReader struct {
	data        []byte
	firstOffset int64
	started     bool
	bytesRead   int64
}

func (r *pgE2ERecordingReader) ReadAt(p []byte, off int64) (int, error) {
	if !r.started {
		r.started = true
		r.firstOffset = off
	}
	n := copy(p, r.data[off:])
	r.bytesRead += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
