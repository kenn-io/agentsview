package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
)

var deploymentTestEnv = []string{
	"AGENTSVIEW_MODE",
	"PG_SERVE",
	"AGENTSVIEW_HOST",
	"AGENTSVIEW_REQUIRE_AUTH",
	"AGENTSVIEW_NO_BROWSER",
	"AGENTSVIEW_PG_ALLOW_INSECURE",
	"AGENTSVIEW_AUTH_TOKEN",
	"AGENTSVIEW_AUTH_TOKEN_FILE",
	"AGENTSVIEW_EMBEDDINGS_ENDPOINT",
	"AGENTSVIEW_EMBEDDINGS_API_KEY_FILE",
}

// isolateDeploymentEnv unsets every deployment variable and points the data
// directory at a fresh temp dir; the host environment is restored afterwards.
func isolateDeploymentEnv(t *testing.T) string {
	t.Helper()
	for _, name := range deploymentTestEnv {
		t.Setenv(name, "")
		require.NoError(t, os.Unsetenv(name))
	}
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	return dir
}

// setImageEnv sets the environment the Dockerfile bakes into the image.
func setImageEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AGENTSVIEW_MODE", "serve")
	t.Setenv("AGENTSVIEW_HOST", "0.0.0.0")
	t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "true")
	t.Setenv("AGENTSVIEW_NO_BROWSER", "true")
}

// parseCommand resolves argv to its subcommand and parses its flags without
// running it.
func parseCommand(t *testing.T, args []string) *cobra.Command {
	t.Helper()
	cmd, rest, err := newRootCommand().Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(rest))
	return cmd
}

func TestDeploymentServeFlagsKeepImageDefaults(t *testing.T) {
	isolateDeploymentEnv(t)
	setImageEnv(t)

	args, err := deploymentArgs([]string{"--port", "9000"})
	require.NoError(t, err)
	cmd := parseCommand(t, args)
	require.Equal(t, "serve", cmd.Name())

	cfg, err := config.LoadPFlags(cmd.Flags())
	require.NoError(t, err)
	assert.Equal(t, "0.0.0.0", cfg.Host, "a user flag must not drop the image host")
	assert.True(t, cfg.NoBrowser, "a user flag must not drop the image no-browser default")
	assert.Equal(t, 9000, cfg.Port)
	assert.True(t, cfg.RequireAuth)
	require.NoError(t, validateServeConfig(cfg))
}

func TestDeploymentArgs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		pgServe string
		args    []string
		want    string
		wantErr string
	}{
		{name: "serve", mode: "serve", args: []string{"--no-browser", "--help"}, want: "agentsview serve [flags]"},
		{name: "pg-serve", mode: "pg-serve", args: []string{"--port=9000", "--help"}, want: "agentsview pg serve [flags]"},
		{name: "PG_SERVE under mode", mode: "serve", pgServe: "1", args: []string{"--port", "9000", "--help"}, want: "agentsview pg serve [flags]"},
		{name: "explicit subcommand", mode: "pg-serve", args: []string{"mcp", "--help"}, want: "agentsview mcp [flags]"},
		{name: "root version", mode: "serve", args: []string{"--version"}, want: "agentsview"},
		{name: "root help", mode: "serve", args: []string{"--help"}, want: "agentsview <command> [flags]"},
		{name: "invalid mode", mode: "daemon", args: []string{"--port", "9000"}, wantErr: "AGENTSVIEW_MODE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDeploymentEnv(t)
			t.Setenv("AGENTSVIEW_MODE", tc.mode)
			if tc.pgServe != "" {
				t.Setenv("PG_SERVE", tc.pgServe)
			}
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, stdout.String(), tc.want)
			if tc.name == "root help" || tc.name == "root version" {
				assert.NotContains(t, stdout.String(), "agentsview serve [flags]")
			}
		})
	}

	t.Run("empty argv", func(t *testing.T) {
		isolateDeploymentEnv(t)
		t.Setenv("AGENTSVIEW_MODE", "pg-serve")
		got, err := deploymentArgs(nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"pg", "serve"}, got)
		t.Setenv("AGENTSVIEW_MODE", "serve")
		got, err = deploymentArgs([]string{})
		require.NoError(t, err)
		assert.Equal(t, []string{"serve"}, got)
	})
}

func TestDeploymentArgsIgnoredWithoutMode(t *testing.T) {
	for _, mode := range []*string{nil, new("  ")} {
		isolateDeploymentEnv(t)
		if mode != nil {
			t.Setenv("AGENTSVIEW_MODE", *mode)
		}
		t.Setenv("PG_SERVE", "on")
		t.Setenv("AGENTSVIEW_HOST", "192.0.2.10")
		t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "maybe")
		t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", "/nonexistent/x")
		for _, args := range [][]string{nil, {}, {"--port", "9000"}, {"serve"}, {"--version"}} {
			got, err := deploymentArgs(args)
			require.NoError(t, err)
			assert.Equal(t, args, got)
		}
		var stdout, stderr bytes.Buffer
		require.NoError(t, executeCLIWithLegacyFlagCompat(
			[]string{"--help"}, &stdout, &stderr))
		assert.Contains(t, stdout.String(), "agentsview <command> [flags]")
		// Without the mode a leading serve flag reaches the root command,
		// exactly as on a desktop install.
		err := executeCLIWithLegacyFlagCompat(
			[]string{"--no-browser", "--help"}, &stdout, &stderr)
		require.ErrorContains(t, err, "unknown flag: --no-browser")
	}
}

func TestValidateServeConfigDeploymentHostRequiresAuth(t *testing.T) {
	t.Run("env host without auth", func(t *testing.T) {
		isolateDeploymentEnv(t)
		t.Setenv("AGENTSVIEW_MODE", "serve")
		t.Setenv("AGENTSVIEW_HOST", "192.0.2.10")
		cfg, err := config.LoadPFlags(parseCommand(t, []string{"serve"}).Flags())
		require.NoError(t, err)
		assert.False(t, cfg.HostExplicit)
		err = validateServeConfig(cfg)
		require.ErrorContains(t, err, "AGENTSVIEW_REQUIRE_AUTH=true")
		assert.Contains(t, err.Error(), "192.0.2.10")
	})

	t.Run("env host with auth", func(t *testing.T) {
		isolateDeploymentEnv(t)
		t.Setenv("AGENTSVIEW_MODE", "serve")
		t.Setenv("AGENTSVIEW_HOST", "192.0.2.10")
		t.Setenv("AGENTSVIEW_REQUIRE_AUTH", "true")
		cfg, err := config.LoadPFlags(parseCommand(t, []string{"serve"}).Flags())
		require.NoError(t, err)
		require.NoError(t, validateServeConfig(cfg))
	})

	t.Run("explicit host flag", func(t *testing.T) {
		isolateDeploymentEnv(t)
		t.Setenv("AGENTSVIEW_MODE", "serve")
		cfg, err := config.LoadPFlags(
			parseCommand(t, []string{"serve", "--host", "192.0.2.10"}).Flags())
		require.NoError(t, err)
		assert.True(t, cfg.HostExplicit)
		require.NoError(t, validateServeConfig(cfg))
	})
}

func TestDeploymentEnvRemoteServe(t *testing.T) {
	for _, args := range [][]string{{"pg", "serve"}, {"duckdb", "serve"}} {
		t.Run(args[0], func(t *testing.T) {
			dir := isolateDeploymentEnv(t)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"),
				[]byte("host = \"10.0.0.5\"\n"), 0o600))
			setImageEnv(t)
			cmd := parseCommand(t, args)
			cfg, err := config.LoadRemoteServePFlags(cmd.Flags())
			require.NoError(t, err)
			assert.Equal(t, "0.0.0.0", cfg.Host)
			assert.True(t, cfg.NoBrowser)
			assert.True(t, cfg.RequireAuth)
			assert.False(t, cfg.HostExplicit)
			require.NoError(t, validateServeConfig(cfg))

			cmd = parseCommand(t, append(args, "--port", "9000", "--host", "127.0.0.1"))
			cfg, err = config.LoadRemoteServePFlags(cmd.Flags())
			require.NoError(t, err)
			assert.Equal(t, "127.0.0.1", cfg.Host, "flags beat deployment env")
			assert.Equal(t, 9000, cfg.Port)
			assert.True(t, cfg.NoBrowser)
		})
	}
}

func TestMCPDeploymentTokenDoesNotOpenNetworkBind(t *testing.T) {
	dir := isolateDeploymentEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	setImageEnv(t)
	token := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(token, []byte("listener-secret\n"), 0o600))
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", token)

	run := func(args ...string) error {
		cmd := newMCPCommand()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		cmd.SetContext(ctx)
		require.NoError(t, cmd.ParseFlags(args))
		return cmd.RunE(cmd, nil)
	}
	err := run("--http", "[::]:0")
	require.ErrorContains(t, err, "--http-allow-insecure")
	require.NoError(t, run("--http", "[::]:0", "--http-allow-insecure"),
		"with the flag, the file token authenticates the listener")
}
