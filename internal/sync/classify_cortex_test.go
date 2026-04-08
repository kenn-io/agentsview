package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/wesm/agentsview/internal/parser"
)

func TestClassifyOnePath_Cortex(t *testing.T) {
	dir := t.TempDir()
	uuid := "11111111-2222-3333-4444-555555555555"

	// Create session .json and companion .history.jsonl.
	jsonPath := filepath.Join(dir, uuid+".json")
	jsonlPath := filepath.Join(dir, uuid+".history.jsonl")
	require.NoError(t, os.WriteFile(jsonPath, []byte("{}"), 0o644))
	require.NoError(t, os.WriteFile(jsonlPath, []byte("{}"), 0o644))

	eng := &Engine{
		agentDirs: map[parser.AgentType][]string{
			parser.AgentCortex: {dir},
		},
	}
	geminiMap := make(map[string]map[string]string)

	tests := []struct {
		name    string
		path    string
		want    bool
		agent   parser.AgentType
		retPath string // expected Path in DiscoveredFile
	}{
		{
			name:    "uuid.json classified",
			path:    jsonPath,
			want:    true,
			agent:   parser.AgentCortex,
			retPath: jsonPath,
		},
		{
			name:    "history.jsonl remaps to .json",
			path:    jsonlPath,
			want:    true,
			agent:   parser.AgentCortex,
			retPath: jsonPath,
		},
		{
			name: "backup file ignored",
			path: filepath.Join(
				dir, uuid+".back.12345.json",
			),
			want: false,
		},
		{
			name: "nested path ignored",
			path: filepath.Join(
				dir, "subdir", uuid+".json",
			),
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
			got, ok := eng.classifyOnePath(tt.path, geminiMap)
			assert.Equal(t, tt.want, ok)
			if ok {
				assert.Equal(t, tt.agent, got.Agent)
				assert.Equal(t, tt.retPath, got.Path)
			}
		})
	}
}
