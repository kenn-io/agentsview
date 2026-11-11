package config

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankIntegrationValidation(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, key string
		wantErr             string
	}{
		{"disabled", "", "", ""},
		{"missing key", "https://example.com", "", "[integrations.docbank] url and api_key_env are required together"},
		{"remote plaintext", "http://example.com", "DOCBANK_TEST_KEY", "[integrations.docbank] url must be https, or http to a loopback host; set allow_http = true to permit other HTTP hosts"},
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

func TestDocbankHTTPTransport(t *testing.T) {
	for _, field := range []string{"url", "web_url"} {
		for _, tc := range []struct {
			name, endpoint string
			allow, wantOK  bool
		}{
			{"container without opt-in", "http://docbank:8485", false, false},
			{"container with opt-in", "http://docbank:8485", true, true},
			{"public IP with opt-in", "http://8.8.8.8:8485", true, true},
			{"loopback without opt-in", "http://127.0.0.1:8485", false, true},
			{"unsupported scheme", "ftp://docbank:8485", true, false},
			{"missing host", "https://:443", true, false},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				apiURL, webURL := tc.endpoint, ""
				if field == "web_url" {
					apiURL, webURL = "https://docbank.example", tc.endpoint
				}
				var cfg Config
				data := fmt.Sprintf("[integrations.docbank]\nurl = %q\nweb_url = %q\napi_key_env = 'DOCBANK_TEST_KEY'\nallow_http = %t\n", apiURL, webURL, tc.allow)
				require.NoError(t, cfg.applyConfigTOML(data))
				err := cfg.Integrations.Docbank.Validate()
				if tc.wantOK {
					assert.NoError(t, err)
				} else {
					assert.ErrorContains(t, err, field)
				}
				assert.Equal(t, tc.wantOK, cfg.Integrations.Docbank.Enabled())
			})
		}
	}
}

func TestDocbankAllowHTTPLoad(t *testing.T) {
	unsetDeploymentEnv(t)
	dir := setupTestEnv(t)
	writeConfig(t, dir, map[string]any{"integrations": map[string]any{"docbank": map[string]any{
		"url": "http://docbank:8485", "api_key_env": "DOCBANK_TEST_KEY", "allow_http": true,
	}}})
	cfg, err := LoadMinimalFor(ScopeDocbank)
	require.NoError(t, err)
	assert.True(t, cfg.Integrations.Docbank.Enabled())
}
