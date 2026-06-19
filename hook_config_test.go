package agentsview_test

import (
	"os"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type prekConfig struct {
	DefaultInstallHookTypes []string   `toml:"default_install_hook_types"`
	Repos                   []prekRepo `toml:"repos"`
}

type prekRepo struct {
	Hooks []prekHook `toml:"hooks"`
}

type prekHook struct {
	ID     string   `toml:"id"`
	Stages []string `toml:"stages"`
}

func TestNilAwayHookRunsOnlyOnPrePush(t *testing.T) {
	contents, err := os.ReadFile("prek.toml")
	require.NoError(t, err)

	var config prekConfig
	require.NoError(t, toml.Unmarshal(contents, &config))

	assert.Contains(t, config.DefaultInstallHookTypes, "pre-commit")
	assert.Contains(t, config.DefaultInstallHookTypes, "pre-push")

	hook := findPrekHook(t, config, "nilaway")
	assert.Equal(t, []string{"pre-push"}, hook.Stages)
}

func findPrekHook(t *testing.T, config prekConfig, id string) prekHook {
	t.Helper()

	for _, repo := range config.Repos {
		for _, hook := range repo.Hooks {
			if hook.ID == id {
				return hook
			}
		}
	}

	require.Failf(t, "missing prek hook", "hook ID %q was not found", id)
	return prekHook{}
}
