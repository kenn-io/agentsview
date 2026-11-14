package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMachineForSourcePathUsesNormalizedRootSpecificity(t *testing.T) {
	parent := t.TempDir()
	nested := filepath.Join(parent, "nested")
	var legacyParent strings.Builder
	legacyParent.Grow(len(parent) + 20)
	legacyParent.WriteString(parent)
	for range 10 {
		legacyParent.WriteByte(byte(filepath.Separator))
		legacyParent.WriteByte('.')
	}
	legacyParentPath := legacyParent.String()
	require.Greater(t, len(legacyParentPath), len(nested))

	machine, found := MachineForSourcePath(map[string]string{
		legacyParentPath: "parentbox",
		nested:           "nestedbox",
	}, filepath.Join(nested, "session.jsonl"))
	assert.True(t, found)
	assert.Equal(t, "nestedbox", machine)
}

func TestMachineForSourcePathMatchesAbsolutePathToRelativeRoot(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	relativeRoot := filepath.Join("testdata", "relative-session-source")

	machine, found := MachineForSourcePath(map[string]string{
		relativeRoot: "archivebox",
	}, filepath.Join(cwd, relativeRoot, "session.jsonl"))
	assert.True(t, found)
	assert.Equal(t, "archivebox", machine)
}
