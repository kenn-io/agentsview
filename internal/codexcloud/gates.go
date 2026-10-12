package codexcloud

import (
	"sync"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// Gates are the file-backed settings both jobs re-read before each run, so
// a config.toml change takes effect without a restart. NoSync is not here:
// it never appears in config.toml and is fixed at daemon start.
type Gates struct {
	CodexCloud config.CodexCloudConfig
	// Disabled means codex-cloud is in disabled_agents, which wins over
	// enabled = true.
	Disabled bool
	// Homes are the Codex homes to read logins from, in priority order.
	Homes []string
	// ReadErr is set when config.toml could not be read; the other fields
	// then hold the last good read.
	ReadErr error
}

// GateSource returns the current gates.
type GateSource func() Gates

// NewGateSource re-reads the config with load on each call. A failed read
// keeps the last good gates, so a half-written config.toml never turns the
// source off or on by accident.
func NewGateSource(
	load func() (config.Config, error), getenv func(string) string, userHome string,
) GateSource {
	var mu sync.Mutex
	var last Gates
	return func() Gates {
		cfg, err := load()
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			last.ReadErr = err
			return last
		}
		last = Gates{
			CodexCloud: cfg.CodexCloud,
			Disabled:   cfg.AgentDisabled(parser.AgentCodexCloud),
			Homes:      DiscoverHomes(getenv, userHome, cfg.ConfiguredAgentHomes(parser.AgentCodex)),
		}
		return last
	}
}

// Fetching reports whether thread sync may run.
func (g Gates) Fetching() bool {
	return !g.Disabled && g.CodexCloud.FetchEnabled()
}
