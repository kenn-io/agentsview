package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeRawConfig(t *testing.T, dir, text string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, configFileName), []byte(text), 0o600))
}

func TestCodexCloudDefaults(t *testing.T) {
	dir := setupTestEnv(t)
	writeRawConfig(t, dir, "[agents.codex-cloud]\n")
	cfg, err := LoadMinimal()
	require.NoError(t, err)
	got := cfg.CodexCloud
	assert.True(t, got.Present)
	assert.False(t, got.Enabled)
	assert.True(t, got.LiveTokens)
	assert.Equal(t, "wss://codex-cloud-backend.chatgpt.com/", got.Endpoint.String())
	assert.Equal(t, "https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2", got.UsageEndpoint.String())
	assert.Nil(t, got.Err)
}

func TestCodexCloudAbsentTable(t *testing.T) {
	setupTestEnv(t)
	cfg, err := LoadMinimal()
	require.NoError(t, err)
	assert.False(t, cfg.CodexCloud.Present)
	assert.False(t, cfg.CodexCloud.Enabled)
	assert.Nil(t, cfg.CodexCloud.Err)
}

func TestCodexCloudSettings(t *testing.T) {
	for _, tt := range []struct {
		name        string
		body        string
		wantErrKey  string
		wantUnknown bool
		check       func(t *testing.T, got CodexCloudConfig)
	}{
		{
			name: "enabled with live tokens off",
			body: "enabled = true\nlive_tokens = false\n",
			check: func(t *testing.T, got CodexCloudConfig) {
				t.Helper()
				assert.True(t, got.Enabled)
				assert.False(t, got.LiveTokens)
			},
		},
		{name: "dirs is unknown", body: "dirs = [\"/x\"]\n", wantErrKey: "dirs", wantUnknown: true},
		{name: "homes is unknown", body: "homes = [\"/x\"]\n", wantErrKey: "homes", wantUnknown: true},
		{name: "non-bool enabled", body: "enabled = \"yes\"\n", wantErrKey: "enabled"},
		{name: "non-bool live_tokens", body: "live_tokens = 1\n", wantErrKey: "live_tokens"},
		{name: "clear-text websocket", body: "endpoint = \"ws://codex-cloud-backend.chatgpt.com/\"\n", wantErrKey: "endpoint"},
		{name: "foreign websocket host", body: "endpoint = \"wss://evil.example/\"\n", wantErrKey: "endpoint"},
		{name: "lookalike host", body: "endpoint = \"wss://chatgpt.com.evil.example/\"\n", wantErrKey: "endpoint"},
		{name: "suffix without dot", body: "endpoint = \"wss://notchatgpt.com/\"\n", wantErrKey: "endpoint"},
		{name: "websocket endpoint with https", body: "endpoint = \"https://chatgpt.com/\"\n", wantErrKey: "endpoint"},
		{name: "usage over http", body: "usage_endpoint = \"http://chatgpt.com/x\"\n", wantErrKey: "usage_endpoint"},
		{name: "usage on foreign host", body: "usage_endpoint = \"https://evil.example/x\"\n", wantErrKey: "usage_endpoint"},
		{name: "credentials in url", body: "usage_endpoint = \"https://user:pw@chatgpt.com/x\"\n", wantErrKey: "usage_endpoint"},
		{
			name: "loopback clear text",
			body: "endpoint = \"ws://127.0.0.1:9/\"\nusage_endpoint = \"http://localhost:9/q\"\n",
			check: func(t *testing.T, got CodexCloudConfig) {
				t.Helper()
				assert.Equal(t, "ws://127.0.0.1:9/", got.Endpoint.String())
				assert.Equal(t, "http://localhost:9/q", got.UsageEndpoint.String())
			},
		},
		{
			name: "openai subdomain",
			body: "usage_endpoint = \"https://API.OpenAI.com/x\"\n",
			check: func(t *testing.T, got CodexCloudConfig) {
				t.Helper()
				assert.Equal(t, "https://API.OpenAI.com/x", got.UsageEndpoint.String())
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := setupTestEnv(t)
			writeRawConfig(t, dir, "[agents.codex-cloud]\n"+tt.body)
			cfg, err := LoadMinimal()
			require.NoError(t, err, "a bad codex-cloud table must not fail the whole load")
			got := cfg.CodexCloud
			if tt.wantErrKey == "" {
				require.Nil(t, got.Err)
				tt.check(t, got)
				return
			}
			require.NotNil(t, got.Err)
			assert.Equal(t, tt.wantErrKey, got.Err.Key)
			assert.Equal(t, tt.wantUnknown, got.Err.Unknown)
			assert.False(t, got.Enabled, "an invalid table leaves fetching off")
		})
	}
}

func TestCodexCloudErrorMessages(t *testing.T) {
	path := filepath.FromSlash("/data/config.toml")
	unknown := &CodexCloudConfigError{Path: path, Key: "dirs", Unknown: true}
	assert.Equal(t,
		path+" has an unknown setting dirs under [agents.codex-cloud]. Remove it or correct its name. Cloud import is off until then.",
		unknown.Error())
	bad := &CodexCloudConfigError{Path: path, Key: "enabled", Reason: "expected true or false"}
	assert.Equal(t,
		path+" has an invalid value for enabled under [agents.codex-cloud]: expected true or false. Cloud import is off until then.",
		bad.Error())
}

func TestCodexCloudErrorKeepsOtherAgents(t *testing.T) {
	dir := setupTestEnv(t)
	other := filepath.Join(dir, "claude-projects")
	writeRawConfig(t, dir, "[agents.codex-cloud]\nhomes = 3\n\n[agents.claude]\ndirs = [\""+filepath.ToSlash(other)+"\"]\n")
	cfg, err := LoadMinimal()
	require.NoError(t, err)
	require.NotNil(t, cfg.CodexCloud.Err)
	assert.Equal(t, []string{other}, cfg.AgentDirs["claude"])
}
