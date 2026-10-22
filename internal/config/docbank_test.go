package config

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDocbankIntegrationValidation(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, key string
		valid               bool
	}{
		{"disabled", "", "", true}, {"missing key", "https://example.com", "", false},
		{"remote plaintext", "http://example.com", "DOCBANK_TEST_KEY", false},
		{"loopback", "http://127.0.0.1", "DOCBANK_TEST_KEY", true},
		{"https", "https://example.com", "DOCBANK_TEST_KEY", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (DocbankIntegration{URL: tc.endpoint, APIKeyEnv: tc.key}).Validate()
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
	var cfg Config
	require.NoError(t, cfg.applyConfigTOML("[integrations.docbank]\nurl = 'http://127.0.0.1:9191'\napi_key_env = 'DOCBANK_TEST_KEY'"))
	assert.Equal(t, "http://127.0.0.1:9191", cfg.Integrations.Docbank.URL)
	assert.True(t, cfg.Integrations.Docbank.Configured())
}
