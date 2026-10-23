package config

import (
	"go.kenn.io/agentsview/internal/pathutil"
	"os"
	"path/filepath"
)

// RemoteConfig selects the default server for allowlisted read commands.
// Validation and credential reads happen only when this target is selected.
type RemoteConfig struct {
	URL               string `json:"url,omitempty" toml:"url"`
	TokenFile         string `json:"token_file,omitempty" toml:"token_file"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http,omitempty" toml:"allow_insecure_http"`
	SpokeHost         string `json:"spoke_host,omitempty" toml:"spoke_host"`
}

func remoteTokenPath(path, base string) string {
	if path == "" {
		return ""
	}
	expanded, err := pathutil.ExpandHome(path)
	if err == nil {
		path = expanded
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return path
}
func (c *Config) applyRemoteEnv() {
	if value := os.Getenv("AGENTSVIEW_REMOTE_URL"); value != "" {
		c.Remote.URL = value
	}
	if value := os.Getenv("AGENTSVIEW_REMOTE_TOKEN_FILE"); value != "" {
		cwd, _ := os.Getwd()
		c.Remote.TokenFile = remoteTokenPath(value, cwd)
	}
}

// LoadRemote reads only configuration layers, without migrating files, resolving
// source roots, generating secrets, or reading local installation metadata.
func LoadRemote() (RemoteConfig, error) {
	cfg, err := Default()
	if err != nil {
		return RemoteConfig{}, err
	}
	cfg.validationScope = ScopeLocal
	cfg.loadEnv()
	if err = expandDataDir(&cfg); err != nil {
		return RemoteConfig{}, err
	}
	if err = cfg.loadFileReadOnly(); err != nil {
		return RemoteConfig{}, err
	}
	return cfg.Remote, nil
}
