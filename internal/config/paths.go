package config

import (
	"fmt"

	"go.kenn.io/agentsview/internal/pathutil"
)

func expandDataDir(cfg *Config) error {
	expanded, err := pathutil.ExpandHome(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("expanding data directory: %w", err)
	}
	cfg.DataDir = expanded
	return nil
}

func expandLocalPaths(cfg *Config) error {
	expand := func(name string, value *string) error {
		expanded, err := pathutil.ExpandHome(*value)
		if err != nil {
			return fmt.Errorf("expanding %s: %w", name, err)
		}
		*value = expanded
		return nil
	}
	expandSlice := func(name string, values []string) error {
		for i := range values {
			if err := expand(name, &values[i]); err != nil {
				return err
			}
		}
		return nil
	}

	if err := expand("data directory", &cfg.DataDir); err != nil {
		return err
	}
	fields := []struct {
		name  string
		value *string
	}{
		{"terminal custom binary", &cfg.Terminal.CustomBin},
		{"proxy binary", &cfg.Proxy.Bin},
		{"proxy TLS certificate", &cfg.Proxy.TLSCert},
		{"proxy TLS key", &cfg.Proxy.TLSKey},
		{"DuckDB path", &cfg.DuckDB.Path},
		{"vector database path", &cfg.Vector.DBPath},
		{"recall prompt directory", &cfg.Recall.Extract.Prompts.Dir},
	}
	for _, field := range fields {
		if err := expand(field.name, field.value); err != nil {
			return err
		}
	}
	if err := expandSlice("sync cwd prefix", cfg.SyncIncludeCwdPrefixes); err != nil {
		return err
	}
	for agent, dirs := range cfg.AgentDirs {
		if err := expandSlice(fmt.Sprintf("%s session directory", agent), dirs); err != nil {
			return err
		}
		cfg.AgentDirs[agent] = dirs
	}
	for name, agent := range cfg.Agent {
		if err := expand(fmt.Sprintf("agent %s binary", name), &agent.Binary); err != nil {
			return err
		}
		if err := expand(fmt.Sprintf("agent %s sandbox", name), &agent.Sandbox); err != nil {
			return err
		}
		cfg.Agent[name] = agent
	}
	return nil
}
