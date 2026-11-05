package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectSessionCandidatesKeepNestedSubagentsAndProviderLabels(t *testing.T) {
	for _, agent := range []AgentType{AgentClaude, AgentIcodemate} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			paths := []string{"project-a/shared.jsonl", "project-b/shared.jsonl", "project-a/unrelated.jsonl", "project-a/shared/subagents/workflows/task/agent-child.jsonl"}
			for _, name := range paths {
				path := filepath.Join(root, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, nil, 0o600))
			}
			for _, tc := range []struct {
				ids  map[string]struct{}
				want []string
			}{
				{map[string]struct{}{"shared": {}}, []string{paths[0], paths[1]}},
				{map[string]struct{}{"agent-child": {}}, []string{paths[3]}},
			} {
				var got []string
				for _, file := range ProjectJSONLSessionCandidates(root, agent, tc.ids) {
					assert.Equal(t, agent, file.Agent)
					rel, err := filepath.Rel(root, file.Path)
					require.NoError(t, err)
					got = append(got, filepath.ToSlash(rel))
				}
				assert.ElementsMatch(t, tc.want, got)
			}
		})
	}
}
