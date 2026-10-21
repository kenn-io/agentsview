package ingest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestFileEvidencePairingPreservesEmptyReadAndPolicy(t *testing.T) {
	n := int64(0)
	parsed := parser.ParseResult{Session: parser.ParsedSession{ID: "evidence", Agent: parser.AgentPi}, Messages: []parser.ParsedMessage{
		{Ordinal: 0, Role: parser.RoleAssistant, Content: "read example.txt", ToolCalls: []parser.ParsedToolCall{{ToolUseID: "t", ToolName: "read", Category: "Read", InputJSON: `{"path":"example.txt"}`}}, FileEvidence: []fileevidence.Candidate{{Record: fileevidence.Record{Kind: "read", Size: &n, SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", ProofMethod: fileevidence.ProofPiReadUTF8V1}, Support: "result", Call: 0}}},
		{Ordinal: 1, Role: parser.RoleUser, ToolResults: []parser.ParsedToolResult{{ToolUseID: "t", ContentRaw: `[{"type":"text","text":""}]`}}},
	}}
	for _, blocked := range []bool{false, true} {
		c, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{BlockedResultCategories: map[string]bool{"Read": blocked}})
		require.NoError(t, err)
		require.Len(t, c.Messages, 1)
		if blocked {
			assert.Empty(t, c.Messages[0].FileEvidenceCandidates)
			assert.Equal(t, "withheld", c.Messages[0].FileEvidenceCoverage.State)
		} else {
			require.Len(t, c.Messages[0].FileEvidenceCandidates, 1)
			assert.Equal(t, n, *c.Messages[0].FileEvidenceCandidates[0].Record.Size)
		}
	}
	parsed.Session.Agent = parser.AgentOMP
	c, err := ingest.PrepareCandidate(t.Context(), parsed, ingest.ContentOptions{})
	require.NoError(t, err)
	assert.Empty(t, c.Messages[0].FileEvidenceCandidates)
}

func TestFileEvidencePairingUpdatesPersistedCallCoordinate(t *testing.T) {
	parsed := []parser.ParsedMessage{
		{
			Ordinal: 0, Role: parser.RoleAssistant,
			ToolCalls: []parser.ParsedToolCall{{
				ToolUseID: "t", ToolName: "read", Category: "Read",
			}},
		},
		{
			Ordinal: 1, Role: parser.RoleUser,
			ToolResults: []parser.ParsedToolResult{{
				ToolUseID: "t", ContentRaw: `[{"type":"text","text":""}]`,
			}},
			FileEvidence: []fileevidence.Candidate{{
				Record: fileevidence.Record{
					Position: fileevidence.Position{Entry: "result", Call: -1, ResultEntry: "result", Result: 0, Block: 1},
					Kind:     "image", MediaType: "image/png",
				},
				Support: "result", ToolUseID: "t", SupportingText: "", Call: -1, Result: 0,
			}},
		},
	}
	messages, err := ingest.ConvertMessagesContext(t.Context(), "evidence", parser.AgentPi, parsed, nil)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].FileEvidenceCandidates, 1)
	candidate := messages[0].FileEvidenceCandidates[0]
	assert.Equal(t, 0, candidate.Call)
	assert.Equal(t, 0, candidate.Record.Position.Call)
}

func TestFileEvidenceClaudeMixedResultCarrier(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project", "evidence.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	content := `{"type":"assistant","uuid":"a","sessionId":"evidence","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Read","input":{"file_path":"example.txt"}}]}}` + "\n" +
		`{"type":"user","uuid":"u","parentUuid":"a","sessionId":"evidence","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWxwaGEK"}},{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"YWxwaGEK"}}]}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	provider, ok := parser.NewProvider(parser.AgentClaude, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	candidate, err := ingest.PrepareCandidate(t.Context(), outcome.Results[0].Result, ingest.ContentOptions{})
	require.NoError(t, err)
	require.Len(t, candidate.Messages, 2)
	assert.Empty(t, candidate.Messages[1].Content)
	require.Len(t, candidate.Messages[1].FileEvidenceCandidates, 2)
	database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	database.SetAssetsDir(t.TempDir())
	require.NoError(t, database.UpsertSession(t.Context(), candidate.Session))
	require.NoError(t, database.ReplaceSessionMessages(t.Context(), candidate.Session.ID, candidate.Messages))
	snapshot, err := database.ReadFileEvidenceSnapshot(t.Context(), candidate.Session.ID, fileevidence.Window{FirstOrdinal: new(1)})
	require.NoError(t, err)
	require.Len(t, snapshot.Rows, 2)
	for _, row := range snapshot.Rows {
		assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", row.Record.SHA256)
	}
	payload, err := os.ReadFile(filepath.Join(database.AssetsDir(), "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060.png"))
	require.NoError(t, err)
	assert.Equal(t, "alpha\n", string(payload))
}
