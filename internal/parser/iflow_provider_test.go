package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIflowProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentIflow, discovered[0].Provider)
	assert.Equal(sourcePath, discovered[0].DisplayPath)
	assert.Equal("test-project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~iflow:" + rawID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	forkID := rawID + "-6f5d8718-7a95-4bb8-965f-faa23246c82d"
	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: forkID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	require.NoError(os.Remove(sourcePath))
	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "remove", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestIflowProviderDiscoversSymlinkedProjectDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	realProjectDir := filepath.Join(t.TempDir(), "real-project")
	linkProjectDir := filepath.Join(root, "linked-project")
	rawID := "5de701fc-7454-4858-a249-95cac4fd3b51"
	// Populate the target directory before symlinking so Windows records a
	// directory symlink. Symlinking a not-yet-existent target yields a file
	// symlink there, which discovery cannot descend into.
	copyFixtureFile(
		t,
		"testdata/iflow/session-"+rawID+".jsonl",
		filepath.Join(realProjectDir, "session-"+rawID+".jsonl"),
	)
	if err := os.Symlink(realProjectDir, linkProjectDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	sourcePath := filepath.Join(linkProjectDir, "session-"+rawID+".jsonl")

	provider, ok := NewProvider(AgentIflow, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)
	assert.Equal("linked-project", discovered[0].ProjectHint)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		RawSessionID: rawID,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)
}

func TestIflowProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	project := "test-project"
	rawID := "5de701fc-7454-4858-a249-95cac4fd3b51"
	sourcePath := filepath.Join(root, project, "session-"+rawID+".jsonl")
	copyFixtureFile(t, "testdata/iflow/session-"+rawID+".jsonl", sourcePath)

	provider, ok := NewProvider(AgentIflow, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: SourceFingerprint{Key: sourcePath, Hash: "abc123"},
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	assert.Equal(DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal("iflow:"+rawID, outcome.Results[0].Result.Session.ID)
	// The provider mirrors the legacy sync project resolution, deriving the
	// canonical project from the session's recorded cwd rather than the raw
	// project directory name.
	assert.Equal("docker_image_retagger", outcome.Results[0].Result.Session.Project)
	assert.Equal("devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal("abc123", outcome.Results[0].Result.Session.File.Hash)
	assert.Len(outcome.Results[0].Result.Messages, 11)
}

func copyFixtureFile(t *testing.T, src, dst string) {
	t.Helper()

	data, err := os.ReadFile(src)
	require.NoError(t, err)
	writeSourceFile(t, dst, string(data))
}
