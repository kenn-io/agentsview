package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wesm/agentsview/internal/parser"
)

// TerminalConfig holds terminal launch preferences.
type TerminalConfig struct {
	// Mode: "auto" (detect terminal), "custom" (use CustomBin),
	// or "clipboard" (never launch, always copy).
	Mode string `json:"mode"`
	// CustomBin is the terminal binary path (used when Mode == "custom").
	CustomBin string `json:"custom_bin,omitempty"`
	// CustomArgs is a template for terminal args. Use {cmd} as
	// placeholder for the resume command (e.g. "-- bash -c {cmd}").
	CustomArgs string `json:"custom_args,omitempty"`
}

// Config holds all application configuration.
type Config struct {
	Host         string         `json:"host"`
	Port         int            `json:"port"`
	NoBrowser    bool           `json:"no_browser"`
	DataDir      string         `json:"data_dir"`
	DBPath       string         `json:"-"`
	CursorSecret string         `json:"cursor_secret"`
	GithubToken  string         `json:"github_token,omitempty"`
	Terminal     TerminalConfig `json:"terminal,omitempty"`
	AuthToken    string         `json:"auth_token,omitempty"`
	RemoteAccess bool           `json:"remote_access"`
	WriteTimeout time.Duration  `json:"-"`

	// AgentDirs maps each AgentType to its configured
	// directories. Single-dir agents store a one-element
	// slice; unconfigured agents use nil.
	AgentDirs map[parser.AgentType][]string `json:"-"`

	// agentDirSource tracks how each agent's dirs were
	// set so loadFile doesn't override env-set values.
	agentDirSource map[parser.AgentType]dirSource

	ResultContentBlockedCategories []string `json:"result_content_blocked_categories,omitempty"`
}

type dirSource int

const (
	dirDefault dirSource = iota
	dirFile
	dirEnv
)

// ResolveDirs returns the effective directories for an agent.
func (c *Config) ResolveDirs(
	agent parser.AgentType,
) []string {
	return c.AgentDirs[agent]
}

// IsUserConfigured reports whether the agent's directories
// were explicitly set by the user (via env var or config file)
// rather than populated from defaults.
func (c *Config) IsUserConfigured(
	agent parser.AgentType,
) bool {
	return c.agentDirSource[agent] != dirDefault
}

// Default returns a Config with default values.
func Default() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf(
			"determining home directory: %w", err,
		)
	}
	dataDir := filepath.Join(home, ".agentsview")

	agentDirs := make(map[parser.AgentType][]string)
	agentDirSource := make(map[parser.AgentType]dirSource)
	for _, def := range parser.Registry {
		dirs := make([]string, len(def.DefaultDirs))
		for i, rel := range def.DefaultDirs {
			dirs[i] = filepath.Join(home, rel)
		}
		agentDirs[def.Type] = dirs
		agentDirSource[def.Type] = dirDefault
	}

	return Config{
		Host:                           "127.0.0.1",
		Port:                           8080,
		DataDir:                        dataDir,
		DBPath:                         filepath.Join(dataDir, "sessions.db"),
		WriteTimeout:                   30 * time.Second,
		AgentDirs:                      agentDirs,
		agentDirSource:                 agentDirSource,
		ResultContentBlockedCategories: []string{"Read", "Glob"},
	}, nil
}

// Load builds a Config by layering: defaults < config file < env < flags.
// The provided FlagSet must already be parsed by the caller.
// Only flags that were explicitly set override the lower layers.
func Load(fs *flag.FlagSet) (Config, error) {
	cfg, err := LoadMinimal()
	if err != nil {
		return cfg, err
	}
	applyFlags(&cfg, fs)
	return cfg, nil
}

// LoadMinimal builds a Config from defaults, env, and config file,
// without parsing CLI flags. Use this for subcommands that manage
// their own flag sets.
func LoadMinimal() (Config, error) {
	cfg, err := Default()
	if err != nil {
		return cfg, err
	}
	cfg.loadEnv()

	if err := cfg.loadFile(); err != nil {
		return cfg, fmt.Errorf("loading config file: %w", err)
	}
	if err := cfg.ensureCursorSecret(); err != nil {
		return cfg, fmt.Errorf("ensuring cursor secret: %w", err)
	}
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	return cfg, nil
}

func (c *Config) configPath() string {
	return filepath.Join(c.DataDir, "config.json")
}

func (c *Config) loadFile() error {
	data, err := os.ReadFile(c.configPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}

	var file struct {
		GithubToken                    string         `json:"github_token"`
		CursorSecret                   string         `json:"cursor_secret"`
		ResultContentBlockedCategories []string       `json:"result_content_blocked_categories"`
		Terminal                       TerminalConfig `json:"terminal"`
		AuthToken                      string         `json:"auth_token"`
		RemoteAccess                   bool           `json:"remote_access"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if file.GithubToken != "" {
		c.GithubToken = file.GithubToken
	}
	if file.CursorSecret != "" {
		c.CursorSecret = file.CursorSecret
	}
	if file.ResultContentBlockedCategories != nil {
		c.ResultContentBlockedCategories = file.ResultContentBlockedCategories
	}
	if file.Terminal.Mode != "" {
		c.Terminal = file.Terminal
	}
	if file.AuthToken != "" {
		c.AuthToken = file.AuthToken
	}
	c.RemoteAccess = file.RemoteAccess

	// Parse config-file dir arrays for agents that have a
	// ConfigKey. Only apply when not already set by env var.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parsing config raw: %w", err)
	}
	for _, def := range parser.Registry {
		if def.ConfigKey == "" {
			continue
		}
		rawVal, exists := raw[def.ConfigKey]
		if !exists {
			continue
		}
		if c.agentDirSource[def.Type] == dirEnv {
			continue
		}
		var dirs []string
		if err := json.Unmarshal(rawVal, &dirs); err != nil {
			log.Printf(
				"config: %s: expected string array: %v",
				def.ConfigKey, err,
			)
			continue
		}
		if len(dirs) > 0 {
			c.AgentDirs[def.Type] = dirs
			c.agentDirSource[def.Type] = dirFile
		}
	}
	return nil
}

func (c *Config) ensureCursorSecret() error {
	if c.CursorSecret != "" {
		return nil
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generating secret: %w", err)
	}
	secret := base64.StdEncoding.EncodeToString(b)
	c.CursorSecret = secret

	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing := make(map[string]any)
	data, err := os.ReadFile(c.configPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("existing config invalid: %w", err)
		}
	}

	existing["cursor_secret"] = secret
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(c.configPath(), out, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

func (c *Config) loadEnv() {
	for _, def := range parser.Registry {
		if v := os.Getenv(def.EnvVar); v != "" {
			c.AgentDirs[def.Type] = []string{v}
			c.agentDirSource[def.Type] = dirEnv
		}
	}
	if v := os.Getenv("AGENT_VIEWER_DATA_DIR"); v != "" {
		c.DataDir = v
	}
}

// RegisterServeFlags registers serve-command flags on fs.
// The caller must call fs.Parse before passing fs to Load.
func RegisterServeFlags(fs *flag.FlagSet) {
	fs.String("host", "127.0.0.1", "Host to bind to")
	fs.Int("port", 8080, "Port to listen on")
	fs.Bool(
		"no-browser", false,
		"Don't open browser on startup",
	)
}

// applyFlags copies explicitly-set flags from fs into cfg.
func applyFlags(cfg *Config, fs *flag.FlagSet) {
	if fs == nil {
		return
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			cfg.Host = f.Value.String()
		case "port":
			// flag already validated the int; ignore parse error
			cfg.Port, _ = strconv.Atoi(f.Value.String())
		case "no-browser":
			cfg.NoBrowser = f.Value.String() == "true"
		}
	})
}

// ResolveDataDir returns the effective data directory by applying
// defaults and environment overrides, without reading any files.
// Use this to determine where migration should target before
// calling Load or LoadMinimal.
func ResolveDataDir() (string, error) {
	cfg, err := Default()
	if err != nil {
		return "", err
	}
	if v := os.Getenv("AGENT_VIEWER_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	return cfg.DataDir, nil
}

// SaveTerminalConfig persists terminal settings to the config file.
func (c *Config) SaveTerminalConfig(tc TerminalConfig) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing := make(map[string]any)
	data, err := os.ReadFile(c.configPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config file: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf(
				"existing config is invalid, cannot update: %w",
				err,
			)
		}
	}

	existing["terminal"] = tc
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(c.configPath(), out, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	c.Terminal = tc
	return nil
}

// SaveSettings persists a partial settings update to the config file.
// The patch map contains JSON keys mapped to their new values. Only
// the keys present in patch are written; other config keys are preserved.
func (c *Config) SaveSettings(patch map[string]any) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing := make(map[string]any)
	data, err := os.ReadFile(c.configPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config file: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf(
				"existing config is invalid, cannot update: %w",
				err,
			)
		}
	}

	for k, v := range patch {
		existing[k] = v
	}

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(c.configPath(), out, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	// Update in-memory config for known keys.
	if v, ok := patch["terminal"]; ok {
		if b, err := json.Marshal(v); err == nil {
			var tc TerminalConfig
			if err := json.Unmarshal(b, &tc); err == nil {
				c.Terminal = tc
			}
		}
	}
	if v, ok := patch["github_token"]; ok {
		if s, ok := v.(string); ok {
			c.GithubToken = s
		}
	}
	if v, ok := patch["auth_token"]; ok {
		if s, ok := v.(string); ok {
			c.AuthToken = s
		}
	}
	if v, ok := patch["remote_access"]; ok {
		if b, ok := v.(bool); ok {
			c.RemoteAccess = b
		}
	}
	return nil
}

// EnsureAuthToken generates and persists an auth token if one does
// not already exist. Called when remote_access is enabled.
func (c *Config) EnsureAuthToken() error {
	if c.AuthToken != "" {
		return nil
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("generating auth token: %w", err)
	}
	token := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
	c.AuthToken = token

	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing := make(map[string]any)
	data, err := os.ReadFile(c.configPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("existing config invalid: %w", err)
		}
	}

	existing["auth_token"] = token
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(c.configPath(), out, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// SaveGithubToken persists the GitHub token to the config file.
func (c *Config) SaveGithubToken(token string) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing := make(map[string]any)
	data, err := os.ReadFile(c.configPath())
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading config file: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf(
				"existing config is invalid, cannot update: %w",
				err,
			)
		}
	}

	existing["github_token"] = token
	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(c.configPath(), out, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	c.GithubToken = token
	return nil
}
