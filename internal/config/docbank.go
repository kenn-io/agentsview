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
	// APIKeyFile names a private file holding the API key, read when the
	// section loads. It is the alternative to APIKeyEnv.
	APIKeyFile string `json:"api_key_file,omitempty" toml:"api_key_file"`
	// AllowHTTP permits plaintext HTTP on a trusted private network.
	AllowHTTP bool `json:"allow_http,omitempty" toml:"allow_http"`

	// apiKey holds the key read from APIKeyFile.
	apiKey string
}

func (c DocbankIntegration) Configured() bool {
	return strings.TrimSpace(c.URL) != "" &&
		(strings.TrimSpace(c.APIKeyEnv) != "" || strings.TrimSpace(c.APIKeyFile) != "")
}

// Enabled reports whether lookup may run: the settings are present and valid.
func (c DocbankIntegration) Enabled() bool { return c.Configured() && c.Validate() == nil }

// APIKey returns the key read from APIKeyFile, or else the value of the
// environment variable APIKeyEnv names.
func (c DocbankIntegration) APIKey() string {
	if c.apiKey != "" {
		return c.apiKey
	}
	return os.Getenv(strings.TrimSpace(c.APIKeyEnv))
}

// resolveSecrets validates the section and reads api_key_file.
func (c *DocbankIntegration) resolveSecrets() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.APIKeyFile) == "" {
		return nil
	}
	key, err := readSettingSecretFile("[integrations.docbank] api_key_file", c.APIKeyFile)
	if err != nil {
		return err
	}
	c.apiKey = key
	return nil
}

func (c DocbankIntegration) Validate() error {
	if c.URL == "" && c.APIKeyEnv == "" && c.APIKeyFile == "" && c.WebURL == "" {
		return nil
	}
	if err := oneSecretSource("[integrations.docbank]", "api_key_env", c.APIKeyEnv,
		"api_key_file", c.APIKeyFile); err != nil {
		return err
	}
	if !c.Configured() {
		return errors.New("[integrations.docbank] url and one of api_key_env or api_key_file are required together")
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
