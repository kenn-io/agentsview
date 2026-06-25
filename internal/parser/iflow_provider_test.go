package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIflowProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentIflow)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentIflow, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestIflowProviderSourceMethods(t *testing.T) {
	root := t.TempDir()
	projectDir := filepath.Join(root, "test-project")
	rawID := "5de701fc-7454-4858-a249-95cac4fd3b51"
	sourcePath := filepath.Join(projectDir, "session-"+rawID+".jsonl")
	copyFixtureFile(t, "testdata/iflow/session-"+rawID+".jsonl", sourcePath)
	writeSourceFile(t, filepath.Join(projectDir, rawID+".jsonl"), "{}\n")

	provider, ok := NewProvider(AgentIflow, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentIflow, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Equal(t, "test-project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~iflow:" + rawID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	forkID := rawID + "-6f5d8718-7a95-4bb8-965f-faa23246c82d"
	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: forkID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	require.NoError(t, os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestIflowProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	root := t.TempDir()
	realProjectDir := filepath.Join(t.TempDir(), "real-project")
	linkProjectDir := filepath.Join(root, "linked-project")
	if err := os.Symlink(realProjectDir, linkProjectDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	rawID := "5de701fc-7454-4858-a249-95cac4fd3b51"
	sourcePath := filepath.Join(linkProjectDir, "session-"+rawID+".jsonl")
	copyFixtureFile(
		t,
		"testdata/iflow/session-"+rawID+".jsonl",
		filepath.Join(realProjectDir, "session-"+rawID+".jsonl"),
	)

	provider, ok := NewProvider(AgentIflow, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)
	assert.Equal(t, "linked-project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		RawSessionID: rawID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)
}

func TestIflowProviderParse(t *testing.T) {
	root := t.TempDir()
	project := "test-project"
	rawID := "5de701fc-7454-4858-a249-95cac4fd3b51"
	sourcePath := filepath.Join(root, project, "session-"+rawID+".jsonl")
	copyFixtureFile(t, "testdata/iflow/session-"+rawID+".jsonl", sourcePath)

	provider, ok := NewProvider(AgentIflow, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "iflow:"+rawID, outcome.Results[0].Result.Session.ID)
	// The provider mirrors the legacy sync project resolution, deriving the
	// canonical project from the session's recorded cwd rather than the raw
	// project directory name.
	assert.Equal(t, "docker_image_retagger", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t, "abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(t, outcome.Results[0].Result.Messages, 11)
}

func copyFixtureFile(t *testing.T, src, dst string) {
	t.Helper()

	data, err := os.ReadFile(src)
	require.NoError(t, err)
	writeSourceFile(t, dst, string(data))
}
