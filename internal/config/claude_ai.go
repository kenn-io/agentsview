package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// ClaudeAIConfig is the Claude.ai Sync part of the [agents.claude-ai]
// table. An empty Organizations list imports every organization.
type ClaudeAIConfig struct {
	// Organizations lists the lowercase organization UUIDs Sync imports.
	Organizations []string
	// Err holds a problem in the table. Sync refuses to run while it is
	// set, so a typo never widens the import.
	Err *ClaudeAIConfigError
}

// ClaudeAIConfigError is a problem in [agents.claude-ai].
type ClaudeAIConfigError struct {
	Path    string
	Key     string
	Reason  string
	Unknown bool
}

func (e *ClaudeAIConfigError) Error() string {
	const off = " Claude.ai Sync stays off until this is fixed."
	if e.Unknown {
		return fmt.Sprintf("%s has an unknown setting %q under [agents.claude-ai]. "+
			"Remove it or correct its name."+off, e.Path, e.Key)
	}
	return fmt.Sprintf("%s has an invalid value for %q under [agents.claude-ai]: %s."+off,
		e.Path, e.Key, e.Reason)
}

// parseClaudeAITable reads organizations from [agents.claude-ai]. Any other
// key is an error because a misspelled organizations key would otherwise
// import everything.
func parseClaudeAITable(cfgPath string, agents any) ClaudeAIConfig {
	table, _ := agents.(map[string]any)
	values, _ := table[string(parser.AgentClaudeAI)].(map[string]any)
	fail := func(key, reason string, unknown bool) ClaudeAIConfig {
		return ClaudeAIConfig{Err: &ClaudeAIConfigError{Path: cfgPath, Key: key, Reason: reason, Unknown: unknown}}
	}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		if key != "organizations" {
			return fail(key, "", true)
		}
	}
	raw, exists := values["organizations"]
	if !exists {
		return ClaudeAIConfig{}
	}
	list, ok := raw.([]any)
	if !ok {
		return fail("organizations", "expected an array of organization UUIDs", false)
	}
	orgs := make([]string, 0, len(list))
	for _, item := range list {
		text, ok := item.(string)
		id := strings.ToLower(strings.TrimSpace(text))
		if !ok || id == "" {
			return fail("organizations", "expected non-empty organization UUID strings", false)
		}
		orgs = append(orgs, id)
	}
	return ClaudeAIConfig{Organizations: orgs}
}
