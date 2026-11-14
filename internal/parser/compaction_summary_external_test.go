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

func TestCompactionSummaryParserMarkers(t *testing.T) {
	for _, agent := range []parser.AgentType{parser.AgentRooCode, parser.AgentKiloLegacy, parser.AgentEvener} {
		t.Run(string(agent), func(t *testing.T) {
			root := parsedOutputFixture(t, agent)
			if agent == parser.AgentEvener {
				path := filepath.Join(root, "sessions", "session.transcript.jsonl")
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = f.WriteString("\n{\"kind\":\"entry\",\"seq\":2,\"turn\":{\"kind\":\"CHECKPOINT\"}}\n{\"kind\":\"entry\",\"seq\":3,\"turn\":{\"kind\":\"SUMMARY\"}}\n{\"kind\":\"entry\",\"seq\":4,\"turn\":{\"kind\":\"CHECKPOINT\",\"message\":{\"role\":\"system\",\"content\":[{\"kind\":\"text\",\"text\":\"Retained summary\"}]}}}\n")
				require.NoError(t, err)
				require.NoError(t, f.Close())
			} else {
				task := "session"
				if agent == parser.AgentRooCode {
					task = "test-task-tools"
				}
				path := filepath.Join(root, "tasks", task, "ui_messages.json")
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var rows []map[string]any
				require.NoError(t, json.Unmarshal(data, &rows))
				rows = append(rows, map[string]any{"ts": 1700000020000, "type": "say", "say": "condense_context", "text": "Context condensed"})
				data, err = json.Marshal(rows)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0600))
			}
			provider, ok := parser.NewProvider(agent, parser.ProviderConfig{Roots: []string{root}, Machine: "test"})
			require.True(t, ok)
			sources, err := provider.Discover(t.Context())
			require.NoError(t, err)
			require.NotEmpty(t, sources)
			found := map[string]bool{}
			for _, source := range sources {
				out, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "test"})
				require.NoError(t, err)
				require.Empty(t, out.SourceErrors)
				for _, result := range out.Results {
					if agent != parser.AgentEvener {
						assert.True(t, db.IsCompactionEventAgent(string(result.Result.Session.Agent)))
					}
					for _, m := range result.Result.Messages {
						if m.IsCompactBoundary {
							found[m.Content] = db.IsCompactionPlaceholder(m.SourceSubtype, m.Content)
						}
					}
				}
			}
			if agent == parser.AgentEvener {
				assert.Equal(t, map[string]bool{"[CHECKPOINT]": true, "[SUMMARY]": true, "Retained summary": false}, found)
			} else {
				assert.Contains(t, found, "Context condensed")
			}
		})
	}
}
