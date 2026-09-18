package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestIflowProviderDiscoversProjects(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	// Create a temporary directory structure for testing
	tmpDir := t.TempDir()

	// Create project directories
	proj1 := filepath.Join(tmpDir, "project1")
	proj2 := filepath.Join(tmpDir, "project2")

	require.NoError(os.MkdirAll(proj1, 0o755))
	require.NoError(os.MkdirAll(proj2, 0o755))

	// Create session files in project1
	session1 := filepath.Join(proj1, "session-abc123.jsonl")
	session2 := filepath.Join(proj1, "session-def456.jsonl")

	require.NoError(os.WriteFile(session1, []byte(`{"test":"data"}`), 0o644))
	require.NoError(os.WriteFile(session2, []byte(`{"test":"data"}`), 0o644))

	// Create a session file in project2
	session3 := filepath.Join(proj2, "session-ghi789.jsonl")
	require.NoError(os.WriteFile(session3, []byte(`{"test":"data"}`), 0o644))

	// Create a non-session file (should be ignored)
	otherFile := filepath.Join(proj1, "other.txt")
	require.NoError(os.WriteFile(otherFile, []byte(`not a session`), 0o644))

	// Create a directory (should be ignored)
	subDir := filepath.Join(proj1, "subdir")
	require.NoError(os.MkdirAll(subDir, 0o755))

	provider, ok := parser.NewProvider(parser.AgentIflow, parser.ProviderConfig{
		Roots: []string{tmpDir},
	})
	require.True(ok)

	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	assert.Len(sources, 3)

	// Verify source paths
	paths := make(map[string]bool)
	for _, s := range sources {
		paths[s.DisplayPath] = true
	}

	assert.True(paths[session1], "session1 not found in results")
	assert.True(paths[session2], "session2 not found in results")
	assert.True(paths[session3], "session3 not found in results")
	assert.False(paths[otherFile], "other.txt should not be in results")

	// Verify project hints
	projects := make(map[string]bool)
	for _, s := range sources {
		projects[s.ProjectHint] = true
	}

	assert.True(projects["project1"], "project1 not found in projects")
	assert.True(projects["project2"], "project2 not found in projects")

	// Verify provider type
	for _, s := range sources {
		assert.Equal(parser.AgentIflow, s.Provider)
	}
}

func TestIflowProviderFindsSourceFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tmpDir := t.TempDir()

	// Create a project directory
	proj := filepath.Join(tmpDir, "test-project")
	require.NoError(os.MkdirAll(proj, 0o755))

	// Create a session file
	sessionID := "abc123-def456"
	sessionFile := filepath.Join(proj, "session-"+sessionID+".jsonl")
	require.NoError(os.WriteFile(sessionFile, []byte(`{"test":"data"}`), 0o644))

	provider, ok := parser.NewProvider(parser.AgentIflow, parser.ProviderConfig{
		Roots: []string{tmpDir},
	})
	require.True(ok)

	// Test finding the file
	found, ok, err := provider.FindSource(t.Context(), parser.FindSourceRequest{
		RawSessionID: sessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sessionFile, found.DisplayPath)

	// Test finding a non-existent file
	_, ok, err = provider.FindSource(t.Context(), parser.FindSourceRequest{
		RawSessionID: "nonexistent",
	})
	require.NoError(err)
	assert.False(ok)

	// Test finding a fork ID (should extract base session ID)
	// Fork IDs have format: <baseUUID>-<childUUID>
	// The source lookup should use only the base UUID
	baseSessionID := "96e6d875-92eb-40b9-b193-a9ba99f0f709"
	forkSessionID := baseSessionID + "-12345678-1234-5678-9abc-def012345678"
	forkSessionFile := filepath.Join(proj, "session-"+baseSessionID+".jsonl")
	require.NoError(os.WriteFile(forkSessionFile, []byte(`{"test":"fork"}`), 0o644))

	// Test finding the fork session - should find the base file
	foundFork, ok, err := provider.FindSource(t.Context(), parser.FindSourceRequest{
		RawSessionID: forkSessionID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(forkSessionFile, foundFork.DisplayPath, "for fork ID %s", forkSessionID)
}
