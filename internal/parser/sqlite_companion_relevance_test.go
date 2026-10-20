package parser

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSQLiteCompanionChangedPathRelevance(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "antigravity-cli")
	ide := filepath.Join(root, "antigravity")
	inner := filepath.Join(cli, "another-gemini-home")
	cliConv := filepath.Join(cli, "conversations")
	ideConv := filepath.Join(ide, "conversations")
	chat := filepath.Join("tmp", "project", "chats", "session-fixture.json")
	summaries := filepath.Join(ide, AntigravitySummariesDBName)
	const (
		nonData = ChangedPathNonData
		unknown = ChangedPathUnclassified
	)
	for _, tc := range []struct {
		name  string
		agent AgentType
		roots []string
		path  string
		want  ChangedPathRelevance
	}{
		{
			"cli_shm", AgentAntigravityCLI,
			[]string{cli},
			filepath.Join(cliConv, "session.db-shm"), nonData,
		},
		{
			"cli_db", AgentAntigravityCLI,
			[]string{cli},
			filepath.Join(cliConv, "session.db"), unknown,
		},
		{
			"cli_wal", AgentAntigravityCLI,
			[]string{cli},
			filepath.Join(cliConv, "session.db-wal"), unknown,
		},
		{
			"cli_outside", AgentAntigravityCLI,
			[]string{cli},
			filepath.Join(root, "conversations", "session.db-shm"), unknown,
		},
		{
			"cli_unknown", AgentAntigravityCLI,
			[]string{cli},
			filepath.Join(cliConv, "unknown.bin"), unknown,
		},
		{
			"ide_shm", AgentAntigravity,
			[]string{ide},
			filepath.Join(ideConv, "session.db-shm"), nonData,
		},
		{
			"ide_db", AgentAntigravity,
			[]string{ide},
			filepath.Join(ideConv, "session.db"), unknown,
		},
		{
			"ide_wal", AgentAntigravity,
			[]string{ide},
			filepath.Join(ideConv, "session.db-wal"), unknown,
		},
		{"ide_titles_shm", AgentAntigravity, []string{ide}, summaries + "-shm", nonData},
		{"ide_titles_db", AgentAntigravity, []string{ide}, summaries, unknown},
		{"ide_titles_wal", AgentAntigravity, []string{ide}, summaries + "-wal", unknown},
		{
			"ide_unknown", AgentAntigravity,
			[]string{ide},
			filepath.Join(ideConv, "unknown.bin"), unknown,
		},
		{
			"gemini_nested_cli_shm", AgentGemini,
			[]string{root},
			filepath.Join(cliConv, "session.db-shm"), nonData,
		},
		{
			"gemini_nested_cli_db", AgentGemini,
			[]string{root},
			filepath.Join(cliConv, "session.db"), nonData,
		},
		{
			"gemini_nested_ide_wal", AgentGemini,
			[]string{root},
			filepath.Join(ideConv, "session.db-wal"), nonData,
		},
		{"gemini_nested_ide_titles", AgentGemini, []string{root}, summaries, nonData},
		{"gemini_session", AgentGemini, []string{root}, filepath.Join(root, chat), unknown},
		{"gemini_tmp_dir", AgentGemini, []string{root}, filepath.Join(root, "tmp"), unknown},
		{
			"gemini_projects", AgentGemini,
			[]string{root},
			filepath.Join(root, "projects.json"), unknown,
		},
		{
			"gemini_trusted_folders", AgentGemini,
			[]string{root},
			filepath.Join(root, "trustedFolders.json"), unknown,
		},
		{
			"gemini_explicit_inner_root", AgentGemini,
			[]string{root, inner},
			filepath.Join(inner, chat), unknown,
		},
		{
			"gemini_outside_roots", AgentGemini,
			[]string{cli},
			filepath.Join(ide, "session.db-shm"), unknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, ok := NewProvider(tc.agent, ProviderConfig{Roots: tc.roots})
			require.True(t, ok)
			relevance, err := ResolveChangedPathRelevance(t.Context(), provider,
				ChangedPathRequest{Path: tc.path, EventKind: "write"})
			require.NoError(t, err)
			assert.Equal(t, tc.want, relevance)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = ResolveChangedPathRelevance(ctx, provider, ChangedPathRequest{Path: tc.path})
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}
