//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenConfiguresThreadCount(t *testing.T) {
	duck, err := Open(filepath.Join(t.TempDir(), "threads.duckdb"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, duck.Close())
	})

	var got string
	require.NoError(t, duck.QueryRowContext(context.Background(), `
		SELECT value FROM duckdb_settings() WHERE name = 'threads'`,
	).Scan(&got))
	assert.Equal(t, strconv.Itoa(duckDBThreadCount()), got)
}

func TestDuckDBThreadCountPositive(t *testing.T) {
	assert.Positive(t, duckDBThreadCount())
}
