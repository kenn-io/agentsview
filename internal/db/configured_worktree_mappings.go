package db

import (
	"strings"

	"go.kenn.io/agentsview/internal/worktree"
)

// configuredWorktreeMappings is immutable and lives only on a local archive
// handle. It never participates in CRUD, archive copies, or publication.
type configuredWorktreeMappings struct {
	machine string
	rules   []WorktreeProjectMapping
}

// SetConfiguredWorktreeMappings binds validated config rules to this
// installation, not to any remote machine ingested by the same archive.
func (db *DB) SetConfiguredWorktreeMappings(machine string, rules []worktree.Rule) {
	machine = strings.TrimSpace(machine)
	if machine == "" || len(rules) == 0 {
		db.configuredWorktrees.Store(nil)
		return
	}
	mappings := make([]WorktreeProjectMapping, 0, len(rules))
	for _, rule := range rules {
		mappings = append(mappings, WorktreeProjectMapping{
			Machine: machine, PathPrefix: rule.PathPrefix,
			Layout: rule.Layout, Project: rule.Project, Enabled: true,
		})
	}
	db.configuredWorktrees.Store(&configuredWorktreeMappings{machine: machine, rules: mappings})
}

// UseConfiguredWorktreeMappingsFrom gives a scratch rebuild the same in-memory
// rules as the original archive, without copying those rules from disk.
func (db *DB) UseConfiguredWorktreeMappingsFrom(source *DB) {
	db.configuredWorktrees.Store(source.configuredWorktrees.Load())
}

// mergeConfiguredWorktreeMappings returns the effective rules in resolution
// order. An enabled archive rule owns its exact prefix; a disabled archive
// rule is still visible to callers listing stored metadata, but claims no cwd.
// An empty machine selects rules for all machines for inventory evaluation.
func (db *DB) mergeConfiguredWorktreeMappings(machine string, stored []WorktreeProjectMapping) []WorktreeProjectMapping {
	configured := db.configuredWorktrees.Load()
	if configured == nil || (machine != "" && machine != configured.machine) {
		return stored
	}
	claimed := make(map[string]bool, len(stored))
	for _, mapping := range stored {
		if mapping.Enabled && mapping.Machine == configured.machine {
			claimed[normalizedMappingPath(mapping.PathPrefix)] = true
		}
	}
	merged := make([]WorktreeProjectMapping, 0, len(stored)+len(configured.rules))
	merged = append(merged, stored...)
	for _, mapping := range configured.rules {
		if !claimed[mapping.PathPrefix] {
			merged = append(merged, mapping)
		}
	}
	sortWorktreeProjectMappings(merged)
	return merged
}
