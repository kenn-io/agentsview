package sync

import (
	"maps"
	"slices"

	"go.kenn.io/agentsview/internal/parser"
)

// SourceConfig is the part of EngineConfig that selects which providers an
// engine runs and which session roots they discover from. A running engine
// can replace it with ReconfigureSources.
type SourceConfig struct {
	AgentDirs        map[parser.AgentType][]string
	SourceMachines   map[parser.AgentType]map[string]string
	ProviderMetadata map[parser.AgentType]map[string][]string
	DisabledAgents   []parser.AgentType
}

// engineSources is one immutable snapshot of the configured provider set.
// Readers take it once per operation through Engine.sources, so a concurrent
// reconfiguration never exposes a mix of old roots and new providers.
type engineSources struct {
	agentDirs      map[parser.AgentType][]string
	sourceMachines map[parser.AgentType]map[string]string
	// preserveAgents lists the disabled providers whose archived sessions
	// discovery must leave untouched.
	preserveAgents    []parser.AgentType
	providerFactories map[parser.AgentType]parser.ProviderFactory
	// providerStatHashers caches the optional MultiFileStatHasher
	// implementations keyed by AgentType, built by type-asserting each
	// constructed provider; nil entries indicate the provider does not
	// implement MultiFileStatHasher (single-file agents and providers without
	// a multi-file layout take the existing stat-only composite path).
	providerStatHashers map[parser.AgentType]parser.MultiFileStatHasher
}

func newEngineSources(
	base []parser.ProviderFactory, cfg SourceConfig,
) *engineSources {
	dirs := make(map[parser.AgentType][]string, len(cfg.AgentDirs))
	for k, v := range cfg.AgentDirs {
		dirs[k] = append([]string(nil), v...)
	}
	sourceMachines := make(
		map[parser.AgentType]map[string]string, len(cfg.SourceMachines),
	)
	for agent, roots := range cfg.SourceMachines {
		sourceMachines[agent] = maps.Clone(roots)
	}
	disabledAgents := append([]parser.AgentType(nil), cfg.DisabledAgents...)
	factories := make([]parser.ProviderFactory, 0, len(base))
	for _, factory := range base {
		agent := factory.Definition().Type
		if slices.Contains(disabledAgents, agent) {
			continue
		}
		factories = append(factories, parser.ConfigureProviderFactory(
			factory, cfg.ProviderMetadata[agent],
		))
	}
	factoryMap := providerFactoryMap(factories)
	return &engineSources{
		agentDirs:           dirs,
		sourceMachines:      sourceMachines,
		preserveAgents:      disabledAgents,
		providerFactories:   factoryMap,
		providerStatHashers: buildProviderStatHashers(factoryMap),
	}
}

// noEngineSources is the empty snapshot seen by an engine that was never
// given sources.
var noEngineSources = &engineSources{}

func (e *Engine) sources() *engineSources {
	if sources := e.sourceSet.Load(); sources != nil {
		return sources
	}
	return noEngineSources
}

// ReconfigureSources replaces the engine's provider set and session roots.
// It waits for any in-flight sync operation so a pass never switches
// providers midway, and it does not sync the new roots; the caller decides
// what to reconcile. Sessions from providers that become disabled stay in
// the archive, exactly as when the engine starts with them disabled.
func (e *Engine) ReconfigureSources(cfg SourceConfig) {
	next := newEngineSources(e.baseProviderFactories, cfg)
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.sourceSet.Store(next)
	e.providerWatchRootsMu.Lock()
	clear(e.providerWatchRoots)
	e.providerWatchRootsMu.Unlock()
}
