//go:build pgtest

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawRolloutCommandScopes(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "")
	body := fmt.Sprintf("[pg.provision]\nurl = %q\nschema = %q\nraw_tenant = %q\n", os.Getenv("TEST_PG_URL"), cfg.PG.Schema, cfg.PG.RawTenant)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(body), 0600))
	for _, args := range [][]string{{"shadow", "provision"}, {"shadow", "provision", "--device", "device-'alpha", "--provider", "codex"}} {
		_, err := executeCommand(newPGRawRolloutCommand(), args...)
		require.NoError(t, err)
	}
	output, err := executeCommand(newPGRawRolloutCommand(), "list", "provision", "--device", "device-'alpha")
	require.NoError(t, err)
	require.Contains(t, output, "\"device-'alpha\"\t\"codex\"\tshadow\t0\t0\t0\tnull")
	_, err = executeCommand(newPGRawRolloutCommand(), "clear", "provision")
	require.ErrorContains(t, err, "clear narrower rollout scopes")
	for _, args := range [][]string{{"clear", "provision", "--device", "device-'alpha", "--provider", "codex"}, {"clear", "provision"}} {
		_, err := executeCommand(newPGRawRolloutCommand(), args...)
		require.NoError(t, err)
	}
}

// The current config type cannot attest that embeddings are disabled. The real
// owner command must fail closed even with a waiver; runtime role rejection is
// independently observable through the same command entry point.
func TestRawRolloutActivationConfigPrerequisite(t *testing.T) {
	cfg, _ := hostedRuntimeConfig(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", cfg.DataDir)
	t.Setenv("AGENTSVIEW_PG_URL", "")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "")
	body := fmt.Sprintf("default_pg = \"provision\"\n[pg.provision]\nurl = %q\nschema = %q\nraw_tenant = %q\n[pg.runtime]\nurl = %q\nschema = %q\nraw_tenant = %q\n", os.Getenv("TEST_PG_URL"), cfg.PG.Schema, cfg.PG.RawTenant, cfg.PG.URL, cfg.PG.Schema, cfg.PG.RawTenant)
	require.NoError(t, os.WriteFile(filepath.Join(cfg.DataDir, "config.toml"), []byte(body), 0600))
	_, err := executeCommand(newPGRawRolloutCommand(), "activate", "provision", "--accept-differences")
	require.ErrorContains(t, err, "unknown")
	_, err = executeCommand(newPGRawRolloutCommand(), "activate", "runtime", "--accept-differences")
	require.ErrorContains(t, err, "owner")
}
