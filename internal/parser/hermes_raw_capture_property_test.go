//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package parser

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Source identity belongs to the home, even when transcript names/content change.
func TestHermesRawCapturePropertyStableFileOnlyIdentity(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		alternate := hegel.Draw(ht, hegel.Booleans())
		home := ht.TempDir()
		dir := filepath.Join(home, "sessions")
		require.NoError(ht, os.Mkdir(dir, 0o700))
		encoded, err := json.Marshal(map[string]string{"role": "user", "content": text})
		require.NoError(ht, err)
		first := filepath.Join(dir, "first.jsonl")
		require.NoError(ht, os.WriteFile(first, append(encoded, '\n'), 0o600))
		root := home
		if alternate {
			root = dir
		}
		p, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{root}})
		require.True(ht, ok)
		before, err := DiscoverRawCaptureSources(ht.Context(), p)
		require.NoError(ht, err)
		require.True(ht, before.Complete)
		require.Len(ht, before.Sources, 1)
		require.NoError(ht, os.Rename(first, filepath.Join(dir, "last.jsonl")))
		after, err := DiscoverRawCaptureSources(ht.Context(), p)
		require.NoError(ht, err)
		require.True(ht, after.Complete)
		require.Len(ht, after.Sources, 1)
		assert.Equal(ht, before.Sources[0].Key, after.Sources[0].Key)
	}, hegel.WithTestCases(1000))
}
