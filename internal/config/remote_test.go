package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoteConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Chdir(cwd)
	for _, tc := range []struct{ name, file, env, want, urlEnv, wantURL string }{
		{"file relative", "file.token", "", filepath.Join(dir, "file.token"), "", "https://file.example"},
		{"env relative", "file.token", "env.token", filepath.Join(cwd, "env.token"), "https://env.example", "https://env.example"},
		{"nested file", "nested/file.token", "", filepath.Join(dir, "nested", "file.token"), "https://env.example", "https://env.example"},
		{"parent file", "../file.token", "", filepath.Join(dir, "..", "file.token"), "", "https://file.example"},
		{"file absolute", filepath.Join(dir, "absolute-file.token"), "", filepath.Join(dir, "absolute-file.token"), "", "https://file.example"},
		{"env absolute", "file.token", filepath.Join(cwd, "absolute-env.token"), filepath.Join(cwd, "absolute-env.token"), "", "https://file.example"},
		{"file home", "~/home-file.token", "", filepath.Join(home, "home-file.token"), "", "https://file.example"},
		{"env home", "~/home-file.token", "~/home-env.token", filepath.Join(home, "home-env.token"), "", "https://file.example"},
		{"no token", "", "", "", "", "https://file.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(fmt.Sprintf("[remote]\nurl='https://file.example'\ntoken_file='%s'\nspoke_host='workstation'\nallow_insecure_http=true\n", filepath.ToSlash(tc.file))), 0o600))
			t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", tc.env)
			t.Setenv("AGENTSVIEW_REMOTE_URL", tc.urlEnv)
			remote, err := LoadRemote()
			require.NoError(t, err)
			require.Equal(t, RemoteConfig{URL: tc.wantURL, TokenFile: tc.want, SpokeHost: "workstation", AllowInsecureHTTP: true}, remote)
		})
	}
}

func TestRemoteIgnoresLocalValidation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	t.Setenv("AGENTSVIEW_REMOTE_URL", "")
	t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(`[remote]
url="https://hub.example"
token_file="missing.token"
[vector.embeddings]
max_batch_tokens=123
`), 0o600))
	remote, err := LoadRemote()
	require.NoError(t, err)
	require.Equal(t, "https://hub.example", remote.URL)
	_, err = LoadReadOnly()
	require.ErrorContains(t, err, "max_batch_tokens")
}

func TestRemoteReadOnlyFormats(t *testing.T) {
	for _, tc := range []struct {
		name, toml, json, url, token string
		bad                          bool
	}{
		{name: "legacy JSON", json: `{"remote":{"url":"https://legacy.example","token_file":"legacy.token"},"vector":{"embeddings":{"max_batch_tokens":123}}}`, url: "https://legacy.example", token: "legacy.token"},
		{name: "TOML wins", toml: "[remote]\nurl='https://toml.example'\ntoken_file='toml.token'\n", json: `{"remote":{"url":"https://legacy.example","token_file":"legacy.token"}}`, url: "https://toml.example", token: "toml.token"},
		{name: "missing config"},
		{name: "malformed TOML", toml: "[remote", bad: true},
		{name: "malformed JSON", json: `{"remote":`, bad: true},
		{name: "invalid remote field", toml: "[remote]\nurl=123\n", bad: true},
		{name: "legacy trailing JSON", json: `{"remote":{}} {}`, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AGENTSVIEW_DATA_DIR", dir)
			t.Setenv("AGENTSVIEW_REMOTE_URL", "")
			t.Setenv("AGENTSVIEW_REMOTE_TOKEN_FILE", "")
			if tc.toml != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.toml), 0o600))
			}
			if tc.json != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.json), 0o600))
			}
			// A full local loader would reject this identity. Remote selection must never read it.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "telemetry-install-id"), []byte("invalid"), 0o600))
			before, err := os.ReadDir(dir)
			require.NoError(t, err)
			remote, err := LoadRemote()
			if tc.bad {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.url, remote.URL)
				want := ""
				if tc.token != "" {
					want = filepath.Join(dir, tc.token)
				}
				require.Equal(t, want, remote.TokenFile)
			}
			after, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Equal(t, before, after)
			for name, want := range map[string]string{"config.toml": tc.toml, "config.json": tc.json, "telemetry-install-id": "invalid"} {
				if want == "" {
					continue
				}
				data, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				require.Equal(t, want, string(data))
			}
		})
	}
}
