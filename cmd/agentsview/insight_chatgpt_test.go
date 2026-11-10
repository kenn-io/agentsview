package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/chatgpt"
)

func chatGPTCommandFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("AGENTSVIEW_DATA_DIR", filepath.Join(home, "data"))
	t.Setenv("AGENT_VIEWER_DATA_DIR", "")
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(home, "pi"))
	return filepath.Join(home, "data")
}

func TestInsightChatGPTStatusAndLogout(t *testing.T) {
	data := chatGPTCommandFixture(t)
	out, _, err := runInsightCommand(t, "insight", "chatgpt", "status", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"disconnected"}`, out)
	_, err = os.Stat(filepath.Join(data, "insights-chatgpt"))
	require.ErrorIs(t, err, os.ErrNotExist)
	dir := filepath.Join(data, "insights-chatgpt")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	fixture := fmt.Sprintf(`{"access_token":"private-access","refresh_token":"private-refresh","account_id":"private-account","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(fixture), 0o600))
	out, _, err = runInsightCommand(t, "insight", "chatgpt", "status", "--format", "json")
	require.NoError(t, err)
	assert.Contains(t, out, `"state":"connected"`)
	assert.NotContains(t, out, "private-")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"reconnect_required":true}`), 0o600))
	out, _, err = runInsightCommand(t, "insight", "chatgpt", "status")
	require.NoError(t, err)
	assert.Equal(t, "Log in again\n", out)
	out, _, err = runInsightCommand(t, "insight", "chatgpt", "status", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"reconnect_required"}`, out)
	out, _, err = runInsightCommand(t, "insight", "chatgpt", "logout")
	require.NoError(t, err)
	assert.Contains(t, out, "cleared")
	out, _, err = runInsightCommand(t, "insight", "chatgpt", "status", "--json")
	require.NoError(t, err)
	assert.JSONEq(t, `{"state":"disconnected"}`, out)
	_, err = os.Stat(filepath.Join(dir, ".lock"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(data, "sessions.db"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestInsightChatGPTRejectsRemoteFlagsBeforeLocalIO(t *testing.T) {
	for _, action := range []string{"login", "status", "logout"} {
		for _, flag := range []string{"--server", "--server-token-file"} {
			for _, before := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/%t", action, flag, before), func(t *testing.T) {
					data := chatGPTCommandFixture(t)
					// Invalid configuration must not mask the local-only target error.
					require.NoError(t, os.MkdirAll(data, 0o700))
					require.NoError(t, os.WriteFile(filepath.Join(data, "config.toml"), []byte("invalid = ["), 0o600))
					args := []string{"insight", "chatgpt", action}
					if before {
						args = append([]string{"insight", flag, ""}, args[1:]...)
					} else {
						args = append(args, flag, "")
					}
					_, _, err := runInsightCommand(t, args...)
					require.ErrorContains(t, err, "local-only")
					_, err = os.Stat(filepath.Join(data, "insights-chatgpt"))
					require.ErrorIs(t, err, os.ErrNotExist)
				})
			}
		}
	}
}

func TestInsightChatGPTHelp(t *testing.T) {
	chatGPTCommandFixture(t)
	out, _, err := runInsightCommand(t, "insight", "chatgpt", "--help")
	require.NoError(t, err)
	for _, name := range []string{"login", "status", "logout"} {
		assert.Contains(t, out, name)
	}
	out, _, err = runInsightCommand(t, "insight", "chatgpt", "login", "extra")
	require.Error(t, err)
	assert.Empty(t, out)
}

// The command adapter prints the device code explicitly and forwards context.
type fakeChatGPTLogin struct{ ctx context.Context }

func (f *fakeChatGPTLogin) Login(ctx context.Context, show func(chatgpt.DeviceCode) error) error {
	f.ctx = ctx
	return show(chatgpt.DeviceCode{URL: chatgpt.DeviceURL, Code: "ABCD-EFGH"})
}

func TestInsightChatGPTLoginOutput(t *testing.T) {
	cmd := newInsightCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	fake := &fakeChatGPTLogin{}
	require.NoError(t, runChatGPTLogin(cmd, fake))
	assert.Equal(t, t.Context(), fake.ctx)
	for _, text := range []string{chatgpt.DeviceURL, "ABCD-EFGH", "connected"} {
		assert.Contains(t, out.String(), text)
	}
}

func TestInsightChatGPTDoesNotMutateConfigurationRegression(t *testing.T) {
	for _, action := range []string{"status", "logout", "login"} {
		t.Run(action, func(t *testing.T) {
			data := chatGPTCommandFixture(t)
			require.NoError(t, os.MkdirAll(data, 0o700))
			original := []byte(`{"host":"127.0.0.1"}`)
			require.NoError(t, os.WriteFile(filepath.Join(data, "config.json"), original, 0o600))
			root := newRootCommand()
			var out bytes.Buffer
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs([]string{"insight", "chatgpt", action})
			if action == "login" {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				root.SetContext(ctx)
			} else {
				root.SetContext(t.Context())
			}
			err := root.Execute()
			if action == "login" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			got, err := os.ReadFile(filepath.Join(data, "config.json"))
			require.NoError(t, err)
			assert.Equal(t, original, got)
			entries, err := os.ReadDir(data)
			require.NoError(t, err)
			var names []string
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			if action == "logout" {
				assert.ElementsMatch(t, []string{"config.json", "insights-chatgpt"}, names)
			} else {
				assert.Equal(t, []string{"config.json"}, names)
			}
		})
	}
}
