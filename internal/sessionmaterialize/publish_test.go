package sessionmaterialize

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishPreservesExistingFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "project", "session.jsonl")
	created, err := publishFiles(t.Context(), []installFile{{p, []byte("first")}})
	require.NoError(t, err)
	assert.True(t, created)
	created, err = publishFiles(t.Context(), []installFile{{p, []byte("first")}})
	require.NoError(t, err)
	assert.False(t, created)
	_, err = publishFiles(t.Context(), []installFile{{p, []byte("different")}})
	require.Error(t, err)
	contents, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "first", string(contents))
}

func TestPublishCancelledBeforeWriting(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	name := filepath.Join(t.TempDir(), "primary")
	_, err := publishFiles(ctx, []installFile{{name, []byte("data")}})
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(name)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPublishPreflightsAllDestinations(t *testing.T) {
	dir := t.TempDir()
	primary := filepath.Join(dir, "primary")
	dependency := filepath.Join(dir, "dependency")
	require.NoError(t, os.WriteFile(primary, []byte("existing"), 0o600))
	_, err := publishFiles(t.Context(), []installFile{{dependency, []byte("tool")}, {primary, []byte("other")}})
	require.Error(t, err)
	_, err = os.Stat(dependency)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPublishRejectsOversizedAndEqualLengthConflicts(t *testing.T) {
	name := filepath.Join(t.TempDir(), "destination")
	f, err := os.Create(name)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1<<20))
	require.NoError(t, f.Close())
	_, err = publishFiles(t.Context(), []installFile{{name, []byte("tiny")}})
	require.ErrorContains(t, err, "destination differs")
	info, err := os.Stat(name)
	require.NoError(t, err)
	assert.EqualValues(t, 1<<20, info.Size())
	require.NoError(t, os.WriteFile(name, []byte("else"), 0o600))
	_, err = publishFiles(t.Context(), []installFile{{name, []byte("tiny")}})
	require.ErrorContains(t, err, "destination differs")
}
