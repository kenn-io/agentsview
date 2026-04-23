package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/spf13/pflag"
	"github.com/wesm/agentsview/internal/parser"
)

// TerminalConfig holds terminal launch preferences.
type TerminalConfig struct {
	// Mode: "auto" (detect terminal), "custom" (use CustomBin),
	// or "clipboard" (never launch, always copy).
	Mode string `json:"mode" toml:"mode"`
	// CustomBin is the terminal binary path (used when Mode == "custom").
	CustomBin string `json:"custom_bin,omitempty" toml:"custom_bin"`
	// CustomArgs is a template for terminal args. Use {cmd} as
	// placeholder for the resume command (e.g. "-- bash -c {cmd}").
	CustomArgs string `json:"custom_args,omitempty" toml:"custom_args"`
}

// ProxyConfig controls an optional managed reverse proxy.
type ProxyConfig struct {
	// Mode enables a managed proxy implementation.
	// Currently supported: "caddy".
	Mode string `json:"mode,omitempty" toml:"mode"`
	// Bin overrides the proxy executable path.
	Bin string `json:"bin,omitempty" toml:"bin"`
	// BindHost is the local interface/IP the proxy binds to.
	BindHost string `json:"bind_host,omitempty" toml:"bind_host"`
	// PublicPort is the external port exposed by the proxy.
	PublicPort int `json:"public_port,omitempty" toml:"public_port"`
	// TLSCert and TLSKey are used by managed HTTPS mode.
	TLSCert string `json:"tls_cert,omitempty" toml:"tls_cert"`
	TLSKey  string `json:"tls_key,omitempty" toml:"tls_key"`
	// AllowedSubnets restrict inbound clients to these CIDRs.
	AllowedSubnets []string `json:"allowed_subnets,omitempty" toml:"allowed_subnets"`
}

// PGConfig holds PostgreSQL connection settings.
type PGConfig struct {
	URL             string   `toml:"url" json:"url"`
	Schema          string   `toml:"schema" json:"schema"`
	MachineName     string   `toml:"machine_name" json:"machine_name"`
	AllowInsecure   bool     `toml:"allow_insecure" json:"allow_insecure"`
	Projects        []string `toml:"projects" json:"projects,omitempty"`
	ExcludeProjects []string `toml:"exclude_projects" json:"exclude_projects,omitempty"`
}

// AutomatedConfig holds user-supplied additions to the
// automated-session classifier. Parse-only; all semantic
// normalization (trim, dedupe, length cap, built-in overlap
// drop) happens inside db.SetUserAutomationPrefixes.
type AutomatedConfig struct {
	Prefixes []string `toml:"prefixes" json:"prefixes,omitempty"`
}

type CustomModelRate struct {
	Input         float64 `json:"input" toml:"input"`
	Output        float64 `json:"output" toml:"output"`
	CacheCreation float64 `json:"cache_creation,omitempty" toml:"cache_creation"`
	CacheRead     float64 `json:"cache_read,omitempty" toml:"cache_read"`
}

// Config holds all application configuration.
type Config struct {
	Host                 string          `json:"host" toml:"host"`
	Port                 int             `json:"port" toml:"port"`
	DataDir              string          `json:"data_dir" toml:"data_dir"`
	DBPath               string          `json:"-" toml:"-"`
	PublicURL            string          `json:"public_url,omitempty" toml:"public_url"`
	PublicOrigins        []string        `json:"public_origins,omitempty" toml:"public_origins"`
	Proxy                ProxyConfig     `json:"proxy,omitempty" toml:"proxy"`
	WatchExcludePatterns []string        `json:"watch_exclude_patterns,omitempty" toml:"watch_exclude_patterns"`
	CursorSecret         string          `json:"cursor_secret" toml:"cursor_secret"`
	GithubToken          string          `json:"github_token,omitempty" toml:"github_token"`
	Terminal             TerminalConfig  `json:"terminal,omitempty" toml:"terminal"`
	AuthToken            string          `json:"auth_token,omitempty" toml:"auth_token"`
	RequireAuth          bool            `json:"require_auth" toml:"require_auth"`
	NoBrowser            bool            `json:"no_browser" toml:"no_browser"`
	DisableUpdateCheck   bool            `json:"disable_update_check" toml:"disable_update_check"`
	NoSync               bool            `json:"-" toml:"-"`
	PG                   PGConfig        `json:"pg,omitempty" toml:"pg"`
	Automated            AutomatedConfig `json:"automated,omitempty" toml:"automated"`
	WriteTimeout         time.Duration   `json:"-" toml:"-"`

	// AgentDirs maps each AgentType to its configured
	// directories. Single-dir agents store a one-element
	// slice; unconfigured agents use nil.
	AgentDirs map[parser.AgentType][]string `json:"-" toml:"-"`

	// agentDirSource tracks how each agent's dirs were
	// set so loadFile doesn't override env-set values.
	agentDirSource map[parser.AgentType]dirSource

	ResultContentBlockedCategories []string `json:"result_content_blocked_categories,omitempty" toml:"result_content_blocked_categories"`

	// EventsCoalesceInterval is the minimum wall-clock time between
	// SSE data_changed broadcasts to connected clients. Emits that
	// arrive within this window after a prior broadcast are coalesced
	// into a single trailing broadcast, bounding dashboard refetch
	// work during bursts of sync activity. Zero disables coalescing.
	EventsCoalesceInterval time.Duration `json:"events_coalesce_interval,omitempty" toml:"events_coalesce_interval"`

	CustomModelPricing map[string]CustomModelRate `json:"custom_model_pricing,omitempty" toml:"custom_model_pricing"`

	// HostExplicit is true when the user passed --host on the CLI.
	// Used to prevent auto-bind to 0.0.0.0 when the user
	// explicitly requested a specific host.
	HostExplicit bool `json:"-" toml:"-"`
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
		WatchExcludePatterns:           []string{".git", "node_modules", "__pycache__", ".venv", "venv", "vendor", ".next"},
		ResultContentBlockedCategories: []string{"Read", "Glob"},
		EventsCoalesceInterval:         10 * time.Second,
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
	if err := finalize(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// LoadPFlags builds a Config from a parsed Cobra/pflag FlagSet.
func LoadPFlags(fs *pflag.FlagSet) (Config, error) {
	cfg, err := LoadMinimal()
	if err != nil {
		return cfg, err
	}
	applyPFlags(&cfg, fs)
	if err := finalize(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// LoadPGServe builds a Config for `pg serve` by preserving
// shared and PG settings from defaults/env/config file while
// resetting serve-specific network/browser settings to defaults.
// Only explicitly provided serve flags are applied on top.
func LoadPGServe(fs *flag.FlagSet) (Config, error) {
	cfg, err := loadPGServeBase()
	if err != nil {
		return cfg, err
	}
	applyFlags(&cfg, fs)
	if err := finalize(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// LoadPGServePFlags builds a PG serve config from a parsed Cobra/pflag FlagSet.
func LoadPGServePFlags(fs *pflag.FlagSet) (Config, error) {
	cfg, err := loadPGServeBase()
	if err != nil {
		return cfg, err
	}
	applyPFlags(&cfg, fs)
	if err := finalize(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func loadPGServeBase() (Config, error) {
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

	// pg serve intentionally ignores persisted normal serve/public/proxy
	// settings so an existing SQLite-backed serve deployment cannot silently
	// reconfigure the PG-backed server. Until a dedicated pg-serve config
	// namespace exists, only explicit pg-serve flags should shape its
	// network/proxy behavior.
	cfg.Host = "127.0.0.1"
	cfg.Port = 8080
	cfg.PublicURL = ""
	cfg.PublicOrigins = nil
	cfg.Proxy = ProxyConfig{}
	cfg.NoBrowser = false
	cfg.HostExplicit = false
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
	if err := finalize(&cfg); err != nil {
		return cfg, err
	}
	if err := cfg.ensureCursorSecret(); err != nil {
		return cfg, fmt.Errorf("ensuring cursor secret: %w", err)
	}
	cfg.DBPath = filepath.Join(cfg.DataDir, "sessions.db")
	return cfg, nil
}

func (c *Config) configPath() string {
	return filepath.Join(c.DataDir, "config.toml")
}

func (c *Config) jsonConfigPath() string {
	return filepath.Join(c.DataDir, "config.json")
}

// migrateJSONToTOML converts config.json to config.toml if
// config.json exists and config.toml does not. The original
// JSON file is renamed to config.json.bak.
func (c *Config) migrateJSONToTOML() error {
	jsonPath := c.jsonConfigPath()
	tomlPath := c.configPath()

	if _, err := os.Stat(tomlPath); err == nil {
		return nil // TOML already exists
	}
	data, err := os.ReadFile(jsonPath)
	if os.IsNotExist(err) {
		return nil // no JSON to migrate
	}
	if err != nil {
		return fmt.Errorf("reading config.json for migration: %w", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parsing config.json for migration: %w", err)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return fmt.Errorf("encoding config.toml: %w", err)
	}
	if err := os.WriteFile(tomlPath, buf.Bytes(), 0o600); err != nil {
		return fmt.Errorf("writing config.toml: %w", err)
	}
	if err := os.Rename(jsonPath, jsonPath+".bak"); err != nil {
		return fmt.Errorf("renaming config.json to .bak: %w", err)
	}
	return nil
}

func (c *Config) loadFile() error {
	if err := c.migrateJSONToTOML(); err != nil {
		return err
	}

	path := c.configPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	var file struct {
		GithubToken                    string                     `toml:"github_token"`
		CursorSecret                   string                     `toml:"cursor_secret"`
		PublicURL                      string                     `toml:"public_url"`
		PublicOrigins                  []string                   `toml:"public_origins"`
		Proxy                          ProxyConfig                `toml:"proxy"`
		WatchExcludePatterns           []string                   `toml:"watch_exclude_patterns"`
		ResultContentBlockedCategories []string                   `toml:"result_content_blocked_categories"`
		Terminal                       TerminalConfig             `toml:"terminal"`
		AuthToken                      string                     `toml:"auth_token"`
		RequireAuth                    bool                       `toml:"require_auth"`
		RemoteAccess                   bool                       `toml:"remote_access"`
		DisableUpdateCheck             bool                       `toml:"disable_update_check"`
		PG                             PGConfig                   `toml:"pg"`
		Automated                      AutomatedConfig            `toml:"automated"`
		EventsCoalesceInterval         time.Duration              `toml:"events_coalesce_interval"`
		CustomModelPricing             map[string]CustomModelRate `toml:"custom_model_pricing"`
	}
	meta, err := toml.DecodeFile(path, &file)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	if file.GithubToken != "" {
		c.GithubToken = file.GithubToken
	}
	if file.CursorSecret != "" {
		c.CursorSecret = file.CursorSecret
	}
	if file.PublicURL != "" {
		c.PublicURL = file.PublicURL
	}
	if file.PublicOrigins != nil {
		c.PublicOrigins = file.PublicOrigins
	}
	if file.Proxy.Mode != "" || file.Proxy.Bin != "" ||
		file.Proxy.BindHost != "" || file.Proxy.PublicPort != 0 ||
		file.Proxy.TLSCert != "" || file.Proxy.TLSKey != "" ||
		file.Proxy.AllowedSubnets != nil {
		c.Proxy = file.Proxy
	}
	if file.WatchExcludePatterns != nil {
		c.WatchExcludePatterns = file.WatchExcludePatterns
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
	c.RequireAuth = file.RequireAuth || file.RemoteAccess
	c.DisableUpdateCheck = file.DisableUpdateCheck
	// Merge pg field-by-field so env vars override only
	// the fields they set, preserving config-file settings.
	if file.PG.URL != "" && c.PG.URL == "" {
		c.PG.URL = file.PG.URL
	}
	if file.PG.Schema != "" && c.PG.Schema == "" {
		c.PG.Schema = file.PG.Schema
	}
	if file.PG.MachineName != "" && c.PG.MachineName == "" {
		c.PG.MachineName = file.PG.MachineName
	}
	if file.PG.AllowInsecure {
		c.PG.AllowInsecure = true
	}
	if file.PG.Projects != nil && c.PG.Projects == nil {
		c.PG.Projects = file.PG.Projects
	}
	if file.PG.ExcludeProjects != nil && c.PG.ExcludeProjects == nil {
		c.PG.ExcludeProjects = file.PG.ExcludeProjects
	}
	// IsDefined distinguishes "unset" (leave default 10s) from an
	// explicit "0s" (disable coalescing). Checking != 0 would silently
	// ignore the latter.
	if meta.IsDefined("events_coalesce_interval") {
		c.EventsCoalesceInterval = file.EventsCoalesceInterval
	}
	if file.Automated.Prefixes != nil {
		c.Automated.Prefixes = file.Automated.Prefixes
	}
	if len(file.CustomModelPricing) > 0 {
		c.CustomModelPricing = file.CustomModelPricing
	}

	// Parse config-file dir arrays for agents that have a
	// ConfigKey. Only apply when not already set by env var.
	var raw map[string]any
	if _, err := toml.DecodeFile(path, &raw); err != nil {
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
		rawSlice, ok := rawVal.([]any)
		if !ok {
			log.Printf(
				"config: %s: expected string array: got %T",
				def.ConfigKey, rawVal,
			)
			continue
		}
		dirs := make([]string, 0, len(rawSlice))
		for _, v := range rawSlice {
			s, ok := v.(string)
			if !ok {
				log.Printf(
					"config: %s: expected string array: element is %T",
					def.ConfigKey, v,
				)
				dirs = nil
				break
			}
			dirs = append(dirs, s)
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

	existing, err := c.readConfigMap()
	if err != nil {
		return err
	}

	existing["cursor_secret"] = secret
	return c.writeConfigMap(existing)
}

// readConfigMap reads the TOML config file into a map. Returns
// an empty map if the file does not exist.
func (c *Config) readConfigMap() (map[string]any, error) {
	existing := make(map[string]any)
	data, err := os.ReadFile(c.configPath())
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	if err == nil {
		if _, err := toml.Decode(string(data), &existing); err != nil {
			return nil, fmt.Errorf("existing config invalid: %w", err)
		}
	}
	return existing, nil
}

// writeConfigMap encodes a map as TOML and writes it to the
// config file.
func (c *Config) writeConfigMap(m map[string]any) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(c.configPath(), buf.Bytes(), 0o600); err != nil {
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
	if v := os.Getenv("AGENTSVIEW_PG_URL"); v != "" {
		c.PG.URL = v
	}
	if v := os.Getenv("AGENTSVIEW_PG_SCHEMA"); v != "" {
		c.PG.Schema = v
	}
	if v := os.Getenv("AGENTSVIEW_PG_MACHINE"); v != "" {
		c.PG.MachineName = v
	}
	if v := os.Getenv("AGENTSVIEW_DISABLE_UPDATE_CHECK"); v != "" {
		c.DisableUpdateCheck = v == "1" || v == "true"
	}
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		*f = append(*f, part)
	}
	return nil
}

func (f *stringListFlag) Type() string {
	return "stringList"
}

// RegisterServeFlags registers serve-command flags on fs.
// The caller must call fs.Parse before passing fs to Load.
func RegisterServeFlags(fs *flag.FlagSet) {
	fs.String("host", "127.0.0.1", "Host to bind to")
	fs.Int("port", 8080, "Port to listen on")
	fs.String(
		"public-url", "",
		"Public URL to trust and open for hostname or proxy access",
	)
	fs.Var(
		&stringListFlag{},
		"public-origin",
		"Trusted browser origin to allow for remote or proxied access (repeatable or comma-separated)",
	)
	fs.String(
		"proxy", "",
		"Managed reverse proxy mode (currently: caddy)",
	)
	fs.String(
		"caddy-bin", "",
		"Caddy binary to use when -proxy=caddy (default: caddy)",
	)
	fs.String(
		"proxy-bind-host", "",
		"Local interface/IP for managed Caddy to bind (default: 0.0.0.0)",
	)
	fs.Int(
		"public-port", 0,
		"External port for the public URL in managed Caddy mode (default: 8443)",
	)
	fs.String(
		"tls-cert", "",
		"TLS certificate path for managed Caddy HTTPS mode",
	)
	fs.String(
		"tls-key", "",
		"TLS key path for managed Caddy HTTPS mode",
	)
	fs.Var(
		&stringListFlag{},
		"allowed-subnet",
		"Client CIDR allowed to connect to the managed proxy (repeatable or comma-separated)",
	)
	fs.Bool(
		"no-browser", false,
		"Don't open browser on startup",
	)
	fs.Bool(
		"no-sync", false,
		"Skip initial sync and disable background sync/file watching",
	)
	fs.Bool(
		"no-update-check", false,
		"Disable the update check API endpoint",
	)
	fs.Bool(
		"require-auth", false,
		"Require a bearer token for all API requests",
	)
	fs.Duration(
		"events-coalesce-interval", 10*time.Second,
		"Minimum interval between SSE data_changed broadcasts (0 disables coalescing)",
	)
}

// RegisterServePFlags registers serve-command flags on fs.
func RegisterServePFlags(fs *pflag.FlagSet) {
	fs.String("host", "127.0.0.1", "Host to bind to")
	fs.Int("port", 8080, "Port to listen on")
	fs.String(
		"public-url", "",
		"Public URL to trust and open for hostname or proxy access",
	)
	fs.Var(
		&stringListFlag{},
		"public-origin",
		"Trusted browser origin to allow for remote or proxied access (repeatable or comma-separated)",
	)
	fs.String(
		"proxy", "",
		"Managed reverse proxy mode (currently: caddy)",
	)
	fs.String(
		"caddy-bin", "",
		"Caddy binary to use when -proxy=caddy (default: caddy)",
	)
	fs.String(
		"proxy-bind-host", "",
		"Local interface/IP for managed Caddy to bind (default: 0.0.0.0)",
	)
	fs.Int(
		"public-port", 0,
		"External port for the public URL in managed Caddy mode (default: 8443)",
	)
	fs.String(
		"tls-cert", "",
		"TLS certificate path for managed Caddy HTTPS mode",
	)
	fs.String(
		"tls-key", "",
		"TLS key path for managed Caddy HTTPS mode",
	)
	fs.Var(
		&stringListFlag{},
		"allowed-subnet",
		"Client CIDR allowed to connect to the managed proxy (repeatable or comma-separated)",
	)
	fs.Bool(
		"no-browser", false,
		"Don't open browser on startup",
	)
	fs.Bool(
		"no-sync", false,
		"Skip initial sync and disable background sync/file watching",
	)
	fs.Bool(
		"no-update-check", false,
		"Disable the update check API endpoint",
	)
	fs.Bool(
		"require-auth", false,
		"Require a bearer token for all API requests",
	)
	fs.Duration(
		"events-coalesce-interval", 10*time.Second,
		"Minimum interval between SSE data_changed broadcasts (0 disables coalescing)",
	)
}

// applyFlags copies explicitly-set flags from fs into cfg.
func applyFlags(cfg *Config, fs *flag.FlagSet) {
	if fs == nil {
		return
	}
	fs.Visit(func(f *flag.Flag) {
		applyFlagValue(cfg, f.Name, f.Value.String())
	})
}

// applyPFlags copies explicitly-set pflags from fs into cfg.
func applyPFlags(cfg *Config, fs *pflag.FlagSet) {
	if fs == nil {
		return
	}
	fs.Visit(func(f *pflag.Flag) {
		applyFlagValue(cfg, f.Name, f.Value.String())
	})
}

func applyFlagValue(cfg *Config, name, value string) {
	switch name {
	case "host":
		cfg.Host = value
		cfg.HostExplicit = true
	case "port":
		cfg.Port, _ = strconv.Atoi(value)
	case "public-url":
		cfg.PublicURL = value
	case "public-origin":
		cfg.PublicOrigins = splitFlagList(value)
	case "proxy":
		cfg.Proxy.Mode = value
	case "caddy-bin":
		cfg.Proxy.Bin = value
	case "proxy-bind-host":
		cfg.Proxy.BindHost = value
	case "public-port":
		cfg.Proxy.PublicPort, _ = strconv.Atoi(value)
	case "tls-cert":
		cfg.Proxy.TLSCert = value
	case "tls-key":
		cfg.Proxy.TLSKey = value
	case "allowed-subnet":
		cfg.Proxy.AllowedSubnets = splitFlagList(value)
	case "no-browser":
		cfg.NoBrowser = value == "true"
	case "no-sync":
		cfg.NoSync = value == "true"
	case "no-update-check":
		cfg.DisableUpdateCheck = value == "true"
	case "require-auth":
		cfg.RequireAuth = value == "true"
	case "events-coalesce-interval":
		if d, err := time.ParseDuration(value); err == nil {
			cfg.EventsCoalesceInterval = d
		}
	}
}

func splitFlagList(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	return out
}

func finalize(cfg *Config) error {
	var err error
	if err := normalizeProxyConfig(&cfg.Proxy); err != nil {
		return err
	}
	cfg.PublicURL, err = resolvePublicURL(cfg.PublicURL, cfg.Proxy)
	if err != nil {
		return fmt.Errorf("invalid public url: %w", err)
	}
	cfg.PublicOrigins, err = normalizePublicOrigins(cfg.PublicOrigins)
	if err != nil {
		return fmt.Errorf("invalid public origins: %w", err)
	}
	if cfg.PublicURL != "" {
		cfg.PublicOrigins, err = normalizePublicOrigins(
			append(cfg.PublicOrigins, cfg.PublicURL),
		)
		if err != nil {
			return fmt.Errorf("invalid public url: %w", err)
		}
	}
	return nil
}

func resolvePublicURL(value string, proxyCfg ProxyConfig) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	if u == nil || u.Host == "" {
		return "", fmt.Errorf("%q must include a host", value)
	}
	if u.User != nil {
		return "", fmt.Errorf("%q must not include user info", value)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q must not include query or fragment", value)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%q must not include a path", value)
	}
	if proxyCfg.Mode != "caddy" {
		return normalizePublicOrigin(value)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%q must use http or https", value)
	}
	resolvedPort := proxyCfg.PublicPort
	if resolvedPort == 0 {
		resolvedPort = 8443
	}
	if rawPort := u.Port(); rawPort != "" {
		explicitPort, err := strconv.Atoi(rawPort)
		if err != nil || explicitPort < 1 || explicitPort > 65535 {
			return "", fmt.Errorf("%q has an invalid port", value)
		}
		if proxyCfg.PublicPort != 0 && explicitPort != proxyCfg.PublicPort {
			return "", fmt.Errorf(
				"%q conflicts with configured public port %d",
				value, proxyCfg.PublicPort,
			)
		}
		resolvedPort = explicitPort
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%q must include a host", value)
	}
	if resolvedPort == defaultPortForScheme(scheme) {
		return scheme + "://" + hostLiteral(host), nil
	}
	return scheme + "://" + net.JoinHostPort(host, strconv.Itoa(resolvedPort)), nil
}

func normalizePublicOrigins(origins []string) ([]string, error) {
	if len(origins) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(origins))
	seen := make(map[string]bool, len(origins))
	for _, origin := range origins {
		if strings.TrimSpace(origin) == "" {
			continue
		}
		norm, err := normalizePublicOrigin(origin)
		if err != nil {
			return nil, err
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		normalized = append(normalized, norm)
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	return normalized, nil
}

func normalizePublicOrigin(origin string) (string, error) {
	origin = strings.TrimSpace(origin)
	u, err := url.Parse(origin)
	if err != nil {
		return "", fmt.Errorf("parsing %q: %w", origin, err)
	}
	if u == nil || u.Host == "" {
		return "", fmt.Errorf("%q must include a host", origin)
	}
	if u.User != nil {
		return "", fmt.Errorf("%q must not include user info", origin)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q must not include query or fragment", origin)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("%q must not include a path", origin)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%q must use http or https", origin)
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("%q must include a host", origin)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("%q has an invalid port", origin)
		}
		if n == defaultPortForScheme(scheme) {
			port = ""
		}
	}

	if port == "" {
		return scheme + "://" + hostLiteral(host), nil
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func normalizeProxyConfig(cfg *ProxyConfig) error {
	if cfg == nil {
		return nil
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch cfg.Mode {
	case "", "caddy":
	default:
		return fmt.Errorf("invalid proxy mode %q", cfg.Mode)
	}
	if cfg.Mode == "caddy" && strings.TrimSpace(cfg.Bin) == "" {
		cfg.Bin = "caddy"
	}
	if cfg.Mode == "caddy" {
		cfg.BindHost = strings.TrimSpace(cfg.BindHost)
		if cfg.BindHost == "" {
			cfg.BindHost = "127.0.0.1"
		}
		if cfg.PublicPort < 0 || cfg.PublicPort > 65535 {
			return fmt.Errorf("invalid public port %d", cfg.PublicPort)
		}
	}
	var err error
	cfg.AllowedSubnets, err = normalizeAllowedSubnets(cfg.AllowedSubnets)
	if err != nil {
		return fmt.Errorf("invalid allowed subnets: %w", err)
	}
	return nil
}

func normalizeAllowedSubnets(subnets []string) ([]string, error) {
	if len(subnets) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(subnets))
	seen := make(map[string]bool, len(subnets))
	for _, subnet := range subnets {
		subnet = strings.TrimSpace(subnet)
		if subnet == "" {
			continue
		}
		network, err := parseAllowedSubnet(subnet)
		if err != nil {
			return nil, fmt.Errorf("parsing %q: %w", subnet, err)
		}
		value := network.String()
		if seen[value] {
			continue
		}
		seen[value] = true
		normalized = append(normalized, value)
	}
	if len(normalized) == 0 {
		return nil, nil
	}
	return normalized, nil
}

func parseAllowedSubnet(value string) (*net.IPNet, error) {
	_, network, err := net.ParseCIDR(value)
	if err == nil {
		return network, nil
	}
	expanded, ok := expandIPv4CIDRShorthand(value)
	if !ok {
		return nil, err
	}
	_, network, err = net.ParseCIDR(expanded)
	if err != nil {
		return nil, err
	}
	return network, nil
}

func expandIPv4CIDRShorthand(value string) (string, bool) {
	addr, mask, ok := strings.Cut(value, "/")
	if !ok || strings.Contains(addr, ":") {
		return "", false
	}
	parts := strings.Split(addr, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return "", false
	}
	if slices.Contains(parts, "") {
		return "", false
	}
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	return strings.Join(parts, ".") + "/" + mask, true
}

func defaultPortForScheme(scheme string) int {
	if scheme == "https" {
		return 443
	}
	return 80
}

func hostLiteral(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
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

// ResolvePG returns a copy of PG config with defaults applied
// and environment variables expanded in URL.
func (c *Config) ResolvePG() (PGConfig, error) {
	pg := c.PG
	if pg.URL != "" {
		expanded, err := expandBracedEnv(pg.URL)
		if err != nil {
			return pg, fmt.Errorf("expanding url: %w", err)
		}
		pg.URL = expanded
	}
	if pg.Schema == "" {
		pg.Schema = "agentsview"
	}
	if pg.MachineName == "" {
		h, err := os.Hostname()
		if err != nil {
			return pg, fmt.Errorf("os.Hostname failed (%w); set machine_name explicitly in config", err)
		}
		pg.MachineName = h
	}
	return pg, nil
}

var (
	bracedEnvPattern      = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	bareEnvPattern        = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	partialBareEnvPattern = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
)

// bareEnvWarned tracks which bare $VAR names have already been warned
// about, so each distinct variable triggers a warning at most once.
var bareEnvWarned sync.Map

// ResetBareEnvWarned clears the warning dedup state. Exported for tests.
func ResetBareEnvWarned() {
	bareEnvWarned.Range(func(k, _ any) bool { bareEnvWarned.Delete(k); return true })
}

// expandBracedEnv expands ${VAR} references in s. As a convenience,
// if the entire string is a single bare $VAR (e.g. "$PGURL"), it is
// expanded as a whole-string shortcut. Bare $VAR references embedded
// in a larger string (e.g. "postgres://$USER@host") are NOT expanded;
// use ${VAR} syntax instead.
func expandBracedEnv(s string) (string, error) {
	if parts := bareEnvPattern.FindStringSubmatch(s); parts != nil {
		val, ok := os.LookupEnv(parts[1])
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", parts[1])
		}
		return val, nil
	}

	// Warn about bare $VAR references that won't be expanded.
	if remaining := bracedEnvPattern.ReplaceAllString(s, ""); partialBareEnvPattern.MatchString(remaining) {
		for _, m := range partialBareEnvPattern.FindAllStringSubmatch(remaining, -1) {
			if _, set := os.LookupEnv(m[1]); set {
				if _, warned := bareEnvWarned.LoadOrStore(m[1], true); !warned {
					log.Printf("warning: pg.url contains bare $%s which will NOT be expanded; use ${%s} syntax instead", m[1], m[1])
				}
			}
		}
	}

	var missingVars []string
	result := bracedEnvPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := bracedEnvPattern.FindStringSubmatch(match)[1]
		val, ok := os.LookupEnv(name)
		if !ok {
			missingVars = append(missingVars, name)
			return ""
		}
		return val
	})
	if len(missingVars) > 0 {
		return "", fmt.Errorf("environment variable(s) not set: %s",
			strings.Join(missingVars, ", "))
	}
	return result, nil
}

// SaveTerminalConfig persists terminal settings to the config file.
func (c *Config) SaveTerminalConfig(tc TerminalConfig) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing, err := c.readConfigMap()
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	existing["terminal"] = tc
	if err := c.writeConfigMap(existing); err != nil {
		return err
	}
	c.Terminal = tc
	return nil
}

// SaveSettings persists a partial settings update to the config file.
// The patch map contains config keys mapped to their new values. Only
// the keys present in patch are written; other config keys are preserved.
func (c *Config) SaveSettings(patch map[string]any) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing, err := c.readConfigMap()
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	maps.Copy(existing, patch)

	// When require_auth is written, remove the legacy
	// remote_access key so it cannot override on next load.
	if _, ok := patch["require_auth"]; ok {
		delete(existing, "remote_access")
	}

	if err := c.writeConfigMap(existing); err != nil {
		return err
	}

	// Update in-memory config for known keys.
	if v, ok := patch["terminal"]; ok {
		if tc, ok := v.(TerminalConfig); ok {
			c.Terminal = tc
		} else if m, ok := v.(map[string]any); ok {
			if s, ok := m["mode"].(string); ok {
				c.Terminal.Mode = s
			}
			if s, ok := m["custom_bin"].(string); ok {
				c.Terminal.CustomBin = s
			}
			if s, ok := m["custom_args"].(string); ok {
				c.Terminal.CustomArgs = s
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
	if v, ok := patch["require_auth"]; ok {
		if b, ok := v.(bool); ok {
			c.RequireAuth = b
		}
	}
	return nil
}

// EnsureAuthToken generates and persists an auth token if one does
// not already exist. Called when require_auth is enabled.
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

	existing, err := c.readConfigMap()
	if err != nil {
		return err
	}

	existing["auth_token"] = token
	return c.writeConfigMap(existing)
}

// SaveGithubToken persists the GitHub token to the config file.
func (c *Config) SaveGithubToken(token string) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}

	existing, err := c.readConfigMap()
	if err != nil {
		return fmt.Errorf("reading config file: %w", err)
	}

	existing["github_token"] = token
	if err := c.writeConfigMap(existing); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	c.GithubToken = token
	return nil
}
