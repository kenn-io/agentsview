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
	inner := filepath.Join(cli, "another-gemini-home")
	for _, tc := range []struct {
		name  string
		agent AgentType
		roots []string
		path  string
		want  ChangedPathRelevance
	}{
		{"cli_shm", AgentAntigravityCLI, []string{cli}, filepath.Join(cli, "conversations", "session.db-shm"), ChangedPathNonData},
		{"cli_db", AgentAntigravityCLI, []string{cli}, filepath.Join(cli, "conversations", "session.db"), ChangedPathUnclassified},
		{"cli_wal", AgentAntigravityCLI, []string{cli}, filepath.Join(cli, "conversations", "session.db-wal"), ChangedPathUnclassified},
		{"cli_outside", AgentAntigravityCLI, []string{cli}, filepath.Join(root, "conversations", "session.db-shm"), ChangedPathUnclassified},
		{"cli_unknown", AgentAntigravityCLI, []string{cli}, filepath.Join(cli, "conversations", "unknown.bin"), ChangedPathUnclassified},
		{"gemini_nested_shm", AgentGemini, []string{root}, filepath.Join(cli, "conversations", "session.db-shm"), ChangedPathNonData},
		{"gemini_nested_db", AgentGemini, []string{root}, filepath.Join(cli, "conversations", "session.db"), ChangedPathNonData},
		{"gemini_nested_wal", AgentGemini, []string{root}, filepath.Join(cli, "conversations", "session.db-wal"), ChangedPathNonData},
		{"gemini_session", AgentGemini, []string{root}, filepath.Join(root, "tmp", "project", "chats", "session-fixture.json"), ChangedPathUnclassified},
		{"gemini_explicit_inner_root", AgentGemini, []string{root, inner}, filepath.Join(inner, "tmp", "project", "chats", "session-fixture.json"), ChangedPathUnclassified},
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
