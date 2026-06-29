package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAiderProviderFindSourceUsesCanonicalIdentity(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	historyPath := filepath.Join(repo, AiderHistoryFileName())
	require.NoError(t, os.WriteFile(historyPath, []byte(strings.Join([]string{
		"# aider chat started at 2026-06-09 14:01:00",
		"#### canonical prompt",
		"canonical answer",
	}, "\n")+"\n"), 0o644))

	remoteHistoryPath := "/remote/repo/" + AiderHistoryFileName()
	rewriter := func(path string) string {
		if history, idx, ok := ParseAiderVirtualPath(path); ok && history == historyPath {
			return AiderVirtualPath(remoteHistoryPath, idx)
		}
		if path == historyPath {
			return remoteHistoryPath
		}
		return path
	}
	provider, ok := NewProvider(AgentAider, ProviderConfig{
		Roots:        []string{root},
		Machine:      "remote-host",
		PathRewriter: rewriter,
	})
	require.True(t, ok)

	discovered, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{
		Source: discovered[0],
	})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	rawID := strings.TrimPrefix(result.Session.ID, "aider:")
	localVirtualPath := result.Session.File.Path
	remoteVirtualPath := rewriter(localVirtualPath)

	foundByRawID, ok, err := provider.FindSource(
		context.Background(),
		FindSourceRequest{RawSessionID: rawID},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, localVirtualPath, foundByRawID.DisplayPath)

	foundByStoredPath, ok, err := provider.FindSource(
		context.Background(),
		FindSourceRequest{StoredFilePath: remoteVirtualPath},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, localVirtualPath, foundByStoredPath.DisplayPath)

	foundByFingerprintKey, ok, err := provider.FindSource(
		context.Background(),
		FindSourceRequest{FingerprintKey: remoteVirtualPath},
	)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, localVirtualPath, foundByFingerprintKey.DisplayPath)
}
