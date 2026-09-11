package config

import (
	"maps"
	"slices"
)

// ApplyMachineAliases resolves source keys whose recorded owner is this
// installation. Display names never establish source ownership.
func (c *Config) ApplyMachineAliases(aliases map[string]string) {
	c.machineAliases = maps.Clone(aliases)
	c.SourceMachines = maps.Clone(c.SourceMachines)
	for agent, roots := range c.SourceMachines {
		roots = maps.Clone(roots)
		for root, machine := range roots {
			roots[root] = c.sourceMachineIdentity(machine)
		}
		c.SourceMachines[agent] = roots
	}
	c.SessionSources = slices.Clone(c.SessionSources)
	for i := range c.SessionSources {
		c.SessionSources[i].Machine = c.sourceMachineIdentity(c.SessionSources[i].Machine)
	}
}

func (c *Config) sourceMachineIdentity(machine string) string {
	if c.InstallationID != "" && c.machineAliases[machine] == c.InstallationID {
		return c.InstallationID
	}
	return machine
}
