package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/mcpdiscovery"
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
	"AGENTSVIEW_EMBEDDINGS_BATCH_SIZE",
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

func TestDeploymentArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		args []string
		want string
	}{
		{name: "serve", mode: "serve", args: []string{"--no-browser", "--help"}, want: "agentsview serve [flags]"},
		{name: "pg-serve", mode: "pg-serve", args: []string{"--port=9000", "--help"}, want: "agentsview pg serve [flags]"},
		{name: "explicit subcommand", mode: "pg-serve", args: []string{"mcp", "--help"}, want: "agentsview mcp [flags]"},
		{name: "root version", mode: "serve", args: []string{"--version"}, want: "agentsview"},
		{name: "root help", mode: "serve", args: []string{"--help"}, want: "agentsview <command> [flags]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDeploymentEnv(t)
			t.Setenv("AGENTSVIEW_MODE", tc.mode)
			var stdout, stderr bytes.Buffer
			err := executeCLIWithLegacyFlagCompat(tc.args, &stdout, &stderr)
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

func TestMCPDeploymentTokenDoesNotOpenNetworkBind(t *testing.T) {
	dir := isolateDeploymentEnv(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	setImageEnv(t)
	token := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(token, []byte("listener-secret\n"), 0o600))
	t.Setenv("AGENTSVIEW_AUTH_TOKEN_FILE", token)

	cmd := newMCPCommand()
	cmd.SetContext(t.Context())
	require.NoError(t, cmd.ParseFlags([]string{"--http", "0.0.0.0:0"}))
	require.ErrorContains(t, cmd.RunE(cmd, nil), "--http-allow-insecure")

	cmd = newMCPCommand()
	ctx, cancel := context.WithCancel(t.Context())
	cmd.SetContext(ctx)
	require.NoError(t, cmd.ParseFlags([]string{"--http", "0.0.0.0:0", "--http-allow-insecure"}))
	done := make(chan error, 1)
	go func() { done <- cmd.RunE(cmd, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			require.Fail(t, "MCP listener did not stop after cancellation")
		}
	})

	var endpoint string
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		listeners, err := mcpdiscovery.List(filepath.Join(dir, "mcp"))
		if assert.NoError(c, err) && assert.Len(c, listeners, 1) {
			endpoint = listeners[0].URL
		}
	}, 10*time.Second, 10*time.Millisecond)

	client := &http.Client{Timeout: 10 * time.Second}
	for _, tc := range []struct {
		name   string
		token  string
		status int
	}{
		{name: "missing token", status: http.StatusUnauthorized},
		{name: "wrong token", token: "wrong-secret", status: http.StatusUnauthorized},
		{name: "file token", token: "listener-secret", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			assert.Equal(t, tc.status, resp.StatusCode)
		})
	}
}
