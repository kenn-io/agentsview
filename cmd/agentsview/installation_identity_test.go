package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestSessionSyncUsesInstallationIdentityAndPreservesHistoricalNames(t *testing.T) {
	isolateDirectCLISources(t)
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_TELEMETRY_ENABLED", "0")
	const id = "0123456789abcdef0123456789abcdef"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte(id), 0o600))
	root := filepath.Join(dir, "claude", "project")
	require.NoError(t, os.MkdirAll(root, 0o700))
	path := filepath.Join(root, "current.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-07-12T00:00:00Z", "installation identity check").String()), 0o600))

	for _, name := range []string{"Laptop", "Renamed Laptop"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
			[]byte("local_machine_name = \""+name+"\"\n"), 0o600))
		cfg, err := config.LoadMinimal()
		require.NoError(t, err)
		cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentClaude: {filepath.Dir(root)}}
		svc, closeService, err := syncService(cfg, transport{Mode: transportDirect})
		require.NoError(t, err)
		detail, err := svc.Sync(t.Context(), service.SyncInput{Path: path})
		closeService()
		require.NoError(t, err)
		assert.Equal(t, id, detail.Machine)

		database, err := openDB(cfg)
		require.NoError(t, err)
		if name == "Laptop" {
			require.NoError(t, database.UpsertSession(db.Session{
				ID: "historical", Project: "project", Agent: "claude", Machine: "old-host",
			}))
		}
		labels, err := database.GetMachineLabels(t.Context())
		require.NoError(t, err)
		assert.Equal(t, map[string]string{id: name}, labels)
		historical, err := database.GetSession(t.Context(), "historical")
		require.NoError(t, err)
		require.NotNil(t, historical)
		assert.Equal(t, "old-host", historical.Machine)
		require.NoError(t, database.Close())
	}
}
