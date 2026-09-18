package parser

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepSeekTUIProviderSourceMethods(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "session_123.json")
	writeSourceFile(t, sourcePath, deepSeekTUIProviderFixture())
	writeSourceFile(t, filepath.Join(root, "latest.json"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "offline_queue.json"), "{}\n")
	writeSourceFile(t, filepath.Join(root, "nested", "session_456.json"), "{}\n")

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(AgentDeepSeekTUI, discovered[0].Provider)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~deepseek-tui:session_123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(t.Context(), FindSourceRequest{
		FingerprintKey: sourcePath,
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

func TestDeepSeekTUIProviderSourceMethodsFollowSymlinkedSessionFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "session_123.json")
	sourcePath := filepath.Join(root, "session_123.json")
	writeSourceFile(t, targetPath, deepSeekTUIProviderFixture())
	if err := os.Symlink(targetPath, sourcePath); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)

	discovered, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(discovered, 1)
	assert.Equal(sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(t.Context(), FindSourceRequest{
		FullSessionID: "host~deepseek-tui:session_123",
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		t.Context(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(err)
	require.Len(changed, 1)
	assert.Equal(sourcePath, changed[0].DisplayPath)
}

func TestDeepSeekTUIProviderParse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := t.TempDir()
	sourcePath := filepath.Join(root, "session_123.json")
	content := deepSeekTUIProviderFixture()
	writeSourceFile(t, sourcePath, content)

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)

	fingerprint, err := provider.Fingerprint(t.Context(), sources[0])
	require.NoError(err)

	outcome, err := provider.Parse(t.Context(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(err)
	require.True(outcome.ResultSetComplete)
	require.Len(outcome.Results, 1)
	assert.Equal(DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal("deepseek-tui:session_123", outcome.Results[0].Result.Session.ID)
	assert.Equal("sample_project", outcome.Results[0].Result.Session.Project)
	assert.Equal("devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		outcome.Results[0].Result.Session.File.Hash,
	)
	assert.Len(outcome.Results[0].Result.Messages, 2)
}

func deepSeekTUIProviderFixture() string {
	return `{
  "metadata": {
    "id": "session_123",
    "title": "Investigate DeepSeek TUI",
    "created_at": "2026-06-01T10:00:00Z",
    "updated_at": "2026-06-01T10:02:00Z",
    "model": "deepseek-chat",
    "workspace": "/Users/alice/code/sample-project"
  },
  "messages": [
    {"role": "user", "content": "Inspect server logs", "timestamp": "2026-06-01T10:00:05Z"},
    {"role": "assistant", "content": [{"type": "text", "text": "The server failed during startup."}], "timestamp": "2026-06-01T10:00:10Z"}
  ]
}`
}
