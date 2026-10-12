package codexcloud

import (
	"fmt"
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
	// ConfigPath names config.toml in the read-error message.
	ConfigPath string
}

// GateSource returns the current gates.
type GateSource func() Gates

// NewGateSource re-reads the config with load on each call. A failed read
// keeps the last good gates, so a half-written config.toml never turns the
// source off or on by accident.
func NewGateSource(
	configPath string, load func() (config.Config, error), getenv func(string) string, userHome string,
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
			ConfigPath: configPath,
			CodexCloud: cfg.CodexCloud,
			Disabled:   cfg.AgentDisabled(parser.AgentCodexCloud),
			Homes:      DiscoverHomes(getenv, userHome, cfg.ConfiguredAgentHomes(parser.AgentCodex)),
		}
		return last
	}
}

// Configured reports whether the user set the source up: config.toml has
// [agents.codex-cloud], or disabled_agents lists it. Status shows nothing
// otherwise.
func (g Gates) Configured() bool {
	return g.CodexCloud.Present || g.Disabled
}

// Fetching reports whether thread sync may run.
func (g Gates) Fetching() bool {
	return !g.Disabled && g.CodexCloud.FetchEnabled()
}

// SourceRowKey keys the one row that stands for the whole source when it
// is off or its configuration has a problem.
const SourceRowKey = "codex-cloud"

// offRow returns the source row when the gates keep sync off, checked in
// the order the status names them: source sync, disabled_agents, a config
// error, then enabled.
func (g Gates) offRow(noSync bool) (HomeStatus, bool) {
	row := HomeStatus{Key: SourceRowKey}
	switch {
	case noSync:
		row.State, row.Message = StateOffNoSync, "Off: source sync is off for this daemon."
	case g.Disabled:
		row.State, row.Message = StateOffDisabledAgents, "Off: listed in disabled_agents."
	case g.CodexCloud.Err != nil:
		row.State, row.Message = StateConfigError, g.CodexCloud.Err.Error()
	case !g.CodexCloud.Enabled:
		row.State, row.Message = StateOffDisabled, "Off: enabled is false."
	default:
		return HomeStatus{}, false
	}
	return row, true
}

// readErrRow returns the source row for a config.toml that could not be
// read while sync carries on with the last good settings.
func (g Gates) readErrRow() (HomeStatus, bool) {
	if g.ReadErr == nil {
		return HomeStatus{}, false
	}
	return HomeStatus{
		Key: SourceRowKey, State: StateConfigError,
		Message: fmt.Sprintf("%s could not be read: %v. Cloud import keeps its last good settings.",
			g.ConfigPath, g.ReadErr),
	}, true
}
