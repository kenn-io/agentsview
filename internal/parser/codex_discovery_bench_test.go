package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkCodexStreamingDiscovery(b *testing.B) {
	root := b.TempDir()
	for i := range 1000 {
		path := filepath.Join(root, fmt.Sprintf("rollout-2026-06-01T10-00-00-00000000-0000-4000-8000-%012d.jsonl", i))
		require.NoError(b, os.WriteFile(path, nil, 0o600))
	}
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(b, ok)
	discoverer := provider.(StreamingDiscoverer)
	b.ReportAllocs()
	for b.Loop() {
		count := 0
		require.NoError(b, discoverer.DiscoverEach(b.Context(), func(SourceRef) error { count++; return nil }))
		require.Equal(b, 1000, count)
	}
}

func TestCodexStreamingDiscoveryExcludesSymlinksAndDirectories(t *testing.T) {
	root := t.TempDir()
	name := "rollout-2026-06-01T10-00-00-00000000-0000-4000-8000-000000000001.jsonl"
	target := filepath.Join(root, name)
	require.NoError(t, os.WriteFile(target, nil, 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "rollout-directory.jsonl"), 0o700))
	if err := os.Symlink(target, filepath.Join(root, "rollout-link.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	var paths []string
	require.NoError(t, provider.(StreamingDiscoverer).DiscoverEach(t.Context(), func(s SourceRef) error { paths = append(paths, s.DisplayPath); return nil }))
	require.Equal(t, []string{target}, paths)
}
