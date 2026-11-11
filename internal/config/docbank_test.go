package config

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocbankIntegrationValidation(t *testing.T) {
	for _, field := range []string{"url", "web_url"} {
		for _, tc := range []struct {
			name, endpoint, wantErr string
			allow                   bool
		}{
			{"container without opt-in", "http://docbank:8485", "[integrations.docbank] %s must be https, or http to a loopback host; set allow_http = true to permit other HTTP hosts", false},
			{"container with opt-in", "http://docbank:8485", "", true},
			{"public IP with opt-in", "http://8.8.8.8:8485", "", true},
			{"loopback", "http://127.0.0.1", "", false},
			{"https", "https://example.com", "", false},
			{"missing host", "https://:443", "[integrations.docbank] invalid %s", true},
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
				if tc.wantErr == "" {
					assert.NoError(t, err)
				} else {
					assert.EqualError(t, err, fmt.Sprintf(tc.wantErr, field))
				}
			})
		}
	}
	assert.NoError(t, (DocbankIntegration{}).Validate())
	assert.EqualError(t, (DocbankIntegration{URL: "https://example.com"}).Validate(), "[integrations.docbank] url and api_key_env are required together")
}
