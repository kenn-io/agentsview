//go:build pgtest

package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/server"
)

func TestRawSyncStatusPostgresHTTP(t *testing.T) {
	pg := newRawStatusTestDatabase(t, "agentsview_raw_status_http_test")
	ctx := t.Context()
	metadata, err := postgres.NewRawIngestStore(pg)
	require.NoError(t, err)
	authStore, err := postgres.NewRawDeviceAuthStore(pg)
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, time.Hour)
	require.NoError(t, err)

	first, err := auth.EnrollDevice(ctx, "tenant-a", "first device")
	require.NoError(t, err)
	second, err := auth.EnrollDevice(ctx, "tenant-a", "tokenless device")
	require.NoError(t, err)
	revoked, err := auth.EnrollDevice(ctx, "tenant-a", "revoked device")
	require.NoError(t, err)
	other, err := auth.EnrollDevice(ctx, "tenant-b", "other tenant")
	require.NoError(t, err)

	firstIdentity := first.Identity
	otherIdentity := other.Identity
	object := rawStatusObject(t)
	require.NoError(t, metadata.RecordVerifiedObject(ctx, firstIdentity, object))
	firstCommit := commitRawStatusGeneration(
		t, metadata, firstIdentity, object, "capture-one", "", "2026-09-01T00:00:00Z",
	)
	secondCommit := commitRawStatusGeneration(
		t, metadata, firstIdentity, object, "capture-two", firstCommit.Receipt,
		"2026-09-02T00:00:00Z",
	)
	require.NoError(t, metadata.RecordVerifiedObject(ctx, other.Identity, object))
	otherCommit := commitRawStatusGeneration(
		t, metadata, otherIdentity, object, "other-capture", "", "2026-09-03T00:00:00Z",
	)

	insertRawStatusHead(t, pg, firstIdentity, parser.AgentClaude, "root-zero", "zero.jsonl", 0)
	insertRawStatusHead(t, pg, otherIdentity, parser.AgentClaude, "root-other", "other.jsonl", 0)
	insertRawStatusJobs(t, pg, firstIdentity.TenantID, secondCommit.ManifestID)
	insertRawStatusJobs(t, pg, otherIdentity.TenantID, otherCommit.ManifestID)

	statusToken, err := auth.IssueToken(ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)
	secondStatusToken, err := auth.IssueToken(ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)
	firstIssuedAt := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	secondIssuedAt := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	_, err = pg.ExecContext(ctx, `
		UPDATE raw_device_tokens
		SET issued_at = CASE
			WHEN token_sha256 = $1 THEN $2::timestamptz
			WHEN token_sha256 = $3 THEN $4::timestamptz
		END
		WHERE token_sha256 IN ($1, $3)`,
		tokenDigest(statusToken.Token), firstIssuedAt,
		tokenDigest(secondStatusToken.Token), secondIssuedAt)
	require.NoError(t, err)
	revokedToken, err := auth.IssueToken(ctx, revoked.Identity.DeviceID, revoked.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)
	_, err = auth.RevokeDevice(ctx, revoked.Identity)
	require.NoError(t, err)
	otherToken, err := auth.IssueToken(ctx, other.Identity.DeviceID, other.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)
	_ = otherToken

	insertRawStatusUploads(t, pg, firstIdentity, otherIdentity)
	before := readRawStatusPersistence(t, pg)

	srv := server.New(config.Config{
		Host: "127.0.0.1", Port: 0, WriteTimeout: 30 * time.Second,
	}, nil, nil,
		server.WithRawSyncServices(auth, nil),
		server.WithRawSyncStatus(metadata),
	)
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)

	response := rawStatusHTTPGet(t, httpServer.URL, statusToken.Token)
	assert.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assertRawStatusJSONShape(t, body)

	var got rawsync.Status
	require.NoError(t, json.Unmarshal(body, &got))
	require.Len(t, got.SourceHeads, 2)
	current := findRawStatusHead(t, got.SourceHeads, "current.jsonl")
	assert.Equal(t, first.Identity.DeviceID, current.DeviceID)
	assert.Equal(t, "root-a", current.ConfiguredRootID)
	assert.Equal(t, parser.AgentCodex, current.Provider)
	assert.Equal(t, int64(2), current.Generation)
	assert.NotNil(t, current.LastAcceptedAt)
	var wantAcceptedAt time.Time
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT accepted_at FROM raw_manifests
		WHERE tenant_id = $1 AND manifest_id = $2`,
		firstIdentity.TenantID, secondCommit.ManifestID).Scan(&wantAcceptedAt))
	assert.Equal(t, wantAcceptedAt.UTC(), current.LastAcceptedAt.UTC())
	assert.True(t, current.ParsePending)
	assert.True(t, current.ParseLeased)
	assert.True(t, current.ParseFailed)
	zero := findRawStatusHead(t, got.SourceHeads, "zero.jsonl")
	assert.Equal(t, int64(0), zero.Generation)
	assert.Nil(t, zero.LastAcceptedAt)
	assert.False(t, zero.ParsePending)
	assert.False(t, zero.ParseLeased)
	assert.False(t, zero.ParseFailed)

	assert.Equal(t, rawsync.ParseJobCounts{
		Ready: 1, Leased: 1, Retrying: 1,
		Complete: 1, Failed: 1, Superseded: 2,
	}, got.ParseJobs)
	assert.Equal(t, int64(2), got.ActiveDeviceCount)
	devices := make(map[string]rawsync.DeviceStatus, len(got.Devices))
	for _, device := range got.Devices {
		devices[device.DeviceID] = device
	}
	require.Contains(t, devices, first.Identity.DeviceID)
	require.Contains(t, devices, second.Identity.DeviceID)
	assert.NotNil(t, devices[first.Identity.DeviceID].LastSeenAt)
	var wantLastSeenAt time.Time
	require.NoError(t, pg.QueryRowContext(ctx, `
		SELECT MAX(issued_at) FROM raw_device_tokens
		WHERE tenant_id = $1 AND device_id = $2`,
		firstIdentity.TenantID, firstIdentity.DeviceID).Scan(&wantLastSeenAt))
	assert.Equal(t, wantLastSeenAt.UTC(), devices[first.Identity.DeviceID].LastSeenAt.UTC())
	assert.Nil(t, devices[second.Identity.DeviceID].LastSeenAt)
	assert.NotContains(t, devices, revoked.Identity.DeviceID)

	assert.Equal(t, int64(3), got.Uploads.OpenCount)
	assert.Equal(t, int64(30), got.Uploads.PendingBytes)
	require.NotNil(t, got.Uploads.OldestOpenSession)
	assert.Equal(t, "upload-tie-a", got.Uploads.OldestOpenSession.UploadID)
	assert.Equal(t, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		got.Uploads.OldestOpenSession.CreatedAt)

	after := readRawStatusPersistence(t, pg)
	assert.Equal(t, before, after, "status reads must not mutate raw metadata")

	wrongScope, err := auth.IssueToken(ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeCommit)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized,
		rawStatusHTTPGet(t, httpServer.URL, wrongScope.Token).StatusCode)
	assert.Equal(t, http.StatusUnauthorized,
		rawStatusHTTPGet(t, httpServer.URL, revokedToken.Token).StatusCode)

	expired, err := auth.IssueToken(ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)
	issuedAt := time.Now().UTC().Add(-2 * time.Hour)
	_, err = pg.ExecContext(ctx, `
		UPDATE raw_device_tokens
		SET issued_at = $1, expires_at = $2
		WHERE token_sha256 = $3`, issuedAt, issuedAt.Add(time.Hour), tokenDigest(expired.Token))
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized,
		rawStatusHTTPGet(t, httpServer.URL, expired.Token).StatusCode)
}

func TestRawSyncStatusPostgresEmptyHTTP(t *testing.T) {
	pg := newRawStatusTestDatabase(t, "agentsview_raw_status_empty_test")
	ctx := t.Context()
	metadata, err := postgres.NewRawIngestStore(pg)
	require.NoError(t, err)
	authStore, err := postgres.NewRawDeviceAuthStore(pg)
	require.NoError(t, err)
	auth, err := rawsync.NewDeviceAuthService(authStore, time.Hour)
	require.NoError(t, err)
	enrollment, err := auth.EnrollDevice(ctx, "tenant-empty", "tokenless device")
	require.NoError(t, err)

	srv := server.New(config.Config{Host: "127.0.0.1", Port: 0}, nil, nil,
		server.WithRawSyncServices(&rawStatusAuthStub{identity: enrollment.Identity}, nil),
		server.WithRawSyncStatus(metadata),
	)
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)

	response := rawStatusHTTPGet(t, httpServer.URL, "avdt_test")
	require.Equal(t, http.StatusOK, response.StatusCode)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	assertRawStatusJSONShape(t, body)
	var got rawsync.Status
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Empty(t, got.SourceHeads)
	assert.NotNil(t, got.SourceHeads)
	assert.Equal(t, rawsync.ParseJobCounts{}, got.ParseJobs)
	assert.Equal(t, int64(1), got.ActiveDeviceCount)
	require.Len(t, got.Devices, 1)
	assert.Equal(t, enrollment.Identity.DeviceID, got.Devices[0].DeviceID)
	assert.Nil(t, got.Devices[0].LastSeenAt)
	assert.Empty(t, got.Uploads.OpenCount)
	assert.Zero(t, got.Uploads.PendingBytes)
	assert.Nil(t, got.Uploads.OldestOpenSession)
}

func TestRawSyncStatusRollsBackAfterQueryFailure(t *testing.T) {
	pg := newRawStatusTestDatabase(t, "agentsview_raw_status_failure_test")
	pg.SetMaxOpenConns(1)
	metadata, err := postgres.NewRawIngestStore(pg)
	require.NoError(t, err)
	identity := rawsync.AuthIdentity{TenantID: "tenant-failure", DeviceID: "dev-failure"}

	_, err = pg.ExecContext(t.Context(),
		`ALTER TABLE raw_ingest_jobs RENAME TO raw_ingest_jobs_missing`)
	require.NoError(t, err)
	status, err := metadata.ReadRawSyncStatus(t.Context(), identity)
	require.Error(t, err)
	assert.Equal(t, rawsync.Status{}, status)
	checkCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, renameErr := pg.ExecContext(checkCtx,
		`ALTER TABLE raw_ingest_jobs_missing RENAME TO raw_ingest_jobs`)
	require.NoError(t, renameErr)

	status, err = metadata.ReadRawSyncStatus(checkCtx, identity)
	require.NoError(t, err)
	assert.Empty(t, status.SourceHeads)
	assert.Empty(t, status.Devices)
}

func newRawStatusTestDatabase(t *testing.T, schema string) *sql.DB {
	t.Helper()
	pgURL := os.Getenv("TEST_PG_URL")
	if pgURL == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	dropRawStatusSchema(t, pgURL, schema)
	pg, err := postgres.Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { dropRawStatusSchema(t, pgURL, schema) })
	t.Cleanup(func() { require.NoError(t, pg.Close()) })
	require.NoError(t, postgres.EnsureSchema(t.Context(), pg, schema))
	return pg
}

func dropRawStatusSchema(t *testing.T, pgURL, schema string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err)
	defer pg.Close()
	_, err = pg.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	require.NoError(t, err)
}

func rawStatusObject(t *testing.T) rawsync.ObjectRef {
	t.Helper()
	digest := sha256.Sum256([]byte("raw status object"))
	object, err := rawsync.NewObjectRef(hex.EncodeToString(digest[:]), 17)
	require.NoError(t, err)
	return object
}

func commitRawStatusGeneration(
	t *testing.T,
	store *postgres.RawIngestStore,
	identity rawsync.AuthIdentity,
	object rawsync.ObjectRef,
	captureID string,
	parentReceipt string,
	capturedAt string,
) rawsync.CommitResult {
	t.Helper()
	captured, err := time.Parse(time.RFC3339, capturedAt)
	require.NoError(t, err)
	manifest, err := rawsync.ValidateAndCanonicalize(identity, rawsync.Manifest{
		SchemaVersion:         rawsync.ManifestSchemaVersion,
		Provider:              parser.AgentCodex,
		ConfiguredRootID:      "root-a",
		SourceKey:             "current.jsonl",
		ExpectedParentReceipt: parentReceipt,
		CaptureID:             captureID,
		CapturedAt:            captured,
		Kind:                  rawsync.ManifestSnapshot,
		Entries: []rawsync.Entry{{
			Path: "current.jsonl", Type: "file", Length: object.Length,
			Objects: []rawsync.ObjectRef{object},
		}},
	}, rawsync.DefaultManifestLimits())
	require.NoError(t, err)
	result, err := store.CommitManifest(t.Context(), manifest, "status-test-version")
	require.NoError(t, err)
	return result
}

func insertRawStatusHead(
	t *testing.T,
	pg *sql.DB,
	identity rawsync.AuthIdentity,
	provider parser.AgentType,
	rootID, sourceKey string,
	generation int64,
) {
	t.Helper()
	_, err := pg.ExecContext(t.Context(), `
		INSERT INTO raw_source_heads (
			tenant_id, device_id, provider, configured_root_id, source_key,
			source_key_sha256, generation
		) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		identity.TenantID, identity.DeviceID, provider, rootID, sourceKey,
		rawStatusSourceKeyDigest(sourceKey), generation)
	require.NoError(t, err)
}

func insertRawStatusJobs(t *testing.T, pg *sql.DB, tenantID, manifestID string) {
	t.Helper()
	for _, job := range []struct {
		version string
		state   string
	}{
		{version: "leased-version", state: "leased"},
		{version: "retrying-version", state: "retrying"},
		{version: "complete-version", state: "complete"},
		{version: "failed-version", state: "failed"},
		{version: "superseded-version", state: "superseded"},
	} {
		_, err := pg.ExecContext(t.Context(), `
			INSERT INTO raw_ingest_jobs (
				tenant_id, manifest_id, stage, processing_version, state
			) VALUES ($1, $2, 'parse', $3, $4)`,
			tenantID, manifestID, job.version, job.state)
		require.NoError(t, err)
	}
}

func insertRawStatusUploads(
	t *testing.T,
	pg *sql.DB,
	identity, other rawsync.AuthIdentity,
) {
	t.Helper()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	insert := func(
		id string,
		owner rawsync.AuthIdentity,
		size, offset int64,
		state string,
		createdAt, expiresAt time.Time,
		completedAt *time.Time,
	) {
		t.Helper()
		_, err := pg.ExecContext(t.Context(), `
			INSERT INTO raw_upload_sessions (
				upload_id, tenant_id, device_id, provider, sha256, size_bytes,
				offset_bytes, generation, state, created_at, updated_at,
				expires_at, completed_at
			) VALUES ($1, $2, $3, 'codex', $4, $5, $6, 0, $7, $8, $8, $9, $10)`,
			id, owner.TenantID, owner.DeviceID, strings.Repeat("a", 64),
			size, offset, state, createdAt, expiresAt, completedAt)
		require.NoError(t, err)
	}
	insert("upload-tie-b", identity, 10, 2, "open", created, created.Add(time.Hour), nil)
	insert("upload-tie-a", identity, 20, 5, "open", created, created.Add(time.Hour), nil)
	insert("upload-expired", identity, 12, 5, "open", created.Add(time.Hour), created.Add(2*time.Hour), nil)
	completedAt := created.Add(4 * time.Hour)
	insert("upload-complete", identity, 10, 10, "complete", created.Add(3*time.Hour), created.Add(4*time.Hour), &completedAt)
	insert("other-upload", other, 100, 0, "open", created, created.Add(time.Hour), nil)
}

type rawStatusPersistence struct {
	Devices, Tokens, Heads, Jobs, Uploads int
	ExpiredState                          string
	ExpiredOffset                         int64
}

func readRawStatusPersistence(t *testing.T, pg *sql.DB) rawStatusPersistence {
	t.Helper()
	var snapshot rawStatusPersistence
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT
			(SELECT count(*) FROM raw_devices),
			(SELECT count(*) FROM raw_device_tokens),
			(SELECT count(*) FROM raw_source_heads),
			(SELECT count(*) FROM raw_ingest_jobs),
			(SELECT count(*) FROM raw_upload_sessions),
			(SELECT state FROM raw_upload_sessions WHERE upload_id = 'upload-expired'),
			(SELECT offset_bytes FROM raw_upload_sessions WHERE upload_id = 'upload-expired')
	`).Scan(
		&snapshot.Devices, &snapshot.Tokens, &snapshot.Heads, &snapshot.Jobs,
		&snapshot.Uploads, &snapshot.ExpiredState, &snapshot.ExpiredOffset,
	))
	return snapshot
}

func rawStatusHTTPGet(t *testing.T, baseURL, token string) *http.Response {
	t.Helper()
	request, err := http.NewRequestWithContext(
		t.Context(), http.MethodGet, baseURL+"/api/v1/raw-sync/status", nil,
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	return response
}

func assertRawStatusJSONShape(t *testing.T, body []byte) {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &object))
	assert.ElementsMatch(t,
		[]string{"source_heads", "parse_jobs", "active_device_count", "devices", "uploads"},
		mapKeys(object),
	)
}

func mapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func findRawStatusHead(
	t *testing.T,
	heads []rawsync.SourceHeadStatus,
	sourceKey string,
) rawsync.SourceHeadStatus {
	t.Helper()
	for _, head := range heads {
		if head.SourceKey == sourceKey {
			return head
		}
	}
	require.FailNow(t, "source head not found", sourceKey)
	return rawsync.SourceHeadStatus{}
}

func rawStatusSourceKeyDigest(sourceKey string) string {
	digest := sha256.Sum256([]byte(sourceKey))
	return hex.EncodeToString(digest[:])
}

func tokenDigest(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

type rawStatusAuthStub struct {
	identity rawsync.AuthIdentity
}

func (s *rawStatusAuthStub) AuthenticateCredential(
	_ context.Context,
	_ string,
	_ string,
) (rawsync.AuthIdentity, error) {
	return s.identity, nil
}

func (s *rawStatusAuthStub) IssueToken(
	_ context.Context,
	_ string,
	_ string,
	_ rawsync.DeviceTokenScope,
) (rawsync.IssuedDeviceToken, error) {
	return rawsync.IssuedDeviceToken{}, errors.New("not used")
}

func (s *rawStatusAuthStub) AuthenticateToken(
	_ context.Context,
	_ string,
	required rawsync.DeviceTokenScope,
) (rawsync.AuthIdentity, error) {
	if required != rawsync.ScopeStatus {
		return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
	}
	return s.identity, nil
}

var _ server.RawSyncDeviceAuth = (*rawStatusAuthStub)(nil)
