package parser_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCompactionSummaryEventAgents(t *testing.T) {
	for agent, task := range map[parser.AgentType]string{parser.AgentRooCode: "test-task-tools", parser.AgentKiloLegacy: "session"} {
		t.Run(string(agent), func(t *testing.T) {
			root := parsedOutputFixture(t, agent)
			path := filepath.Join(root, "tasks", task, "ui_messages.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			var rows []map[string]any
			require.NoError(t, json.Unmarshal(data, &rows))
			rows = append(rows, map[string]any{"ts": 1700000020000, "type": "say", "say": "condense_context", "text": "Context condensed"})
			data, err = json.Marshal(rows)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}, Machine: "test"})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			var events []string
			for _, source := range sources {
				out, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "test"})
				require.NoError(t, err)
				for _, result := range out.Results {
					assert.True(t, db.IsCompactionSummaryExcludedAgent(string(result.Result.Session.Agent)))
					for _, m := range result.Result.Messages {
						if m.IsCompactBoundary {
							events = append(events, m.Content)
						}
					}
				}
			}
			assert.Contains(t, events, "Context condensed")
		})
	}
}
