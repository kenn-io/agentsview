package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestClassifyOnePath_OpenHands(t *testing.T) {
	root := t.TempDir()
	sessionDir := filepath.Join(
		root, "086c7ecf6cb746b69fbcb900358d1247",
	)
	eventsDir := filepath.Join(sessionDir, "events")
	require.NoError(t, os.MkdirAll(eventsDir, 0o755))

	baseStatePath := filepath.Join(sessionDir, "base_state.json")
	tasksPath := filepath.Join(sessionDir, "TASKS.json")
	eventPath := filepath.Join(
		eventsDir, "event-00001-abc.json",
	)
	require.NoError(t, os.WriteFile(
		baseStatePath, []byte(`{}`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		tasksPath, []byte(`{}`), 0o644,
	))
	require.NoError(t, os.WriteFile(
		eventPath, []byte(`{}`), 0o644,
	))

	eng := &Engine{
		db: openTestDB(t),
		agentDirs: map[parser.AgentType][]string{
			parser.AgentOpenHands: {root},
		},
		providerFactories: providerFactoryMap(parser.ProviderFactories()),
		providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentOpenHands: parser.ProviderMigrationProviderAuthoritative,
		},
	}

	tests := []struct {
		name    string
		path    string
		want    bool
		retPath string
	}{
		{
			name:    "base_state remaps to session dir",
			path:    baseStatePath,
			want:    true,
			retPath: sessionDir,
		},
		{
			name:    "tasks remaps to session dir",
			path:    tasksPath,
			want:    true,
			retPath: sessionDir,
		},
		{
			name:    "event remaps to session dir",
			path:    eventPath,
			want:    true,
			retPath: sessionDir,
		},
		{
			name: "observations ignored",
			path: filepath.Join(
				sessionDir, "observations", "out.txt",
			),
			want: false,
		},
		{
			name: "unrelated file ignored",
			path: filepath.Join(root, "README.md"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := eng.classifyPaths([]string{tt.path})
			if !tt.want {
				assert.Empty(t, files)
				return
			}
			require.Len(t, files, 1)
			got := files[0]
			assert.Equal(t, parser.AgentOpenHands, got.Agent)
			assert.Equal(t, tt.retPath, got.Path)
		})
	}
}
