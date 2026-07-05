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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gofrs/flock"
	"github.com/spf13/pflag"
	"go.kenn.io/agentsview/internal/parser"
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

type pgEnvOverrides struct {
	URL         string
	Schema      string
	MachineName string
}

// ResolvedPGTarget is one PostgreSQL target after target selection,
// defaulting, and default-target env overrides are applied.
type ResolvedPGTarget struct {
	Name      string
	Config    PGConfig
	IsDefault bool
}

var pgConfigKeys = map[string]struct{}{
	"url":              {},
	"schema":           {},
	"machine_name":     {},
	"allow_insecure":   {},
	"projects":         {},
	"exclude_projects": {},
}

// DuckDBConfig holds DuckDB mirror and Quack connection settings.
type DuckDBConfig struct {
	Path            string   `toml:"path" json:"path"`
	URL             string   `toml:"url" json:"url"`
	Token           string   `toml:"token" json:"token,omitempty"`
	MachineName     string   `toml:"machine_name" json:"machine_name"`
	AllowInsecure   bool     `toml:"allow_insecure" json:"allow_insecure"`
	Projects        []string `toml:"projects" json:"projects,omitempty"`
	ExcludeProjects []string `toml:"exclude_projects" json:"exclude_projects,omitempty"`
}

// VectorConfig holds settings for the optional local semantic-search
// vector index (embeddings + vectors.db).
type VectorConfig struct {
	Enabled    bool                   `toml:"enabled" json:"enabled"`
	DBPath     string                 `toml:"db_path" json:"db_path,omitempty"`
	Embeddings VectorEmbeddingsConfig `toml:"embeddings" json:"embeddings"`
	Embed      VectorEmbedConfig      `toml:"embed" json:"embed"`
}

// VectorEmbeddingsConfig configures the OpenAI-compatible embeddings
// endpoint used to generate vectors for indexed content.
type VectorEmbeddingsConfig struct {
	Endpoint  string `toml:"endpoint" json:"endpoint"`
	Model     string `toml:"model" json:"model"`
	Dimension int    `toml:"dimension" json:"dimension"`
	// APIKeyEnv names the environment variable holding the API key.
	// Empty means anonymous access.
	APIKeyEnv string `toml:"api_key_env" json:"api_key_env,omitempty"`
	// BatchSize is the number of inputs sent per HTTP call. Default 32.
	BatchSize int `toml:"batch_size" json:"batch_size"`
	// Timeout is a parseable duration string applied to each HTTP
	// call. Default "30s".
	Timeout string `toml:"timeout" json:"timeout"`
	// MaxRetries is the number of retries for 429/5xx/network errors;
	// 4xx failures are not retried. Default 3.
	MaxRetries int `toml:"max_retries" json:"max_retries"`
	// MaxInputChars caps the rune length of each chunk sent for
	// embedding. Default 8192.
	MaxInputChars int `toml:"max_input_chars" json:"max_input_chars"`
}

// VectorEmbedConfig configures when the daemon runs embedding work.
type VectorEmbedConfig struct {
	// RunAfterSync enables debounced embedding of sync deltas.
	// Defaults to true when unset; read it via RunAfterSyncEnabled.
	RunAfterSync *bool `toml:"run_after_sync" json:"run_after_sync,omitempty"`
	// BackstopInterval is a parseable duration string for a periodic
	// full rescan. Default "24h"; a negative duration disables it.
	BackstopInterval string `toml:"backstop_interval" json:"backstop_interval"`
}

// Validate checks the vector config for internal consistency. It is a
// no-op when the section is disabled.
func (c VectorConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Embeddings.Endpoint == "" {
		return fmt.Errorf("[vector.embeddings] endpoint is required when [vector] is enabled")
	}
	if c.Embeddings.Model == "" {
		return fmt.Errorf("[vector.embeddings] model is required when [vector] is enabled")
	}
	if c.Embeddings.Dimension <= 0 {
		return fmt.Errorf("[vector.embeddings] dimension must be greater than 0 when [vector] is enabled")
	}
	if _, err := time.ParseDuration(c.Embeddings.Timeout); err != nil {
		return fmt.Errorf("[vector.embeddings] invalid timeout %q: %w", c.Embeddings.Timeout, err)
	}
	if _, err := time.ParseDuration(c.Embed.BackstopInterval); err != nil {
		return fmt.Errorf("[vector.embed] invalid backstop_interval %q: %w", c.Embed.BackstopInterval, err)
	}
	return nil
}

// ResolvedDBPath returns DBPath if set, else <dataDir>/vectors.db.
func (c VectorConfig) ResolvedDBPath(dataDir string) string {
	if c.DBPath != "" {
		return c.DBPath
	}
	return filepath.Join(dataDir, "vectors.db")
}

// APIKey reads the API key from the environment variable named by
// APIKeyEnv. Returns "" when APIKeyEnv is unset.
func (c VectorEmbeddingsConfig) APIKey() string {
	if c.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.APIKeyEnv)
}

// RunAfterSyncEnabled reports whether embedding should run after sync,
// defaulting to true when RunAfterSync is unset.
func (c VectorEmbedConfig) RunAfterSyncEnabled() bool {
	if c.RunAfterSync == nil {
		return true
	}
	return *c.RunAfterSync
}

// AutomatedConfig holds user-supplied additions to the
// automated-session classifier. Parse-only; all semantic
// normalization (trim, dedupe, length cap, built-in overlap
// drop) happens inside the db setters.
type AutomatedConfig struct {
	Prefixes     []string `toml:"prefixes" json:"prefixes,omitempty"`
	Substrings   []string `toml:"substrings" json:"substrings,omitempty"`
	ExactMatches []string `toml:"exact_matches" json:"exact_matches,omitempty"`
}

// AgentConfig holds per-agent runtime overrides.
type AgentConfig struct {
	Binary      string `json:"binary,omitempty" toml:"binary"`
	Sandbox     string `json:"sandbox,omitempty" toml:"sandbox"`
	AllowUnsafe bool   `json:"allow_unsafe,omitempty" toml:"allow_unsafe"`
}

type CustomModelRate struct {
	Input         float64 `json:"input" toml:"input"`
	Output        float64 `json:"output" toml:"output"`
	CacheCreation float64 `json:"cache_creation,omitempty" toml:"cache_creation"`
	CacheRead     float64 `json:"cache_read,omitempty" toml:"cache_read"`
}

type RemoteTransport string

const (
	RemoteTransportSSH  RemoteTransport = "ssh"
	RemoteTransportHTTP RemoteTransport = "http"
)

// RemoteHost describes one target for config-driven `agentsview sync`
// fan-out. Host is required. SSH remotes may set User and Port
// (Port 0 means the ssh default of 22). HTTP remotes must set URL
// and Token. A zero/empty Interval disables periodic remote
// sync for this host.
type RemoteHost struct {
	Host      string          `toml:"host" json:"host"`
	Transport RemoteTransport `toml:"transport,omitempty" json:"transport,omitempty"`
	User      string          `toml:"user,omitempty" json:"user,omitempty"`
	Port      int             `toml:"port,omitempty" json:"port,omitempty"`
	URL       string          `toml:"url,omitempty" json:"url,omitempty"`
	Token     string          `toml:"token,omitempty" json:"-"`
	Interval  time.Duration   `toml:"interval,omitempty" json:"interval,omitempty"`
}

// Config holds all application configuration.
type Config struct {
	Host                 string                 `json:"host" toml:"host"`
	Port                 int                    `json:"port" toml:"port"`
	DataDir              string                 `json:"data_dir" toml:"data_dir"`
	DBPath               string                 `json:"-" toml:"-"`
	PublicURL            string                 `json:"public_url,omitempty" toml:"public_url"`
	PublicOrigins        []string               `json:"public_origins,omitempty" toml:"public_origins"`
	Proxy                ProxyConfig            `json:"proxy,omitempty" toml:"proxy"`
	WatchExcludePatterns []string               `json:"watch_exclude_patterns,omitempty" toml:"watch_exclude_patterns"`
	CursorSecret         string                 `json:"cursor_secret" toml:"cursor_secret"`
	CursorAdminAPIKey    string                 `json:"cursor_admin_api_key,omitempty" toml:"cursor_admin_api_key"`
	CursorAdminEmail     string                 `json:"cursor_admin_email,omitempty" toml:"cursor_admin_email"`
	CursorAdminUserID    string                 `json:"cursor_admin_user_id,omitempty" toml:"cursor_admin_user_id"`
	GithubToken          string                 `json:"github_token,omitempty" toml:"github_token"`
	Terminal             TerminalConfig         `json:"terminal,omitempty" toml:"terminal"`
	AuthToken            string                 `json:"auth_token,omitempty" toml:"auth_token"`
	RequireAuth          bool                   `json:"require_auth" toml:"require_auth"`
	NoBrowser            bool                   `json:"no_browser" toml:"no_browser"`
	DisableUpdateCheck   bool                   `json:"disable_update_check" toml:"disable_update_check"`
	NoSync               bool                   `json:"-" toml:"-"`
	PG                   PGConfig               `json:"pg,omitempty" toml:"pg"`
	DefaultPG            string                 `json:"default_pg,omitempty" toml:"default_pg"`
	PGTargets            map[string]PGConfig    `json:"-" toml:"-"`
	DuckDB               DuckDBConfig           `json:"duckdb,omitempty" toml:"duckdb"`
	Vector               VectorConfig           `json:"vector,omitempty" toml:"vector"`
	Automated            AutomatedConfig        `json:"automated,omitempty" toml:"automated"`
	Agent                map[string]AgentConfig `json:"agent,omitempty" toml:"agent"`
	WriteTimeout         time.Duration          `json:"-" toml:"-"`

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

	DaemonIdleTimeout time.Duration `json:"daemon_idle_timeout,omitempty" toml:"daemon_idle_timeout"`

	CustomModelPricing map[string]CustomModelRate `json:"custom_model_pricing,omitempty" toml:"custom_model_pricing"`

	// RemoteHosts is the config-file list of remote targets that
	// `agentsview sync` (with no --host) syncs after the local
	// pass. CLI/config-file only; never serialized to the
	// settings API, so there is no web-UI editing of this list.
	RemoteHosts []RemoteHost `json:"-" toml:"-"`

	// HostExplicit is true when the user passed --host on the CLI.
	// Used to prevent auto-bind to 0.0.0.0 when the user
	// explicitly requested a specific host.
	HostExplicit bool `json:"-" toml:"-"`

	pgEnvOverrides pgEnvOverrides
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

// ValidateRemoteHosts checks the configured remote_hosts entries
// for semantic errors. It checks the trimmed values that loadFile
// already normalized, so what is validated here is exactly what is
// passed to remote sync. Returns an aggregated error naming every
// offending entry, or nil when all entries are valid.
func (c Config) ValidateRemoteHosts() error {
	var problems []string
	seen := make(map[string]int, len(c.RemoteHosts))
	for i, h := range c.RemoteHosts {
		transport := h.Transport
		if transport == "" {
			transport = RemoteTransportSSH
		}
		if h.Host == "" {
			problems = append(problems,
				fmt.Sprintf("entry %d: host is required", i+1))
		}
		if trimmed := strings.TrimSpace(h.Host); isSSHOptionShaped(h.Host) {
			problems = append(problems,
				fmt.Sprintf("entry %d: host must not begin with '-' (got %q)",
					i+1, trimmed))
		}
		if trimmed := strings.TrimSpace(h.User); isSSHOptionShaped(h.User) {
			problems = append(problems,
				fmt.Sprintf("entry %d (%q): user must not begin with '-' (got %q)",
					i+1, h.Host, trimmed))
		}
		if h.Port < 0 || h.Port > 65535 {
			problems = append(problems,
				fmt.Sprintf("entry %d (%q): invalid port %d",
					i+1, h.Host, h.Port))
		}
		if h.Interval < 0 {
			problems = append(problems,
				fmt.Sprintf("entry %d (%q): invalid interval %s",
					i+1, h.Host, h.Interval))
		}
		switch transport {
		case RemoteTransportSSH:
			if h.URL != "" {
				problems = append(problems,
					fmt.Sprintf("entry %d (%q): url is only valid for http",
						i+1, h.Host))
			}
			if h.Token != "" {
				problems = append(problems,
					fmt.Sprintf("entry %d (%q): token is only valid for http",
						i+1, h.Host))
			}
		case RemoteTransportHTTP:
			if h.User != "" {
				problems = append(problems,
					fmt.Sprintf("entry %d (%q): user is only valid for ssh",
						i+1, h.Host))
			}
			if h.Port != 0 {
				problems = append(problems,
					fmt.Sprintf("entry %d (%q): port is only valid for ssh",
						i+1, h.Host))
			}
			if err := validateRemoteHTTPURL(h.URL); err != nil {
				problems = append(problems,
					fmt.Sprintf("entry %d (%q): %v",
						i+1, h.Host, err))
			}
			if h.Token == "" {
				problems = append(problems,
					fmt.Sprintf("entry %d (%q): token is required for http",
						i+1, h.Host))
			}
		default:
			problems = append(problems,
				fmt.Sprintf("entry %d (%q): invalid transport %q",
					i+1, h.Host, h.Transport))
		}
		// Remote sync namespaces sessions and the skip cache by
		// host alone (see ssh.RemoteSync), so two entries sharing a
		// host collide regardless of user/port. Reject duplicates
		// rather than silently share or overwrite cached state.
		if h.Host != "" {
			if first, ok := seen[h.Host]; ok {
				problems = append(problems,
					fmt.Sprintf("entry %d: duplicate host %q (already at entry %d)",
						i+1, h.Host, first))
			} else {
				seen[h.Host] = i + 1
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("remote_hosts: %s",
			strings.Join(problems, "; "))
	}
	return nil
}

func validateRemoteHTTPURL(raw string) error {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url must use http or https")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("url must include a host")
	}
	if u.User != nil {
		return fmt.Errorf("url must not include userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return fmt.Errorf("url must not include query")
	}
	if u.Fragment != "" || u.RawFragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("url must not include fragment")
	}
	return nil
}

func isSSHOptionShaped(value string) bool {
	return strings.HasPrefix(strings.TrimSpace(value), "-")
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
		root := ""
		if def.DefaultRootEnvVar != "" {
			root = os.Getenv(def.DefaultRootEnvVar)
		}
		for i, rel := range def.DefaultDirs {
			if root != "" {
				dirs[i] = reRootDefaultDir(root, rel)
				continue
			}
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
		DaemonIdleTimeout:              20 * time.Minute,
		Agent:                          map[string]AgentConfig{},
		Vector: VectorConfig{
			Embeddings: VectorEmbeddingsConfig{
				BatchSize:     32,
				Timeout:       "30s",
				MaxRetries:    3,
				MaxInputChars: 8192,
			},
			Embed: VectorEmbedConfig{
				BackstopInterval: "24h",
			},
		},
	}, nil
}

func reRootDefaultDir(root, rel string) string {
	rel = filepath.Clean(rel)
	if _, tail, ok := strings.Cut(rel, string(filepath.Separator)); ok && tail != "" {
		return filepath.Join(root, tail)
	}
	return root
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

// LoadDuckDBServePFlags builds a DuckDB serve config from a parsed Cobra/pflag
// FlagSet. It intentionally uses the same isolated serve defaults as pg serve.
func LoadDuckDBServePFlags(fs *pflag.FlagSet) (Config, error) {
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

// LoadReadOnly builds a Config from defaults, env, and config.toml without
// writing config migrations or generated secrets. Use it for diagnostic
// commands that must not mutate user state.
func LoadReadOnly() (Config, error) {
	cfg, err := Default()
	if err != nil {
		return cfg, err
	}
	cfg.loadEnv()

	if err := cfg.loadFileReadOnly(); err != nil {
		return cfg, fmt.Errorf("loading config file: %w", err)
	}
	if err := finalize(&cfg); err != nil {
		return cfg, err
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
	return c.withConfigLock(func() error {
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
	})
}

func (c *Config) loadFile() error {
	return c.loadFileWithMigration(true)
}

func (c *Config) loadFileReadOnly() error {
	return c.loadFileWithMigration(false)
}

func (c *Config) loadFileWithMigration(migrate bool) error {
	if migrate {
		if err := c.migrateJSONToTOML(); err != nil {
			return err
		}
	}

	path := c.configPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if migrate {
			return nil
		}
		return c.loadLegacyJSONReadOnly()
	} else if err != nil {
		return fmt.Errorf("checking config: %w", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	return c.applyConfigTOML(string(data))
}

func (c *Config) loadLegacyJSONReadOnly() error {
	data, err := os.ReadFile(c.jsonConfigPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading config.json: %w", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parsing config.json: %w", err)
	}

	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return fmt.Errorf("encoding config.json: %w", err)
	}
	return c.applyConfigTOML(buf.String())
}

func (c *Config) applyConfigTOML(data string) error {
	var file struct {
		GithubToken                    string                     `toml:"github_token"`
		CursorSecret                   string                     `toml:"cursor_secret"`
		CursorAdminAPIKey              string                     `toml:"cursor_admin_api_key"`
		CursorAdminEmail               string                     `toml:"cursor_admin_email"`
		CursorAdminUserID              string                     `toml:"cursor_admin_user_id"`
		Host                           string                     `toml:"host"`
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
		DefaultPG                      string                     `toml:"default_pg"`
		PG                             PGConfig                   `toml:"pg"`
		DuckDB                         DuckDBConfig               `toml:"duckdb"`
		Vector                         VectorConfig               `toml:"vector"`
		Automated                      AutomatedConfig            `toml:"automated"`
		Agent                          map[string]AgentConfig     `toml:"agent"`
		EventsCoalesceInterval         time.Duration              `toml:"events_coalesce_interval"`
		DaemonIdleTimeout              time.Duration              `toml:"daemon_idle_timeout"`
		CustomModelPricing             map[string]CustomModelRate `toml:"custom_model_pricing"`
		RemoteHosts                    []RemoteHost               `toml:"remote_hosts"`
	}
	meta, err := toml.Decode(data, &file)
	if err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}
	var raw map[string]any
	if _, err := toml.Decode(data, &raw); err != nil {
		return fmt.Errorf("parsing config raw: %w", err)
	}
	if file.GithubToken != "" {
		c.GithubToken = file.GithubToken
	}
	if file.CursorSecret != "" {
		c.CursorSecret = file.CursorSecret
	}
	if file.CursorAdminAPIKey != "" && c.CursorAdminAPIKey == "" {
		c.CursorAdminAPIKey = file.CursorAdminAPIKey
	}
	if file.CursorAdminEmail != "" && c.CursorAdminEmail == "" {
		c.CursorAdminEmail = file.CursorAdminEmail
	}
	if file.CursorAdminUserID != "" && c.CursorAdminUserID == "" {
		c.CursorAdminUserID = file.CursorAdminUserID
	}
	if file.Host != "" {
		c.Host = file.Host
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
	if file.AuthToken != "" && c.AuthToken == "" {
		c.AuthToken = file.AuthToken
	}
	c.RequireAuth = file.RequireAuth || file.RemoteAccess
	c.DisableUpdateCheck = file.DisableUpdateCheck
	if meta.IsDefined("default_pg") {
		c.DefaultPG = normalizePGTargetName(file.DefaultPG)
	}
	legacyPG, namedPG, err := parsePGConfigSection(raw["pg"])
	if err != nil {
		return fmt.Errorf("pg: %w", err)
	}
	if len(namedPG) > 0 {
		c.PG = PGConfig{}
		c.PGTargets = namedPG
	} else {
		c.PGTargets = nil
		if legacyPG.URL != "" {
			c.PG.URL = legacyPG.URL
		}
		if legacyPG.Schema != "" {
			c.PG.Schema = legacyPG.Schema
		}
		if legacyPG.MachineName != "" {
			c.PG.MachineName = legacyPG.MachineName
		}
		if legacyPG.AllowInsecure {
			c.PG.AllowInsecure = true
		}
		if legacyPG.Projects != nil {
			c.PG.Projects = legacyPG.Projects
		}
		if legacyPG.ExcludeProjects != nil {
			c.PG.ExcludeProjects = legacyPG.ExcludeProjects
		}
	}
	// Merge duckdb field-by-field so env vars override only
	// the fields they set, preserving config-file settings.
	if file.DuckDB.Path != "" && c.DuckDB.Path == "" {
		c.DuckDB.Path = file.DuckDB.Path
	}
	if file.DuckDB.URL != "" && c.DuckDB.URL == "" {
		c.DuckDB.URL = file.DuckDB.URL
	}
	if file.DuckDB.Token != "" && c.DuckDB.Token == "" {
		c.DuckDB.Token = file.DuckDB.Token
	}
	if file.DuckDB.MachineName != "" && c.DuckDB.MachineName == "" {
		c.DuckDB.MachineName = file.DuckDB.MachineName
	}
	if file.DuckDB.AllowInsecure {
		c.DuckDB.AllowInsecure = true
	}
	if file.DuckDB.Projects != nil && c.DuckDB.Projects == nil {
		c.DuckDB.Projects = file.DuckDB.Projects
	}
	if file.DuckDB.ExcludeProjects != nil && c.DuckDB.ExcludeProjects == nil {
		c.DuckDB.ExcludeProjects = file.DuckDB.ExcludeProjects
	}
	if file.Vector.Enabled {
		c.Vector.Enabled = true
	}
	if file.Vector.DBPath != "" {
		c.Vector.DBPath = file.Vector.DBPath
	}
	if file.Vector.Embeddings.Endpoint != "" {
		c.Vector.Embeddings.Endpoint = file.Vector.Embeddings.Endpoint
	}
	if file.Vector.Embeddings.Model != "" {
		c.Vector.Embeddings.Model = file.Vector.Embeddings.Model
	}
	if file.Vector.Embeddings.Dimension != 0 {
		c.Vector.Embeddings.Dimension = file.Vector.Embeddings.Dimension
	}
	if file.Vector.Embeddings.APIKeyEnv != "" {
		c.Vector.Embeddings.APIKeyEnv = file.Vector.Embeddings.APIKeyEnv
	}
	// IsDefined distinguishes "unset" (keep the default) from an
	// explicit zero value, e.g. max_retries = 0 to disable retries.
	if meta.IsDefined("vector", "embeddings", "batch_size") {
		c.Vector.Embeddings.BatchSize = file.Vector.Embeddings.BatchSize
	}
	if file.Vector.Embeddings.Timeout != "" {
		c.Vector.Embeddings.Timeout = file.Vector.Embeddings.Timeout
	}
	if meta.IsDefined("vector", "embeddings", "max_retries") {
		c.Vector.Embeddings.MaxRetries = file.Vector.Embeddings.MaxRetries
	}
	if meta.IsDefined("vector", "embeddings", "max_input_chars") {
		c.Vector.Embeddings.MaxInputChars = file.Vector.Embeddings.MaxInputChars
	}
	if file.Vector.Embed.RunAfterSync != nil {
		c.Vector.Embed.RunAfterSync = file.Vector.Embed.RunAfterSync
	}
	if file.Vector.Embed.BackstopInterval != "" {
		c.Vector.Embed.BackstopInterval = file.Vector.Embed.BackstopInterval
	}
	// IsDefined distinguishes "unset" (leave default 10s) from an
	// explicit "0s" (disable coalescing). Checking != 0 would silently
	// ignore the latter.
	if meta.IsDefined("events_coalesce_interval") {
		c.EventsCoalesceInterval = file.EventsCoalesceInterval
	}
	if meta.IsDefined("daemon_idle_timeout") {
		c.DaemonIdleTimeout = file.DaemonIdleTimeout
	}
	if file.Automated.Prefixes != nil {
		c.Automated.Prefixes = file.Automated.Prefixes
	}
	if file.Automated.Substrings != nil {
		c.Automated.Substrings = file.Automated.Substrings
	}
	if file.Automated.ExactMatches != nil {
		c.Automated.ExactMatches = file.Automated.ExactMatches
	}
	if len(file.Agent) > 0 {
		if c.Agent == nil {
			c.Agent = map[string]AgentConfig{}
		}
		for name, cfg := range file.Agent {
			name = strings.TrimSpace(strings.ToLower(name))
			if name == "" {
				continue
			}
			cfg.Binary = strings.TrimSpace(cfg.Binary)
			c.Agent[name] = cfg
		}
	}
	if len(file.CustomModelPricing) > 0 {
		c.CustomModelPricing = file.CustomModelPricing
	}
	if len(file.RemoteHosts) > 0 {
		hosts := make([]RemoteHost, len(file.RemoteHosts))
		for i, h := range file.RemoteHosts {
			hosts[i] = RemoteHost{
				Host:      strings.TrimSpace(h.Host),
				Transport: RemoteTransport(strings.TrimSpace(string(h.Transport))),
				User:      strings.TrimSpace(h.User),
				Port:      h.Port,
				URL:       strings.TrimSpace(h.URL),
				Token:     strings.TrimSpace(h.Token),
				Interval:  h.Interval,
			}
		}
		c.RemoteHosts = hosts
	}

	// Parse config-file dir arrays for agents that have a
	// ConfigKey. Only apply when not already set by env var.
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

	return c.withConfigLock(func() error {
		existing, err := c.readConfigMap()
		if err != nil {
			return err
		}
		if secret, ok := existing["cursor_secret"].(string); ok && secret != "" {
			c.CursorSecret = secret
			return nil
		}

		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating secret: %w", err)
		}
		secret := base64.StdEncoding.EncodeToString(b)
		existing["cursor_secret"] = secret
		if err := c.writeConfigMap(existing); err != nil {
			return err
		}
		c.CursorSecret = secret
		return nil
	})
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

func (c *Config) withConfigLock(fn func() error) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data dir: %w", err)
	}
	lock := flock.New(c.configPath() + ".lock")
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking config: %w", err)
	}
	defer func() {
		_ = lock.Unlock()
	}()
	return fn()
}

// dataDirFromEnv returns the data directory from the environment, preferring
// AGENTSVIEW_DATA_DIR and falling back to the legacy AGENT_VIEWER_DATA_DIR.
// Returns "" when neither is set.
func dataDirFromEnv() string {
	if v := os.Getenv("AGENTSVIEW_DATA_DIR"); v != "" {
		return v
	}
	return os.Getenv("AGENT_VIEWER_DATA_DIR")
}

func (c *Config) loadEnv() {
	for _, def := range parser.Registry {
		if v := os.Getenv(def.EnvVar); v != "" {
			c.AgentDirs[def.Type] = []string{v}
			c.agentDirSource[def.Type] = dirEnv
		}
	}
	if v := dataDirFromEnv(); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("AGENTSVIEW_AUTH_TOKEN"); v != "" {
		c.AuthToken = v
	}
	if v := os.Getenv("AGENTSVIEW_PG_URL"); v != "" {
		c.pgEnvOverrides.URL = v
	}
	if v := os.Getenv("AGENTSVIEW_PG_SCHEMA"); v != "" {
		c.pgEnvOverrides.Schema = v
	}
	if v := os.Getenv("AGENTSVIEW_PG_MACHINE"); v != "" {
		c.pgEnvOverrides.MachineName = v
	}
	if v := firstEnv(
		"AGENTSVIEW_CURSOR_ADMIN_API_KEY",
		"CURSOR_ADMIN_API_KEY",
	); v != "" {
		c.CursorAdminAPIKey = v
	}
	if v := firstEnv(
		"AGENTSVIEW_CURSOR_ADMIN_EMAIL",
		"CURSOR_ADMIN_EMAIL",
	); v != "" {
		c.CursorAdminEmail = v
	}
	if v := firstEnv(
		"AGENTSVIEW_CURSOR_ADMIN_USER_ID",
		"CURSOR_ADMIN_USER_ID",
	); v != "" {
		c.CursorAdminUserID = v
	}
	if v := os.Getenv("AGENTSVIEW_DUCKDB_PATH"); v != "" {
		c.DuckDB.Path = v
	}
	if v := os.Getenv("AGENTSVIEW_DUCKDB_URL"); v != "" {
		c.DuckDB.URL = v
	}
	if v := os.Getenv("AGENTSVIEW_DUCKDB_TOKEN"); v != "" {
		c.DuckDB.Token = v
	}
	if v := os.Getenv("AGENTSVIEW_DUCKDB_MACHINE"); v != "" {
		c.DuckDB.MachineName = v
	}
	if v := os.Getenv("AGENTSVIEW_DISABLE_UPDATE_CHECK"); v != "" {
		c.DisableUpdateCheck = v == "1" || v == "true"
	}
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if v := os.Getenv(name); v != "" {
			return v
		}
	}
	return ""
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
	case "pg":
		// Read-routing only. The CLI resolver combines this flag
		// with cfg.PG from env/config and does not persist a new
		// config field for it.
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
	if cfg.DaemonIdleTimeout < 0 {
		return fmt.Errorf("invalid daemon_idle_timeout: %s", cfg.DaemonIdleTimeout)
	}
	if err := cfg.Vector.Validate(); err != nil {
		return err
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

func normalizePGTargetName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}

func isReservedPGTargetName(name string) bool {
	switch normalizePGTargetName(name) {
	case "all", "local":
		return true
	default:
		return false
	}
}

func decodePGConfigMap(raw map[string]any) (PGConfig, error) {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return PGConfig{}, fmt.Errorf("encoding pg config: %w", err)
	}
	var cfg PGConfig
	if _, err := toml.Decode(buf.String(), &cfg); err != nil {
		return PGConfig{}, fmt.Errorf("decoding pg config: %w", err)
	}
	return cfg, nil
}

func parsePGConfigSection(value any) (PGConfig, map[string]PGConfig, error) {
	if value == nil {
		return PGConfig{}, nil, nil
	}
	section, ok := value.(map[string]any)
	if !ok {
		return PGConfig{}, nil, fmt.Errorf("expected [pg] to be a table")
	}
	hasLegacyFields := false
	hasNamedTargets := false
	legacyRaw := make(map[string]any)
	namedTargets := make(map[string]PGConfig)
	seenNames := make(map[string]string)
	for rawName, rawValue := range section {
		name := normalizePGTargetName(rawName)
		if _, ok := pgConfigKeys[name]; ok {
			if _, nested := rawValue.(map[string]any); nested {
				return PGConfig{}, nil, fmt.Errorf(
					"[pg].%s must be a scalar or array field, not a nested table",
					rawName,
				)
			}
			hasLegacyFields = true
			legacyRaw[rawName] = rawValue
			continue
		}
		targetRaw, ok := rawValue.(map[string]any)
		if !ok {
			return PGConfig{}, nil, fmt.Errorf(
				"[pg].%s must be a named target table",
				rawName,
			)
		}
		hasNamedTargets = true
		if name == "" {
			return PGConfig{}, nil, fmt.Errorf(
				"named PG targets must not be blank",
			)
		}
		if isReservedPGTargetName(name) {
			return PGConfig{}, nil, fmt.Errorf(
				"named PG target %q is reserved",
				name,
			)
		}
		if prev, exists := seenNames[name]; exists {
			return PGConfig{}, nil, fmt.Errorf(
				"named PG targets %q and %q normalize to the same name %q",
				prev, rawName, name,
			)
		}
		seenNames[name] = rawName
		targetCfg, err := decodePGConfigMap(targetRaw)
		if err != nil {
			return PGConfig{}, nil, fmt.Errorf(
				"[pg].%s: %w", rawName, err,
			)
		}
		namedTargets[name] = targetCfg
	}
	if hasLegacyFields && hasNamedTargets {
		return PGConfig{}, nil, fmt.Errorf(
			"cannot mix legacy [pg] fields with named [pg.NAME] targets",
		)
	}
	if hasLegacyFields {
		legacyCfg, err := decodePGConfigMap(legacyRaw)
		if err != nil {
			return PGConfig{}, nil, err
		}
		return legacyCfg, nil, nil
	}
	if hasNamedTargets {
		return PGConfig{}, namedTargets, nil
	}
	return PGConfig{}, nil, nil
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
	if v := dataDirFromEnv(); v != "" {
		cfg.DataDir = v
	}
	return cfg.DataDir, nil
}

// DefaultPGTargetName returns the effective named PG target for this config.
func (c *Config) DefaultPGTargetName() (string, error) {
	if len(c.PGTargets) == 0 {
		if c.DefaultPG != "" {
			return "", fmt.Errorf(
				"default_pg requires named [pg.NAME] targets",
			)
		}
		return "", nil
	}
	if c.DefaultPG != "" {
		if _, ok := c.PGTargets[c.DefaultPG]; !ok {
			return "", fmt.Errorf(
				"default_pg %q does not match any named [pg.NAME] target",
				c.DefaultPG,
			)
		}
		return c.DefaultPG, nil
	}
	if len(c.PGTargets) == 1 {
		for name := range c.PGTargets {
			return name, nil
		}
	}
	return "", fmt.Errorf(
		"default_pg is required when more than one [pg.NAME] target is defined",
	)
}

func (c *Config) validatePGTargets() error {
	_, err := c.DefaultPGTargetName()
	return err
}

func (c *Config) PGTargetNames() ([]string, string, error) {
	if err := c.validatePGTargets(); err != nil {
		return nil, "", err
	}
	if len(c.PGTargets) == 0 {
		return nil, "", nil
	}
	defaultName, err := c.DefaultPGTargetName()
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(c.PGTargets))
	for name := range c.PGTargets {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == defaultName {
			return true
		}
		if names[j] == defaultName {
			return false
		}
		return names[i] < names[j]
	})
	return names, defaultName, nil
}

// RawPGTarget returns the configured PG target before env expansion and
// default-field synthesis. An empty name selects the effective default target.
func (c *Config) RawPGTarget(name string) (PGConfig, error) {
	if err := c.validatePGTargets(); err != nil {
		return PGConfig{}, err
	}
	targetName := normalizePGTargetName(name)
	if len(c.PGTargets) == 0 {
		if targetName != "" {
			return PGConfig{}, fmt.Errorf(
				"pg target %q is not configured; config uses a single legacy [pg] block",
				name,
			)
		}
		return c.PG, nil
	}
	if targetName == "" {
		var err error
		targetName, err = c.DefaultPGTargetName()
		if err != nil {
			return PGConfig{}, err
		}
	}
	targetCfg, ok := c.PGTargets[targetName]
	if !ok {
		return PGConfig{}, fmt.Errorf(
			"pg target %q is not configured",
			targetName,
		)
	}
	return targetCfg, nil
}

func (c *Config) resolvePGConfig(
	pg PGConfig, applyDefaultEnv bool,
) (PGConfig, error) {
	if applyDefaultEnv {
		if c.pgEnvOverrides.URL != "" {
			pg.URL = c.pgEnvOverrides.URL
		}
		if c.pgEnvOverrides.Schema != "" {
			pg.Schema = c.pgEnvOverrides.Schema
		}
		if c.pgEnvOverrides.MachineName != "" {
			pg.MachineName = c.pgEnvOverrides.MachineName
		}
	}
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

// ResolvePG returns the effective default PG target with defaults applied
// and environment variables expanded in URL.
func (c *Config) ResolvePG() (PGConfig, error) {
	return c.ResolvePGTarget("")
}

// ResolvePGTarget resolves one named PG target, or the effective default
// target when name is empty. In legacy single-target mode, only the empty
// name is valid.
func (c *Config) ResolvePGTarget(name string) (PGConfig, error) {
	if err := c.validatePGTargets(); err != nil {
		return PGConfig{}, err
	}
	targetName := normalizePGTargetName(name)
	if len(c.PGTargets) == 0 {
		if targetName != "" {
			return PGConfig{}, fmt.Errorf(
				"pg target %q is not configured; config uses a single legacy [pg] block",
				name,
			)
		}
		return c.resolvePGConfig(c.PG, true)
	}
	defaultName, err := c.DefaultPGTargetName()
	if err != nil {
		return PGConfig{}, err
	}
	if targetName == "" {
		targetName = defaultName
	}
	targetCfg, ok := c.PGTargets[targetName]
	if !ok {
		return PGConfig{}, fmt.Errorf(
			"pg target %q is not configured",
			targetName,
		)
	}
	return c.resolvePGConfig(targetCfg, targetName == defaultName)
}

// ResolvePGTargets resolves every configured PG target. Legacy single-target
// mode returns one unnamed default target.
func (c *Config) ResolvePGTargets() ([]ResolvedPGTarget, error) {
	if err := c.validatePGTargets(); err != nil {
		return nil, err
	}
	if len(c.PGTargets) == 0 {
		pg, err := c.resolvePGConfig(c.PG, true)
		if err != nil {
			return nil, err
		}
		return []ResolvedPGTarget{{
			Config:    pg,
			IsDefault: true,
		}}, nil
	}
	names, defaultName, err := c.PGTargetNames()
	if err != nil {
		return nil, err
	}
	targets := make([]ResolvedPGTarget, 0, len(names))
	for _, name := range names {
		targetCfg, err := c.resolvePGConfig(
			c.PGTargets[name], name == defaultName,
		)
		if err != nil {
			return nil, err
		}
		targets = append(targets, ResolvedPGTarget{
			Name:      name,
			Config:    targetCfg,
			IsDefault: name == defaultName,
		})
	}
	return targets, nil
}

// ResolveDuckDB returns a copy of DuckDB config with defaults applied
// and environment variables expanded in path, URL, and token.
func (c *Config) ResolveDuckDB() (DuckDBConfig, error) {
	duck := c.DuckDB
	if duck.Path != "" {
		expanded, err := expandBracedEnv(duck.Path)
		if err != nil {
			return duck, fmt.Errorf("expanding path: %w", err)
		}
		duck.Path = expanded
	}
	if duck.URL != "" {
		expanded, err := expandBracedEnv(duck.URL)
		if err != nil {
			return duck, fmt.Errorf("expanding url: %w", err)
		}
		duck.URL = expanded
	}
	if duck.Token != "" {
		expanded, err := expandBracedEnv(duck.Token)
		if err != nil {
			return duck, fmt.Errorf("expanding token: %w", err)
		}
		duck.Token = expanded
	}
	if duck.Path == "" {
		duck.Path = filepath.Join(c.DataDir, "sessions.duckdb")
	}
	if duck.MachineName == "" {
		h, err := os.Hostname()
		if err != nil {
			return duck, fmt.Errorf("os.Hostname failed (%w); set machine_name explicitly in config", err)
		}
		duck.MachineName = h
	}
	return duck, nil
}

var (
	bracedEnvPattern      = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	bareEnvPattern        = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	partialBareEnvPattern = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
)

// IsEnvDependentURL reports whether s would have environment variables
// expanded by expandBracedEnv: it contains any ${VAR} reference, or the
// whole string is a single bare $VAR shortcut. Embedded bare $VAR
// references (e.g. "postgres://$USER@host") are deliberately NOT expanded
// and so do not count. Callers that must persist a literal URL into a
// context without the shell environment (e.g. a background service) use
// this to reject env-dependent values. It shares the exact patterns
// expandBracedEnv uses so the rejection check cannot drift from the
// expansion semantics.
func IsEnvDependentURL(s string) bool {
	return bracedEnvPattern.MatchString(s) ||
		bareEnvPattern.MatchString(strings.TrimSpace(s))
}

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
	return c.withConfigLock(func() error {
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
	})
}

// SaveSettings persists a partial settings update to the config file.
// The patch map contains config keys mapped to their new values. Only
// the keys present in patch are written; other config keys are preserved.
func (c *Config) SaveSettings(patch map[string]any) error {
	return c.withConfigLock(func() error {
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
	})
}

// EnsureAuthToken generates and persists an auth token if one does
// not already exist. Called when require_auth is enabled.
func (c *Config) EnsureAuthToken() error {
	if c.AuthToken != "" {
		return nil
	}
	return c.withConfigLock(func() error {
		existing, err := c.readConfigMap()
		if err != nil {
			return err
		}
		if token, ok := existing["auth_token"].(string); ok && token != "" {
			c.AuthToken = token
			return nil
		}

		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("generating auth token: %w", err)
		}
		token := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
		existing["auth_token"] = token
		if err := c.writeConfigMap(existing); err != nil {
			return err
		}
		c.AuthToken = token
		return nil
	})
}

// SaveGithubToken persists the GitHub token to the config file.
func (c *Config) SaveGithubToken(token string) error {
	return c.withConfigLock(func() error {
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
	})
}
