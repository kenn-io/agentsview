package parser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkParseCodexSession(b *testing.B) {
	fixtureDir := filepath.Join("testdata", "codex")
	toolCalls, err := os.ReadFile(filepath.Join(fixtureDir, "function_calls.jsonl"))
	require.NoError(b, err)
	largePath := filepath.Join(b.TempDir(), "large_session.jsonl")
	err = os.WriteFile(
		largePath, []byte(strings.Repeat(string(toolCalls), 100)), 0o600,
	)
	require.NoError(b, err)

	factory, ok := ProviderFactoryByType(AgentCodex)
	require.True(b, ok)
	provider, ok := factory.NewProvider(ProviderConfig{}).(*codexProvider)
	require.True(b, ok)

	for _, fixture := range []struct {
		name string
		path string
	}{
		{"standard", filepath.Join(fixtureDir, "standard_session.jsonl")},
		{"tool_calls", filepath.Join(fixtureDir, "function_calls.jsonl")},
		{"large_tool_calls", largePath},
	} {
		b.Run(fixture.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_, _, err := provider.parseSession(
					fixture.path, "benchmark", false,
				)
				require.NoError(b, err)
			}
		})
	}
}
