package parser

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var formatSourceHeadingRE = regexp.MustCompile(
	"(?m)^## [^\\n]+ \\(`([a-z0-9-]+)`\\)$",
)

func TestSessionFormatSourcesCoverRegistry(t *testing.T) {
	_, testFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve format inventory test path")

	inventoryPath := filepath.Join(
		filepath.Dir(testFile), "..", "..", "docs", "internal",
		"session-format-sources.md",
	)
	raw, err := os.ReadFile(inventoryPath)
	require.NoError(t, err)

	documented := make(map[AgentType]bool)
	for _, match := range formatSourceHeadingRE.FindAllSubmatch(raw, -1) {
		agent := AgentType(match[1])
		assert.Falsef(t, documented[agent],
			"provider %q documented more than once", agent)
		documented[agent] = true
	}

	expected := make(map[AgentType]bool, len(Registry)-1)
	for _, def := range Registry {
		if def.Type == AgentGrok {
			continue
		}
		expected[def.Type] = true
	}

	for agent := range documented {
		assert.Truef(t, expected[agent],
			"inventory documents unknown or excluded provider %q", agent)
	}
	for agent := range expected {
		assert.Truef(t, documented[agent],
			"provider %q missing from format inventory", agent)
	}
}
