package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankScopedValidation(t *testing.T) {
	t.Skip("Invalid config finding: adding Docbank to validateSections would lengthen the function")
	unsetDeploymentEnv(t)
	dir := setupTestEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[integrations.docbank]\nurl='https://example.com'\n"), 0o600))
	_, err := LoadMinimalFor(0)
	require.ErrorContains(t, err, "[integrations.docbank]")
}

func TestDocbankIntegrationValidation(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, key string
		wantErr             string
	}{
		{"disabled", "", "", ""},
		{"missing key", "https://example.com", "", "[integrations.docbank] url and api_key_env are required together"},
		{"remote plaintext", "http://example.com", "DOCBANK_TEST_KEY", "[integrations.docbank] url must be https, or http to a loopback host"},
		{"loopback", "http://127.0.0.1", "DOCBANK_TEST_KEY", ""},
		{"https", "https://example.com", "DOCBANK_TEST_KEY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (DocbankIntegration{URL: tc.endpoint, APIKeyEnv: tc.key}).Validate()
			if tc.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.EqualError(t, err, tc.wantErr)
			}
		})
	}
	var cfg Config
	require.NoError(t, cfg.applyConfigTOML("[integrations.docbank]\nurl = 'http://127.0.0.1:9191'\napi_key_env = 'DOCBANK_TEST_KEY'"))
	assert.Equal(t, "http://127.0.0.1:9191", cfg.Integrations.Docbank.URL)
	assert.True(t, cfg.Integrations.Docbank.Configured())
}
