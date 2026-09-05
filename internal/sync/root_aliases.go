package sync

import "go.kenn.io/agentsview/internal/parser"

// InstallRootAliases publishes the Codex root aliases from a resolved
// configuration to the parser package. Title lookups, hint sources, watch
// plans, and raw capture read the alias table through path-only helpers,
// so every entry point that builds providers from configuration without a
// sync engine must call this: the daemon engine, the standalone file
// watcher behind push --watch, and raw-sync. An empty alias set leaves the
// table untouched so an import, rebuild, or probe engine cannot clear the
// aliases a running engine depends on.
func InstallRootAliases(aliases map[parser.AgentType]map[string][]string) {
	if codex := aliases[parser.AgentCodex]; len(codex) > 0 {
		parser.SetCodexRootAliases(codex)
	}
}
