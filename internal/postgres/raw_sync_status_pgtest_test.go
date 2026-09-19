//go:build pgtest

package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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
	pg, _ := newPGE2ETestDatabase(t)
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
	object, err := rawsync.NewObjectRef(
		"98627d5753b568650fce01e540e4b7d3a394cb56a4d922dc19ca4d0439771c98", 17,
	)
	require.NoError(t, err)
	require.NoError(t, metadata.RecordVerifiedObject(ctx, firstIdentity, object))
	firstCommit := commitRawStatusGeneration(
		t, metadata, firstIdentity, object, rawsync.Manifest{
			CaptureID:  "capture-one",
			CapturedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	)
	secondCommit := commitRawStatusGeneration(
		t, metadata, firstIdentity, object, rawsync.Manifest{
			CaptureID:             "capture-two",
			ExpectedParentReceipt: firstCommit.Receipt,
			CapturedAt:            time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		},
	)
	require.NoError(t, metadata.RecordVerifiedObject(ctx, other.Identity, object))
	otherCommit := commitRawStatusGeneration(
		t, metadata, otherIdentity, object, rawsync.Manifest{
			CaptureID:  "other-capture",
			CapturedAt: time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC),
		},
	)

	for _, head := range []struct {
		identity  rawsync.AuthIdentity
		root, key string
		digest    string
	}{
		{
			firstIdentity, "root-zero", "zero.jsonl",
			"b6c74c2ec57f6feb02d16fd167a327849a93b4e5617a4fa1ac48fb23df99e5d5",
		},
		{
			otherIdentity, "root-other", "other.jsonl",
			"9a3bc4527dfb421f6eaaa4bb005aa24bb699e5fe372470a88d93be5914f65563",
		},
	} {
		_, err := pg.ExecContext(ctx, `
			INSERT INTO raw_source_heads (
				tenant_id, device_id, provider, configured_root_id, source_key,
				source_key_sha256, generation
			) VALUES ($1, $2, 'claude', $3, $4, $5, 0)`,
			head.identity.TenantID, head.identity.DeviceID, head.root, head.key, head.digest)
		require.NoError(t, err)
	}
	insertRawStatusJobs(t, pg, firstIdentity.TenantID, secondCommit.ManifestID)
	insertRawStatusJobs(t, pg, otherIdentity.TenantID, otherCommit.ManifestID)

	statusToken, err := auth.IssueToken(
		ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeStatus,
	)
	require.NoError(t, err)
	secondStatusToken, err := auth.IssueToken(
		ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeStatus,
	)
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
	revokedToken, err := auth.IssueToken(
		ctx, revoked.Identity.DeviceID, revoked.Credential, rawsync.ScopeStatus,
	)
	require.NoError(t, err)
	_, err = auth.RevokeDevice(ctx, revoked.Identity)
	require.NoError(t, err)
	_, err = auth.IssueToken(ctx, other.Identity.DeviceID, other.Credential, rawsync.ScopeStatus)
	require.NoError(t, err)

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
	require.NotNil(t, current.LastAcceptedAt)
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
	require.NotNil(t, devices[first.Identity.DeviceID].LastSeenAt)
	assert.Equal(t, secondIssuedAt, *devices[first.Identity.DeviceID].LastSeenAt)
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

	for _, tc := range []struct {
		state                   string
		pending, leased, failed bool
	}{
		{state: "ready", pending: true},
		{state: "retrying", pending: true},
		{state: "leased", leased: true},
		{state: "failed", failed: true},
		{state: "complete"},
		{state: "superseded"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			_, err := pg.ExecContext(t.Context(), `
				UPDATE raw_ingest_jobs SET state = $1
				WHERE tenant_id = $2 AND manifest_id = $3`,
				tc.state, firstIdentity.TenantID, secondCommit.ManifestID)
			require.NoError(t, err)
			status, err := metadata.ReadRawSyncStatus(t.Context(), firstIdentity)
			require.NoError(t, err)
			head := findRawStatusHead(t, status.SourceHeads, "current.jsonl")
			assert.Equal(t, tc.pending, head.ParsePending)
			assert.Equal(t, tc.leased, head.ParseLeased)
			assert.Equal(t, tc.failed, head.ParseFailed)
		})
	}

	wrongScope, err := auth.IssueToken(
		ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeCommit,
	)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized,
		rawStatusHTTPGet(t, httpServer.URL, wrongScope.Token).StatusCode)
	assert.Equal(t, http.StatusUnauthorized,
		rawStatusHTTPGet(t, httpServer.URL, revokedToken.Token).StatusCode)

	expired, err := auth.IssueToken(
		ctx, first.Identity.DeviceID, first.Credential, rawsync.ScopeStatus,
	)
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
	pg, _ := newPGE2ETestDatabase(t)
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
	pg, _ := newPGE2ETestDatabase(t)
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
	assert.Zero(t, status.ActiveDeviceCount)
}

func commitRawStatusGeneration(
	t *testing.T,
	store *postgres.RawIngestStore,
	identity rawsync.AuthIdentity,
	object rawsync.ObjectRef,
	manifest rawsync.Manifest,
) rawsync.CommitResult {
	t.Helper()
	manifest.SchemaVersion = rawsync.ManifestSchemaVersion
	manifest.Provider = parser.AgentCodex
	manifest.ConfiguredRootID = "root-a"
	manifest.SourceKey = "current.jsonl"
	manifest.Kind = rawsync.ManifestSnapshot
	manifest.Entries = []rawsync.Entry{{
		Path: "current.jsonl", Type: "file", Length: object.Length,
		Objects: []rawsync.ObjectRef{object},
	}}
	canonical, err := rawsync.ValidateAndCanonicalize(
		identity, manifest, rawsync.DefaultManifestLimits(),
	)
	require.NoError(t, err)
	result, err := store.CommitManifest(t.Context(), canonical, "status-test-version")
	require.NoError(t, err)
	return result
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
	completedAt := created.Add(4 * time.Hour)
	for _, upload := range []struct {
		id           string
		owner        rawsync.AuthIdentity
		size, offset int64
		state        string
		createdAt    time.Time
		completedAt  *time.Time
	}{
		{
			id: "upload-tie-b", owner: identity, size: 10, offset: 2,
			state: "open", createdAt: created,
		},
		{
			id: "upload-tie-a", owner: identity, size: 20, offset: 5,
			state: "open", createdAt: created,
		},
		{
			id: "upload-expired", owner: identity, size: 12, offset: 5,
			state: "open", createdAt: created.Add(time.Hour),
		},
		{
			id: "upload-complete", owner: identity, size: 10, offset: 10,
			state: "complete", createdAt: created.Add(3 * time.Hour), completedAt: &completedAt,
		},
		{
			id: "other-upload", owner: other, size: 100,
			state: "open", createdAt: created,
		},
	} {
		_, err := pg.ExecContext(t.Context(), `
			INSERT INTO raw_upload_sessions (
				upload_id, tenant_id, device_id, provider, sha256, size_bytes,
				offset_bytes, generation, state, created_at, updated_at,
				expires_at, completed_at
			) VALUES ($1, $2, $3, 'codex', $4, $5, $6, 0, $7, $8, $8, $9, $10)`,
			upload.id, upload.owner.TenantID, upload.owner.DeviceID, strings.Repeat("a", 64),
			upload.size, upload.offset, upload.state, upload.createdAt,
			upload.createdAt.Add(time.Hour), upload.completedAt)
		require.NoError(t, err)
	}
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
	t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
	return response
}

func assertRawStatusJSONShape(t *testing.T, body []byte) {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &object))
	assert.ElementsMatch(t,
		[]string{"source_heads", "parse_jobs", "active_device_count", "devices", "uploads"},
		slices.Collect(maps.Keys(object)),
	)
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
