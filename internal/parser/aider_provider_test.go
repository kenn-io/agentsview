package parser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAiderStreamingDiscoveryReportsTraversalLimits(t *testing.T) {
	writeHistory := func(t *testing.T, root, project string) {
		t.Helper()
		dir := filepath.Join(root, project)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, aiderHistoryFile),
			[]byte("# aider chat started at 2026-07-14 12:00:00\n#### hello\nworld\n"),
			0o600,
		))
	}

	for _, tc := range []struct {
		name   string
		limits discoveryTraversalLimits
		files  int
		want   string
	}{
		{name: "time", limits: discoveryTraversalLimits{expired: func() bool { return true }}, files: 1, want: "time budget"},
		{name: "directories", limits: discoveryTraversalLimits{maxDirs: 1}, files: 2, want: "directory limit"},
		{name: "files", limits: discoveryTraversalLimits{maxFiles: 1}, files: 2, want: "file limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i := range tc.files {
				writeHistory(t, root, fmt.Sprintf("project-%d", i))
			}
			provider, ok := NewProvider(AgentAider, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			ctx := withDiscoveryTraversalLimits(t.Context(), tc.limits)

			err := provider.(StreamingDiscoverer).DiscoverEach(ctx, func(SourceRef) error { return nil })

			var incomplete DiscoveryIncompleteError
			require.ErrorAs(t, err, &incomplete)
			assert.Contains(t, err.Error(), tc.want)
			assert.False(t, errors.Is(err, context.Canceled))
		})
	}
}

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
