package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// TestEngineClassifyKimiWorkPaths covers provider changed-path
// classification for Kimi Work wire.jsonl layouts: the 5-segment
// agents/<agent> layout the kimi-desktop daimon runtime writes and the
// 3-segment legacy layout. Auxiliary daimon sessions (ctitle-*, sklsum-*,
// dvlt-*) must never be classified.
func TestEngineClassifyKimiWorkPaths(t *testing.T) {
	dir := t.TempDir()
	wd := "wd_agentsview_e901f41e2366"

	mainPath := filepath.Join(
		dir, wd, "conv-3fac68340656963a67a35ba9",
		"agents", "main", "wire.jsonl",
	)
	legacyPath := filepath.Join(
		dir, "wd_legacy_1234567890ab", "conv-a2a93747284e099415320d57",
		"wire.jsonl",
	)
	auxPaths := []string{
		filepath.Join(
			dir, wd, "ctitle-019f85a8-bd77-7f02-ad95-ce249ffdc5c5",
			"agents", "main", "wire.jsonl",
		),
		filepath.Join(
			dir, wd, "sklsum-019e98fd-eaad-7943-aab5-8ffa54a0ef2f",
			"agents", "main", "wire.jsonl",
		),
		filepath.Join(
			dir, wd, "dvlt-019f6bae-4e80-7248-9f8b-4ca8c0e481db",
			"wire.jsonl",
		),
	}
	for _, p := range append([]string{mainPath, legacyPath}, auxPaths...) {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644))
	}

	eng := &Engine{
		db: openTestDB(t),
		agentDirs: map[parser.AgentType][]string{
			parser.AgentKimiWork: {dir},
		},
		providerFactories: providerFactoryMap(parser.ProviderFactories()),
		providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentKimiWork: parser.ProviderMigrationProviderAuthoritative,
		},
	}

	tests := []struct {
		name    string
		path    string
		want    bool
		project string
	}{
		{
			name:    "conv session agents layout classified",
			path:    mainPath,
			want:    true,
			project: "agentsview",
		},
		{
			name:    "conv session legacy layout classified",
			path:    legacyPath,
			want:    true,
			project: "legacy",
		},
		{
			name: "ctitle aux session ignored",
			path: auxPaths[0],
			want: false,
		},
		{
			name: "sklsum aux session ignored",
			path: auxPaths[1],
			want: false,
		},
		{
			name: "dvlt aux session ignored",
			path: auxPaths[2],
			want: false,
		},
		{
			name: "unrelated file ignored",
			path: filepath.Join(dir, "readme.txt"),
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
			assert.Equal(t, parser.AgentKimiWork, got.Agent)
			assert.Equal(t, tt.project, got.Project)
			assert.Equal(t, tt.path, got.Path)
		})
	}
}
