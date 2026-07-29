package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDuckDB_ExpandsLeadingTildePath(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	cfg := Config{
		DuckDB: DuckDBConfig{
			Path: "~/mirror.duckdb",
		},
	}

	resolved, err := cfg.ResolveDuckDB()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "mirror.duckdb"), resolved.Path)
}

func TestResolveDuckDB_DoesNotExpandMidStringTilde(t *testing.T) {
	cfg := Config{
		DuckDB: DuckDBConfig{
			Path: "path/with~marker.duckdb",
		},
	}

	resolved, err := cfg.ResolveDuckDB()
	require.NoError(t, err)
	assert.Equal(t, "path/with~marker.duckdb", resolved.Path)
}
