package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type IntegrationsConfig struct {
	Docbank DocbankIntegration `json:"docbank,omitempty" toml:"docbank"`
}

type DocbankIntegration struct {
	URL       string `json:"url,omitempty" toml:"url"`
	WebURL    string `json:"web_url,omitempty" toml:"web_url"`
	APIKeyEnv string `json:"api_key_env,omitempty" toml:"api_key_env"`
	// AllowHTTP permits plaintext HTTP on a trusted private network.
	AllowHTTP bool `json:"allow_http,omitempty" toml:"allow_http"`
}

func (c DocbankIntegration) Configured() bool {
	return strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.APIKeyEnv) != ""
}

// Enabled reports whether lookup may run: the settings are present and valid.
func (c DocbankIntegration) Enabled() bool { return c.Configured() && c.Validate() == nil }

func (c DocbankIntegration) APIKey() string { return os.Getenv(strings.TrimSpace(c.APIKeyEnv)) }
func (c DocbankIntegration) Validate() error {
	if c.URL == "" && c.APIKeyEnv == "" && c.WebURL == "" {
		return nil
	}
	if !c.Configured() {
		return errors.New("[integrations.docbank] url and api_key_env are required together")
	}
	for _, item := range []struct{ field, endpoint string }{{"url", c.URL}, {"web_url", c.WebURL}} {
		field, endpoint := item.field, item.endpoint
		if endpoint == "" {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(endpoint))
		if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("[integrations.docbank] invalid %s", field)
		}
		if err := ValidateExtractTransport(u, c.AllowHTTP); err != nil {
			if u.Scheme != "http" {
				return fmt.Errorf("[integrations.docbank] %s must use http or https", field)
			}
			return fmt.Errorf("[integrations.docbank] %s must be https, or http to a loopback host; set allow_http = true to permit other HTTP hosts", field)
		}
	}
	return nil
}
