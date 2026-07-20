package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// RecallConfig holds recall subsystem settings.
type RecallConfig struct {
	Extract RecallExtractConfig `toml:"extract" json:"extract"`
}

// RecallExtractConfig configures model-backed recall extraction: which
// model distills session transcripts into recall entries, where it is
// served, and how the daemon schedules extraction passes.
//
// Model and Deployment are identity: together with the segmenter, prompts,
// and request shape they fingerprint an extraction generation, so changing
// them builds a new corpus instead of mixing outputs. Servers are
// transport only and deliberately outside that identity — moving the same
// deployment to a new address must not orphan the corpus.
type RecallExtractConfig struct {
	Enabled bool   `toml:"enabled" json:"enabled"`
	Model   string `toml:"model" json:"model"`
	// Deployment labels which serving instance of Model produced the
	// corpus, for setups where two deployments serve different weights
	// under one model name. Optional.
	Deployment string `toml:"deployment" json:"deployment,omitempty"`
	// Server names the entry in Servers used for extraction. Optional
	// when exactly one server is defined.
	Server  string                               `toml:"server" json:"server,omitempty"`
	Servers map[string]RecallExtractServerConfig `toml:"servers" json:"servers"`
	// MaxWindowChars caps each extraction unit's rune length. Part of the
	// generation fingerprint. Default 50000.
	MaxWindowChars int `toml:"max_window_chars" json:"max_window_chars"`
	// MaxTokens caps the model's response length per call. Part of the
	// generation fingerprint; 0 defers to the prompt profile's default.
	MaxTokens int `toml:"max_tokens" json:"max_tokens,omitempty"`
	// QuietPeriod is how long a session must have been ended before it is
	// extracted, so sessions that resume shortly after ending settle
	// first. Default "30m".
	QuietPeriod string `toml:"quiet_period" json:"quiet_period"`
	// BackstopInterval is a parseable duration string for the daemon's
	// periodic catch-up scan. Default "1h"; a negative duration disables
	// it (event-driven passes still run).
	BackstopInterval string `toml:"backstop_interval" json:"backstop_interval"`
	// FailureBackoff is how long a failed session waits before being
	// retried. Default "1h".
	FailureBackoff string                     `toml:"failure_backoff" json:"failure_backoff"`
	Prompts        RecallExtractPromptsConfig `toml:"prompts" json:"prompts"`
	Request        RecallExtractRequestConfig `toml:"request" json:"request"`
}

// RecallExtractServerConfig is one named extraction endpoint: transport
// settings only; model identity lives on RecallExtractConfig.
type RecallExtractServerConfig struct {
	// Endpoint is the OpenAI-compatible base URL, e.g.
	// "http://127.0.0.1:30000/v1".
	Endpoint string `toml:"endpoint" json:"endpoint"`
	// Timeout is a parseable duration string applied to each model call.
	// Distillation calls on local models are slow; default "120s".
	Timeout string `toml:"timeout" json:"timeout"`
	// AllowHTTP opts into plaintext HTTP to a non-loopback host.
	// Extraction sends transcript content to the endpoint; without this
	// explicit opt-in, non-loopback endpoints must use HTTPS.
	AllowHTTP bool `toml:"allow_http" json:"allow_http,omitempty"`
}

// RecallExtractPromptsConfig selects the prompt profile and optional
// per-role override files.
type RecallExtractPromptsConfig struct {
	// Profile names a built-in prompt profile. Empty selects by model
	// name prefix, falling back to the base profile.
	Profile string `toml:"profile" json:"profile,omitempty"`
	// Dir holds optional per-role prompt override files (intent.txt,
	// action.txt, generic.txt). Overrides join the generation fingerprint.
	Dir string `toml:"dir" json:"dir,omitempty"`
}

// RecallExtractRequestConfig overrides request-shape fields the selected
// prompt profile would otherwise set. All fields join the generation
// fingerprint.
type RecallExtractRequestConfig struct {
	// Temperature, when set, overrides the profile's sampling temperature.
	Temperature *float64 `toml:"temperature" json:"temperature,omitempty"`
	// ExtraBody, when set, replaces the profile's extra request fields
	// (e.g. chat_template_kwargs).
	ExtraBody map[string]any `toml:"extra_body" json:"extra_body,omitempty"`
}

// ResolvedServer resolves the configured server selection: Server when
// set, otherwise the only defined server.
func (c RecallExtractConfig) ResolvedServer() (string, RecallExtractServerConfig, error) {
	name := c.Server
	if name == "" && len(c.Servers) == 1 {
		for only := range c.Servers {
			name = only
		}
	}
	s, ok := c.Servers[name]
	if !ok {
		return "", RecallExtractServerConfig{}, fmt.Errorf(
			"[recall.extract] server %q is not a defined server; define it "+
				"under [recall.extract.servers.%s] (have: %s)",
			name, name,
			strings.Join(sortedRecallServerNames(c.Servers), ", "))
	}
	return name, s, nil
}

// Validate checks the extraction config for internal consistency. It is a
// no-op when the section is disabled.
func (c RecallExtractConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf(
			"[recall.extract] model is required when extraction is enabled")
	}
	if len(c.Servers) == 0 {
		return fmt.Errorf(
			"[recall.extract] at least one server is required; define one " +
				"under [recall.extract.servers.<name>]")
	}
	if c.Server == "" && len(c.Servers) > 1 {
		return fmt.Errorf(
			"[recall.extract] server is required when multiple servers are "+
				"defined (have: %s)",
			strings.Join(sortedRecallServerNames(c.Servers), ", "))
	}
	if c.Server != "" {
		if _, ok := c.Servers[c.Server]; !ok {
			return fmt.Errorf(
				"[recall.extract] server %q is not a defined server (have: %s)",
				c.Server,
				strings.Join(sortedRecallServerNames(c.Servers), ", "))
		}
	}
	for _, name := range sortedRecallServerNames(c.Servers) {
		if err := c.Servers[name].validate(name); err != nil {
			return err
		}
	}
	if c.MaxWindowChars <= 0 {
		return fmt.Errorf(
			"[recall.extract] max_window_chars must be greater than 0, got %d",
			c.MaxWindowChars)
	}
	if c.MaxTokens < 0 {
		return fmt.Errorf(
			"[recall.extract] max_tokens must not be negative, got %d",
			c.MaxTokens)
	}
	quiet, err := time.ParseDuration(c.QuietPeriod)
	if err != nil {
		return fmt.Errorf(
			"[recall.extract] invalid quiet_period %q: %w", c.QuietPeriod, err)
	}
	if quiet < 0 {
		return fmt.Errorf(
			"[recall.extract] quiet_period must not be negative, got %q",
			c.QuietPeriod)
	}
	backstop, err := time.ParseDuration(c.BackstopInterval)
	if err != nil {
		return fmt.Errorf(
			"[recall.extract] invalid backstop_interval %q: %w",
			c.BackstopInterval, err)
	}
	if backstop == 0 {
		return fmt.Errorf(
			"[recall.extract] backstop_interval must not be zero; " +
				"use a negative value to disable or omit for the 1h default")
	}
	backoff, err := time.ParseDuration(c.FailureBackoff)
	if err != nil {
		return fmt.Errorf(
			"[recall.extract] invalid failure_backoff %q: %w",
			c.FailureBackoff, err)
	}
	if backoff < 0 {
		return fmt.Errorf(
			"[recall.extract] failure_backoff must not be negative, got %q",
			c.FailureBackoff)
	}
	if backoff == 0 {
		return fmt.Errorf(
			"[recall.extract] failure_backoff must not be zero; a failed " +
				"session would retry a model call on every pass — omit it " +
				"for the 1h default")
	}
	return nil
}

func (s RecallExtractServerConfig) validate(name string) error {
	if strings.TrimSpace(s.Endpoint) == "" {
		return fmt.Errorf(
			"[recall.extract.servers.%s] endpoint is required", name)
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf(
			"[recall.extract.servers.%s] endpoint %q must be an http(s) URL",
			name, RedactedEndpoint(s.Endpoint))
	}
	if err := ValidateExtractTransport(u, s.AllowHTTP); err != nil {
		return fmt.Errorf("[recall.extract.servers.%s] %w", name, err)
	}
	timeout, err := time.ParseDuration(s.Timeout)
	if err != nil {
		return fmt.Errorf(
			"[recall.extract.servers.%s] invalid timeout %q: %w",
			name, s.Timeout, err)
	}
	if timeout <= 0 {
		return fmt.Errorf(
			"[recall.extract.servers.%s] timeout must be positive, got %q: "+
				"without an HTTP deadline one hung request stalls "+
				"extraction indefinitely", name, s.Timeout)
	}
	return nil
}

// RedactedEndpoint returns an endpoint URL safe for errors, logs, and
// display: userinfo can carry Basic-auth credentials (the username alone
// can be an API key), query values and fragments can carry keys, and
// capability-style URLs carry bearer tokens in path segments — and these
// strings land on stderr, in CI logs, and in stored failure messages. The
// scheme, host, and allowlisted parameters stay visible: they identify
// which endpoint failed, which is the diagnostic value, while the path is
// masked whole. Anything that does not parse as an http(s) URL with a
// host fails closed to a constant: url.Parse accepts malformed absolute
// URLs as relative paths, which can carry the credentials in a component
// no field-level masking covers.
func RedactedEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<unparseable endpoint>"
	}
	return redactedEndpointURL(parsed)
}

func redactedEndpointURL(u *url.URL) string {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "<invalid endpoint>"
	}
	redacted := *u
	redacted.User = nil
	if redacted.Path != "" && redacted.Path != "/" {
		redacted.Path = "/REDACTED"
		redacted.RawPath = ""
	}
	if redacted.RawQuery != "" {
		redacted.RawQuery = redactedEndpointQuery(redacted.RawQuery)
	}
	if redacted.Fragment != "" {
		redacted.Fragment = "REDACTED"
		redacted.RawFragment = ""
	}
	return redacted.String()
}

// redactedEndpointQuery masks a raw query string, parsing it explicitly:
// URL.Query() drops the segments it cannot parse along with the error, so
// masking its output and keeping the original on "nothing masked" would
// pass a malformed query — credentials included — through verbatim. A query
// the parser cannot fully account for is masked whole, and a parsed one is
// always re-encoded so no raw byte survives untranslated.
func redactedEndpointQuery(rawQuery string) string {
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "REDACTED"
	}
	for key, values := range query {
		if safeEndpointParams[strings.ToLower(key)] {
			continue
		}
		// A bare token (?opaque-capability-token) parses as a key with an
		// empty value: the key itself is the credential, and masking
		// values cannot hide it. An explicit empty value (?api_key=) is
		// indistinguishable after parsing, so both mask the whole query.
		if slices.Contains(values, "") {
			return "REDACTED"
		}
		for i := range values {
			values[i] = "REDACTED"
		}
		query[key] = values
	}
	return query.Encode()
}

// safeEndpointParams are the only query parameters shown unredacted: values
// that select the API surface rather than authenticate the caller. Every
// other value is masked — credentials travel under too many vendor-specific
// names (api_key, sig, code, sas, ...) for a name pattern to stay ahead of,
// so the redactor fails closed.
var safeEndpointParams = map[string]bool{
	"api-version": true,
}

// ValidateExtractTransport enforces the extraction transport privacy rule
// shared by config validation and the model client's redirect policy:
// transcript content only travels in plaintext to loopback hosts, unless
// allow_http explicitly opts a server in. Applying the same rule to
// redirect targets means a compliant endpoint cannot be downgraded to a
// non-compliant one mid-request.
func ValidateExtractTransport(u *url.URL, allowHTTP bool) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
	default:
		return fmt.Errorf(
			"endpoint %q must use http or https", redactedEndpointURL(u))
	}
	if allowHTTP {
		return nil
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf(
		"endpoint %q sends transcript content over plaintext http to a "+
			"non-loopback host; use https, or set allow_http = true to "+
			"accept that risk explicitly", redactedEndpointURL(u))
}

// normalizedRecallExtractServers fills each named server's unset timeout
// with the 120s default.
func normalizedRecallExtractServers(
	servers map[string]RecallExtractServerConfig,
) map[string]RecallExtractServerConfig {
	out := make(map[string]RecallExtractServerConfig, len(servers))
	for name, s := range servers {
		if s.Timeout == "" {
			s.Timeout = "120s"
		}
		out[name] = s
	}
	return out
}

func sortedRecallServerNames(
	servers map[string]RecallExtractServerConfig,
) []string {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// mergeRecallExtractTOML layers the config file's [recall.extract] section
// over the defaults in c.
func (c *Config) mergeRecallExtractTOML(file RecallConfig, meta toml.MetaData) {
	extract := &c.Recall.Extract
	if file.Extract.Enabled {
		extract.Enabled = true
	}
	if file.Extract.Model != "" {
		extract.Model = file.Extract.Model
	}
	if file.Extract.Deployment != "" {
		extract.Deployment = file.Extract.Deployment
	}
	if file.Extract.Server != "" {
		extract.Server = file.Extract.Server
	}
	if len(file.Extract.Servers) > 0 {
		extract.Servers = normalizedRecallExtractServers(file.Extract.Servers)
	}
	if meta.IsDefined("recall", "extract", "max_window_chars") {
		extract.MaxWindowChars = file.Extract.MaxWindowChars
	}
	if meta.IsDefined("recall", "extract", "max_tokens") {
		extract.MaxTokens = file.Extract.MaxTokens
	}
	if file.Extract.QuietPeriod != "" {
		extract.QuietPeriod = file.Extract.QuietPeriod
	}
	if file.Extract.BackstopInterval != "" {
		extract.BackstopInterval = file.Extract.BackstopInterval
	}
	if file.Extract.FailureBackoff != "" {
		extract.FailureBackoff = file.Extract.FailureBackoff
	}
	if file.Extract.Prompts.Profile != "" {
		extract.Prompts.Profile = file.Extract.Prompts.Profile
	}
	if file.Extract.Prompts.Dir != "" {
		extract.Prompts.Dir = file.Extract.Prompts.Dir
	}
	if file.Extract.Request.Temperature != nil {
		extract.Request.Temperature = file.Extract.Request.Temperature
	}
	if len(file.Extract.Request.ExtraBody) > 0 {
		extract.Request.ExtraBody = file.Extract.Request.ExtraBody
	}
}
