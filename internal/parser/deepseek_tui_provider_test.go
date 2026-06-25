package parser

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeepSeekTUIProviderFactoryReplacesLegacyAdapter(t *testing.T) {
	factory, ok := ProviderFactoryByType(AgentDeepSeekTUI)
	require.True(t, ok)
	require.NotNil(t, factory)

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{t.TempDir()},
		Machine: "devbox",
	})
	require.True(t, ok)
	require.NotNil(t, provider)
}

func TestDeepSeekTUIProviderSourceMethods(t *testing.T) {
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
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, AgentDeepSeekTUI, discovered[0].Provider)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~deepseek-tui:session_123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	found, ok, err = provider.FindSource(context.Background(), FindSourceRequest{
		FingerprintKey: sourcePath,
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

func TestDeepSeekTUIProviderSourceMethodsFollowSymlinkedSessionFile(t *testing.T) {
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
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	assert.Equal(t, sourcePath, discovered[0].DisplayPath)

	found, ok, err := provider.FindSource(context.Background(), FindSourceRequest{
		FullSessionID: "host~deepseek-tui:session_123",
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourcePath, found.DisplayPath)

	changed, err := provider.SourcesForChangedPath(
		context.Background(),
		ChangedPathRequest{Path: sourcePath, EventKind: "write", WatchRoot: root},
	)
	require.NoError(t, err)
	require.Len(t, changed, 1)
	assert.Equal(t, sourcePath, changed[0].DisplayPath)
}

func TestDeepSeekTUIProviderParse(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "session_123.json")
	content := deepSeekTUIProviderFixture()
	writeSourceFile(t, sourcePath, content)

	provider, ok := NewProvider(AgentDeepSeekTUI, ProviderConfig{
		Roots:   []string{root},
		Machine: "devbox",
	})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)

	fingerprint, err := provider.Fingerprint(context.Background(), sources[0])
	require.NoError(t, err)

	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source:      sources[0],
		Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, outcome.ResultSetComplete)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Equal(t, "deepseek-tui:session_123", outcome.Results[0].Result.Session.ID)
	assert.Equal(t, "sample_project", outcome.Results[0].Result.Session.Project)
	assert.Equal(t, "devbox", outcome.Results[0].Result.Session.Machine)
	assert.Equal(t,
		fmt.Sprintf("%x", sha256.Sum256([]byte(content))),
		outcome.Results[0].Result.Session.File.Hash,
	)
	assert.Len(t, outcome.Results[0].Result.Messages, 2)
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
