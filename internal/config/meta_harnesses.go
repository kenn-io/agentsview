package config

import (
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/agentsview/internal/parser"
)

// metaHarnessRoot describes a default container directory where a meta-harness
// (such as T3 Code) stores isolated per-instance data directories for an
// underlying coding agent provider.
//
// Adding default discovery for another meta-harness requires only adding an
// entry here (and ensuring the target provider expands container roots).
type metaHarnessRoot struct {
	Agent parser.AgentType
	// EnvVar, when set to an absolute path, replaces HomeRelBase as the
	// meta-harness base directory (for example, T3CODE_HOME replaces ~/.t3).
	EnvVar string
	// HomeRelBase is the default base directory relative to $HOME, in slash
	// form (for example, ".t3").
	HomeRelBase string
	// Subpath is the container directory relative to the base directory, in
	// slash form (for example, "userdata/providers/antigravity").
	Subpath string
}

var defaultMetaHarnessRoots = []metaHarnessRoot{
	{
		// T3 Code launches Antigravity ACP (`agy acp`) with an isolated
		// `--gemini-dir <stateDir>/providers/antigravity/<sha256(instanceId)>`
		// and `--app-data-dir antigravity-acp`, where `<stateDir>` defaults to
		// `$T3CODE_HOME/userdata` (or `~/.t3/userdata`). Keeping the stable
		// providers container as a default root lets `antigravity-cli` discover
		// and watch instances created after startup without restarting.
		Agent:       parser.AgentAntigravityCLI,
		EnvVar:      "T3CODE_HOME",
		HomeRelBase: ".t3",
		Subpath:     "userdata/providers/antigravity",
	},
}

func resolveMetaHarnessDefaultDirs(agent parser.AgentType, home string) []string {
	var dirs []string
	for _, entry := range defaultMetaHarnessRoots {
		if entry.Agent != agent {
			continue
		}
		base := ""
		if entry.EnvVar != "" {
			if envVal := os.Getenv(entry.EnvVar); envVal != "" && filepath.IsAbs(envVal) {
				base = envVal
			}
		}
		if base == "" {
			base = filepath.Join(home, filepath.FromSlash(entry.HomeRelBase))
		}
		dir := filepath.Join(base, filepath.FromSlash(entry.Subpath))
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	return dirs
}

// MetaHarnessEnvVars returns the deduplicated environment variable names that
// redirect default meta-harness container roots. Service installers use this
// list to warn when shell overrides will not be visible to background services,
// and tests use it to isolate HOME-based discovery from the host environment.
func MetaHarnessEnvVars() []string {
	var vars []string
	for _, entry := range defaultMetaHarnessRoots {
		if entry.EnvVar != "" && !slices.Contains(vars, entry.EnvVar) {
			vars = append(vars, entry.EnvVar)
		}
	}
	return vars
}
