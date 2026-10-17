package ingest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestPiDialogueProjectionReachesStorage(t *testing.T) {
	const body = `{"type":"session","version":3,"id":"dialogue","timestamp":"2026-05-01T10:00:00Z"}
{"type":"message","id":"u","message":{"role":"user","content":[{"type":"text","text":"Question"}]}}
{"type":"message","id":"a","parentId":"u","message":{"role":"assistant","content":[{"type":"text","text":"Literal [Read: quokka.txt]"},{"type":"thinking","thinking":"reasoningneedle"},{"type":"toolCall","id":"call-a","name":"read","arguments":{"file_path":"quokka.txt"}}]}}
`
	const dialogue = "Literal [Read: quokka.txt]"
	root := t.TempDir()
	path := filepath.Join(root, "dialogue.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	provider, ok := parser.NewProvider(parser.AgentPi, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
	require.True(t, ok)
	outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: parser.SourceRef{Provider: parser.AgentPi, DisplayPath: path, Opaque: parser.JSONLSource{Root: root, Path: path}}})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	candidate, err := ingest.PrepareCandidate(t.Context(), outcome.Results[0].Result, ingest.ContentOptions{})
	require.NoError(t, err)
	prepared, err := ingest.Finalize(t.Context(), candidate, ingest.ContentOptions{})
	require.NoError(t, err)
	destination := dbtest.OpenTestDB(t)
	_, err = destination.WriteSessionBatchContext(t.Context(), []db.SessionBatchWrite{{Session: prepared.Session, Messages: prepared.Messages, ReplaceMessages: true}})
	require.NoError(t, err)
	stored, err := destination.GetAllMessages(t.Context(), prepared.Session.ID)
	require.NoError(t, err)
	require.Len(t, stored, 2)
	assert.Equal(t, new(dialogue), stored[1].DialogueText)
	assert.Equal(t, "reasoningneedle", stored[1].ThinkingText)
}
