package db_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSourceMachineAliasesPreserveLocalProviderResolution(t *testing.T) {
	const sessionID = "11111111-2222-4333-8444-555555555555"
	for _, tc := range []struct {
		agent parser.AgentType
		path  string
	}{
		{parser.AgentCursor, filepath.Join("unknown-project", "agent-transcripts", sessionID+".jsonl")},
		{parser.AgentAntigravityCLI, filepath.Join("conversations", sessionID+".pb")},
	} {
		t.Run(string(tc.agent), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.path)
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			require.NoError(t, os.WriteFile(path, []byte("{}\n"), 0o600))
			dbPath := filepath.Join(t.TempDir(), "sessions.db")
			database := dbtest.OpenTestDBAt(t, dbPath)
			readOnly, err := db.OpenReadOnly(dbPath)
			require.NoError(t, err)
			defer readOnly.Close()
			cfg := config.Config{
				InstallationID: "installation-a", LocalMachineName: "old-host",
				SourceMachines: map[parser.AgentType]map[string]string{
					tc.agent: {root: "old-host"},
				},
			}
			// The local cwd state differs by platform; only remoteness is under test.
			for _, remote := range []bool{true, false} {
				require.NoError(t, readOnly.ApplyMachineAliases(t.Context(), &cfg))
				provider, ok := parser.NewProvider(tc.agent, parser.ProviderConfig{
					Roots: []string{root}, Machine: cfg.InstallationID,
					SourceMachines: cfg.SourceMachines[tc.agent],
				})
				require.True(t, ok)
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				require.Len(t, sources, 1)
				assert.Equal(t, remote, sources[0].CwdResolution.State == parser.SourceCwdRemote)
				require.NoError(t, database.SetSyncState(db.MachineAliasKeyPrefix+"old-host", "installation-a"))
			}
		})
	}
}
