package config

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"go.kenn.io/agentsview/internal/pathutil"
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
	dir := dataDirFromEnv()
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return RemoteConfig{}, fmt.Errorf("determining home directory: %w", err)
		}
		dir = filepath.Join(home, ".agentsview")
	}
	cfg := Config{DataDir: dir}
	if err := expandDataDir(&cfg); err != nil {
		return RemoteConfig{}, err
	}
	var file struct {
		Remote RemoteConfig `json:"remote" toml:"remote"`
	}
	data, err := os.ReadFile(cfg.configPath())
	if os.IsNotExist(err) {
		data, err = os.ReadFile(cfg.jsonConfigPath())
		if err == nil {
			err = json.Unmarshal(data, &file)
		}
	} else if err == nil {
		_, err = toml.Decode(string(data), &file)
	}
	if err != nil && !os.IsNotExist(err) {
		return RemoteConfig{}, fmt.Errorf("loading remote config: %w", err)
	}
	cfg.Remote = file.Remote
	cfg.Remote.TokenFile = remoteTokenPath(cfg.Remote.TokenFile, filepath.Dir(cfg.configPath()))
	cfg.applyRemoteEnv()
	return cfg.Remote, nil
}
