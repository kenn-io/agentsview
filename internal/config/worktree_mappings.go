package config

import (
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/worktree"
)

func normalizeWorktreeMappings(input []worktree.Rule) ([]worktree.Rule, error) {
	if len(input) == 0 {
		return nil, nil
	}
	rules := make([]worktree.Rule, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for i, rule := range input {
		prefix, err := pathutil.ExpandHome(strings.TrimSpace(rule.PathPrefix))
		if err != nil {
			return nil, fmt.Errorf("worktree_mappings[%d].path_prefix: %w", i, err)
		}
		rule.PathPrefix = prefix
		rule, err = rule.Normalize()
		if err != nil {
			return nil, fmt.Errorf("worktree_mappings[%d]: %w", i, err)
		}
		if _, duplicate := seen[rule.PathPrefix]; duplicate {
			return nil, fmt.Errorf("worktree_mappings[%d]: duplicate path_prefix %q", i, rule.PathPrefix)
		}
		seen[rule.PathPrefix] = struct{}{}
		rules = append(rules, rule)
	}
	return rules, nil
}
