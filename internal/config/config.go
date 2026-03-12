package config

import (
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
	"strconv"
	"slices"
	"strings"
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

// ProxyConfig controls an optional managed reverse proxy.
type ProxyConfig struct {
	// Mode enables a managed proxy implementation.
	// Currently supported: "caddy".
	Mode string `json:"mode,omitempty"`
	// Bin overrides the proxy executable path.
	Bin string `json:"bin,omitempty"`
	// BindHost is the local interface/IP the proxy binds to.
	BindHost string `json:"bind_host,omitempty"`
	// PublicPort is the external port exposed by the proxy.
	PublicPort int `json:"public_port,omitempty"`
	// TLSCert and TLSKey are used by managed HTTPS mode.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// AllowedSubnets restrict inbound clients to these CIDRs.
	AllowedSubnets []string `json:"allowed_subnets,omitempty"`
}

// Config holds all application configuration.
type Config struct {
	Host                 string         `json:"host"`
	Port                 int            `json:"port"`
	NoBrowser            bool           `json:"no_browser"`
	DataDir              string         `json:"data_dir"`
	DBPath               string         `json:"-"`
	PublicURL            string         `json:"public_url,omitempty"`
	PublicOrigins        []string       `json:"public_origins,omitempty"`
	Proxy                ProxyConfig    `json:"proxy,omitempty"`
	WatchExcludePatterns []string       `json:"watch_exclude_patterns,omitempty"`
	CursorSecret         string         `json:"cursor_secret"`
	GithubToken          string         `json:"github_token,omitempty"`
	Terminal             TerminalConfig `json:"terminal,omitempty"`
	AuthToken            string         `json:"auth_token,omitempty"`
	RemoteAccess         bool           `json:"remote_access"`
	WriteTimeout         time.Duration  `json:"-"`

	// AgentDirs maps each AgentType to its configured
	// directories. Single-dir agents store a one-element
	// slice; unconfigured agents use nil.
	AgentDirs map[parser.AgentType][]string `json:"-"`

	// agentDirSource tracks how each agent's dirs were
	// set so loadFile doesn't override env-set values.
	agentDirSource map[parser.AgentType]dirSource

	ResultContentBlockedCategories []string `json:"result_content_blocked_categories,omitempty"`

	// HostExplicit is true when the user passed --host on the CLI.
	// Used to prevent auto-bind to 0.0.0.0 when the user
	// explicitly requested a specific host.
	HostExplicit bool `json:"-"`
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
		PublicURL                      string         `json:"public_url"`
		PublicOrigins                  []string       `json:"public_origins"`
		Proxy                          ProxyConfig    `json:"proxy"`
		WatchExcludePatterns           []string       `json:"watch_exclude_patterns"`
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
			cfg.HostExplicit = true
		case "port":
			// flag already validated the int; ignore parse error
			cfg.Port, _ = strconv.Atoi(f.Value.String())
		case "public-url":
			cfg.PublicURL = f.Value.String()
		case "public-origin":
			cfg.PublicOrigins = splitFlagList(f.Value.String())
		case "proxy":
			cfg.Proxy.Mode = f.Value.String()
		case "caddy-bin":
			cfg.Proxy.Bin = f.Value.String()
		case "proxy-bind-host":
			cfg.Proxy.BindHost = f.Value.String()
		case "public-port":
			cfg.Proxy.PublicPort, _ = strconv.Atoi(f.Value.String())
		case "tls-cert":
			cfg.Proxy.TLSCert = f.Value.String()
		case "tls-key":
			cfg.Proxy.TLSKey = f.Value.String()
		case "allowed-subnet":
			cfg.Proxy.AllowedSubnets = splitFlagList(f.Value.String())
		case "no-browser":
			cfg.NoBrowser = f.Value.String() == "true"
		}
	})
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

	maps.Copy(existing, patch)

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
