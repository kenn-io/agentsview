package parser_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// parsedOutputFixture copies the corpus, which stays independent of unit fixtures,
// into an isolated source root.
func parsedOutputFixture(t *testing.T, agent parser.AgentType) string {
	t.Helper()
	root := t.TempDir()
	if agent == parser.AgentCrush {
		root = filepath.Join(root, "data")
	}
	require.NoError(t, os.CopyFS(root, os.DirFS(filepath.Join("testdata", "data-version-corpus", string(agent)))))
	return root
}
