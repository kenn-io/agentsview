package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

// TestEngineClassifyKimiPaths covers provider changed-path classification for
// both Kimi session layouts. The new .kimi-code layout has a 5-segment relative
// path and must be classified with its decoded project just like the legacy
// 3-segment layout.
func TestEngineClassifyKimiPaths(t *testing.T) {
	dir := t.TempDir()

	// Legacy: <kimiDir>/<project>/<session>/wire.jsonl
	legacyPath := filepath.Join(
		dir, "03cd233b1066bcc214245959059ca4c8",
		"uuid-1", "wire.jsonl",
	)
	// New (.kimi-code):
	//   <kimiDir>/<workdir>/<session>/agents/<agent>/wire.jsonl
	newMainPath := filepath.Join(
		dir, "wd_kimi-code_057f5c09ee3f",
		"session_cf2c3d74-c9d2-4ae4-95b7-d1d817298382",
		"agents", "main", "wire.jsonl",
	)
	newAgentPath := filepath.Join(
		dir, "wd_pi-mono_77f54d0fe81a",
		"session_49474979-70b1-49ac-b23b-b3bc224b3aca",
		"agents", "agent-0", "wire.jsonl",
	)
	// An agent name with a character outside [A-Za-z0-9_-] cannot
	// round-trip through the ':'-delimited session ID; it must not be
	// classified, so it is never imported in a state that cannot be
	// resynced. A space stands in for any such character (':' itself is
	// not a portable path component on Windows).
	invalidAgentPath := filepath.Join(
		dir, "wd_foo_1234567890ab",
		"session_uuid-1", "agents", "sub agent", "wire.jsonl",
	)
	for _, p := range []string{legacyPath, newMainPath, newAgentPath, invalidAgentPath} {
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte("{}"), 0o644))
	}

	eng := &Engine{
		db: openTestDB(t),
		agentDirs: map[parser.AgentType][]string{
			parser.AgentKimi: {dir},
		},
		providerFactories: providerFactoryMap(parser.ProviderFactories()),
		providerMigrationModes: map[parser.AgentType]parser.ProviderMigrationMode{
			parser.AgentKimi: parser.ProviderMigrationProviderAuthoritative,
		},
	}

	tests := []struct {
		name    string
		path    string
		want    bool
		project string
	}{
		{
			name:    "legacy wire.jsonl classified",
			path:    legacyPath,
			want:    true,
			project: "03cd233b1066bcc214245959059ca4c8",
		},
		{
			name:    "new-layout main agent classified",
			path:    newMainPath,
			want:    true,
			project: "kimi-code",
		},
		{
			name:    "new-layout non-main agent classified",
			path:    newAgentPath,
			want:    true,
			project: "pi-mono",
		},
		{
			name: "unrelated file ignored",
			path: filepath.Join(dir, "readme.txt"),
			want: false,
		},
		{
			name: "new-layout agent name with invalid char not classified",
			path: invalidAgentPath,
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
			assert.Equal(t, parser.AgentKimi, got.Agent)
			assert.Equal(t, tt.project, got.Project)
			assert.Equal(t, tt.path, got.Path)
		})
	}
}
