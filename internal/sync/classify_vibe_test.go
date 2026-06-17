package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
)

func TestClassifyOnePath_Vibe(t *testing.T) {
	dir := t.TempDir()
	sessionDir := "session_20260616_083518_0107f266"

	// Vibe layout: <vibeDir>/session_<ts>_<uuid>/messages.jsonl.
	msgPath := filepath.Join(dir, sessionDir, "messages.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(msgPath), 0o755))
	require.NoError(t, os.WriteFile(msgPath, []byte("{}\n"), 0o644))

	// A real meta.json sits beside messages.jsonl. Changes to it should
	// route back to the sibling messages.jsonl, since title/model/usage
	// stats are sourced from meta.json.
	metaPath := filepath.Join(dir, sessionDir, "meta.json")
	require.NoError(t, os.WriteFile(metaPath, []byte("{}\n"), 0o644))

	deletedMetaDir := "session_20260616_083519_deleted"
	deletedMetaMsgPath := filepath.Join(dir, deletedMetaDir, "messages.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(deletedMetaMsgPath), 0o755))
	require.NoError(t, os.WriteFile(deletedMetaMsgPath, []byte("{}\n"), 0o644))
	deletedMetaPath := filepath.Join(dir, deletedMetaDir, "meta.json")

	// A non-session directory must not classify.
	otherPath := filepath.Join(dir, "scratch", "messages.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(otherPath), 0o755))
	require.NoError(t, os.WriteFile(otherPath, []byte("{}\n"), 0o644))

	eng := &Engine{
		agentDirs: map[parser.AgentType][]string{
			parser.AgentVibe: {dir},
		},
	}
	geminiMap := make(map[string]map[string]string)

	tests := []struct {
		name        string
		path        string
		want        bool
		wantPath    string
		wantProject string
	}{
		{
			name:        "messages.jsonl under session dir classifies",
			path:        msgPath,
			want:        true,
			wantPath:    msgPath,
			wantProject: sessionDir,
		},
		{
			name: "messages.jsonl outside session dir ignored",
			path: otherPath,
			want: false,
		},
		{
			name:        "meta.json routes to sibling messages.jsonl",
			path:        metaPath,
			want:        true,
			wantPath:    msgPath,
			wantProject: sessionDir,
		},
		{
			name:        "deleted meta.json routes to sibling messages.jsonl",
			path:        deletedMetaPath,
			want:        true,
			wantPath:    deletedMetaMsgPath,
			wantProject: deletedMetaDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := eng.classifyOnePath(tt.path, geminiMap)
			assert.Equal(t, tt.want, ok)
			if ok {
				assert.Equal(t, parser.AgentVibe, got.Agent)
				assert.Equal(t, tt.wantPath, got.Path)
				assert.Equal(t, tt.wantProject, got.Project)
			}
		})
	}
}
