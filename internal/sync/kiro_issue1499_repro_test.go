package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// Synthetic reproduction: issue #1499 shipped no loadable transcript artifact.
// This preserves the reported provider-relative path and documented envelope
// shape, then proves the existing Kiro provider emits the session.
func TestKiroIssue1499CurrentLayoutReproduction(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(root, "workspace", "sess_0123456789abcdef", "messages.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
	require.NoError(t, os.WriteFile(transcript,
		[]byte(`{"payload":{"type":"user","content":"synthetic prompt"}}`+"\n"), 0o644))

	provider, ok := parser.NewProvider(parser.AgentKiro, parser.ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	result, err := provider.Parse(context.Background(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, result.Results, 1)
	require.Equal(t, "kiro:sess_0123456789abcdef", result.Results[0].Result.Session.ID)
}
