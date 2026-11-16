//go:build !duckdb || (windows && arm64)

package duckdb

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestOpenReportsDuckDBUnavailable(t *testing.T) {
	db, err := Open(t.Context(), "sessions.duckdb")
	require.Error(t, err)
	assert.Nil(t, db)
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestNewQuackStoreReportsDuckDBUnavailable(t *testing.T) {
	store, err := NewQuackStore(t.Context(), "quack:localhost:8765", "token", false, 0)
	require.Error(t, err)
	assert.Nil(t, store)
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestPushWithoutDriverPreservesUnfinishedRebuild(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			local := dbtest.OpenTestDB(t)
			path := filepath.Join(t.TempDir(), "mirror.duckdb")
			workDir := path + ".agentsview-work"
			require.NoError(t, os.Mkdir(workDir, 0o700))
			files := map[string]string{
				"mirror.duckdb.rebuild":     "saved rebuild progress",
				"mirror.duckdb.rebuild.wal": "saved uncheckpointed progress",
			}
			for name, content := range files {
				require.NoError(t, os.WriteFile(filepath.Join(workDir, name), []byte(content), 0o600))
			}

			_, err := Push(t.Context(), path, local, "test-machine", storage.MirrorPushOptions{}, full, nil)
			assert.ErrorIs(t, err, ErrUnavailable)
			if full {
				var notStarted storage.PushNotStartedError
				assert.ErrorAs(t, err, &notStarted)
			}
			for name, content := range files {
				got, err := os.ReadFile(filepath.Join(workDir, name))
				assert.NoError(t, err)
				assert.Equal(t, content, string(got))
			}
		})
	}
}
