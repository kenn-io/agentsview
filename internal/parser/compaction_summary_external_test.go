package parser_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
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
				if agent == parser.AgentEvener && !strings.HasSuffix(source.DisplayPath, "/session.transcript.jsonl") {
					continue
				}
				out, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "test"})
				require.NoError(t, err)
				require.Empty(t, out.SourceErrors)
				for _, result := range out.Results {
					if agent != parser.AgentEvener {
						assert.True(t, db.IsCompactionEventAgent(string(result.Result.Session.Agent)))
					}
					for _, m := range result.Result.Messages {
						if m.IsCompactBoundary {
							found[m.Content] = db.IsCompactionPlaceholder(m.SourceType)
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

func TestEvenerLiteralSummarySurvivesCompactionRead(t *testing.T) {
	root := parsedOutputFixture(t, parser.AgentEvener)
	path := filepath.Join(root, "sessions", "session.transcript.jsonl")
	provider, ok := parser.NewProvider(parser.AgentEvener, parser.ProviderConfig{Roots: []string{root}, Machine: "test"})
	require.True(t, ok)
	beforeSources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var beforeID, beforeKey string
	for _, source := range beforeSources {
		if !strings.HasSuffix(source.DisplayPath, "/session.transcript.jsonl") {
			continue
		}
		beforeKey = source.FingerprintKey
		out, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "test"})
		require.NoError(t, err)
		require.Len(t, out.Results, 1)
		beforeID = out.Results[0].Result.Session.ID
	}
	require.NotEmpty(t, beforeID)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.WriteString("\n{\"kind\":\"entry\",\"seq\":2,\"turn\":{\"kind\":\"SUMMARY\"}}\n{\"kind\":\"entry\",\"seq\":3,\"turn\":{\"kind\":\"SUMMARY\",\"message\":{\"role\":\"system\",\"content\":[{\"kind\":\"text\",\"text\":\"[SUMMARY]\"}]}}}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	var boundary []parser.ParsedMessage
	for _, source := range sources {
		if !strings.HasSuffix(source.DisplayPath, "/session.transcript.jsonl") {
			continue
		}
		assert.Equal(t, beforeKey, source.FingerprintKey)
		out, err := provider.Parse(parser.WithoutFilesystemProjectDiscovery(t.Context()), parser.ParseRequest{Source: source, Machine: "test"})
		require.NoError(t, err)
		for _, result := range out.Results {
			assert.Equal(t, beforeID, result.Result.Session.ID)
			for _, msg := range result.Result.Messages {
				if msg.IsCompactBoundary {
					boundary = append(boundary, msg)
				}
			}
		}
	}
	require.Len(t, boundary, 2)
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, d, "evener", "project", func(s *db.Session) { s.Agent = "evener"; s.EndedAt = new("2024-06-15T00:00:00Z") })
	for _, msg := range boundary {
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: "evener", Ordinal: msg.Ordinal, Role: string(msg.Role), IsCompactBoundary: true, SourceType: msg.SourceType, SourceSubtype: msg.SourceSubtype, Content: msg.Content}}))
	}
	svc := service.NewDirectBackend(d, nil)
	for _, req := range []service.CompactionSummariesRequest{{SessionID: "evener"}, {ActiveSince: "2024-06-01T00:00:00Z"}} {
		out, err := service.CompactionSummaries(t.Context(), svc, req)
		require.NoError(t, err)
		require.Len(t, out.Summaries, 1)
		assert.Equal(t, "[SUMMARY]", out.Summaries[0].Content)
		assert.Equal(t, boundary[1].Ordinal, out.Summaries[0].Ordinal)
	}
}
