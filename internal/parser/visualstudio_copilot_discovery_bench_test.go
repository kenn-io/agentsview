package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkVisualStudioCopilotStreamingDiscovery(b *testing.B) {
	root := b.TempDir()
	var traces strings.Builder
	for i := range 1000 {
		conversation := fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		for j := range 4 {
			traces.WriteString(vsCopilotTraceLineJSONWithSpanID(
				conversation, fmt.Sprintf("span-%d-%d", i, j), "chat gpt-5.5",
				"1781293600000000000", "1781293610000000000", nil,
			))
			traces.WriteByte('\n')
		}
	}
	path := filepath.Join(root, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl")
	require.NoError(b, os.WriteFile(path, []byte(traces.String()), 0o600))
	provider, ok := NewProvider(AgentVSCopilot, ProviderConfig{Roots: []string{root}})
	require.True(b, ok)
	discoverer := provider.(StreamingDiscoverer)
	b.ReportAllocs()
	for b.Loop() {
		ctx, closeCache, err := WithReconciliationCache(b.Context())
		require.NoError(b, err)
		count := 0
		err = discoverer.DiscoverEach(ctx, func(SourceRef) error {
			count++
			return nil
		})
		closeErr := closeCache()
		require.NoError(b, err)
		require.NoError(b, closeErr)
		require.Equal(b, 1000, count)
	}
}
