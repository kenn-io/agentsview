package kata

import (
	"strings"

	"go.kenn.io/agentsview/internal/config"
)

// ConfigFrom resolves a [kata] section into a client configuration. hub is
// filing.EligibleHost for this process.
func ConfigFrom(c config.KataConfig, hub bool) Config {
	out := Config{
		Enabled:       c.Enabled,
		Hub:           hub,
		Endpoint:      strings.TrimSpace(c.Endpoint),
		TokenEnv:      strings.TrimSpace(c.TokenEnv),
		TokenRequired: strings.TrimSpace(c.TokenEnv) != "",
		Project:       strings.TrimSpace(c.Project),
		Actor:         strings.TrimSpace(c.Actor),
		AllowInsecure: c.AllowInsecure,
		Timeout:       c.Timeout,
	}
	if out.Project == "" {
		out.Project = config.DefaultKataProject
	}
	if out.Actor == "" {
		out.Actor = config.DefaultKataActor
	}
	if out.Timeout <= 0 {
		out.Timeout = DefaultTimeout
	}
	return out
}
