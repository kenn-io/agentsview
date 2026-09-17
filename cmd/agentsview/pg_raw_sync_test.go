package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/rawsync"
	"go.kenn.io/agentsview/internal/server"
)

func TestPreparePGRawSyncServicesRegistersHostedRoutes(t *testing.T) {
	t.Parallel()

	database := newEmptyRawUploadTestDB(t)
	option, cleanup, err := preparePGRawSyncServices(t.Context(), t.TempDir(), database)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })

	spec := server.OpenAPISpec(server.VersionInfo{}, option)
	for _, path := range []string{
		"/api/v1/raw-sync/tokens",
		"/api/v1/raw-sync/status",
		"/api/v1/raw-sync/health",
		"/api/v1/raw-sync/objects/missing",
		"/api/v1/raw-sync/manifests",
		"/api/v1/raw-sync/uploads",
		"/api/v1/raw-sync/uploads/{upload_id}",
	} {
		assert.Contains(t, spec.Paths, path)
	}
}

func TestPreparePGRawSyncServicesWiresRuntimeStatusRoute(t *testing.T) {
	t.Parallel()

	database := newEmptyRawUploadTestDB(t)
	option, cleanup, err := preparePGRawSyncServices(t.Context(), t.TempDir(), database)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })

	srv := server.New(config.Config{
		Host:         "127.0.0.1",
		Port:         8080,
		AuthToken:    "legacy-shared-token",
		RequireAuth:  true,
		WriteTimeout: 30 * time.Second,
	}, nil, nil, option)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/raw-sync/status", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Authorization", "Bearer legacy-shared-token")
	srv.Handler().ServeHTTP(recorder, request)

	assert.Equal(t, http.StatusUnauthorized, recorder.Code, recorder.Body.String())
}

func TestPreparePGRawSyncServicesHealthDoesNotOpenRepository(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repositoryParent := filepath.Join(dataDir, pgRawSyncDataDirectory)
	contents := []byte("occupied")
	require.NoError(t, os.WriteFile(repositoryParent, contents, 0o600))

	option, cleanup, err := preparePGRawSyncServices(
		t.Context(), dataDir, newRawSyncHealthTestDB(t),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })

	srv := server.New(config.Config{
		Host: "127.0.0.1", Port: 8080, RequireAuth: true,
		WriteTimeout: 30 * time.Second,
	}, nil, nil, option, server.WithRawSyncServices(rawSyncHealthAuthStub{}, nil))
	statusToken := "avdt_" + strings.Repeat("a", 43)
	request := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/raw-sync/health?max_attempts=5&stale_after_seconds=1",
		nil,
	)
	request.Header.Set("Authorization", "Bearer "+statusToken)
	response := httptest.NewRecorder()
	srv.Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Equal(t, contents, func() []byte {
		got, readErr := os.ReadFile(repositoryParent)
		require.NoError(t, readErr)
		return got
	}())
}

func TestPreparePGRawSyncServicesSkipsReadOnlySchema(t *testing.T) {
	t.Parallel()

	option, cleanup, err := preparePGRawSyncServicesIfWritable(
		t.Context(), "", nil, false,
	)

	require.NoError(t, err)
	assert.Nil(t, option)
	assert.Nil(t, cleanup)
}

func TestPreparePGRawSyncServicesDoesNotOpenArtifactSyncRepository(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repositoryPath := filepath.Join(dataDir, "artifacts")
	require.NoError(t, os.WriteFile(repositoryPath, []byte("occupied"), 0o600))

	_, cleanup, err := preparePGRawSyncServices(
		t.Context(), dataDir, newEmptyRawUploadTestDB(t),
	)
	require.NoError(t, err)
	require.NoError(t, cleanup())
}

func TestPreparePGRawSyncServicesDefersRawRepositoryOpen(t *testing.T) {
	t.Parallel()

	dataDir := t.TempDir()
	repositoryParent := filepath.Join(dataDir, pgRawSyncDataDirectory)
	require.NoError(t, os.WriteFile(repositoryParent, []byte("occupied"), 0o600))

	_, cleanup, err := preparePGRawSyncServices(
		t.Context(), dataDir, newEmptyRawUploadTestDB(t),
	)
	require.NoError(t, err)
	require.NoError(t, cleanup())
}

var registerEmptyRawUploadDriver sync.Once
var registerRawSyncHealthDriver sync.Once

func newEmptyRawUploadTestDB(t *testing.T) *sql.DB {
	t.Helper()
	registerEmptyRawUploadDriver.Do(func() {
		sql.Register("empty-raw-upload-cleanup", emptyRawUploadDriver{})
	})
	database, err := sql.Open("empty-raw-upload-cleanup", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database
}

func newRawSyncHealthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	registerRawSyncHealthDriver.Do(func() {
		sql.Register("raw-sync-health", rawSyncHealthDriver{})
	})
	database, err := sql.Open("raw-sync-health", "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	return database
}

type emptyRawUploadDriver struct{}

func (emptyRawUploadDriver) Open(string) (driver.Conn, error) {
	return emptyRawUploadConnection{}, nil
}

type emptyRawUploadConnection struct{}

func (emptyRawUploadConnection) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (emptyRawUploadConnection) Close() error { return nil }

func (emptyRawUploadConnection) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }

func (emptyRawUploadConnection) QueryContext(
	context.Context,
	string,
	[]driver.NamedValue,
) (driver.Rows, error) {
	return emptyRawUploadRows{}, nil
}

type emptyRawUploadRows struct{}

func (emptyRawUploadRows) Columns() []string { return []string{"upload_id"} }

func (emptyRawUploadRows) Close() error { return nil }

func (emptyRawUploadRows) Next([]driver.Value) error { return io.EOF }

var _ driver.QueryerContext = emptyRawUploadConnection{}

type rawSyncHealthDriver struct{}

func (rawSyncHealthDriver) Open(string) (driver.Conn, error) {
	return rawSyncHealthConnection{}, nil
}

type rawSyncHealthConnection struct{}

func (rawSyncHealthConnection) Prepare(string) (driver.Stmt, error) {
	return nil, driver.ErrSkip
}

func (rawSyncHealthConnection) Close() error { return nil }

func (rawSyncHealthConnection) Begin() (driver.Tx, error) { return nil, driver.ErrSkip }

func (rawSyncHealthConnection) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "FROM raw_device_tokens"):
		return &rawSyncHealthRows{
			columns: []string{"tenant_id", "device_id"},
			values:  []driver.Value{"tenant-a", "device-a"},
		}, nil
	case strings.Contains(query, "WITH snapshot AS MATERIALIZED"):
		return &rawSyncHealthRows{
			columns: []string{
				"observed_at", "orphaned_count", "orphaned_json",
				"expired_count", "expired_json", "failed_count",
				"failed_json", "retrying_count", "retrying_json",
				"stale_count", "stale_json",
			},
			values: []driver.Value{
				time.Now().UTC(), int64(0), "[]",
				int64(0), "[]", int64(0),
				"[]", int64(0), "[]",
				int64(0), "[]",
			},
		}, nil
	default:
		return emptyRawUploadRows{}, nil
	}
}

type rawSyncHealthRows struct {
	columns []string
	values  []driver.Value
	read    bool
}

func (r *rawSyncHealthRows) Columns() []string { return r.columns }

func (r *rawSyncHealthRows) Close() error { return nil }

func (r *rawSyncHealthRows) Next(dest []driver.Value) error {
	if r.read {
		return io.EOF
	}
	copy(dest, r.values)
	r.read = true
	return nil
}

var _ driver.QueryerContext = rawSyncHealthConnection{}

type rawSyncHealthAuthStub struct{}

func (rawSyncHealthAuthStub) AuthenticateCredential(
	context.Context, string, string,
) (rawsync.AuthIdentity, error) {
	return rawsync.AuthIdentity{}, rawsync.ErrUnauthorized
}

func (rawSyncHealthAuthStub) IssueToken(
	context.Context, string, string, rawsync.DeviceTokenScope,
) (rawsync.IssuedDeviceToken, error) {
	return rawsync.IssuedDeviceToken{}, rawsync.ErrUnauthorized
}

func (rawSyncHealthAuthStub) AuthenticateToken(
	context.Context,
	string,
	rawsync.DeviceTokenScope,
) (rawsync.AuthIdentity, error) {
	return rawsync.AuthIdentity{TenantID: "tenant-a", DeviceID: "device-a"}, nil
}

var _ server.RawSyncDeviceAuth = rawSyncHealthAuthStub{}
