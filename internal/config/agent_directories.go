package config

import (
	"bytes"
	"fmt"
	"log"
	"maps"
	"os"
	"slices"

	"github.com/BurntSushi/toml"
	"go.kenn.io/agentsview/internal/parser"
)

// AgentDirectoryConfig is the shared [agents.<id>] configuration. A nil
// Dirs leaves defaults intact; an explicit empty array clears them. Homes
// always add their native session roots to the selected directories.
type AgentDirectoryConfig struct {
	Dirs  []string `toml:"dirs"`
	Homes []string `toml:"homes"`
}

func (c *Config) applyAgentDirectories(value any) error {
	if value == nil {
		return nil
	}
	agents, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("agents: expected a TOML table")
	}
	for _, name := range slices.Sorted(maps.Keys(agents)) {
		def, ok := parser.AgentByType(parser.AgentType(name))
		if !ok {
			return fmt.Errorf("agents: unknown session provider %q", name)
		}
		table, ok := agents[name].(map[string]any)
		if !ok {
			return fmt.Errorf("agents.%s: expected a TOML table", name)
		}
		var entry AgentDirectoryConfig
		if dirs, exists := table["dirs"]; exists {
			entry.Dirs = agentDirectoryArray("agents."+name+".dirs", dirs)
		}
		if homes, exists := table["homes"]; exists {
			entry.Homes = agentDirectoryArray("agents."+name+".homes", homes)
		}
		if entry.Homes != nil {
			if !def.HomesSupported {
				return fmt.Errorf("agents.%s.homes: provider does not support alternate homes", name)
			}
			if c.agentHomes == nil {
				c.agentHomes = make(map[parser.AgentType][]string)
			}
			c.agentHomes[def.Type] = dedupeTrimmedStrings(entry.Homes)
		}
		if entry.Dirs != nil && c.agentDirSource[def.Type] != dirEnv {
			c.AgentDirs[def.Type] = entry.Dirs
			c.agentDirSource[def.Type] = dirFile
		}
	}
	return nil
}

// Invalid directory lists retain the existing warning-and-ignore behavior.
func agentDirectoryArray(key string, value any) []string {
	raw, ok := value.([]any)
	if !ok {
		log.Printf("config: %s: expected string array: got %T", key, value)
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok {
			log.Printf("config: %s: expected string array: element is %T", key, value)
			return nil
		}
		values = append(values, text)
	}
	return values
}

func setAgentHomes(config map[string]any, homes map[parser.AgentType][]string) error {
	if len(homes) == 0 {
		return nil
	}
	agents, err := configTable(config, "agents")
	if err != nil {
		return err
	}
	for agent, dirs := range homes {
		entry, err := configTable(agents, string(agent))
		if err != nil {
			return err
		}
		if len(dirs) == 0 {
			delete(entry, "homes")
		} else {
			entry["homes"] = dirs
		}
		if len(entry) == 0 {
			delete(agents, string(agent))
		}
	}
	if len(agents) == 0 {
		delete(config, "agents")
	}
	return nil
}

// configTable obtains a writable TOML table while preserving its other fields.
func configTable(parent map[string]any, key string) (map[string]any, error) {
	if value, exists := parent[key]; exists {
		table, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected a TOML table", key)
		}
		return table, nil
	}
	table := make(map[string]any)
	parent[key] = table
	return table, nil
}

// convertAgentTables is the format-conversion boundary for configs written
// before [agents.<id>]. Normal loads persist it once; diagnostic loads only
// convert in memory. Runtime resolution and settings writes use the new format.
func convertAgentTables(data string) (string, bool, error) {
	var raw map[string]any
	if _, err := toml.Decode(data, &raw); err != nil {
		return "", false, fmt.Errorf("parsing config: %w", err)
	}
	changed, err := convertAgentTableMap(raw)
	if err != nil || !changed {
		return data, changed, err
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(raw); err != nil {
		return "", false, fmt.Errorf("encoding agent config: %w", err)
	}
	return buf.String(), true, nil
}

func convertAgentTableMap(raw map[string]any) (bool, error) {
	changed := false
	for _, def := range parser.Registry {
		keys := map[string]string{def.ConfigKey: "dirs"}
		switch def.Type {
		case parser.AgentClaude:
			keys["claude_homes"] = "homes"
		case parser.AgentCodex:
			keys["codex_homes"] = "homes"
		}
		for _, key := range slices.Sorted(maps.Keys(keys)) {
			value, exists := raw[key]
			if !exists || key == "" {
				continue
			}
			agents, err := configTable(raw, "agents")
			if err != nil {
				return false, err
			}
			entry, err := configTable(agents, string(def.Type))
			if err != nil {
				return false, err
			}
			field := keys[key]
			if _, conflict := entry[field]; conflict {
				return false, fmt.Errorf("both %s and agents.%s.%s are configured; remove one before migration", key, def.Type, field)
			}
			entry[field] = value
			delete(raw, key)
			changed = true
		}
	}
	return changed, nil
}

func (c *Config) migrateAgentTables() error {
	return c.withConfigLock(func() error {
		data, err := os.ReadFile(c.configPath())
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading config for agent table migration: %w", err)
		}
		converted, changed, err := convertAgentTables(string(data))
		if err != nil || !changed {
			return err
		}
		if err := os.WriteFile(c.configPath(), []byte(converted), 0o600); err != nil {
			return fmt.Errorf("writing agent configuration: %w", err)
		}
		return nil
	})
}
