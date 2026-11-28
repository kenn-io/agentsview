package db

import (
	"strings"

	"go.kenn.io/agentsview/internal/worktree"
)

// configuredWorktreeMappings is an immutable, in-memory view. It is not
// stored in SQLite or exposed through rule CRUD or publication.
type configuredWorktreeMappings struct {
	machine string
	rules   []WorktreeProjectMapping
}

// WithConfiguredWorktreeMappings binds validated config rules to the local
// installation before a writable open can repair repository project names.
func WithConfiguredWorktreeMappings(machine string, rules []worktree.Rule) OpenOption {
	mappings := newConfiguredWorktreeMappings(machine, rules)
	return func(opts *openOptions) { opts.configuredWorktrees = mappings }
}

// SetConfiguredWorktreeMappings binds validated config rules to this
// installation, not to any remote machine ingested by the same archive.
func (db *DB) SetConfiguredWorktreeMappings(machine string, rules []worktree.Rule) {
	db.configuredWorktrees.Store(newConfiguredWorktreeMappings(machine, rules))
}

func newConfiguredWorktreeMappings(machine string, rules []worktree.Rule) *configuredWorktreeMappings {
	machine = strings.TrimSpace(machine)
	if machine == "" || len(rules) == 0 {
		return nil
	}
	mappings := make([]WorktreeProjectMapping, 0, len(rules))
	for _, rule := range rules {
		mappings = append(mappings, WorktreeProjectMapping{
			Machine: machine, PathPrefix: rule.PathPrefix,
			Layout: rule.Layout, Project: rule.Project, Enabled: true,
		})
	}
	return &configuredWorktreeMappings{machine: machine, rules: mappings}
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
