//go:build !(windows && arm64)

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	duckdbsync "go.kenn.io/agentsview/internal/duckdb"
)

// These tests open a real DuckDB mirror. duckdb-go-bindings ships no
// prebuilt DuckDB library for windows/arm64, where the backend reports an
// unsupported-platform error instead.

func TestProbeDuckDBMirrorForServeAcceptsCompatibleMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror.duckdb")
	buildEmptyDuckDBMirrorFixture(t, path)

	assert.NoError(t, probeDuckDBMirrorForServe(t.Context(), path))
}

func TestDuckDBServeRuntimeRecordWriteFailureWarnsVisible(t *testing.T) {
	out, err := runDuckDBRuntimeWarningHelper(t)
	require.NoError(t, err, string(out))
	assert.Contains(t, string(out), "could not write daemon runtime record")
}

func runDuckDBRuntimeWarningHelper(t *testing.T) ([]byte, error) {
	t.Helper()
	dataDir := t.TempDir()
	mirrorPath := filepath.Join(dataDir, "mirror.duckdb")
	buildEmptyDuckDBMirrorFixture(t, mirrorPath)
	return runRuntimeWarningHelperProcess(
		t, "DuckDB", "TestRunDuckDBRuntimeWarningHelperProcess",
		[]string{
			"AGENTSVIEW_RUN_DUCKDB_RUNTIME_WARNING_HELPER=1",
			"AGENTSVIEW_DATA_DIR=" + dataDir,
			"AGENTSVIEW_DUCKDB_RUNTIME_WARNING_PATH=" + mirrorPath,
		},
		"could not write daemon runtime record",
	)
}

// buildEmptyDuckDBMirrorFixture creates a schema-compatible, empty DuckDB
// mirror file at path. 'duckdb serve' now probes instead of migrating (see
// probeDuckDBMirrorForServe), so it fatally refuses to serve a missing or
// bare file; tests that just need serve to reach its normal startup path
// must seed a valid mirror first instead of relying on serve to create one.
func buildEmptyDuckDBMirrorFixture(t *testing.T, path string) {
	t.Helper()

	conn, err := duckdbsync.Open(t.Context(), path)
	require.NoError(t, err)
	require.NoError(t, duckdbsync.EnsureSchema(t.Context(), conn))
	require.NoError(t, conn.Close())
}

func TestRunDuckDBRuntimeWarningHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_DUCKDB_RUNTIME_WARNING_HELPER") != "1" {
		return
	}
	writeDaemonRuntimeWithAuth = func(
		string, string, int, string, string, bool, bool, ...int,
	) (string, error) {
		return "", errors.New("forced runtime-record write failure")
	}
	// This is only an orphan guard if the parent dies; normal completion is
	// driven by the parent observing the warning on stdout.
	time.AfterFunc(2*time.Minute, func() { os.Exit(0) })
	runDuckDBServe(config.Config{
		Host:    "127.0.0.1",
		Port:    0,
		DataDir: os.Getenv("AGENTSVIEW_DATA_DIR"),
		DuckDB: config.DuckDBConfig{
			Path: os.Getenv("AGENTSVIEW_DUCKDB_RUNTIME_WARNING_PATH"),
		},
	}, "")
}
