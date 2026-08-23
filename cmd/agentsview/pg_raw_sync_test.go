package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		"/api/v1/raw-sync/objects/missing",
		"/api/v1/raw-sync/manifests",
		"/api/v1/raw-sync/uploads",
		"/api/v1/raw-sync/uploads/{upload_id}",
	} {
		assert.Contains(t, spec.Paths, path)
	}
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
