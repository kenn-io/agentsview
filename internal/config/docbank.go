package config

import (
	"errors"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// IntegrationsConfig configures optional external lookups.
type IntegrationsConfig struct {
	Docbank DocbankIntegration `json:"docbank" toml:"docbank"`
}

// DocbankIntegration holds references to credentials, never the credential itself.
type DocbankIntegration struct {
	Enabled    bool   `json:"enabled" toml:"enabled"`
	URL        string `json:"url" toml:"url"`
	WebURL     string `json:"web_url,omitempty" toml:"web_url"`
	APIKeyEnv  string `json:"api_key_env,omitempty" toml:"api_key_env"`
	APIKeyFile string `json:"api_key_file,omitempty" toml:"api_key_file"`
}

// DocbankLoopbackHost recognizes literal loopback and special-use localhost names.
// Network transport must additionally verify and pin resolved dial addresses.
func DocbankLoopbackHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	host = strings.ToLower(host)
	if host != "localhost" && !strings.HasSuffix(host, ".localhost") {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return len(host) <= 253
}

// Validate is offline: it neither resolves credentials nor contacts the service.
func (c DocbankIntegration) Validate() error {
	if !c.Enabled {
		return nil
	}
	if (c.APIKeyEnv == "") == (c.APIKeyFile == "") {
		return errors.New("docbank requires exactly one credential source")
	}
	if strings.TrimSpace(c.APIKeyEnv) != c.APIKeyEnv || strings.TrimSpace(c.APIKeyFile) != c.APIKeyFile {
		return errors.New("invalid docbank credential source")
	}
	if err := validateDocbankURL(c.URL, false); err != nil {
		return err
	}
	if c.WebURL != "" {
		return validateDocbankURL(c.WebURL, true)
	}
	return nil
}

func validateDocbankURL(raw string, browser bool) error {
	invalid := errors.New("invalid docbank URL")
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.Opaque != "" {
		return invalid
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !DocbankLoopbackHost(u.Hostname())) {
		return invalid
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return invalid
		}
	}
	if u.RawPath != "" || strings.Contains(u.Path, "\\") || (u.Path != "" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/") && u.Path != "/") {
		return invalid
	}
	if browser && u.Path != "" && u.Path != "/" {
		return invalid
	}
	return nil
}
