package sync

import (
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
	require := require.New(t)

	root := t.TempDir()
	transcript := filepath.Join(root, "workspace", "sess_0123456789abcdef", "messages.jsonl")
	require.NoError(os.MkdirAll(filepath.Dir(transcript), 0o755))
	require.NoError(os.WriteFile(transcript,
		[]byte(`{"payload":{"type":"user","content":"synthetic prompt"}}`+"\n"), 0o644))

	provider, ok := parser.NewProvider(parser.AgentKiro, parser.ProviderConfig{Roots: []string{root}})
	require.True(ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(err)
	require.Len(sources, 1)
	result, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(err)
	require.Len(result.Results, 1)
	require.Equal("kiro:sess_0123456789abcdef", result.Results[0].Result.Session.ID)
}
