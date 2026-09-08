package parser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTauIssue1634ArtifactReproduction(t *testing.T) {
	artifact := os.Getenv("AGENTSVIEW_TAU_ISSUE_ARTIFACT")
	if artifact == "" {
		artifact = filepath.Join("testdata", "tau", "issue-session.jsonl")
	}
	data, err := os.ReadFile(artifact)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	factory, ok := ProviderFactoryByType(AgentType("tau"))
	require.True(t, ok, "AgentType(\"tau\") provider must be registered")
	root := t.TempDir()
	project := filepath.Join(root, "project.with-hyphen_and-dots")
	require.NoError(t, os.MkdirAll(project, 0o755))
	path := filepath.Join(project, "issue-session.jsonl")
	require.NoError(t, os.WriteFile(path, data, 0o644))

	provider := factory.NewProvider(ProviderConfig{
		Roots: []string{root}, Machine: "test-machine",
	})
	sources, err := provider.Discover(context.Background())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(context.Background(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	assert.Equal(t, "tau:issue-session", result.Session.ID)
	assert.Equal(t, 33, len(dataLines(data)))
	assert.Equal(t, 14, result.Session.MessageCount)
	assert.Equal(t, 4, result.Session.UserMessageCount)
	assert.Equal(t, 3, countTauToolCalls(result.Messages))
	assert.Equal(t, 3, countTauToolResults(result.Messages))
	assert.Equal(t, 7985, sumTauUsage(result.Messages, "input_tokens"))
	assert.Equal(t, 588, sumTauUsage(result.Messages, "output_tokens"))
}

func dataLines(data []byte) []string {
	lines := make([]string, 0)
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, string(data[start:i]))
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}

func countTauToolCalls(messages []ParsedMessage) int {
	count := 0
	for _, message := range messages {
		count += len(message.ToolCalls)
	}
	return count
}

func countTauToolResults(messages []ParsedMessage) int {
	count := 0
	for _, message := range messages {
		count += len(message.ToolResults)
	}
	return count
}

func sumTauUsage(messages []ParsedMessage, key string) int {
	count := 0
	for _, message := range messages {
		count += tauUsageValue(message, key)
	}
	return count
}

func tauUsageValue(message ParsedMessage, key string) int {
	var usage map[string]int
	if err := json.Unmarshal(message.TokenUsage, &usage); err != nil {
		return 0
	}
	return usage[key]
}
