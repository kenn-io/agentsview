package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	DefaultKataProject = "agentsview"
	DefaultKataActor   = "agentsview"
	DefaultKataTimeout = 10 * time.Second
)

// KataConfig configures the optional Kata spoke connection. TokenEnv names
// the environment variable holding the bearer token; the token is not stored.
type KataConfig struct {
	Enabled       bool          `json:"-" toml:"enabled"`
	Endpoint      string        `json:"-" toml:"endpoint"`
	TokenEnv      string        `json:"-" toml:"token_env"`
	Project       string        `json:"-" toml:"project"`
	Actor         string        `json:"-" toml:"actor"`
	AllowInsecure bool          `json:"-" toml:"allow_insecure"`
	Timeout       time.Duration `json:"-" toml:"timeout"`
}

func defaultKataConfig() KataConfig {
	return KataConfig{Project: DefaultKataProject, Actor: DefaultKataActor, Timeout: DefaultKataTimeout}
}

// Token reads the configured environment variable when called.
func (c KataConfig) Token() string {
	name := strings.TrimSpace(c.TokenEnv)
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

// Validate checks the enabled connection's transport settings.
func (c KataConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Project) == "" {
		return errors.New("[kata] project must be non-empty")
	}
	if c.Timeout <= 0 {
		return errors.New("[kata] timeout must be positive")
	}
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("[kata] endpoint %q must be unix://, https:// or http://", RedactedEndpoint(endpoint))
	}
	if u.User != nil {
		return errors.New("[kata] endpoint must not contain URL credentials")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("[kata] endpoint must not contain a query or fragment")
	}
	switch u.Scheme {
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return errors.New("[kata] endpoint must be unix:///absolute/path")
		}
		return nil
	case "https":
		if u.Host == "" {
			return fmt.Errorf("[kata] endpoint %q has no host", RedactedEndpoint(endpoint))
		}
		if strings.TrimSpace(c.TokenEnv) == "" {
			return errors.New("[kata] token_env is required for https endpoints")
		}
		return nil
	case "http":
		if u.Host == "" {
			return fmt.Errorf("[kata] endpoint %q has no host", RedactedEndpoint(endpoint))
		}
		if c.AllowInsecure || isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("[kata] endpoint %q uses plaintext http to a non-loopback host; use https or set allow_insecure = true", RedactedEndpoint(endpoint))
	default:
		return fmt.Errorf("[kata] endpoint %q must be unix://, https:// or http://", RedactedEndpoint(endpoint))
	}
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HasPGPushTarget reports whether any resolved PostgreSQL target has a URL.
// Failed target resolution counts as a target so hub eligibility fails closed.
func (c *Config) HasPGPushTarget() bool {
	targets, err := c.ResolvePGTargets()
	if err != nil {
		return true
	}
	for _, target := range targets {
		if strings.TrimSpace(target.Config.URL) != "" {
			return true
		}
	}
	return false
}

// mergeKataTOML keeps defaults for keys omitted from the file.
func mergeKataTOML(dst *KataConfig, src KataConfig, meta toml.MetaData) {
	if meta.IsDefined("kata", "enabled") {
		dst.Enabled = src.Enabled
	}
	if meta.IsDefined("kata", "endpoint") {
		dst.Endpoint = strings.TrimSpace(src.Endpoint)
	}
	if meta.IsDefined("kata", "token_env") {
		dst.TokenEnv = strings.TrimSpace(src.TokenEnv)
	}
	if meta.IsDefined("kata", "project") {
		dst.Project = strings.TrimSpace(src.Project)
	}
	if meta.IsDefined("kata", "actor") {
		dst.Actor = strings.TrimSpace(src.Actor)
	}
	if meta.IsDefined("kata", "allow_insecure") {
		dst.AllowInsecure = src.AllowInsecure
	}
	if meta.IsDefined("kata", "timeout") {
		dst.Timeout = src.Timeout
	}
}
