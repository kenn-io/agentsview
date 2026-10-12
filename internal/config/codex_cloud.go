package config

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

const (
	defaultCodexCloudEndpoint      = "wss://codex-cloud-backend.chatgpt.com/"
	defaultCodexCloudUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2"
)

// CodexCloudConfig is the [agents.codex-cloud] table. Fetching from
// OpenAI is off unless Enabled is true and Err is nil.
type CodexCloudConfig struct {
	// Present reports that config.toml has the table.
	Present       bool
	Enabled       bool
	Endpoint      *url.URL
	UsageEndpoint *url.URL
	LiveTokens    bool
	// Err holds the first problem in the table. It turns this source off
	// without failing the rest of the config.
	Err *CodexCloudConfigError
}

// FetchEnabled reports whether the table allows contacting OpenAI.
func (c CodexCloudConfig) FetchEnabled() bool {
	return c.Enabled && c.Err == nil
}

// CodexCloudConfigError is a problem in [agents.codex-cloud].
type CodexCloudConfigError struct {
	Path    string
	Key     string
	Reason  string
	Unknown bool
}

func (e *CodexCloudConfigError) Error() string {
	if e.Unknown {
		return fmt.Sprintf(
			"%s has an unknown setting %s under [agents.codex-cloud]. "+
				"Remove it or correct its name. Cloud import is off until then.",
			e.Path, e.Key,
		)
	}
	return fmt.Sprintf(
		"%s has an invalid value for %s under [agents.codex-cloud]: %s. "+
			"Cloud import is off until then.",
		e.Path, e.Key, e.Reason,
	)
}

func defaultCodexCloudConfig() CodexCloudConfig {
	endpoint, _ := url.Parse(defaultCodexCloudEndpoint)
	usage, _ := url.Parse(defaultCodexCloudUsageEndpoint)
	return CodexCloudConfig{Endpoint: endpoint, UsageEndpoint: usage, LiveTokens: true}
}

// parseCodexCloudTable reads [agents.codex-cloud] from the decoded agents
// table. Problems land in Err rather than failing the config load.
func parseCodexCloudTable(cfgPath string, agents any) CodexCloudConfig {
	cfg := defaultCodexCloudConfig()
	table, ok := agents.(map[string]any)
	if !ok {
		return cfg
	}
	raw, exists := table[string(parser.AgentCodexCloud)]
	if !exists {
		return cfg
	}
	cfg.Present = true
	fail := func(key, reason string, unknown bool) CodexCloudConfig {
		off := defaultCodexCloudConfig()
		off.Present = true
		off.Err = &CodexCloudConfigError{Path: cfgPath, Key: key, Reason: reason, Unknown: unknown}
		return off
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return fail(string(parser.AgentCodexCloud), "expected a table", false)
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		value := values[key]
		switch key {
		case "enabled", "live_tokens":
			flag, ok := value.(bool)
			if !ok {
				return fail(key, "expected true or false", false)
			}
			if key == "enabled" {
				cfg.Enabled = flag
			} else {
				cfg.LiveTokens = flag
			}
		case "endpoint":
			u, reason := codexCloudURL(value, "wss", "ws")
			if reason != "" {
				return fail(key, reason, false)
			}
			cfg.Endpoint = u
		case "usage_endpoint":
			u, reason := codexCloudURL(value, "https", "http")
			if reason != "" {
				return fail(key, reason, false)
			}
			cfg.UsageEndpoint = u
		default:
			return fail(key, "", true)
		}
	}
	return cfg
}

// codexCloudURL accepts a URL on an OpenAI host over the secure scheme, or
// on a loopback address over either scheme. The token goes to this URL, so
// anything else is refused.
func codexCloudURL(value any, secure, plain string) (*url.URL, string) {
	text, ok := value.(string)
	if !ok {
		return nil, "expected a URL string"
	}
	u, err := url.Parse(strings.TrimSpace(text))
	if err != nil || u.Host == "" {
		return nil, "expected an absolute URL"
	}
	if u.User != nil {
		return nil, "the URL must not contain a user name or password"
	}
	host := strings.ToLower(u.Hostname())
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil {
		loopback = ip.IsLoopback()
	}
	switch {
	case u.Scheme == secure && (loopback || openAIHost(host)):
		return u, ""
	case u.Scheme == plain && loopback:
		return u, ""
	case u.Scheme == plain:
		return nil, fmt.Sprintf("%s is allowed only for loopback addresses; use %s", plain, secure)
	case u.Scheme != secure:
		return nil, fmt.Sprintf("expected a %s URL", secure)
	default:
		return nil, "the host must be chatgpt.com, openai.com, or a subdomain of either"
	}
}

func openAIHost(host string) bool {
	for _, domain := range []string{"chatgpt.com", "openai.com"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}
