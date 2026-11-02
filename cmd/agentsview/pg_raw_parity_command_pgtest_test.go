//go:build pgtest

package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawparity"
)

// No worker or subprocess is needed: even an empty hosted scope is a report,
// not a command failure.
func TestPGRawParityRuntimeCommand(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	dir := testDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[pg.parity]\nurl = %q\nschema = %q\nraw_tenant = %q\n", cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant)), 0600))
	for _, args := range [][]string{{"parity", "--json"}, {"parity", "--device", "missing", "--provider", "codex"}} {
		cmd := newPGRawParityCommand()
		var out, summary bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&summary)
		cmd.SetArgs(args)
		require.NoError(t, cmd.Execute())
		if len(args) == 2 {
			var report rawparity.Report
			require.NoError(t, json.Unmarshal(out.Bytes(), &report))
			assert.False(t, report.Pass)
			assert.Contains(t, summary.String(), "pass=false")
		} else {
			assert.Contains(t, out.String(), "pass=false")
		}
	}
}

func TestPGRawParityRejectsOwnerRole(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	dir := testDataDir(t)
	// Reuse only the isolated fixture's owner credentials, never a live target.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[pg.owner]\nurl = %q\nschema = %q\nraw_tenant = %q\n", os.Getenv("TEST_PG_URL"), cfg.PG.Schema, cfg.PG.RawTenant)), 0600))
	cmd := newPGRawParityCommand()
	var out, summary bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&summary)
	cmd.SetArgs([]string{"owner", "--json"})
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	require.ErrorContains(t, cmd.Execute(), "hosted runtime requires a restricted non-owner role")
	assert.Empty(t, out.String())
	assert.Empty(t, summary.String())
}
