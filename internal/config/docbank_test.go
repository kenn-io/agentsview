package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankConfig(t *testing.T) {
	for _, tc := range []struct {
		url, web string
		valid    bool
	}{
		{"https://example.test/deploy", "", true},
		{"http://127.0.0.1:8080/prefix", "http://docbank-test.docbank.localhost:8081", true},
		{"http://[::1]:8080", "https://example.test/", true},
		{"http://localhost:8080", "http://localhost:8081", true},
		{"http://example.test", "", false},
		{"ftp://localhost", "", false},
		{"https://user@example.test", "", false},
		{"https://example.test/?key=secret", "", false},
		{"https://example.test/#fragment", "", false},
		{"https://example.test/a/../b", "", false},
		{"https://example.test", "http://evillocalhost", false},
		{"https://example.test", "http://localhost.example", false},
		{"https://example.test", "http://a..localhost", false},
		{"https://example.test", "http://-bad.localhost", false},
		{"https://example.test", "https://example.test/prefix", false},
		{"https://example.test", "http://user@localhost", false},
	} {
		t.Run(tc.url+tc.web, func(t *testing.T) {
			c := DocbankIntegration{Enabled: true, URL: tc.url, WebURL: tc.web, APIKeyEnv: "DOCBANK_TEST_KEY"}
			assert.Equal(t, tc.valid, c.Validate() == nil)
		})
	}
	require.NoError(t, (DocbankIntegration{URL: "bad"}).Validate())
	for _, c := range []DocbankIntegration{
		{Enabled: true, URL: "https://example.test"},
		{Enabled: true, URL: "https://example.test", APIKeyEnv: "KEY", APIKeyFile: "key"},
	} {
		require.Error(t, c.Validate())
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[integrations.docbank]\nenabled=true\nurl='https://example.test/prefix'\napi_key_env='DOCBANK_TEST_KEY'\n"), 0o600))
	cfg := Config{DataDir: dir}
	require.NoError(t, cfg.loadFileReadOnly())
	assert.True(t, cfg.Integrations.Docbank.Enabled)
	assert.Equal(t, "https://example.test/prefix", cfg.Integrations.Docbank.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[integrations.docbank]\nenabled=true\nurl='http://example.test'\napi_key_env='KEY'\n"), 0o600))
	require.NoError(t, cfg.loadFileReadOnly())
	require.Error(t, cfg.Integrations.Docbank.Validate())
}
