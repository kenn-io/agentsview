package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
)

func TestLookupRejectsUnsupportedClientBeforeSetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable shims require Unix")
	}
	dir := t.TempDir()
	client := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(client, []byte("#!/bin/sh\necho 'codex-cli 0.0.0'\n"), 0o700))
	t.Setenv("PATH", dir)
	cfg := config{outputDir: filepath.Join(dir, "output"), repository: dir}
	_, err := runLookupClient(t.Context(), cfg, "missing-agentsview", clientCodex, nil, nil)
	require.ErrorIs(t, err, errLookupCaptureUnsupported)
	_, err = os.Stat(cfg.outputDir)
	require.True(t, os.IsNotExist(err))
}

func TestLookupVersionUsesResolvedClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable shims require Unix")
	}
	dir := t.TempDir()
	resolved := filepath.Join(dir, "real-codex")
	require.NoError(t, os.WriteFile(resolved, []byte("#!/bin/sh\necho 'codex-cli 0.172.0'\n"), 0o700))
	mise := filepath.Join(dir, "mise")
	require.NoError(t, os.WriteFile(mise, []byte("#!/bin/sh\nif [ \"$1\" = which ]; then echo '"+resolved+"'; else echo 'codex-cli 0.0.0'; fi\n"), 0o700))
	require.NoError(t, os.Symlink(mise, filepath.Join(dir, "codex")))
	t.Setenv("PATH", dir)
	cfg := config{outputDir: filepath.Join(dir, "output"), repository: dir}
	_, err := runLookupClient(t.Context(), cfg, "missing-agentsview", clientCodex, nil, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, errLookupCaptureUnsupported)
}

func TestLookupClientVersionSupported(t *testing.T) {
	for _, tc := range []struct {
		client  clientName
		version string
		want    bool
	}{
		{clientClaude, "2.1.332 (Claude Code)", true},
		{clientClaude, "2.1.333 (Claude Code)", false},
		{clientCodex, "codex-cli 0.172.0", true},
		{clientCodex, "codex-cli 0.173.0", false},
		{clientCodex, "unknown", false},
	} {
		t.Run(string(tc.client)+"/"+tc.version, func(t *testing.T) {
			require.Equal(t, tc.want, lookupClientVersionSupported(tc.client, tc.version))
		})
	}
}

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

func TestLookupOwnerFixturesMatchPRFilter(t *testing.T) {
	root := t.TempDir()
	transcriptsRoot := filepath.Join(root, "transcripts")
	fixtures := []lookupFixture{
		{
			ID: "33333333-3333-4333-8333-333333333333", AgeHours: 48,
			Title:   "Review https://github.com/example-org/project-a/pull/123 ownership",
			Content: "The earlier owner handed the pull request off.",
		},
		{
			ID: "44444444-4444-4444-8444-444444444444", AgeHours: 24,
			Title:   "Take over https://github.com/example-org/project-a/pull/123",
			Content: "The maple team now owns the pull request.",
		},
	}
	for _, fixture := range fixtures {
		_, err := writeLookupFixtureTranscript(transcriptsRoot, root, fixture)
		require.NoError(t, err)
	}
	provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{transcriptsRoot}, Machine: "local"})
	require.True(t, ok)
	database := dbtest.OpenTestDB(t)
	for _, fixture := range fixtures {
		source, found, err := provider.FindSource(t.Context(), parser.FindSourceRequest{FullSessionID: "codex:" + fixture.ID})
		require.NoError(t, err)
		require.True(t, found)
		parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, parsed.Results, 1)
		session := parsed.Results[0].Result.Session
		links, err := parser.PromptPRLinksContext(t.Context(), fixture.Title)
		require.NoError(t, err)
		require.Len(t, links, 1)
		require.Equal(t, "example-org/project-a", links[0].Repository)
		dbtest.SeedSession(t, database, session.ID, "lookup-project", func(stored *db.Session) {
			stored.PRLinks = links
		})
	}

	listed, err := service.NewDirectBackend(database, nil).List(t.Context(), service.ListFilter{PR: "example-org/project-a#123"})
	require.NoError(t, err)
	require.Equal(t, 2, listed.Total)
	require.Len(t, listed.Sessions, 2)
}
