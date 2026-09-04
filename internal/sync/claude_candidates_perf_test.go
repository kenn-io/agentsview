package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

func TestClaudeChangedPathCandidateWorkIgnoresUnrelatedSessions(t *testing.T) {
	var allocations []float64
	for _, size := range []int{8, 8000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root := t.TempDir()
			for _, project := range []string{"project-a", "project-b"} {
				require.NoError(t, os.Mkdir(filepath.Join(root, project), 0o700))
			}
			changed := filepath.Join(root, "project-a", "shared.jsonl")
			duplicate := filepath.Join(root, "project-b", "shared.jsonl")
			require.NoError(t, os.WriteFile(changed, []byte("{}\n"), 0o600))
			require.NoError(t, os.WriteFile(duplicate, []byte("{}\n"), 0o600))
			require.NoError(t, os.Chtimes(changed, time.Unix(100, 0), time.Unix(100, 0)))
			require.NoError(t, os.Chtimes(duplicate, time.Unix(200, 0), time.Unix(200, 0)))
			for i := range size {
				require.NoError(t, os.WriteFile(filepath.Join(root, "project-a", fmt.Sprintf("unrelated-%d.jsonl", i)), nil, 0o600))
			}
			database := openTestDB(t)
			engine := NewEngine(database, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}})
			t.Cleanup(engine.Close)
			var files []parser.DiscoveredFile
			var err error
			allocations = append(allocations, testing.AllocsPerRun(3, func() { files, err = engine.classifyPaths(t.Context(), []string{changed}) }))
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, duplicate, files[0].Path, "the newer duplicate must still win")
			require.NoError(t, os.Remove(duplicate))
			files, err = engine.classifyPaths(t.Context(), []string{changed})
			require.NoError(t, err)
			require.Len(t, files, 1)
			assert.Equal(t, changed, files[0].Path, "a deleted duplicate must not stay selected")
		})
	}
	assert.Less(t, allocations[1], allocations[0]*2, "one changed transcript must not enumerate thousands of unrelated files")
}
