package parser

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countWorkspaceManifestReads swaps the workspace-manifest seam with a counter
// for the duration of the test and returns a pointer to the call count.
func countWorkspaceManifestReads(t *testing.T) *int {
	t.Helper()
	var calls int
	orig := readVSCodeWorkspaceManifest
	readVSCodeWorkspaceManifest = func(dir string) string {
		calls++
		return orig(dir)
	}
	t.Cleanup(func() { readVSCodeWorkspaceManifest = orig })
	return &calls
}

func writeWorkspaceManifestSessions(t *testing.T, root string, ids ...string) {
	t.Helper()
	hashDir := filepath.Join(root, "workspaceStorage", "ws-hash")
	chatDir := filepath.Join(hashDir, "chatSessions")
	writeSourceFile(t, filepath.Join(hashDir, "workspace.json"),
		`{"folder":"file:///Users/alice/code/myproj"}`)
	for _, id := range ids {
		writeSourceFile(t, filepath.Join(chatDir, id+".jsonl"),
			vscodeCopilotProviderJSONL(id, "hi"))
	}
}

// TestVSCodeCopilotDiscoverReadsWorkspaceManifestOncePerDir guards against
// re-reading workspace.json for every discovered session. The manifest depends
// only on the workspace hash dir, so reading it per source made discovery scale
// with session count.
func TestVSCodeCopilotDiscoverReadsWorkspaceManifestOncePerDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	writeWorkspaceManifestSessions(t, root, "s1", "s2", "s3")

	calls := countWorkspaceManifestReads(t)
	provider, ok := NewProvider(AgentVSCodeCopilot, ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	require.True(ok)

	srcs, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(srcs, 3)
	for _, s := range srcs {
		assert.Equal("myproj", s.ProjectHint)
	}
	assert.Equal(1, *calls,
		"workspace.json should be read once per workspace dir, not per session")
}

// TestPositronDiscoverReadsWorkspaceManifestOncePerDir is the Positron
// counterpart: Positron reuses the VSCode workspaceStorage layout and the same
// manifest reader, and had the same per-source rebuild.
func TestPositronDiscoverReadsWorkspaceManifestOncePerDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	writeWorkspaceManifestSessions(t, root, "s1", "s2", "s3")

	calls := countWorkspaceManifestReads(t)
	provider, ok := NewProvider(AgentPositron, ProviderConfig{
		Roots: []string{root}, Machine: "local",
	})
	require.True(ok)

	srcs, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(srcs, 3)
	for _, s := range srcs {
		assert.Equal("myproj", s.ProjectHint)
	}
	assert.Equal(1, *calls,
		"workspace.json should be read once per workspace dir, not per session")
}
