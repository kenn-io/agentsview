//go:build (linux && !android && (amd64 || arm64)) || (darwin && !ios && arm64) || (windows && (amd64 || arm64))

package parser

import (
	"encoding/hex"
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

// Two actual homes never share a physical source even with identical inputs.
func TestHermesRawCapturePropertyProfilesDoNotMerge(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		// Bound only filesystem materialization; all generated text reaches encoding.
		encoded := hex.EncodeToString([]byte(name))
		if len(encoded) > 80 {
			encoded = encoded[:80]
		}
		container := filepath.Join(ht.TempDir(), ".hermes", "profiles")
		for _, prefix := range []string{"a-", "b-"} {
			dir := filepath.Join(container, prefix+encoded, "sessions")
			require.NoError(ht, os.MkdirAll(dir, 0o700))
			require.NoError(ht, os.WriteFile(filepath.Join(dir, "same.jsonl"), []byte("{}\n"), 0o600))
		}
		p, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{container}})
		require.True(ht, ok)
		d, err := DiscoverRawCaptureSources(ht.Context(), p)
		require.NoError(ht, err)
		require.True(ht, d.Complete)
		require.Len(ht, d.Sources, 2)
		assert.NotEqual(ht, d.Sources[0].Key, d.Sources[1].Key)
	}, hegel.WithTestCases(1000))
}

// A broken sibling archive cannot block a changed profile's raw source.
func TestHermesRawCapturePropertyRoutingIgnoresSiblingHealth(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		name := hex.EncodeToString([]byte(hegel.Draw(ht, hegel.Text())))
		if len(name) > 80 {
			name = name[:80]
		}
		base, err := filepath.EvalSymlinks(ht.TempDir())
		require.NoError(ht, err)
		container := filepath.Join(base, ".hermes", "profiles")
		home := filepath.Join(container, "target-"+name)
		dir := filepath.Join(home, "sessions")
		require.NoError(ht, os.MkdirAll(dir, 0o700))
		file := filepath.Join(dir, "example.jsonl")
		require.NoError(ht, os.WriteFile(file, []byte("{}\n"), 0o600))
		other := filepath.Join(container, "unrelated")
		require.NoError(ht, os.Mkdir(other, 0o700))
		require.NoError(ht, os.WriteFile(filepath.Join(other, "sessions"), []byte("invalid directory"), 0o600))
		p, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{container}})
		require.True(ht, ok)
		sources, err := RawCaptureSourcesForChangedPath(ht.Context(), p, ChangedPathRequest{Path: file, WatchRoot: container})
		require.NoError(ht, err)
		require.Len(ht, sources, 1)
		assert.Equal(ht, filepath.Join(home, "state.db"), sources[0].Key)
	}, hegel.WithTestCases(1000))
}
