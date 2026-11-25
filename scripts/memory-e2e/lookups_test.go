package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

func TestLookupFixtureSessionsAppearInDefaultList(t *testing.T) {
	root := t.TempDir()
	transcriptsRoot := filepath.Join(root, "transcripts")
	fixture := lookupFixture{
		ID:       lookupID,
		AgeHours: 24,
		Title:    "Question about an interactive session",
		Content:  "The session records the answer to this question.",
	}
	_, err := writeLookupFixtureTranscript(transcriptsRoot, root, fixture)
	require.NoError(t, err)

	cfg := parser.ProviderConfig{Roots: []string{transcriptsRoot}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{
		FullSessionID: "codex:" + fixture.ID,
	})
	require.NoError(t, err)
	require.True(t, found)
	parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, parsed.Results, 1)
	session := parsed.Results[0].Result.Session

	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, session.ID, "lookup-project", func(stored *db.Session) {
		stored.Agent = string(session.Agent)
		stored.SessionKind = session.SessionKind
		stored.RelationshipType = string(session.RelationshipType)
		stored.FirstMessage = dbtest.Ptr(session.FirstMessage)
		stored.MessageCount = session.MessageCount
		stored.UserMessageCount = session.UserMessageCount
	})
	listed, err := service.NewDirectBackend(database, nil).List(t.Context(), service.ListFilter{})
	require.NoError(t, err)
	require.Equal(t, 1, listed.Total)
	require.Equal(t, session.ID, listed.Sessions[0].ID)
}
