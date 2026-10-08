package db_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestUsageGroups(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	conn, err := sql.Open("sqlite3", database.Path())
	require.NoError(t, err)
	defer conn.Close()
	dbtest.SeedUsageGroups(t, conn)
	dbtest.AssertUsageGroups(t, database)
}

func TestHermesCronGroupsKeepProfilesSeparate(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), ".hermes", "profiles")
	for _, profile := range []string{"profile-a", "profile-b"} {
		sessions := filepath.Join(profiles, profile, "sessions")
		require.NoError(t, os.MkdirAll(sessions, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(sessions, "cron_digest_20261007_120000.jsonl"), []byte("{\"role\":\"session_meta\",\"platform\":\"cron\"}\n{\"role\":\"user\",\"content\":\"Run digest\"}\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(sessions, "session_cron_digest_20261008_120000.json"), []byte(`{"platform":"cron","messages":[{"role":"user","content":"Run digest"}]}`), 0o644))
	}
	provider, ok := parser.NewProvider(parser.AgentHermes, parser.ProviderConfig{Roots: []string{profiles}, Machine: "local"})
	require.True(t, ok)
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 4)
	var entries []db.TopSessionEntry
	for _, source := range sources {
		outcome, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, outcome.Results, 1)
		session := outcome.Results[0].Result.Session
		entries = append(entries, db.TopSessionEntry{SessionID: session.ID, Project: session.Project, GroupKey: session.GroupKey, InputTokens: 10})
	}
	groups, err := db.GroupTopSessions(entries, 100, db.TopSessionsSortTokens, db.UsageTokenTypesAll)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.ElementsMatch(t, []string{
		filepath.Join(profiles, "profile-a", "state.db") + "#digest",
		filepath.Join(profiles, "profile-b", "state.db") + "#digest",
	}, []string{groups[0].GroupKey, groups[1].GroupKey})
	for _, group := range groups {
		assert.Equal(t, "hermes-cron", group.Project)
		assert.Equal(t, 20, group.InputTokens)
	}
}
