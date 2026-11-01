package config

import (
	"errors"
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
	for _, endpoint := range []string{c.URL, c.WebURL} {
		if endpoint == "" {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(endpoint))
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("[integrations.docbank] invalid URL")
		}
		if err := ValidateExtractTransport(u, false); err != nil {
			return errors.New("[integrations.docbank] url must be https, or http to a loopback host")
		}
	}
	return nil
}
