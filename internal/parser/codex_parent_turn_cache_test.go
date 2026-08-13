package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexParentTurnCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	key := codexParentTurnCacheKeyFor(path, info)
	turns := map[string]struct{}{"opaque-turn": {}}

	cache := newCodexParentTurnCache(1)
	cache.PutParent("root\x00parent", key, turns)
	got, ok := cache.Get(key)
	require.True(t, ok)
	assert.Equal(t, turns, got)
	got, ok = cache.GetParent("root\x00parent")
	require.True(t, ok)
	assert.Equal(t, turns, got)

	other := key
	other.path = filepath.Join(filepath.Dir(path), "other.jsonl")
	cache.Put(other, map[string]struct{}{"other": {}})
	_, kept := cache.Get(key)
	assert.False(t, kept)
	_, kept = cache.GetParent("root\x00parent")
	assert.False(t, kept)
}
